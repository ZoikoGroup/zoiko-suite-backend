package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ── command request bodies ───────────────────────────────────────────────────

// All command bodies carry expected_version (optional unless the handler says
// otherwise) and are decoded strictly (unknown fields refused).

type AmendInvoiceDraftRequest struct {
	ExpectedVersion *int `json:"expected_version,omitempty"`

	// Vendor, invoice number, currency and document type are the invoice's
	// identity and cannot be amended; everything else is re-stated in full.
	Amount        float64                        `json:"amount"`
	InvoiceDate   CalendarDate                   `json:"invoice_date"`
	SupplyDate    CalendarDate                   `json:"supply_date"`
	DueDate       CalendarDate                   `json:"due_date"`
	Lines         []CreateVendorInvoiceLineInput `json:"lines"`
	PurchaseOrderID   *string                    `json:"purchase_order_id,omitempty"`
	GoodsReceiptRef   *string                    `json:"goods_receipt_ref,omitempty"`
	InvoiceDocumentID *string                    `json:"invoice_document_id,omitempty"`
	AttachmentHash    *string                    `json:"attachment_hash,omitempty"`
	ExtractedBankDetails       *BankDetails      `json:"extracted_bank_details,omitempty"`
	WithholdingDeterminationID *string           `json:"withholding_determination_id,omitempty"`
	Reason        string                         `json:"reason"`
}

type ReasonRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Reason          string `json:"reason"`
}

type ApproveRequest struct {
	ExpectedVersion *int   `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
}

type ResolveQuarantineRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	Outcome         string `json:"outcome"` // RELEASE | REJECT
	Reason          string `json:"reason"`
}

type HoldRequest struct {
	ExpectedVersion *int   `json:"expected_version,omitempty"`
	HoldKind        string `json:"hold_kind"` // HELD | DISPUTED
	Reason          string `json:"reason"`
}

type CreditDocumentRequest struct {
	DocumentType      string                         `json:"document_type"` // CREDIT_NOTE | DEBIT_NOTE
	InvoiceNumber     string                         `json:"invoice_number"`
	Amount            float64                        `json:"amount"`
	CurrencyCode      string                         `json:"currency_code"`
	InvoiceDate       CalendarDate                   `json:"invoice_date"`
	SupplyDate        CalendarDate                   `json:"supply_date"`
	DueDate           CalendarDate                   `json:"due_date"`
	Lines             []CreateVendorInvoiceLineInput `json:"lines"`
	CorrelationID     string                         `json:"correlation_id"`
	InvoiceDocumentID *string                        `json:"invoice_document_id,omitempty"`
	AttachmentHash    *string                        `json:"attachment_hash,omitempty"`
	Reason            string                         `json:"reason"`
}

type LinkCorrectionRequest struct {
	LinkedInvoiceID string `json:"linked_invoice_id"`
	LinkKind        string `json:"link_kind"` // CREDIT | DEBIT | REVERSAL | REPLACEMENT
	Reason          string `json:"reason"`
}

// ── persistence / outbox contracts ───────────────────────────────────────────

// OutboxEvent is one domain event to be written to the outbox in the same
// transaction as the state change.
type OutboxEvent struct {
	EventType string
	Payload   any
}

// HistoryEntry is one append-only invoice_history row.
type HistoryEntry struct {
	HistoryID     int64           `json:"history_id,omitempty"`
	InvoiceID     string          `json:"invoice_id"`
	Version       int             `json:"version"`
	Command       string          `json:"command"`
	ActorID       string          `json:"actor_id"`
	Reason        string          `json:"reason,omitempty"`
	FromState     json.RawMessage `json:"from_state,omitempty"`
	ToState       json.RawMessage `json:"to_state,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
}

