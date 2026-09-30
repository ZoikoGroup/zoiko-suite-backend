package store_test

import (
	"strings"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/ncd"
)

// NCD-01 — intent and template registry (§4).

func TestNCD01_IntentActivationIsSegregated(t *testing.T) {
	h := newHarness(t)
	i, err := h.svc.CreateIntent(h.ctx, h.author, ncd.IntentInput{LegalEntityID: h.entity, IntentCode: "x", DisplayName: "X",
		PurposeClass: ncd.PurposeOperational, DomainOwner: "ops", Sensitivity: "S0", Urgency: "U0", EvidenceClass: "E0",
		AllowedChannels: []string{"IN_APP"}})
	h.must(err)
	if i.Status != "DRAFT" || i.Version != 1 {
		t.Fatalf("new intent should be DRAFT v1, got %s v%d", i.Status, i.Version)
	}
	_, err = h.svc.ActivateIntent(h.ctx, h.author, i.IntentID, 1, nil)
	h.kind(err, ncd.KindForbidden, "self_approval_forbidden")
	a, err := h.svc.ActivateIntent(h.ctx, h.approver, i.IntentID, 1, nil)
	h.must(err)
	if a.Status != "ACTIVE" || a.ApprovedByPrincipalID != "bob" || a.ActivatedAt == nil {
		t.Fatalf("activation not recorded: %+v", a)
	}
	// An ACTIVE version is immutable: a change is a new version (§4.2).
	_, err = h.admin.Exec(h.ctx, `UPDATE ncd_communication_intents SET purpose_class = 'MARKETING_PROMOTIONAL' WHERE intent_id = $1`, i.IntentID)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("editing an active intent version must be refused by the database, got %v", err)
	}
}

func TestNCD01_IntentPolicyInvariants(t *testing.T) {
	h := newHarness(t)
	base := ncd.IntentInput{LegalEntityID: h.entity, IntentCode: "x", DisplayName: "X", DomainOwner: "sec",
		Sensitivity: "S1", Urgency: "U3", EvidenceClass: "E1", AllowedChannels: []string{"EMAIL"}}
	cases := map[string]func(*ncd.IntentInput){
		"marketing in security intent": func(i *ncd.IntentInput) { i.PurposeClass = ncd.PurposeSecurityCritical; i.MarketingAllowed = true },
		"mandatory marketing":          func(i *ncd.IntentInput) { i.PurposeClass = ncd.PurposeMarketing; i.Mandatory = true },
		"regulated below E2":           func(i *ncd.IntentInput) { i.PurposeClass = ncd.PurposeRegulated },
		"override without mandatory":   func(i *ncd.IntentInput) { i.PurposeClass = ncd.PurposeOperational; i.PreferenceOverrideAllowed = true },
		"unknown channel": func(i *ncd.IntentInput) {
			i.PurposeClass = ncd.PurposeOperational
			i.AllowedChannels = []string{"PIGEON"}
		},
		"required with fallback": func(i *ncd.IntentInput) {
			i.PurposeClass = ncd.PurposeOperational
			i.VariableContract = []ncd.VariableSpec{{Name: "a", Required: true, FallbackText: "x"}}
		},
	}
	for name, mod := range cases {
		in := base
		mod(&in)
		if _, err := h.svc.CreateIntent(h.ctx, h.author, in); err == nil {
			t.Errorf("%s: expected refusal", name)
		}
	}
}

func TestNCD01_TemplateLifecycleImmutableAndSegregated(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	tv, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: i.IntentID, Channel: "EMAIL", Locale: "en-GB",
		Subject: "Invoice {{invoice_no}}", Body: "<p>{{invoice_no}}</p>"})
	h.must(err)
	_, err = h.svc.ApproveTemplate(h.ctx, h.approver, tv.TemplateVersionID)
	h.kind(err, ncd.KindConflict, "not_review")
	tv, err = h.svc.ValidateTemplate(h.ctx, h.author, tv.TemplateVersionID)
	h.must(err)
	if tv.Status != ncd.TemplateReview || !tv.ValidationReport.Passed || len(tv.ValidationReport.Checks) < 10 {
		t.Fatalf("validation should pass into REVIEW with its checks listed: %+v", tv.ValidationReport)
	}
	_, err = h.svc.ApproveTemplate(h.ctx, h.author, tv.TemplateVersionID)
	h.kind(err, ncd.KindForbidden, "self_approval_forbidden")
	_, err = h.svc.PublishTemplate(h.ctx, h.approver, tv.TemplateVersionID, nil)
	h.kind(err, ncd.KindConflict, "not_approved")
	_, err = h.svc.ApproveTemplate(h.ctx, h.approver, tv.TemplateVersionID)
	h.must(err)
	past := h.clock.now().Add(-time.Hour)
	_, err = h.svc.PublishTemplate(h.ctx, h.approver, tv.TemplateVersionID, &past)
	h.kind(err, ncd.KindInvalid, "effective_from_in_past")
	pub, err := h.svc.PublishTemplate(h.ctx, h.approver, tv.TemplateVersionID, nil)
	h.must(err)
	if pub.Status != ncd.TemplatePublished || pub.PublishedAt == nil || pub.EffectiveFrom == nil {
		t.Fatalf("publication not recorded: %+v", pub)
	}

	// NP-05: a published template edited in place is refused by the database.
	_, err = h.admin.Exec(h.ctx, `UPDATE ncd_templates SET body = 'changed' WHERE template_version_id = $1`, tv.TemplateVersionID)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("NP-05: in-place edit must be refused, got %v", err)
	}
	// A correction is a new version under the same template id.
	v2, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: i.IntentID, Channel: "EMAIL", Locale: "en-GB",
		Subject: "Invoice {{invoice_no}}", Body: "<p>Corrected {{invoice_no}}</p>"})
	h.must(err)
	if v2.TemplateID != tv.TemplateID || v2.Version != 2 {
		t.Fatalf("new version should share template id at version 2, got %s v%d", v2.TemplateID, v2.Version)
	}
}

