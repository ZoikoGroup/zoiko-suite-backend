package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/ncd"
)

// NCD-02 — recipient, channel, preference and suppression resolution (§5).

func TestNCD02_RecipientPlanCarriesProvenance(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice, inAppInvoice)
	r := h.recipient("Pat@Example.com")
	plan, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: r})
	h.must(err)
	if len(plan.Endpoints) != 2 {
		t.Fatalf("expected EMAIL and IN_APP endpoints, got %+v", plan.Endpoints)
	}
	for _, e := range plan.Endpoints {
		if !e.Verified || e.EndpointHash == "" || strings.Contains(e.EndpointMasked, "Pat@") {
			t.Fatalf("endpoint must be verified, hashed and masked: %+v", e)
		}
		if e.Channel == "EMAIL" && e.Provenance != ncd.ProvenanceIdentityContext {
			t.Fatalf("email provenance should be IDENTITY_CONTEXT, got %s", e.Provenance)
		}
	}
	// NP-12: a recipient in another tenant is refused.
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: r, RecipientTenantID: "other"})
	h.refusal(err, ncd.NCD020CrossTenantRecipientBlock)
	// An unknown principal is unresolved.
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "ghost"})
	h.refusal(err, ncd.NCD006RecipientUnresolved)
}

func TestNCD02_FreeTextEndpointForRegulatedNeedsControlledException(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) {
		in.PurposeClass, in.EvidenceClass, in.AllowedChannels = ncd.PurposeRegulated, "E2", []string{"EMAIL", "IN_APP"}
	})
	r := h.recipient("")
	free := &ncd.FreeTextEndpoint{Channel: "EMAIL", Address: "someone@example.com"}
	_, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: r, FreeTextEndpoint: free})
	h.refusal(err, ncd.NCD007EndpointUnverified) // NP-11
	free.ExceptionRef, free.VerificationRef, free.ReviewerPrincipalID = "EXC-1", "VER-1", "alice"
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: r, FreeTextEndpoint: free})
	h.kind(err, ncd.KindForbidden, "self_review_forbidden")
	free.ReviewerPrincipalID = "carol"
	plan, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: r, FreeTextEndpoint: free})
	h.must(err)
	if plan.Endpoints[0].Provenance != ncd.ProvenanceControlledInput || !strings.Contains(plan.Endpoints[0].ProvenanceRef, "reviewer:carol") {
		t.Fatalf("controlled exception provenance not recorded: %+v", plan.Endpoints[0])
	}
}

func decide(h *harness, intentID, recipient string, priv, mkt ncd.PermissionDecision) *ncd.ChannelDecision {
	h.t.Helper()
	plan, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: intentID, RecipientPrincipalID: recipient})
	h.must(err)
	d, err := h.svc.ChannelDecision(h.ctx, h.author, ncd.DecisionRequest{RecipientPlanID: plan.PlanID, PrivacyPermission: priv, MarketingPermission: mkt})
	h.must(err)
	return d
}

func TestNCD02_PermissionFailsClosed(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	r := h.recipient("p@example.com")
	if d := decide(h, i.IntentID, r, ncd.PermissionDecision{Decision: "INDETERMINATE"}, ncd.PermissionDecision{}); d.Outcome != ncd.DecisionReviewRequired {
		t.Fatalf("NP-17: INDETERMINATE must go to review, got %s", d.Outcome)
	}
	mand := h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.Mandatory, in.Urgency = "sec.alert", ncd.PurposeSecurityCritical, true, "U3"
	})
	d := decide(h, mand.IntentID, r, ncd.PermissionDecision{Decision: "DENY", DecisionID: "prv-1"}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionBlocked || d.ReasonCodes[0] != ncd.NCD008PrivacyPermissionBlocked {
		t.Fatalf("NP-18: a mandatory notice cannot override a privacy prohibition, got %s %v", d.Outcome, d.ReasonCodes)
	}
	mkt := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.PurposeClass = "promo", ncd.PurposeMarketing })
	d = decide(h, mkt.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionBlocked || d.ReasonCodes[0] != ncd.NCD009MarketingPermissionBlock {
		t.Fatalf("NP-47: marketing without a PERMIT must block, got %s %v", d.Outcome, d.ReasonCodes)
	}
}

