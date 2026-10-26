package inbound_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

var (
	reqr = inbound.Actor{PrincipalID: "requester"}
	appr = inbound.Actor{PrincipalID: "approver"}
)

func (e *env) quarantined(tenant string, d deliv) *inbound.Exchange {
	e.t.Helper()
	o := e.deliver(d)
	if o.Exchange == nil || o.Exchange.State != inbound.StateQuarantined {
		e.t.Fatalf("not quarantined: %+v", o)
	}
	return o.Exchange
}

// ---- schema/mapping quarantine: correct, then release as a new attempt ---------------------------------------

func TestRelease_SchemaOrMappingQuarantineIsReplayedFromTheStoredBodyAfterCorrection(t *testing.T) {
	e := newEnv(t)
	// version 1 maps balances to 2 decimals; the provider sends 3
	v1 := e.publish(nil)
	c := e.activeConn(tenantA, v1, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	body := strings.Replace(goodBody("evt-1"), "10.5", "10.555", 1)
	x := e.quarantined(tenantA, deliv{conn: c, body: body, key: "k1"})
	if x.QuarantineClass != inbound.ClassMapping || x.ReasonCode != "PRECISION_LOSS" {
		t.Fatalf("%+v", x)
	}
	ctx := ctxFor(tenantA)

	// before any correction a release is approved but produces the same quarantine: a new attempt, nothing guessed
	req, err := e.gw.RequestAction(ctx, reqr, x.ExchangeID, "RELEASE", "provider confirmed 3-decimal balances")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.gw.DecideAction(ctx, reqr, x.ExchangeID, req.RequestID, true); !errors.Is(err, inbound.ErrSelfAction) {
		t.Fatalf("the requester approved their own release: %v", err)
	}
	ex, dq, err := e.gw.DecideAction(ctx, appr, x.ExchangeID, req.RequestID, true)
	if err != nil || dq.Status != inbound.ReqUsed || ex.State != inbound.StateQuarantined || ex.AttemptNo != 2 || ex.ReasonCode != "PRECISION_LOSS" {
		t.Fatalf("%v %+v", err, ex)
	}

	// the platform publishes a corrected contract (scale 3) and the connection migrates to it
	v2 := e.publish(func(in *registry.VersionInput) {
		in.Content.Mappings[0].Spec = []byte(strings.Replace(string(in.Content.Mappings[0].Spec), `"scale":2`, `"scale":3`, 1))
		in.ReplayAnalysisRef = "REPLAY-1 mapping scale widened"
	})
	mreq, err := e.connSvc.RequestMigration(ctx, creatorX, c.ConnectionID, migInput(v2.ContractVersionID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.connSvc.DecideMigration(ctx, approverX, c.ConnectionID, mreq.RequestID, true); err != nil {
		t.Fatal(err)
	}
	// the release now succeeds, as attempt 3, with the new mapping, and the exchange continues to the domain
	req, err = e.gw.RequestAction(ctx, reqr, x.ExchangeID, "RELEASE", "mapping corrected in 1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	ex, _, err = e.gw.DecideAction(ctx, appr, x.ExchangeID, req.RequestID, true)
	if err != nil || ex.State != inbound.StateValidated || ex.AttemptNo != 3 || ex.DomainOutcome != inbound.DomainPending || !strings.Contains(string(ex.Canonical), `"balance":"10.555"`) {
		t.Fatalf("%v %+v %s", err, ex, ex.Canonical)
	}
	e.driveAll()
	if got := e.exchange(tenantA, x.ExchangeID); got.State != inbound.StateHandedOff || got.DomainOutcome != inbound.DomainAccepted {
		t.Fatalf("%+v", got)
	}
	// the source identity and body hash of the original delivery never moved
	if got := e.exchange(tenantA, x.ExchangeID); got.BodyHash != x.BodyHash || got.SourceRefHash != x.SourceRefHash || got.ReceivedAt != x.ReceivedAt {
		t.Fatal("release rewrote the original delivery's identity")
	}
	events, _ := e.repo.Events(ctx, x.ExchangeID)
	kinds := ""
	for _, ev := range events {
		kinds += ev.Kind + ","
	}
	if !strings.Contains(kinds, "QUARANTINE_RELEASE_REQUESTED") || !strings.Contains(kinds, "QUARANTINE_RELEASED") || !hasType(e.repo.Types(), "integration.inbound.quarantine-released") {
		t.Fatalf("%s", kinds)
	}
}

// ---- the class rules ---------------------------------------------------------------------------------------------

func TestRelease_ClassRules(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	ctx := ctxFor(tenantA)

	auth := e.quarantined(tenantA, deliv{conn: c, body: strings.Replace(goodBody("a1"), tenantA, tenantB, 1), key: "ka"})
	data := e.quarantined(tenantA, deliv{conn: c, body: strings.Replace(goodBody("d1"), `"holder_account"`, `"holder":{"national_id":"X"},"holder_account"`, 1), key: "kd"})
	good := e.deliver(deliv{conn: c, body: goodBody("g1"), key: "kg"})
	e.deliver(deliv{conn: c, body: strings.Replace(goodBody("g1"), "10.5", "1.5", 1), key: "kg"}) // BODY_CHANGED: rejected, not quarantined

	for name, x := range map[string]*inbound.Exchange{"AUTH_CONTEXT": auth, "DATA_POLICY": data} {
		if _, err := e.gw.RequestAction(ctxForTenant(x), reqr, x.ExchangeID, "RELEASE", "please"); !errors.Is(err, inbound.ErrNotReleasable) {
			t.Errorf("%s: a release was allowed: %v", name, err)
		}
	}
	// every class can be dismissed, by someone else, with a reason
	req, err := e.gw.RequestAction(ctx, reqr, auth.ExchangeID, "DISMISS", "confirmed spoofing attempt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.gw.DecideAction(ctx, reqr, auth.ExchangeID, req.RequestID, true); !errors.Is(err, inbound.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	ex, _, err := e.gw.DecideAction(ctx, appr, auth.ExchangeID, req.RequestID, true)
	if err != nil || ex.State != inbound.StateRejected || ex.ReasonCode != "DISMISSED_AUTH_CONTEXT" {
		t.Fatalf("%v %+v", err, ex)
	}
	if !hasType(e.repo.Types(), "integration.inbound.quarantine-dismissed") {
		t.Fatal("no dismissed event")
	}
	// a dismissed exchange is final
	if _, err := e.gw.RequestAction(ctx, reqr, auth.ExchangeID, "DISMISS", "again"); !errors.Is(err, inbound.ErrTransition) {
		t.Fatalf("%v", err)
	}
	// a delivery that is not quarantined cannot be released or dismissed
	if _, err := e.gw.RequestAction(ctx, reqr, good.Exchange.ExchangeID, "DISMISS", "x"); !errors.Is(err, inbound.ErrTransition) {
		t.Fatalf("%v", err)
	}
	// input checks
	for name, a := range map[string][2]string{"bad action": {"DELETE", "x"}, "no reason": {"DISMISS", " "}, "long reason": {"DISMISS", strings.Repeat("x", 300)}} {
		if _, err := e.gw.RequestAction(ctx, reqr, data.ExchangeID, a[0], a[1]); !errors.Is(err, inbound.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// one open request at a time, and a rejected one frees the exchange
	r1, _ := e.gw.RequestAction(ctx, reqr, data.ExchangeID, "DISMISS", "policy resolved")
	if _, err := e.gw.RequestAction(ctx, reqr, data.ExchangeID, "DISMISS", "again"); !errors.Is(err, inbound.ErrRequestState) {
		t.Fatalf("%v", err)
	}
	if _, rq, err := e.gw.DecideAction(ctx, appr, data.ExchangeID, r1.RequestID, false); err != nil || rq.Status != inbound.ReqRejected {
		t.Fatalf("%v", err)
	}
	if got := e.exchange(tenantA, data.ExchangeID); got.State != inbound.StateQuarantined {
		t.Fatal("a rejected dismissal changed the exchange")
	}
	if _, err := e.gw.RequestAction(ctx, reqr, data.ExchangeID, "DISMISS", "retry"); err != nil {
		t.Fatalf("%v", err)
	}
	if _, _, err := e.gw.DecideAction(ctx, appr, data.ExchangeID, "00000000-0000-4000-8000-000000000000", true); !errors.Is(err, inbound.ErrRequestNotFound) {
		t.Fatalf("%v", err)
	}
	// a decided request does not decide again
	if _, _, err := e.gw.DecideAction(ctx, appr, data.ExchangeID, r1.RequestID, true); !errors.Is(err, inbound.ErrRequestState) {
		t.Fatalf("%v", err)
	}
}

func ctxForTenant(x *inbound.Exchange) context.Context { return ctxFor(x.TenantID) }

func TestRelease_IdempotencyQuarantineIsNeverReleased(t *testing.T) {
	e2 := newEnv(t)
	v := e2.publish(func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "idem-in" {
				in.Content.Profiles[i].Spec = []byte(`{"key_sources":["header:Idempotency-Key","body:/event_id"],"scope":"CONNECTION","retention_days":30,"on_body_change":"QUARANTINE"}`)
			}
		}
	})
	c := e2.activeConn(tenantA, v, "acc-a", refHMAC)
	e2.target("https://domain.example")
	e2.deliver(deliv{conn: c, body: goodBody("i1"), key: "ki"})
	x := e2.quarantined(tenantA, deliv{conn: c, body: strings.Replace(goodBody("i1"), "10.5", "7.5", 1), key: "ki"})
	if _, err := e2.gw.RequestAction(ctxFor(tenantA), reqr, x.ExchangeID, "RELEASE", "please"); !errors.Is(err, inbound.ErrNotReleasable) {
		t.Fatalf("a changed-body quarantine was releasable: %v", err)
	}
	r, err := e2.gw.RequestAction(ctxFor(tenantA), reqr, x.ExchangeID, "DISMISS", "investigated; sender will use a new key")
	if err != nil {
		t.Fatal(err)
	}
	if ex, _, err := e2.gw.DecideAction(ctxFor(tenantA), appr, x.ExchangeID, r.RequestID, true); err != nil || ex.State != inbound.StateRejected {
		t.Fatalf("%v", err)
	}
}

func TestRelease_DomainPreconditionRequeuesTheCanonicalPayload(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 409, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	e.driveAll() // the domain says 409: not admissible right now
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateQuarantined || x.QuarantineClass != inbound.ClassDomainPrecondition {
		t.Fatalf("%+v", x)
	}
	ctx := ctxFor(tenantA)
	req, err := e.gw.RequestAction(ctx, reqr, x.ExchangeID, "RELEASE", "the blocking case was closed")
	if err != nil {
		t.Fatal(err)
	}
	ex, _, err := e.gw.DecideAction(ctx, appr, x.ExchangeID, req.RequestID, true)
	if err != nil || ex.State != inbound.StateValidated || ex.AttemptNo != 2 || ex.HandoffAttempts != 0 || ex.DomainOutcome != inbound.DomainPending {
		t.Fatalf("%v %+v", err, ex)
	}
	e.driveAll()
	got := e.exchange(tenantA, x.ExchangeID)
	if got.State != inbound.StateHandedOff || got.DomainOutcome != inbound.DomainAccepted || dom.count() != 2 {
		t.Fatalf("%+v calls=%d", got, dom.count())
	}
	// the same idempotency key as the first attempt: the domain does not run the command twice
	if dom.reqs[0].Header.Get("Idempotency-Key") != dom.reqs[1].Header.Get("Idempotency-Key") {
		t.Fatal("the re-queued delivery used a different key")
	}
}

func TestRelease_NoRawBodyMeansTheSenderMustRedeliver(t *testing.T) {
	e := newEnv(t)
	e.gw.Cfg.RawRetention = 0
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	x := e.quarantined(tenantA, deliv{conn: c, body: strings.Replace(goodBody("e1"), "10.5", "10.555", 1), key: "k"})
	if _, err := e.gw.RequestAction(ctxFor(tenantA), reqr, x.ExchangeID, "RELEASE", "x"); !errors.Is(err, inbound.ErrNoRaw) {
		t.Fatalf("%v", err)
	}
	// and one whose retention has passed
	e2 := newEnv(t)
	v2 := e2.publish(nil)
	c2 := e2.activeConn(tenantA, v2, "acc-a", refHMAC)
	e2.target("https://domain.example")
	x2 := e2.quarantined(tenantA, deliv{conn: c2, body: strings.Replace(goodBody("e1"), "10.5", "10.555", 1), key: "k"})
	e2.advance(31 * 24 * time.Hour)
	_, _ = e2.repo.PurgeExpired(context.Background(), e2.at(), 10)
	if _, err := e2.gw.RequestAction(ctxFor(tenantA), reqr, x2.ExchangeID, "RELEASE", "x"); !errors.Is(err, inbound.ErrNoRaw) {
		t.Fatalf("%v", err)
	}
}

func TestRelease_TenantIsolation(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	x := e.quarantined(tenantA, deliv{conn: c, body: strings.Replace(goodBody("e1"), "10.5", "10.555", 1), key: "k"})
	b := ctxFor(tenantB)
	if _, err := e.gw.RequestAction(b, reqr, x.ExchangeID, "DISMISS", "x"); !errors.Is(err, inbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, _, err := e.gw.DecideAction(b, appr, x.ExchangeID, "r", true); !errors.Is(err, inbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.repo.Get(b, x.ExchangeID); !errors.Is(err, inbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// ---- authentication primitives ------------------------------------------------------------------------------------

func authInfo() registry.AuthInfo {
	return registry.AuthInfo{Mechanism: "HMAC", SignatureHeader: "X-Signature", SignatureEncoding: "hex", SignedContent: "TIMESTAMP_DOT_BODY",
		TimestampHeader: "X-Timestamp", TimestampFormat: "UNIX_SECONDS", ReplayWindowSeconds: 300, SignatureAlgorithm: "HMAC-SHA256", APIKeyHeader: "X-Api-Key"}
}

func mac(alg func() hash.Hash, key, msg string) []byte {
	m := hmac.New(alg, []byte(key))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func TestVerifyHMAC_Variants(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"a":1}`)
	hdr := func(sig, tsv string) http.Header {
		h := http.Header{}
		h.Set("X-Signature", sig)
		h.Set("X-Timestamp", tsv)
		return h
	}
	hexSig := func(alg func() hash.Hash, key, msg string) string { return hex.EncodeToString(mac(alg, key, msg)) }
	sha256New := func() hash.Hash { return sha256.New() }
	sha512New := func() hash.Hash { return sha512.New() }
	sha384New := func() hash.Hash { return sha512.New384() }

	info := authInfo()
	// hex SHA-256 over "ts.body"
	if r, err := inbound.VerifyHMAC(info, hdr(hexSig(sha256New, "k", ts+"."+string(body)), ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil || r.KeyLabel != "current" {
		t.Fatalf("%v %+v", err, r)
	}
	// a prefix such as "sha256=" is tolerated
	if _, err := inbound.VerifyHMAC(info, hdr("sha256="+hexSig(sha256New, "k", ts+"."+string(body)), ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("prefixed: %v", err)
	}
	// base64 encoding and SHA-512 / SHA-384
	b64 := info
	b64.SignatureEncoding, b64.SignatureAlgorithm = "base64", "HMAC-SHA512"
	sig := base64.StdEncoding.EncodeToString(mac(sha512New, "k", ts+"."+string(body)))
	if _, err := inbound.VerifyHMAC(b64, hdr(sig, ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("base64/sha512: %v", err)
	}
	s384 := info
	s384.SignatureAlgorithm = "HMAC-SHA384"
	if _, err := inbound.VerifyHMAC(s384, hdr(hexSig(sha384New, "k", ts+"."+string(body)), ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("sha384: %v", err)
	}
	// body-only signing
	bo := info
	bo.SignedContent = "BODY"
	if _, err := inbound.VerifyHMAC(bo, hdr(hexSig(sha256New, "k", string(body)), ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("body only: %v", err)
	}
	// the wrong algorithm for the key is a failure, not a pass
	if _, err := inbound.VerifyHMAC(s384, hdr(hexSig(sha256New, "k", ts+"."+string(body)), ts), body, [][]byte{[]byte("k")}, []string{"current"}, now); !errors.Is(err, inbound.ErrAuthFailed) {
		t.Fatalf("%v", err)
	}
	// millisecond and RFC 3339 timestamps
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	if _, err := inbound.VerifyHMAC(info, hdr(hexSig(sha256New, "k", ms+"."+string(body)), ms), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("ms: %v", err)
	}
	rf := info
	rf.TimestampFormat = "RFC3339"
	rts := now.UTC().Format(time.RFC3339)
	if _, err := inbound.VerifyHMAC(rf, hdr(hexSig(sha256New, "k", rts+"."+string(body)), rts), body, [][]byte{[]byte("k")}, []string{"current"}, now); err != nil {
		t.Fatalf("rfc3339: %v", err)
	}
	// failure shapes
	good := hexSig(sha256New, "k", ts+"."+string(body))
	for name, c := range map[string]struct {
		h    http.Header
		body []byte
		keys [][]byte
	}{
		"empty key":        {hdr(good, ts), body, [][]byte{{}}},
		"no keys":          {hdr(good, ts), body, nil},
		"truncated sig":    {hdr(good[:20], ts), body, [][]byte{[]byte("k")}},
		"one flipped byte": {hdr(strings.Replace(good, good[:1], "0", 1)+"", ts), body, [][]byte{[]byte("k")}},
		"altered body":     {hdr(good, ts), []byte(`{"a":2}`), [][]byte{[]byte("k")}},
		"timestamp moved":  {hdr(good, strconv.FormatInt(now.Unix()+1, 10)), body, [][]byte{[]byte("k")}},
	} {
		labels := make([]string, len(c.keys))
		if _, err := inbound.VerifyHMAC(info, c.h, c.body, c.keys, labels, now); err == nil {
			t.Errorf("%s verified", name)
		}
	}
	// the replay window is judged only after the signature
	old := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	if r, err := inbound.VerifyHMAC(info, hdr(hexSig(sha256New, "k", old+"."+string(body)), old), body, [][]byte{[]byte("k")}, []string{"current"}, now); !errors.Is(err, inbound.ErrReplayWindow) || r.KeyLabel != "current" {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := inbound.VerifyHMAC(info, hdr(hexSig(sha256New, "wrong", old+"."+string(body)), old), body, [][]byte{[]byte("k")}, []string{"current"}, now); !errors.Is(err, inbound.ErrAuthFailed) {
		t.Fatalf("an invalid signature must not reveal the window: %v", err)
	}
	// the matching key is reported even when it is not first
	if r, err := inbound.VerifyHMAC(info, hdr(good, ts), body, [][]byte{[]byte("other"), []byte("k")}, []string{"current", "previous"}, now); err != nil || r.KeyLabel != "previous" {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestVerifyAPIKey(t *testing.T) {
	info := authInfo()
	h := http.Header{}
	h.Set("X-Api-Key", " secret-key ")
	if r, err := inbound.VerifyAPIKey(info, h, [][]byte{[]byte("secret-key\n")}, []string{"current"}); err != nil || r.Mechanism != "API_KEY_HEADER" {
		t.Fatalf("%v", err)
	}
	for name, hv := range map[string]string{"wrong": "nope", "empty": "", "prefix": "secret", "longer": "secret-key-extra"} {
		h := http.Header{}
		if hv != "" {
			h.Set("X-Api-Key", hv)
		}
		if _, err := inbound.VerifyAPIKey(info, h, [][]byte{[]byte("secret-key")}, []string{"current"}); !errors.Is(err, inbound.ErrAuthFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := inbound.VerifyAPIKey(info, http.Header{}, [][]byte{{}}, []string{"current"}); !errors.Is(err, inbound.ErrAuthFailed) {
		t.Fatal("an empty key matched an empty header")
	}
}

func TestVerifyMTLS_EdgeAttestation_NP04(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	edge := []byte("edge-shared-secret")
	fp := strings.Repeat("ab", 32)
	mk := func(fingerprint, att, tsv string) http.Header {
		h := http.Header{}
		h.Set(inbound.HeaderCertFingerprint, fingerprint)
		h.Set(inbound.HeaderEdgeTimestamp, tsv)
		h.Set(inbound.HeaderEdgeAttestation, att)
		return h
	}
	good := inbound.EdgeAttestation(edge, fp, ts)
	bound := [][]byte{[]byte(strings.Repeat("cd", 32) + "\n" + fp + "\n")}
	if r, err := inbound.VerifyMTLS(mk(fp, good, ts), edge, bound, []string{"current"}, now); err != nil || r.Mechanism != "MTLS" {
		t.Fatalf("%v", err)
	}
	// a certificate bound to ANOTHER connection is just an unknown certificate here, however valid it is
	other := strings.Repeat("ef", 32)
	if _, err := inbound.VerifyMTLS(mk(other, inbound.EdgeAttestation(edge, other, ts), ts), edge, bound, []string{"current"}, now); !errors.Is(err, inbound.ErrAuthFailed) {
		t.Fatalf("%v", err)
	}
	for name, h := range map[string]http.Header{
		"bare fingerprint, no attestation":    mk(fp, "", ts),
		"forged attestation":                  mk(fp, inbound.EdgeAttestation([]byte("guess"), fp, ts), ts),
		"attestation for another fingerprint": mk(fp, inbound.EdgeAttestation(edge, other, ts), ts),
		"stale attestation":                   mk(fp, inbound.EdgeAttestation(edge, fp, strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)), strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)),
		"no timestamp":                        mk(fp, good, ""),
	} {
		if _, err := inbound.VerifyMTLS(h, edge, bound, []string{"current"}, now); !errors.Is(err, inbound.ErrAuthFailed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// no edge secret configured: the mechanism cannot be used at all
	if _, err := inbound.VerifyMTLS(mk(fp, good, ts), nil, bound, []string{"current"}, now); !errors.Is(err, inbound.ErrAuthFailed) {
		t.Fatalf("%v", err)
	}
}
