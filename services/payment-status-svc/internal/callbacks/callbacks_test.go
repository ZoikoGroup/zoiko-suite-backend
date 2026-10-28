package callbacks_test

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
	"zoiko.io/payment-status-svc/internal/callbacks"
	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/store"
)

const tenant = "7e57ab1e-0000-4000-8000-0000000000b1"

type fakeSecrets struct {
	secret []byte
	err    error
	paths  []string
}

func (f *fakeSecrets) Secrets(_ context.Context, tenantID, path string) ([][]byte, error) {
	f.paths = append(f.paths, tenantID+"|"+path)
	if f.err != nil {
		return nil, f.err
	}
	return [][]byte{append([]byte(nil), f.secret...)}, nil
}

type fakeStore struct {
	calls []store.CallbackInput
	res   *store.CallbackResult
	err   error
}

func (f *fakeStore) ApplyProviderCallback(_ context.Context, in store.CallbackInput) (*store.CallbackResult, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}
func (f *fakeStore) ListReceipts(context.Context, string, bool, string, int) ([]store.Receipt, error) {
	return nil, nil
}

func cbCfg(scheme string) callbacks.Config {
	return callbacks.Config{"simbank": {Callback: adapter.Callback{
		Scheme: scheme, ToleranceSeconds: 300, EventRefPath: "event_id", PaymentRefPath: "payment_id", StatusPath: "status", EventTimePath: "at",
		StatusMap: map[string]string{"ACCP": "ACKNOWLEDGED", "PDNG": "PENDING", "BOOK": "SETTLED", "RJCT": "REJECTED"},
	}}}
}

func newProc(scheme string) (*callbacks.Processor, *fakeSecrets, *fakeStore, *time.Time) {
	now := time.Now().UTC()
	fs := &fakeSecrets{secret: []byte("s3cret")}
	st := &fakeStore{res: &store.CallbackResult{Outcome: store.OutcomeApplied}}
	return &callbacks.Processor{Config: cbCfg(scheme), Secrets: fs, Store: st, Guard: signature.NewMemoryGuard(100, func() time.Time { return now }),
		Log: zap.NewNop(), Now: func() time.Time { return now }}, fs, st, &now
}

const body = `{"event_id":"E1","payment_id":"P1","status":"BOOK","at":"2026-01-02T03:04:05Z"}`

func hexSig(b string) string {
	s, _ := adapter.HMACSigner([]byte("s3cret"))(context.Background(), []byte(b))
	return s
}

func hdr(k, v string) http.Header { h := http.Header{}; h.Set(k, v); return h }

func TestSignedCallbackIsAppliedWithTheProfilesMapping(t *testing.T) {
	p, fs, st, _ := newProc("hmac-sha256-hex")
	r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body))
	if r.Status != 200 || r.Outcome != store.OutcomeApplied {
		t.Fatalf("%+v", r)
	}
	if len(st.calls) != 1 {
		t.Fatalf("store calls %d", len(st.calls))
	}
	c := st.calls[0]
	if c.Mapped != domain.StatusSettled || c.EventType != domain.EventPaymentSettled || c.EventRef != "E1" || c.PaymentRef != "P1" ||
		c.RawStatus != "BOOK" || c.Provider != "simbank" || c.EventTime == nil || len(c.BodyHash) != 64 || !strings.HasPrefix(c.MappingVersion, "simbank:") {
		t.Fatalf("%+v", c)
	}
	// the secret is fetched for THIS tenant and provider
	if fs.paths[0] != tenant+"|kv/callbacks/"+tenant+"/simbank" {
		t.Fatalf("secret path: %v", fs.paths)
	}
}

func TestEveryProfileStatusMapsToAPaymentStatus(t *testing.T) {
	for raw, want := range map[string]domain.ExecutionStatus{"ACCP": domain.StatusAccepted, "PDNG": domain.StatusPending, "BOOK": domain.StatusSettled, "RJCT": domain.StatusRejected} {
		p, _, st, _ := newProc("hmac-sha256-hex")
		b := strings.Replace(body, "BOOK", raw, 1)
		if r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(b)), strings.NewReader(b)); r.Status != 200 {
			t.Fatalf("%s: %+v", raw, r)
		}
		if st.calls[0].Mapped != want {
			t.Errorf("%s: %s", raw, st.calls[0].Mapped)
		}
	}
}

