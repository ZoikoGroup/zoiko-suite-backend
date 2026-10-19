package authority_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/signature"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
)

const cbTenant = "tenant-A"

type cbSecrets struct {
	secret []byte
	err    error
	paths  []string
}

func (f *cbSecrets) Secrets(_ context.Context, tenantID, path string) ([][]byte, error) {
	f.paths = append(f.paths, tenantID+"|"+path)
	if f.err != nil {
		return nil, f.err
	}
	return [][]byte{append([]byte(nil), f.secret...)}, nil
}

type cbStore struct {
	calls []authority.CallbackInput
	res   *authority.CallbackResult
	err   error
}

func (f *cbStore) ApplyCallback(_ context.Context, in authority.CallbackInput) (*authority.CallbackResult, error) {
	f.calls = append(f.calls, in)
	return f.res, f.err
}
func (f *cbStore) ListCallbacks(context.Context, string, int) ([]authority.CallbackReceipt, error) {
	return nil, nil
}

func cbConfig(scheme string) authority.CallbackConfig {
	return authority.CallbackConfig{"taxauth": {Callback: adapter.Callback{
		Scheme: scheme, ToleranceSeconds: 300, EventRefPath: "event_id", PaymentRefPath: "submission_ref", StatusPath: "status",
		StatusMap: map[string]string{"ACC": "ACCEPTED", "REJ": "REJECTED", "PRC": "PENDING"},
	}}}
}

func newCB(scheme string) (*authority.CallbackProcessor, *cbSecrets, *cbStore, time.Time) {
	now := time.Now().UTC()
	sec, st := &cbSecrets{secret: []byte("s3cret")}, &cbStore{res: &authority.CallbackResult{Outcome: authority.OutcomeApplied}}
	return &authority.CallbackProcessor{Config: cbConfig(scheme), Secrets: sec, Store: st, Guard: signature.NewMemoryGuard(100, func() time.Time { return now }),
		Log: zap.NewNop(), Now: func() time.Time { return now }}, sec, st, now
}

const cbBody = `{"event_id":"E1","submission_ref":"S1","status":"ACC"}`

func hexSig(b string) string {
	s, _ := adapter.HMACSigner([]byte("s3cret"))(context.Background(), []byte(b))
	return s
}

func hdr(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }

func TestSignedCallbackIsMappedAndApplied(t *testing.T) {
	p, sec, st, _ := newCB("hmac-sha256-hex")
	r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(cbBody)), strings.NewReader(cbBody))
	if r.Status != 200 || r.Outcome != authority.OutcomeApplied || len(st.calls) != 1 {
		t.Fatalf("%+v", r)
	}
	c := st.calls[0]
	if c.Mapped != "ACCEPTED" || c.EventRef != "E1" || c.SubmissionRef != "S1" || c.RawStatus != "ACC" || len(c.BodyHash) != 64 {
		t.Fatalf("%+v", c)
	}
	if sec.paths[0] != cbTenant+"|kv/callbacks/"+cbTenant+"/taxauth" {
		t.Fatalf("the secret is fetched for THIS tenant and provider: %v", sec.paths)
	}
}

func TestBadSignatureWritesNothing(t *testing.T) {
	for name, h := range map[string]http.Header{
		"wrong": hdr("X-Webhook-Signature", strings.Repeat("a", 64)), "missing": {}, "short": hdr("X-Webhook-Signature", "abc"),
		"other body": hdr("X-Webhook-Signature", hexSig(`{"event_id":"E1","submission_ref":"S1","status":"REJ"}`)),
	} {
		p, _, st, _ := newCB("hmac-sha256-hex")
		r := p.Handle(context.Background(), "taxauth", cbTenant, h, strings.NewReader(cbBody))
		if r.Status != 401 || r.Outcome != "signature_invalid" || len(st.calls) != 0 || p.Count.SignatureFailed.Load() != 1 {
			t.Errorf("%s: %+v calls=%d", name, r, len(st.calls))
		}
	}
}

func TestTheTenantInTheURLFixesWhichSecretVerifies(t *testing.T) {
	p, sec, st, _ := newCB("hmac-sha256-hex")
	sec.secret = []byte("another-tenants-secret")
	if r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(cbBody)), strings.NewReader(cbBody)); r.Status != 401 || len(st.calls) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownProviderTenantSecretAndOutage(t *testing.T) {
	p, sec, st, _ := newCB("hmac-sha256-hex")
	ok := hdr("X-Webhook-Signature", hexSig(cbBody))
	if r := p.Handle(context.Background(), "nobody", cbTenant, ok, strings.NewReader(cbBody)); r.Status != 404 {
		t.Errorf("unknown provider: %d", r.Status)
	}
	for _, bad := range []string{"", "a/b", "a b", strings.Repeat("x", 65)} {
		if r := p.Handle(context.Background(), "taxauth", bad, ok, strings.NewReader(cbBody)); r.Status != 404 {
			t.Errorf("tenant %q: %d", bad, r.Status)
		}
	}
	sec.err = authority.ErrSecretUnknown
	if r := p.Handle(context.Background(), "taxauth", cbTenant, ok, strings.NewReader(cbBody)); r.Status != 401 {
		t.Errorf("no secret is indistinguishable from a bad signature: %d", r.Status)
	}
	sec.err = errors.New("vault down")
	if r := p.Handle(context.Background(), "taxauth", cbTenant, ok, strings.NewReader(cbBody)); r.Status != 503 {
		t.Errorf("a vault outage must make the authority retry: %d", r.Status)
	}
	if len(st.calls) != 0 {
		t.Fatal("nothing may be applied")
	}
}