func TestNCD01_ValidationBlocksUnsafeContent(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) {
		in.VariableContract = append(in.VariableContract, ncd.VariableSpec{Name: "salary", Type: "money", Sensitivity: "S3"})
	})
	cases := map[string][2]string{
		"script":            {"Invoice", "<p>hi</p><script>alert(1)</script>"},
		"event handler":     {"Invoice", `<p onclick="x()">hi</p>`},
		"S3 in subject":     {"Salary {{salary}}", "<p>hi</p>"},
		"S3 in email body":  {"Invoice", "<p>{{salary}}</p>"},
		"undeclared var":    {"Invoice", "<p>{{nope}}</p>"},
		"unapproved link":   {"Invoice", `<a href="https://evil.example.net/x">x</a>`},
		"http link":         {"Invoice", `<a href="http://example.com">x</a>`},
		"promotional block": {"Invoice", `<p>hi</p><section data-ncd-block="promotional">buy</section>`},
		"img without alt":   {"Invoice", `<img src="cid:logo">`},
		"tracking pixel":    {"Invoice", `<img alt="" width="1" height="1" src="https://t.example.com/p.gif">`},
	}
	for name, c := range cases {
		tv, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: i.IntentID, Channel: "EMAIL", Locale: "en-GB", Subject: c[0], Body: c[1]})
		h.must(err)
		tv, err = h.svc.ValidateTemplate(h.ctx, h.author, tv.TemplateVersionID)
		h.must(err)
		if tv.Status != ncd.TemplateDraft || tv.ValidationReport.Passed {
			t.Errorf("%s: validation should BLOCK and keep DRAFT, got %s passed=%v", name, tv.Status, tv.ValidationReport.Passed)
		}
	}
	// The same S3 value IS allowed on the authenticated in-app surface.
	tv, err := h.svc.CreateTemplate(h.ctx, h.author, ncd.TemplateInput{IntentID: i.IntentID, Channel: "IN_APP", Locale: "en-GB",
		Subject: "Pay statement", Body: "<p>{{salary}}</p>"})
	h.must(err)
	tv, err = h.svc.ValidateTemplate(h.ctx, h.author, tv.TemplateVersionID)
	h.must(err)
	if tv.Status != ncd.TemplateReview {
		t.Fatalf("an S3 value belongs on the in-app surface: %+v", tv.ValidationReport)
	}
}

func TestNCD01_EffectiveIsBitemporal(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	before := h.clock.now()
	h.clock.advance(time.Second)
	v1 := h.publish(i.IntentID, "EMAIL", "en-GB", "Invoice {{invoice_no}}", "<p>v1 {{invoice_no}}</p>", nil)
	h.clock.advance(time.Second)
	mid := h.clock.now()
	h.clock.advance(time.Second)
	v2 := h.publish(i.IntentID, "EMAIL", "en-GB", "Invoice {{invoice_no}}", "<p>v2 {{invoice_no}}</p>", nil)
	after := h.clock.now().Add(time.Second)

	set, err := h.svc.Effective(h.ctx, h.author, i.IntentID, after, after)
	h.must(err)
	if len(set.Templates) != 1 || set.Templates[0].TemplateVersionID != v2.TemplateVersionID {
		t.Fatalf("now: expected v2, got %+v", set.Templates)
	}
	// As known at `mid`, only v1 had been published.
	set, err = h.svc.Effective(h.ctx, h.author, i.IntentID, after, mid)
	h.must(err)
	if len(set.Templates) != 1 || set.Templates[0].TemplateVersionID != v1.TemplateVersionID {
		t.Fatalf("as known at mid: expected v1, got %+v", set.Templates)
	}
	// Before the intent was activated nothing was effective (NCD-002).
	_, err = h.svc.Effective(h.ctx, h.author, i.IntentID, before.Add(-time.Hour), before.Add(-time.Hour))
	h.refusal(err, ncd.NCD002IntentNotEffective)
	_, err = h.svc.Effective(h.ctx, h.author, "00000000-0000-0000-0000-000000000000", after, after)
	h.refusal(err, ncd.NCD001IntentNotFound)
}

func TestNCD01_RenderPreviewIsSyntheticAndRedacted(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	tv := h.publish(i.IntentID, "IN_APP", "en-GB", "Invoice {{invoice_no}}", "<p>{{invoice_no}} {{amount}}</p>", nil)
	p, err := h.svc.RenderPreview(h.ctx, h.author, ncd.PreviewInput{TemplateVersionID: tv.TemplateVersionID})
	h.must(err)
	if !p.Synthetic || !strings.Contains(p.Body, "[INVOICE_NO]") {
		t.Fatalf("default preview must use synthetic data: %+v", p)
	}
	p, err = h.svc.RenderPreview(h.ctx, h.author, ncd.PreviewInput{TemplateVersionID: tv.TemplateVersionID,
		Variables: map[string]string{"invoice_no": "INV-9", "amount": "999.00 GBP"}})
	h.must(err)
	if strings.Contains(p.Body, "999.00") || len(p.Redacted) != 1 {
		t.Fatalf("an S2 value supplied to a preview must be redacted: %+v", p)
	}
	var n int
	h.must(h.admin.QueryRow(h.ctx, `SELECT count(*) FROM ncd_communications WHERE tenant_id = $1`, h.tenant).Scan(&n))
	if n != 0 {
		t.Fatalf("a preview is side-effect free; %d communications exist", n)
	}
}
