package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ncd"
)

// NCD-03 and §3.3/§3.4 — durable jobs, the attempt state machine, routing,
// fallback, quotas and cancellation.

func TestNCD03_HappyPathEvidenceIsPrecise(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice, inAppInvoice)
	r := h.recipient("pat@example.com")
	c := h.comm(i.IntentID, r, nil)
	v := h.send(c)
	if len(v.Attempts) != 1 || v.Attempts[0].Channel != "EMAIL" || v.Attempts[0].State != ncd.AttemptAccepted {
		t.Fatalf("expected one ACCEPTED email attempt, got %+v", v.Attempts)
	}
	a := v.Attempts[0]
	if a.IdempotencyToken == "" || a.ProviderMessageID == "" || a.ContentHash == "" || a.RecipientSnapshot["endpoint_hash"] == nil {
		t.Fatalf("attempt must pin token, provider id, content hash and endpoint snapshot: %+v", a)
	}
	if h.email.sent[0].Headers["X-Zoiko-Idempotency-Token"] != a.IdempotencyToken {
		t.Fatal("§6.1: the idempotency token must travel to the provider")
	}
	// §3.3 / NP-35: provider accepted is not delivered.
	if v.Claims.DeliveryState != "PROVIDER_ACCEPTED" || !v.Claims.ProviderAccepted || v.Claims.Delivered || v.Claims.LegallyServed != ncd.LegalSufficiencyNotDetermined {
		t.Fatalf("claims overstate the evidence: %+v", v.Claims)
	}
	// NP-21: the same source event returns the same communication.
	c2, created, err := h.svc.CreateCommunication(h.ctx, h.author, ncd.CommunicationInput{IntentID: i.IntentID, LegalEntityID: h.entity,
		RecipientPrincipalID: r, Locale: "en-GB", Variables: map[string]string{"invoice_no": "INV-1"}, SourceEventID: c.SourceEventID})
	h.must(err)
	if created || c2.CommunicationID != c.CommunicationID {
		t.Fatal("NP-21: a replayed source event must return the existing communication")
	}
	// The evidence window closes with no negative fact → COMPLETED, and the
	// claim is still only "provider accepted".
	h.clock.advance(20 * time.Minute)
	h.svc.RunOnce(h.ctx)
	v = h.view(c.CommunicationID)
	if v.Communication.LifecycleState != ncd.CommCompleted || v.Claims.Delivered {
		t.Fatalf("expected COMPLETED with no delivery claim, got %s %+v", v.Communication.LifecycleState, v.Claims)
	}
	if h.email.count() != 1 {
		t.Fatalf("exactly one provider submission, got %d", h.email.count())
	}
}

func TestNCD03_ContentBlocksBeforeAnyProvider(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice)
	r := h.recipient("pat@example.com")
	cases := map[string]struct {
		vars   map[string]string
		locale string
		code   ncd.ReasonCode
	}{
		"NP-06 missing required":  {map[string]string{}, "en-GB", ncd.NCD004TemplateVariableInvalid},
		"NP-07 unexpected var":    {map[string]string{"invoice_no": "1", "evil": "x"}, "en-GB", ncd.NCD004TemplateVariableInvalid},
		"NP-08 CRLF injection":    {map[string]string{"invoice_no": "1\r\nBcc: x@evil.com"}, "en-GB", ncd.NCD004TemplateVariableInvalid},
		"NP-09 unapproved locale": {map[string]string{"invoice_no": "1"}, "fr-FR", ncd.NCD005LocaleNotApproved},
	}
	for name, cs := range cases {
		c := h.comm(i.IntentID, r, func(in *ncd.CommunicationInput) { in.Variables, in.Locale = cs.vars, cs.locale })
		p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
		h.must(err)
		if p.Refusal == nil || p.Refusal.Code != cs.code {
			t.Errorf("%s: expected %s, got %+v", name, cs.code, p.Refusal)
		}
	}
	if h.email.count() != 0 {
		t.Fatalf("no provider call may happen for blocked content, got %d", h.email.count())
	}
	// An explicitly compatible locale is used, and the fallback is recorded.
	h.publish(i.IntentID, "EMAIL", "en-US", "Invoice {{invoice_no}}", "<p>US {{invoice_no}}</p>", []string{"en-CA"})
	c := h.comm(i.IntentID, r, func(in *ncd.CommunicationInput) { in.Locale = "en-CA" })
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil || p.Renders[0].Locale != "en-US" || p.Renders[0].LocaleFallbackFrom != "en-CA" {
		t.Fatalf("compatible locale fallback not applied/recorded: %+v %+v", p.Refusal, p.Renders)
	}
}

