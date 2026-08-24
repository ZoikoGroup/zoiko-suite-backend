// Package domain defines the authoritative domain types for general-ledger-svc.
//
// Per docs/architecture/03-microservices.md §10.1, this service is the
// authoritative owner of journalized financial postings and ledger state.
// It owns journal headers, journal lines, posting state, fiscal period
// linkage, and account references — it does NOT own a chart of accounts;
// no Chart-of-Accounts service exists yet anywhere in this platform, so
// account_code is a plain caller-supplied string reference, unvalidated,
// same posture as tenant-entity-registry-svc's fiscal_calendar_id (a
// documented, honest v1 gap, not an oversight).
package domain

import "time"

// JournalStatus implements the Tri-Phase Commit States required by the spec:
// Pending -> Validated -> Finalized. REVERSED is a fourth, terminal state
// reached only from FINALIZED, via a brand-new reversing journal — the
// original journal's lines are never edited (critical constraint: "No
// finalized journal may be hard-edited. Corrections occur only through
// reversal or adjustment.").
type JournalStatus string

const (
	JournalStatusPending   JournalStatus = "PENDING"
	JournalStatusValidated JournalStatus = "VALIDATED"
	JournalStatusFinalized JournalStatus = "FINALIZED"
	JournalStatusReversed  JournalStatus = "REVERSED"
)

// ValidJournalTransitions enumerates the only legal status transitions.
// REVERSED is reached only from FINALIZED (see ReverseJournal) and is
// terminal — a reversed journal itself can never be reversed again; the
// reversing journal is a distinct, separate, freestanding journal.
var ValidJournalTransitions = map[JournalStatus][]JournalStatus{
	JournalStatusPending:   {JournalStatusValidated},
	JournalStatusValidated: {JournalStatusFinalized},
	JournalStatusFinalized: {JournalStatusReversed},
	JournalStatusReversed:  {},
}

// JournalType is ACC-03's "journal type" input.
//
// The set is deliberately closed. The type decides how a posting is read by
// every downstream consumer — an accrual is expected to reverse, an opening
// balance must be excluded from period movement, a reclass must net to zero
// against another — so an open string would let a caller invent a category
// nothing downstream knows how to treat.
type JournalType string

const (
	JournalTypeStandard   JournalType = "STANDARD"
	JournalTypeAdjustment JournalType = "ADJUSTMENT"
	JournalTypeAccrual    JournalType = "ACCRUAL"
	JournalTypeReversal   JournalType = "REVERSAL"
	JournalTypeOpening    JournalType = "OPENING"
	JournalTypeClosing    JournalType = "CLOSING"
	JournalTypeReclass    JournalType = "RECLASS"

	// JournalTypeUnspecified marks rows written before migration 000006 added
	// the field. It is never accepted on a new journal — see ValidJournalType —
	// and exists only so historical postings remain readable and identifiable
	// as pre-dating the input contract.
	JournalTypeUnspecified JournalType = "UNSPECIFIED"
)

var acceptedJournalTypes = map[JournalType]bool{
	JournalTypeStandard: true, JournalTypeAdjustment: true, JournalTypeAccrual: true,
	JournalTypeReversal: true, JournalTypeOpening: true, JournalTypeClosing: true,
	JournalTypeReclass: true,
}

// ValidJournalType reports whether t may be supplied on a new journal.
// UNSPECIFIED is excluded on purpose: it is a backfill marker, not a choice.
func ValidJournalType(t JournalType) bool { return acceptedJournalTypes[t] }

// JournalTypes lists the accepted values, for error messages and the console.
func JournalTypes() []JournalType {
	return []JournalType{
		JournalTypeStandard, JournalTypeAdjustment, JournalTypeAccrual,
		JournalTypeReversal, JournalTypeOpening, JournalTypeClosing, JournalTypeReclass,
	}
}