func TestNCD02_SuppressionPrecedence(t *testing.T) {
	h := newHarness(t)
	trans := h.intent(nil)
	sec := h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.Urgency = "sec.alert", ncd.PurposeSecurityCritical, "U3"
	})
	mkt := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.PurposeClass = "promo", ncd.PurposeMarketing })
	r := h.recipient("pat@example.com")
	permit := ncd.PermissionDecision{Decision: "PERMIT", DecisionID: "mkt-1"}

	// NP-14: a marketing opt-out blocks marketing, and only marketing (§5.3).
	_, created, err := h.svc.AddSuppression(h.ctx, h.as(r), ncd.SuppressionInput{SubjectPrincipalID: r, Reason: ncd.SuppMarketingOptOut,
		Source: "RECIPIENT_UNSUBSCRIBE", SourceEvidenceRef: "unsub-1"})
	h.must(err)
	if !created {
		t.Fatal("suppression should be created")
	}
	if d := decide(h, mkt.IntentID, r, ncd.PermissionDecision{}, permit); d.Outcome != ncd.DecisionBlocked {
		t.Fatalf("NP-14: marketing must be blocked after opt-out, got %s", d.Outcome)
	}
	if d := decide(h, sec.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{}); d.Outcome != ncd.DecisionPermitted {
		t.Fatalf("a marketing opt-out must not suppress a security alert, got %s %+v", d.Outcome, d.Restrictions)
	}
	// Idempotent (§7.3).
	_, created, err = h.svc.AddSuppression(h.ctx, h.as(r), ncd.SuppressionInput{SubjectPrincipalID: r, Reason: ncd.SuppMarketingOptOut,
		Source: "RECIPIENT_UNSUBSCRIBE", SourceEvidenceRef: "unsub-1"})
	h.must(err)
	if created {
		t.Fatal("the same opt-out from the same evidence must be one row")
	}

	// NP-13: a hard-bounced endpoint is suppressed for every purpose; the
	// route re-resolves to IN_APP.
	_, _, err = h.svc.AddSuppression(h.ctx, h.author, ncd.SuppressionInput{Endpoint: &struct {
		Channel string `json:"channel"`
		Address string `json:"address"`
	}{"EMAIL", "PAT@example.com"}, ChannelScope: "EMAIL", Reason: ncd.SuppHardBounce, Source: "PROVIDER_EVENT", SourceEvidenceRef: "bounce-1"})
	h.must(err)
	d := decide(h, trans.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionPermitted || len(d.Routes) != 1 || d.Routes[0].Channel != "IN_APP" {
		t.Fatalf("NP-13: hard bounce should exclude EMAIL and leave IN_APP, got %s %+v", d.Outcome, d.Routes)
	}
	// Even a mandatory security alert cannot use a hard-bounced endpoint.
	d = decide(h, sec.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	for _, rt := range d.Routes {
		if rt.Channel == "EMAIL" {
			t.Fatal("a hard bounce is not a convenience preference; no purpose overrides it")
		}
	}
}

func TestNCD02_LegacySuppressionListIsCanonical(t *testing.T) {
	// NP-45: a suppression recorded by the pre-NCD webhook path (provider A)
	// still blocks traffic routed through the NCD plane.
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.AllowedChannels = []string{"EMAIL"} })
	r := h.recipient("legacy@example.com")
	_, err := h.admin.Exec(h.ctx, `INSERT INTO email_suppressions (suppression_id, tenant_id, recipient_email, reason, source_stream, provider_name)
		VALUES ($1, $2, 'Legacy@Example.com', 'HARD_BOUNCE', 'ALL', 'sendgrid')`, uuid.NewString(), h.tenant)
	h.must(err)
	d := decide(h, i.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionBlocked || d.ReasonCodes[0] != ncd.NCD010ChannelSuppressed {
		t.Fatalf("legacy hard bounce must block, got %s %v", d.Outcome, d.ReasonCodes)
	}
	list, err := h.svc.ListSuppressions(h.ctx, h.author, ncd.SuppressionQuery{Channel: "EMAIL", Address: "legacy@example.com"})
	h.must(err)
	if len(list) != 1 || !list[0].Legacy || list[0].Reason != ncd.SuppHardBounce {
		t.Fatalf("GET /v1/suppressions must show the legacy fact: %+v", list)
	}
}