func TestNCD03_RetiredIntentIsNotEffective(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice)
	_, err := h.svc.RetireIntent(h.ctx, h.approver, i.IntentID)
	h.must(err)
	c := h.comm(i.IntentID, h.recipient("x@example.com"), nil)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal == nil || p.Refusal.Code != ncd.NCD002IntentNotEffective {
		t.Fatalf("NP-03: a retired intent must block with NCD-002, got %+v", p.Refusal)
	}
}

func TestNCD03_DraftTemplateNeverDispatches(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	_, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: i.IntentID, Channel: "EMAIL", Locale: "en-GB",
		Subject: "Invoice {{invoice_no}}", Body: "<p>{{invoice_no}}</p>"})
	h.must(err)
	c := h.comm(i.IntentID, h.recipient("x@example.com"), nil)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal == nil || p.Refusal.Code != ncd.NCD003TemplateNotPublished {
		t.Fatalf("NP-04: only a published version may dispatch, got %+v", p.Refusal)
	}
}

func TestNCD03_TimeoutBecomesUnknownAndNeverBlindResends(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice, inAppInvoice)
	r := h.recipient("pat@example.com")
	h.email.outcome = func(domain.Notification) domain.DeliveryOutcome {
		return domain.DeliveryOutcome{Unknown: true, Reason: "connection dropped after DATA", ProviderName: "smtp"}
	}
	rec := &recMetrics{}
	h.svc.SetMetrics(rec)
	c := h.comm(i.IntentID, r, nil)
	v := h.send(c)
	if v.Attempts[0].State != ncd.AttemptUnknown || v.Attempts[0].ResolutionDueAt == nil {
		t.Fatalf("NP-22: a post-submit timeout must be UNKNOWN with a resolution deadline, got %+v", v.Attempts[0])
	}
	if v.Claims.DeliveryState != "DELIVERY_UNKNOWN" {
		t.Fatalf("delivery state should be DELIVERY_UNKNOWN, got %s", v.Claims.DeliveryState)
	}
	// Nothing sends again: not the worker, not a resend, not a raw insert.
	h.clock.advance(time.Hour)
	h.svc.RunOnce(h.ctx)
	if h.email.count() != 1 {
		t.Fatalf("INV-13: no second send while UNKNOWN, got %d submissions", h.email.count())
	}
	_, _, err := h.svc.Resend(h.ctx, h.author, c.CommunicationID, ncd.ResendInput{Reason: "try again"})
	h.refusal(err, ncd.NCD014DeliveryAttemptUnknown)
	_, err = h.admin.Exec(h.ctx, `INSERT INTO ncd_attempts (attempt_id, tenant_id, job_id, communication_id, route_index, channel,
		binding_id, intent_id, intent_version, purpose_class, recipient_principal_id, origin, idempotency_token, content_hash,
		render_id, recipient_snapshot)
		SELECT gen_random_uuid(), tenant_id, job_id, communication_id, 0, channel, binding_id, intent_id, intent_version,
		       purpose_class, recipient_principal_id, 'RETRY', 'x-' || gen_random_uuid(), content_hash, render_id, recipient_snapshot
		FROM ncd_attempts WHERE attempt_id = $1`, v.Attempts[0].AttemptID)
	if err == nil {
		t.Fatal("INV-13: the database must refuse a new attempt while one is UNKNOWN")
	}
	// Past its deadline it becomes a human exception.
	ex, err := h.svc.ListExceptions(h.ctx, h.author, true)
	h.must(err)
	if !hasException(ex, "UNKNOWN_UNRESOLVED") {
		t.Fatalf("stuck UNKNOWN must surface as an exception: %+v", ex)
	}
	// §13.1 unknown resolution: the submission was counted as UNKNOWN, and the
	// worker's backlog snapshot — read across tenants as the unprivileged role,
	// so RLS would hide it without the platform-scope policy — shows it waiting.
	if rec.attempts["EMAIL/UNKNOWN"] != 1 {
		t.Fatalf("the UNKNOWN submission must be counted once, got %v", rec.attempts)
	}
	if rec.backlog.UnknownAttempts < 1 || rec.backlog.OldestUnknown <= 0 {
		t.Fatalf("the backlog must show the stuck UNKNOWN attempt and its age, got %+v", rec.backlog)
	}
	// Reconciling the ORIGINAL attempt to FAILED permits the governed
	// fallback, which goes to IN_APP under the same communication.
	h.email.outcome = nil
	_, err = h.svc.ResolveUnknown(h.ctx, h.approver, c.CommunicationID, v.Attempts[0].AttemptID,
		ncd.ResolveInput{ResolvedState: "FAILED", EvidenceRef: "provider-query-123", Note: "provider has no record"})
	h.must(err)
	h.svc.RunOnce(h.ctx)
	v = h.view(c.CommunicationID)
	if len(v.Attempts) != 2 || v.Attempts[1].Origin != "FALLBACK" || v.Attempts[1].Channel != "IN_APP" || v.Attempts[1].State != ncd.AttemptDelivered {
		t.Fatalf("after resolution the fallback should deliver in-app: %+v", v.Attempts)
	}
	if v.Communication.CommunicationID != c.CommunicationID || v.Claims.DeliveryState != "DELIVERED_TO_INBOX" {
		t.Fatalf("INV-02: one communication, precise state; got %+v", v.Claims)
	}
}

