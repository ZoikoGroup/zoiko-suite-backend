// Package domain holds financial-statements-svc's (REP-01, ZS-SVC-H-001 §4.1)
// own types: the FinancialStatementSet aggregate, its lifecycle, and the
// StatementFact line items it owns.
//
// Scope cuts from the spec, recorded here because they shape every type below
// (full reasoning in SPEC_DEVIATIONS.md):
//   - Only the Balance Sheet and Income Statement are produced. A real
//     indirect-method Cash Flow Statement needs opening AND closing balances
//     plus a classification engine; it is a materially separate feature, not
//     built in this pass.
//   - PresentationMappingUse is one fixed category -> statement-section map
//     (MappingVersionV1), plus, from MappingVersionV2, grouping of
//     same-category accounts by exact account_name into one line. Not a
//     per-tenant/per-framework versioned registry, and not hierarchy-based.
//   - DisclosureInstance (narrative disclosures) is not built: this version
//     produces primary statements only, no notes.
//   - ComparativePresentation is a two-period pairwise comparison only
//     (BuildComparative / GetComparatives) — no N-period trend, no
//     presentation formatting.
package domain

import "time"

// State is a FinancialStatementSet's lifecycle state.
type State string

const (
	StateDraft      State = "DRAFT"
	StateValidated  State = "VALIDATED"
	StateInReview   State = "IN_REVIEW"
	StateApproved   State = "APPROVED"
	StateCertified  State = "CERTIFIED"
	StatePublished  State = "PUBLISHED"
	StateSuperseded State = "SUPERSEDED"
)

func (s State) Valid() bool {
	switch s {
	case StateDraft, StateValidated, StateInReview, StateApproved, StateCertified, StatePublished, StateSuperseded:
		return true
	}
	return false
}

// Command is a named state-changing action (ZS-SVC-SPEC-001 style command set).
type Command string

const (
	CmdRefreshDraft    Command = "REFRESH_DRAFT"
	CmdValidate        Command = "VALIDATE_STATEMENTS"
	CmdSubmitForReview Command = "SUBMIT_FOR_REVIEW"
	CmdRequestChanges  Command = "REQUEST_CHANGES"
	CmdApprove         Command = "APPROVE_STATEMENT_SET"
	CmdCertify         Command = "CERTIFY_STATEMENT_SET"
	CmdPublish         Command = "PUBLISH_STATEMENT_SET"
)

// MappingVersionV1 is the one fixed category -> statement-section mapping
// this version ships. A real PresentationMappingUse (versioned per tenant,
// framework and jurisdiction) is not built — see the package doc comment.
const MappingVersionV1 = "v1-fixed-category-map"

// MappingVersionV2 is MappingVersionV1's category -> statement map plus one
// grouping rule: accounts on the same statement, in the same category and
// with exactly the same non-empty account_name are reported as ONE line,
// their balances summed (SPEC_DEVIATIONS A3/A4). It is still not a real
// PresentationMappingUse — the snapshot carries no account hierarchy, so a
// shared name is the only grouping signal this service receives.
const MappingVersionV2 = "v2-category-map-name-aggregation"

// MappingVersionCurrent is the mapping new statement sets are built with.
// A set keeps the version it was created with for its whole life, so a
// draft or certified set is never re-presented under a mapping it was not
// built with.
const MappingVersionCurrent = MappingVersionV2

// AccountCategory is the account classification carried in a snapshot's
// content (see SnapshotContent). Deliberately the same five values
// general-ledger-svc's chart of accounts uses (ASSET/LIABILITY/EQUITY/
// REVENUE/EXPENSE) — one vocabulary for "what kind of account is this"
// platform-wide, not a second one invented here.
type AccountCategory string

const (
	CategoryAsset     AccountCategory = "ASSET"
	CategoryLiability AccountCategory = "LIABILITY"
	CategoryEquity    AccountCategory = "EQUITY"
	CategoryRevenue   AccountCategory = "REVENUE"
	CategoryExpense   AccountCategory = "EXPENSE"
)

func (c AccountCategory) valid() bool {
	switch c {
	case CategoryAsset, CategoryLiability, CategoryEquity, CategoryRevenue, CategoryExpense:
		return true
	}
	return false
}

// StatementType names which primary statement a StatementFact belongs to.
type StatementType string

const (
	StatementBalanceSheet    StatementType = "BALANCE_SHEET"
	StatementIncomeStatement StatementType = "INCOME_STATEMENT"
)

// categoryMapping is MappingVersionV1: which statement a category's balance
// is reported on. There is deliberately no per-account override table.
var categoryMapping = map[AccountCategory]StatementType{
	CategoryAsset:     StatementBalanceSheet,
	CategoryLiability: StatementBalanceSheet,
	CategoryEquity:    StatementBalanceSheet,
	CategoryRevenue:   StatementIncomeStatement,
	CategoryExpense:   StatementIncomeStatement,
}

