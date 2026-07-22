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
	SourceGeneralLedger SourceSystem = "GENERAL_LEDGER"
	SourceBankStatement SourceSystem = "BANK_STATEMENTS"
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
	RefID     string    `json:"ref_id"`
	Amount    float64   `json:"amount"`
	Date      string    `json:"date"`
	Narrative string    `json:"narrative,omitempty"`
}

type ReconciliationJob struct {
	ID                  string           `json:"id"`
	TenantID            string           `json:"tenant_id"`
	LegalEntityID       string           `json:"legal_entity_id"`
	JobName             string           `json:"job_name"`
	SourceSystemA       SourceSystem     `json:"source_system_a"`
	SourceSystemB       SourceSystem     `json:"source_system_b"`
	TotalProcessedCount int              `json:"total_processed_count"`
	MatchedCount        int              `json:"matched_count"`
	UnmatchedCount      int              `json:"unmatched_count"`
	ReconciliationRate  float64          `json:"reconciliation_rate"` // 0.00 to 100.00%
	Status              string           `json:"status"`              // COMPLETED, ARCHIVED
	AnalyzedAt          time.Time        `json:"analyzed_at"`
	CreatedAt           time.Time        `json:"created_at"`
	UnmatchedItems      []UnmatchedItem  `json:"unmatched_items,omitempty"`
}

type UnmatchedItem struct {
	ID                string                   `json:"id,omitempty"`
	TenantID          string                   `json:"tenant_id"`
	JobID             string                   `json:"job_id"`
	TransactionRefA   string                   `json:"transaction_ref_a"`
	TransactionRefB   string                   `json:"transaction_ref_b,omitempty"`
	AmountA           float64                  `json:"amount_a"`
	AmountB           float64                  `json:"amount_b"`
	DiscrepancyAmount float64                  `json:"discrepancy_amount"`
	DiscrepancyType   DiscrepancyType          `json:"discrepancy_type"`
	ConfidenceScore   float64                  `json:"confidence_score"`
	Recommendation    ResolutionRecommendation `json:"recommendation"`
	ResolutionStatus  ResolutionStatus         `json:"resolution_status"`
	ResolutionNotes   string                   `json:"resolution_notes,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
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

// PerformIntelligentReconciliation executes matching analysis, discrepancy detection, & resolution recommendations
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
				confScore := 85.0
				var rec ResolutionRecommendation
				if discAmount < 50.0 {
					rec = RecommendationWriteOff
				} else {
					rec = RecommendationTimingAdjustment
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
					CreatedAt:         time.Now(),
					UpdatedAt:         time.Now(),
				})
			}
		} else {
			// Missing Reference in System B
			confScore := 60.0
			rec := RecommendationManualReview
			if txA.Amount < 10.0 {
				rec = RecommendationWriteOff
				confScore = 90.0
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