func TestMalformedUnmappedAndOversizedAreRefusedAfterAuthentication(t *testing.T) {
	p, _, st, _ := newCB("hmac-sha256-hex")
	for name, c := range map[string]struct {
		b    string
		want int
	}{
		"not json": {`nope`, 400}, "no event": {`{"submission_ref":"S1","status":"ACC"}`, 400},
		"no ref": {`{"event_id":"E1","status":"ACC"}`, 400}, "unmapped": {`{"event_id":"E1","submission_ref":"S1","status":"???"}`, 422},
	} {
		if r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(c.b)), strings.NewReader(c.b)); r.Status != c.want {
			t.Errorf("%s: %d want %d", name, r.Status, c.want)
		}
	}
	big := `{"event_id":"E1","submission_ref":"S1","status":"ACC","pad":"` + strings.Repeat("x", 300<<10) + `"}`
	if r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(big)), bytes.NewReader([]byte(big))); r.Status != 413 {
		t.Errorf("oversized: %d", r.Status)
	}
	if len(st.calls) != 0 {
		t.Fatal("none of these may be applied")
	}
}

func TestStoreOutcomesMapToResponses(t *testing.T) {
	for outcome, want := range map[string]int{
		authority.OutcomeApplied: 200, authority.OutcomeDuplicate: 200, authority.OutcomeRegressionBlocked: 200, authority.OutcomeStaleIgnored: 200,
		authority.OutcomeUnknownFiling: 202, authority.OutcomeAmbiguousFiling: 202,
	} {
		p, _, st, _ := newCB("hmac-sha256-hex")
		st.res = &authority.CallbackResult{Outcome: outcome}
		if r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(cbBody)), strings.NewReader(cbBody)); r.Status != want {
			t.Errorf("%s: %d", outcome, r.Status)
		}
	}
	p, _, st, _ := newCB("hmac-sha256-hex")
	st.err = errors.New("db down")
	if r := p.Handle(context.Background(), "taxauth", cbTenant, hdr("X-Webhook-Signature", hexSig(cbBody)), strings.NewReader(cbBody)); r.Status != 503 {
		t.Errorf("a store failure makes the authority retry: %d", r.Status)
	}
}

func TestSharedTimestampedSchemeEnforcesTheReplayWindow(t *testing.T) {
	p, _, st, now := newCB("zoiko-v1")
	sign := func(at time.Time, id string) http.Header {
		s, _ := signature.Sign(context.Background(), at, id, []byte(cbBody), signature.LocalSigner([]byte("s3cret")))
		h := http.Header{}
		h.Set("X-Zoiko-Signature", s)
		h.Set("X-Zoiko-Event-Id", id)
		return h
	}
	if r := p.Handle(context.Background(), "taxauth", cbTenant, sign(now, "ev-1"), strings.NewReader(cbBody)); r.Status != 200 || len(st.calls) != 1 {
		t.Fatalf("%+v", r)
	}
	if r := p.Handle(context.Background(), "taxauth", cbTenant, sign(now, "ev-1"), strings.NewReader(cbBody)); r.Outcome != authority.OutcomeDuplicate || len(st.calls) != 1 {
		t.Fatalf("replay: %+v", r)
	}
	if r := p.Handle(context.Background(), "taxauth", cbTenant, sign(now.Add(-time.Hour), "ev-2"), strings.NewReader(cbBody)); r.Status != 401 || r.Outcome != "outside_replay_window" {
		t.Fatalf("stale: %+v", r)
	}
	h := sign(now, "ev-3")
	h.Set("X-Zoiko-Event-Id", "ev-4")
	if r := p.Handle(context.Background(), "taxauth", cbTenant, h, strings.NewReader(cbBody)); r.Status != 401 {
		t.Fatalf("swapped event id: %+v", r)
	}
}

func TestParseCallbackConfig(t *testing.T) {
	good := `{"taxauth":{"callback":{"scheme":"hmac-sha256-hex","event_ref_path":"event_id","payment_ref_path":"submission_ref","status_path":"status","status_map":{"ACC":"ACCEPTED"}}}}`
	if c, err := authority.ParseCallbackConfig(good); err != nil || len(c) != 1 {
		t.Fatalf("%v", err)
	}
	if c, err := authority.ParseCallbackConfig(""); err != nil || len(c) != 0 {
		t.Fatalf("empty means off: %v", err)
	}
	for name, bad := range map[string]string{
		"not json": `nope`, "bad key": `{"Bad Key":{"callback":{}}}`, "bad scheme": strings.Replace(good, "hmac-sha256-hex", "md5", 1),
		"settled is a bank status": strings.Replace(good, "ACCEPTED", "SETTLED", 1), "unknown field": strings.Replace(good, `"taxauth":{`, `"taxauth":{"extra":1,`, 1),
	} {
		if _, err := authority.ParseCallbackConfig(bad); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}
