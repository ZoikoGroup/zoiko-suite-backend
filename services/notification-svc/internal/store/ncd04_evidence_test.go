package store_test

import (
	"strconv"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ncd"
)

// NCD-04 — evidence, bounce, complaint and reputation (§7).

const testCallbackSecret = "test-callback-secret"

func withCallbackSecret(t *testing.T) {
	prev := ncd.SecretLookup
	ncd.SecretLookup = func(name string) string {
		if name == "NCD_CALLBACK_SECRET_SMTP_PRIMARY" {
			return testCallbackSecret
		}
		return ""
	}
	t.Cleanup(func() { ncd.SecretLookup = prev })
}

func (h *harness) callback(evs ...ncd.ProviderEvent) []ncd.EventResult {
	h.t.Helper()
	body := []byte("{}")
	ts := strconv.FormatInt(h.clock.now().Unix(), 10)
	b, err := h.svc.VerifyCallback(h.ctx, "smtp-primary", ts, ncd.SignCallback(testCallbackSecret, ts, body), body)
	h.must(err)
	return h.svc.IngestProviderEvents(h.ctx, b, evs)
}

func TestNCD04_CallbackAuthentication(t *testing.T) {
	withCallbackSecret(t)
	h := newHarness(t)
	body := []byte(`{"events":[]}`)
	ts := strconv.FormatInt(h.clock.now().Unix(), 10)
	// NP-26: an invalid signature changes nothing.
	_, err := h.svc.VerifyCallback(h.ctx, "smtp-primary", ts, "sha256=deadbeef", body)
	h.kind(err, ncd.KindForbidden, "invalid_signature")
	// Replay outside the window is refused even with a valid signature.
	old := strconv.FormatInt(h.clock.now().Add(-time.Hour).Unix(), 10)
	_, err = h.svc.VerifyCallback(h.ctx, "smtp-primary", old, ncd.SignCallback(testCallbackSecret, old, body), body)
	h.kind(err, ncd.KindForbidden, "stale_callback")
	// A binding with no callback support accepts nothing.
	_, err = h.svc.VerifyCallback(h.ctx, "in-app", ts, ncd.SignCallback(testCallbackSecret, ts, body), body)
	h.kind(err, ncd.KindForbidden, "callbacks_not_supported")
	// With no secret configured, callbacks fail closed.
	ncd.SecretLookup = func(string) string { return "" }
	_, err = h.svc.VerifyCallback(h.ctx, "smtp-primary", ts, ncd.SignCallback(testCallbackSecret, ts, body), body)
	h.kind(err, ncd.KindUnavailable, "callback_secret_unset")
}

func TestNCD04_CallbacksDeduplicateAndNeverRollBack(t *testing.T) {
	withCallbackSecret(t)
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), nil)
	v := h.send(c)
	a := v.Attempts[0]

	delivered := ncd.ProviderEvent{EventID: "ev-1", EventType: "delivered", AttemptToken: a.IdempotencyToken, OccurredAt: h.clock.now()}
	res := h.callback(delivered)
	if res[0].Status != "APPLIED" {
		t.Fatalf("delivered callback should apply: %+v", res)
	}
	// NP-24: the same provider event again is a no-op.
	res = h.callback(delivered)
	if res[0].Status != "DUPLICATE" {
		t.Fatalf("NP-24: a duplicated callback must be deduplicated: %+v", res)
	}
	// NP-25: a late "accepted" cannot roll DELIVERED back; it is evidence only.
	res = h.callback(ncd.ProviderEvent{EventID: "ev-0", EventType: "accepted", AttemptToken: a.IdempotencyToken})
	if res[0].Status != "APPLIED" {
		t.Fatalf("late accepted should be recorded: %+v", res)
	}
	v = h.view(c.CommunicationID)
	if v.Attempts[0].State != ncd.AttemptDelivered || v.Claims.DeliveryState != "DELIVERED_TO_MAILBOX" {
		t.Fatalf("NP-25: no impossible rollback; got %s", v.Attempts[0].State)
	}
	// NP-36: mailbox accepted is delivery, not reading.
	if !v.Claims.Delivered || v.Claims.OpenedOrDisplayed || v.Claims.Acknowledged {
		t.Fatalf("NP-36 claims wrong: %+v", v.Claims)
	}
	// NP-37/38: an open pixel is a low-confidence signal, never an ack.
	h.callback(ncd.ProviderEvent{EventID: "ev-2", EventType: "opened", AttemptToken: a.IdempotencyToken})
	v = h.view(c.CommunicationID)
	if v.Claims.Acknowledged || v.Claims.OpenedOrDisplayed || !v.Claims.OpenSignalOnly {
		t.Fatalf("NP-38: an open pixel is not acknowledgment or display: %+v", v.Claims)
	}
	// NP-59: a provider correction days later appends evidence with lineage.
	h.clock.advance(48 * time.Hour)
	res = h.callback(ncd.ProviderEvent{EventID: "ev-3", EventType: "bounced", AttemptToken: a.IdempotencyToken, Corrects: "ev-1", Detail: "550 user unknown"})
	if res[0].Status != "APPLIED" {
		t.Fatalf("correction should apply: %+v", res)
	}
	b, err := h.svc.Evidence(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	var sawDelivered, sawBounce bool
	for _, e := range b.Evidence {
		if e.ProviderEventID == "ev-1" {
			sawDelivered = true
		}
		if e.ProviderEventID == "ev-3" && e.Details["corrects_provider_event_id"] == "ev-1" {
			sawBounce = true
		}
	}
	if !sawDelivered || !sawBounce {
		t.Fatal("NP-59: the original observation and its correction must both survive")
	}
}

