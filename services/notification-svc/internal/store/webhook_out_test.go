package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/signature"
	"zoiko.io/notification-svc/internal/whout"
)

var whBG = context.Background()

func whMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func whWantErr(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want %v", what, err, target)
	}
}

// ── lifecycle and separation of duties ──────────────────────────────────────

func TestWebhook_LifecycleAndMakerChecker(t *testing.T) {
	e := newWhEnv(t)
	d := e.draft()
	if d.Status != whout.StatusDraft || d.ApprovedBy != "" {
		t.Fatalf("a new subscription is a draft: %+v", d)
	}
	// nothing can be delivered to a draft
	e.emit("evt-draft", "invoice.paid", e.invoice("1"))
	if e.deliveryCount(d) != 0 {
		t.Fatal("a draft received a delivery")
	}

	p, err := e.svc.Submit(whBG, e.actor(whMaker), d.SubscriptionID, d.Version)
	whMust(t, err)
	_, err = e.svc.Approve(whBG, e.actor(whMaker), p.SubscriptionID, p.Version, "me")
	whWantErr(t, "the maker approving their own subscription", err, whout.ErrSelfApproval)
	if e.sub(p.SubscriptionID).Status != whout.StatusPendingApproval {
		t.Fatal("a refused approval must change nothing")
	}
	_, err = e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version+5, "stale")
	whWantErr(t, "a stale version", err, whout.ErrVersionConflict)

	a, err := e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version, "ok")
	whMust(t, err)
	if a.Status != whout.StatusActive || a.ApprovedBy != whChecker || a.ApprovedFingerprint != a.ConfigFingerprint {
		t.Fatalf("%+v", a)
	}
	if e.outboxTypes()[whout.EventSubscriptionActivated] != 1 {
		t.Fatalf("activation must emit its event: %v", e.outboxTypes())
	}

	// a name change keeps the approval; a destination change withdraws it
	name := "renamed"
	r, err := e.svc.Update(whBG, e.actor(whMaker), a.SubscriptionID, a.Version, whout.Patch{Name: &name})
	whMust(t, err)
	if r.Status != whout.StatusActive || r.ApprovedBy != whChecker {
		t.Fatalf("a rename must not need a new approval: %+v", r)
	}
	u := "https://other.example.com/hook"
	e.dns.set("other.example.com", "93.184.216.35")
	w, err := e.svc.Update(whBG, e.actor(whMaker), r.SubscriptionID, r.Version, whout.Patch{DestinationURL: &u})
	whMust(t, err)
	if w.Status != whout.StatusDraft || w.ApprovedBy != "" || w.ApprovedFingerprint != "" || w.LastChangedBy != whMaker {
		t.Fatalf("a protected change must withdraw the approval: %+v", w)
	}
	// the maker who just changed it cannot be the one to approve it
	w2, err := e.svc.Submit(whBG, e.actor(whMaker), w.SubscriptionID, w.Version)
	whMust(t, err)
	_, err = e.svc.Approve(whBG, e.actor(whMaker), w2.SubscriptionID, w2.Version, "")
	whWantErr(t, "re-approval by the changer", err, whout.ErrSelfApproval)

	rj, err := e.svc.Reject(whBG, e.actor(whChecker), w2.SubscriptionID, w2.Version, "wrong host")
	whMust(t, err)
	if rj.Status != whout.StatusDraft {
		t.Fatalf("reject returns to draft: %v", rj.Status)
	}
	_, err = e.svc.Reject(whBG, e.actor(whChecker), rj.SubscriptionID, rj.Version, "nothing to reject")
	whWantErr(t, "rejecting a draft", err, whout.ErrInvalidState)

	rv, err := e.svc.Revoke(whBG, e.actor(whChecker), rj.SubscriptionID, rj.Version, "customer left")
	whMust(t, err)
	if rv.Status != whout.StatusRevoked {
		t.Fatal(rv.Status)
	}
	_, err = e.svc.Update(whBG, e.actor(whMaker), rv.SubscriptionID, rv.Version, whout.Patch{Name: &name})
	whWantErr(t, "editing a revoked subscription", err, whout.ErrInvalidState)
	_, err = e.svc.Revoke(whBG, e.actor(whChecker), rv.SubscriptionID, rv.Version, "again")
	whWantErr(t, "revoking twice", err, whout.ErrInvalidState)

	log, err := e.svc.Log(whBG, e.tenant, a.SubscriptionID, 100)
	whMust(t, err)
	var kinds []string
	for _, l := range log {
		kinds = append(kinds, string(l.Kind))
	}
	if got, want := strings.Join(kinds, ","), "CREATED,SUBMITTED,APPROVED,UPDATED,UPDATED,SUBMITTED,REJECTED,REVOKED"; got != want {
		t.Fatalf("log kinds %s, want %s", got, want)
	}
}

