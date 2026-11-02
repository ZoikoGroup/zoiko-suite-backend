package domain

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

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
				entry := newQuarantineEntry(tenantID, jobID, owner, now)
				entry.RecordRef = rec.Ref
				entry.FieldName = field
				entry.ViolationType = ViolationMissingRequired
				entry.Severity = SeverityMQ2
				entry.ReasonFamily = ReasonSchema
				auditEntries = append(auditEntries, entry)
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
			entry := newQuarantineEntry(tenantID, jobID, owner, now)
			entry.RecordRef = rec.Ref
			entry.ViolationType = ViolationMissingSourceIdentity
			entry.Severity = SeverityMQ0
			entry.ReasonFamily = ReasonIdentity
			auditEntries = append(auditEntries, entry)
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
	// One entry per duplicated ref, in sorted ref order: iterating the map
	// directly would make the audit trail's order differ run to run for the
	// same input, and the engine is meant to be deterministic.
	for _, ref := range sortedKeys(seen) {
		if count := seen[ref]; count > 1 {
			entry := newQuarantineEntry(tenantID, jobID, owner, now)
			entry.RecordRef = ref
			entry.SourceValue = fmt.Sprintf("appears %d times", count)
			entry.ViolationType = ViolationDuplicate
			entry.Severity = SeverityMQ1
			entry.ReasonFamily = ReasonIdentity
			auditEntries = append(auditEntries, entry)
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
			if !IsDecimal(val) {
				if !fmtFailedRecs[i] {
					fmtFailed++
					fmtFailedRecs[i] = true
				}
				violated[i] = true
				entry := newQuarantineEntry(tenantID, jobID, owner, now)
				entry.RecordRef = rec.Ref
				entry.FieldName = field
				entry.SourceValue = val
				entry.ViolationType = ViolationTypeMismatch
				entry.Severity = SeverityMQ1
				entry.ReasonFamily = ReasonSchema
				auditEntries = append(auditEntries, entry)
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

func sev(failCount int, whenFailed Severity) Severity {
	if failCount > 0 {
		return whenFailed
	}
	return SeverityMQ4
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ─── Whole-batch validation ──────────────────────────────────────────────────

// ValidationOutcome is everything one validation run decides.
type ValidationOutcome struct {
	Checks        []IntegrityCheck
	Entries       []AuditEntry
	ControlTotals ControlTotals
	ValidCount    int
	InvalidCount  int
	Score         float64
	BlockerCount  int
	Status        JobStatus
}

// ValidateBatch runs the full, deterministic validation of one batch:
//
//  1. record-level checks (required fields, source identity, duplicates,
//     numeric format) -> per-record validity and the integrity score;
//  2. batch-level source-to-target control totals -> MQ0 on any mismatch;
//  3. acceptance: FAILED on any MQ0, on an all-invalid batch, or on a score
//     below the threshold; COMPLETED otherwise.
//
// The same request and threshold always produce the same counts, score,
// entries (in the same order), control hash and status.
func ValidateBatch(req *ValidateMigrationRequest, jobID, tenantID string, threshold float64) ValidationOutcome {
	checks, entries, validCount, invalidCount, score := PerformIntegrityValidation(req, jobID, tenantID)

	totals, controlCheck, controlEntries := ReconcileControlTotals(req, jobID, tenantID)
	checks = append(checks, controlCheck)
	entries = append(entries, controlEntries...)

	blockers := CountBlockers(entries)
	status := EvaluateJobStatus(score, validCount, invalidCount, blockers, threshold)

	return ValidationOutcome{
		Checks:        checks,
		Entries:       entries,
		ControlTotals: totals,
		ValidCount:    validCount,
		InvalidCount:  invalidCount,
		Score:         score,
		BlockerCount:  blockers,
		Status:        status,
	}
}