// CorrectionLink is an append-only link between an original document and a
// credit/debit/reversal/replacement document.
type CorrectionLink struct {
	LinkID            string    `json:"link_id"`
	OriginalInvoiceID string    `json:"original_invoice_id"`
	LinkedInvoiceID   string    `json:"linked_invoice_id"`
	LinkKind          string    `json:"link_kind"`
	Reason            string    `json:"reason"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
}

// PayableCreationPayload is the frozen AP-08 request. It deliberately carries
// NO bank data: AP-08 resolves the payee destination through ORG-10.
type PayableCreationPayload struct {
	LegalEntityID  string    `json:"legal_entity_id"`
	SourceType     string    `json:"source_type"`
	SourceReference string   `json:"source_reference"`
	PayeeRef       string    `json:"payee_ref"`
	OriginalAmount float64   `json:"original_amount"`
	Currency       string    `json:"currency"`
	DueDate        time.Time `json:"due_date"`
}

// PayableRequest is one row of the durable payable-creation queue.
type PayableRequest struct {
	RequestID       string                 `json:"request_id"`
	TenantID        string                 `json:"tenant_id"`
	LegalEntityID   string                 `json:"legal_entity_id"`
	InvoiceID       string                 `json:"invoice_id"`
	SourceReference string                 `json:"source_reference"`
	Payload         PayableCreationPayload `json:"payload"`
	PrincipalID     string                 `json:"principal_id"`
	CorrelationID   string                 `json:"correlation_id"`
	Status          string                 `json:"status"`
	Attempts        int                    `json:"attempts"`
	NextAttemptAt   time.Time              `json:"next_attempt_at"`
	LastError       *string                `json:"last_error,omitempty"`
	PayableID       *string                `json:"payable_id,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
}

const (
	PayableStatusPending    = "PENDING"
	PayableStatusInProgress = "IN_PROGRESS"
	PayableStatusCreated    = "CREATED"
	PayableStatusBlocked    = "BLOCKED"
	PayableStatusDead       = "DEAD"
)

// Mutation is what a command returns to the store alongside the in-place
// changes it made to the invoice. The store persists the invoice, the history
// row, the outbox events and the optional side records in ONE transaction.
type Mutation struct {
	Command       string
	Actor         string
	Reason        string
	CorrelationID string
	Detail        map[string]any

	Events []OutboxEvent

	// Assessment, when set, is persisted (append-only) with the change.
	Assessment *DuplicateAssessment
	// ReplaceLines, when non-nil, replaces the invoice's lines (draft amend only;
	// the lines trigger refuses it once the invoice is accepted).
	ReplaceLines *[]VendorInvoiceLine
	// NewSourcePayload, when non-nil, replaces the stored canonical payload.
	NewSourcePayload json.RawMessage
	// PayableCreation, when set, writes the durable AP-08 queue row in the same
	// transaction as the approval.
	PayableCreation *PayableCreationPayload
	PayablePrincipal string
	// AccountingPosting, when set, writes the durable ACC-04 posting-queue row in
	// the same transaction (approval / credit acceptance).
	AccountingPosting *AccountingPostingPayload
	// CorrectionLink, when set, appends a correction link and a history row on
	// the linked document.
	CorrectionLink *CorrectionLink
	// KeepVersion suppresses the version bump for bookkeeping-only changes.
	KeepVersion bool
}

// ── errors added by AP-05 v2 ─────────────────────────────────────────────────

var (
	ErrStaleVersion        = errorString("expected_version does not match the invoice's current version")
	ErrExpectedVersionRequired = errorString("expected_version is required for this transition")
	ErrIdempotencyKeyRequired  = errorString("Idempotency-Key header is required for this command")
	ErrIdempotencyKeyReused    = errorString("Idempotency-Key was already used with a different request")
	ErrIdempotencyInProgress   = errorString("a request with this Idempotency-Key is still being processed")

	ErrSupplierNotEligible     = errorString("supplier is not eligible: no active, un-held AP-01 financial profile for this legal entity")
	ErrSupplierEligibilityUnavailable = errorString("supplier eligibility could not be verified — supplier-financial-profile-svc unavailable")
	ErrPurchaseOrderNotIssued  = errorString("purchase order is not ISSUED and cannot be invoiced against")

	ErrTaxUnavailable          = errorString("TAX determination is missing or could not be verified")
	ErrTaxResultChanged        = errorString("the TAX/withholding result changed after it was verified")
	ErrPayeeUnverifiable       = errorString("ORG-10 active payee destination could not be checked — payee-banking-identity-svc unavailable")

	ErrQuarantineNotResolved   = errorString("invoice is quarantined")
	ErrInvoiceImmutable        = errorString("accepted supplier invoice source representation is immutable; use a linked credit/debit document")
	ErrDraftOnly               = errorString("only a draft (not yet accepted) invoice can be amended")
	ErrReasonRequired          = errorString("reason is required")
	ErrCreditExceedsOriginal   = errorString("credit documents would exceed the original invoice amount")
	ErrIdempotencyBodyTooLarge = errorString("request body too large to fingerprint")
)

