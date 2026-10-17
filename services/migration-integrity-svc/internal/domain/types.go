package domain

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// ─── Enums ───────────────────────────────────────────────────────────────────

type CheckType string
type Severity string
type ViolationType string
type JobStatus string

const (
	CheckSchemaValidation   CheckType = "SCHEMA_VALIDATION"
	CheckReferentialInteg   CheckType = "REFERENTIAL_INTEGRITY"
	CheckDuplicateDetection CheckType = "DUPLICATE_DETECTION"
	CheckRangeCheck         CheckType = "RANGE_CHECK"
	CheckFormatCheck        CheckType = "FORMAT_CHECK"
)

const (
	SeverityInfo     Severity = "INFO"
	SeverityWarning  Severity = "WARNING"
	SeverityCritical Severity = "CRITICAL"
)

const (
	ViolationMissingRequired ViolationType = "MISSING_REQUIRED"
	ViolationTypeMismatch    ViolationType = "TYPE_MISMATCH"
	ViolationDuplicate       ViolationType = "DUPLICATE"
	ViolationOutOfRange      ViolationType = "OUT_OF_RANGE"
	ViolationOrphanedRef     ViolationType = "ORPHANED_REFERENCE"
)

const (
	JobStatusPending    JobStatus = "PENDING"
	JobStatusValidating JobStatus = "VALIDATING"
	JobStatusCompleted  JobStatus = "COMPLETED"
	JobStatusFailed     JobStatus = "FAILED"
	JobStatusArchived   JobStatus = "ARCHIVED"
)

// ─── Domain Models ────────────────────────────────────────────────────────────

type MigrationRecord struct {
	Ref    string            `json:"ref"`
	Fields map[string]string `json:"fields"`
}