func TestWebhook_DatabaseRefusesWhatTheServiceShouldNeverDo(t *testing.T) {
	e := newWhEnv(t)
	d := e.draft()
	// A writer that bypasses the service still cannot make a subscription live without
	// an approval by someone other than its last editor.
	for name, q := range map[string]string{
		"live without approval":    `UPDATE webhook_subscriptions SET status = 'ACTIVE' WHERE subscription_id = $1`,
		"self-approved":            `UPDATE webhook_subscriptions SET status = 'ACTIVE', approved_fingerprint = config_fingerprint, approved_by = last_changed_by, approved_at = now() WHERE subscription_id = $1`,
		"approved other config":    `UPDATE webhook_subscriptions SET status = 'ACTIVE', approved_fingerprint = repeat('a', 64), approved_by = 'x', approved_at = now() WHERE subscription_id = $1`,
		"credentials in url":       `UPDATE webhook_subscriptions SET destination_url = 'https://u:p@example.com/x' WHERE subscription_id = $1`,
		"query string in url":      `UPDATE webhook_subscriptions SET destination_url = 'https://example.com/x?k=v' WHERE subscription_id = $1`,
		"identity change":          `UPDATE webhook_subscriptions SET tenant_id = 'other' WHERE subscription_id = $1`,
		"delete":                   `DELETE FROM webhook_subscriptions WHERE subscription_id = $1`,
		"version goes backwards":   `UPDATE webhook_subscriptions SET version = 0 WHERE subscription_id = $1`,
		"previous key without end": `UPDATE webhook_subscriptions SET previous_signing_key_path = 'kv/old' WHERE subscription_id = $1`,
	} {
		if _, err := e.admin.Exec(whBG, q, d.SubscriptionID); err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	if got := e.sub(d.SubscriptionID); got.Status != whout.StatusDraft || got.Version != 1 {
		t.Fatalf("refused writes must leave the row alone: %+v", got)
	}
}

// ── approval refuses unsafe destinations (INT-NP: invalid destination approval) ──

func TestWebhook_ApprovalRefusesUnsafeDestinations(t *testing.T) {
	e := newWhEnv(t)
	e.dns.set("private.example.com", "10.1.2.3")
	e.dns.set("mixed.example.com", "93.184.216.34", "192.168.0.9")
	e.dns.set("meta.example.com", "169.254.169.254")

	// refused at creation: syntax
	for name, url := range map[string]string{
		"http scheme":     "http://example.com/x",
		"ip literal":      "https://10.0.0.1/x",
		"loopback":        "https://127.0.0.1/x",
		"internal name":   "https://db.internal/x",
		"cluster name":    "https://api.default.svc.cluster.local/x",
		"no dots":         "https://intranet/x",
		"odd ip":          "https://2130706433/x",
		"non-http":        "ftp://example.com/x",
		"disallowed port": "https://example.com:8443/x",
	} {
		_, err := e.svc.Create(whBG, e.actor(whMaker), e.spec(func(s *whout.Spec) { s.DestinationURL = url }))
		if err == nil {
			t.Errorf("%s: accepted at creation", name)
		}
	}
	_, err := e.svc.Create(whBG, e.actor(whMaker), e.spec(func(s *whout.Spec) { s.DestinationRegion = "us-east" }))
	whWantErr(t, "a region outside the deployment's allowlist", err, whout.ErrEgressDenied)

	// refused at submit and approve: DNS
	for _, host := range []string{"private.example.com", "mixed.example.com", "meta.example.com", "nxdomain.example.com"} {
		d := e.draft(func(s *whout.Spec) { s.DestinationURL = "https://" + host + "/x" })
		_, err := e.svc.Submit(whBG, e.actor(whMaker), d.SubscriptionID, d.Version)
		if err == nil {
			t.Errorf("%s: submitted", host)
		}
		if e.sub(d.SubscriptionID).Status != whout.StatusDraft {
			t.Errorf("%s: a refused submit must change nothing", host)
		}
	}

	// the destination turns private AFTER submission but BEFORE approval
	p := e.pending()
	e.dns.set(whHost, "10.9.9.9")
	_, err = e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version, "")
	whWantErr(t, "a destination that now resolves privately", err, whout.ErrEgressDenied)
	if e.sub(p.SubscriptionID).Status != whout.StatusPendingApproval {
		t.Fatal("a refused approval must leave it pending")
	}
	e.dns.set(whHost, "93.184.216.34")

	// the signing key reference
	e.keys.mu.Lock()
	e.keys.known["kv/webhooks/erp"] = false
	e.keys.mu.Unlock()
	_, err = e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version, "")
	whWantErr(t, "an unknown signing key", err, whout.ErrKeyUnavailable)
	e.keys.mu.Lock()
	e.keys.known["kv/webhooks/erp"] = true
	e.keys.err = whout.ErrDependency
	e.keys.mu.Unlock()
	_, err = e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version, "")
	whWantErr(t, "a vault outage must fail closed", err, whout.ErrDependency)
	e.keys.mu.Lock()
	e.keys.err = nil
	e.keys.mu.Unlock()

	// the key is confirmed AS THE APPROVER, so they must be able to see it
	_, err = e.svc.Approve(whBG, e.actor(whChecker), p.SubscriptionID, p.Version, "ok")
	whMust(t, err)
	if last := e.keys.verified[len(e.keys.verified)-1]; !strings.HasPrefix(last, whChecker+"|") {
		t.Errorf("the key was confirmed for %q, not the approver", last)
	}
}

// ── the happy path, end to end ──────────────────────────────────────────────

