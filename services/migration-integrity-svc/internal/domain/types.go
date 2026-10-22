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
type ReasonFamily string
type ViolationType string
type JobStatus string

const (
	CheckSchemaValidation   CheckType = "SCHEMA_VALIDATION"
	CheckReferentialInteg   CheckType = "REFERENTIAL_INTEGRITY"
	CheckSourceIdentity     CheckType = "SOURCE_IDENTITY"
	CheckDuplicateDetection CheckType = "DUPLICATE_DETECTION"
	CheckRangeCheck         CheckType = "RANGE_CHECK"
	CheckFormatCheck        CheckType = "FORMAT_CHECK"
)

// Severity is the ZS-MIG-001 §16 quarantine severity. Each class carries a
// required disposition, not just a label; what this service does about each
// one is stated per value (SPEC_DEVIATIONS A6, B3).
const (
	// SeverityMQ0 — Blocker: threatens tenant isolation, ledger balance, legal
	// entity, regulatory filing, financial completeness or cutover integrity.
	// Disposition: stop the affected batch/cutover; executive/domain owner
	// resolution required. Here: any MQ0 entry makes the job FAILED whatever
	// its score (EvaluateJobStatus), and only the entry's named owner may
	// resolve it.
	SeverityMQ0 Severity = "MQ0"
	// SeverityMQ1 — Material: financial/regulatory/business effect above
	// approved materiality. Disposition: must resolve or receive a formally
	// approved, time-limited migration exception. Here: owner required; the
	// exception object and the certificate it gates do not exist (A1–A3).
	SeverityMQ1 Severity = "MQ1"
	// SeverityMQ2 — Significant: operational impact, not financially
	// material. Disposition: owned remediation; may proceed only if the
	// acceptance criteria permit. Here: the record counts as invalid and the
	// integrity-score threshold (A11) is the acceptance criterion.
	SeverityMQ2 Severity = "MQ2"
	// SeverityMQ3 — Minor: non-material presentation/reference defect.
	// Disposition: documented remediation/backlog.
	SeverityMQ3 Severity = "MQ3"
	// SeverityMQ4 — Informational: quality observation with no current
	// effect. Disposition: track/monitor. Also the severity of a check that
	// found nothing.
	SeverityMQ4 Severity = "MQ4"
)

// Blocks reports whether the severity's disposition stops the run: only an
// MQ0 does. Everything below it is decided by the acceptance threshold.
func (s Severity) Blocks() bool { return s == SeverityMQ0 }

// ReasonFamily is the ZS-MIG-001 §16 reason family. It is a separate
// dimension from ViolationType: the same violation can belong to different
// families depending on the field it concerns (a type mismatch on a date is
// TEMPORAL, on a currency REFERENCE), so the family is assigned where the
// violation is raised, never derived from the violation type.
const (
	ReasonIdentity  ReasonFamily = "IDENTITY"  // duplicate, collision, missing canonical relationship
	ReasonReference ReasonFamily = "REFERENCE" // unknown account, tax code, currency, dimension, entity
	ReasonTemporal  ReasonFamily = "TEMPORAL"  // invalid date, effective-date conflict, cutoff outside window
	ReasonFinancial ReasonFamily = "FINANCIAL" // unbalanced journal, control mismatch, currency inconsistency
	ReasonSubledger ReasonFamily = "SUBLEDGER" // open-item amount/aging/allocation inconsistency
	ReasonTax       ReasonFamily = "TAX"       // registration/rule/return relationship unresolved
	ReasonDocument  ReasonFamily = "DOCUMENT"  // missing/corrupt/unlinked/unsafe file
	ReasonSchema    ReasonFamily = "SCHEMA"    // unexpected type/field/version
	ReasonSecurity  ReasonFamily = "SECURITY"  // residency/classification/access violation
	ReasonSource    ReasonFamily = "SOURCE"    // source population incomplete or extract integrity failure
)