// JournalHeader is one journal entry moving through the Tri-Phase Commit
// lifecycle. Entity-bound (LegalEntityID), never hard-deleted.
type JournalHeader struct {
	JournalID     string        `json:"journal_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	FiscalPeriod  string        `json:"fiscal_period"` // e.g. "2026-07" — no Fiscal Calendar service exists yet, plain string reference
	Status        JournalStatus `json:"status"`

	// ── ACC-03 required business/source inputs (§9.D) ──────────────────────

	JournalType JournalType `json:"journal_type"`

	// TransactionDate is ACC-03's "document/transaction date" — when the
	// underlying business document is dated. PostingDate is when the entry
	// takes effect in the ledger. They are separate fields because they
	// legitimately differ: a supplier invoice dated the 28th, received and
	// posted on the 3rd of the next month, is one document with two dates, and
	// collapsing them would move it into the wrong period in one direction or
	// misstate the document in the other.
	//
	// Dates, not timestamps: a posting belongs to a day in the entity's books,
	// and carrying a time-of-day would invite two postings on the same business
	// day to sort into different periods across a timezone boundary.
	TransactionDate Date `json:"transaction_date"`
	PostingDate     Date `json:"posting_date"`

	// CurrencyCode is the transaction currency of every line in this journal.
	// §6.1: "Currency is part of the value, not display metadata."
	//
	// Validated for shape only (three uppercase letters). REF-02 Currency
	// Registry does not exist, so nothing can say whether the code is one this
	// tenant actually transacts in.
	CurrencyCode string `json:"currency_code"`

	// BookID and ReportingBasis are ACC-03's "book" input and INV-03's basis
	// scope. Nil in practice today: REF-06 Accounting Book / Ledger Basis does
	// not exist, so no service can issue or validate one. Carried rather than
	// required so callers can begin sending it and existing postings are not
	// forced to claim a basis nobody decided.
	BookID         *string `json:"book_id,omitempty"`
	ReportingBasis *string `json:"reporting_basis,omitempty"`

	// EvidenceRefs is ACC-03's "source/evidence" input, taken from the §4
	// envelope's evidence_refs. Empty for a posting that needs no supporting
	// document.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`

	// ──────────────────────────────────────────────────────────────────────

	// ReversalOfJournalID is set only on a reversing journal, pointing back
	// at the FINALIZED journal it reverses. Nil for every ordinary journal.
	ReversalOfJournalID *string `json:"reversal_of_journal_id,omitempty"`

	// SourceEventID and GovernanceDecisionID are Atomic Linking references
	// (doctrine §3.3): the upstream event and/or governance decision that
	// caused this posting, when one exists. Both nil for a manually-entered
	// journal — neither is fabricated when there's nothing real to link to.
	SourceEventID        *string `json:"source_event_id,omitempty"`
	GovernanceDecisionID *string `json:"governance_decision_id,omitempty"`

	Description string `json:"description"`

	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	ValidatedByPrincipalID *string    `json:"validated_by_principal_id,omitempty"`
	PostedByPrincipalID    *string    `json:"posted_by_principal_id,omitempty"`
	ReversedByPrincipalID  *string    `json:"reversed_by_principal_id,omitempty"`
	CorrelationID          string     `json:"correlation_id"`
	CreatedAt              time.Time  `json:"created_at"`
	ValidatedAt            *time.Time `json:"validated_at,omitempty"`
	PostedAt               *time.Time `json:"posted_at,omitempty"`
	ReversedAt             *time.Time `json:"reversed_at,omitempty"`
}

// JournalLine is one debit or credit line within a journal. Exactly one of
// DebitAmount/CreditAmount is non-zero — enforced at the handler layer, not
// the database (no CHECK constraint requires it, matching this platform's
// established pattern of application-layer validation over DB constraints
// for anything beyond basic type/NOT NULL safety).
type JournalLine struct {
	JournalLineID string  `json:"journal_line_id"`
	JournalID     string  `json:"journal_id"`
	LineNumber    int     `json:"line_number"`
	AccountCode   string  `json:"account_code"`
	DebitAmount   float64 `json:"debit_amount"`
	CreditAmount  float64 `json:"credit_amount"`
	Description   string  `json:"description,omitempty"`

	// TaxCode and TaxLogicSnapshotID are nil unless this line has a tax
	// component. No TaxLogicSnapshot-producing service exists yet anywhere
	// in this platform, so TaxLogicSnapshotID is currently always nil in
	// practice — a documented v1 gap (see migration 000003), not an
	// oversight.
	TaxCode            *string `json:"tax_code,omitempty"`
	TaxLogicSnapshotID *string `json:"tax_logic_snapshot_id,omitempty"`

	// Dimensions is ACC-03's "dimensions" input — cost centre, project,
	// department, product, or whatever analysis axes the tenant posts against.
	//
	// Per line, not per journal: one journal routinely splits a single cost
	// across cost centres, and a header-level set could not express that.
	//
	// Free-form keys because REF-08 Financial Dimension Registry does not
	// exist. Nothing can say which dimensions a tenant has defined or which
	// values are valid for one, so the keys are recorded as supplied and not
	// validated — the same posture as account_code with no Chart of Accounts.
	Dimensions Dimensions `json:"dimensions,omitempty"`
}

// JournalWithLines is the full aggregate returned by read endpoints.
type JournalWithLines struct {
	JournalHeader
	Lines []JournalLine `json:"lines"`
}

// ── wire types (request bodies) ─────────────────────────────────────────────