func TestWebhook_SignedMinimizedDeliveryWithEvidence(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-100", "invoice.paid", e.invoice("inv-100"))
	if got := e.drain(); got != 1 {
		t.Fatalf("dispatched %d, want 1", got)
	}
	reqs := e.dest.requests()
	if len(reqs) != 1 {
		t.Fatalf("destination saw %d requests", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get(whout.HeaderEventID) != "evt-100" ||
		r.Header.Get(whout.HeaderEventType) != "invoice.paid" || r.Header.Get(whout.HeaderAttempt) != "1" {
		t.Fatalf("headers: %v", r.Header)
	}
	// the recipient verifies the signature with the key only it and the vault hold
	hdr := r.Header.Get(signature.HeaderName)
	whMust(t, signature.Verify(hdr, "evt-100", r.Body, [][]byte{whKey1}, e.clk.now(), 5*time.Minute))
	if err := signature.Verify(hdr, "evt-100", r.Body, [][]byte{whKey2}, e.clk.now(), 5*time.Minute); err == nil {
		t.Fatal("verified under the wrong key")
	}
	if err := signature.Verify(hdr, "evt-100", append([]byte(nil), append(r.Body, ' ')...), [][]byte{whKey1}, e.clk.now(), 5*time.Minute); err == nil {
		t.Fatal("a modified body verified")
	}
	// minimization: only the approved fields reached the destination
	for _, leak := range []string{"internal_note", "never leaves", "account_number", "12345678"} {
		if strings.Contains(string(r.Body), leak) {
			t.Fatalf("restricted data left the platform: %s", r.Body)
		}
	}
	var body struct {
		ID   string         `json:"id"`
		Type string         `json:"type"`
		Data map[string]any `json:"data"`
	}
	whMust(t, json.Unmarshal(r.Body, &body))
	if body.ID != "evt-100" || body.Data["invoice_id"] != "inv-100" || body.Data["amount"] != "10.00" || len(body.Data) != 2 {
		t.Fatalf("body: %s", r.Body)
	}

	d := e.delivery(sub, "evt-100")
	if d.Status != whout.DeliverySucceeded || d.AttemptCount != 1 || d.SucceededAt == nil || d.DeliveryID != whout.DeliveryID(sub.SubscriptionID, "evt-100") {
		t.Fatalf("%+v", d)
	}
	if r.Header.Get(whout.HeaderDeliveryID) != d.DeliveryID {
		t.Fatal("the delivery id header must be the stored identity")
	}
	at := e.attempts(d)
	if len(at) != 1 {
		t.Fatalf("attempts: %d", len(at))
	}
	a := at[0]
	if a.Outcome != whout.OutcomeSucceeded || a.ResponseCode == nil || *a.ResponseCode != 200 || a.DestinationHost != whHost ||
		a.PayloadHash != d.BodyHash || a.SignatureProfile != "v1" || a.SignatureTimestamp == nil || a.FinishedAt == nil ||
		len(a.ResponseBodyHash) != 64 || a.ResponseBodyBytes == nil || len(a.SignatureKeyPaths) != 1 || a.SignatureKeyPaths[0] != "kv/webhooks/erp" {
		t.Fatalf("evidence: %+v", a)
	}
	// the evidence names the host only: never the path (which may carry identifiers)
	if strings.Contains(a.DestinationHost, "/") {
		t.Fatal("the host evidence carries a path")
	}
	h, err := e.svc.Health(whBG, e.tenant, sub.SubscriptionID)
	whMust(t, err)
	if h.Succeeded != 1 || h.ConsecutiveFailures != 0 || h.LastSuccessAt == nil || h.BreakerOpen {
		t.Fatalf("health: %+v", h)
	}
	// a second pass has nothing to do: success is terminal
	e.advance(time.Hour)
	if e.drain() != 0 || e.dest.count() != 1 {
		t.Fatal("a succeeded delivery was sent again")
	}
}

// ── fan-out ─────────────────────────────────────────────────────────────────

func TestWebhook_FanOutIsExactlyOnceAndScoped(t *testing.T) {
	e := newWhEnv(t)
	a := e.active(func(s *whout.Spec) { s.EventTypes = []string{"invoice.paid"} })
	b := e.active(func(s *whout.Spec) { s.Name = "wild"; s.EventTypes = []string{"invoice.*", "payment.settled"} })
	sus := e.active(func(s *whout.Spec) { s.Name = "paused" })
	_, err := e.svc.Suspend(whBG, e.actor(whChecker), sus.SubscriptionID, sus.Version, "customer asked")
	whMust(t, err)
	draft := e.draft(func(s *whout.Spec) { s.Name = "draft" })

	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	e.emit("evt-1", "invoice.paid", e.invoice("1")) // the same event again
	e.emit("evt-2", "invoice.created", e.invoice("2"))
	e.emit("evt-3", "payment.settled", e.invoice("3"))
	e.emit("evt-4", "unrelated.thing", e.invoice("4"))
	e.emit("evt-5", whout.EventDeliveryFailed, e.invoice("5")) // an event about a webhook

	counts := map[string]int{}
	for name, s := range map[string]*whout.Subscription{"a": a, "b": b, "suspended": sus, "draft": draft} {
		counts[name] = e.deliveryCount(s)
	}
	want := map[string]int{"a": 1, "b": 3, "suspended": 0, "draft": 0}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("%s got %d deliveries, want %d (%v)", k, counts[k], v, counts)
		}
	}
	// events nobody subscribed to leave no receipt
	var receipts int
	whMust(t, e.admin.QueryRow(whBG, `SELECT count(*) FROM eventing_inbox WHERE consumer = $1 AND tenant_id = $2`, whout.ConsumerName, e.tenant).Scan(&receipts))
	if receipts != 3 { // evt-1, evt-2 (matches b), evt-3
		t.Errorf("%d inbox receipts, want 3", receipts)
	}

	// another tenant's events never reach this tenant's subscriptions
	if err := e.cons.Handle(whBG, whEventJSON("evt-x", "invoice.paid", "someone-else", e.clk.now(), e.invoice("x"))); err != nil {
		t.Fatal(err)
	}
	if e.deliveryCount(a) != 1 {
		t.Fatal("a foreign tenant's event created a delivery")
	}

	// the same event handled concurrently (a rebalanced consumer group) is still one delivery
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.cons.Handle(whBG, whEventJSON("evt-race", "invoice.paid", e.tenant, e.clk.now(), e.invoice("r"))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if e.deliveryCount(a) != 2 {
		t.Fatalf("concurrent duplicates created %d deliveries for one event", e.deliveryCount(a)-1)
	}
	// and an undecodable event is dropped, not retried forever
	if err := e.cons.Handle(whBG, []byte(`{"not":"an event"}`)); err != nil {
		t.Fatalf("an undecodable event must be acknowledged: %v", err)
	}
}

// ── retry, dead letter, rate limit, breaker ─────────────────────────────────

func TestWebhook_BoundedRetryThenDeadLetter(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active() // max_attempts = 3
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 500, nil })
	e.emit("evt-r", "invoice.paid", e.invoice("r"))

	e.drain()
	d := e.delivery(sub, "evt-r")
	if d.Status != whout.DeliveryRetrying || d.AttemptCount != 1 || d.LastResponseCode == nil || *d.LastResponseCode != 500 {
		t.Fatalf("after one failure: %+v", d)
	}
	if !d.NextAttemptAt.After(e.clk.now()) {
		t.Fatal("a failed delivery must back off")
	}
	// not due yet: nothing is sent however often the dispatcher polls (no retry storm)
	for i := 0; i < 5; i++ {
		e.drain()
	}
	if e.dest.count() != 1 {
		t.Fatalf("%d requests before the backoff elapsed", e.dest.count())
	}
	e.advance(2 * time.Minute)
	e.drain()
	e.advance(10 * time.Minute)
	e.drain()
	d = e.delivery(sub, "evt-r")
	if d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonRetriesExhausted || d.AttemptCount != 3 || d.DeadLetteredAt == nil {
		t.Fatalf("after the budget: %+v", d)
	}
	e.advance(24 * time.Hour)
	e.drain()
	if e.dest.count() != 3 {
		t.Fatalf("%d requests, want exactly the budget of 3", e.dest.count())
	}
	// every attempt reused the same delivery identity
	for _, r := range e.dest.requests() {
		if r.Header.Get(whout.HeaderDeliveryID) != d.DeliveryID || r.Header.Get(whout.HeaderEventID) != "evt-r" {
			t.Fatalf("a retry changed the delivery identity: %v", r.Header)
		}
	}
	at := e.attempts(d)
	if len(at) != 3 || at[0].Outcome != whout.OutcomeHTTPError {
		t.Fatalf("attempts %+v", at)
	}
	types := e.outboxTypes()
	if types[whout.EventDeliveryFailed] != 3 || types[whout.EventDeadLettered] != 1 {
		t.Fatalf("events: %v", types)
	}
	if e.obs.dead[whout.ReasonRetriesExhausted] != 1 {
		t.Fatalf("observer: %+v", e.obs.dead)
	}
	dl, err := e.svc.DeadLetters(whBG, e.tenant, sub.SubscriptionID, 10)
	whMust(t, err)
	if len(dl) != 1 || dl[0].DeliveryID != d.DeliveryID {
		t.Fatalf("dead letters: %+v", dl)
	}
}

func TestWebhook_RateLimitedHonoursRetryAfter(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 5 })
	first := true
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) {
		if first {
			first = false
			return 429, map[string]string{"Retry-After": "600"}
		}
		return 200, nil
	})
	e.emit("evt-429", "invoice.paid", e.invoice("1"))
	e.drain()
	d := e.delivery(sub, "evt-429")
	if d.Status != whout.DeliveryRetrying || d.LastErrorClass != "http_429" {
		t.Fatalf("%+v", d)
	}
	if d.NextAttemptAt.Before(e.clk.now().Add(599 * time.Second)) {
		t.Fatalf("Retry-After of 600s ignored: next attempt in %v", d.NextAttemptAt.Sub(e.clk.now()))
	}
	e.advance(590 * time.Second)
	e.drain()
	if e.dest.count() != 1 {
		t.Fatal("retried before Retry-After elapsed")
	}
	e.advance(20 * time.Second)
	e.drain()
	if e.delivery(sub, "evt-429").Status != whout.DeliverySucceeded {
		t.Fatal("the delivery should succeed once the destination recovers")
	}
	// a hostile Retry-After cannot park a delivery for a year
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) {
		return 429, map[string]string{"Retry-After": "31536000"}
	})
	e.emit("evt-429b", "invoice.paid", e.invoice("2"))
	e.drain()
	if d := e.delivery(sub, "evt-429b"); d.NextAttemptAt.After(e.clk.now().Add(2 * time.Hour)) {
		t.Fatalf("Retry-After was not capped: %v", d.NextAttemptAt.Sub(e.clk.now()))
	}
}

