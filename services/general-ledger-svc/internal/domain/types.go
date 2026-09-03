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

// JournalHeader is one journal entry moving through the Tri-Phase Commit
// lifecycle. Entity-bound (LegalEntityID), never hard-deleted.
type JournalHeader struct {
	JournalID     string        `json:"journal_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	FiscalPeriod  string        `json:"fiscal_period"` // e.g. "2026-07" — no Fiscal Calendar service exists yet, plain string reference
	Status        JournalStatus `json:"status"`

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
type TrialBalanceLine struct {
	AccountCode string  `json:"account_code"`
	NetBalance  float64 `json:"net_balance"`
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
}

type CreateAccountRequest struct {
	AccountCode             string      `json:"account_code"`
	AccountName             string      `json:"account_name"`
	AccountType             AccountType `json:"account_type"`
	ParentAccountID         *string     `json:"parent_account_id,omitempty"`
	IsControlAccount        bool        `json:"is_control_account,omitempty"`
	DirectPostingRestricted bool        `json:"direct_posting_restricted,omitempty"`
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
}

type CreateJournalRequest struct {
	TenantID      string                   `json:"tenant_id"`
	LegalEntityID string                   `json:"legal_entity_id"`
	FiscalPeriod  string                   `json:"fiscal_period"`
	Description   string                   `json:"description"`
	Lines         []CreateJournalLineInput `json:"lines"`
	CorrelationID string                   `json:"correlation_id"`

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

	// ErrTrialBalanceNotFound is returned when a requested trial balance
	// snapshot id does not exist for the caller's tenant.
	ErrTrialBalanceNotFound = errorString("trial balance snapshot not found")

	ErrAccountNotFound       = errorString("account not found")
	ErrAccountAlreadyExists  = errorString("an account with this code already exists")
	ErrInvalidAccountType    = errorString("account_type must be one of ASSET, LIABILITY, EQUITY, REVENUE, EXPENSE")
	ErrParentAccountNotFound = errorString("parent_account_id does not name an existing account")
	ErrAccountInactive       = errorString("account is INACTIVE and may not be posted to")

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
)
