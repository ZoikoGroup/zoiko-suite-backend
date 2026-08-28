package domain

import (
	"fmt"
	"math"
	"time"
)

type SourceSystem string
type DiscrepancyType string
type ResolutionRecommendation string
type ResolutionStatus string

const (
	SourceGeneralLedger  SourceSystem = "GENERAL_LEDGER"
	SourceBankStatement  SourceSystem = "BANK_STATEMENTS"
	SourceInvoices       SourceSystem = "INVOICES"
	SourcePayrollJournal SourceSystem = "PAYROLL_JOURNAL"
)

const (
	DiscrepancyAmountMismatch   DiscrepancyType = "AMOUNT_MISMATCH"
	DiscrepancyDateSkew         DiscrepancyType = "DATE_SKEW"
	DiscrepancyMissingReference DiscrepancyType = "MISSING_REFERENCE"
	DiscrepancyDuplicateEntry   DiscrepancyType = "DUPLICATE_ENTRY"
)

const (
	RecommendationAutoMatch        ResolutionRecommendation = "AUTO_MATCH"
	RecommendationWriteOff         ResolutionRecommendation = "WRITE_OFF"
	RecommendationTimingAdjustment ResolutionRecommendation = "TIMING_ADJUSTMENT"
	RecommendationManualReview     ResolutionRecommendation = "MANUAL_REVIEW"
)

const (
	StatusRecommended ResolutionStatus = "RECOMMENDED"
	StatusApproved    ResolutionStatus = "APPROVED"
	StatusRejected    ResolutionStatus = "REJECTED"
	StatusExecuted    ResolutionStatus = "EXECUTED"
)

type TransactionItem struct {
	RefID     string  `json:"ref_id"`
	Amount    float64 `json:"amount"`
	Date      string  `json:"date"`
	Narrative string  `json:"narrative,omitempty"`
}

type ReconciliationJob struct {
	ID                  string          `json:"id"`
	TenantID            string          `json:"tenant_id"`
	LegalEntityID       string          `json:"legal_entity_id"`
	JobName             string          `json:"job_name"`
	SourceSystemA       SourceSystem    `json:"source_system_a"`
	SourceSystemB       SourceSystem    `json:"source_system_b"`
	TotalProcessedCount int             `json:"total_processed_count"`
	MatchedCount        int             `json:"matched_count"`
	UnmatchedCount      int             `json:"unmatched_count"`
	ReconciliationRate  float64         `json:"reconciliation_rate"` // 0.00 to 100.00%
	Status              string          `json:"status"`              // COMPLETED, ARCHIVED
	AnalyzedAt          time.Time       `json:"analyzed_at"`
	CreatedAt           time.Time       `json:"created_at"`
	UnmatchedItems      []UnmatchedItem `json:"unmatched_items,omitempty"`
}

