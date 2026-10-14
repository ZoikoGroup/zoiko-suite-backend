package kit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cross-module ports.
//
// The AR modules share one process and one database, but each owns its tables
// (spec §3 authority matrix). A module never reads or writes another module's
// tables: it calls the owner's port. Ports that must change state take the
// caller's pgx.Tx so the change is atomic with the caller's own command (an
// issued invoice and its open item commit together or not at all).
//
// The owner implements the port; main.go wires the implementations.

// ── AR-01: customer financial profile ────────────────────────────────────────

// ProfileView is the effective financial profile of a customer at a date.
type ProfileView struct {
	ProfileID        string
	CustomerPartyID  string
	LegalEntityID    string
	Version          int
	PaymentTermsCode string
	PaymentTermsDays int
	CreditLimit      Amount // zero = no limit configured
	CreditCurrency   string
	CreditHold       bool
	DunningProfile   string
	EffectiveFrom    time.Time
}

// ProfileResolver resolves the single effective, ACTIVE profile at a date.
// Errors (all *Problem): CUSTOMER_PROFILE_INACTIVE when there is none, it is
// future/expired/suspended/retired; VALIDATION_FAILED when overlapping effective
// versions make the answer ambiguous. There is deliberately no fallback to a
// stale profile for material issuance (AR-01 failure semantics).
type ProfileResolver interface {
	ActiveAt(ctx context.Context, tenantID, legalEntityID, customerPartyID string, at time.Time) (*ProfileView, error)
}

// ── AR-07: receivable open items ─────────────────────────────────────────────

// Open-item document kinds and adjustment kinds.
const (
	DocInvoice    = "INVOICE"
	DocDebitNote  = "DEBIT_NOTE"
	DocCreditNote = "CREDIT_NOTE"

	AdjCashApplication = "CASH_APPLICATION"
	AdjCashUnapply     = "CASH_UNAPPLICATION"
	AdjCreditNote      = "CREDIT_NOTE"
	AdjDebitNote       = "DEBIT_NOTE"
	AdjRefund          = "REFUND"
	AdjWriteOff        = "WRITE_OFF"
	AdjCorrection      = "CORRECTION"
)

// OpenItemFromDocument creates the receivable consequence of an issued
// document. Idempotent on (tenant, SourceDocumentID).
type OpenItemFromDocument struct {
	DocumentKind     string // DocInvoice | DocDebitNote | DocCreditNote
	SourceDocumentID string
	DocumentNumber   string
	CustomerPartyID  string
	Currency         string
	OriginalAmount   Amount // gross, positive for invoice/debit note; credit notes are applied as adjustments to their source open item
	IssueDate        time.Time
	DueDate          time.Time
	SourceInvoiceID  string // for notes: the invoice they correct
}

// OpenItemRef identifies the created open item.
type OpenItemRef struct {
	OpenItemID string
	Version    int
}

// OpenItemAdjustment changes a residual. Spec invariant 6/7: a balance changes
// only from an authoritative event, and every adjustment names its owning
// document or event — there is no free-form edit.
type OpenItemAdjustment struct {
	OpenItemID    string
	Kind          string // Adj*
	Amount        Amount // positive magnitude; direction is implied by Kind
	SourceRefType string // e.g. CASH_APPLICATION, CREDIT_NOTE, WRITE_OFF
	SourceRefID   string
	EventKey      string // idempotency key for this adjustment; replays are no-ops
	Reason        string
}

// OpenItemView is the read model other modules need.
type OpenItemView struct {
	OpenItemID       string
	DocumentKind     string
	SourceDocumentID string
	CustomerPartyID  string
	LegalEntityID    string
	Currency         string
	OriginalAmount   Amount
	Residual         Amount
	DueDate          time.Time
	State            string // CREATED|OPEN|PARTIALLY_SETTLED|SETTLED|WRITTEN_OFF|CORRECTED
	Disputed         bool
	CollectionHold   bool
	Version          int
}

// OpenItems is implemented by AR-07 (internal/ar07).
type OpenItems interface {
	// CreateFromDocument and ApplyAdjustment run inside the caller's tx.
	// ApplyAdjustment returns CodeReceivableResidualInvalid when the residual
	// would become negative without an authorized credit.
	CreateFromDocument(ctx context.Context, tx pgx.Tx, c Caller, in OpenItemFromDocument) (OpenItemRef, error)
	ApplyAdjustment(ctx context.Context, tx pgx.Tx, c Caller, in OpenItemAdjustment) (*OpenItemView, error)

	// Reads (own transaction).
	GetByID(ctx context.Context, tenantID, openItemID string) (*OpenItemView, error)
	GetBySourceDocument(ctx context.Context, tenantID, sourceDocumentID string) (*OpenItemView, error)
	// Exposure is the customer's total open residual in a currency — the figure
	// credit checks compare to the credit limit.
	Exposure(ctx context.Context, tenantID, legalEntityID, customerPartyID, currency string) (Amount, error)
	// CollectionBlocked reports whether the customer has an active dispute or
	// collection hold on any open item (AR-09 contact suppression).
	CollectionBlocked(ctx context.Context, tenantID, legalEntityID, customerPartyID string) (bool, error)
}