func TestWebhook_CircuitBreakerAndAutoDisable(t *testing.T) {
	e := newWhEnv(t, whOpts{cfg: func(c *whout.DispatcherConfig) {
		c.BreakerThreshold, c.BreakerCooldown, c.DisableAfterDeadLetters = 2, 10*time.Minute, 2
	}})
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 1 })
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 503, nil })

	e.emit("evt-b1", "invoice.paid", e.invoice("1"))
	e.drain()
	e.emit("evt-b2", "invoice.paid", e.invoice("2"))
	e.drain()
	// two consecutive failures: the breaker is open and the second dead letter reached the limit
	got := e.sub(sub.SubscriptionID)
	if got.Status != whout.StatusDisabled || got.ConsecutiveDeadLetters != 2 {
		t.Fatalf("a destination that keeps dead-lettering must be disabled: %+v", got)
	}
	types := e.outboxTypes()
	if types[whout.EventSubscriptionDisabled] != 1 {
		t.Fatalf("events: %v", types)
	}
	// a disabled subscription is not delivered to and receives no new deliveries
	before := e.dest.count()
	e.emit("evt-b3", "invoice.paid", e.invoice("3"))
	e.drain()
	if e.deliveryCount(sub) != 2 || e.dest.count() != before {
		t.Fatal("a disabled subscription was used")
	}
	// resume (the destination is fixed): counters and breaker reset, deliveries flow
	e.dest.setStatus(nil)
	r, err := e.svc.Resume(whBG, e.actor(whMaker), got.SubscriptionID, got.Version, "customer fixed their endpoint")
	whMust(t, err)
	if r.Status != whout.StatusActive || r.ConsecutiveFailures != 0 || r.ConsecutiveDeadLetters != 0 || r.BreakerOpenUntil != nil {
		t.Fatalf("resume must start clean: %+v", r)
	}
	e.emit("evt-b4", "invoice.paid", e.invoice("4"))
	e.drain()
	if e.delivery(sub, "evt-b4").Status != whout.DeliverySucceeded {
		t.Fatal("deliveries must flow after a resume")
	}
}

func TestWebhook_OpenBreakerDefersWithoutSpendingAttempts(t *testing.T) {
	e := newWhEnv(t, whOpts{cfg: func(c *whout.DispatcherConfig) {
		c.BreakerThreshold, c.BreakerCooldown, c.DisableAfterDeadLetters = 2, 10*time.Minute, 99
	}})
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 10 })
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 500, nil })
	for i := 0; i < 6; i++ {
		e.emit("evt-q"+string(rune('a'+i)), "invoice.paid", e.invoice("q"))
	}
	e.drain() // six deliveries fail once each; the breaker opens after the second failure
	if !e.sub(sub.SubscriptionID).BreakerOpenUntil.After(e.clk.now()) {
		t.Fatal("the breaker should be open")
	}
	sent := e.dest.count()
	if sent >= 6 {
		t.Fatalf("a breaker that opened after 2 failures still let %d requests through", sent)
	}
	e.advance(5 * time.Minute) // backoff has elapsed for some, the breaker has not cooled
	e.drain()
	if e.dest.count() != sent {
		t.Fatalf("requests reached a destination whose breaker is open (%d -> %d)", sent, e.dest.count())
	}
	if e.obs.breaker == 0 {
		t.Fatal("deferrals must be observable")
	}
	for i := 0; i < 6; i++ {
		d := e.delivery(sub, "evt-q"+string(rune('a'+i)))
		if d.AttemptCount > 1 {
			t.Errorf("delivery %d spent %d attempts while the breaker was open", i, d.AttemptCount)
		}
	}
	e.advance(20 * time.Minute) // the breaker cools: a probe goes through
	e.drain()
	if e.dest.count() <= sent {
		t.Fatal("deliveries must resume probing after the cooldown")
	}
}

func TestWebhook_GoneDeadLettersAtOnceAndRedirectsAreNotFollowed(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 5 })
	e.dest.setStatus(func(n int, _ whRequest) (int, map[string]string) {
		if n == 1 {
			return 410, nil
		}
		return 302, map[string]string{"Location": "https://169.254.169.254/latest/meta-data"}
	})
	e.emit("evt-g", "invoice.paid", e.invoice("g"))
	e.drain()
	if d := e.delivery(sub, "evt-g"); d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonDestinationGone || d.AttemptCount != 1 {
		t.Fatalf("410 Gone: %+v", d)
	}
	e.emit("evt-r", "invoice.paid", e.invoice("r"))
	e.drain()
	d := e.delivery(sub, "evt-r")
	if d.Status != whout.DeliveryRetrying || d.LastErrorClass != "redirect_not_followed" {
		t.Fatalf("a redirect is a failed delivery: %+v", d)
	}
	if e.dest.count() != 2 {
		t.Fatalf("the redirect was followed: %d requests", e.dest.count())
	}
}

func TestWebhook_TLSFailureIsRetriedNotTrusted(t *testing.T) {
	no := false
	e := newWhEnv(t, whOpts{trustTLS: &no})
	sub := e.active()
	e.emit("evt-tls", "invoice.paid", e.invoice("t"))
	e.drain()
	d := e.delivery(sub, "evt-tls")
	at := e.attempts(d)
	if d.Status != whout.DeliveryRetrying || len(at) != 1 || at[0].Outcome != whout.OutcomeTLSFailure || at[0].ResponseCode != nil {
		t.Fatalf("%+v %+v", d, at)
	}
	if e.dest.count() != 0 {
		t.Fatal("a request completed over a connection whose certificate was not trusted")
	}
}

// ── things that must never be sent ──────────────────────────────────────────

