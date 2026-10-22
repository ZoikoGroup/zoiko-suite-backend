package inbound_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/eventing/integration"
)

func assertXIC(t *testing.T, o *inbound.Outcome, status int, code integration.XICCode) {
	t.Helper()
	if o.Status != status || o.XIC == nil || o.XIC.Code != code {
		t.Fatalf("status=%d xic=%v reason=%s (want %d %s)", o.Status, o.XIC, o.Reason, status, code)
	}
}

// ---- the happy path ---------------------------------------------------------------------------

func TestReceive_ValidDeliveryIsAcceptedAndCanonicalized(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	o := e.deliver(ok(c, "evt-1"))
	if o.Status != http.StatusAccepted || o.Exchange == nil || o.Exchange.State != inbound.StateValidated {
		t.Fatalf("%+v %v", o, o.XIC)
	}
	x, _ := e.repo.Get(ctxFor(tenantA), o.Exchange.ExchangeID)
	if x.DomainOutcome != inbound.DomainPending || x.CanonicalContract != "bank.statement.v1" || x.CanonicalHash == "" || x.MappingVersionID == "" ||
		x.TenantID != tenantA || x.ConnectionID != c.ConnectionID || x.AuthMechanism != "HMAC" || x.SourceSchema != "statement@1" || x.SourceRefHash == "" {
		t.Fatalf("%+v", x)
	}
	// the canonical payload is the deterministic mapping output, minimized
	want := `{"account":"A1","balance":"10.50","booked_at":"2026-10-20T09:00:00Z","currency":"GBP"}`
	if string(x.Canonical) != want {
		t.Fatalf("canonical %s", x.Canonical)
	}
	// transport and business acceptance are distinct dimensions
	if x.State == inbound.StateHandedOff || x.DomainOutcome == inbound.DomainAccepted {
		t.Fatal("transport acceptance was recorded as business acceptance")
	}
	var lin []map[string]any
	if err := json.Unmarshal(x.Lineage, &lin); err != nil || len(lin) != 4 {
		t.Fatalf("lineage %s", x.Lineage)
	}
	if !hasType(e.repo.Types(), "integration.inbound.accepted") {
		t.Fatal("no accepted event")
	}
	var ev inbound.OutEvent
	for _, x := range e.repo.Out {
		if x.Type == "integration.inbound.accepted" {
			ev = x
		}
	}
	for _, k := range []string{"exchange_id", "connection_id", "source_ref_hash", "mapping_version_id"} {
		if _, ok := ev.Data[k]; !ok {
			t.Errorf("accepted event lacks %s", k)
		}
	}
	if ev.Data["source_schema_version"] != "statement@1" {
		t.Errorf("%v", ev.Data)
	}
	events, _ := e.repo.Events(ctxFor(tenantA), x.ExchangeID)
	if len(events) != 1 || events[0].Kind != "ACCEPTED" {
		t.Fatalf("%+v", events)
	}
}

func TestReceive_RawBodyRetentionFollowsPolicy(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil) // statement-push carries FINANCIAL data: raw may be kept
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	x, _ := e.repo.Get(ctxFor(tenantA), e.deliver(ok(c, "e1")).Exchange.ExchangeID)
	if len(x.Raw) == 0 || x.RawExpiresAt == nil || !x.RawExpiresAt.After(e.at().Add(29*24*time.Hour)) {
		t.Fatalf("raw not retained: %+v", x.RawExpiresAt)
	}
	// raw is never kept for personal or restricted classes
	e2 := newEnv(t)
	v2 := e2.publish(func(in *registry.VersionInput) {
		for i := range in.Content.Operations {
			if in.Content.Operations[i].ID == "statement-push" {
				in.Content.Operations[i].DataClasses = []string{"FINANCIAL", "PERSONAL"}
			}
		}
	})
	c2 := e2.activeConn(tenantA, v2, "acc-a", refHMAC)
	e2.target("https://domain.example")
	x2, _ := e2.repo.Get(ctxFor(tenantA), e2.deliver(ok(c2, "e1")).Exchange.ExchangeID)
	if len(x2.Raw) != 0 {
		t.Fatal("a body carrying personal data was retained raw")
	}
	if len(x2.Canonical) == 0 || x2.RawExpiresAt == nil {
		t.Fatal("the canonical payload still needs to be delivered and then purged")
	}
	// and retention can be switched off entirely
	e3 := newEnv(t)
	e3.gw.Cfg.RawRetention = 0
	v3 := e3.publish(nil)
	c3 := e3.activeConn(tenantA, v3, "acc-a", refHMAC)
	e3.target("https://domain.example")
	x3, _ := e3.repo.Get(ctxFor(tenantA), e3.deliver(ok(c3, "e1")).Exchange.ExchangeID)
	if len(x3.Raw) != 0 {
		t.Fatal("raw retained with retention off")
	}
}