func TestNCD04_BounceSuppressesAndFallsBack(t *testing.T) {
	withCallbackSecret(t)
	h := newHarness(t)
	i := h.intent(nil, emailInvoice, inAppInvoice)
	r := h.recipient("bounce@example.com")
	c := h.comm(i.IntentID, r, nil)
	v := h.send(c)
	res := h.callback(ncd.ProviderEvent{EventID: "b-1", EventType: "bounced", AttemptToken: v.Attempts[0].IdempotencyToken, Detail: "550 no such user"})
	if res[0].Status != "APPLIED" {
		t.Fatal(res)
	}
	h.svc.RunOnce(h.ctx)
	v = h.view(c.CommunicationID)
	if len(v.Attempts) != 2 || v.Attempts[0].State != ncd.AttemptBounced || v.Attempts[1].Channel != "IN_APP" || v.Attempts[1].Origin != "FALLBACK" {
		t.Fatalf("§6.4: a bounce falls back to the next governed route: %+v", v.Attempts)
	}
	// The hard bounce is now a canonical suppression (NP-13 for next time).
	sups, err := h.svc.ListSuppressions(h.ctx, h.author, ncd.SuppressionQuery{Channel: "EMAIL", Address: "bounce@example.com", ActiveOnly: true})
	h.must(err)
	if len(sups) != 1 || sups[0].Reason != ncd.SuppHardBounce || sups[0].Source != "PROVIDER_EVENT" {
		t.Fatalf("hard bounce must create a canonical suppression: %+v", sups)
	}
	ex, _ := h.svc.ListExceptions(h.ctx, h.author, true)
	if !hasException(ex, "ENDPOINT_REMEDIATION") {
		t.Fatal("§5.4: a hard bounce raises a recipient-remediation signal")
	}
	next := h.comm(i.IntentID, r, nil)
	p, err := h.svc.Prepare(h.ctx, h.author, next.CommunicationID)
	h.must(err)
	if len(p.Decision.Routes) != 1 || p.Decision.Routes[0].Channel != "IN_APP" {
		t.Fatalf("NP-13: the bounced endpoint is no longer routed: %+v", p.Decision.Routes)
	}
}

func TestNCD04_ProviderMessageIDCollisionIsQuarantined(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	r := h.recipient("pat@example.com")
	v1 := h.send(h.comm(i.IntentID, r, nil))
	// A provider that returns an id it already returned (NP-27).
	h.email.outcome = func(n domain.Notification) domain.DeliveryOutcome {
		return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "ok; message-id=" + v1.Attempts[0].ProviderMessageID}
	}
	c2 := h.comm(i.IntentID, r, nil)
	v2 := h.send(c2)
	if v2.Attempts[0].ProviderMessageID != "" {
		t.Fatal("NP-27: a second attempt must not silently take another attempt's provider id")
	}
	ex, _ := h.svc.ListExceptions(h.ctx, h.author, true)
	if !hasException(ex, "PROVIDER_ID_COLLISION") {
		t.Fatalf("NP-27: collision must be a reconciliation exception: %+v", ex)
	}
}

func TestNCD04_ComplaintSpikePausesMarketingOnly(t *testing.T) {
	withCallbackSecret(t)
	h := newHarness(t)
	mkt := h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.AllowedChannels, in.QuietHoursPolicy = "promo", ncd.PurposeMarketing, []string{"EMAIL"}, "EXEMPT"
	}, [4]string{"EMAIL", "en-GB", "Offer {{invoice_no}}", "<p>Offer {{invoice_no}}</p>"})
	permit := ncd.PermissionDecision{Decision: "PERMIT", DecisionID: "mkt"}
	var tokens []string
	for n := 0; n < 20; n++ {
		c := h.comm(mkt.IntentID, h.recipient("m"+strconv.Itoa(n)+"@example.com"), func(in *ncd.CommunicationInput) { in.MarketingPermission = permit })
		tokens = append(tokens, h.send(c).Attempts[0].IdempotencyToken)
	}
	res := h.callback(ncd.ProviderEvent{EventID: "cmp-1", EventType: "complaint", AttemptToken: tokens[0]})
	if res[0].Status != "APPLIED" {
		t.Fatal(res)
	}
	h.must(h.svc.EvaluateReputation(h.ctx))
	rep, err := h.svc.Reputation(h.ctx, h.author)
	h.must(err)
	if rep.Streams["MARKETING"][:6] != "PAUSED" || rep.Streams["CRITICAL"] != "ACTIVE" {
		t.Fatalf("§7.4: a complaint spike pauses marketing and never critical: %+v", rep.Streams)
	}
	// Queued marketing is deferred, not dropped.
	c := h.comm(mkt.IntentID, h.recipient("late@example.com"), func(in *ncd.CommunicationInput) { in.MarketingPermission = permit })
	v := h.send(c)
	if len(v.Attempts) != 0 || v.Jobs[0].State != ncd.JobQueued {
		t.Fatalf("paused stream must defer: %+v", v.Jobs[0])
	}
}

// A provider event id is unique within tenant/provider scope (§7.5), not
// globally: the same id in another tenant is a different fact. Keyed on the
// binding alone, the second tenant's callback was silently reported DUPLICATE
// against a row it could not see under RLS.
func TestNCD04_EventDedupeIsTenantScoped(t *testing.T) {
	withCallbackSecret(t)
	a, b := newHarness(t), newHarness(t)
	for _, h := range []*harness{a, b} {
		i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
		v := h.send(h.comm(i.IntentID, h.recipient("x@example.com"), nil))
		res := h.callback(ncd.ProviderEvent{EventID: "shared-event-id", EventType: "delivered", AttemptToken: v.Attempts[0].IdempotencyToken})
		if res[0].Status != "APPLIED" {
			t.Fatalf("tenant %s: the same provider event id in another tenant must still apply, got %+v", h.tenant, res)
		}
	}
}