// recMetrics records what the plane reports through ncd.Metrics.
type recMetrics struct {
	attempts  map[string]int
	callbacks map[string]int
	backlog   ncd.Backlog
}

func (m *recMetrics) AttemptSubmitted(channel, _, state string, _ time.Duration) {
	if m.attempts == nil {
		m.attempts = map[string]int{}
	}
	m.attempts[channel+"/"+state]++
}

func (m *recMetrics) Callback(_, outcome string) {
	if m.callbacks == nil {
		m.callbacks = map[string]int{}
	}
	m.callbacks[outcome]++
}

func (m *recMetrics) Backlog(b ncd.Backlog) { m.backlog = b }

func hasException(xs []ncd.Exception, kind string) bool {
	for _, x := range xs {
		if x.Kind == kind {
			return true
		}
	}
	return false
}

func TestNCD03_RetryableFailureRetriesWithHistory(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	r := h.recipient("pat@example.com")
	calls := 0
	h.email.outcome = func(n domain.Notification) domain.DeliveryOutcome {
		calls++
		if calls == 1 {
			return domain.DeliveryOutcome{Retryable: true, Reason: "421 try later", ProviderName: "smtp"}
		}
		return domain.DeliveryOutcome{Delivered: true, ProviderName: "smtp", ProviderResponse: "ok; message-id=<" + n.NotificationID + ">"}
	}
	c := h.comm(i.IntentID, r, nil)
	v := h.send(c)
	if v.Attempts[0].State != ncd.AttemptFailed || !v.Attempts[0].Retryable {
		t.Fatalf("first attempt should be a retryable FAILED, got %+v", v.Attempts[0])
	}
	h.clock.advance(time.Minute)
	h.svc.RunOnce(h.ctx)
	v = h.view(c.CommunicationID)
	if len(v.Attempts) != 2 || v.Attempts[1].Origin != "RETRY" || v.Attempts[1].State != ncd.AttemptAccepted {
		t.Fatalf("NP-23: retry must be a new durable attempt with history preserved: %+v", v.Attempts)
	}
}