const (
	ViolationMissingRequired       ViolationType = "MISSING_REQUIRED"
	ViolationTypeMismatch          ViolationType = "TYPE_MISMATCH"
	ViolationDuplicate             ViolationType = "DUPLICATE"
	ViolationOutOfRange            ViolationType = "OUT_OF_RANGE"
	ViolationOrphanedRef           ViolationType = "ORPHANED_REFERENCE"
	ViolationMissingSourceIdentity ViolationType = "MISSING_SOURCE_IDENTITY"
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

// MigrationJob is one validation run. IntegrityScoreThreshold is the
// acceptance bar the run was actually judged against (the config value at
// validation time), stored per job so a PASS/FAIL decision remains
// auditable after the configured default changes (SPEC_DEVIATIONS A11).
// BlockerCount is the number of MQ0 audit entries the run produced; any
// blocker makes the job FAILED regardless of score (A6).
type MigrationJob struct {
	ID                      string           `json:"id"`
	TenantID                string           `json:"tenant_id"`
	LegalEntityID           string           `json:"legal_entity_id"`
	MigrationName           string           `json:"migration_name"`
	SourceSystem            string           `json:"source_system"`
	TargetService           string           `json:"target_service"`
	TotalRecordsCount       int              `json:"total_records_count"`
	ValidRecordsCount       int              `json:"valid_records_count"`
	InvalidRecordsCount     int              `json:"invalid_records_count"`
	IntegrityScore          float64          `json:"integrity_score"`
	IntegrityScoreThreshold float64          `json:"integrity_score_threshold"`
	BlockerCount            int              `json:"blocker_count"`
	Status                  JobStatus        `json:"status"`
	IntegrityChecks         []IntegrityCheck `json:"integrity_checks,omitempty"`
	AuditEntries            []AuditEntry     `json:"audit_entries,omitempty"`
	StartedAt               *time.Time       `json:"started_at,omitempty"`
	CompletedAt             *time.Time       `json:"completed_at,omitempty"`
	CreatedAt               time.Time        `json:"created_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
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

// AuditEntry is one violation in a job's audit trail. SourceValue is the
// single offending field value -- the evidence remediation acts on
// (MIG-INV-12), never a record dump. It is kept raw rather than masked:
// masking the one value a remediator must act on would leave the read API
// empty-handed, with no narrower path to the evidence. The boundary that
// keeps the raw value from being an exposure is the per-legal-entity read
// authorization (SPEC_DEVIATIONS A9/A10); the list read carries no audit
// entries at all.
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

	// §16 classification and ownership (MIG-INV-12 "owner, reason code and
	// disposition"; MIG-C37 "all MQ0–MQ2 items named owner"). The owner is
	// the exception owner named on the validation request.
	Severity     Severity     `json:"severity"`
	ReasonFamily ReasonFamily `json:"reason_family"`
	OwnerID      string       `json:"owner_id,omitempty"`

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

	// ExceptionOwnerID is the principal accountable for resolving the
	// run's quarantine items. Required: every audit entry carries an owner
	// (MIG-INV-12), and an MQ0 can only be resolved by its owner.
	ExceptionOwnerID string `json:"exception_owner_id"`
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
	if strings.TrimSpace(r.ExceptionOwnerID) == "" {
		return fmt.Errorf("exception_owner_id is required")
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

// PerformIntegrityValidation runs schema, source identity, duplicate, and
// format integrity checks on the provided migration records. Returns checks,
// audit entries, valid count, invalid count, and an integrity score (0–100%).
//
// Every audit entry is classified where it is raised (§16 severity and
// reason family) and owned by the request's exception owner. The
// classification rule (SPEC_DEVIATIONS B3): a defect that defeats the
// completeness accounting is MQ0; a defect on a value the service knows is
// financial, or an identity collision, is MQ1 because no materiality figure
// exists here to show it is immaterial; a defect on a caller-declared field
// of unknown meaning is MQ2, left to the acceptance threshold.
func PerformIntegrityValidation(req *ValidateMigrationRequest, jobID, tenantID string) ([]IntegrityCheck, []AuditEntry, int, int, float64) {
	now := time.Now()
	owner := strings.TrimSpace(req.ExceptionOwnerID)
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
					Severity:      SeverityMQ2,
					ReasonFamily:  ReasonSchema,
					OwnerID:       owner,
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
		Severity:       sev(schemaFailed, SeverityMQ2),
		Detail:         fmt.Sprintf("Validated %d required fields across %d records", len(req.RequiredFields), totalRecords),
		CreatedAt:      now,
	})

	// ── Check 2: Source Identity Presence ─────────────────────────────────────
	// A blank ref is not just another value: the record has no source identity
	// at all (MIG-INV-02 "every object preserves source identity"), which is a
	// different defect from a duplicated one. MQ0: a record with no source
	// identity cannot be accounted for in "loaded + quarantined + excluded =
	// source population" (§17), so it threatens financial completeness and
	// stops the run. Reported here, before duplicate
	// detection, so several blank refs are each reported for what they are
	// rather than only as "duplicates" of one another.
	//
	// refBlank is computed once: the identity check, the duplicate scan and
	// its per-record count all need the same answer, and trimming each ref
	// three times over would be pure repetition.
	refBlank := make([]bool, totalRecords)
	identityFailed := 0
	for i, rec := range req.Records {
		if strings.TrimSpace(rec.Ref) == "" {
			refBlank[i] = true
			identityFailed++
			violated[i] = true
			auditEntries = append(auditEntries, AuditEntry{
				TenantID:      tenantID,
				JobID:         jobID,
				RecordRef:     rec.Ref,
				ViolationType: ViolationMissingSourceIdentity,
				Severity:      SeverityMQ0,
				ReasonFamily:  ReasonIdentity,
				OwnerID:       owner,
				IsRemediated:  false,
				CreatedAt:     now,
			})
		}
	}
	checks = append(checks, IntegrityCheck{
		TenantID:       tenantID,
		JobID:          jobID,
		CheckName:      "Source Identity Presence",
		CheckType:      CheckSourceIdentity,
		RecordsChecked: totalRecords,
		RecordsPassed:  totalRecords - identityFailed,
		RecordsFailed:  identityFailed,
		Severity:       sev(identityFailed, SeverityMQ0),
		Detail:         fmt.Sprintf("Scanned %d records for a source reference", totalRecords),
		CreatedAt:      now,
	})

	// ── Check 3: Duplicate Detection ──────────────────────────────────────────
	// Blank refs are excluded (refBlank, computed once by the identity
	// check above): they were already reported for what they are, and
	// comparing empty strings would report a batch of unidentified records
	// as duplicates of each other instead.
	seen := make(map[string]int)
	for i, rec := range req.Records {
		if refBlank[i] {
			continue
		}
		seen[rec.Ref]++
	}
	// Every record of a duplicated ref fails: which copy is authoritative is
	// unknowable here, so none is silently preferred (ZS-MIG-001 MIG-N05).
	// MQ1: an identity collision risks a duplicate economic effect
	// (MIG-N09/N10) of a size this service cannot measure.
	dupFailed := 0
	for i, rec := range req.Records {
		if refBlank[i] {
			continue
		}
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
				Severity:      SeverityMQ1,
				ReasonFamily:  ReasonIdentity,
				OwnerID:       owner,
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
		Severity:       sev(dupFailed, SeverityMQ1),
		Detail:         fmt.Sprintf("Scanned %d records for duplicate reference IDs", totalRecords),
		CreatedAt:      now,
	})

	// ── Check 4: Numeric Format Check ─────────────────────────────────────────
	// These four fields are amounts and quantities, so an unreadable value is
	// a financial value that cannot load: MQ1, family SCHEMA (unexpected type).
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
					Severity:      SeverityMQ1,
					ReasonFamily:  ReasonSchema,
					OwnerID:       owner,
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
		Severity:       sev(fmtFailed, SeverityMQ1),
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

// CountBlockers returns how many entries carry a blocking severity (MQ0).
func CountBlockers(entries []AuditEntry) int {
	n := 0
	for _, e := range entries {
		if e.Severity.Blocks() {
			n++
		}
	}
	return n
}

// EvaluateJobStatus derives a job's acceptance result from its integrity
// score and the acceptance bar the service applied: below the bar is FAILED,
// at or above is COMPLETED. A run in which every record is invalid is FAILED
// at any bar -- a migration with nothing intact is not an accepted migration,
// whatever the configured threshold says (SPEC_DEVIATIONS A11). A run with
// any MQ0 blocker is FAILED at any score: the blocker's disposition is "stop
// the affected batch", which no score can outweigh (A6).
func EvaluateJobStatus(score float64, validCount, invalidCount, blockerCount int, threshold float64) JobStatus {
	if blockerCount > 0 {
		return JobStatusFailed
	}
	if validCount == 0 && invalidCount > 0 {
		return JobStatusFailed
	}
	if score < threshold {
		return JobStatusFailed
	}
	return JobStatusCompleted
}

// decimalRe is a plain signed decimal: "5", "-5", "5.25", "5.", ".5". It refuses
// "1.2.3", "-", ".", "1-2" and "--", which a per-character scan let through.
var decimalRe = regexp.MustCompile(`^-?([0-9]+\.?[0-9]*|\.[0-9]+)$`)

func sev(failCount int, whenFailed Severity) Severity {
	if failCount > 0 {
		return whenFailed
	}
	return SeverityMQ4
}