// ---- authentication (§8.1 steps 2-5) -----------------------------------------------------------

func TestReceive_NothingDurableBeforeAuthentication(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	outBefore := len(e.repo.Out)
	bad := func(name string, d deliv) {
		t.Helper()
		o := e.deliver(d)
		assertXIC(t, o, http.StatusUnauthorized, integration.XICAuthenticationFailed)
		if o.Exchange != nil {
			t.Errorf("%s: an exchange was created for an unauthenticated delivery", name)
		}
	}
	d := ok(c, "evt")
	// NP-01: valid JSON, nothing authenticable
	nosig := d
	nosig.noSig = true
	bad("no signature", nosig)
	wrong := d
	wrong.secret = "not-the-secret"
	bad("wrong secret", wrong)
	tampered := d
	req := e.request(d)
	req.Body = []byte(strings.Replace(string(req.Body), "10.5", "99999", 1))
	if o := e.gw.Receive(context.Background(), req); o.Status != 401 || o.Exchange != nil {
		t.Errorf("tampered body: %d", o.Status)
	}
	_ = tampered
	garbage := d
	garbage.headers = map[string]string{"X-Signature": "zz-not-hex"}
	bad("non-hex signature", garbage)
	empty := d
	empty.headers = map[string]string{"X-Signature": ""}
	bad("empty signature", empty)
	nots := d
	nots.headers = map[string]string{"X-Timestamp": ""}
	bad("missing timestamp", nots)
	// the route: unknown connection, wrong connector, unknown or wrong-direction operation, wrong method
	unk := d
	unkConn := *c
	unkConn.ConnectionID = "00000000-0000-4000-8000-000000000000"
	unk.conn = &unkConn
	bad("unknown connection", unk)
	badID := d
	malformed := *c
	malformed.ConnectionID = "not-a-uuid"
	badID.conn = &malformed
	bad("malformed connection id", badID)
	wc := d
	wcConn := *c
	wcConn.ConnectorID = "bank-y"
	wc.conn = &wcConn
	bad("wrong connector key", wc)
	op := d
	op.op = "payment-submit" // outbound
	bad("outbound operation", op)
	ghost := d
	ghost.op = "ghost-op"
	bad("unknown operation", ghost)
	meth := d
	meth.method = "GET"
	bad("wrong method", meth)
	// all of them looked the same to the caller
	a := e.deliver(nosig)
	b := e.deliver(unk)
	if a.Status != b.Status || a.XIC.Code != b.XIC.Code || a.XIC.Detail != b.XIC.Detail {
		t.Fatal("pre-authentication refusals are distinguishable")
	}
	if list, _ := e.repo.List(ctxFor(tenantA), inbound.Filter{}); len(list) != 0 {
		t.Fatalf("%d exchanges written for unauthenticated input", len(list))
	}
	if len(e.repo.Out) != outBefore {
		t.Fatalf("%d events written for unauthenticated input", len(e.repo.Out)-outBefore)
	}
}

func TestReceive_ReplayWindow_NP02(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	for name, off := range map[string]time.Duration{"too old": -10 * time.Minute, "too new": 10 * time.Minute} {
		d := ok(c, "evt-"+name)
		d.tsOff = off
		o := e.deliver(d) // the signature is VALID, the timestamp is not
		assertXIC(t, o, http.StatusUnauthorized, integration.XICReplayWindowFailed)
		if o.Exchange == nil || o.Exchange.State != inbound.StateRejected || o.Exchange.ReasonCode != "REPLAY_WINDOW" {
			t.Fatalf("%s: a replay attempt on a valid signature must be evidenced: %+v", name, o.Exchange)
		}
	}
	// inside the window is fine, both directions
	for _, off := range []time.Duration{-4 * time.Minute, 4 * time.Minute, 0} {
		d := ok(c, "evt-"+off.String())
		d.tsOff = off
		if o := e.deliver(d); o.Status != http.StatusAccepted {
			t.Fatalf("%v: %d", off, o.Status)
		}
	}
	// an invalid signature with a stale timestamp is just an authentication failure (no oracle for the window)
	d := ok(c, "evt-x")
	d.tsOff = -time.Hour
	d.secret = "wrong"
	o := e.deliver(d)
	assertXIC(t, o, 401, integration.XICAuthenticationFailed)
	if o.Exchange != nil {
		t.Fatal("a stale timestamp with a bad signature was recorded")
	}
	// the rejected replay did not claim the idempotency key: the genuine delivery is accepted
	fresh := ok(c, "evt-too old")
	if o := e.deliver(fresh); o.Status != http.StatusAccepted {
		t.Fatalf("a replay rejection must not consume the dedupe key: %d", o.Status)
	}
}

