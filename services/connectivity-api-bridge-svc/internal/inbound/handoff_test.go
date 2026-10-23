package inbound_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/eventing/egress"
)

// domainServer stands in for an owning domain. It records every request and answers from a script.
type domainServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []domainReq
	answers []int // consumed in order; the last repeats
	drop    int32 // a number of requests to answer by hanging up
	calls   int32
}

type domainReq struct {
	Header http.Header
	Body   string
}

func newDomain(t *testing.T, answers ...int) *domainServer {
	d := &domainServer{answers: answers}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.reqs = append(d.reqs, domainReq{Header: r.Header.Clone(), Body: string(b)})
		idx := len(d.reqs) - 1
		answer := d.answers[len(d.answers)-1]
		if idx < len(d.answers) {
			answer = d.answers[idx]
		}
		d.mu.Unlock()
		atomic.AddInt32(&d.calls, 1)
		if atomic.LoadInt32(&d.drop) > 0 {
			atomic.AddInt32(&d.drop, -1)
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer)
		_, _ = w.Write([]byte(`{"id":"domain-obj-7"}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *domainServer) count() int { return int(atomic.LoadInt32(&d.calls)) }

func (e *env) driveAll() {
	e.t.Helper()
	if _, err := e.gw.DriveDue(context.Background(), 100); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exchange(tenant, id string) *inbound.Exchange {
	e.t.Helper()
	x, err := e.repo.Get(ctxFor(tenant), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return x
}

// ---- successful handoff ----------------------------------------------------------------------------

func TestHandoff_DeliversCanonicalPayloadWithTheCanonicalInputContract(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 201)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt-1"))
	e.driveAll()
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted || x.DomainStatus != 201 || x.DomainRef != "domain-obj-7" || x.TargetID == "" || x.NextAttemptAt != nil {
		t.Fatalf("%+v", x)
	}
	if dom.count() != 1 {
		t.Fatalf("%d domain calls", dom.count())
	}
	r := dom.reqs[0]
	if r.Body != `{"account":"A1","balance":"10.50","booked_at":"2026-10-20T09:00:00Z","currency":"GBP"}` {
		t.Fatalf("the domain must receive the canonical payload, not the source: %s", r.Body)
	}
	for k, want := range map[string]string{"X-Tenant-Id": tenantA, "X-Request-Id": x.ExchangeID, "Idempotency-Key": x.ExchangeID, "X-Source-Channel": "integration",
		"X-Source-System": "bank-x", "X-Purpose-Context": "inbound-exchange", "X-Xic-Exchange-Id": x.ExchangeID} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if r.Header.Get("X-Principal-Id") == "" || r.Header.Get("X-Correlation-Id") == "" {
		t.Error("principal or correlation missing")
	}
	// the sender's own signature and secrets never travel onward
	for k := range r.Header {
		if strings.EqualFold(k, "X-Signature") || strings.EqualFold(k, "Authorization") {
			t.Errorf("header %s forwarded", k)
		}
	}
	if !hasType(e.repo.Types(), "integration.inbound.handed-off") {
		t.Fatal("no handed-off event")
	}
	// a finished exchange is not picked up again
	e.driveAll()
	if dom.count() != 1 {
		t.Fatalf("a handed-off exchange was delivered again: %d", dom.count())
	}
}

func TestHandoff_LegalEntityPointerBecomesTheDomainsEntityHeader(t *testing.T) {
	e := newEnv(t)
	v := e.publish(func(in *registry.VersionInput) {
		in.Content.Mappings[0].Spec = json.RawMessage(strings.Replace(`{"canonical_contract":"bank.statement.v1","fields":[
 {"to":"/account","from":"/account","type":"string","required":true,"max_length":64},
 {"to":"/balance","from":"/balance","type":"decimal","scale":2,"required":true},
 {"to":"/currency","from":"/currency","type":"enum","required":true,"enum":{"GBP":"GBP","EUR":"EUR","USD":"USD"}},
 {"to":"/booked_at","from":"/booked_at","type":"datetime","required":true},
 {"to":"/legal_entity_id","from":"/tenant","type":"string"}],
 "prohibited_source_paths":["/holder/national_id"]}`, "\n", "", -1))
	})
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.gw.Handoff.Check = func(context.Context, string) (string, error) { return "t", nil }
	tg, _ := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "a"}, inbound.CreateTargetInput{CanonicalContract: "bank.statement.v1",
		BaseURL: dom.srv.URL, Path: "/v1/statements", Region: "uk", LegalEntityPointer: "/legal_entity_id"})
	if _, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "b"}, tg.TargetID, tg.Version); err != nil {
		t.Fatal(err)
	}
	e.gw.Handoff.HTTP = dom.srv.Client()
	e.deliver(ok(c, "e1"))
	e.driveAll()
	if dom.count() != 1 || dom.reqs[0].Header.Get("X-Legal-Entity-Id") != tenantA {
		t.Fatalf("%v", dom.reqs)
	}
}

// ---- business rejection is recorded separately (NP-13) ---------------------------------------------

func TestHandoff_DomainRejectionIsABusinessOutcomeNotATransportFailure_NP13(t *testing.T) {
	for _, status := range []int{400, 404, 409, 412, 422} {
		e := newEnv(t)
		v := e.publish(nil)
		c := e.activeConn(tenantA, v, "acc-a", refHMAC)
		dom := newDomain(t, status)
		e.target(dom.srv.URL)
		e.gw.Handoff.HTTP = dom.srv.Client()
		o := e.deliver(ok(c, "evt"))
		if o.Status != 202 {
			t.Fatalf("the sender was already told: transport accepted (%d)", o.Status)
		}
		e.driveAll()
		x := e.exchange(tenantA, o.Exchange.ExchangeID)
		if x.State != inbound.StateQuarantined || x.QuarantineClass != inbound.ClassDomainPrecondition || x.DomainOutcome != inbound.DomainRejected ||
			x.ReasonCode != "DOMAIN_REJECTED_"+itoa(status) || x.DomainStatus != status {
			t.Fatalf("%d: %+v", status, x)
		}
		if dom.count() != 1 {
			t.Fatalf("%d: a rejection was retried (%d calls)", status, dom.count())
		}
		// the transport acceptance is still on record: the exchange was accepted and validated first
		events, _ := e.repo.Events(ctxFor(tenantA), x.ExchangeID)
		kinds := ""
		for _, ev := range events {
			kinds += ev.Kind + ","
		}
		if !strings.HasPrefix(kinds, "ACCEPTED,HANDOFF_ATTEMPT,DOMAIN_REJECTED") {
			t.Fatalf("%s", kinds)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// ---- ambiguous outcomes are resolved with the same key, never a new command (NP-14) --------------------

func TestHandoff_UnknownOutcomeIsRetriedWithTheSameKeyUntilTheDomainAnswers_NP14(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 503, 429, 200)
	atomic.StoreInt32(&dom.drop, 0)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))

	e.driveAll() // 503
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainUnknown || x.LastErrorCode != "HTTP_503" || x.HandoffAttempts != 1 || x.NextAttemptAt == nil {
		t.Fatalf("an ambiguous answer must leave the exchange VALIDATED/UNKNOWN, never failed: %+v", x)
	}
	if !x.NextAttemptAt.After(e.at()) {
		t.Fatal("no backoff")
	}
	e.driveAll() // not due yet
	if dom.count() != 1 {
		t.Fatal("retried before the backoff")
	}
	e.advance(10 * time.Second)
	e.driveAll() // 429
	e.advance(time.Minute)
	e.driveAll() // 200
	x = e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted || x.HandoffAttempts != 3 {
		t.Fatalf("%+v", x)
	}
	// every attempt carried the same idempotency key: the domain's own idempotency settles the ambiguity
	keys := map[string]bool{}
	for _, r := range dom.reqs {
		keys[r.Header.Get("Idempotency-Key")] = true
	}
	if len(dom.reqs) != 3 || len(keys) != 1 || !keys[x.ExchangeID] {
		t.Fatalf("keys %v over %d calls", keys, len(dom.reqs))
	}
}

func TestHandoff_ConnectionResetAfterSendIsUnknownNotFailed(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 201)
	atomic.StoreInt32(&dom.drop, 1) // the domain may have processed it, then hangs up
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	e.driveAll()
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainUnknown || x.LastErrorCode != "NETWORK" {
		t.Fatalf("%+v", x)
	}
	e.advance(time.Minute)
	e.driveAll()
	x = e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateHandedOff || dom.count() != 2 {
		t.Fatalf("%+v calls=%d", x, dom.count())
	}
}

func TestHandoff_OurIdentityRefusedIsNotABusinessAnswer(t *testing.T) {
	for _, status := range []int{401, 403, 302, 500, 502} {
		e := newEnv(t)
		v := e.publish(nil)
		c := e.activeConn(tenantA, v, "acc-a", refHMAC)
		dom := newDomain(t, status)
		e.target(dom.srv.URL)
		e.gw.Handoff.HTTP = dom.srv.Client()
		o := e.deliver(ok(c, "evt"))
		e.driveAll()
		x := e.exchange(tenantA, o.Exchange.ExchangeID)
		if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainUnknown {
			t.Fatalf("%d treated as a verdict: %+v", status, x)
		}
	}
}

func TestHandoff_StalledEscalatesOnceAndKeepsTrying(t *testing.T) {
	e := newEnv(t)
	e.gw.Cfg.HandoffMaxAttempts = 3
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 503)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	for i := 0; i < 6; i++ {
		e.advance(20 * time.Minute)
		e.driveAll()
	}
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if x.State != inbound.StateValidated || x.HandoffAttempts != 6 || x.DomainOutcome != inbound.DomainUnknown {
		t.Fatalf("a stalled exchange must not be failed to clear a queue: %+v", x)
	}
	if countType(e.repo.Types(), "integration.inbound.handoff-stalled") != 1 {
		t.Fatalf("the escalation is raised exactly once: %v", e.repo.Types())
	}
	// when the domain recovers, it completes
	dom.mu.Lock()
	dom.answers = []int{200}
	dom.reqs = nil
	dom.mu.Unlock()
	e.advance(20 * time.Minute)
	e.driveAll()
	if x = e.exchange(tenantA, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff {
		t.Fatalf("%+v", x)
	}
}

func TestBackoffIsExponentialAndBounded(t *testing.T) {
	e := newEnv(t)
	prev := time.Duration(0)
	for i := 1; i <= 12; i++ {
		d := e.gw.Backoff(i)
		if d < prev || d > e.gw.Cfg.HandoffBackoffMax {
			t.Fatalf("attempt %d: %v after %v", i, d, prev)
		}
		prev = d
	}
	if e.gw.Backoff(1) != e.gw.Cfg.HandoffBackoffBase || e.gw.Backoff(50) != e.gw.Cfg.HandoffBackoffMax {
		t.Fatal("bounds")
	}
}

// ---- concurrency: one delivery per exchange --------------------------------------------------------------

func TestHandoff_ConcurrentWorkersDeliverOnce(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.gw.Drive(context.Background(), inbound.DueRef{TenantID: tenantA, ExchangeID: o.Exchange.ExchangeID})
		}()
	}
	wg.Wait()
	if dom.count() != 1 {
		t.Fatalf("%d concurrent workers made %d domain calls", 8, dom.count())
	}
	if x := e.exchange(tenantA, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff {
		t.Fatalf("%+v", x)
	}
}

// ---- egress, residency and the target registry --------------------------------------------------------------

func TestHandoff_DestinationIsCheckedEveryAttempt(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	var blocked atomic.Bool
	e.gw.Handoff.Check = func(context.Context, string) (string, error) {
		if blocked.Load() {
			return "", errors.New("resolves into a private range now")
		}
		return "ok", nil
	}
	o := e.deliver(ok(c, "evt"))
	blocked.Store(true) // DNS rebinding after approval
	e.driveAll()
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if dom.count() != 0 || x.State != inbound.StateValidated || x.LastErrorCode != "EGRESS_BLOCKED" {
		t.Fatalf("a destination that now fails policy was called: calls=%d %+v", dom.count(), x)
	}
	if len(e.repo.Signals) == 0 || e.repo.Signals[len(e.repo.Signals)-1].Type != "integration.security.handoff-egress-blocked" {
		t.Fatalf("%+v", e.repo.Signals)
	}
	blocked.Store(false)
	e.advance(time.Minute)
	e.driveAll()
	if x = e.exchange(tenantA, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff {
		t.Fatalf("%+v", x)
	}
}

func TestHandoff_RealEgressPolicyRefusesPrivateAndMetadataTargets(t *testing.T) {
	for host, ip := range map[string]string{"internal.example.com": "10.1.2.3", "meta.example.com": "169.254.169.254", "loop.example.com": "127.0.0.1"} {
		e := newEnv(t)
		e.gw.Handoff.Egress = egress.Policy{Resolver: fakeDNS{host: ip}}
		e.gw.Handoff.Check = nil
		_, err := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "a"}, inbound.CreateTargetInput{CanonicalContract: "x.v1", BaseURL: "https://" + host, Path: "/in", Region: "uk"})
		if err != nil {
			t.Fatal(err)
		}
		list, _ := e.repo.ListTargets(ctxFor(platform))
		if _, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "b"}, list[0].TargetID, list[0].Version); !errors.Is(err, inbound.ErrEgress) {
			t.Fatalf("%s -> %s approved: %v", host, ip, err)
		}
	}
}

func TestHandoff_ResidencyIsEnforcedBeforeThePayloadLeaves(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	e.gw.Handoff.AllowedRegions = []string{"eu"} // the deployment no longer serves the target's region
	e.driveAll()
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if dom.count() != 0 || x.LastErrorCode != "REGION_BLOCKED" || x.State != inbound.StateValidated {
		t.Fatalf("calls=%d %+v", dom.count(), x)
	}
}

func TestTargets_PlatformDataWithMakerChecker(t *testing.T) {
	e := newEnv(t)
	e.gw.Handoff.Check = func(context.Context, string) (string, error) { return "ok", nil }
	in := inbound.CreateTargetInput{CanonicalContract: "bank.statement.v1", BaseURL: "https://domain.example", Path: "/v1/x", Region: "uk"}
	if _, err := e.gw.CreateTarget(ctxFor(tenantA), tenantA, inbound.Actor{PrincipalID: "a"}, in); !errors.Is(err, inbound.ErrNotPlatform) {
		t.Fatalf("a tenant registered a target: %v", err)
	}
	for name, bad := range map[string]inbound.CreateTargetInput{
		"no contract":     {BaseURL: "https://d.example", Path: "/x", Region: "uk"},
		"userinfo":        {CanonicalContract: "c", BaseURL: "https://u:p@d.example", Path: "/x", Region: "uk"},
		"query in base":   {CanonicalContract: "c", BaseURL: "https://d.example?x=1", Path: "/x", Region: "uk"},
		"ftp":             {CanonicalContract: "c", BaseURL: "ftp://d.example", Path: "/x", Region: "uk"},
		"path traversal":  {CanonicalContract: "c", BaseURL: "https://d.example", Path: "/a/../b", Region: "uk"},
		"path with query": {CanonicalContract: "c", BaseURL: "https://d.example", Path: "/a?x=1", Region: "uk"},
		"relative path":   {CanonicalContract: "c", BaseURL: "https://d.example", Path: "a", Region: "uk"},
		"foreign region":  {CanonicalContract: "c", BaseURL: "https://d.example", Path: "/a", Region: "us"},
		"bad entity ptr":  {CanonicalContract: "c", BaseURL: "https://d.example", Path: "/a", Region: "uk", LegalEntityPointer: "legal"},
	} {
		if _, err := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "a"}, bad); !errors.Is(err, inbound.ErrBadRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	tg, err := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "author"}, in)
	if err != nil || tg.Status != inbound.TargetDraft {
		t.Fatal(err)
	}
	if _, err := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "author"}, in); !errors.Is(err, inbound.ErrTargetExists) {
		t.Fatalf("two live targets for one contract: %v", err)
	}
	if _, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "author"}, tg.TargetID, tg.Version); !errors.Is(err, inbound.ErrSelfAction) {
		t.Fatalf("self approval: %v", err)
	}
	if _, err := e.gw.ApproveTarget(ctxFor(tenantA), tenantA, inbound.Actor{PrincipalID: "x"}, tg.TargetID, tg.Version); !errors.Is(err, inbound.ErrNotPlatform) {
		t.Fatalf("%v", err)
	}
	if _, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "approver"}, tg.TargetID, 99); !errors.Is(err, inbound.ErrVersionConflict) {
		t.Fatalf("%v", err)
	}
	act, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "approver"}, tg.TargetID, tg.Version)
	if err != nil || act.Status != inbound.TargetActive || act.ApprovedBy != "approver" {
		t.Fatalf("%v", err)
	}
	// suspending stops delivery without losing anything: exchanges wait
	if _, err := e.gw.SetTargetStatus(ctxFor(platform), platform, inbound.Actor{PrincipalID: "approver"}, tg.TargetID, act.Version, "SUSPENDED"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.repo.ActiveTarget(ctxFor(tenantA), "bank.statement.v1"); got != nil {
		t.Fatal("a suspended target is still active")
	}
	if _, err := e.gw.SetTargetStatus(ctxFor(platform), platform, inbound.Actor{PrincipalID: "approver"}, tg.TargetID, 99, "RETIRED"); !errors.Is(err, inbound.ErrVersionConflict) {
		t.Fatalf("%v", err)
	}
	if _, err := e.gw.SetTargetStatus(ctxFor(platform), platform, inbound.Actor{PrincipalID: "approver"}, tg.TargetID, 2, "ACTIVE"); !errors.Is(err, inbound.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
}

func TestHandoff_NoActiveTargetLeavesTheExchangeWaiting(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	tg := e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	if _, err := e.gw.SetTargetStatus(ctxFor(platform), platform, inbound.Actor{PrincipalID: "p"}, tg.TargetID, tg.Version, "SUSPENDED"); err != nil {
		t.Fatal(err)
	}
	e.driveAll()
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if dom.count() != 0 || x.State != inbound.StateValidated || x.LastErrorCode != "NO_TARGET" {
		t.Fatalf("%+v", x)
	}
	cur, _ := e.repo.GetTarget(ctxFor(platform), tg.TargetID)
	if _, err := e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "p2"}, tg.TargetID, cur.Version); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.driveAll()
	if x = e.exchange(tenantA, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff {
		t.Fatalf("%+v", x)
	}
}

// ---- retention ---------------------------------------------------------------------------------------------

func TestPurge_RemovesBodiesOnlyAfterTheirRetention(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	e.driveAll()
	if n, _ := e.repo.PurgeExpired(context.Background(), e.at(), 100); n != 0 {
		t.Fatalf("purged %d before retention", n)
	}
	e.advance(31 * 24 * time.Hour)
	if n, _ := e.repo.PurgeExpired(context.Background(), e.at(), 100); n != 1 {
		t.Fatalf("purged %d", n)
	}
	x := e.exchange(tenantA, o.Exchange.ExchangeID)
	if len(x.Raw) != 0 || len(x.Canonical) != 0 {
		t.Fatal("bodies survived the purge")
	}
	if x.CanonicalHash == "" || x.BodyHash == "" || x.State != inbound.StateHandedOff {
		t.Fatalf("the evidence must outlive the bodies: %+v", x)
	}
}

func TestRunHandoff_DeliversAndPurgesOnATicker(t *testing.T) {
	e := newEnv(t)
	v := e.publish(nil)
	c := e.activeConn(tenantA, v, "acc-a", refHMAC)
	dom := newDomain(t, 200)
	e.target(dom.srv.URL)
	e.gw.Handoff.HTTP = dom.srv.Client()
	o := e.deliver(ok(c, "evt"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var passes int32
	go func() {
		e.gw.RunHandoff(ctx, 5*time.Millisecond, 10, func(int, error) { atomic.AddInt32(&passes, 1) })
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.exchange(tenantA, o.Exchange.ExchangeID).State != inbound.StateHandedOff {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if e.exchange(tenantA, o.Exchange.ExchangeID).State != inbound.StateHandedOff || atomic.LoadInt32(&passes) == 0 {
		t.Fatal("the worker did not deliver")
	}
	d2 := make(chan struct{})
	go func() { e.gw.RunHandoff(context.Background(), 0, 10, nil); close(d2) }()
	select {
	case <-d2:
	case <-time.After(time.Second):
		t.Fatal("a disabled worker kept running")
	}
}