// INT-NP-017: a payload field outside the approved scope is blocked before sending.
func TestWebhook_BodyOutsideScopeIsBlockedBeforeSend(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	// A body that somehow carries more than the scope allows: written directly, as a
	// bug, a bad migration or a widened scope later narrowed would leave it.
	evil := `{"id":"evt-evil","type":"invoice.paid","created":"2026-10-17T09:00:00Z","api_version":"v1","tenant_id":"` + e.tenant +
		`","data":{"invoice_id":"1","amount":"10.00","internal_note":"never leaves","bank":{"account_number":"12345678"}}}`
	_, err := e.admin.Exec(whBG, `INSERT INTO webhook_deliveries (delivery_id, tenant_id, subscription_id, event_id, event_type,
		event_occurred_at, body, body_hash, scope_fingerprint, status, next_attempt_at, correlation_id)
		VALUES ($1, $2, $3, 'evt-evil', 'invoice.paid', now(), $4, repeat('b', 64), 'x', 'PENDING', now(), 'c')`,
		whout.DeliveryID(sub.SubscriptionID, "evt-evil"), e.tenant, sub.SubscriptionID, []byte(evil))
	whMust(t, err)
	e.drain()
	d := e.delivery(sub, "evt-evil")
	if d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonMinimizationBlock {
		t.Fatalf("%+v", d)
	}
	if e.dest.count() != 0 {
		t.Fatalf("an out-of-scope body reached the destination: %s", e.dest.requests()[0].Body)
	}
	if e.vault.callCount() != 0 {
		t.Fatal("a blocked body must not even be signed")
	}
	at := e.attempts(d)
	if len(at) != 1 || at[0].Outcome != whout.OutcomeBlocked || at[0].ErrorClass != "scope_violation" {
		t.Fatalf("%+v", at)
	}
	if e.obs.control != 1 || e.outboxTypes()[whout.EventControlFailed] != 1 {
		t.Fatalf("a control failure must be raised: %d %v", e.obs.control, e.outboxTypes())
	}
	// it is the platform's fault, not the destination's: health is untouched
	if s := e.sub(sub.SubscriptionID); s.ConsecutiveFailures != 0 || s.ConsecutiveDeadLetters != 0 || s.Status != whout.StatusActive {
		t.Fatalf("a control failure must not penalise the destination: %+v", s)
	}
}

// INT-NP-018: egress to a prohibited region or a private address is blocked.
func TestWebhook_EgressIsReCheckedOnEverySend(t *testing.T) {
	t.Run("destination starts resolving to a private address after approval", func(t *testing.T) {
		e := newWhEnv(t)
		sub := e.active()
		e.dns.set(whHost, "169.254.169.254") // DNS rebinding to the metadata endpoint
		e.emit("evt-1", "invoice.paid", e.invoice("1"))
		e.drain()
		d := e.delivery(sub, "evt-1")
		if d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonEgressDenied || e.dest.count() != 0 {
			t.Fatalf("%+v, %d requests", d, e.dest.count())
		}
		if at := e.attempts(d); len(at) != 1 || at[0].Outcome != whout.OutcomeEgressBlocked {
			t.Fatalf("%+v", at)
		}
	})
	t.Run("the deployment no longer allows the region", func(t *testing.T) {
		e := newWhEnv(t)
		sub := e.active() // region uk
		// a dispatcher whose deployment now allows only the EU
		eu := newWhEnv(t, whOpts{allowRegions: []string{"eu"}})
		_ = eu
		d2, err := newDispatcherForRegions(e, []string{"eu"})
		whMust(t, err)
		e.emit("evt-1", "invoice.paid", e.invoice("1"))
		if _, err := d2.RunOnce(whBG); err != nil {
			t.Fatal(err)
		}
		d := e.delivery(sub, "evt-1")
		if d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonEgressDenied || e.dest.count() != 0 {
			t.Fatalf("%+v, %d requests", d, e.dest.count())
		}
	})
}

func TestWebhook_EndpointChangedAfterApprovalIsNotSentTo(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	// before it is delivered, someone repoints the subscription
	e.dns.set("elsewhere.example.com", "93.184.216.40")
	u := "https://elsewhere.example.com/steal"
	changed, err := e.svc.Update(whBG, e.actor(whMaker), sub.SubscriptionID, sub.Version, whout.Patch{DestinationURL: &u})
	whMust(t, err)
	if changed.Status != whout.StatusDraft {
		t.Fatal(changed.Status)
	}
	e.drain()
	if e.dest.count() != 0 {
		t.Fatal("a delivery went out for a configuration nobody approved")
	}
	if d := e.delivery(sub, "evt-1"); d.Status != whout.DeliveryPending || d.AttemptCount != 0 {
		t.Fatalf("the delivery must wait, untouched: %+v", d)
	}
	// new events are not even queued while it is a draft
	e.emit("evt-2", "invoice.paid", e.invoice("2"))
	if e.deliveryCount(sub) != 1 {
		t.Fatal("a draft received a delivery")
	}
	// back to the old, approved destination: reviewed and approved by someone else
	back := whURL
	b, err := e.svc.Update(whBG, e.actor(whMaker), changed.SubscriptionID, changed.Version, whout.Patch{DestinationURL: &back})
	whMust(t, err)
	s, err := e.svc.Submit(whBG, e.actor(whMaker), b.SubscriptionID, b.Version)
	whMust(t, err)
	_, err = e.svc.Approve(whBG, e.actor(whChecker), s.SubscriptionID, s.Version, "reviewed")
	whMust(t, err)
	e.advance(5 * time.Minute)
	e.drain()
	if d := e.delivery(sub, "evt-1"); d.Status != whout.DeliverySucceeded {
		t.Fatalf("once approved again the queued delivery goes out: %+v", d)
	}
}

// ── replay ──────────────────────────────────────────────────────────────────