func TestNCD03_FallbackCannotLowerEvidenceOrResidency(t *testing.T) {
	h := newHarness(t)
	// E3 needs authenticated display: EMAIL (E1) is excluded (NP-29).
	e3 := h.intent(func(in *ncd.IntentInput) { in.EvidenceClass = "E3" }, emailInvoice, inAppInvoice)
	r := h.recipient("pat@example.com")
	d := decide(h, e3.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if len(d.Routes) != 1 || d.Routes[0].Channel != "IN_APP" || !hasRestriction(d, "EMAIL", ncd.NCD016EvidenceInsufficient) {
		t.Fatalf("NP-29: EMAIL must be excluded as evidence-insufficient: %+v %+v", d.Routes, d.Restrictions)
	}
	// NP-30: an explicit residency constraint excludes GLOBAL routes.
	i := h.intent(func(in *ncd.IntentInput) { in.IntentCode = "res" }, emailInvoice)
	c := h.comm(i.IntentID, r, func(in *ncd.CommunicationInput) { in.ResidencyRegions = []string{"eu-west"} })
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal == nil || p.Refusal.Code != ncd.NCD013ProviderRouteUnavailable {
		t.Fatalf("NP-30: residency must block with no compliant route, got %+v", p.Refusal)
	}
}

func hasRestriction(d *ncd.ChannelDecision, ch string, code ncd.ReasonCode) bool {
	for _, r := range d.Restrictions {
		if r.Channel == ch && r.Code == code {
			return true
		}
	}
	return false
}

func TestNCD03_OutagePreservesQueue(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), nil)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		t.Fatal(p.Refusal)
	}
	_, _, err = h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.must(h.svc.SetCircuit(h.ctx, h.approver, "smtp-primary", true, "provider outage drill"))
	defer func() { _ = h.svc.SetCircuit(h.ctx, h.approver, "smtp-primary", false, "drill over") }()
	h.svc.RunOnce(h.ctx)
	v := h.view(c.CommunicationID)
	if len(v.Attempts) != 0 || v.Jobs[0].State != ncd.JobQueued || v.Jobs[0].LastDeferralReason == "" {
		t.Fatalf("NP-28: an outage with no certified equivalent must defer, not drop or reroute: %+v", v.Jobs[0])
	}
	h.must(h.svc.SetCircuit(h.ctx, h.approver, "smtp-primary", false, "restored"))
	h.clock.advance(10 * time.Minute)
	h.svc.RunOnce(h.ctx)
	if v = h.view(c.CommunicationID); len(v.Attempts) != 1 {
		t.Fatalf("after restoration the queued job must submit, got %d attempts", len(v.Attempts))
	}
}

func TestNCD03_ExpiryAndPriority(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	r := h.recipient("pat@example.com")
	exp := h.clock.now().Add(2 * time.Minute)
	nb := h.clock.now().Add(time.Hour)
	c := h.comm(i.IntentID, r, func(in *ncd.CommunicationInput) { in.ExpiresAt, in.NotBefore = &exp, &nb })
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		t.Fatal(p.Refusal)
	}
	_, _, err = h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.clock.advance(3 * time.Minute)
	h.svc.RunOnce(h.ctx)
	h.clock.advance(2 * time.Hour)
	h.svc.RunOnce(h.ctx)
	v := h.view(c.CommunicationID)
	if len(v.Attempts) != 0 || v.Communication.LifecycleState != ncd.CommExpired {
		t.Fatalf("NP-52: a job that expires before submit is EXPIRED with no send, got %s %d attempts", v.Communication.LifecycleState, len(v.Attempts))
	}
	if ncd.PurposeSecurityCritical.Priority() <= ncd.PurposeMarketing.Priority() {
		t.Fatal("NP-53: security must outrank marketing")
	}
}

