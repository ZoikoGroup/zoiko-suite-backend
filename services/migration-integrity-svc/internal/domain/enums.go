package domain

// ─── Enums ───────────────────────────────────────────────────────────────────
//
// Every enum here is persisted as text and guarded by a CHECK constraint in
// deployments/migrations, so the database refuses a value the code would
// never produce.

// CheckType names one integrity check run against a migration batch.
type CheckType string

const (
	CheckSchemaValidation   CheckType = "SCHEMA_VALIDATION"
	CheckReferentialInteg   CheckType = "REFERENTIAL_INTEGRITY" // declared, not yet produced (SPEC_DEVIATIONS A7)
	CheckSourceIdentity     CheckType = "SOURCE_IDENTITY"
	CheckDuplicateDetection CheckType = "DUPLICATE_DETECTION"
	CheckRangeCheck         CheckType = "RANGE_CHECK" // declared, not yet produced (SPEC_DEVIATIONS A7)
	CheckFormatCheck        CheckType = "FORMAT_CHECK"
	CheckControlTotals      CheckType = "CONTROL_TOTALS"
)

// Severity is the ZS-MIG-001 §16 quarantine severity. Each class carries a
// required disposition, not just a label; what this service does about each
// one is stated per value (SPEC_DEVIATIONS A6, B3).
type Severity string

const (
	// SeverityMQ0 — Blocker: threatens tenant isolation, ledger balance, legal
	// entity, regulatory filing, financial completeness or cutover integrity.
	// Disposition: stop the affected batch/cutover; executive/domain owner
	// resolution required. Here: any MQ0 entry makes the job FAILED whatever
	// its score (EvaluateJobStatus), only the entry's named owner may resolve
	// it, and it can never be waived.
	SeverityMQ0 Severity = "MQ0"
	// SeverityMQ1 — Material: financial/regulatory/business effect above
	// approved materiality. Disposition: must resolve or receive a formally
	// approved, time-limited migration exception. Here: REMEDIATED, or WAIVED
	// with a mandatory future waiver_expires_at.
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
type ReasonFamily string

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

// ViolationType is what was wrong with one record (or, for the CONTROL_*
// types, with the batch as a whole against its source manifest).
type ViolationType string

const (
	ViolationMissingRequired       ViolationType = "MISSING_REQUIRED"
	ViolationTypeMismatch          ViolationType = "TYPE_MISMATCH"
	ViolationDuplicate             ViolationType = "DUPLICATE"
	ViolationOutOfRange            ViolationType = "OUT_OF_RANGE"       // declared, not yet produced
	ViolationOrphanedRef           ViolationType = "ORPHANED_REFERENCE" // declared, not yet produced
	ViolationMissingSourceIdentity ViolationType = "MISSING_SOURCE_IDENTITY"
	ViolationControlCountMismatch  ViolationType = "CONTROL_COUNT_MISMATCH"
	ViolationControlAmountMismatch ViolationType = "CONTROL_AMOUNT_MISMATCH"
	ViolationControlHashMismatch   ViolationType = "CONTROL_HASH_MISMATCH"
)

// JobStatus is the validation run's lifecycle state.
//
//	(validate) ──► COMPLETED ──┐
//	           └─► FAILED    ──┴─► ARCHIVED   (DELETE; terminal, soft delete)
//
// PENDING and VALIDATING are reserved for an asynchronous validator; the
// current validator is synchronous, so a run is born decided.
type JobStatus string

const (
	JobStatusPending    JobStatus = "PENDING"
	JobStatusValidating JobStatus = "VALIDATING"
	JobStatusCompleted  JobStatus = "COMPLETED"
	JobStatusFailed     JobStatus = "FAILED"
	JobStatusArchived   JobStatus = "ARCHIVED"
)

// IsValidJobStatus reports whether s names a JobStatus (used to validate the
// list filter, so a typo is a 400 rather than an empty page).
func IsValidJobStatus(s string) bool {
	switch JobStatus(s) {
	case JobStatusPending, JobStatusValidating, JobStatusCompleted, JobStatusFailed, JobStatusArchived:
		return true
	}
	return false
}

// EntryStatus is a quarantine item's disposition state.
//
//	QUARANTINED ──► REMEDIATED   (fixed; evidence required)
//	            └─► WAIVED       (time-limited exception; not for MQ0)
//
// REMEDIATED and WAIVED are terminal: a decision is never rewritten.
type EntryStatus string

const (
	EntryStatusQuarantined EntryStatus = "QUARANTINED"
	EntryStatusRemediated  EntryStatus = "REMEDIATED"
	EntryStatusWaived      EntryStatus = "WAIVED"
)

// IsDecided reports whether the entry has left QUARANTINED.
func (s EntryStatus) IsDecided() bool { return s != EntryStatusQuarantined }