type MigrationJob struct {
	ID                  string           `json:"id"`
	TenantID            string           `json:"tenant_id"`
	LegalEntityID       string           `json:"legal_entity_id"`
	MigrationName       string           `json:"migration_name"`
	SourceSystem        string           `json:"source_system"`
	TargetService       string           `json:"target_service"`
	TotalRecordsCount   int              `json:"total_records_count"`
	ValidRecordsCount   int              `json:"valid_records_count"`
	InvalidRecordsCount int              `json:"invalid_records_count"`
	IntegrityScore      float64          `json:"integrity_score"`
	Status              JobStatus        `json:"status"`
	IntegrityChecks     []IntegrityCheck `json:"integrity_checks,omitempty"`
	AuditEntries        []AuditEntry     `json:"audit_entries,omitempty"`
	StartedAt           *time.Time       `json:"started_at,omitempty"`
	CompletedAt         *time.Time       `json:"completed_at,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}

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

type AuditEntry struct {
	ID            string        `json:"id,omitempty"`
	TenantID      string        `json:"tenant_id"`
	JobID         string        `json:"job_id"`
	RecordRef     string        `json:"record_ref"`
	FieldName     string        `json:"field_name,omitempty"`
	SourceValue   string        `json:"source_value,omitempty"`
	TargetValue   string        `json:"target_value,omitempty"`
	ViolationType ViolationType `json:"violation_type"`
	IsRemediated  bool          `json:"is_remediated"`

	// Remediation evidence (ZS-MIG-001 MIG-INV-12: owner, reason code and
	// disposition). Recorded on the first remediation only; a replayed
	// remediate request returns the original decision unchanged.
	RemediationReason      string     `json:"remediation_reason,omitempty"`
	RemediationEvidenceRef string     `json:"remediation_evidence_ref,omitempty"`
	RemediatedBy           string     `json:"remediated_by,omitempty"`
	RemediatedAt           *time.Time `json:"remediated_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// ─── Request DTOs ─────────────────────────────────────────────────────────────

type ValidateMigrationRequest struct {
	LegalEntityID  string            `json:"legal_entity_id"`
	MigrationName  string            `json:"migration_name"`
	SourceSystem   string            `json:"source_system"`
	TargetService  string            `json:"target_service"`
	RequiredFields []string          `json:"required_fields"`
	Records        []MigrationRecord `json:"records"`
}

// RemediateRequest is the body of
// POST /v1/migrations/{id}/audit/{entryId}/remediate. Remediation is an
// evidenced decision, not a boolean flip: a reason and a reference to the
// evidence supporting it are both required (MIG-INV-12, MIG-N27).
type RemediateRequest struct {
	RemediationReason string `json:"remediation_reason"`

	RemediationEvidenceRef string `json:"remediation_evidence_ref"`
}

// ─── Validation ───────────────────────────────────────────────────────────────

func (r *ValidateMigrationRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.MigrationName == "" {
		return fmt.Errorf("migration_name is required")
	}
	if r.SourceSystem == "" {
		return fmt.Errorf("source_system is required")
	}
	if r.TargetService == "" {
		return fmt.Errorf("target_service is required")
	}
	if len(r.Records) == 0 {
		return fmt.Errorf("records cannot be empty")
	}
	return nil
}

// Validate enforces that a remediation carries its evidence: a reason
// (the disposition) and a reference to what supports it. Both are required;
// a blank or whitespace-only value is missing, not provided.
func (r *RemediateRequest) Validate() error {
	if strings.TrimSpace(r.RemediationReason) == "" {
		return fmt.Errorf("remediation_reason is required")
	}
	if strings.TrimSpace(r.RemediationEvidenceRef) == "" {
		return fmt.Errorf("remediation_evidence_ref is required")
	}
	return nil
}

// ─── Integrity Engine ─────────────────────────────────────────────────────────

// PerformIntegrityValidation runs schema, duplicate, and format integrity checks
// on the provided migration records. Returns checks, audit entries, valid count,
// invalid count, and an integrity score (0–100%).
func PerformIntegrityValidation(req *ValidateMigrationRequest, jobID, tenantID string) ([]IntegrityCheck, []AuditEntry, int, int, float64) {
	now := time.Now()
	var checks []IntegrityCheck
	var auditEntries []AuditEntry
	totalRecords := len(req.Records)
	// Violations are counted per RECORD (by position), never per distinct ref:
	// several records sharing one ref are each a violation, so counting refs
	// would overstate the valid count and the integrity score.
	violated := make(map[int]bool)

	// ── Check 1: Schema Validation (required fields) ──────────────────────────
	schemaFailed := 0
	for i, rec := range req.Records {
		hasMissing := false
		for _, field := range req.RequiredFields {
			val, exists := rec.Fields[field]
			if !exists || strings.TrimSpace(val) == "" {
				hasMissing = true
				violated[i] = true
				auditEntries = append(auditEntries, AuditEntry{
					TenantID:      tenantID,
					JobID:         jobID,
					RecordRef:     rec.Ref,
					FieldName:     field,
					SourceValue:   "",
					ViolationType: ViolationMissingRequired,
					IsRemediated:  false,
					CreatedAt:     now,
				})
			}
		}
		if hasMissing {
			schemaFailed++
		}
	}
	checks = append(checks, IntegrityCheck{
		TenantID:       tenantID,
		JobID:          jobID,
		CheckName:      "Required Fields Presence",
		CheckType:      CheckSchemaValidation,
		RecordsChecked: totalRecords,
		RecordsPassed:  totalRecords - schemaFailed,
		RecordsFailed:  schemaFailed,
		Severity:       sev(schemaFailed, SeverityCritical),
		Detail:         fmt.Sprintf("Validated %d required fields across %d records", len(req.RequiredFields), totalRecords),
		CreatedAt:      now,
	})

	// ── Check 2: Duplicate Detection ──────────────────────────────────────────
	seen := make(map[string]int)
	for _, rec := range req.Records {
		seen[rec.Ref]++
	}
	// Every record of a duplicated ref fails: which copy is authoritative is
	// unknowable here, so none is silently preferred (ZS-MIG-001 MIG-N05).
	dupFailed := 0
	for i, rec := range req.Records {
		if seen[rec.Ref] > 1 {
			dupFailed++
			violated[i] = true
		}
	}
	for ref, count := range seen {
		if count > 1 {
			auditEntries = append(auditEntries, AuditEntry{
				TenantID:      tenantID,
				JobID:         jobID,
				RecordRef:     ref,
				SourceValue:   fmt.Sprintf("appears %d times", count),
				ViolationType: ViolationDuplicate,
				IsRemediated:  false,
				CreatedAt:     now,
			})
		}
	}
	checks = append(checks, IntegrityCheck{
		TenantID:       tenantID,
		JobID:          jobID,
		CheckName:      "Duplicate Record Detection",
		CheckType:      CheckDuplicateDetection,
		RecordsChecked: totalRecords,
		RecordsPassed:  totalRecords - dupFailed,
		RecordsFailed:  dupFailed,
		Severity:       sev(dupFailed, SeverityWarning),
		Detail:         fmt.Sprintf("Scanned %d records for duplicate reference IDs", totalRecords),
		CreatedAt:      now,
	})

	// ── Check 3: Numeric Format Check ─────────────────────────────────────────
	numericFields := []string{"amount", "salary", "balance", "quantity"}
	fmtFailed := 0
	fmtFailedRecs := make(map[int]bool)
	for i, rec := range req.Records {
		for _, field := range numericFields {
			val, exists := rec.Fields[field]
			// A blank value is a presence question, answered by the required-fields check.
			if !exists || strings.TrimSpace(val) == "" {
				continue
			}
			if !decimalRe.MatchString(strings.TrimSpace(val)) {
				if !fmtFailedRecs[i] {
					fmtFailed++
					fmtFailedRecs[i] = true
				}
				violated[i] = true
				auditEntries = append(auditEntries, AuditEntry{
					TenantID:      tenantID,
					JobID:         jobID,
					RecordRef:     rec.Ref,
					FieldName:     field,
					SourceValue:   val,
					ViolationType: ViolationTypeMismatch,
					IsRemediated:  false,
					CreatedAt:     now,
				})
			}
		}
	}
	checks = append(checks, IntegrityCheck{
		TenantID:       tenantID,
		JobID:          jobID,
		CheckName:      "Numeric Field Format Check",
		CheckType:      CheckFormatCheck,
		RecordsChecked: totalRecords,
		RecordsPassed:  totalRecords - fmtFailed,
		RecordsFailed:  fmtFailed,
		Severity:       sev(fmtFailed, SeverityWarning),
		Detail:         "Validated numeric formats for amount, salary, balance, quantity fields",
		CreatedAt:      now,
	})

	// ── Compute final score ───────────────────────────────────────────────────
	invalidCount := len(violated)
	validCount := totalRecords - invalidCount
	if validCount < 0 {
		validCount = 0
	}

	score := 100.0
	if totalRecords > 0 {
		score = math.Round((float64(validCount)/float64(totalRecords))*10000) / 100
	}

	return checks, auditEntries, validCount, invalidCount, score
}

// decimalRe is a plain signed decimal: "5", "-5", "5.25", "5.", ".5". It refuses
// "1.2.3", "-", ".", "1-2" and "--", which a per-character scan let through.
var decimalRe = regexp.MustCompile(`^-?([0-9]+\.?[0-9]*|\.[0-9]+)$`)

func sev(failCount int, whenFailed Severity) Severity {
	if failCount > 0 {
		return whenFailed
	}
	return SeverityInfo
}
