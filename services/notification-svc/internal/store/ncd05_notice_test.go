package store_test

import (
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/ncd"
)

// NCD-05 — regulated notice, acknowledgment and record (§8), plus governed
// bulk sends (§6.5) and the misdelivery path (NP-44).

var inAppNotice = [4]string{"IN_APP", "en-GB", "Consultation notice", "<p>Consultation notice {{invoice_no}}. Please acknowledge.</p>"}

func (h *harness) regulatedIntent() *ncd.Intent {
	return h.intent(func(in *ncd.IntentInput) {
		in.IntentCode, in.PurposeClass, in.EvidenceClass = "hr.consultation", ncd.PurposeRegulated, "E3"
		in.AckRequirement, in.RecordRequirement, in.Mandatory = ncd.AckAuthenticatedAck, true, true
		in.AllowedChannels = []string{"IN_APP", "EMAIL"}
	}, inAppNotice)
}

// preparedNotice creates, prepares and wraps a regulated communication.
func (h *harness) preparedNotice(i *ncd.Intent, recipient string, deadline time.Time, mod func(*ncd.CommunicationInput), nmod func(*ncd.NoticeInput)) (*ncd.Communication, *ncd.RegulatedNotice) {
	h.t.Helper()
	c := h.comm(i.IntentID, recipient, mod)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		h.t.Fatalf("prepare refused: %+v", p.Refusal)
	}
	in := ncd.NoticeInput{CommunicationID: c.CommunicationID, LegalBasisRef: "PDC-RULE-UK-CONSULT-7", RecipientCapacity: "employee",
		DeadlineAt: &deadline, WFCObligationRef: "wfc-obl-1"}
	if nmod != nil {
		nmod(&in)
	}
	n, err := h.svc.CreateNotice(h.ctx, h.author, in)
	h.must(err)
	return c, n
}