func TestWebhook_ReplayNeedsApprovalAndKeepsIdentity(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 1 })
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 500, nil })
	e.emit("evt-p", "invoice.paid", e.invoice("p"))
	e.drain()
	d := e.delivery(sub, "evt-p")
	if d.Status != whout.DeliveryDeadLettered {
		t.Fatalf("%+v", d)
	}

	op := e.actor("operator-1")
	_, err := e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{})
	whWantErr(t, "no approval at all", err, whout.ErrReplayUnapproved)
	_, err = e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{Approver: "operator-1", ApprovalRef: "CHG-1", Reason: "x"})
	whWantErr(t, "self-approval", err, whout.ErrReplayUnapproved)
	_, err = e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", Reason: "x"})
	whWantErr(t, "no approval reference", err, whout.ErrReplayUnapproved)
	_, err = e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", ApprovalRef: "CHG-1"})
	whWantErr(t, "no reason", err, whout.ErrReplayUnapproved)
	if e.delivery(sub, "evt-p").Status != whout.DeliveryDeadLettered {
		t.Fatal("a refused replay must change nothing")
	}

	// the destination is fixed; an approved replay goes out under the SAME identity
	e.dest.setStatus(nil)
	r, err := e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", ApprovalRef: "CHG-42", Reason: "customer fixed their endpoint"})
	whMust(t, err)
	if r.DeliveryID != d.DeliveryID || r.Status != whout.DeliveryPending || r.ReplayCount != 1 || r.BudgetBase != r.AttemptCount || r.DeadLetterReason != "" {
		t.Fatalf("%+v", r)
	}
	e.drain()
	done := e.delivery(sub, "evt-p")
	if done.Status != whout.DeliverySucceeded || done.AttemptCount != 2 {
		t.Fatalf("%+v", done)
	}
	last := e.dest.requests()[e.dest.count()-1]
	if last.Header.Get(whout.HeaderDeliveryID) != d.DeliveryID || last.Header.Get(whout.HeaderEventID) != "evt-p" {
		t.Fatalf("a replay must carry the original identity so the recipient can recognise a duplicate: %v", last.Header)
	}
	if e.outboxTypes()[whout.EventReplayRequested] != 1 {
		t.Fatalf("events: %v", e.outboxTypes())
	}
	// the log names both people and the approval
	log, _ := e.svc.Log(whBG, e.tenant, sub.SubscriptionID, 100)
	last2 := log[len(log)-1]
	if last2.Kind != whout.LogReplayRequested || last2.Actor != "operator-1" || last2.Approver != "approver-1" || last2.ApprovalRef != "CHG-42" || last2.DeliveryID != d.DeliveryID {
		t.Fatalf("%+v", last2)
	}
	// only a dead letter can be replayed
	_, err = e.svc.Replay(whBG, op, d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", ApprovalRef: "CHG-43", Reason: "again"})
	whWantErr(t, "replaying a succeeded delivery", err, whout.ErrReplayNotAllowed)
	// another tenant cannot replay it
	other := whout.Actor{TenantID: "someone-else", PrincipalID: "operator-1"}
	_, err = e.svc.Replay(whBG, other, d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", ApprovalRef: "CHG-44", Reason: "x"})
	whWantErr(t, "another tenant's replay", err, whout.ErrDeliveryNotFound)
}

func TestWebhook_ReplayRefusedOnAnInactiveSubscription(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 1 })
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 500, nil })
	e.emit("evt-p", "invoice.paid", e.invoice("p"))
	e.drain()
	d := e.delivery(sub, "evt-p")
	cur := e.sub(sub.SubscriptionID)
	_, err := e.svc.Suspend(whBG, e.actor(whChecker), cur.SubscriptionID, cur.Version, "pause")
	whMust(t, err)
	_, err = e.svc.Replay(whBG, e.actor("operator-1"), d.DeliveryID, whout.ReplayApproval{Approver: "approver-1", ApprovalRef: "C", Reason: "r"})
	whWantErr(t, "replay into a suspended subscription", err, whout.ErrInvalidState)
}

func TestWebhook_RevokeDeadLettersTheQueueAndNothingIsSent(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	for _, id := range []string{"evt-1", "evt-2", "evt-3"} {
		e.emit(id, "invoice.paid", e.invoice(id))
	}
	rv, err := e.svc.Revoke(whBG, e.actor(whChecker), sub.SubscriptionID, sub.Version, "contract ended")
	whMust(t, err)
	for _, id := range []string{"evt-1", "evt-2", "evt-3"} {
		if d := e.delivery(sub, id); d.Status != whout.DeliveryDeadLettered || d.DeadLetterReason != whout.ReasonSubscriptionEnded {
			t.Fatalf("%s: %+v", id, d)
		}
	}
	e.advance(time.Hour)
	e.drain()
	if e.dest.count() != 0 {
		t.Fatal("a revoked subscription was delivered to")
	}
	e.emit("evt-4", "invoice.paid", e.invoice("4"))
	if e.deliveryCount(sub) != 3 {
		t.Fatal("a revoked subscription received a new delivery")
	}
	if e.outboxTypes()[whout.EventSubscriptionDisabled] != 1 || rv.Status != whout.StatusRevoked {
		t.Fatalf("%v", e.outboxTypes())
	}
}

// ── signing ─────────────────────────────────────────────────────────────────

func TestWebhook_KeyRotationSignsWithBothKeysDuringGrace(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	rot, err := e.svc.RotateSigningKey(whBG, e.actor(whMaker), sub.SubscriptionID, sub.Version, "kv/webhooks/erp-2", time.Hour)
	whMust(t, err)
	if rot.Status != whout.StatusActive || rot.SigningKeyPath != "kv/webhooks/erp-2" || rot.PreviousSigningKeyPath != "kv/webhooks/erp" ||
		rot.PreviousKeyValidUntil == nil || rot.ApprovedBy != whChecker {
		t.Fatalf("a rotation must not stop deliveries or need a new approval: %+v", rot)
	}
	_, err = e.svc.RotateSigningKey(whBG, e.actor(whMaker), rot.SubscriptionID, rot.Version, "kv/webhooks/erp-2", time.Hour)
	if err == nil {
		t.Fatal("rotating to the key already in use was accepted")
	}
	_, err = e.svc.RotateSigningKey(whBG, e.actor(whMaker), rot.SubscriptionID, rot.Version, "kv/webhooks/unknown", time.Hour)
	whWantErr(t, "an unconfirmed key", err, whout.ErrKeyUnavailable)
	_, err = e.svc.RotateSigningKey(whBG, e.actor(whMaker), rot.SubscriptionID, rot.Version, "kv/webhooks/erp-3", 365*24*time.Hour)
	if err == nil {
		t.Fatal("an unbounded grace period was accepted")
	}

	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	e.drain()
	r := e.dest.requests()[0]
	hdr := r.Header.Get(signature.HeaderName)
	if strings.Count(hdr, ",v1=") != 2 {
		t.Fatalf("during the grace period both keys sign: %s", hdr)
	}
	for name, keys := range map[string][][]byte{"old key": {whKey1}, "new key": {whKey2}} {
		if err := signature.Verify(hdr, "evt-1", r.Body, keys, e.clk.now(), 5*time.Minute); err != nil {
			t.Errorf("a recipient on the %s must still verify: %v", name, err)
		}
	}
	// after the grace period only the new key signs
	e.advance(2 * time.Hour)
	e.emit("evt-2", "invoice.paid", e.invoice("2"))
	e.drain()
	r2 := e.dest.requests()[1]
	hdr2 := r2.Header.Get(signature.HeaderName)
	if strings.Count(hdr2, ",v1=") != 1 {
		t.Fatalf("the retired key must stop signing: %s", hdr2)
	}
	if signature.Verify(hdr2, "evt-2", r2.Body, [][]byte{whKey1}, e.clk.now(), 5*time.Minute) == nil {
		t.Fatal("the retired key still verifies")
	}
	whMust(t, signature.Verify(hdr2, "evt-2", r2.Body, [][]byte{whKey2}, e.clk.now(), 5*time.Minute))
}

func TestWebhook_NeverSentUnsigned(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active(func(s *whout.Spec) { s.MaxAttempts = 3 })
	e.vault.fail = whout.ErrSignerUnavailable
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	e.drain()
	if e.dest.count() != 0 {
		t.Fatal("a webhook was sent without a signature")
	}
	d := e.delivery(sub, "evt-1")
	at := e.attempts(d)
	if d.Status != whout.DeliveryRetrying || len(at) != 1 || at[0].Outcome != whout.OutcomeSigningFailed {
		t.Fatalf("%+v %+v", d, at)
	}
	// the vault being down is not the destination's fault
	if s := e.sub(sub.SubscriptionID); s.ConsecutiveFailures != 0 || s.BreakerOpenUntil != nil {
		t.Fatalf("a signing outage must not penalise the destination: %+v", s)
	}
	e.vault.fail = nil
	e.advance(10 * time.Minute)
	e.drain()
	if e.delivery(sub, "evt-1").Status != whout.DeliverySucceeded {
		t.Fatal("the delivery must complete once signing is back")
	}
}

