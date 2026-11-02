package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// controlTotalsRecordRef is the record_ref of a batch-level quarantine item:
// a control-total mismatch concerns the whole batch, not one record.
const controlTotalsRecordRef = "CONTROL_TOTALS"

// ReconcileControlTotals compares the source manifest's declared control
// totals with the totals computed from the submitted records (ZS-MIG-001
// §17: "source count/hash vs target + quarantined + explicitly excluded =
// 100% accounted for"; accuracy: "financial totals ... comparisons").
//
// Three comparisons, each an MQ0 blocker when it fails, because each one
// means the batch cannot be shown to be the source population:
//
//   - record count  differs → CONTROL_COUNT_MISMATCH,  family SOURCE    (MIG-N01)
//   - amount total  differs → CONTROL_AMOUNT_MISMATCH, family FINANCIAL (MIG-N28)
//   - content hash  differs → CONTROL_HASH_MISMATCH,   family SOURCE    (MIG-N29;
//     only when the manifest declares a hash)
//
// The computed amount total sums only values that parse as decimals; an
// unparseable amount is already its own MQ1 (format check) and, by being
// left out, also shows up here as an amount mismatch — a value that cannot
// be loaded cannot be counted as reconciled.
func ReconcileControlTotals(req *ValidateMigrationRequest, jobID, tenantID string) (ControlTotals, IntegrityCheck, []AuditEntry) {
	now := time.Now()
	owner := strings.TrimSpace(req.ExceptionOwnerID)
	declared := req.ControlTotals
	amountField := declared.amountField()

	// Step 1: compute the target-side totals from the submitted records.
	targetCount := len(req.Records)
	targetAmount := sumAmounts(req.Records, amountField)
	targetHash := ComputeControlHash(req.Records, amountField)

	// Step 2: read the source-side declaration (already validated).
	sourceCount := *declared.RecordCount
	sourceAmount, _ := ParseDecimal(declared.AmountTotal)
	sourceHash := declared.ControlHash

	// Step 3: compare and raise one MQ0 per failed comparison.
	var entries []AuditEntry
	if sourceCount != targetCount {
		entries = append(entries, controlMismatchEntry(tenantID, jobID, owner, now,
			ViolationControlCountMismatch, ReasonSource, "record_count",
			fmt.Sprintf("%d", sourceCount), fmt.Sprintf("%d", targetCount)))
	}
	if sourceAmount.Cmp(targetAmount) != 0 {
		entries = append(entries, controlMismatchEntry(tenantID, jobID, owner, now,
			ViolationControlAmountMismatch, ReasonFinancial, amountField,
			FormatDecimal(sourceAmount), FormatDecimal(targetAmount)))
	}
	if sourceHash != "" && sourceHash != targetHash {
		entries = append(entries, controlMismatchEntry(tenantID, jobID, owner, now,
			ViolationControlHashMismatch, ReasonSource, "control_hash", sourceHash, targetHash))
	}

	totals := ControlTotals{
		AmountField:       amountField,
		SourceRecordCount: sourceCount,
		TargetRecordCount: targetCount,
		SourceAmountTotal: FormatDecimal(sourceAmount),
		TargetAmountTotal: FormatDecimal(targetAmount),
		SourceControlHash: sourceHash,
		TargetControlHash: targetHash,
		Matched:           len(entries) == 0,
	}

	// The comparison is all-or-nothing for the batch: when it fails, no
	// record can be shown to belong to the reconciled population.
	recordsPassed, recordsFailed := targetCount, 0
	if !totals.Matched {
		recordsPassed, recordsFailed = 0, targetCount
	}
	check := IntegrityCheck{
		TenantID:       tenantID,
		JobID:          jobID,
		CheckName:      "Source-to-Target Control Totals",
		CheckType:      CheckControlTotals,
		RecordsChecked: targetCount,
		RecordsPassed:  recordsPassed,
		RecordsFailed:  recordsFailed,
		Severity:       sev(len(entries), SeverityMQ0),
		Detail: fmt.Sprintf("count %d/%d, %s total %s/%s (source/target), %d comparison(s) failed",
			sourceCount, targetCount, amountField, totals.SourceAmountTotal, totals.TargetAmountTotal, len(entries)),
		CreatedAt: now,
	}
	return totals, check, entries
}

// ComputeControlHash is the deterministic content hash of a batch: SHA-256
// over one line per record, "<ref>\x1f<amount>\n", with the lines sorted so
// the hash does not depend on record order. The amount is the canonical
// FormatDecimal rendering ("5" and "5.00" hash alike); an unparseable amount
// is hashed as "!<raw>" and an absent one as an empty string. A source system
// computing the same hash over its extract lets the service detect records
// that were swapped or misclassified while the totals still agree (MIG-N29).
func ComputeControlHash(records []MigrationRecord, amountField string) string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, record.Ref+"\x1f"+canonicalAmount(record.Fields[amountField])+"\n")
	}
	sort.Strings(lines)

	hasher := sha256.New()
	for _, line := range lines {
		hasher.Write([]byte(line))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func canonicalAmount(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	value, ok := ParseDecimal(raw)
	if !ok {
		return "!" + raw
	}
	return FormatDecimal(value)
}

func sumAmounts(records []MigrationRecord, amountField string) *big.Rat {
	total := new(big.Rat)
	for _, record := range records {
		value, ok := ParseDecimal(record.Fields[amountField])
		if ok {
			total.Add(total, value)
		}
	}
	return total
}

func controlMismatchEntry(tenantID, jobID, owner string, now time.Time, violation ViolationType, family ReasonFamily, field, declared, computed string) AuditEntry {
	entry := newQuarantineEntry(tenantID, jobID, owner, now)
	entry.RecordRef = controlTotalsRecordRef
	entry.FieldName = field
	entry.SourceValue = declared
	entry.TargetValue = computed
	entry.ViolationType = violation
	entry.Severity = SeverityMQ0
	entry.ReasonFamily = family
	return entry
}
