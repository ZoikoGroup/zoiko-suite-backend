// Package domain defines the authoritative domain types for general-ledger-svc.
//
// Per docs/architecture/03-microservices.md §10.1, this service is the
// authoritative owner of journalized financial postings and ledger state.
// It owns journal headers, journal lines, posting state, and fiscal period
// linkage.
//
// It also now hosts ACC-01's Chart of Accounts as a SEPARATE authority
// (see migration 000007 and the Account type below) — co-located in this
// same deployable process, per this domain spec's own opening statement
// ("does not require 18 separately deployed microservices... does
// require that the authority... remain separately testable and
// non-bypassable regardless of physical deployment grouping"), but never
// the same authority: journal state, posting consequence, and report
// presentation remain explicitly outside what ACC-01 may own, per the
// spec's own Cross-Service Accounting Authority Matrix.
package domain

import (
	"encoding/json"
	"time"
)

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

// ApprovalStatus implements ACC-03's own state model (verbatim from spec):
// "Draft → PendingApproval → Approved → PostingRequested → Posted;
// rejected/cancelled before posting; corrections create new journals."
// See migration 000010's doc comment for why this is a separate lifecycle
// from JournalStatus.
type ApprovalStatus string

const (
	ApprovalStatusDraft            ApprovalStatus = "DRAFT"
	ApprovalStatusPendingApproval  ApprovalStatus = "PENDING_APPROVAL"
	ApprovalStatusApproved         ApprovalStatus = "APPROVED"
	ApprovalStatusPostingRequested ApprovalStatus = "POSTING_REQUESTED"
	ApprovalStatusPosted           ApprovalStatus = "POSTED"
	ApprovalStatusRejected         ApprovalStatus = "REJECTED"
	ApprovalStatusCancelled        ApprovalStatus = "CANCELLED"
)

// ValidApprovalTransitions enumerates the only legal ApprovalStatus moves.
// PENDING_APPROVAL -> DRAFT models AmendDraftJournal being called on a
// submitted-but-not-yet-approved journal — editing it withdraws it from
// approval rather than silently leaving a stale approval request pending
// against content that changed underneath it.
var ValidApprovalTransitions = map[ApprovalStatus][]ApprovalStatus{
	ApprovalStatusDraft:            {ApprovalStatusPendingApproval, ApprovalStatusCancelled},
	ApprovalStatusPendingApproval:  {ApprovalStatusDraft, ApprovalStatusApproved, ApprovalStatusRejected},
	ApprovalStatusApproved:         {ApprovalStatusPostingRequested},
	ApprovalStatusPostingRequested: {ApprovalStatusPosted},
	ApprovalStatusPosted:           {},
	ApprovalStatusRejected:         {},
	ApprovalStatusCancelled:        {},
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

	// JournalTypeRevaluation is the journal the kernel posts for period-end revaluation. It is never accepted from a caller:
	// only the kernel originates it (standard §8.1).
	JournalTypeRevaluation JournalType = "REVALUATION"

	// JournalTypeConsolidation and JournalTypeElimination are the journals the kernel posts into a group book when it consolidates:
	// the translated member balances and the intercompany eliminations. Like revaluation, only the kernel originates them.
	JournalTypeConsolidation JournalType = "CONSOLIDATION"
	JournalTypeElimination   JournalType = "ELIMINATION"

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

	// ── ACC-03 journal proposal/approval lifecycle (§6) ─────────────────────
	//
	// A SEPARATE lifecycle from Status above — Status is ACC-04/05's own
	// Tri-Phase Commit for the actual ledger write; ApprovalStatus is
	// whether this proposal is even ELIGIBLE to reach the posting engine.
	// See migration 000010's doc comment.
	ApprovalStatus ApprovalStatus `json:"approval_status"`

	// ApprovalFingerprint is the spec's own named evidence: a permanent
	// hash of exactly what content was approved, captured at ApproveJournal
	// time. Never used to gate anything after the fact — no endpoint edits
	// a journal once it leaves DRAFT/PENDING_APPROVAL, so there is nothing
	// for it to detect a mismatch against; it exists purely so an audit can
	// prove what a human actually signed off on.
	ApprovalFingerprint *string `json:"approval_fingerprint,omitempty"`

	SubmittedAt                   *time.Time `json:"submitted_at,omitempty"`
	SubmittedByPrincipalID        *string    `json:"submitted_by_principal_id,omitempty"`
	ApprovedAt                    *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID         *string    `json:"approved_by_principal_id,omitempty"`
	RejectedAt                    *time.Time `json:"rejected_at,omitempty"`
	RejectedByPrincipalID         *string    `json:"rejected_by_principal_id,omitempty"`
	RejectionReason               *string    `json:"rejection_reason,omitempty"`
	PostingRequestedAt            *time.Time `json:"posting_requested_at,omitempty"`
	PostingRequestedByPrincipalID *string    `json:"posting_requested_by_principal_id,omitempty"`

	// CorrectionOfJournalID is the spec's own "correction chain": set only
	// on a journal created via RequestCorrection, pointing at the original
	// it corrects. "Corrections create new journals" — never an in-place
	// edit of a posted one.
	CorrectionOfJournalID *string `json:"correction_of_journal_id,omitempty"`

	// SoftCloseOverrideReason is the mandatory, non-empty reason provided
	// when a journal is posted during SOFT_CLOSE (posting_policy RESTRICTED)
	// with override_soft_close=true. Persisted per ZS-SVC-B-001 ACC-14 §10.1
	// and emitted in the JournalCreated outbox event for audit traceability.
	SoftCloseOverrideReason *string `json:"soft_close_override_reason,omitempty"`

	// ── Accounting Kernel journal model (standard §8.1) ──────────────────────
	//
	// JournalNumber is the human-readable sequence, unique per entity+book+year and allocated when the journal is posted;
	// it is never a relational key. AccountingBookID names the kernel book (BookID above stays the legacy book code that
	// ledger entries are keyed by). SourceAccountingEventID and PostingExecutionID tie a system journal to the exact event
	// and rule execution that produced it. JournalHash seals the posted content (AK-INV: posted journals are immutable).
	JournalNumber           *string `json:"journal_number,omitempty"`
	AccountingBookID        *string `json:"accounting_book_id,omitempty"`
	SourceAccountingEventID *string `json:"source_accounting_event_id,omitempty"`
	PostingExecutionID      *string `json:"posting_execution_id,omitempty"`
	JournalHash             *string `json:"journal_hash,omitempty"`

	// JournalPurpose is the controlled classification a manual journal must carry (§9). CorrectionKind and
	// ReversalReason record the nature of a correction (§11), so a policy change is never filed as an error correction.
	JournalPurpose *string `json:"journal_purpose,omitempty"`
	CorrectionKind *string `json:"correction_kind,omitempty"`
	ReversalReason *string `json:"reversal_reason,omitempty"`

	// CopyOfJournalID and RecurringTemplateID record provenance: a copy or a generated instance is a new draft with a
	// new identity, never a duplicate of posting identifiers (§9).
	CopyOfJournalID     *string `json:"copy_of_journal_id,omitempty"`
	RecurringTemplateID *string `json:"recurring_template_id,omitempty"`

	WithdrawnAt            *time.Time `json:"withdrawn_at,omitempty"`
	WithdrawnByPrincipalID *string    `json:"withdrawn_by_principal_id,omitempty"`
	WithdrawnReason        *string    `json:"withdrawn_reason,omitempty"`

	// ReconciliationCaseID names the reconciliation case a reconciliation adjustment settles (standard §17): a bank or
	// subledger adjustment is never a free-standing correction.
	ReconciliationCaseID *string `json:"reconciliation_case_id,omitempty"`
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

	// Foreign-currency and provenance data (standard §8.2). Debit/credit above are always in the book's functional
	// currency; when the source transaction was in another currency the original amount, its currency and the rate that
	// converted it are kept, and PostingRuleLineRef names the exact rule template that generated the line.
	TransactionAmount  *string `json:"transaction_amount,omitempty"`
	TransactionCcy     *string `json:"transaction_currency,omitempty"`
	ExchangeRate       *string `json:"exchange_rate,omitempty"`
	RateRef            *string `json:"rate_ref,omitempty"`
	PostingRuleLineRef *string `json:"posting_rule_line_ref,omitempty"`

	// CounterpartyID is the customer or supplier a line on a subledger control account belongs to (standard §8.2, §12).
	CounterpartyID *string `json:"counterparty_id,omitempty"`
}