// ── AR-03: sales orders ──────────────────────────────────────────────────────

// BillingBasis is an approved basis for an invoice: a billable milestone, a
// delivered quantity, or a direct amount on a confirmed order.
type BillingBasis struct {
	OrderID       string
	MilestoneID   string
	CustomerParty string
	Currency      string
	EligibleNet   Amount
	Version       int
}

// Orders is implemented by AR-03 (internal/ar03).
type Orders interface {
	// InvoiceEligibility returns the billable basis, or ORDER_NOT_BILLABLE
	// (cancelled/held/unconfirmed order, unknown or already-billed milestone,
	// credit hold) as a *Problem.
	InvoiceEligibility(ctx context.Context, tenantID, orderID, milestoneID string) (*BillingBasis, error)
	// RecordInvoiced marks a milestone/amount as invoiced inside the invoice
	// issue transaction, so remaining-to-bill moves atomically with issuance.
	RecordInvoiced(ctx context.Context, tx pgx.Tx, c Caller, orderID, milestoneID, invoiceID string, net Amount) error
}

// ── AR-04: issued invoices (read by AR-05/06/07/08/09/10) ────────────────────

// IssuedInvoiceLine is one frozen line of an issued invoice.
type IssuedInvoiceLine struct {
	LineNumber  int
	Description string
	Quantity    Amount
	UnitPrice   Amount
	Net         Amount
	Tax         Amount
	TaxCode     string
}

// IssuedInvoice is the immutable issued representation another module needs.
type IssuedInvoice struct {
	InvoiceID          string
	LegalEntityID      string
	CustomerPartyID    string
	InvoiceNumber      string
	Currency           string
	Net                Amount
	Tax                Amount
	Gross              Amount
	IssueDate          time.Time
	DueDate            time.Time
	TaxDeterminationID string
	TaxRulePackVersion string
	ContentHash        string
	Lines              []IssuedInvoiceLine
	// OriginalCorrectable is gross minus everything already credited, plus debits.
	// AR-04 does not compute it (AR-06 does); it is here only as the issued gross.
}

// Invoices is implemented by AR-04 (internal/ar04).
type Invoices interface {
	// GetIssued returns the invoice only if it is ISSUED (or later). A draft,
	// unapproved or cancelled invoice returns NOT_FOUND so no downstream module
	// can act on a document that was never authoritative.
	GetIssued(ctx context.Context, tenantID, invoiceID string) (*IssuedInvoice, error)
	// LinkCorrection records a correction document against the invoice inside
	// the caller's tx (InvoiceCorrectionLinked). It never edits issued content.
	LinkCorrection(ctx context.Context, tx pgx.Tx, c Caller, invoiceID, correctionID, kind string) error
	// SetEInvoiceState mirrors the AR-05 submission state onto the invoice's
	// e-invoice dimension (NOT_REQUIRED|REQUIRED|PREPARED|SUBMITTED|PENDING_UNKNOWN|ACCEPTED|REJECTED).
	// A rejection updates only that dimension — never the issued invoice.
	SetEInvoiceState(ctx context.Context, tx pgx.Tx, c Caller, invoiceID, state string) error
	// SetSettlementState mirrors AR-07 residual onto the settlement dimension
	// (UNPAID|PARTIALLY_PAID|PAID|WRITTEN_OFF|CREDITED).
	SetSettlementState(ctx context.Context, tx pgx.Tx, c Caller, invoiceID, state string) error
	// SetDisputeState mirrors an AR-07 dispute (NONE|DISPUTED).
	SetDisputeState(ctx context.Context, tx pgx.Tx, c Caller, invoiceID, state string) error
}

// ── AR-05 hook consumed by AR-04/AR-06 ───────────────────────────────────────

// EInvoiceRequirement tells an issuing module whether an e-invoice submission
// must follow. Implemented by AR-05; resolved from the jurisdiction pack.
type EInvoiceRequirement interface {
	Required(ctx context.Context, tenantID, legalEntityID, jurisdictionID, documentType string) (bool, error)
}
