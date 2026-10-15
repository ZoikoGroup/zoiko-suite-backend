package ncd

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NoticeInput is POST /v1/regulated-notices.
type NoticeInput struct {
	CommunicationID    string     `json:"communication_id"`
	LegalBasisRef      string     `json:"legal_basis_ref"`
	RecipientCapacity  string     `json:"recipient_capacity"`
	DeliveryMethods    []string   `json:"delivery_methods,omitempty"`
	EffectiveDate      string     `json:"effective_date,omitempty"`
	WFCObligationRef   string     `json:"wfc_obligation_ref,omitempty"`
	DeadlineAt         *time.Time `json:"deadline_at,omitempty"`
	AckRequirement     string     `json:"ack_requirement,omitempty"`
	SupersedesNoticeID string     `json:"supersedes_notice_id,omitempty"`
	SupersessionReason string     `json:"supersession_reason,omitempty"`
}

// CreateNotice builds the §8.1 package over a PREPARED communication whose
// content is already pinned. A notice that supersedes another is its next
// version, bound to a correction communication; the original stays intact.
func (s *Service) CreateNotice(ctx context.Context, a Actor, in NoticeInput) (*RegulatedNotice, error) {
	if in.CommunicationID == "" || strings.TrimSpace(in.LegalBasisRef) == "" || strings.TrimSpace(in.RecipientCapacity) == "" {
		return nil, Invalid("missing_fields", "communication_id, legal_basis_ref (the PDC decision/rule) and recipient_capacity are required")
	}
	if (in.SupersedesNoticeID == "") != (strings.TrimSpace(in.SupersessionReason) == "") {
		return nil, Invalid("supersession_needs_reason", "supersedes_notice_id and supersession_reason are supplied together (§8.4)")
	}
	if in.EffectiveDate != "" {
		if _, err := time.Parse("2006-01-02", in.EffectiveDate); err != nil {
			return nil, Invalid("invalid_effective_date", "effective_date must be YYYY-MM-DD")
		}
	}
	var out *RegulatedNotice
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		c, err := tx.GetCommunication(in.CommunicationID, true)
		if err != nil {
			return mapNotFound(err, "communication_not_found", "communication %s", in.CommunicationID)
		}
		if c.LifecycleState != CommPrepared {
			return Conflict("not_prepared", "a notice package is built over a PREPARED communication; %s is %s", c.CommunicationID, c.LifecycleState)
		}
		intent, err := tx.GetIntent(c.IntentID, c.IntentVersion)
		if err != nil {
			return err
		}
		if intent.PurposeClass != PurposeRegulated {
			return Conflict("not_regulated", "intent %s is %s; regulated notices are for REGULATED_RIGHTS_AFFECTING intents", intent.IntentID, intent.PurposeClass)
		}
		if _, err := tx.GetNoticeByCommunication(c.CommunicationID); err == nil {
			return Conflict("notice_exists", "communication %s already has a notice package", c.CommunicationID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		ack := in.AckRequirement
		if ack == "" {
			ack = intent.AckRequirement
		}
		if !ValidAckRequirement(ack) {
			return Invalid("invalid_ack_requirement", "ack_requirement must be NONE, RECEIPT, AUTHENTICATED_ACK or ACCEPTANCE")
		}
		if ack != AckNone && in.DeadlineAt == nil {
			return Invalid("deadline_required", "an acknowledgment requirement needs deadline_at, the WFC-owned clock")
		}
		renders, err := tx.ListRenders(c.CommunicationID)
		if err != nil {
			return err
		}
		n := &RegulatedNotice{
			NoticeID: uuid.NewString(), NoticeVersion: 1, TenantID: a.TenantID, LegalEntityID: c.LegalEntityID,
			CommunicationID: c.CommunicationID, LegalBasisRef: in.LegalBasisRef, RecipientCapacity: in.RecipientCapacity,
			DeliveryMethods: nonNilStrings(in.DeliveryMethods), ContentHash: combinedContentHash(renders, c.Attachments),
			AttachmentManifest: c.Attachments, Locale: c.Locale, EffectiveDate: in.EffectiveDate,
			WFCObligationRef: in.WFCObligationRef, DeadlineAt: in.DeadlineAt, AckRequirement: ack,
			EvidenceRequirement: intent.EvidenceClass, RecordRequirement: intent.RecordRequirement, RecordStatus: "NOT_REQUIRED",
			State: NoticePrepared, CreatedByPrincipalID: a.PrincipalID, CreatedAt: s.now(), StateChangedAt: s.now(),
		}
		if len(n.DeliveryMethods) == 0 {
			n.DeliveryMethods = intent.AllowedChannels
		}
		if in.SupersedesNoticeID != "" {
			prior, err := tx.GetNotice(in.SupersedesNoticeID, 0)
			if err != nil {
				return mapNotFound(err, "notice_not_found", "notice %s", in.SupersedesNoticeID)
			}
			if c.SupersedesCommunicationID != prior.CommunicationID {
				return Invalid("correction_mismatch", "a superseding notice rides on a correction communication of the notice it supersedes")
			}
			n.NoticeID, n.NoticeVersion = prior.NoticeID, prior.NoticeVersion+1
			n.SupersedesNoticeID, n.SupersessionReason = prior.NoticeID, in.SupersessionReason
			prior.State = NoticeSuperseded
			if err := tx.UpdateNotice(prior); err != nil {
				return err
			}
		}
		if err := tx.InsertNotice(n); err != nil {
			return err
		}
		n.LegalSufficiency = LegalSufficiencyNotDetermined
		out = n
		return nil
	})
	return out, err
}