// JournalWithLines is the full aggregate returned by read endpoints.
type JournalWithLines struct {
	JournalHeader
	Lines []JournalLine `json:"lines"`
}

// TrialBalanceSnapshot is ACC-15's real, durable trial-balance dataset —
// pinned to an explicit ledger watermark (invariant #11: "trial balances
// reconcile to an explicit ledger watermark") rather than recompiled ad
// hoc, client-side, by every caller that needs one. Never updated or
// deleted once written (database-enforced, see migration 000006).
type TrialBalanceSnapshot struct {
	TrialBalanceSnapshotID string `json:"trial_balance_snapshot_id"`
	TenantID               string `json:"tenant_id"`
	LegalEntityID          string `json:"legal_entity_id"`
	FiscalPeriod           string `json:"fiscal_period"`
	// LedgerWatermark is MAX(journal_seq) among the FINALIZED/REVERSED
	// journals this snapshot actually included — a real, monotonic,
	// reproducible answer to "as of what point in the ledger."
	LedgerWatermark       int64              `json:"ledger_watermark"`
	CompiledAt            time.Time          `json:"compiled_at"`
	CompiledByPrincipalID string             `json:"compiled_by_principal_id"`
	Lines                 []TrialBalanceLine `json:"lines"`
}

// TrialBalanceLine is one account's net balance (debit - credit) within a
// TrialBalanceSnapshot, summed across every FINALIZED/REVERSED journal
// line for that account at the snapshot's watermark.
//
// NetBalance is an exact decimal string, never float64 — ZS-DATA-001 D06,
// the same rule SumLines's own doc comment explains for this package. The
// underlying trial_balance_lines.net_balance column is NUMERIC(18,2); a
// float64 field here would silently reintroduce the exactness this
// snapshot otherwise guarantees, for every consumer that reads it back
// (e.g. REP-01's ACC-16 ingestion), not just for GL's own code.
type TrialBalanceLine struct {
	AccountCode string `json:"account_code"`
	NetBalance  string `json:"net_balance"`
}

// AccountType is one of the five fundamental account classes.
type AccountType string

const (
	AccountTypeAsset     AccountType = "ASSET"
	AccountTypeLiability AccountType = "LIABILITY"
	AccountTypeEquity    AccountType = "EQUITY"
	AccountTypeRevenue   AccountType = "REVENUE"
	AccountTypeExpense   AccountType = "EXPENSE"
)