func TestNCD05_RegulatedNeedsPackageBeforeDispatch(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("")
	c := h.comm(i.IntentID, r, nil)
	p, err := h.svc.Prepare(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if p.Refusal != nil {
		t.Fatal(p.Refusal)
	}
	_, _, err = h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.refusal(err, ncd.NCD018RegulatedReviewRequired)
}

func TestNCD05_AcknowledgmentBindsActorAndExactVersion(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("emp@example.com")
	c, n := h.preparedNotice(i, r, h.clock.now().Add(72*time.Hour), nil, nil)
	if n.LegalSufficiency != ncd.LegalSufficiencyNotDetermined || n.State != ncd.NoticePrepared {
		t.Fatalf("§8.5: NCD never decides legal sufficiency: %+v", n)
	}
	_, _, err := h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	v := h.view(c.CommunicationID)
	// E3 requires authenticated display: only the in-app route qualifies.
	if len(v.Attempts) != 1 || v.Attempts[0].Channel != "IN_APP" || v.Notice.State != ncd.NoticeAckPending {
		t.Fatalf("delivered in-app notice should await acknowledgment: %+v %s", v.Attempts, v.Notice.State)
	}
	// Someone else cannot acknowledge.
	_, err = h.svc.Acknowledge(h.ctx, h.as("mallory"), ncd.AckInput{NoticeID: n.NoticeID, NoticeVersion: 1, Disposition: "ACKNOWLEDGED", ContentHash: n.ContentHash})
	h.refusal(err, ncd.NCD017AcknowledgmentInvalid)
	// The recipient must present the hash of what they were shown.
	_, err = h.svc.Acknowledge(h.ctx, h.as(r), ncd.AckInput{NoticeID: n.NoticeID, NoticeVersion: 1, Disposition: "ACKNOWLEDGED", ContentHash: "0000"})
	h.refusal(err, ncd.NCD017AcknowledgmentInvalid)
	nv, _, err := h.svc.GetNotice(h.ctx, h.as(r), n.NoticeID, 0)
	h.must(err)
	if nv.Content == nil || nv.Notice.ContentHash == "" {
		t.Fatal("the recipient reads the exact content they acknowledge")
	}
	ack, err := h.svc.Acknowledge(h.ctx, h.as(r), ncd.AckInput{NoticeID: n.NoticeID, NoticeVersion: 1, Disposition: "ACKNOWLEDGED", ContentHash: nv.Notice.ContentHash})
	h.must(err)
	if ack.ActorPrincipalID != r || ack.Method != "AUTHENTICATED_IN_APP" {
		t.Fatalf("INV-21: ack binds actor and method: %+v", ack)
	}
	v = h.view(c.CommunicationID)
	if v.Notice.State != ncd.NoticeAcknowledged || !v.Claims.Acknowledged || v.Claims.LegallyServed != ncd.LegalSufficiencyNotDetermined {
		t.Fatalf("acknowledged, but never 'legally served': %+v %+v", v.Notice.State, v.Claims)
	}
	// Record handoff: pending until DRC declares, escalated after the window.
	h.clock.advance(2 * time.Hour)
	h.svc.RunOnce(h.ctx)
	ex, _ := h.svc.ListExceptions(h.ctx, h.author, true)
	if !hasException(ex, "RECORD_HANDOFF_PENDING") {
		t.Fatal("NP-58: a missing record declaration is escalated")
	}
	d, err := h.svc.DeclareRecord(h.ctx, h.author, n.NoticeID, "drc://records/123@v1")
	h.must(err)
	if d.RecordStatus != "DECLARED" {
		t.Fatal("record declaration not stored")
	}
	ex, _ = h.svc.ListExceptions(h.ctx, h.author, true)
	if hasException(ex, "RECORD_HANDOFF_PENDING") {
		t.Fatal("declaring the record resolves the handoff exception")
	}
}

func TestNCD05_SupersededVersionCannotBeAcknowledged(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("")
	deadline := h.clock.now().Add(72 * time.Hour)
	c1, n1 := h.preparedNotice(i, r, deadline, nil, nil)
	_, _, err := h.svc.Dispatch(h.ctx, h.author, c1.CommunicationID)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	// NP-43: a correction is a new communication and a new notice version.
	c2, n2 := h.preparedNotice(i, r, deadline, func(in *ncd.CommunicationInput) {
		in.SupersedesCommunicationID, in.CorrectionReason = c1.CommunicationID, "wrong date in paragraph 2"
		in.Variables = map[string]string{"invoice_no": "INV-2"}
	}, func(in *ncd.NoticeInput) {
		in.SupersedesNoticeID, in.SupersessionReason = n1.NoticeID, "wrong date in paragraph 2"
	})
	if n2.NoticeID != n1.NoticeID || n2.NoticeVersion != 2 || n2.ContentHash == n1.ContentHash {
		t.Fatalf("correction must be version 2 of the same notice with new content: %+v", n2)
	}
	nv, _, err := h.svc.GetNotice(h.ctx, h.author, n1.NoticeID, 0)
	h.must(err)
	if len(nv.Versions) != 2 || nv.Versions[0].State != ncd.NoticeSuperseded {
		t.Fatalf("§8.4: the prior version stays visible and SUPERSEDED: %+v", nv.Versions)
	}
	// NP-40: acknowledging the superseded version is refused.
	_, err = h.svc.Acknowledge(h.ctx, h.as(r), ncd.AckInput{NoticeID: n1.NoticeID, NoticeVersion: 1, Disposition: "ACKNOWLEDGED", ContentHash: n1.ContentHash})
	h.refusal(err, ncd.NCD017AcknowledgmentInvalid)
	// The original communication and its evidence are intact.
	b, err := h.svc.Evidence(h.ctx, h.author, c1.CommunicationID)
	h.must(err)
	if len(b.Attempts) != 1 || len(b.Evidence) == 0 {
		t.Fatal("INV-19: the original communication's evidence survives the correction")
	}
	_ = c2
}

func TestNCD05_OperatorEvidenceNeedsMakerChecker(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("")
	c, n := h.preparedNotice(i, r, h.clock.now().Add(72*time.Hour), nil, nil)
	_, _, err := h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	_, err = h.svc.RequestManualEvidence(h.ctx, h.author, n.NoticeID, ncd.ManualEvidenceInput{Kind: "RECEIPT_ACKNOWLEDGED", Reason: "signed paper copy"})
	h.kind(err, ncd.KindInvalid, "missing_fields")
	appr, err := h.svc.RequestManualEvidence(h.ctx, h.author, n.NoticeID, ncd.ManualEvidenceInput{Kind: "RECEIPT_ACKNOWLEDGED",
		ArtifactRef: "drc://scan/77", Reason: "signed paper copy handed in"})
	h.must(err)
	if v := h.view(c.CommunicationID); v.Notice.State != ncd.NoticeAckPending {
		t.Fatal("NP-41: a request alone changes nothing")
	}
	_, _, err = h.svc.DecideApproval(h.ctx, h.author, appr.ApprovalID, true, "")
	h.kind(err, ncd.KindForbidden, "self_approval_forbidden")
	_, _, err = h.svc.DecideApproval(h.ctx, h.approver, appr.ApprovalID, true, "checked the scan")
	h.must(err)
	nv, _, err := h.svc.GetNotice(h.ctx, h.author, n.NoticeID, 0)
	h.must(err)
	if nv.Notice.State != ncd.NoticeAcknowledged || nv.Acks[0].Method != "OPERATOR_RECORDED" || nv.Acks[0].EvidenceRef != "drc://scan/77" {
		t.Fatalf("approved operator evidence applies with its artifact: %+v %+v", nv.Notice.State, nv.Acks)
	}
}

func TestNCD05_DeadlineExpiresWithoutFabricatingAck(t *testing.T) {
	h := newHarness(t)
	i := h.regulatedIntent()
	r := h.recipient("")
	c, n := h.preparedNotice(i, r, h.clock.now().Add(30*time.Hour), nil, nil)
	_, _, err := h.svc.Dispatch(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	h.clock.advance(10 * time.Hour) // inside the 24h at-risk window
	h.svc.RunOnce(h.ctx)
	ex, _ := h.svc.ListExceptions(h.ctx, h.author, true)
	if !hasException(ex, "DEADLINE_AT_RISK") {
		t.Fatal("notice.deadline.at_risk should be raised inside the window")
	}
	h.clock.advance(24 * time.Hour)
	h.svc.RunOnce(h.ctx)
	nv, _, err := h.svc.GetNotice(h.ctx, h.author, n.NoticeID, 0)
	h.must(err)
	if nv.Notice.State != ncd.NoticeExpired || len(nv.Acks) != 0 {
		t.Fatalf("NP-42: expiry, never an automatic acknowledgment: %s %d acks", nv.Notice.State, len(nv.Acks))
	}
	var n2 int
	h.must(h.admin.QueryRow(h.ctx, `SELECT count(*) FROM event_outbox WHERE tenant_id = $1 AND event_type = 'notice.deadline.at_risk'`, h.tenant).Scan(&n2))
	if n2 != 2 {
		t.Fatalf("expected one at-risk and one expiry escalation event, got %d", n2)
	}
}

func TestNCD06_GovernedBulkSend(t *testing.T) {
	h := newHarness(t)
	lim := ncd.DefaultLimits()
	lim.BulkApprovalThreshold = 3
	h.svc = ncd.NewService(h.store, h.resolver, ncd.RouterTransport{Email: h.email, Inbox: h.store}, lim, nil)
	h.svc.SetClock(h.clock.now)
	i := h.intent(func(in *ncd.IntentInput) { in.BulkAllowed, in.AllowedChannels = true, []string{"IN_APP"} }, inAppInvoice)
	rs := []string{h.recipient(""), h.recipient(""), h.recipient("")}
	_, _, err := h.svc.PreviewBulk(h.ctx, h.author, ncd.BulkInput{IntentID: i.IntentID, LegalEntityID: h.entity, SourceEventID: "run-1",
		Recipients: []string{"other-tenant:p1"}, Locale: "en-GB"})
	h.refusal(err, ncd.NCD020CrossTenantRecipientBlock) // NP-49
	b, appr, err := h.svc.PreviewBulk(h.ctx, h.author, ncd.BulkInput{IntentID: i.IntentID, LegalEntityID: h.entity, SourceEventID: "run-1",
		Recipients: append(rs, rs[0]), Locale: "en-GB", Variables: map[string]string{"invoice_no": "RUN-1"}})
	h.must(err)
	if b.AudienceCount != 3 || appr == nil {
		t.Fatalf("deduped count 3 at the threshold needs approval: %+v %+v", b, appr)
	}
	_, _, err = h.svc.DispatchBulk(h.ctx, h.author, b.BulkID, b.AudienceHash)
	h.kind(err, ncd.KindConflict, "approval_required")
	_, _, err = h.svc.DecideApproval(h.ctx, h.approver, appr.ApprovalID, true, "")
	h.must(err)
	_, _, err = h.svc.DispatchBulk(h.ctx, h.author, b.BulkID, ncd.AudienceHash(rs[:2]))
	h.kind(err, ncd.KindConflict, "audience_changed") // NP-50
	_, results, err := h.svc.DispatchBulk(h.ctx, h.author, b.BulkID, b.AudienceHash)
	h.must(err)
	h.svc.RunOnce(h.ctx)
	for _, r := range results {
		if r.Refusal != nil || h.view(r.CommunicationID).Claims.DeliveryState != "DELIVERED_TO_INBOX" {
			t.Fatalf("bulk recipient not delivered: %+v", r)
		}
	}
}

func TestNCD06_MisdeliveryStopsSendsAndPreservesEvidence(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil, emailInvoice, inAppInvoice)
	r := h.recipient("wrong@example.com")
	c := h.comm(i.IntentID, r, nil)
	h.send(c)
	_, err := h.svc.Misdelivery(h.ctx, h.approver, c.CommunicationID, "sent to the wrong employee")
	h.must(err)
	ex, _ := h.svc.ListExceptions(h.ctx, h.author, true)
	if !hasException(ex, "MISDELIVERY_INCIDENT") {
		t.Fatal("NP-44: a misdelivery opens a privacy/security incident")
	}
	b, err := h.svc.Evidence(h.ctx, h.author, c.CommunicationID)
	h.must(err)
	if len(b.Attempts) != 1 || len(b.Evidence) == 0 {
		t.Fatal("NP-44: evidence is preserved, never deleted")
	}
	next := h.comm(i.IntentID, r, nil)
	p, err := h.svc.Prepare(h.ctx, h.author, next.CommunicationID)
	h.must(err)
	for _, rt := range p.Decision.Routes {
		if rt.Channel == "EMAIL" {
			t.Fatal("the misdelivered endpoint is on a security hold")
		}
	}
}