// StatementFor reports which statement an account category belongs on.
func StatementFor(c AccountCategory) (StatementType, bool) {
	t, ok := categoryMapping[c]
	return t, ok
}

// FinancialStatementSet is the aggregate root: one certified-or-in-progress
// set of financial statements for one legal entity, reporting period and
// source snapshot.
type FinancialStatementSet struct {
	StatementSetID       string   `json:"statement_set_id"`
	TenantID             string   `json:"tenant_id"`
	LegalEntityID        string   `json:"legal_entity_id"`
	ReportingPeriod      string   `json:"reporting_period"`
	Framework            string   `json:"framework"`
	PresentationCurrency string   `json:"presentation_currency"`
	SourceSnapshotID     string   `json:"source_snapshot_id"`
	SourceSnapshotHash   string   `json:"source_snapshot_hash"`
	MappingVersion       string   `json:"mapping_version"`
	Status               State    `json:"status"`
	Version              int64    `json:"version"`
	ValidationErrors     []string `json:"validation_errors,omitempty"`

	SupersedesSetID   *string `json:"supersedes_set_id,omitempty"`
	SupersededBySetID *string `json:"superseded_by_set_id,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`

	LastRefreshedAt            *time.Time `json:"last_refreshed_at,omitempty"`
	LastRefreshedByPrincipalID *string    `json:"last_refreshed_by_principal_id,omitempty"`

	ValidatedAt *time.Time `json:"validated_at,omitempty"`

	SubmittedForReviewAt            *time.Time `json:"submitted_for_review_at,omitempty"`
	SubmittedForReviewByPrincipalID *string    `json:"submitted_for_review_by_principal_id,omitempty"`

	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`

	CertifiedAt            *time.Time `json:"certified_at,omitempty"`
	CertifiedByPrincipalID *string    `json:"certified_by_principal_id,omitempty"`
	CertificationReason    *string    `json:"certification_reason,omitempty"`

	PublishedAt *time.Time `json:"published_at,omitempty"`

	SupersededAt *time.Time `json:"superseded_at,omitempty"`
}

// StatementFact is one reported line in one statement set: one source
// account under MappingVersionV1, or every same-category account sharing an
// exact account_name under MappingVersionV2 (see SourceAccounts).
type StatementFact struct {
	FactID         string          `json:"fact_id"`
	TenantID       string          `json:"tenant_id"`
	StatementSetID string          `json:"statement_set_id"`
	StatementType  StatementType   `json:"statement_type"`
	Category       AccountCategory `json:"category"`
	AccountCode    string          `json:"account_code"`
	AccountName    string          `json:"account_name"`
	// Amount is an exact decimal string (ZS-DATA-001 D06: binary floating
	// point is prohibited for monetary values) — never a Go float.
	Amount string `json:"amount"`
	// SourceAccounts lists every snapshot account summed into this line,
	// with each one's own balance exactly as the snapshot stated it. Empty
	// for facts built before aggregation existed (and for MappingVersionV1,
	// where a line IS its one account) — use Sources(), never this field
	// directly, to answer "which accounts make up this number".
	SourceAccounts []SnapshotContentAccount `json:"source_accounts,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
}

// Sources reports the source accounts behind a fact: SourceAccounts when
// recorded, otherwise the fact's own single account.
func (f StatementFact) Sources() []SnapshotContentAccount {
	if len(f.SourceAccounts) > 0 {
		return f.SourceAccounts
	}
	return []SnapshotContentAccount{{AccountCode: f.AccountCode, AccountName: f.AccountName, Category: f.Category, Balance: f.Amount}}
}

// ComparativeFact pairs one reported line across two periods. Under
// MappingVersionV1 a line is one account; under MappingVersionV2 it is
// every same-category account sharing one account_name. A side is nil when
// the line appears in only one period's facts — a new or discontinued line
// is reported with an explicit gap, never silently dropped.
type ComparativeFact struct {
	// AccountCode is the line's representative code (under V2, its lowest
	// source account code; the current period's when both sides exist).
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name"`
	Category    AccountCategory `json:"category"`
	// PriorCategory is set only when the line was classified differently
	// in the prior period (a reclassification within the same statement), so
	// that change is visible rather than hidden behind one category value.
	PriorCategory *AccountCategory `json:"prior_category,omitempty"`

	// Current/PriorSourceAccountCodes name the accounts summed into each
	// side. Under V2 a line's composition can change between periods (an
	// account added to "Cash"), which moves the number without any balance
	// changing — so the composition is shown, not just the totals.
	CurrentSourceAccountCodes []string `json:"current_source_account_codes,omitempty"`
	PriorSourceAccountCodes   []string `json:"prior_source_account_codes,omitempty"`

	CurrentFactID *string `json:"current_fact_id"`
	PriorFactID   *string `json:"prior_fact_id"`
	// Amounts are exact decimal strings (ZS-DATA-001 D06), never floats.
	CurrentAmount *string `json:"current_amount"`
	PriorAmount   *string `json:"prior_amount"`
	// Change is CurrentAmount - PriorAmount, computed with big.Rat. It is
	// nil when either side is missing: an absent line is not the same thing
	// as a zero balance, so no change is invented for it.
	Change *string `json:"change"`
}

