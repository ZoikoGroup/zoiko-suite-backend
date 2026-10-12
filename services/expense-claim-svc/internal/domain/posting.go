package domain

import (
	"math"
	"time"
)

// PostingConfig carries the ACC-02 mapping keys (never account codes) and
// the fiscal-period naming layout used to build the ACC-04 request. All four
// are deployment configuration (see internal/config).
type PostingConfig struct {
	ExpenseKey        string
	PayableKey        string
	TaxRecoverableKey string
	// FiscalPeriodLayout is a Go time layout turned into the GL's fiscal
	// period name from the document date (default "2006-01").
	FiscalPeriodLayout string
}

func (c PostingConfig) withDefaults() PostingConfig {
	if c.ExpenseKey == "" {
		c.ExpenseKey = "AP07_EXPENSE_CLAIM_EXPENSE"
	}
	if c.PayableKey == "" {
		c.PayableKey = "AP07_EXPENSE_CLAIM_PAYABLE"
	}
	if c.TaxRecoverableKey == "" {
		c.TaxRecoverableKey = "AP07_EXPENSE_CLAIM_TAX_RECOVERABLE"
	}
	if c.FiscalPeriodLayout == "" {
		c.FiscalPeriodLayout = "2006-01"
	}
	return c
}

// GLPostingLine / GLPostingRequest mirror general-ledger-svc's
// PostAccountingEventRequest (POST /v1/postings/events, ACC-04).
type GLPostingLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

type GLPostingRequest struct {
	LegalEntityID       string          `json:"legal_entity_id"`
	FiscalPeriod        string          `json:"fiscal_period"`
	Description         string          `json:"description"`
	SourceEventID       string          `json:"source_event_id"`
	CorrelationID       string          `json:"correlation_id"`
	TransactionCurrency string          `json:"transaction_currency"`
	DocumentDate        string          `json:"document_date"`
	Lines               []GLPostingLine `json:"lines"`
}

func cents(v float64) float64 { return math.Round(v*100) / 100 }

// ApprovalSourceEventID is the idempotency identity of an approved claim's
// posting (GL enforces UNIQUE(tenant_id, source_event_id)).
func ApprovalSourceEventID(claimID string) string { return "expense-claim-approved:" + claimID }

// BuildApprovalPosting builds the expense/payable posting for an approved
// claim: Dr expense (net of recoverable tax), Dr recoverable tax (only from
// lines that carry a TAX determination), Cr claimant payable (gross). The
// payable credit equals the AP-08 payable's original amount.
func BuildApprovalPosting(c *ExpenseClaim, lines []ExpenseLine, cfg PostingConfig, correlationID string, at time.Time) GLPostingRequest {
	cfg = cfg.withDefaults()
	var total, taxRec float64
	for _, l := range lines {
		total += l.Amount
		if l.ClaimTaxRecovery && l.TaxDeterminationID != "" {
			taxRec += l.CalculatedTaxAmount
		}
	}
	total, taxRec = cents(total), cents(taxRec)
	if correlationID == "" {
		correlationID = c.ClaimID
	}
	req := GLPostingRequest{
		LegalEntityID: c.LegalEntityID, FiscalPeriod: at.UTC().Format(cfg.FiscalPeriodLayout),
		Description: "Expense claim " + c.ClaimID + " approved", SourceEventID: ApprovalSourceEventID(c.ClaimID),
		CorrelationID: correlationID, TransactionCurrency: c.Currency, DocumentDate: at.UTC().Format("2006-01-02"),
	}
	req.Lines = append(req.Lines, GLPostingLine{MappingKey: cfg.ExpenseKey, DebitAmount: cents(total - taxRec), Description: "expense"})
	if taxRec > 0 {
		req.Lines = append(req.Lines, GLPostingLine{MappingKey: cfg.TaxRecoverableKey, DebitAmount: taxRec, Description: "recoverable tax"})
	}
	req.Lines = append(req.Lines, GLPostingLine{MappingKey: cfg.PayableKey, CreditAmount: total, Description: "claimant payable"})
	return req
}

// Posting statuses (accounting_posting_requests.status).
const (
	PostingPending     = "PENDING"
	PostingPosted      = "POSTED"
	PostingFailed      = "FAILED"
	PostingQuarantined = "QUARANTINED"
)

// PostingRequest is the durable ACC-04 hand-off record, exposed so callers
// see the real accounting status of a claim.
type PostingRequest struct {
	RequestID          string
	TenantID           string
	LegalEntityID      string
	AggregateID        string
	SourceEventID      string
	Payload            []byte `json:"-"`
	Status             string
	Attempts           int
	LastError          string
	PostingExecutionID string
	JournalID          string
	PostedAt           *time.Time
	CreatedAt          time.Time
}

// PostingOutcome is the dispatcher's verdict for one request.
type PostingOutcome struct {
	Status      string // PostingPosted / PostingFailed / PostingQuarantined, or PostingPending to retry
	ExecutionID string
	JournalID   string
	Error       string
	NextAttempt time.Time // for a retry
}
