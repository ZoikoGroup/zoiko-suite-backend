package domain

import "time"

// ─── Domain Models ────────────────────────────────────────────────────────────

// MigrationRecord is one candidate record of a migration batch: its source
// identity (ref) and its field values as strings.
type MigrationRecord struct {
	Ref    string            `json:"ref"`
	Fields map[string]string `json:"fields"`
}

// MigrationJob is one validation run.
//
// IntegrityScoreThreshold is the acceptance bar the run was actually judged
// against (the config value at validation time), stored per job so a
// PASS/FAIL decision remains auditable after the configured default changes
// (SPEC_DEVIATIONS A11). BlockerCount is the number of MQ0 audit entries the
// run produced; any blocker makes the job FAILED regardless of score (A6).
//
// Version starts at 1 and increases on every state change of the job or of
// one of its quarantine items; it is the ETag clients send back in If-Match.
type MigrationJob struct {
	ID                      string           `json:"id"`
	TenantID                string           `json:"tenant_id"`
	LegalEntityID           string           `json:"legal_entity_id"`
	MigrationName           string           `json:"migration_name"`
	SourceSystem            string           `json:"source_system"`
	TargetService           string           `json:"target_service"`
	RequestedBy             string           `json:"requested_by,omitempty"`
	TotalRecordsCount       int              `json:"total_records_count"`
	ValidRecordsCount       int              `json:"valid_records_count"`
	InvalidRecordsCount     int              `json:"invalid_records_count"`
	IntegrityScore          float64          `json:"integrity_score"`
	IntegrityScoreThreshold float64          `json:"integrity_score_threshold"`
	BlockerCount            int              `json:"blocker_count"`
	OpenQuarantineCount     int              `json:"open_quarantine_count"`
	Status                  JobStatus        `json:"status"`
	Version                 int              `json:"version"`
	ControlTotals           *ControlTotals   `json:"control_totals,omitempty"`
	IntegrityChecks         []IntegrityCheck `json:"integrity_checks,omitempty"`
	AuditEntries            []AuditEntry     `json:"audit_entries,omitempty"`
	StartedAt               *time.Time       `json:"started_at,omitempty"`
	CompletedAt             *time.Time       `json:"completed_at,omitempty"`
	ArchivedAt              *time.Time       `json:"archived_at,omitempty"`
	ArchivedBy              string           `json:"archived_by,omitempty"`
	CreatedAt               time.Time        `json:"created_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
}

// FindEntry returns the job's audit entry with the given id, or nil.
func (j *MigrationJob) FindEntry(entryID string) *AuditEntry {
	for i := range j.AuditEntries {
		if j.AuditEntries[i].ID == entryID {
			return &j.AuditEntries[i]
		}
	}
	return nil
}

// ControlTotals is the source-to-target reconciliation of one batch
// (ZS-MIG-001 §17 completeness and accuracy, MIG-INV-15): what the source
// manifest declared beside what the service computed from the submitted
// records. Amounts are decimal strings, never floats.
type ControlTotals struct {
	AmountField       string `json:"amount_field"`
	SourceRecordCount int    `json:"source_record_count"`
	TargetRecordCount int    `json:"target_record_count"`
	SourceAmountTotal string `json:"source_amount_total"`
	TargetAmountTotal string `json:"target_amount_total"`
	SourceControlHash string `json:"source_control_hash,omitempty"`
	TargetControlHash string `json:"target_control_hash"`
	Matched           bool   `json:"matched"`
}

// IntegrityCheck is the outcome of one check over the whole batch.
type IntegrityCheck struct {
	ID             string    `json:"id,omitempty"`
	TenantID       string    `json:"tenant_id"`
	JobID          string    `json:"job_id"`
	CheckName      string    `json:"check_name"`
	CheckType      CheckType `json:"check_type"`
	RecordsChecked int       `json:"records_checked"`
	RecordsPassed  int       `json:"records_passed"`
	RecordsFailed  int       `json:"records_failed"`
	Severity       Severity  `json:"severity"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"created_at"`
}

// AuditEntry is one quarantine item in a job's audit trail.
//
// SourceValue is the single offending field value -- the evidence remediation
// acts on (MIG-INV-12), never a record dump. It is kept raw rather than
// masked: masking the one value a remediator must act on would leave the read
// API empty-handed. The boundary that keeps the raw value from being an
// exposure is the per-legal-entity read authorization (SPEC_DEVIATIONS
// A9/A10); the list read carries no audit entries at all, and events carry
// no source values.
type AuditEntry struct {
	ID            string        `json:"id,omitempty"`
	TenantID      string        `json:"tenant_id"`
	JobID         string        `json:"job_id"`
	RecordRef     string        `json:"record_ref"`
	FieldName     string        `json:"field_name,omitempty"`
	SourceValue   string        `json:"source_value,omitempty"`
	TargetValue   string        `json:"target_value,omitempty"`
	ViolationType ViolationType `json:"violation_type"`

	// Status is the disposition state; IsRemediated is kept for existing
	// readers and is true only when Status is REMEDIATED.
	Status       EntryStatus `json:"status"`
	IsRemediated bool        `json:"is_remediated"`

	// §16 classification and ownership (MIG-INV-12 "owner, reason code and
	// disposition"; MIG-C37 "all MQ0–MQ2 items named owner").
	Severity     Severity     `json:"severity"`
	ReasonFamily ReasonFamily `json:"reason_family"`
	OwnerID      string       `json:"owner_id,omitempty"`

	// Disposition evidence. Recorded once, on the transition out of
	// QUARANTINED; the database refuses any later change.
	RemediationReason      string     `json:"remediation_reason,omitempty"`
	RemediationEvidenceRef string     `json:"remediation_evidence_ref,omitempty"`
	RemediatedBy           string     `json:"remediated_by,omitempty"`
	RemediatedAt           *time.Time `json:"remediated_at,omitempty"`
	WaiverExpiresAt        *time.Time `json:"waiver_expires_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// newQuarantineEntry builds an open quarantine item; every check raises its
// violations through it so no entry is born without status or owner.
func newQuarantineEntry(tenantID, jobID, owner string, now time.Time) AuditEntry {
	return AuditEntry{
		TenantID:  tenantID,
		JobID:     jobID,
		OwnerID:   owner,
		Status:    EntryStatusQuarantined,
		CreatedAt: now,
	}
}