func TestNCD03_QuotaDefersNeverDrops(t *testing.T) {
	h := newHarness(t)
	lim := ncd.DefaultLimits()
	lim.RecipientPerHour = 1
	h.svc = ncd.NewService(h.store, h.resolver, ncd.RouterTransport{Email: h.email, Inbox: h.store}, lim, nil)
	h.svc.SetClock(h.clock.now)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	r := h.recipient("pat@example.com")
	first := h.send(h.comm(i.IntentID, r, nil))
	if len(first.Attempts) != 1 {
		t.Fatal("first send should submit")
	}
	second := h.send(h.comm(i.IntentID, r, nil))
	if len(second.Attempts) != 0 || second.Jobs[0].State != ncd.JobQueued || second.Jobs[0].LastDeferralReason == "" {
		t.Fatalf("NP-51: over quota the job is deferred and kept: %+v", second.Jobs[0])
	}
}

func TestNCD03_CancelPreservesEvidence(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} }, emailInvoice)
	nb := h.clock.now().Add(time.Hour)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), func(in *ncd.CommunicationInput) { in.NotBefore = &nb })
	_, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	_, _, err = h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	cc, n, err := h.svc.Cancel(h.ctx, h.author, c.CommunicationID, "workflow withdrawn")
	h.must(err)
	if n != 1 || cc.LifecycleState != ncd.CommCancelled {
		t.Fatalf("§6.6: cancel should cancel the queued job, got %d %s", n, cc.LifecycleState)
	}
	b, err := h.svc.Evidence(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if len(b.Evidence) == 0 || b.Evidence[len(b.Evidence)-1].EvidenceType != "CANCELLED" {
		t.Fatalf("cancellation evidence must be preserved: %+v", b.Evidence)
	}
}

func TestNCD03_ResendIsReasonedAndGoverned(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"IN_APP"} }, inAppInvoice)
	c := h.comm(i.IntentID, h.recipient(""), nil)
	h.send(c)
	_, _, err := h.svc.Resend(h.ctx, h.author, c.CommunicationID, ncd.ResendInput{})
	h.kind(err, ncd.KindInvalid, "missing_fields")
	job, appr, err := h.svc.Resend(h.ctx, h.author, c.CommunicationID, ncd.ResendInput{Reason: "recipient asked"})
	h.must(err)
	if appr != nil || job.Origin != "RESEND" || job.ResendReason != "recipient asked" {
		t.Fatalf("a routine resend is a reasoned new job: %+v %+v", job, appr)
	}
	h.svc.RunOnce(h.ctx)
	v := h.view(c.CommunicationID)
	if len(v.Attempts) != 2 || v.Attempts[0].State != ncd.AttemptDelivered || v.Attempts[1].Origin != "RESEND" {
		t.Fatalf("§3.4: the original attempt is preserved alongside the resend: %+v", v.Attempts)
	}
	// A security communication's resend needs a second principal (§11.3).
	sec := h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.Urgency, in.AllowedChannels = "sec", ncd.PurposeSecurityCritical, "U3", []string{"IN_APP"}
	}, inAppInvoice)
	c2 := h.comm(sec.IntentID, h.recipient(""), nil)
	h.send(c2)
	job, appr, err = h.svc.Resend(h.ctx, h.author, c2.CommunicationID, ncd.ResendInput{Reason: "support request"})
	h.must(err)
	if job != nil || appr == nil {
		t.Fatal("a security resend must become a maker-checker approval")
	}
	_, res, err := h.svc.DecideApproval(h.ctx, h.approver, appr.ApprovalID, true, "ok")
	h.must(err)
	if res.(*ncd.DeliveryJob).Origin != "RESEND" {
		t.Fatal("approval must execute the resend")
	}
}

func TestNCD03_TenantIsolation(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice)
	c := h.comm(i.IntentID, h.recipient("pat@example.com"), nil)
	other := ncd.Actor{TenantID: "tenant-" + uuid.NewString()[:8], PrincipalID: "mallory"}
	_, err := h.svc.GetCommunication(h.ctx, other, c.CommunicationID)
	var e *ncd.Error
	if !errors.As(err, &e) || e.Kind != ncd.KindNotFound {
		t.Fatalf("another tenant must see nothing, got %v", err)
	}
}