// acceptance: callback signature failure
func TestBadSignatureWritesNothingAndLeaksNothing(t *testing.T) {
	for name, h := range map[string]http.Header{
		"wrong":      hdr("X-Webhook-Signature", strings.Repeat("a", 64)),
		"missing":    {},
		"short":      hdr("X-Webhook-Signature", "abc"),
		"other body": hdr("X-Webhook-Signature", hexSig(`{"event_id":"E1","payment_id":"P1","status":"RJCT"}`)),
	} {
		p, _, st, _ := newProc("hmac-sha256-hex")
		r := p.Handle(context.Background(), "simbank", tenant, h, strings.NewReader(body))
		if r.Status != 401 || r.Outcome != "signature_invalid" {
			t.Errorf("%s: %+v", name, r)
		}
		if len(st.calls) != 0 {
			t.Errorf("%s: an unauthenticated callback reached the store", name)
		}
		if p.Count.SignatureFailed.Load() != 1 {
			t.Errorf("%s: not counted", name)
		}
	}
	// the body of the refusal says nothing about why
	p, _, _, _ := newProc("hmac-sha256-hex")
	r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", strings.Repeat("a", 64)), strings.NewReader(body))
	if len(r.Body) != 1 || r.Body["error"] != "signature_invalid" {
		t.Fatalf("%v", r.Body)
	}
}

// a callback signed with ANOTHER tenant's secret is refused: the tenant in the URL fixes which secret verifies
func TestTheTenantInTheURLFixesWhichSecretVerifies(t *testing.T) {
	p, fs, st, _ := newProc("hmac-sha256-hex")
	fs.secret = []byte("tenant-B-secret") // the secret for the tenant in the URL is not the one the caller signed with
	r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body))
	if r.Status != 401 || len(st.calls) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownProviderTenantAndSecret(t *testing.T) {
	p, fs, st, _ := newProc("hmac-sha256-hex")
	if r := p.Handle(context.Background(), "nobank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body)); r.Status != 404 {
		t.Errorf("unknown provider: %d", r.Status)
	}
	for _, bad := range []string{"not-a-uuid", "", "../etc"} {
		if r := p.Handle(context.Background(), "simbank", bad, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body)); r.Status != 404 {
			t.Errorf("tenant %q: %d", bad, r.Status)
		}
	}
	fs.err = callbacks.ErrSecretUnknown
	if r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body)); r.Status != 401 {
		t.Errorf("no secret configured is indistinguishable from a bad signature: %d", r.Status)
	}
	fs.err = errors.New("vault down")
	if r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body)); r.Status != 503 {
		t.Errorf("a vault outage must make the provider retry, not drop: %d", r.Status)
	}
	if len(st.calls) != 0 {
		t.Fatal("nothing may be applied")
	}
}

func TestMalformedAndUnmappedAreRefusedAfterAuthentication(t *testing.T) {
	p, _, st, _ := newProc("hmac-sha256-hex")
	for name, c := range map[string]struct {
		b    string
		want int
	}{
		"not json":     {`nope`, 400},
		"no event ref": {`{"payment_id":"P1","status":"BOOK"}`, 400},
		"no payment":   {`{"event_id":"E1","status":"BOOK"}`, 400},
		"unmapped":     {`{"event_id":"E1","payment_id":"P1","status":"WHAT"}`, 422},
	} {
		r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(c.b)), strings.NewReader(c.b))
		if r.Status != c.want {
			t.Errorf("%s: %d want %d", name, r.Status, c.want)
		}
	}
	big := `{"event_id":"E1","payment_id":"P1","status":"BOOK","pad":"` + strings.Repeat("x", 300<<10) + `"}`
	if r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(big)), bytes.NewReader([]byte(big))); r.Status != 413 {
		t.Errorf("oversized: %d", r.Status)
	}
	if len(st.calls) != 0 {
		t.Fatal("none of these may be applied")
	}
}