// ── isolation ───────────────────────────────────────────────────────────────

func TestWebhook_PerDestinationBulkhead(t *testing.T) {
	e := newWhEnv(t, whOpts{cfg: func(c *whout.DispatcherConfig) { c.PerDestinationConcurrency, c.Workers, c.BatchSize = 1, 6, 6 }})
	sub := e.active()
	e.dest.delay = 120 * time.Millisecond
	for i := 0; i < 6; i++ {
		e.emit("evt-"+string(rune('a'+i)), "invoice.paid", e.invoice("x"))
	}
	for i := 0; i < 30 && e.dest.count() < 6; i++ {
		e.drain()
		e.advance(2 * time.Second)
	}
	if e.dest.count() != 6 {
		t.Fatalf("only %d of 6 delivered", e.dest.count())
	}
	if e.dest.maxConc != 1 {
		t.Fatalf("one destination saw %d concurrent requests, the bulkhead allows 1", e.dest.maxConc)
	}
	_ = sub
}

func TestWebhook_CrashedAttemptBecomesUnknownThenRetried(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	d := e.delivery(sub, "evt-1")
	// a worker begins an attempt and dies before recording how it ended
	n, err := e.st.BeginAttempt(whBG, e.tenant, d, whHost, e.clk.now(), time.Minute)
	whMust(t, err)
	if n != 1 {
		t.Fatal(n)
	}
	e.drain() // lease still held: nobody else touches it
	if e.dest.count() != 0 {
		t.Fatal("a delivery whose worker may still be running was sent twice")
	}
	e.advance(2 * time.Minute) // the lease expires
	e.drain()
	got := e.delivery(sub, "evt-1")
	at := e.attempts(got)
	if got.Status != whout.DeliverySucceeded || got.AttemptCount != 2 || len(at) != 2 ||
		at[0].Outcome != whout.OutcomeUnknown || at[0].ErrorClass != "lease_expired" || at[1].Outcome != whout.OutcomeSucceeded {
		t.Fatalf("%+v %+v", got, at)
	}
}

