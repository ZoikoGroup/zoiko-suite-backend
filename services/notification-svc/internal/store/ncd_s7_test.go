package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/ncd"
	"zoiko.io/notification-svc/internal/privacy"
	"zoiko.io/notification-svc/internal/recordvault"
)

type scriptedPRV struct {
	d   *privacy.Decision
	err error
	got []privacy.Request
}

func (s *scriptedPRV) Decide(_ context.Context, r privacy.Request) (*privacy.Decision, error) {
	s.got = append(s.got, r)
	return s.d, s.err
}

// S7-B1: the channel decision asks PRV itself and a caller-supplied permission is ignored.
func TestS7B1_ChannelDecisionAsksPRVAndIgnoresTheCaller(t *testing.T) {
	h := newHarness(t)
	bound := h.intent(func(in *ncd.IntentInput) { in.PrivacyActivityID, in.PrivacyPurposeID = "act-invoicing", "purp-billing" })
	if bound.PrivacyActivityID != "act-invoicing" || bound.PrivacyPurposeID != "purp-billing" {
		t.Fatalf("the privacy binding is stored with the intent version: %+v", bound)
	}
	r := h.recipient("p@example.com")

	prv := &scriptedPRV{d: &privacy.Decision{DecisionID: "prv-block-1", Result: privacy.ResultBlock}}
	h.svc.SetPrivacyDecider(prv)
	d := decide(h, bound.IntentID, r, ncd.PermissionDecision{Decision: "PERMIT", DecisionID: "forged"}, ncd.PermissionDecision{})
	if d.Outcome != ncd.DecisionBlocked || d.ReasonCodes[0] != ncd.NCD008PrivacyPermissionBlocked {
		t.Fatalf("a PRV BLOCK beats a caller PERMIT: %s %v", d.Outcome, d.ReasonCodes)
	}
	if got := d.Inputs["privacy_source"]; got != "PRV" {
		t.Fatalf("the decision records where the permission came from: %v", d.Inputs)
	}
	if len(prv.got) != 1 || prv.got[0].SubjectRef != r || prv.got[0].ActivityID != "act-invoicing" || prv.got[0].TenantID != h.tenant {
		t.Fatalf("PRV is asked about the recipient, the intent binding and the tenant: %+v", prv.got)
	}

	// PERMIT proceeds, and the decision carries PRV's decision id as evidence.
	prv.d = &privacy.Decision{DecisionID: "prv-permit-1", Result: privacy.ResultPermit}
	d = decide(h, bound.IntentID, r, ncd.PermissionDecision{Decision: "DENY"}, ncd.PermissionDecision{})
	if d.Outcome == ncd.DecisionBlocked || d.Outcome == ncd.DecisionReviewRequired {
		t.Fatalf("PRV PERMIT proceeds even when a caller says DENY: %s", d.Outcome)
	}
	raw, _ := json.Marshal(d.Inputs)
	if !strings.Contains(string(raw), "prv-permit-1") {
		t.Fatalf("the PRV decision id is recorded in the decision inputs: %s", raw)
	}

	// An outage fails closed to review; it never falls back to the caller.
	prv.d, prv.err = nil, privacy.ErrUnavailable
	if d = decide(h, bound.IntentID, r, ncd.PermissionDecision{Decision: "PERMIT"}, ncd.PermissionDecision{}); d.Outcome != ncd.DecisionReviewRequired {
		t.Fatalf("an unreachable PRV must fail closed: %s", d.Outcome)
	}

	// An intent that names no binding is never asked and never permitted.
	prv.err = nil
	prv.d = &privacy.Decision{DecisionID: "x", Result: privacy.ResultPermit}
	n := len(prv.got)
	unbound := h.intent(func(in *ncd.IntentInput) { in.IntentCode = "unbound.intent" })
	if d = decide(h, unbound.IntentID, r, ncd.PermissionDecision{Decision: "PERMIT"}, ncd.PermissionDecision{}); d.Outcome != ncd.DecisionReviewRequired || len(prv.got) != n {
		t.Fatalf("no binding: review, and nothing asked: %s asked=%d", d.Outcome, len(prv.got)-n)
	}
}