// NoticeView is GET /v1/regulated-notices/{id}.
type NoticeView struct {
	Notice   *RegulatedNotice  `json:"notice"`
	Versions []RegulatedNotice `json:"versions"`
	Acks     []Acknowledgment  `json:"acknowledgments"`
	// Content is the in-app rendering the recipient acknowledges against,
	// returned only to the recipient.
	Content *struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	} `json:"content,omitempty"`
}

// GetNotice returns the latest (or a specific) version with its history —
// the UI must not hide that a prior version was sent (§8.4).
func (s *Service) GetNotice(ctx context.Context, a Actor, id string, version int) (*NoticeView, *Communication, error) {
	v := &NoticeView{}
	var comm *Communication
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		n, err := tx.GetNotice(id, version)
		if err != nil {
			return mapNotFound(err, "notice_not_found", "notice %s", id)
		}
		n.LegalSufficiency = LegalSufficiencyNotDetermined
		v.Notice = n
		if v.Versions, err = tx.ListNoticeVersions(id); err != nil {
			return err
		}
		for i := range v.Versions {
			v.Versions[i].LegalSufficiency = LegalSufficiencyNotDetermined
		}
		if v.Acks, err = tx.ListAcks(id); err != nil {
			return err
		}
		if comm, err = tx.GetCommunication(n.CommunicationID, false); err != nil {
			return err
		}
		if comm.RecipientPrincipalID == a.PrincipalID {
			rs, err := tx.ListRenders(n.CommunicationID)
			if err != nil {
				return err
			}
			for _, r := range rs {
				if r.Channel == ChannelInApp || v.Content == nil {
					v.Content = &struct {
						Subject string `json:"subject"`
						Body    string `json:"body"`
					}{r.Subject, r.Body}
				}
			}
		}
		return nil
	})
	if v.Acks == nil {
		v.Acks = []Acknowledgment{}
	}
	return v, comm, err
}

// AckInput is POST /v1/acknowledgments.
type AckInput struct {
	NoticeID      string `json:"notice_id"`
	NoticeVersion int    `json:"notice_version"`
	Disposition   string `json:"disposition"`
	ContentHash   string `json:"content_hash"`
	Comment       string `json:"comment,omitempty"`
}