func TestWebhook_TenantIsolation(t *testing.T) {
	a := newWhEnv(t)
	sub := a.active()
	a.emit("evt-1", "invoice.paid", a.invoice("1"))
	a.drain()
	d := a.delivery(sub, "evt-1")

	b := whout.Actor{TenantID: "tenant-wh-other-" + uuid.NewString()[:6], PrincipalID: whChecker}
	if _, err := a.svc.Get(whBG, b.TenantID, sub.SubscriptionID); !errors.Is(err, whout.ErrNotFound) {
		t.Errorf("get: %v", err)
	}
	if list, _ := a.svc.List(whBG, b.TenantID, "", 100); len(list) != 0 {
		t.Errorf("list leaked %d", len(list))
	}
	if _, err := a.svc.Evidence(whBG, b.TenantID, d.DeliveryID); !errors.Is(err, whout.ErrDeliveryNotFound) {
		t.Errorf("evidence: %v", err)
	}
	if _, err := a.svc.Approve(whBG, b, sub.SubscriptionID, sub.Version, ""); !errors.Is(err, whout.ErrNotFound) {
		t.Errorf("approve: %v", err)
	}
	if _, err := a.svc.Revoke(whBG, b, sub.SubscriptionID, sub.Version, "x"); !errors.Is(err, whout.ErrNotFound) {
		t.Errorf("revoke: %v", err)
	}
	if _, err := a.svc.Health(whBG, b.TenantID, sub.SubscriptionID); !errors.Is(err, whout.ErrNotFound) {
		t.Errorf("health: %v", err)
	}
	if dl, _ := a.svc.Deliveries(whBG, b.TenantID, sub.SubscriptionID, "", 10); len(dl) != 0 {
		t.Errorf("deliveries leaked")
	}
	// not even an id that is not a UUID gets anything but "not found"
	if _, err := a.svc.Get(whBG, a.tenant, "not-a-uuid"); !errors.Is(err, whout.ErrNotFound) {
		t.Errorf("malformed id: %v", err)
	}

	// row-level security itself, below the service: as the application role
	count := func(setting, value, table string) int {
		tx, err := a.app.Begin(whBG)
		whMust(t, err)
		defer tx.Rollback(whBG) //nolint:errcheck
		_, err = tx.Exec(whBG, "SELECT set_config($1, $2, true)", setting, value)
		whMust(t, err)
		var n int
		whMust(t, tx.QueryRow(whBG, "SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	for _, table := range []string{"webhook_subscriptions", "webhook_deliveries", "webhook_delivery_attempts", "webhook_subscription_log"} {
		if n := count("app.tenant_id", b.TenantID, table); n != 0 {
			t.Errorf("%s: another tenant sees %d rows", table, n)
		}
		if n := count("app.tenant_id", "", table); n != 0 {
			t.Errorf("%s: no tenant scope must see nothing, saw %d", table, n)
		}
		if n := count("app.tenant_id", a.tenant, table); n == 0 {
			t.Errorf("%s: the owner sees nothing", table)
		}
	}
	// the dispatcher's flag opens deliveries for claiming and nothing else
	if n := count("app.webhook_dispatch", "true", "webhook_deliveries"); n == 0 {
		t.Error("the dispatcher flag must expose deliveries")
	}
	for _, table := range []string{"webhook_subscriptions", "webhook_delivery_attempts", "webhook_subscription_log"} {
		if n := count("app.webhook_dispatch", "true", table); n != 0 {
			t.Errorf("%s: the dispatcher flag exposed %d rows", table, n)
		}
	}
	tx, err := a.app.Begin(whBG)
	whMust(t, err)
	defer tx.Rollback(whBG) //nolint:errcheck
	_, _ = tx.Exec(whBG, "SELECT set_config('app.webhook_dispatch', 'true', true)")
	if _, err := tx.Exec(whBG, `INSERT INTO webhook_deliveries (delivery_id, tenant_id, subscription_id, event_id, event_type, event_occurred_at,
		body, body_hash, scope_fingerprint) VALUES ($1, 'x', $2, 'e', 't', now(), '{}', repeat('a', 64), 'x')`, uuid.NewString(), sub.SubscriptionID); err == nil {
		t.Error("the dispatcher flag allowed an INSERT")
	}
}

// ── evidence cannot be rewritten ────────────────────────────────────────────

func TestWebhook_EvidenceIsAppendOnly(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	e.drain()
	d := e.delivery(sub, "evt-1")
	rejected := func(name, q string, args ...any) {
		t.Helper()
		if _, err := e.admin.Exec(whBG, q, args...); err == nil {
			t.Errorf("%s: the database allowed it", name)
		}
	}
	rejected("rewrite the log", `UPDATE webhook_subscription_log SET actor = 'someone-else' WHERE subscription_id = $1`, sub.SubscriptionID)
	rejected("delete the log", `DELETE FROM webhook_subscription_log WHERE subscription_id = $1`, sub.SubscriptionID)
	rejected("rewrite a finished attempt", `UPDATE webhook_delivery_attempts SET outcome = 'HTTP_ERROR' WHERE delivery_id = $1`, d.DeliveryID)
	rejected("delete an attempt", `DELETE FROM webhook_delivery_attempts WHERE delivery_id = $1`, d.DeliveryID)
	rejected("change a delivery body", `UPDATE webhook_deliveries SET body = '{}' WHERE delivery_id = $1`, d.DeliveryID)
	rejected("change a delivery hash", `UPDATE webhook_deliveries SET body_hash = repeat('c', 64) WHERE delivery_id = $1`, d.DeliveryID)
	rejected("move a succeeded delivery", `UPDATE webhook_deliveries SET status = 'PENDING' WHERE delivery_id = $1`, d.DeliveryID)
	rejected("delete a delivery", `DELETE FROM webhook_deliveries WHERE delivery_id = $1`, d.DeliveryID)
	rejected("lower the attempt counter", `UPDATE webhook_deliveries SET attempt_count = 0 WHERE delivery_id = $1`, d.DeliveryID)

	// a dead letter cannot go back on the queue except by a replay
	e.dest.setStatus(func(int, whRequest) (int, map[string]string) { return 410, nil })
	e.emit("evt-2", "invoice.paid", e.invoice("2"))
	e.drain()
	dead := e.delivery(sub, "evt-2")
	if dead.Status != whout.DeliveryDeadLettered {
		t.Fatalf("%+v", dead)
	}
	rejected("un-dead-letter without a replay", `UPDATE webhook_deliveries SET status = 'PENDING', dead_letter_reason = NULL, dead_lettered_at = NULL WHERE delivery_id = $1`, dead.DeliveryID)

	// a revoked subscription is frozen
	cur := e.sub(sub.SubscriptionID)
	rv, err := e.svc.Revoke(whBG, e.actor(whChecker), cur.SubscriptionID, cur.Version, "done")
	whMust(t, err)
	rejected("revive a revoked subscription", `UPDATE webhook_subscriptions SET status = 'DRAFT' WHERE subscription_id = $1`, rv.SubscriptionID)
}

// An attempt that finishes after the subscription was revoked records its evidence and
// moves nothing else (no health bookkeeping on a frozen row, no error).
func TestWebhook_LateAttemptOnRevokedSubscription(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	d := e.delivery(sub, "evt-1")
	n, err := e.st.BeginAttempt(whBG, e.tenant, d, whHost, e.clk.now(), time.Minute)
	whMust(t, err)
	_, err = e.svc.Revoke(whBG, e.actor(whChecker), sub.SubscriptionID, sub.Version, "mid-flight")
	whMust(t, err)
	code := 200
	whMust(t, e.st.FinishAttempt(whBG, whout.FinishInput{TenantID: e.tenant, DeliveryID: d.DeliveryID, AttemptNo: n, Now: e.clk.now(),
		Outcome: whout.OutcomeSucceeded, ResponseCode: &code, NewStatus: whout.DeliverySucceeded, Success: true}))
	got := e.delivery(sub, "evt-1")
	if got.Status != whout.DeliveryDeadLettered {
		t.Fatalf("a revoked subscription's dead letter must not flip to succeeded: %+v", got)
	}
	if at := e.attempts(got); len(at) != 1 || at[0].Outcome != whout.OutcomeSucceeded {
		t.Fatalf("the attempt's evidence must still be recorded: %+v", at)
	}
}

func TestWebhook_FinishRefusesAnAttemptThatIsNotInFlight(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	e.emit("evt-1", "invoice.paid", e.invoice("1"))
	e.drain()
	d := e.delivery(sub, "evt-1")
	err := e.st.FinishAttempt(whBG, whout.FinishInput{TenantID: e.tenant, DeliveryID: d.DeliveryID, AttemptNo: 1, Now: e.clk.now(),
		Outcome: whout.OutcomeHTTPError, NewStatus: whout.DeliveryRetrying, NextAttemptAt: e.clk.now()})
	if err == nil {
		t.Fatal("a finished attempt was finished twice")
	}
	if e.delivery(sub, "evt-1").Status != whout.DeliverySucceeded {
		t.Fatal("the refused write must change nothing")
	}
}

var _ = pgx.ErrNoRows

// A subscription receives what happened after it was activated, never the history: a
// consumer that starts at the beginning of a topic must not flood a new destination.
func TestWebhook_OnlyEventsAfterActivationAreDelivered(t *testing.T) {
	e := newWhEnv(t)
	sub := e.active()
	at := *sub.ApprovedAt
	feed := func(id string, when time.Time) {
		derr := e.cons.Handle(whBG, whEventJSON(id, "invoice.paid", e.tenant, when, e.invoice(id)))
		if derr != nil {
			t.Fatal(derr)
		}
	}
	feed("evt-old", at.Add(-time.Hour))
	feed("evt-just-before", at.Add(-time.Second))
	if e.deliveryCount(sub) != 0 {
		t.Fatal("events that predate the activation were delivered")
	}
	feed("evt-at", at)
	feed("evt-after", at.Add(time.Minute))
	if e.deliveryCount(sub) != 2 {
		t.Fatalf("events at or after the activation must be delivered, got %d deliveries", e.deliveryCount(sub))
	}
	// replaying the whole topic changes nothing
	feed("evt-old", at.Add(-time.Hour))
	feed("evt-after", at.Add(time.Minute))
	if e.deliveryCount(sub) != 2 {
		t.Fatalf("a replay of the topic changed the deliveries: %d", e.deliveryCount(sub))
	}
	// the cut-off moves with a re-approval: the events of the draft period are not owed
	u := "https://other.example.com/h"
	e.dns.set("other.example.com", "93.184.216.41")
	w, err := e.svc.Update(whBG, e.actor(whMaker), sub.SubscriptionID, sub.Version, whout.Patch{DestinationURL: &u})
	must2(t, err)
	e.advance(10 * time.Minute)
	s, err := e.svc.Submit(whBG, e.actor(whMaker), w.SubscriptionID, w.Version)
	must2(t, err)
	re, err := e.svc.Approve(whBG, e.actor(whChecker), s.SubscriptionID, s.Version, "ok")
	must2(t, err)
	feed("evt-draft-period", at.Add(5*time.Minute))
	feed("evt-after-reapproval", re.ApprovedAt.Add(time.Second))
	if e.deliveryCount(sub) != 3 {
		t.Fatalf("only the event after the re-approval is owed: %d deliveries", e.deliveryCount(sub))
	}
}

func must2(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