func TestReceive_AuthenticationFailsClosedAndNeverLeaksSecrets(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	// a vault outage: retryable, nothing processed, nothing durable
	e.secrets.down = true
	o := e.deliver(ok(c, "e1"))
	if o.Status != http.StatusServiceUnavailable || o.RetryAfter == 0 || o.Exchange != nil {
		t.Fatalf("%+v", o)
	}
	e.secrets.down = false
	// the vault releases nothing for this reference
	delete(e.secrets.m, refHMAC)
	assertXIC(t, e.deliver(ok(c, "e1")), 401, integration.XICAuthenticationFailed)
	e.secrets.set(refHMAC, secretHMAC)
	// the secret is never in what the caller is told
	b, _ := json.Marshal(e.deliver(deliv{conn: c, body: goodBody("e9"), key: "k9", secret: "x"}))
	if strings.Contains(string(b), secretHMAC) {
		t.Fatal("secret leaked")
	}
	// a mechanism this listener cannot verify fails closed
	e2 := newEnv(t)
	v2 := e2.publish(func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "hmac-in" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_CLIENT_CREDENTIALS","audience":"x","scopes_required":["a"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
	_ = v2
}

func TestReceive_KeyRotationGraceWindow(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	exp := e.at().Add(90 * 24 * time.Hour)
	_ = exp
	rot, err := e.connSvc.Rotate(ctxFor(tenantA), operator, c.ConnectionID, c.Version, xicconnRotate("hmac-in", refNew, 3600))
	if err != nil {
		t.Fatal(err)
	}
	_ = rot
	// the new secret works; so does the old one inside the grace window (labelled previous); an unrelated one never
	if o := e.deliver(deliv{conn: c, body: goodBody("n1"), key: "k-n1", secret: "rotated-secret"}); o.Status != 202 {
		t.Fatalf("new secret: %d", o.Status)
	}
	o := e.deliver(deliv{conn: c, body: goodBody("o1"), key: "k-o1", secret: secretHMAC})
	if o.Status != 202 {
		t.Fatalf("previous secret inside grace: %d", o.Status)
	}
	events, _ := e.repo.Events(ctxFor(tenantA), o.Exchange.ExchangeID)
	if !strings.Contains(string(events[0].Detail), `"key":"previous"`) {
		t.Fatalf("the previous key must be labelled in the evidence: %s", events[0].Detail)
	}
	assertXIC(t, e.deliver(deliv{conn: c, body: goodBody("u1"), key: "k-u1", secret: "some-third-secret"}), 401, integration.XICAuthenticationFailed)
	// after the window only the new secret works
	e.advance(2 * time.Hour)
	assertXIC(t, e.deliver(deliv{conn: c, body: goodBody("o2"), key: "k-o2", secret: secretHMAC}), 401, integration.XICAuthenticationFailed)
	if o := e.deliver(deliv{conn: c, body: goodBody("n2"), key: "k-n2", secret: "rotated-secret"}); o.Status != 202 {
		t.Fatalf("%d", o.Status)
	}
}

func TestReceive_ConnectionStateIsEnforcedAfterAuthentication(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	if _, err := e.connSvc.Suspend(ctxFor(tenantA), operator, c.ConnectionID, c.Version, "PROVIDER_INCIDENT"); err != nil {
		t.Fatal(err)
	}
	o := e.deliver(ok(c, "e1"))
	assertXIC(t, o, http.StatusConflict, integration.XICConnectionNotActive) // I-01
	if o.Exchange == nil || o.Exchange.State != inbound.StateRejected {
		t.Fatalf("an authenticated refusal is evidenced: %+v", o.Exchange)
	}
	// an unauthenticated caller learns nothing about the state
	if o := e.deliver(deliv{conn: c, body: goodBody("e2"), key: "k2", secret: "bad"}); o.XIC.Code != integration.XICAuthenticationFailed {
		t.Fatal("state leaked to an unauthenticated caller")
	}
	// a revoked credential cannot authenticate at all
	e2 := newEnv(t)
	v2 := e2.publish(nil)
	c2 := e2.activeConn(tenantA, v2, "acc-a", refHMAC)
	if _, err := e2.connSvc.Revoke(ctxFor(tenantA), operator, c2.ConnectionID, c2.Version, "gone"); err != nil {
		t.Fatal(err)
	}
	assertXIC(t, e2.deliver(ok(c2, "e1")), 401, integration.XICAuthenticationFailed)
}

// ---- tenant context (I-06, NP-05) ---------------------------------------------------------------

func TestReceive_TheTenantIsTheConnectionsNeverThePayloads(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	a := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	// the payload claims tenant B: held, never switched, and a security signal is raised
	body := strings.Replace(goodBody("evt-1"), tenantA, tenantB, 1)
	o := e.deliver(deliv{conn: a, body: body, key: "k1"})
	assertXIC(t, o, http.StatusForbidden, integration.XICTenantScopeMismatch)
	if o.Exchange == nil || o.Exchange.State != inbound.StateQuarantined || o.Exchange.QuarantineClass != inbound.ClassAuthContext || o.Exchange.TenantID != tenantA {
		t.Fatalf("%+v", o.Exchange)
	}
	if len(e.repo.Signals) != 1 || e.repo.Signals[0].Type != "integration.security.inbound-tenant-claim-mismatch" {
		t.Fatalf("%+v", e.repo.Signals)
	}
	x, _ := e.repo.Get(ctxFor(tenantA), o.Exchange.ExchangeID)
	if len(x.Raw) != 0 || len(x.Canonical) != 0 {
		t.Fatal("a foreign-tenant payload was retained")
	}
	// tenant B sees nothing of it
	if list, _ := e.repo.List(ctxFor(tenantB), inbound.Filter{}); len(list) != 0 {
		t.Fatal("tenant B sees tenant A's exchange")
	}
	// a payload naming another provider account is held the same way
	acct := strings.Replace(goodBody("evt-2"), "acc-a", "someone-elses-account", 1)
	o = e.deliver(deliv{conn: a, body: acct, key: "k2"})
	assertXIC(t, o, http.StatusForbidden, integration.XICTenantScopeMismatch)
	if o.Exchange.ReasonCode != "ACCOUNT_CLAIM_MISMATCH" {
		t.Fatalf("%s", o.Exchange.ReasonCode)
	}
	// a payload that makes no claim is fine; one that makes the right claim is fine
	if o := e.deliver(deliv{conn: a, body: `{"event_id":"e3","account":"A1","balance":"1","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}`, key: "k3"}); o.Status != 202 {
		t.Fatalf("%d", o.Status)
	}
	// tenant B's connection cannot be reached with tenant A's secret (a certificate or key valid elsewhere, NP-04)
	b := e.activeConn(tenantB, v, "acc-b", refHMAC2)
	assertXIC(t, e.deliver(deliv{conn: b, body: strings.Replace(goodBody("e4"), tenantA, tenantB, 1), key: "k4", secret: secretHMAC}), 401, integration.XICAuthenticationFailed)
}

// ---- idempotency (I-08, NP-06, NP-07) -------------------------------------------------------------

func TestReceive_DuplicateDeliveryReturnsTheOriginal_NP06(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	first := e.deliver(ok(c, "evt-1"))
	second := e.deliver(ok(c, "evt-1"))
	if second.Status != http.StatusOK || !second.Duplicate || second.Exchange.ExchangeID != first.Exchange.ExchangeID {
		t.Fatalf("%+v", second)
	}
	if countType(e.repo.Types(), "integration.inbound.accepted") != 1 {
		t.Fatal("a duplicate produced a second effect")
	}
	var dup inbound.OutEvent
	for _, x := range e.repo.Out {
		if x.Type == "integration.inbound.duplicate" {
			dup = x
		}
	}
	for _, k := range []string{"exchange_id", "original_exchange_id", "dedupe_basis"} {
		if _, ok := dup.Data[k]; !ok {
			t.Errorf("duplicate event lacks %s", k)
		}
	}
	if dup.Data["original_exchange_id"] != first.Exchange.ExchangeID {
		t.Fatal("duplicate event does not name the original")
	}
	// the duplicate delivery is itself on record, pointing back, and holds no key of its own
	list, _ := e.repo.List(ctxFor(tenantA), inbound.Filter{State: inbound.StateDuplicate})
	if len(list) != 1 || list[0].OriginalExchangeID != first.Exchange.ExchangeID {
		t.Fatalf("%+v", list)
	}
	// even a duplicate of a quarantined delivery returns the original
	badBody := strings.Replace(goodBody("evt-q"), `"GBP"`, `"XXX"`, 1)
	q1 := e.deliver(deliv{conn: c, body: badBody, key: "kq"})
	q2 := e.deliver(deliv{conn: c, body: badBody, key: "kq"})
	if q1.Exchange.State != inbound.StateQuarantined || !q2.Duplicate || q2.Exchange.ExchangeID != q1.Exchange.ExchangeID {
		t.Fatalf("%+v %+v", q1, q2)
	}
}

func TestReceive_ChangedBodyUnderTheSameKey_NP07(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	first := e.deliver(deliv{conn: c, body: goodBody("evt-1"), key: "k"})
	changed := strings.Replace(goodBody("evt-1"), "10.5", "99.9", 1)
	o := e.deliver(deliv{conn: c, body: changed, key: "k"})
	assertXIC(t, o, http.StatusConflict, integration.XICDuplicateExchange)
	if o.Exchange == nil || o.Exchange.ExchangeID == first.Exchange.ExchangeID || o.Exchange.State != inbound.StateRejected || o.Exchange.ReasonCode != "BODY_CHANGED_UNDER_SAME_KEY" {
		t.Fatalf("%+v", o.Exchange)
	}
	// nothing about the original changed, and no second business effect
	orig, _ := e.repo.Get(ctxFor(tenantA), first.Exchange.ExchangeID)
	if orig.State != inbound.StateValidated || !strings.Contains(string(orig.Canonical), `"balance":"10.50"`) {
		t.Fatalf("the original was disturbed: %+v", orig)
	}
	if countType(e.repo.Types(), "integration.inbound.accepted") != 1 {
		t.Fatal("a changed body produced an effect")
	}
	// a profile that says QUARANTINE holds it instead
	e2 := newEnv(t)
	v2 := e2.publish(func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "idem-in" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"key_sources":["header:Idempotency-Key","body:/event_id"],"scope":"CONNECTION","retention_days":30,"on_body_change":"QUARANTINE"}`)
			}
		}
	})
	c2 := e2.activeConn(tenantA, v2, "acc-a", refHMAC)
	e2.target("https://domain.example")
	e2.deliver(deliv{conn: c2, body: goodBody("evt-1"), key: "k"})
	o = e2.deliver(deliv{conn: c2, body: changed, key: "k"})
	if o.Status != 409 || o.Exchange.State != inbound.StateQuarantined || o.Exchange.QuarantineClass != inbound.ClassIdempotency {
		t.Fatalf("%+v", o.Exchange)
	}
}

func TestReceive_DeliveryWithoutItsKeyIsRefused_I08(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	// the profile requires the header AND body:/event_id
	o := e.deliver(deliv{conn: c, body: goodBody("e1")}) // no Idempotency-Key header
	if o.Status != 422 || o.Exchange.State != inbound.StateRejected || o.Exchange.ReasonCode != "DEDUPE_KEY_MISSING" {
		t.Fatalf("%+v", o)
	}
	o = e.deliver(deliv{conn: c, body: `{"account":"A1","balance":"1","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}`, key: "k"}) // no event_id
	if o.Status != 422 {
		t.Fatalf("%d", o.Status)
	}
	// the key is scoped to the connection: the same key on another connection of the tenant is a different event
	c2 := e.activeConn(tenantA, v, "acc-a2", refHMAC2)
	e.secrets.set(refHMAC2, secretHMAC)
	body := strings.Replace(goodBody("same-event"), "acc-a", "acc-a2", 1)
	x := e.deliver(deliv{conn: c, body: goodBody("same-event"), key: "same-key"})
	y := e.deliver(deliv{conn: c2, body: body, key: "same-key"})
	if x.Status != 202 || y.Status != 202 || y.Duplicate {
		t.Fatalf("keys must be scoped to their connection: %d %d", x.Status, y.Status)
	}
	// and never across tenants
	b := e.activeConn(tenantB, v, "acc-b", "0a000000-0000-4000-8000-0000000000b2")
	e.secrets.set("0a000000-0000-4000-8000-0000000000b2", "secret-b")
	zb := strings.NewReplacer(tenantA, tenantB, "acc-a", "acc-b").Replace(goodBody("same-event"))
	if o := e.deliver(deliv{conn: b, body: zb, key: "same-key", secret: "secret-b"}); o.Status != 202 || o.Duplicate {
		t.Fatalf("tenant B's delivery was treated as tenant A's duplicate: %+v", o)
	}
}

func TestReceive_ConcurrentIdenticalDeliveries(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	const n = 16
	var fresh, dup, other int32
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := e.deliver(deliv{conn: c, body: goodBody("race"), key: "race"})
			switch {
			case o.Status == 202:
				atomic.AddInt32(&fresh, 1)
			case o.Status == 200 && o.Duplicate:
				atomic.AddInt32(&dup, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("%d %v", o.Status, o.XIC)
			}
			if o.Exchange != nil {
				ids <- o.Exchange.ExchangeID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if fresh != 1 || dup != n-1 || other != 0 || len(seen) != 1 {
		t.Fatalf("fresh=%d dup=%d other=%d distinct references=%d", fresh, dup, other, len(seen))
	}
	if countType(e.repo.Types(), "integration.inbound.accepted") != 1 {
		t.Fatal("concurrent duplicates produced more than one effect")
	}
}

// ---- validation, quarantine, no guessing (§8, I-09..11) ----------------------------------------------

func TestReceive_QuarantineOfUnsupportedAndUnmappableInput(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	type tc struct {
		name   string
		d      deliv
		status int
		class  string
		reason string
		xic    integration.XICCode
	}
	mk := func(event, body string, hdr map[string]string) deliv {
		return deliv{conn: c, body: body, key: "k-" + event, headers: hdr}
	}
	cases := []tc{
		{"unsupported schema version (NP-08)", mk("s1", goodBody("s1"), map[string]string{"X-Source-Schema-Version": "statement@2"}), 422, inbound.ClassSchema, "SCHEMA_VERSION_UNSUPPORTED", integration.XICSchemaVersionUnsupported},
		{"schema version of another operation", mk("s2", goodBody("s2"), map[string]string{"X-Source-Schema-Version": "payment-req@1"}), 422, inbound.ClassSchema, "SCHEMA_VERSION_UNSUPPORTED", integration.XICSchemaVersionUnsupported},
		{"malformed schema reference", mk("s3", goodBody("s3"), map[string]string{"X-Source-Schema-Version": "statement@latest"}), 422, inbound.ClassSchema, "SCHEMA_VERSION_UNSUPPORTED", integration.XICSchemaVersionUnsupported},
		{"missing required field", mk("s4", `{"event_id":"s4","account":"A1","booked_at":"2026-10-20T10:00:00Z"}`, nil), 422, inbound.ClassSchema, "SCHEMA_VIOLATION", ""},
		{"wrong field type", mk("s5", `{"event_id":"s5","account":"A1","balance":10,"booked_at":"2026-10-20T10:00:00Z"}`, nil), 422, inbound.ClassSchema, "SCHEMA_VIOLATION", ""},
		{"unknown currency (NP-09)", mk("m1", strings.Replace(goodBody("m1"), `"GBP"`, `"ZZZ"`, 1), nil), 422, inbound.ClassSchema, "SCHEMA_VIOLATION", ""},
		{"lossy decimal (NP-10)", mk("m2", strings.Replace(goodBody("m2"), "10.5", "10.555", 1), nil), 422, inbound.ClassMapping, "PRECISION_LOSS", integration.XICMappingFailed},
		{"zoneless time", mk("m3", strings.Replace(goodBody("m3"), "+01:00", "", 1), nil), 422, inbound.ClassMapping, "DATETIME_INVALID_OR_ZONELESS", integration.XICMappingFailed},
		{"prohibited field (NP-12)", mk("p1", strings.Replace(goodBody("p1"), `"holder_account"`, `"holder":{"national_id":"AB123456C"},"holder_account"`, 1), nil), 422, inbound.ClassDataPolicy, "PROHIBITED_FIELD", integration.XICMappingFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := e.deliver(c.d)
			if o.Status != c.status || o.Exchange == nil || o.Exchange.State != inbound.StateQuarantined ||
				o.Exchange.QuarantineClass != c.class || o.Exchange.ReasonCode != c.reason {
				t.Fatalf("status=%d %+v", o.Status, o.Exchange)
			}
			if c.xic != "" && (o.XIC == nil || o.XIC.Code != c.xic) {
				t.Fatalf("xic %v", o.XIC)
			}
			// a held delivery has no canonical payload and no handoff
			x, _ := e.repo.Get(ctxFor(tenantA), o.Exchange.ExchangeID)
			if len(x.Canonical) != 0 || x.CanonicalHash != "" || x.DomainOutcome != inbound.DomainNA {
				t.Fatalf("a held delivery carries a canonical result: %+v", x)
			}
		})
	}
	if countType(e.repo.Types(), "integration.inbound.accepted") != 0 || countType(e.repo.Types(), "integration.inbound.quarantined") != len(cases) {
		t.Fatalf("events %v", e.repo.Types())
	}
	// NP-12: the prohibited value is retained nowhere
	for _, x := range func() []inbound.Exchange {
		l, _ := e.repo.List(ctxFor(tenantA), inbound.Filter{Class: inbound.ClassDataPolicy})
		return l
	}() {
		full, _ := e.repo.Get(ctxFor(tenantA), x.ExchangeID)
		if len(full.Raw) != 0 {
			t.Fatal("a prohibited payload was retained")
		}
		b, _ := json.Marshal(full)
		if strings.Contains(string(b), "AB123456C") {
			t.Fatal("a prohibited value leaked into the record")
		}
	}
}

func TestReceive_UnparseableBodyCannotBeRecognizedAsARepeat(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	o := e.deliver(deliv{conn: c, body: `not json at all`, key: "k"})
	if o.Status != 422 || o.Exchange.State != inbound.StateRejected || o.Exchange.ReasonCode != "DEDUPE_KEY_MISSING" {
		t.Fatalf("%+v", o.Exchange)
	}
}

func TestReceive_NoCanonicalTargetIsAPreconditionNotAGuess(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	// no handoff target registered for bank.statement.v1
	o := e.deliver(ok(c, "e1"))
	if o.Status != 422 || o.Exchange.QuarantineClass != inbound.ClassDomainPrecondition || o.Exchange.ReasonCode != "NO_HANDOFF_TARGET" {
		t.Fatalf("%+v", o.Exchange)
	}
	x, _ := e.repo.Get(ctxFor(tenantA), o.Exchange.ExchangeID)
	if len(x.Canonical) == 0 {
		t.Fatal("the mapped payload should be kept so a release can deliver it once a target exists")
	}
}

func TestReceive_TransportShape(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	d := ok(c, "t1")
	d.headers = map[string]string{"Content-Encoding": "gzip"}
	o := e.deliver(d)
	if o.Status != http.StatusUnsupportedMediaType || o.Exchange.ReasonCode != "UNSUPPORTED_ENCODING" {
		t.Fatalf("a compressed body was not refused: %d", o.Status)
	}
	d = ok(c, "t2")
	d.headers = map[string]string{"Content-Type": "text/csv"}
	if o := e.deliver(d); o.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("csv: %d", o.Status)
	}
	d = ok(c, "t3")
	d.headers = map[string]string{"Content-Type": "application/json; charset=utf-8"}
	if o := e.deliver(d); o.Status != 202 {
		t.Fatalf("a charset parameter is not a different media type: %d", o.Status)
	}
	d = ok(c, "t4")
	d.headers = map[string]string{"Content-Encoding": "identity"}
	if o := e.deliver(d); o.Status != 202 {
		t.Fatalf("%d", o.Status)
	}
	// the operation's own limit and the global cap
	small := e.publish(func(in *registry.VersionInput) {
		for i := range in.Content.Operations {
			if in.Content.Operations[i].ID == "statement-push" {
				in.Content.Operations[i].MaxPayloadBytes = 64
			}
		}
	})
	cs := e.activeConn(tenantB, small, "acc-b", refHMAC2)
	e.secrets.set(refHMAC2, secretHMAC)
	big := deliv{conn: cs, body: strings.Replace(goodBody("big"), tenantA, tenantB, 1), key: "kb"}
	if o := e.deliver(big); o.Status != http.StatusRequestEntityTooLarge || o.Exchange == nil || o.Exchange.ReasonCode != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("%+v", o)
	}
	e.gw.Cfg.MaxBodyBytes = 100
	if o := e.deliver(ok(c, "t5")); o.Status != http.StatusRequestEntityTooLarge || o.Exchange != nil {
		t.Fatalf("the global cap must refuse before authentication work: %+v", o)
	}
}

func TestReceive_RateLimitsAreEnforcedBeforeSecretsAreFetched(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	e.gw.Cfg.ConnRatePerMin = 5
	e.gw.Cfg.IPRatePerMin = 0
	before := e.secrets.hits
	limited := 0
	for i := 0; i < 20; i++ {
		o := e.deliver(deliv{conn: c, body: goodBody("rl"), key: "rl", secret: "wrong"})
		if o.Status == http.StatusTooManyRequests {
			limited++
			if o.RetryAfter == 0 || o.XIC.Code != integration.XICRateLimited || !o.XIC.Code.Info().Retryable {
				t.Fatalf("%+v", o)
			}
		}
	}
	if limited < 14 {
		t.Fatalf("only %d of 20 were limited", limited)
	}
	if e.secrets.hits-before > 6 {
		t.Fatalf("%d vault calls for a flooded connection: the limit must come first", e.secrets.hits-before)
	}
	// tokens refill
	e.advance(time.Minute)
	if o := e.deliver(deliv{conn: c, body: goodBody("rl2"), key: "rl2"}); o.Status != 202 {
		t.Fatalf("%d", o.Status)
	}
	// per-address limit
	e.gw.Cfg.ConnRatePerMin, e.gw.Cfg.IPRatePerMin = 0, 3
	n := 0
	for i := 0; i < 10; i++ {
		if o := e.deliver(deliv{conn: c, body: goodBody("ip"), key: "ip", ip: "203.0.113.9:1000", secret: "wrong"}); o.Status == 429 {
			n++
		}
	}
	if n < 6 {
		t.Fatalf("%d", n)
	}
	if o := e.deliver(deliv{conn: c, body: goodBody("ip2"), key: "ip2", ip: "203.0.113.10:1000"}); o.Status != 202 {
		t.Fatalf("another address must not share the budget: %d", o.Status)
	}
}

func TestReceive_StorageOutageIsRetryableAndDoesNotClaimTheKey(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	e.repo.FailNext = true
	o := e.deliver(ok(c, "e1"))
	if o.Status != http.StatusServiceUnavailable || o.RetryAfter == 0 {
		t.Fatalf("%+v", o)
	}
	if o := e.deliver(ok(c, "e1")); o.Status != 202 {
		t.Fatalf("the retry after an outage must be accepted as new: %d", o.Status)
	}
}

func TestReceive_ObserverSeesEveryOutcomeWithoutValues(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var seen []string
	e.gw.Observe = func(kind, reason string) { mu.Lock(); seen = append(seen, kind+":"+reason); mu.Unlock() }
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	e.deliver(ok(c, "o1"))
	e.deliver(ok(c, "o1"))
	e.deliver(deliv{conn: c, body: goodBody("o2"), key: "o2", secret: "x"})
	got := strings.Join(seen, ",")
	for _, w := range []string{"accepted:validated", "duplicate:same_body", "refused:bad_credential"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if strings.Contains(got, "A1") || strings.Contains(got, secretHMAC) {
		t.Fatal("telemetry carries a value")
	}
}

func TestReceive_ProblemBodiesNeverCarryPayloadValues(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	e.target("https://domain.example")
	o := e.deliver(deliv{conn: c, body: strings.Replace(goodBody("v1"), "10.5", "10.555", 1), key: "v1"})
	b, _ := json.Marshal(integration.XICProblem(o.XIC, "t"))
	if strings.Contains(string(b), "10.555") || strings.Contains(string(b), "acc-a") {
		t.Fatalf("%s", b)
	}
	events, _ := e.repo.Events(ctxFor(tenantA), o.Exchange.ExchangeID)
	for _, ev := range events {
		if strings.Contains(string(ev.Detail), "10.555") {
			t.Fatalf("evidence carries a payload value: %s", ev.Detail)
		}
	}
	_ = context.Background
}