type UnmatchedItem struct {
	ID                string          `json:"id,omitempty"`
	TenantID          string          `json:"tenant_id"`
	JobID             string          `json:"job_id"`
	TransactionRefA   string          `json:"transaction_ref_a"`
	TransactionRefB   string          `json:"transaction_ref_b,omitempty"`
	AmountA           float64         `json:"amount_a"`
	AmountB           float64         `json:"amount_b"`
	DiscrepancyAmount float64         `json:"discrepancy_amount"`
	DiscrepancyType   DiscrepancyType `json:"discrepancy_type"`
	// ConfidenceScore is NOT statistically or ML-derived — it is one of three
	// fixed values produced by PerformIntelligentReconciliation's hardcoded
	// threshold rules (see the named heuristic constants below). ZS-SVC-Z-001
	// (Data Quality/Reconciliation/Lineage/Control Totals) requires tolerance
	// be "explicit and versioned" (its own INV-09); this remains an
	// unversioned Go-code heuristic, not that. Kept as a documented,
	// human-reviewed starting point — ResolutionNotes below carries the
	// actual rule that fired, so a reviewer sees the reasoning rather than
	// trusting this number as if it were a real confidence estimate.
	ConfidenceScore  float64                  `json:"confidence_score"`
	Recommendation   ResolutionRecommendation `json:"recommendation"`
	ResolutionStatus ResolutionStatus         `json:"resolution_status"`
	ResolutionNotes  string                   `json:"resolution_notes,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
	UpdatedAt        time.Time                `json:"updated_at"`
}

type AnalyzeReconciliationRequest struct {
	LegalEntityID string            `json:"legal_entity_id"`
	JobName       string            `json:"job_name"`
	SourceSystemA SourceSystem      `json:"source_system_a"`
	SourceSystemB SourceSystem      `json:"source_system_b"`
	TransactionsA []TransactionItem `json:"transactions_a"`
	TransactionsB []TransactionItem `json:"transactions_b"`
}

type ApplyResolutionRequest struct {
	ResolutionStatus ResolutionStatus `json:"resolution_status"` // APPROVED, REJECTED, EXECUTED
	ResolutionNotes  string           `json:"resolution_notes"`
}

func (r *AnalyzeReconciliationRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.JobName == "" {
		return fmt.Errorf("job_name is required")
	}
	if r.SourceSystemA == "" || r.SourceSystemB == "" {
		return fmt.Errorf("both source_system_a and source_system_b are required")
	}
	if len(r.TransactionsA) == 0 && len(r.TransactionsB) == 0 {
		return fmt.Errorf("transaction batches cannot be empty")
	}
	return nil
}

// Heuristic thresholds and scores for PerformIntelligentReconciliation.
//
// These were previously bare numeric literals inline in the function body
// (50.0, 10.0, 85.0, 60.0, 90.0), with no name, no comment, and no visible
// connection between "this threshold" and "that confidence score." Named and
// documented here instead — not because naming them makes the rule
// governed or versioned (it does not; see the ConfidenceScore field comment
// above and ZS-SVC-Z-001 INV-09), but because a rule a reviewer cannot see
// is a rule they cannot audit or question. A future real tolerance-policy
// service replaces this block, not just its literals.
const (
	// amountMismatchWriteOffTolerance: a matched pair (same reference in
	// both systems) whose amounts differ by less than this is recommended
	// for write-off; at or above it, a timing adjustment is recommended
	// instead. Arbitrary and unversioned — not a tenant- or entity-specific
	// materiality policy.
	amountMismatchWriteOffTolerance = 50.0
	amountMismatchConfidence        = 85.0

	// missingReferenceSmallAmountTolerance: an item present in system A with
	// no matching reference in system B is normally flagged for manual
	// review; below this amount it is instead recommended for write-off
	// outright, on the reasoning that the review cost exceeds the amount at
	// risk. Also arbitrary and unversioned.
	missingReferenceSmallAmountTolerance   = 10.0
	missingReferenceWriteOffConfidence     = 90.0
	missingReferenceManualReviewConfidence = 60.0
)

// PerformIntelligentReconciliation executes matching analysis, discrepancy
// detection, & resolution recommendations.
//
// "Intelligent" here means two fixed threshold comparisons, not a model or a
// governed rule — see the heuristic constants above. Every item this
// produces carries ResolutionStatus RECOMMENDED, never anything further:
// domain.StatusApproved/Rejected/Executed are only ever reached through
// Handler.ApplyResolution, which requires an authenticated principal to hold
// RECONCILIATION_APPLY_RESOLUTION. Nothing in this function writes anything
// off; it proposes, and a human decides. That human-approval gate is what
// keeps a heuristic acceptable as a starting point here — it would not be
// if this function's output executed anything itself.
func PerformIntelligentReconciliation(req *AnalyzeReconciliationRequest, jobID, tenantID string) (int, int, float64, []UnmatchedItem) {
	mapB := make(map[string]TransactionItem)
	for _, txB := range req.TransactionsB {
		mapB[txB.RefID] = txB
	}

	matchedCount := 0
	var unmatchedItems []UnmatchedItem
	totalProcessed := len(req.TransactionsA)

	for _, txA := range req.TransactionsA {
		txB, exists := mapB[txA.RefID]
		if exists {
			// Reference matches! Check amounts and dates
			discAmount := math.Round(math.Abs(txA.Amount-txB.Amount)*100) / 100
			if discAmount == 0.0 {
				// Exact Match
				matchedCount++
			} else {
				// Amount Mismatch
				confScore := amountMismatchConfidence
				var rec ResolutionRecommendation
				var rationale string
				if discAmount < amountMismatchWriteOffTolerance {
					rec = RecommendationWriteOff
					rationale = fmt.Sprintf(
						"heuristic rule: discrepancy $%.2f is below the $%.2f write-off tolerance (unversioned platform default, not a governed policy)",
						discAmount, amountMismatchWriteOffTolerance)
				} else {
					rec = RecommendationTimingAdjustment
					rationale = fmt.Sprintf(
						"heuristic rule: discrepancy $%.2f is at or above the $%.2f write-off tolerance (unversioned platform default, not a governed policy)",
						discAmount, amountMismatchWriteOffTolerance)
				}

				unmatchedItems = append(unmatchedItems, UnmatchedItem{
					TenantID:          tenantID,
					JobID:             jobID,
					TransactionRefA:   txA.RefID,
					TransactionRefB:   txB.RefID,
					AmountA:           txA.Amount,
					AmountB:           txB.Amount,
					DiscrepancyAmount: discAmount,
					DiscrepancyType:   DiscrepancyAmountMismatch,
					ConfidenceScore:   confScore,
					Recommendation:    rec,
					ResolutionStatus:  StatusRecommended,
					ResolutionNotes:   rationale,
					CreatedAt:         time.Now(),
					UpdatedAt:         time.Now(),
				})
			}
		} else {
			// Missing Reference in System B
			confScore := missingReferenceManualReviewConfidence
			rec := RecommendationManualReview
			var rationale string
			if txA.Amount < missingReferenceSmallAmountTolerance {
				rec = RecommendationWriteOff
				confScore = missingReferenceWriteOffConfidence
				rationale = fmt.Sprintf(
					"heuristic rule: amount $%.2f is below the $%.2f small-amount write-off tolerance (unversioned platform default, not a governed policy)",
					txA.Amount, missingReferenceSmallAmountTolerance)
			} else {
				rationale = fmt.Sprintf(
					"heuristic rule: no matching reference in system B; amount $%.2f is at or above the $%.2f small-amount tolerance, so manual review is recommended rather than automatic write-off",
					txA.Amount, missingReferenceSmallAmountTolerance)
			}

			unmatchedItems = append(unmatchedItems, UnmatchedItem{
				TenantID:          tenantID,
				JobID:             jobID,
				TransactionRefA:   txA.RefID,
				AmountA:           txA.Amount,
				AmountB:           0.0,
				DiscrepancyAmount: txA.Amount,
				DiscrepancyType:   DiscrepancyMissingReference,
				ConfidenceScore:   confScore,
				Recommendation:    rec,
				ResolutionStatus:  StatusRecommended,
				ResolutionNotes:   rationale,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			})
		}
	}

	unmatchedCount := len(unmatchedItems)
	recRate := 0.0
	if totalProcessed > 0 {
		recRate = math.Round((float64(matchedCount)/float64(totalProcessed))*10000) / 100
	}

	return matchedCount, unmatchedCount, recRate, unmatchedItems
}