// MutateFn is a command's in-place change to a locked invoice. It may return a
// domain error to abort (the transaction rolls back).
type MutateFn func(inv *VendorInvoice) (*Mutation, error)

// IdemRecord is a stored command outcome keyed by Idempotency-Key.
type IdemRecord struct {
	Operation   string
	RequestHash string
	StatusCode  *int
	Response    []byte
	CreatedAt   time.Time
}

// AccountingPostingLine / AccountingPostingPayload are the frozen body of
// general-ledger-svc POST /v1/postings/events (ACC-04 PostAccountingEvent).
// Lines carry mapping keys, never account codes: ACC-02 resolves them.
type AccountingPostingLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

type AccountingPostingPayload struct {
	LegalEntityID       string                  `json:"legal_entity_id"`
	FiscalPeriod        string                  `json:"fiscal_period"`
	Description         string                  `json:"description"`
	SourceEventID       string                  `json:"source_event_id"`
	CorrelationID       string                  `json:"correlation_id"`
	TransactionCurrency string                  `json:"transaction_currency"`
	DocumentDate        string                  `json:"document_date"`
	Lines               []AccountingPostingLine `json:"lines"`
}

// AccountingPostingRequest is one row of accounting_posting_requests.
type AccountingPostingRequest struct {
	RequestID     string                   `json:"request_id"`
	TenantID      string                   `json:"tenant_id"`
	LegalEntityID string                   `json:"legal_entity_id"`
	InvoiceID     string                   `json:"invoice_id"`
	SourceEventID string                   `json:"source_event_id"`
	Payload       AccountingPostingPayload `json:"request_payload"`
	PrincipalID   string                   `json:"principal_id"`
	CorrelationID string                   `json:"correlation_id"`
	Status        string                   `json:"status"`
	Attempts      int                      `json:"attempts"`
	LastError     *string                  `json:"last_error,omitempty"`
	PostingExecutionID *string             `json:"posting_execution_id,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
	CompletedAt   *time.Time               `json:"completed_at,omitempty"`
}

const (
	PostingStatusPending     = "PENDING"
	PostingStatusPosted      = "POSTED"
	PostingStatusFailed      = "FAILED"
	PostingStatusQuarantined = "QUARANTINED"
)

// BuildAccountingPosting derives the ACC-04 lines for an approved supplier
// document. Mapping keys are configurable; AP never names GL accounts.
// Invoice/debit note: Dr expense (net) [+ Dr input tax], Cr payable control (gross).
// Credit note: the mirror image.
func BuildAccountingPosting(inv *VendorInvoice, correlationID string, keys PostingMappingKeys) AccountingPostingPayload {
	debitSide := inv.DocumentType != DocCreditNote
	line := func(key string, amt float64, desc string, debit bool) AccountingPostingLine {
		l := AccountingPostingLine{MappingKey: key, Description: desc}
		if debit {
			l.DebitAmount = amt
		} else {
			l.CreditAmount = amt
		}
		return l
	}
	p := AccountingPostingPayload{
		LegalEntityID: inv.LegalEntityID, FiscalPeriod: inv.InvoiceDate.Time.UTC().Format("2006-01"),
		Description:   fmt.Sprintf("Supplier %s %s", inv.DocumentType, inv.InvoiceNumber),
		SourceEventID: inv.InvoiceID, CorrelationID: correlationID,
		TransactionCurrency: inv.CurrencyCode, DocumentDate: inv.InvoiceDate.Time.UTC().Format("2006-01-02"),
	}
	// Net is derived as gross minus tax so the posting always balances, including for a
	// pre-contract invoice that carries only a gross amount.
	net := math.Round((inv.Amount-inv.TaxAmount)*100) / 100
	p.Lines = append(p.Lines, line(keys.Expense, net, "net", debitSide))
	if inv.TaxAmount > 0 {
		p.Lines = append(p.Lines, line(keys.TaxInput, inv.TaxAmount, "tax", debitSide))
	}
	p.Lines = append(p.Lines, line(keys.PayableControl, inv.Amount, "gross payable", !debitSide))
	return p
}

// PostingMappingKeys are the ACC-02 mapping keys AP posts against.
type PostingMappingKeys struct {
	Expense, TaxInput, PayableControl string
}

func DefaultPostingMappingKeys() PostingMappingKeys {
	return PostingMappingKeys{Expense: "AP_EXPENSE", TaxInput: "AP_TAX_INPUT", PayableControl: "AP_PAYABLE_CONTROL"}
}