var validAccountTypes = map[AccountType]bool{
	AccountTypeAsset: true, AccountTypeLiability: true, AccountTypeEquity: true,
	AccountTypeRevenue: true, AccountTypeExpense: true,
}

func IsValidAccountType(t AccountType) bool { return validAccountTypes[t] }

// Normal balance sides (ACC-01 "Required source inputs": normal_balance).
const (
	NormalBalanceDebit  = "DEBIT"
	NormalBalanceCredit = "CREDIT"
)

// NormalBalance is the side on which an account of this type normally
// carries its balance, by standard accounting convention: ASSET and EXPENSE
// are debit-normal; LIABILITY, EQUITY and REVENUE are credit-normal.
//
// Derived from the type, never stored (SPEC_DEVIATIONS A2/B11): a separate
// editable column could contradict the class. An unrecognised type returns
// "", never a guess.
func (t AccountType) NormalBalance() string {
	switch t {
	case AccountTypeAsset, AccountTypeExpense:
		return NormalBalanceDebit
	case AccountTypeLiability, AccountTypeEquity, AccountTypeRevenue:
		return NormalBalanceCredit
	default:
		return ""
	}
}

// MarshalJSON adds the read-only, derived normal_balance to every account
// response (CreateAccount, GetAccount, ListAccounts, AmendAccountVersion,
// Activate/Deactivate/ReparentAccount all return Account). It is computed
// from AccountType on the way out, so it cannot disagree with it.
func (a Account) MarshalJSON() ([]byte, error) {
	type account Account // drops this method, so no recursion
	return json.Marshal(struct {
		account
		NormalBalance string `json:"normal_balance,omitempty"`
	}{account(a), a.AccountType.NormalBalance()})
}

// MarshalJSON adds normal_balance to an account version (GetAccountAsOf),
// derived from the type in force in that version.
func (v AccountVersion) MarshalJSON() ([]byte, error) {
	type accountVersion AccountVersion
	return json.Marshal(struct {
		accountVersion
		NormalBalance string `json:"normal_balance,omitempty"`
	}{accountVersion(v), v.AccountType.NormalBalance()})
}

// Account is ACC-01's Chart of Accounts entry — the platform's first real
// posting-account master (see migration 000007's doc comment). Kept as
// its own authority, separate from JournalHeader/JournalLine, per the
// spec's own Cross-Service Accounting Authority Matrix.
type Account struct {
	AccountID       string      `json:"account_id"`
	TenantID        string      `json:"tenant_id"`
	AccountCode     string      `json:"account_code"`
	AccountName     string      `json:"account_name"`
	AccountType     AccountType `json:"account_type"`
	ParentAccountID *string     `json:"parent_account_id,omitempty"`

	// IsControlAccount/DirectPostingRestricted implement invariant #7:
	// "Control accounts cannot be bypassed by ordinary manual journals
	// where policy restricts direct posting." Two independent facts — a
	// control account with no restriction is a real, allowed state, not
	// every control account blocks direct posting by default.
	IsControlAccount        bool `json:"is_control_account"`
	DirectPostingRestricted bool `json:"direct_posting_restricted"`

	Status               string    `json:"status"` // ACTIVE | INACTIVE
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`

	// Version is the current head version. Version 1 is written with the
	// account itself; every later AmendAccountVersion/state/hierarchy command
	// appends a new immutable account_versions row and advances this number.
	Version int64 `json:"version"`
}

type CreateAccountRequest struct {
	AccountCode             string      `json:"account_code"`
	AccountName             string      `json:"account_name"`
	AccountType             AccountType `json:"account_type"`
	ParentAccountID         *string     `json:"parent_account_id,omitempty"`
	IsControlAccount        bool        `json:"is_control_account,omitempty"`
	DirectPostingRestricted bool        `json:"direct_posting_restricted,omitempty"`
}

// AccountVersion is one immutable row of ACC-01's AccountHierarchyVersion
// history. EffectiveTo is nil for the currently effective version; an
// amendment/state/hierarchy change closes the prior interval at the new
// version's EffectiveFrom. GetAccountAsOf reconstructs the version in force at
// a caller-supplied instant from exactly these intervals.
type AccountVersion struct {
	AccountVersionID        string      `json:"account_version_id"`
	AccountID               string      `json:"account_id"`
	TenantID                string      `json:"tenant_id"`
	AccountCode             string      `json:"account_code"`
	Version                 int64       `json:"version"`
	AccountName             string      `json:"account_name"`
	AccountType             AccountType `json:"account_type"`
	ParentAccountID         *string     `json:"parent_account_id,omitempty"`
	IsControlAccount        bool        `json:"is_control_account"`
	DirectPostingRestricted bool        `json:"direct_posting_restricted"`
	Status                  string      `json:"status"`
	EffectiveFrom           time.Time   `json:"effective_from"`
	EffectiveTo             *time.Time  `json:"effective_to,omitempty"`
	ChangeKind              string      `json:"change_kind"`
	ChangeReason            string      `json:"change_reason,omitempty"`
	EvidenceRefs            []string    `json:"evidence_refs,omitempty"`
	CreatedAt               time.Time   `json:"created_at"`
	CreatedByPrincipalID    string      `json:"created_by_principal_id"`
}

// AmendAccountVersionRequest is a partial amendment of an existing account:
// omitted fields keep their current values, so a caller changing one field
// does not have to echo the whole master. To clear a parent, send
// "parent_account_id": null explicitly (the handler distinguishes an absent
// field from an explicit null). ExpectedVersion is optional for compatibility
// with this service's existing commands, but when supplied it is enforced as a
// real optimistic-concurrency check.
type AmendAccountVersionRequest struct {
	ExpectedVersion         int64          `json:"expected_version,omitempty"`
	EffectiveFrom           *EffectiveDate `json:"effective_from,omitempty"`
	AccountName             *string        `json:"account_name,omitempty"`
	AccountType             *AccountType   `json:"account_type,omitempty"`
	ParentAccountID         *string        `json:"parent_account_id,omitempty"`
	ParentAccountIDSet      bool           `json:"-"`
	IsControlAccount        *bool          `json:"is_control_account,omitempty"`
	DirectPostingRestricted *bool          `json:"direct_posting_restricted,omitempty"`
	Reason                  string         `json:"reason,omitempty"`
	EvidenceRefs            []string       `json:"evidence_refs,omitempty"`
}

// ActivateAccountRequest and DeactivateAccountRequest are the optional request
// bodies of the two status commands. Both fields beyond the path are optional:
// the existing DeactivateAccount accepted no body at all, so requiring one now
// would break every current caller. expected_version, when supplied, is a real
// optimistic-concurrency check.
type ActivateAccountRequest struct {
	ExpectedVersion int64          `json:"expected_version,omitempty"`
	EffectiveFrom   *EffectiveDate `json:"effective_from,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	EvidenceRefs    []string       `json:"evidence_refs,omitempty"`
}