// Acknowledge records an authenticated acknowledgment, decline or dispute
// bound to the exact notice version and content the actor was shown (INV-21).
// The actor is the verified caller and must be the recipient; an open pixel,
// a click or an operator's say-so never reaches here (INV-20, NP-41).
func (s *Service) Acknowledge(ctx context.Context, a Actor, in AckInput) (*Acknowledgment, error) {
	if in.NoticeID == "" || in.NoticeVersion < 1 || in.ContentHash == "" {
		return nil, Invalid("missing_fields", "notice_id, notice_version and content_hash are required")
	}
	switch in.Disposition {
	case "ACKNOWLEDGED", "DECLINED", "DISPUTED":
	default:
		return nil, Invalid("invalid_disposition", "disposition must be ACKNOWLEDGED, DECLINED or DISPUTED")
	}
	var out *Acknowledgment
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		n, err := tx.GetNotice(in.NoticeID, in.NoticeVersion)
		if err != nil {
			return mapNotFound(err, "notice_not_found", "notice %s version %d", in.NoticeID, in.NoticeVersion)
		}
		c, err := tx.GetCommunication(n.CommunicationID, true)
		if err != nil {
			return err
		}
		if c.RecipientPrincipalID != a.PrincipalID {
			return Refused(Refuse(NCD017AcknowledgmentInvalid, "only the authenticated recipient can acknowledge a notice"))
		}
		latest, err := tx.GetNotice(in.NoticeID, 0)
		if err != nil {
			return err
		}
		if n.State == NoticeSuperseded || latest.NoticeVersion != n.NoticeVersion {
			return Refused(Refuse(NCD017AcknowledgmentInvalid,
				"notice version "+itoa(n.NoticeVersion)+" is superseded; acknowledge the current version (NP-40)"))
		}
		if in.ContentHash != n.ContentHash {
			return Refused(Refuse(NCD017AcknowledgmentInvalid, "content_hash does not match the notice's exact content; nobody acknowledges a version they were not shown (NP-40)"))
		}
		if n.AckRequirement == AckNone {
			return Refused(Refuse(NCD017AcknowledgmentInvalid, "this notice has no acknowledgment requirement"))
		}
		if n.AckRequirement == AckReceipt && in.Disposition != "ACKNOWLEDGED" {
			return Refused(Refuse(NCD017AcknowledgmentInvalid, "a RECEIPT requirement is acknowledged, not declined or disputed"))
		}
		if n.State != NoticeAckPending {
			return Refused(Refuse(NCD017AcknowledgmentInvalid, "the notice is "+n.State+", not awaiting acknowledgment"))
		}
		now := s.now()
		ack := &Acknowledgment{AckID: uuid.NewString(), TenantID: a.TenantID, NoticeID: n.NoticeID, NoticeVersion: n.NoticeVersion,
			CommunicationID: n.CommunicationID, ActorPrincipalID: a.PrincipalID, Method: "AUTHENTICATED_IN_APP",
			Disposition: in.Disposition, ContentHash: in.ContentHash, Comment: in.Comment, AcknowledgedAt: now}
		if err := tx.InsertAck(ack); err != nil {
			return err
		}
		if err := s.concludeAck(tx, a, c, n, ack, "HUMAN"); err != nil {
			return err
		}
		out = ack
		return nil
	})
	return out, err
}

func (s *Service) concludeAck(tx Tx, a Actor, c *Communication, n *RegulatedNotice, ack *Acknowledgment, source string) error {
	switch ack.Disposition {
	case "ACKNOWLEDGED":
		n.State = NoticeAcknowledged
	case "DECLINED":
		n.State = NoticeDeclined
	case "DISPUTED":
		n.State = NoticeDisputed
	}
	if n.RecordRequirement {
		n.RecordStatus = "PENDING"
	}
	if err := tx.UpdateNotice(n); err != nil {
		return err
	}
	if err := s.recordEvidence(tx, a, c, &Evidence{NoticeID: n.NoticeID, EvidenceType: ack.Disposition, NormalizedState: ack.Disposition,
		Source: source, Confidence: "HIGH", ActorPrincipalID: ack.ActorPrincipalID, ObservedAt: ack.AcknowledgedAt,
		DoesNotProve: "the truth of the content, a waiver of rights, or legal sufficiency of service (§3.3, INV-22)",
		Details:      map[string]any{"ack_id": ack.AckID, "method": ack.Method, "notice_version": n.NoticeVersion, "content_hash": ack.ContentHash, "evidence_ref": ack.EvidenceRef}}); err != nil {
		return err
	}
	return emit(tx, a, EvtNoticeAck, n.LegalEntityID, n.CommunicationID, map[string]any{
		"notice_id": n.NoticeID, "notice_version": n.NoticeVersion, "communication_id": n.CommunicationID,
		"actor_id": ack.ActorPrincipalID, "method": ack.Method, "disposition": ack.Disposition, "timestamp": ack.AcknowledgedAt,
	})
}

// ManualEvidenceInput is POST /v1/regulated-notices/{id}/manual-evidence.
type ManualEvidenceInput struct {
	Kind        string `json:"kind"` // DELIVERY_EVIDENCED or RECEIPT_ACKNOWLEDGED
	ArtifactRef string `json:"artifact_ref"`
	Reason      string `json:"reason"`
}

