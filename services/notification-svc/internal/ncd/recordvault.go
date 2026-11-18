package ncd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"zoiko.io/notification-svc/internal/recordvault"
)

// S7-B2 (ZS-SVC-Y-001 s8.1, NP-58, TC-15, TC-20): the notice's evidence package is declared as a record in
// document-vault-svc by this service, instead of waiting for a record reference to arrive from elsewhere.
//
// What stays with a person: the jurisdiction scope of the record is a legal judgement (it binds retention rules),
// so the operator names it. The record class is fixed (COMPLIANCE_EVIDENCE) and the package states
// legal_sufficiency NOT_DETERMINED_BY_NCD, because NCD never decides it (INV-22).

// RecordVault declares an evidence package as a record. *recordvault.Client implements it.
type RecordVault interface {
	Declare(ctx context.Context, d recordvault.Declaration) (string, error)
}

// SetRecordVault turns on declaring evidence packages in document-vault-svc.
func (s *Service) SetRecordVault(v RecordVault) { s.recordVault = v }

// VaultDeclareInput is what the operator supplies to declare through the vault.
type VaultDeclareInput struct {
	JurisdictionScope    string `json:"jurisdiction_scope"`
	RetentionScheduleRef string `json:"retention_schedule_ref,omitempty"`
	DeclarationReason    string `json:"declaration_reason,omitempty"`
}

// RecordClassNoticeEvidence is the vault record class an evidence package is declared under.
const RecordClassNoticeEvidence = "COMPLIANCE_EVIDENCE"

type evidencePackage struct {
	NoticeID           string           `json:"notice_id"`
	NoticeVersion      int              `json:"notice_version"`
	CommunicationID    string           `json:"communication_id"`
	LegalBasisRef      string           `json:"legal_basis_ref"`
	RecipientCapacity  string           `json:"recipient_capacity"`
	DeliveryMethods    []string         `json:"delivery_methods"`
	ContentHash        string           `json:"content_hash"`
	AttachmentManifest []Attachment     `json:"attachment_manifest"`
	Locale             string           `json:"locale"`
	EffectiveDate      string           `json:"effective_date,omitempty"`
	DeadlineAt         *time.Time       `json:"deadline_at,omitempty"`
	AckRequirement     string           `json:"ack_requirement"`
	State              string           `json:"state"`
	Acknowledgments    []Acknowledgment `json:"acknowledgments"`
	LegalSufficiency   string           `json:"legal_sufficiency"`
	AssembledAt        time.Time        `json:"assembled_at"`
}

// DeclareRecordViaVault stores the notice's evidence package in document-vault-svc, declares it a record and
// attaches the returned record id to the notice (announcing communication.record.declared). The vault is called
// outside any database transaction; any failure leaves the notice undeclared.
func (s *Service) DeclareRecordViaVault(ctx context.Context, a Actor, noticeID string, in VaultDeclareInput) (*RegulatedNotice, error) {
	if s.recordVault == nil {
		return nil, Unavailable("record_vault_not_configured", "no document vault is configured; supply drc_record_ref instead")
	}
	if strings.TrimSpace(in.JurisdictionScope) == "" {
		return nil, Invalid("missing_fields", "jurisdiction_scope is required: it is a legal judgement that binds retention, so the operator names it")
	}
	var n *RegulatedNotice
	var pkg []byte
	err := s.store.InTx(ctx, a.TenantID, func(tx Tx) error {
		var err error
		if n, err = tx.GetNotice(noticeID, 0); err != nil {
			return mapNotFound(err, "notice_not_found", "notice %s", noticeID)
		}
		if n.DRCRecordRef != "" {
			return Conflict("already_declared", "notice %s is already declared as %s", noticeID, n.DRCRecordRef)
		}
		acks, err := tx.ListAcks(noticeID)
		if err != nil {
			return err
		}
		pkg, err = json.Marshal(evidencePackage{NoticeID: n.NoticeID, NoticeVersion: n.NoticeVersion, CommunicationID: n.CommunicationID,
			LegalBasisRef: n.LegalBasisRef, RecipientCapacity: n.RecipientCapacity, DeliveryMethods: nonNilStrings(n.DeliveryMethods),
			ContentHash: n.ContentHash, AttachmentManifest: n.AttachmentManifest, Locale: n.Locale, EffectiveDate: n.EffectiveDate,
			DeadlineAt: n.DeadlineAt, AckRequirement: n.AckRequirement, State: n.State, Acknowledgments: acks,
			LegalSufficiency: LegalSufficiencyNotDetermined, AssembledAt: s.now()})
		return err
	})
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.DeclarationReason)
	if reason == "" {
		reason = "NCD-05 regulated notice evidence package declared by " + a.PrincipalID
	}
	id, err := s.recordVault.Declare(ctx, recordvault.Declaration{
		TenantID: a.TenantID, PrincipalID: a.PrincipalID, CorrelationID: a.CorrelationID, LegalEntityID: n.LegalEntityID,
		Title:   "Regulated notice " + n.NoticeID + " v" + itoa(n.NoticeVersion) + " evidence package",
		Package: pkg, RecordClass: RecordClassNoticeEvidence, JurisdictionScope: strings.TrimSpace(in.JurisdictionScope),
		RetentionScheduleRef: strings.TrimSpace(in.RetentionScheduleRef),
		BusinessContext:      "regulated notice " + n.NoticeID + " v" + itoa(n.NoticeVersion) + ", communication " + n.CommunicationID,
		DeclarationReason:    reason, IdempotencyKey: "ncd-notice-" + n.NoticeID + "-v" + itoa(n.NoticeVersion),
	})
	switch {
	case errors.Is(err, recordvault.ErrRejected):
		return nil, Invalid("record_declaration_rejected", "%v", err)
	case err != nil:
		return nil, Unavailable("record_vault_unavailable", "%v; the notice stays undeclared", err)
	}
	return s.DeclareRecord(ctx, a, noticeID, id)
}