// DeactivateAccountRequest is the DeactivateAccount body — a separate named
// type so the two commands' contracts can diverge later without an alias.
type DeactivateAccountRequest struct {
	ExpectedVersion int64          `json:"expected_version,omitempty"`
	EffectiveFrom   *EffectiveDate `json:"effective_from,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	EvidenceRefs    []string       `json:"evidence_refs,omitempty"`
}

// ResolvePostingEligibilityRequest is ACC-01's read-side answer to "may this
// account be posted to right now". AccountCode is optional when the route
// already names the account. LegalEntityID is only used as the authorization
// scope when the caller asks for the control-account override.
type ResolvePostingEligibilityRequest struct {
	AccountCode                       string `json:"account_code,omitempty"`
	LegalEntityID                     string `json:"legal_entity_id,omitempty"`
	OverrideControlAccountRestriction bool   `json:"override_control_account_restriction,omitempty"`
}

// PostingEligibility is the resolver's answer. It formalises the exact rules
// PostAccountingEvent already enforces inline (see
// handler.checkAccountRestrictions): an account the chart has never heard of
// remains postable (the deliberate bootstrap gap), an INACTIVE account is not,
// and a control account with direct_posting_restricted needs an explicit,
// separately authorized override.
type PostingEligibility struct {
	AccountID               string `json:"account_id,omitempty"`
	AccountCode             string `json:"account_code"`
	Registered              bool   `json:"registered"`
	Eligible                bool   `json:"eligible"`
	Status                  string `json:"status,omitempty"`
	Version                 int64  `json:"version,omitempty"`
	IsControlAccount        bool   `json:"is_control_account"`
	DirectPostingRestricted bool   `json:"direct_posting_restricted"`
	RequiresOverride        bool   `json:"requires_override"`
	OverrideApplied         bool   `json:"override_applied"`
	Reason                  string `json:"reason,omitempty"`
}

// EvaluatePostingEligibility applies ACC-01's posting rules to an account. A
// nil account is the unregistered case: the caller gets an explicit
// registered=false answer rather than a 404, because ACC-04 must still be able
// to accept the legacy codes nothing has registered.
func EvaluatePostingEligibility(accountCode string, a *Account, override bool) PostingEligibility {
	if a == nil {
		return PostingEligibility{
			AccountCode: accountCode,
			Registered:  false,
			Eligible:    true,
			Status:      "UNREGISTERED",
			Reason:      "account is not registered in the Chart of Accounts; legacy posting remains permitted",
		}
	}
	result := PostingEligibility{
		AccountID:               a.AccountID,
		AccountCode:             a.AccountCode,
		Registered:              true,
		Status:                  a.Status,
		Version:                 a.Version,
		IsControlAccount:        a.IsControlAccount,
		DirectPostingRestricted: a.DirectPostingRestricted,
	}
	if a.Status != "ACTIVE" {
		result.Reason = ErrAccountInactive.Error()
		return result
	}
	if a.IsControlAccount && a.DirectPostingRestricted {
		result.RequiresOverride = true
		if !override {
			result.Reason = ErrControlAccountPostingRestricted.Error()
			return result
		}
		result.OverrideApplied = true
	}
	result.Eligible = true
	return result
}

// GetAccountAsOfRequest is the point-in-time read over the version history.
// AsOf is required; a request without one is refused rather than defaulted to
// "now", because a historical read with an accidental current-time default
// returns a plausible wrong answer.
type GetAccountAsOfRequest struct {
	AccountCode string         `json:"account_code,omitempty"`
	AsOf        *EffectiveDate `json:"as_of,omitempty"`
}

// ReparentAccountRequest changes an account's parent in the chart hierarchy.
// The parent field is required but may be null: an explicit null makes the
// account a root, while omitting the field entirely is refused so an
// accidental empty body cannot silently detach an account. The alias
// new_parent_account_id is accepted for callers reading the command name
// literally.
type ReparentAccountRequest struct {
	ExpectedVersion    int64          `json:"expected_version,omitempty"`
	ParentAccountID    *string        `json:"parent_account_id"`
	ParentAccountIDSet bool           `json:"-"`
	EffectiveFrom      *EffectiveDate `json:"effective_from,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	EvidenceRefs       []string       `json:"evidence_refs,omitempty"`
}