// RequestManualEvidence files operator evidence for a maker-checker
// approval. It changes nothing until a second principal approves (NP-41).
func (s *Service) RequestManualEvidence(ctx context.Context, a Actor, noticeID string, in ManualEvidenceInput) (*Approval, error) {
	if in.Kind != "DELIVERY_EVIDENCED" && in.Kind != "RECEIPT_ACKNOWLEDGED" {
		return nil, Invalid("invalid_kind", "kind must be DELIVERY_EVIDENCED or RECEIPT_ACKNOWLEDGED")
	}
	if strings.TrimSpace(in.ArtifactRef) == "" || strings.TrimSpace(in.Reason) == "" {
		return nil, Invalid("missing_fields", "artifact_ref and reason are required: operator evidence needs a source artifact (§7.1 manual proof)")
	}
	var appr *Approval
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		n, err := tx.GetNotice(noticeID, 0)
		if err != nil {
			return mapNotFound(err, "notice_not_found", "notice %s", noticeID)
		}
		appr = &Approval{ApprovalID: uuid.NewString(), TenantID: a.TenantID, LegalEntityID: n.LegalEntityID, Kind: ApprovalManualEvidence,
			TargetID: n.CommunicationID, Payload: map[string]any{"notice_id": n.NoticeID, "notice_version": n.NoticeVersion,
				"kind": in.Kind, "artifact_ref": in.ArtifactRef}, Reason: in.Reason, Status: "PENDING",
			RequestedBy: a.PrincipalID, RequestedAt: s.now()}
		return tx.InsertApproval(appr)
	})
	return appr, err
}

// applyManualEvidence runs when a MANUAL_EVIDENCE approval is approved.
func (s *Service) applyManualEvidence(tx Tx, a Actor, appr *Approval) error {
	noticeID, _ := appr.Payload["notice_id"].(string)
	kind, _ := appr.Payload["kind"].(string)
	artifact, _ := appr.Payload["artifact_ref"].(string)
	n, err := tx.GetNotice(noticeID, 0)
	if err != nil {
		return err
	}
	c, err := tx.GetCommunication(n.CommunicationID, true)
	if err != nil {
		return err
	}
	now := s.now()
	if err := s.recordEvidence(tx, a, c, &Evidence{NoticeID: n.NoticeID, EvidenceType: "OPERATOR_EVIDENCE_ADDED", NormalizedState: kind,
		Source: "OPERATOR", Confidence: "MEDIUM", ActorPrincipalID: appr.RequestedBy, ObservedAt: now,
		DoesNotProve: "more than the cited artifact shows; not legal service (§8.5)",
		Details:      map[string]any{"artifact_ref": artifact, "reason": appr.Reason, "approved_by": a.PrincipalID, "approval_id": appr.ApprovalID}}); err != nil {
		return err
	}
	switch kind {
	case "DELIVERY_EVIDENCED":
		switch n.State {
		case NoticeReady, NoticeInProgress, NoticeException, NoticePrepared:
			if n.AckRequirement == AckNone {
				n.State = NoticeSatisfiedByPolicy
				if n.RecordRequirement {
					n.RecordStatus = "PENDING"
				}
			} else {
				n.State = NoticeAckPending
			}
			return tx.UpdateNotice(n)
		}
		return Conflict("notice_state", "notice is %s; delivery evidence no longer changes it", n.State)
	default: // RECEIPT_ACKNOWLEDGED
		if n.State != NoticeAckPending {
			return Conflict("notice_state", "notice is %s, not awaiting acknowledgment", n.State)
		}
		ack := &Acknowledgment{AckID: uuid.NewString(), TenantID: tx.TenantID(), NoticeID: n.NoticeID, NoticeVersion: n.NoticeVersion,
			CommunicationID: n.CommunicationID, ActorPrincipalID: appr.RequestedBy, Method: "OPERATOR_RECORDED",
			Disposition: "ACKNOWLEDGED", ContentHash: n.ContentHash, EvidenceRef: artifact,
			Comment: "operator-recorded receipt, approved by " + a.PrincipalID, AcknowledgedAt: now}
		if err := tx.InsertAck(ack); err != nil {
			return err
		}
		return s.concludeAck(tx, a, c, n, ack, "OPERATOR")
	}
}