// ComparativeStatement is one primary statement's paired lines, ordered by
// representative account code.
type ComparativeStatement struct {
	StatementType StatementType     `json:"statement_type"`
	Lines         []ComparativeFact `json:"lines"`
}

// ComparativeSetRef identifies one side of a comparison.
type ComparativeSetRef struct {
	StatementSetID     string `json:"statement_set_id"`
	ReportingPeriod    string `json:"reporting_period"`
	Status             State  `json:"status"`
	SourceSnapshotID   string `json:"source_snapshot_id"`
	SourceSnapshotHash string `json:"source_snapshot_hash"`
}

// ComparativeStatementView is GetComparatives's response: a two-period
// pairwise comparison of one set against a certified prior set.
type ComparativeStatementView struct {
	LegalEntityID        string                 `json:"legal_entity_id"`
	Framework            string                 `json:"framework"`
	PresentationCurrency string                 `json:"presentation_currency"`
	MappingVersion       string                 `json:"mapping_version"`
	Current              ComparativeSetRef      `json:"current"`
	Prior                ComparativeSetRef      `json:"prior"`
	Statements           []ComparativeStatement `json:"statements"`
}

// RefFor builds a ComparativeSetRef from a set.
func RefFor(s *FinancialStatementSet) ComparativeSetRef {
	return ComparativeSetRef{
		StatementSetID:     s.StatementSetID,
		ReportingPeriod:    s.ReportingPeriod,
		Status:             s.Status,
		SourceSnapshotID:   s.SourceSnapshotID,
		SourceSnapshotHash: s.SourceSnapshotHash,
	}
}

// SnapshotContent is the JSON schema this service requires inside an ACC-16
// FinancialSnapshot's Content field. ACC-16 itself treats Content as an
// opaque string with no defined schema (see SPEC_DEVIATIONS.md item A1) — no
// producer in the platform emits this shape yet. This struct IS that schema,
// defined here because nothing else defines it.
type SnapshotContent struct {
	Currency string                   `json:"currency"`
	Accounts []SnapshotContentAccount `json:"accounts"`
}

type SnapshotContentAccount struct {
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name"`
	Category    AccountCategory `json:"category"`
	// Balance is an exact decimal string, never a float (same D06 rule).
	Balance string `json:"balance"`
}

// Validate checks the snapshot content is well-formed enough to build
// statement facts from. It does not check the tie-out (Assets = Liabilities
// + Equity); that is ValidateStatements's job, over the facts this produces.
func (c SnapshotContent) Validate() error {
	if c.Currency == "" {
		return Errf(CodeValidationFailed, "snapshot content: currency is required")
	}
	if len(c.Accounts) == 0 {
		return Errf(CodeValidationFailed, "snapshot content: at least one account is required")
	}
	seen := make(map[string]bool, len(c.Accounts))
	for i, a := range c.Accounts {
		if a.AccountCode == "" {
			return Errf(CodeValidationFailed, "snapshot content: account %d has no account_code", i)
		}
		if seen[a.AccountCode] {
			return Errf(CodeValidationFailed, "snapshot content: duplicate account_code %q", a.AccountCode)
		}
		seen[a.AccountCode] = true
		if !a.Category.valid() {
			return Errf(CodeValidationFailed, "snapshot content: account %q has invalid category %q", a.AccountCode, a.Category)
		}
		if _, err := ParseAmount(a.Balance); err != nil {
			return Errf(CodeValidationFailed, "snapshot content: account %q has invalid balance %q: %v", a.AccountCode, a.Balance, err)
		}
	}
	return nil
}

var (
	ErrStatementSetNotFound = errorString("financial statement set not found")
	ErrFactNotFound         = errorString("statement fact not found")
)

// Preparer is whoever most recently shaped a set's content: the last
// refresher, or the creator if it has never been refreshed. SoD checks
// compare against this, not just CreatedByPrincipalID, because refreshing a
// draft with a new snapshot is itself authorship (ZS-SVC-H-001 §4.1:
// "Preparer cannot independently certify material statutory statements").
func (s *FinancialStatementSet) Preparer() string {
	if s.LastRefreshedByPrincipalID != nil && *s.LastRefreshedByPrincipalID != "" {
		return *s.LastRefreshedByPrincipalID
	}
	return s.CreatedByPrincipalID
}