func TestStoreOutcomesMapToResponses(t *testing.T) {
	for outcome, want := range map[string]int{
		store.OutcomeApplied: 200, store.OutcomeDuplicate: 200, store.OutcomeRegressionBlocked: 200, store.OutcomeStaleIgnored: 200,
		store.OutcomeUnknownPayment: 202, store.OutcomeAmbiguousPayment: 202,
		store.OutcomeConflictBlocked: 202, store.OutcomeProviderMismatch: 202,
	} {
		p, _, st, _ := newProc("hmac-sha256-hex")
		st.res = &store.CallbackResult{Outcome: outcome}
		r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body))
		if r.Status != want || r.Body["outcome"] != outcome {
			t.Errorf("%s: %+v", outcome, r)
		}
	}
	p, _, st, _ := newProc("hmac-sha256-hex")
	st.err = errors.New("db down")
	if r := p.Handle(context.Background(), "simbank", tenant, hdr("X-Webhook-Signature", hexSig(body)), strings.NewReader(body)); r.Status != 503 {
		t.Errorf("a store failure makes the provider retry: %d", r.Status)
	}
}

func TestSharedTimestampedSchemeEnforcesTheReplayWindowAndOnce(t *testing.T) {
	p, _, st, now := newProc("zoiko-v1")
	sign := func(at time.Time, eventID string) http.Header {
		s, _ := signature.Sign(context.Background(), at, eventID, []byte(body), signature.LocalSigner([]byte("s3cret")))
		h := http.Header{}
		h.Set("X-Zoiko-Signature", s)
		h.Set("X-Zoiko-Event-Id", eventID)
		return h
	}
	if r := p.Handle(context.Background(), "simbank", tenant, sign(*now, "ev-1"), strings.NewReader(body)); r.Status != 200 || len(st.calls) != 1 {
		t.Fatalf("%+v", r)
	}
	// the very same signed message again: refused as a replay before it reaches the store
	if r := p.Handle(context.Background(), "simbank", tenant, sign(*now, "ev-1"), strings.NewReader(body)); r.Outcome != store.OutcomeDuplicate || len(st.calls) != 1 {
		t.Fatalf("replay: %+v calls=%d", r, len(st.calls))
	}
	// stale: correctly signed, but outside the window
	if r := p.Handle(context.Background(), "simbank", tenant, sign(now.Add(-time.Hour), "ev-2"), strings.NewReader(body)); r.Status != 401 || r.Outcome != "outside_replay_window" {
		t.Fatalf("stale: %+v", r)
	}
	// future-dated beyond the window too
	if r := p.Handle(context.Background(), "simbank", tenant, sign(now.Add(time.Hour), "ev-3"), strings.NewReader(body)); r.Status != 401 {
		t.Fatalf("future: %+v", r)
	}
	// the event id is part of what is signed
	h := sign(*now, "ev-4")
	h.Set("X-Zoiko-Event-Id", "ev-5")
	if r := p.Handle(context.Background(), "simbank", tenant, h, strings.NewReader(body)); r.Status != 401 {
		t.Fatalf("swapped event id: %+v", r)
	}
	if len(st.calls) != 1 {
		t.Fatalf("calls %d", len(st.calls))
	}
}

func TestParseConfig(t *testing.T) {
	good := `{"simbank":{"callback":{"scheme":"hmac-sha256-hex","event_ref_path":"event_id","payment_ref_path":"payment_id","status_path":"status","status_map":{"BOOK":"SETTLED"}}}}`
	c, err := callbacks.ParseConfig(good)
	if err != nil || len(c) != 1 {
		t.Fatalf("%v", err)
	}
	if c, err := callbacks.ParseConfig(""); err != nil || len(c) != 0 {
		t.Fatalf("empty means off: %v", err)
	}
	for name, bad := range map[string]string{
		"not json":      `nope`,
		"bad key":       `{"Bad Key":{"callback":{}}}`,
		"bad scheme":    strings.Replace(good, "hmac-sha256-hex", "md5", 1),
		"no status map": strings.Replace(good, `"status_map":{"BOOK":"SETTLED"}`, `"status_map":{}`, 1),
		"unknown field": strings.Replace(good, `"simbank":{`, `"simbank":{"extra":1,`, 1),
		"bad class":     strings.Replace(good, "SETTLED", "RETURNED", 1),
	} {
		if _, err := callbacks.ParseConfig(bad); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestGuardsAgainstUnknownProviderCounters(t *testing.T) {
	p, _, _, _ := newProc("hmac-sha256-hex")
	p.Handle(context.Background(), "nobank", tenant, http.Header{}, strings.NewReader(body))
	if p.Count.UnknownProvider.Load() != 1 {
		t.Fatal("refusals are counted")
	}
}