// DeclareRecord attaches the DRC record reference (§8.1 record declaration)
// and announces communication.record.declared.
func (s *Service) DeclareRecord(ctx context.Context, a Actor, noticeID, drcRef string) (*RegulatedNotice, error) {
	if strings.TrimSpace(drcRef) == "" {
		return nil, Invalid("missing_fields", "drc_record_ref is required")
	}
	var out *RegulatedNotice
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		n, err := tx.GetNotice(noticeID, 0)
		if err != nil {
			return mapNotFound(err, "notice_not_found", "notice %s", noticeID)
		}
		if n.DRCRecordRef != "" {
			return Conflict("already_declared", "notice %s is already declared as %s", noticeID, n.DRCRecordRef)
		}
		n.DRCRecordRef, n.RecordStatus = drcRef, "DECLARED"
		if err := tx.UpdateNotice(n); err != nil {
			return err
		}
		if x, err := tx.ListExceptions(true, 500); err == nil {
			for _, e := range x {
				if e.NoticeID == noticeID && e.Kind == "RECORD_HANDOFF_PENDING" {
					_ = tx.ResolveException(e.ExceptionID, a.PrincipalID, "declared as "+drcRef, s.now())
				}
			}
		}
		n.LegalSufficiency = LegalSufficiencyNotDetermined
		out = n
		return emit(tx, a, EvtRecordDeclared, n.LegalEntityID, n.CommunicationID, map[string]any{
			"communication_id": n.CommunicationID, "notice_id": n.NoticeID, "notice_version": n.NoticeVersion,
			"drc_record_ref": drcRef, "evidence_package_hash": n.ContentHash,
		})
	})
	return out, err
}

// SweepNotice runs the WFC clock on one notice: at-risk warning, expiry of an
// unacknowledged notice (never a fabricated acknowledgment, NP-42), and the
// record-handoff escalation (NP-58).
func (s *Service) SweepNotice(ctx context.Context, ref WorkRef) error {
	a := s.workerActor(ref.TenantID, ref.ID)
	return s.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		n, err := tx.GetNotice(ref.ID, 0)
		if err != nil {
			return err
		}
		now := s.now()
		active := n.State == NoticeReady || n.State == NoticeInProgress || n.State == NoticeDeliveryEvidenced || n.State == NoticeAckPending || n.State == NoticeException
		if active && n.DeadlineAt != nil {
			if !now.Before(*n.DeadlineAt) {
				if n.State == NoticeAckPending {
					n.State = NoticeExpired
					if n.RecordRequirement {
						n.RecordStatus = "PENDING"
					}
				}
				if err := tx.UpdateNotice(n); err != nil {
					return err
				}
				created, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: ref.TenantID, CommunicationID: n.CommunicationID,
					NoticeID: n.NoticeID, Kind: "ACK_EXPIRED", ReasonCode: NCD016EvidenceInsufficient,
					Detail: "deadline passed without the required evidence/acknowledgment; WFC escalation, no acknowledgment is assumed", CreatedAt: now})
				if err != nil || !created {
					return err
				}
				return emit(tx, a, EvtDeadlineAtRisk, n.LegalEntityID, n.CommunicationID, map[string]any{
					"notice_id": n.NoticeID, "notice_version": n.NoticeVersion, "communication_id": n.CommunicationID,
					"workflow_ref": n.WFCObligationRef, "deadline_at": n.DeadlineAt, "state": n.State,
					"deficiency": "deadline passed", "escalation_reason": "acknowledgment not received by deadline",
				})
			}
			if n.AtRiskNotifiedAt == nil && n.DeadlineAt.Sub(now) <= s.limits.DeadlineAtRiskWindow {
				return s.atRisk(tx, a, n, "deadline within "+s.limits.DeadlineAtRiskWindow.String()+" and the notice is still "+n.State)
			}
		}
		if n.RecordRequirement && n.RecordStatus == "PENDING" && now.Sub(n.StateChangedAt) >= s.limits.RecordHandoffWindow {
			_, err := tx.InsertException(&Exception{ExceptionID: uuid.NewString(), TenantID: ref.TenantID, CommunicationID: n.CommunicationID,
				NoticeID: n.NoticeID, Kind: "RECORD_HANDOFF_PENDING",
				Detail: "the notice concluded but its DRC record declaration has not arrived; the record-required workflow is incomplete (NP-58)", CreatedAt: now})
			return err
		}
		return nil
	})
}

func itoa(i int) string { return strconv.Itoa(i) }