type CreateJournalLineInput struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`

	TaxCode            *string `json:"tax_code,omitempty"`
	TaxLogicSnapshotID *string `json:"tax_logic_snapshot_id,omitempty"`

	// ACC-03 "dimensions". See JournalLine.Dimensions.
	Dimensions Dimensions `json:"dimensions,omitempty"`
}

type CreateJournalRequest struct {
	TenantID      string                   `json:"tenant_id"`
	LegalEntityID string                   `json:"legal_entity_id"`
	FiscalPeriod  string                   `json:"fiscal_period"`
	Description   string                   `json:"description"`
	Lines         []CreateJournalLineInput `json:"lines"`
	CorrelationID string                   `json:"correlation_id"`

	// ── ACC-03 required business/source inputs ────────────────────────────
	//
	// Required. See JournalHeader for why transaction and posting dates are
	// separate fields and why currency is not display metadata.
	JournalType     JournalType `json:"journal_type"`
	TransactionDate Date        `json:"transaction_date"`
	PostingDate     Date        `json:"posting_date"`
	CurrencyCode    string      `json:"currency_code"`

	// Optional: no service can issue or validate these yet (REF-06).
	BookID         *string `json:"book_id,omitempty"`
	ReportingBasis *string `json:"reporting_basis,omitempty"`

	// EvidenceRefs may be sent in the body or, preferably, as the §4 envelope's
	// X-Evidence-Refs header. The handler merges both — a caller that has
	// already put its evidence on the envelope should not have to repeat it.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`

	// SourceEventID and GovernanceDecisionID are optional Atomic Linking
	// references — see JournalHeader's field docs.
	SourceEventID        *string `json:"source_event_id,omitempty"`
	GovernanceDecisionID *string `json:"governance_decision_id,omitempty"`
}

type ReverseJournalRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id"`
}

// ListJournalsFilter holds optional filters for querying journals.
type ListJournalsFilter struct {
	TenantID      string
	LegalEntityID string
	FiscalPeriod  string
	Status        string

	// Limit bounds the page. Zero means "the store's default" — a general
	// ledger only grows, and the register that reads this renders a page at a
	// time whatever the query returns. See store.DefaultListLimit.
	Limit int
}

// ── errors ───────────────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrJournalNotFound         = errorString("journal not found")
	ErrNoLines                 = errorString("journal must have at least one line")
	ErrUnbalancedJournal       = errorString("journal is not balanced: sum(debits) must equal sum(credits)")
	ErrInvalidLine             = errorString("each journal line must have exactly one of debit_amount or credit_amount set, and it must be greater than zero")
	ErrInvalidTransition       = errorString("invalid journal status transition")
	ErrOnlyFinalizedReversible = errorString("only a FINALIZED journal may be reversed")
	ErrStoreUnavailable        = errorString("general ledger store unavailable")

	ErrAuthorizationDenied             = errorString("authorization denied for this ledger action")
	ErrAuthorizationServiceUnavailable = errorString("authorization-svc unavailable")

	// ErrIdentityMissing is returned when a mutation request carries no
	// resolved identity (no X-Principal-Id header) — the request never
	// passed through gateway-auth-svc's ForwardAuth verification. Fail
	// closed, same pattern as schema-registry-svc.
	ErrIdentityMissing = errorString("caller identity missing")

	ErrPeriodLocked            = errorString("accounting period is closed or locked")
	ErrCloseServiceUnavailable = errorString("financial-close-svc unavailable")

	// ErrInvalidIdentifier is returned when an id cannot be a UUID at all.
	// journal_id, tenant_id and legal_entity_id are uuid columns, so Postgres
	// raises SQLSTATE 22P02 from inside the driver before any row is examined.
	// Distinguished from a dead store so a typo in a URL stops answering 503.
	ErrInvalidIdentifier = errorString("identifier is not a valid UUID")

	// ErrTenantScopeMismatch is returned when a request names a tenant other
	// than the one the caller was verified as. The tenant in a body or query
	// string is a caller-supplied claim; X-Tenant-Id is the gateway's verified
	// answer, and where they disagree the request is refused rather than
	// quietly served under either one.
	ErrTenantScopeMismatch = errorString("request tenant_id does not match the caller's verified tenant scope")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope at all — it never passed gateway-auth-svc's ForwardAuth
	// verification. Fail closed, same posture as ErrIdentityMissing.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ── ACC-03 input contract ────────────────────────────────────────────

	ErrInvalidJournalType = errorString("journal_type must be one of STANDARD, ADJUSTMENT, ACCRUAL, REVERSAL, OPENING, CLOSING, RECLASS")

	// ErrInvalidCurrency covers shape only. REF-02 Currency Registry does not
	// exist, so nothing can say whether a well-formed code is one this tenant
	// or book actually transacts in.
	ErrInvalidCurrency = errorString("currency_code must be a 3-letter ISO 4217 code, e.g. GBP")

	// ErrPostingBeforeTransaction is raised when posting_date precedes
	// transaction_date. The reverse order is ordinary — an invoice dated the
	// 28th posted on the 3rd — but a journal that reaches the ledger before its
	// own source document exists is a data-entry error every time, and catching
	// it here is cheaper than finding it in a period reconciliation.
	ErrPostingBeforeTransaction = errorString("posting_date cannot precede transaction_date")
)

// ValidCurrencyCode reports whether s has the shape of an ISO 4217 alphabetic
// code: exactly three uppercase ASCII letters.
//
// Shape only, deliberately. Checking membership of a real currency list would
// mean embedding one, and doctrine forbids a service hardcoding a currency —
// that belongs in REF-02 Currency Registry, consumed as versioned data.
func ValidCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