// UnmarshalJSON accepts both parent_account_id and new_parent_account_id and
// records whether either key was present.
func (r *ReparentAccountRequest) UnmarshalJSON(b []byte) error {
	type plain ReparentAccountRequest
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = ReparentAccountRequest(p)
	if v, ok := raw["new_parent_account_id"]; ok {
		var parent *string
		if err := json.Unmarshal(v, &parent); err != nil {
			return err
		}
		r.ParentAccountID = parent
		r.ParentAccountIDSet = true
		return nil
	}
	_, r.ParentAccountIDSet = raw["parent_account_id"]
	return nil
}

// UnmarshalJSON records whether parent_account_id was present at all. A plain
// *string cannot tell "clear the parent" from "leave the parent unchanged",
// and silently treating one as the other would move an account in the chart
// hierarchy without the caller having asked for it.
func (r *AmendAccountVersionRequest) UnmarshalJSON(b []byte) error {
	type plain AmendAccountVersionRequest
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = AmendAccountVersionRequest(p)
	_, r.ParentAccountIDSet = raw["parent_account_id"]
	return nil
}

// AccountMapping is ACC-02's effective-dated mapping of a caller-declared
// business concept (MappingKey — its meaning belongs to whichever domain
// declares it; this service never interprets it) to a real, chart-
// registered AccountCode. Versioned, never mutated in place (see
// migration 000008's doc comment).
type AccountMapping struct {
	AccountMappingID     string     `json:"account_mapping_id"`
	TenantID             string     `json:"tenant_id"`
	MappingKey           string     `json:"mapping_key"`
	AccountCode          string     `json:"account_code"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

type SetAccountMappingRequest struct {
	MappingKey  string `json:"mapping_key"`
	AccountCode string `json:"account_code"`
}

// Posting execution lifecycle states (ACC-04's state model, verbatim from
// spec: "Submitted → Validating → Ready → Committed or Failed/Quarantined;
// no partial committed state").
const (
	PostingExecutionStatusSubmitted   = "SUBMITTED"
	PostingExecutionStatusValidating  = "VALIDATING"
	PostingExecutionStatusReady       = "READY"
	PostingExecutionStatusCommitted   = "COMMITTED"
	PostingExecutionStatusFailed      = "FAILED"
	PostingExecutionStatusQuarantined = "QUARANTINED"
)

// PostingExecutionKind distinguishes which ACC-04 command created a
// posting_executions row.
const (
	PostingExecutionKindEvent           = "EVENT"
	PostingExecutionKindApprovedJournal = "APPROVED_JOURNAL"
	PostingExecutionKindReversal        = "REVERSAL"
)

// PostingExecution is ACC-04's own authority — "PostingExecution,
// calculation trace, rule resolution, posting batch and consequence
// uniqueness record; ledger entries are committed to ACC-05," explicitly
// never "Source business fact, tax determination": every posting
// execution's REQUIRED input is an already-approved journal or event —
// ACC-04 orchestrates the commit, it never decides what should be posted.
type PostingExecution struct {
	ExecutionID          string     `json:"execution_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	Kind                 string     `json:"kind"`
	SourceEventID        *string    `json:"source_event_id,omitempty"`
	IdempotencyKey       *string    `json:"idempotency_key,omitempty"`
	Status               string     `json:"status"`
	JournalID            *string    `json:"journal_id,omitempty"`
	CalculationTrace     string     `json:"calculation_trace"` // raw JSON — see ExplainPosting
	FailureReason        *string    `json:"failure_reason,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CommittedAt          *time.Time `json:"committed_at,omitempty"`

	// RequestPayload is the accepted PostAccountingEventRequest, kept so
	// ReprocessFailedPosting can replay an execution that failed before its
	// journal was written (migration 000014). Nil for executions recorded
	// before then. Not part of the API response.
	RequestPayload []byte `json:"-"`
}

// PostingEventLineInput is one line of a caller-declared accounting event.
// Exactly one of AccountCode/MappingKey must be set — both or neither is
// the spec's own negative path, "Posting rule ambiguity."
type PostingEventLineInput struct {
	AccountCode  *string `json:"account_code,omitempty"`
	MappingKey   *string `json:"mapping_key,omitempty"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

// PostAccountingEventRequest is ACC-04's PostAccountingEvent command input
// — the required source inputs the spec names: "approved accounting
// event/journal; entity/book; source amounts; dimensions; tax result;
// source references; idempotency key." (Tax result and dimensions are
// accepted as opaque, caller-declared pass-through fields where this
// platform has no owning service for them yet, same posture as ACC-03's
// own book_id/reporting_basis fields.)
type PostAccountingEventRequest struct {
	LegalEntityID string                  `json:"legal_entity_id"`
	FiscalPeriod  string                  `json:"fiscal_period"`
	Description   string                  `json:"description"`
	SourceEventID string                  `json:"source_event_id"`
	CorrelationID string                  `json:"correlation_id"`
	Lines         []PostingEventLineInput `json:"lines"`

	// Spec s5.1 / s8.1 mandatory fields. TransactionCurrency is ISO 4217
	// shape (three uppercase letters); DocumentDate maps to the journal's
	// TransactionDate; PostingDate defaults to DocumentDate when omitted.
	TransactionCurrency string `json:"transaction_currency"`
	DocumentDate        Date   `json:"document_date"`
	PostingDate         Date   `json:"posting_date"`

	// OverrideSoftClose and SoftCloseOverrideReason mirror the fields on
	// CreateJournalRequest — see those field docs for semantics.
	OverrideSoftClose       bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

type PostApprovedJournalRequest struct {
	JournalID string `json:"journal_id"`

	// OverrideSoftClose and SoftCloseOverrideReason mirror the fields on
	// CreateJournalRequest — see those field docs for semantics.
	OverrideSoftClose       bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

type CreateReversalPostingRequest struct {
	OriginalJournalID string `json:"original_journal_id"`
	Reason            string `json:"reason"`

	// OverrideSoftClose and SoftCloseOverrideReason mirror the fields on
	// CreateJournalRequest — see those field docs for semantics.
	OverrideSoftClose       bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

// LedgerEntry is ACC-05's own authority: "LedgerEntry and authoritative
// posted balance state; no draft/manual business lifecycle." One row per
// journal line, written exactly once — when the journal that contains it
// reaches FINALIZED — and never updated or deleted afterward (see
// migration 000011's doc comment and its append-only trigger). Never
// constructed directly by a handler; only PgStore.appendLedgerEntries,
// called from inside TransitionJournal's own transaction, ever inserts one.
type LedgerEntry struct {
	LedgerEntryID   string     `json:"ledger_entry_id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	BookID          string     `json:"book_id,omitempty"`
	FiscalPeriod    string     `json:"fiscal_period"`
	JournalID       string     `json:"journal_id"`
	JournalLineID   string     `json:"journal_line_id"`
	LineNumber      int        `json:"line_number"`
	AccountCode     string     `json:"account_code"`
	DebitAmount     float64    `json:"debit_amount"`
	CreditAmount    float64    `json:"credit_amount"`
	CurrencyCode    string     `json:"currency_code"`
	Dimensions      Dimensions `json:"dimensions,omitempty"`
	TransactionDate Date       `json:"transaction_date"`
	PostingDate     Date       `json:"posting_date"`
	SourceEventID   *string    `json:"source_event_id,omitempty"`
	CorrelationID   string     `json:"correlation_id"`
	EntrySeq        int64      `json:"entry_seq"`
	CreatedAt       time.Time  `json:"created_at"`
}

// LedgerBalance is ACC-05's "balance projections versioned/rebuildable
// from entries" — a derived, replaceable aggregate over ledger_entries,
// never itself a source of truth. Grouped by entity/book/account/period
// and a canonicalized dimensions key, matching the spec's "by entity,
// book, account, dimension and period."
//
// DebitTotal/CreditTotal/NetBalance are exact decimal strings, never
// float64 (ZS-DATA-001 D06: binary floating point is prohibited for
// monetary values — the same rule SumLines's own doc comment already
// explains for this package). The underlying ledger_balances columns are
// NUMERIC; these fields used to lose that exactness the moment Go scanned
// them into a float64, which also meant equality comparisons on NetBalance
// anywhere downstream were comparing binary floats representing decimal
// money, exactly the defect class SumLines exists to avoid.
type LedgerBalance struct {
	TenantID          string    `json:"tenant_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	BookID            string    `json:"book_id,omitempty"`
	AccountCode       string    `json:"account_code"`
	FiscalPeriod      string    `json:"fiscal_period"`
	DimensionsKey     string    `json:"dimensions_key,omitempty"`
	DebitTotal        string    `json:"debit_total"`
	CreditTotal       string    `json:"credit_total"`
	NetBalance        string    `json:"net_balance"`
	WatermarkEntrySeq int64     `json:"watermark_entry_seq"`
	RebuiltAt         time.Time `json:"rebuilt_at"`
}

// QueryLedgerFilter is QueryLedger's own scope — every field but
// LegalEntityID is optional narrowing. LegalEntityID is mandatory
// (ErrLedgerScopeRequired) so a caller can never accidentally receive
// every entity's entries at once.
type QueryLedgerFilter struct {
	LegalEntityID string
	BookID        string
	AccountCode   string
	FiscalPeriod  string
	JournalID     string
	MaxEntrySeq   *int64 // set by QueryLedgerAsOf to reconstruct a point-in-time view
}

// QueryAccountBalanceRequest is QueryAccountBalance's input — reads the
// ledger_balances projection, not ledger_entries directly.
type QueryAccountBalanceRequest struct {
	LegalEntityID string
	BookID        string
	AccountCode   string
	FiscalPeriod  string
}

// RebuildBalanceProjectionRequest is RebuildDerivedBalanceProjection's
// input — always scoped to one entity/period so a rebuild's blast radius
// is explicit and bounded, never a whole-tenant rebuild by accident.
type RebuildBalanceProjectionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	BookID        string `json:"book_id,omitempty"`
	FiscalPeriod  string `json:"fiscal_period"`
}

type CompileTrialBalanceRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	FiscalPeriod  string `json:"fiscal_period"`
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

	// CounterpartyID is required on a line posted to an account that needs one (AR, AP control accounts and any account
	// flagged counterparty_required).
	CounterpartyID *string `json:"counterparty_id,omitempty"`
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

	// JournalPurpose is the controlled purpose of a manual journal (standard §9). Required for the adjusting journal
	// types (ADJUSTMENT, ACCRUAL, RECLASS, OPENING, CLOSING), where free text alone is not enough.
	JournalPurpose string `json:"journal_purpose,omitempty"`

	// ReconciliationCaseID is required when the purpose is RECONCILIATION_ADJUSTMENT.
	ReconciliationCaseID string `json:"reconciliation_case_id,omitempty"`

	// EvidenceRefs may be sent in the body or, preferably, as the §4 envelope's
	// X-Evidence-Refs header. The handler merges both — a caller that has
	// already put its evidence on the envelope should not have to repeat it.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`

	// SourceEventID and GovernanceDecisionID are optional Atomic Linking
	// references — see JournalHeader's field docs.
	SourceEventID        *string `json:"source_event_id,omitempty"`
	GovernanceDecisionID *string `json:"governance_decision_id,omitempty"`

	// OverrideControlAccountRestriction is a caller-declared, never-inferred
	// opt-in (same doctrine as privacy-decision-svc's ConsentCheckRequired)
	// — required, and separately authorized, to post directly to a control
	// account with direct_posting_restricted=true (ACC-01 invariant #7).
	// False/omitted is the ordinary case and needs no special authority.
	OverrideControlAccountRestriction bool `json:"override_control_account_restriction,omitempty"`

	// OverrideSoftClose is a caller-declared, never-inferred opt-in to post
	// during a SOFT_CLOSE period (posting_policy RESTRICTED). Unlike the
	// control-account override, this one also requires a non-empty
	// SoftCloseOverrideReason that is persisted on the journal header and
	// included in outbox events. Both the flag and the reason must be
	// present, and the caller must hold the GL_SOFT_CLOSE_POSTING_OVERRIDE
	// authorization action (checked via authorization-svc).
	OverrideSoftClose bool `json:"override_soft_close,omitempty"`

	// SoftCloseOverrideReason is the mandatory, non-empty reason string
	// required when OverrideSoftClose is true. Whitespace-only is rejected.
	// Persisted on the journal header's soft_close_override_reason column and
	// emitted in the JournalCreated outbox event for audit traceability.
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

type ReverseJournalRequest struct {
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id"`

	// CorrectionKind is the nature of the correction (§11); optional so existing callers keep working, validated when sent.
	// PostingDate lets the caller choose the date the reversal takes effect (§32); it defaults as it always has.
	CorrectionKind string `json:"correction_kind,omitempty"`
	PostingDate    Date   `json:"posting_date,omitempty"`

	// OverrideSoftClose and SoftCloseOverrideReason mirror the fields on
	// CreateJournalRequest — see those field docs for semantics.
	OverrideSoftClose       bool   `json:"override_soft_close,omitempty"`
	SoftCloseOverrideReason string `json:"soft_close_override_reason,omitempty"`
}

// AmendDraftJournalRequest is ACC-03's AmendDraftJournal command — a full
// replace of the header's editable business fields and every line. Only
// legal while ApprovalStatus is DRAFT or PENDING_APPROVAL (see
// ValidApprovalTransitions); the spec's own negative paths #2 ("journal
// changed after approval") and #4 ("attempt edit after posting") are
// satisfied by there being no other status this succeeds from.
type AmendDraftJournalRequest struct {
	Description     string                   `json:"description"`
	JournalType     JournalType              `json:"journal_type"`
	TransactionDate Date                     `json:"transaction_date"`
	PostingDate     Date                     `json:"posting_date"`
	CurrencyCode    string                   `json:"currency_code"`
	BookID          *string                  `json:"book_id,omitempty"`
	ReportingBasis  *string                  `json:"reporting_basis,omitempty"`
	EvidenceRefs    []string                 `json:"evidence_refs,omitempty"`
	Lines           []CreateJournalLineInput `json:"lines"`
}

// ApproveJournalRequest carries nothing today beyond the implicit actor —
// its own type exists so a future policy_version/approval-note field has
// somewhere to land without changing the handler's signature.
type ApproveJournalRequest struct{}

type RejectJournalRequest struct {
	Reason string `json:"reason"`
}

type RequestCorrectionRequest struct {
	Reason      string                   `json:"reason"`
	Description string                   `json:"description"`
	Lines       []CreateJournalLineInput `json:"lines"`
}

// AvailableActions answers ACC-03's own GetAvailableActions query — which
// of the lifecycle commands are legal to call on this journal right now,
// derived directly from ValidApprovalTransitions rather than a second,
// separately-maintained rulebook that could drift from the one the
// handlers actually enforce.
type AvailableActions struct {
	JournalID string   `json:"journal_id"`
	Actions   []string `json:"actions"`
}

// JournalHistoryEntry is one step of ACC-03's own GetJournalHistory query
// — derived from the header's own timestamp/actor columns, never a
// separate events table this v1 didn't build; an entry appears only if
// its corresponding timestamp is actually set.
type JournalHistoryEntry struct {
	Event       string    `json:"event"`
	At          time.Time `json:"at"`
	PrincipalID string    `json:"principal_id"`
	Detail      string    `json:"detail,omitempty"`
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

	ErrPeriodLocked              = errorString("accounting period is closed or locked")
	ErrSoftCloseOverrideRequired = errorString("soft-close override required: elevated finance role and non-empty reason must be provided")
	ErrPeriodHardClosed          = errorString("accounting period is hard-closed and cannot be posted to")
	ErrCloseServiceUnavailable   = errorString("financial-close-svc unavailable")

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

	// ErrTrialBalanceNotFound is returned when a requested trial balance
	// snapshot id does not exist for the caller's tenant.
	ErrTrialBalanceNotFound = errorString("trial balance snapshot not found")

	ErrAccountNotFound       = errorString("account not found")
	ErrAccountAlreadyExists  = errorString("an account with this code already exists")
	ErrInvalidAccountType    = errorString("account_type must be one of ASSET, LIABILITY, EQUITY, REVENUE, EXPENSE")
	ErrParentAccountNotFound = errorString("parent_account_id does not name an existing account")
	ErrAccountInactive       = errorString("account is INACTIVE and may not be posted to")

	// ACC-01 versioned account master.
	ErrAccountVersionConflict       = errorString("account version conflict: expected_version does not match the current account version")
	ErrAccountEffectiveDate         = errorString("effective_from cannot precede the currently effective account version")
	ErrAccountHierarchyCycle        = errorString("reparent would create a cycle in the account hierarchy")
	ErrAccountHierarchyBackdated    = errorString("hierarchy change cannot be backdated before the current UTC accounting date")
	ErrAccountEffectiveDateRequired = errorString("as_of is required and must be an ISO date or RFC3339 timestamp")

	// ErrControlAccountPostingRestricted is invariant #7 enforced: a
	// control account with direct_posting_restricted=true was named on an
	// ordinary journal line with no override — the exact bypass the
	// invariant exists to prevent.
	ErrControlAccountPostingRestricted = errorString("account is a control account with direct posting restricted — an explicit, authorized override is required")

	ErrAccountMappingNotFound = errorString("no effective account mapping found for this key")
	// ErrMappingTargetAccountInvalid is returned when a mapping names an
	// account_code that either doesn't exist in the Chart of Accounts or
	// exists but is INACTIVE — ACC-02 must never map a business concept
	// onto an account that can't legitimately be posted to.
	ErrMappingTargetAccountInvalid = errorString("account_code does not name an existing ACTIVE account in the Chart of Accounts")

	ErrPostingExecutionNotFound = errorString("posting execution not found")

	// ErrPostingRuleAmbiguous is returned when a posting line names neither
	// or both of account_code/mapping_key, or a named mapping_key resolves
	// to no current mapping — the spec's own negative path, "Posting rule
	// ambiguity": ACC-04 must never guess which account a line posts to.
	ErrPostingRuleAmbiguous = errorString("posting rule is ambiguous: each line must name exactly one of account_code or mapping_key, and a mapping_key must resolve")

	ErrInvalidPostingTransition = errorString("posting execution is not in a status that allows this action")

	// ErrPostingAlreadyCommitted is returned by ReprocessFailedPosting when
	// the named execution already reached COMMITTED — a committed posting
	// consequence is permanent; reprocessing exists for FAILED/QUARANTINED
	// executions only, never to retry one that already succeeded.
	ErrPostingAlreadyCommitted = errorString("posting execution is already COMMITTED and cannot be reprocessed")

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

	// ── ACC-03 journal proposal/approval lifecycle ──────────────────────

	// ErrInvalidApprovalTransition covers every ACC-03 lifecycle command
	// called against a journal not in the one status it requires — the
	// spec's own negative paths #2 ("journal changed after approval") and
	// #4 ("attempt edit after posting") both resolve to this: there is no
	// status other than DRAFT/PENDING_APPROVAL from which AmendDraftJournal
	// succeeds.
	ErrInvalidApprovalTransition = errorString("journal is not in a status that allows this action")

	// ErrJournalUnbalancedAtSubmit is SubmitJournal's own check — the
	// spec's own negative path #1 ("debit/credit imbalance"), caught
	// before an approver's time is spent reviewing a proposal that could
	// never post.
	ErrJournalUnbalancedAtSubmit = errorString("journal debits and credits do not balance and cannot be submitted for approval")

	// ErrSelfApprovalNotPermitted is the spec's own negative path #3,
	// verbatim scenario: "Preparer self-approves protected journal." No
	// journal-class configuration service exists to say which journals are
	// "protected" (the spec's own word), so this v1 treats every journal as
	// protected — maker/checker applies universally, not selectively.
	ErrSelfApprovalNotPermitted = errorString("the principal who submitted this journal may not also approve it")

	ErrRejectReasonRequired = errorString("reason is required to reject a journal")

	// ErrCorrectionSourceNotPosted is RequestCorrection's own guard — the
	// spec's own state model, "corrections create new journals," presumes
	// there is a posted fact to correct; a draft or rejected journal has
	// nothing yet to correct.
	ErrCorrectionSourceNotPosted = errorString("only a POSTED journal may be corrected")

	// ── ACC-05 General Ledger ────────────────────────────────────────────

	// ErrLedgerScopeRequired is every ACC-05 query's own negative-path
	// guard against "Cross-book query leakage": legal_entity_id must
	// always be supplied and is never inferred or defaulted.
	ErrLedgerScopeRequired = errorString("legal_entity_id is required to query the ledger")
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