func TestS7B1_AnActiveIntentVersionsPrivacyBindingIsImmutable(t *testing.T) {
	h := newHarness(t)
	i := h.intent(func(in *ncd.IntentInput) { in.PrivacyActivityID, in.PrivacyPurposeID = "act-1", "purp-1" })
	_, err := h.admin.Exec(h.ctx, `UPDATE ncd_communication_intents SET privacy_purpose_id = 'purp-2' WHERE intent_id = $1`, i.IntentID)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a published version's binding must not change: %v", err)
	}
	if _, err := h.svc.CreateIntent(h.ctx, h.author, ncd.IntentInput{LegalEntityID: h.entity, IntentCode: "half.bound", DisplayName: "x",
		PurposeClass: ncd.PurposeTransactional, DomainOwner: "d", Sensitivity: "S0", Urgency: "U0", EvidenceClass: "E0",
		AllowedChannels: []string{"EMAIL"}, PrivacyActivityID: "only-activity"}); err == nil {
		t.Fatal("an activity without a purpose must be refused")
	}
}

type scriptedVault struct {
	got []recordvault.Declaration
	err error
}

func (s *scriptedVault) Declare(_ context.Context, d recordvault.Declaration) (string, error) {
	s.got = append(s.got, d)
	if s.err != nil {
		return "", s.err
	}
	return "rec-from-vault-1", nil
}

// S7-B2: the evidence package is declared in the vault by this service and the record id lands on the notice.
func TestS7B2_NoticeEvidencePackageIsDeclaredInTheVault(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("emp@example.com")
	_, n := h.preparedNotice(i, r, h.clock.now().Add(72*time.Hour), nil, nil)
	in := ncd.VaultDeclareInput{JurisdictionScope: "GB", RetentionScheduleRef: "ret-7y"}

	// No vault configured: the notice stays undeclared and the error says why.
	_, err := h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, in)
	h.kind(err, ncd.KindUnavailable, "record_vault_not_configured")

	v := &scriptedVault{}
	h.svc.SetRecordVault(v)
	_, err = h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, ncd.VaultDeclareInput{})
	h.kind(err, ncd.KindInvalid, "missing_fields")

	v.err = recordvault.ErrUnavailable
	_, err = h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, in)
	h.kind(err, ncd.KindUnavailable, "record_vault_unavailable")
	v.err = recordvault.ErrRejected
	_, err = h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, in)
	h.kind(err, ncd.KindInvalid, "record_declaration_rejected")
	if cur, _, err := h.svc.GetNotice(h.ctx, h.author, n.NoticeID, 0); err != nil || cur.Notice.DRCRecordRef != "" {
		t.Fatalf("a failed declaration leaves the notice undeclared: %+v %v", cur, err)
	}

	v.err, v.got = nil, nil
	got, err := h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, in)
	h.must(err)
	if got.DRCRecordRef != "rec-from-vault-1" || got.RecordStatus != "DECLARED" {
		t.Fatalf("the record id the vault returned is attached: %+v", got)
	}
	if len(v.got) != 1 {
		t.Fatalf("one declaration, got %d", len(v.got))
	}
	d := v.got[0]
	if d.RecordClass != "COMPLIANCE_EVIDENCE" || d.JurisdictionScope != "GB" || d.RetentionScheduleRef != "ret-7y" || d.LegalEntityID != h.entity ||
		d.TenantID != h.tenant || d.IdempotencyKey != "ncd-notice-"+n.NoticeID+"-v1" {
		t.Fatalf("declaration fields: %+v", d)
	}
	var pkg map[string]any
	if err := json.Unmarshal(d.Package, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg["notice_id"] != n.NoticeID || pkg["content_hash"] != n.ContentHash || pkg["legal_sufficiency"] != ncd.LegalSufficiencyNotDetermined {
		t.Fatalf("the package carries the notice, its content hash and never a legal conclusion: %v", pkg)
	}
	if strings.Contains(string(d.Package), "emp@example.com") {
		t.Fatal("the package holds no recipient address")
	}

	// Declared once.
	_, err = h.svc.DeclareRecordViaVault(h.ctx, h.author, n.NoticeID, in)
	h.kind(err, ncd.KindConflict, "already_declared")
	if len(v.got) != 1 {
		t.Fatal("an already declared notice must not reach the vault again")
	}
}