func TestNCD02_PreferencesVersusMandatoryPolicy(t *testing.T) {
	h := newHarness(t)
	routine := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.PurposeClass = "task", ncd.PurposeOperational })
	mand := h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.Urgency = "sec.alert", ncd.PurposeSecurityCritical, "U3"
		in.Mandatory, in.PreferenceOverrideAllowed = true, true
	})
	r := h.recipient("m@example.com")
	_, err := h.svc.SetPreference(h.ctx, h.as(r), ncd.PreferenceInput{MutedChannels: []string{"EMAIL"}})
	h.must(err)
	// NP-16: routine message, muted channel → use the preferred alternative.
	d := decide(h, routine.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if len(d.Routes) != 1 || d.Routes[0].Channel != "IN_APP" {
		t.Fatalf("NP-16: muted EMAIL must not be used for a routine message: %+v", d.Routes)
	}
	// NP-15: mandatory security policy explicitly permits the override, and
	// the override is recorded as decision evidence.
	d = decide(h, mand.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if d.Routes[0].Channel != "EMAIL" {
		t.Fatalf("NP-15: mandatory override should keep EMAIL first: %+v", d.Routes)
	}
	if ov, _ := d.Inputs["overrides"].([]string); len(ov) == 0 {
		t.Fatalf("NP-15: override evidence missing from decision inputs: %+v", d.Inputs)
	}
	// Stale version is refused; consent fields cannot exist on the type.
	_, err = h.svc.SetPreference(h.ctx, h.as(r), ncd.PreferenceInput{ExpectedVersion: 0})
	h.kind(err, ncd.KindConflict, "stale_preference_version")
	_, err = h.svc.SetPreference(h.ctx, h.as(r), ncd.PreferenceInput{QuietHoursStart: "22:00", QuietHoursEnd: "07:00", ExpectedVersion: 1})
	h.kind(err, ncd.KindInvalid, "time_zone_required")
}

func TestNCD02_QuietHoursUseRecipientCivilTime(t *testing.T) {
	h := newHarness(t)
	routine := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.PurposeClass = "task", ncd.PurposeOperational })
	r := h.recipient("q@example.com")
	// A window covering all but the last minute of the day, in the
	// recipient's zone, so "now" is always inside it.
	_, err := h.svc.SetPreference(h.ctx, h.as(r), ncd.PreferenceInput{QuietHoursStart: "00:00", QuietHoursEnd: "23:59", TimeZone: "Asia/Kolkata"})
	h.must(err)
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if h.clock.now().In(loc).Format("15:04") == "23:59" {
		t.Skip("inside the one open minute")
	}
	d := decide(h, routine.IntentID, r, ncd.PermissionDecision{}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionDeferred || d.NotBefore == nil || d.NotBefore.In(loc).Format("15:04") != "23:59" {
		t.Fatalf("NP-19: routine message must defer to the end of the recipient's quiet window, got %s %v", d.Outcome, d.NotBefore)
	}
	// NP-20: marketing with no known time zone is reviewed, never guessed.
	mkt := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.PurposeClass = "promo", ncd.PurposeMarketing })
	r2 := h.recipient("z@example.com")
	d = decide(h, mkt.IntentID, r2, ncd.PermissionDecision{}, ncd.PermissionDecision{Decision: "PERMIT"})
	if d.Outcome != ncd.DecisionReviewRequired || d.ReasonCodes[0] != ncd.NCD012QuietHourDeferred {
		t.Fatalf("NP-20: unknown time zone must go to review, got %s %v", d.Outcome, d.ReasonCodes)
	}
}

func TestNCD02_GovernedReactivation(t *testing.T) {
	h := newHarness(t)
	sup, _, err := h.svc.AddSuppression(h.ctx, h.author, ncd.SuppressionInput{SubjectPrincipalID: "p1", Reason: ncd.SuppHardBounce,
		Source: "PROVIDER_EVENT", SourceEvidenceRef: "b-1"})
	h.must(err)
	_, appr, err := h.svc.LiftSuppression(h.ctx, h.author, sup.SuppressionID, ncd.LiftInput{EvidenceRef: "address-corrected", Reason: "fixed"})
	h.must(err)
	if appr == nil {
		t.Fatal("§7.3: lifting a hard bounce needs a second principal")
	}
	_, _, err = h.svc.DecideApproval(h.ctx, h.author, appr.ApprovalID, true, "")
	h.kind(err, ncd.KindForbidden, "self_approval_forbidden")
	_, res, err := h.svc.DecideApproval(h.ctx, h.approver, appr.ApprovalID, true, "verified")
	h.must(err)
	if s := res.(*ncd.Suppression); s.LiftedAt == nil || s.LiftApprovedBy != "bob" {
		t.Fatalf("lift not applied with its approver: %+v", s)
	}
	// Rows are never deleted.
	if _, err := h.admin.Exec(h.ctx, `DELETE FROM ncd_suppressions WHERE suppression_id = $1`, sup.SuppressionID); err == nil {
		t.Fatal("a suppression row must never be deleted")
	}
}
