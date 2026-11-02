package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MaxRecordsPerBatch bounds one validation request. A larger population is
// split into several batches by the caller; an unbounded body would let one
// request hold a transaction and the connection pool hostage.
const MaxRecordsPerBatch = 10000

// DefaultControlAmountField is the record field summed for the amount
// control total when the manifest does not name one.
const DefaultControlAmountField = "amount"

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ─── POST /v1/migrations/validate ────────────────────────────────────────────

// ValidateMigrationRequest is the body of POST /v1/migrations/validate.
type ValidateMigrationRequest struct {
	// TenantID is optional and, when present, must equal X-Tenant-Id (the
	// header is the only source of tenant authority; a disagreeing body is
	// refused 403).
	TenantID string `json:"tenant_id,omitempty"`
	// LegalEntityID may be omitted, in which case X-Legal-Entity-Id is used;
	// when both are present they must agree (403 otherwise).
	LegalEntityID  string            `json:"legal_entity_id"`
	MigrationName  string            `json:"migration_name"`
	SourceSystem   string            `json:"source_system"`
	TargetService  string            `json:"target_service"`
	RequiredFields []string          `json:"required_fields"`
	Records        []MigrationRecord `json:"records"`

	// ExceptionOwnerID is the principal accountable for resolving the run's
	// quarantine items. Required: every audit entry carries an owner
	// (MIG-INV-12), and an MQ0 can only be resolved by its owner.
	ExceptionOwnerID string `json:"exception_owner_id"`

	// ControlTotals is the source manifest's declaration the batch is
	// reconciled against (§17, MIG-INV-15). Required.
	ControlTotals *ControlTotalsDeclaration `json:"control_totals"`
}

// ControlTotalsDeclaration is what the source manifest says the batch holds.
type ControlTotalsDeclaration struct {
	// RecordCount is the source population size (pointer: 0 is a legitimate
	// declaration, absence is not).
	RecordCount *int `json:"record_count"`
	// AmountTotal is the sum of AmountField over the source population, as a
	// plain decimal string ("0" when the batch carries no amounts).
	AmountTotal string `json:"amount_total"`
	// AmountField names the record field that carries the amount; defaults
	// to "amount".
	AmountField string `json:"amount_field,omitempty"`
	// ControlHash is the optional lower-case hex SHA-256 the source computed
	// with the same algorithm as ComputeControlHash. When given, a mismatch
	// catches swapped or misclassified records whose totals still agree
	// (MIG-N29).
	ControlHash string `json:"control_hash,omitempty"`
}

// Validate checks the request's shape. Each failure is a client error (400).
// Domain rules that need the caller's identity (the exception owner may not
// be the submitter) are checked by CheckSubmissionRules.
func (r *ValidateMigrationRequest) Validate() error {
	if strings.TrimSpace(r.LegalEntityID) == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if strings.TrimSpace(r.MigrationName) == "" {
		return fmt.Errorf("migration_name is required")
	}
	if strings.TrimSpace(r.SourceSystem) == "" {
		return fmt.Errorf("source_system is required")
	}
	if strings.TrimSpace(r.TargetService) == "" {
		return fmt.Errorf("target_service is required")
	}
	if len(r.Records) == 0 {
		return fmt.Errorf("records cannot be empty")
	}
	if len(r.Records) > MaxRecordsPerBatch {
		return fmt.Errorf("records exceeds the maximum of %d per batch", MaxRecordsPerBatch)
	}
	if strings.TrimSpace(r.ExceptionOwnerID) == "" {
		return fmt.Errorf("exception_owner_id is required")
	}
	return r.validateControlTotals()
}

func (r *ValidateMigrationRequest) validateControlTotals() error {
	declared := r.ControlTotals
	if declared == nil {
		return fmt.Errorf("control_totals is required (source record count and amount total from the extract manifest)")
	}
	if declared.RecordCount == nil {
		return fmt.Errorf("control_totals.record_count is required")
	}
	if *declared.RecordCount < 0 {
		return fmt.Errorf("control_totals.record_count must not be negative")
	}
	if !IsDecimal(declared.AmountTotal) {
		return fmt.Errorf("control_totals.amount_total must be a plain decimal string such as \"1250.00\"")
	}
	if decimalPlaces(declared.AmountTotal) > MaxAmountDecimals {
		return fmt.Errorf("control_totals.amount_total must have at most %d decimal places", MaxAmountDecimals)
	}
	if declared.ControlHash != "" && !sha256HexRe.MatchString(declared.ControlHash) {
		return fmt.Errorf("control_totals.control_hash must be a lower-case hex SHA-256 (64 characters)")
	}
	return nil
}

// amountField returns the record field the amount control total sums.
func (d *ControlTotalsDeclaration) amountField() string {
	if d == nil || strings.TrimSpace(d.AmountField) == "" {
		return DefaultControlAmountField
	}
	return strings.TrimSpace(d.AmountField)
}

// ─── POST /v1/migrations/{id}/audit/{entryId}/remediate ──────────────────────

// RemediateRequest is the body of
// POST /v1/migrations/{id}/audit/{entryId}/remediate. A disposition is an
// evidenced decision, not a boolean flip: a reason and a reference to the
// evidence supporting it are both required (MIG-INV-12, MIG-N27).
type RemediateRequest struct {
	// TenantID is optional and must equal X-Tenant-Id when present.
	TenantID string `json:"tenant_id,omitempty"`

	// Disposition is REMEDIATED (default) or WAIVED.
	Disposition EntryStatus `json:"disposition,omitempty"`

	RemediationReason      string `json:"remediation_reason"`
	RemediationEvidenceRef string `json:"remediation_evidence_ref"`

	// WaiverExpiresAt is required for WAIVED and refused otherwise: §16 MQ1
	// allows only a "formally approved, time-limited migration exception".
	WaiverExpiresAt *time.Time `json:"waiver_expires_at,omitempty"`
}

// TargetStatus is the state the entry moves to.
func (r *RemediateRequest) TargetStatus() EntryStatus {
	if r.Disposition == "" {
		return EntryStatusRemediated
	}
	return r.Disposition
}

// Validate enforces the request's shape (400 on failure). A blank or
// whitespace-only reason or evidence reference is missing, not provided.
func (r *RemediateRequest) Validate() error {
	target := r.TargetStatus()
	if target != EntryStatusRemediated && target != EntryStatusWaived {
		return fmt.Errorf("disposition must be REMEDIATED or WAIVED")
	}
	if strings.TrimSpace(r.RemediationReason) == "" {
		return fmt.Errorf("remediation_reason is required")
	}
	if strings.TrimSpace(r.RemediationEvidenceRef) == "" {
		return fmt.Errorf("remediation_evidence_ref is required")
	}
	if target == EntryStatusWaived && r.WaiverExpiresAt == nil {
		return fmt.Errorf("waiver_expires_at is required when disposition is WAIVED (a waiver is time-limited)")
	}
	if target == EntryStatusRemediated && r.WaiverExpiresAt != nil {
		return fmt.Errorf("waiver_expires_at is only allowed when disposition is WAIVED")
	}
	return nil
}
