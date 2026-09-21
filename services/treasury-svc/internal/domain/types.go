package domain

import "time"

// BankAccount represents a registered bank account — BNK-01's own
// identity/ownership/verification/operational-status model, extending
// this pre-existing type in place rather than a new service.
type BankAccount struct {
	BankAccountID       string `json:"bank_account_id"`
	TenantID            string `json:"tenant_id"`
	LegalEntityID       string `json:"legal_entity_id"`
	AccountName         string `json:"account_name"`
	MaskedAccountNumber string `json:"masked_account_number"`
	BankIdentifier      string `json:"bank_identifier"`
	CurrencyCode        string `json:"currency_code"`
	AccountStatus       string `json:"account_status"`

	BranchRef               string `json:"branch_ref"`
	Country                 string `json:"country"`
	AccountType             string `json:"account_type"`
	RequestedOperationalUse string `json:"requested_operational_use"`
	TokenVersion            int    `json:"token_version"`
	CreatedByPrincipalID    string `json:"created_by_principal_id,omitempty"`
	CorrelationID           string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BNK-01 operational-status lifecycle. account_status keeps its original
// three values (ACTIVE/SUSPENDED/CLOSED) as the default path for backward
// compatibility with every existing caller of RegisterBankAccount — see
// migration 000003's own doc comment for why the create-time default was
// deliberately NOT changed to PENDING_VERIFICATION. DRAFT/
// PENDING_VERIFICATION are valid states a caller may explicitly use.
const (
	BankAccountDraft               = "DRAFT"
	BankAccountPendingVerification = "PENDING_VERIFICATION"
	BankAccountActive              = "ACTIVE"
	BankAccountSuspended           = "SUSPENDED"
	BankAccountClosed              = "CLOSED"
)

func CanAmendMetadata(status string) bool {
	return status == BankAccountDraft || status == BankAccountPendingVerification || status == BankAccountActive || status == BankAccountSuspended
}
func CanChangeOperationalUse(status string) bool { return CanAmendMetadata(status) }
func CanSuspendAccount(status string) bool       { return status == BankAccountActive }
func CanReactivateAccount(status string) bool    { return status == BankAccountSuspended }
func CanCloseAccount(status string) bool {
	return status == BankAccountDraft || status == BankAccountPendingVerification || status == BankAccountActive || status == BankAccountSuspended
}
func CanRotateAccountToken(status string) bool { return status != BankAccountClosed }

// OwnershipEvidence is orthogonal to AccountStatus — append-only,
// superseded by a new row on re-verification, never edited in place. See
// migration 000003's own doc comment: IsOwnershipVerified is a derived
// fact (does at least one non-superseded row exist), never a column.
type OwnershipEvidence struct {
	EvidenceID            string    `json:"evidence_id"`
	BankAccountID         string    `json:"bank_account_id"`
	TenantID              string    `json:"tenant_id"`
	VerificationMethod    string    `json:"verification_method"`
	EvidenceRef           string    `json:"evidence_ref"`
	VerifiedByPrincipalID string    `json:"verified_by_principal_id"`
	VerifiedAt            time.Time `json:"verified_at"`
	SupersededBy          *string   `json:"superseded_by,omitempty"`
}

// AccountHistoryEntry is one append-only snapshot of a bank account's
// mutable fields at the moment a BNK-01 command changed them — see
// migration 000006's own doc comment. Mirrors OwnershipEvidence's
// superseded-by shape: never edited in place, only ever superseded by the
// next entry.
type AccountHistoryEntry struct {
	HistoryID               string    `json:"history_id"`
	BankAccountID            string    `json:"bank_account_id"`
	TenantID                 string    `json:"tenant_id"`
	AccountName              string    `json:"account_name"`
	MaskedAccountNumber      string    `json:"masked_account_number"`
	BankIdentifier            string    `json:"bank_identifier"`
	AccountStatus              string    `json:"account_status"`
	BranchRef                    string    `json:"branch_ref"`
	Country                       string    `json:"country"`
	AccountType                   string    `json:"account_type"`
	RequestedOperationalUse       string    `json:"requested_operational_use"`
	TokenVersion                   int       `json:"token_version"`
	ChangedByPrincipalID       string    `json:"changed_by_principal_id"`
	EffectiveAt                  time.Time `json:"effective_at"`
	SupersededBy                  *string   `json:"superseded_by,omitempty"`
}

// ConnectionOption is one BNK-02 connection reported for a bank account
// via ListConnectionOptions (Wave 10d) — read-only data this service
// never owns; sourced live from banking-connector-svc, never persisted
// here.
type ConnectionOption struct {
	ConnectionID string   `json:"connection_id"`
	ProviderRef  string   `json:"provider_ref"`
	Status       string   `json:"status"`
	HealthStatus string   `json:"health_status"`
	Region       string   `json:"region"`
	Currency     string   `json:"currency"`
	ConsentScope []string `json:"consent_scope"`
}

// ── BNK-01 command params ───────────────────────────────────────────────────

type VerifyOwnershipParams struct {
	BankAccountID, TenantID         string
	VerificationMethod, EvidenceRef string
	VerifiedByPrincipalID           string
}

type AmendBankAccountMetadataParams struct {
	BankAccountID, TenantID                                      string
	AccountName, BranchRef, BankIdentifier, Country, AccountType string
	ActorPrincipalID                                             string
}

type ChangeOperationalUseParams struct {
	BankAccountID, TenantID, RequestedOperationalUse, ActorPrincipalID string
}

type SuspendAccountParams struct {
	BankAccountID, TenantID, Reason, ActorPrincipalID string
}

type ReactivateAccountParams struct {
	BankAccountID, TenantID, ActorPrincipalID string
}

type CloseAccountParams struct {
	BankAccountID, TenantID, Reason, ActorPrincipalID string
}

type RotateAccountTokenParams struct {
	BankAccountID, TenantID                   string
	NewMaskedAccountNumber, NewBankIdentifier string
	ActorPrincipalID                          string
}

// CashBalance represents a balance snapshot for a bank account.
type CashBalance struct {
	BalanceID        string    `json:"balance_id"`
	TenantID         string    `json:"tenant_id"`
	BankAccountID    string    `json:"bank_account_id"`
	LedgerBalance    float64   `json:"ledger_balance"`
	AvailableBalance float64   `json:"available_balance"`
	AsOfTimestamp    time.Time `json:"as_of_timestamp"`
	CorrelationID    string    `json:"correlation_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// LiquidityThreshold represents a minimum required balance policy.
type LiquidityThreshold struct {
	ThresholdID            string    `json:"threshold_id"`
	TenantID               string    `json:"tenant_id"`
	LegalEntityID          string    `json:"legal_entity_id"`
	CurrencyCode           string    `json:"currency_code"`
	MinimumRequiredBalance float64   `json:"minimum_required_balance"`
	EscalationEmail        string    `json:"escalation_email"`
	CreatedAt              time.Time `json:"created_at"`
}

// RegisterBankAccountRequest input model. BranchRef/Country/AccountType/
// RequestedOperationalUse/CorrelationID are additive (BNK-01) fields —
// all optional so every existing caller's request body keeps working
// unchanged.
type RegisterBankAccountRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	AccountName         string `json:"account_name"`
	MaskedAccountNumber string `json:"masked_account_number"`
	BankIdentifier      string `json:"bank_identifier"`
	CurrencyCode        string `json:"currency_code"`

	BranchRef               string `json:"branch_ref,omitempty"`
	Country                 string `json:"country,omitempty"`
	AccountType             string `json:"account_type,omitempty"`
	RequestedOperationalUse string `json:"requested_operational_use,omitempty"`
	CorrelationID           string `json:"correlation_id,omitempty"`
}

// SetThresholdRequest input model.
type SetThresholdRequest struct {
	LegalEntityID          string  `json:"legal_entity_id"`
	CurrencyCode           string  `json:"currency_code"`
	MinimumRequiredBalance float64 `json:"minimum_required_balance"`
	EscalationEmail        string  `json:"escalation_email"`
}

// CashPositionResponse output model.
type CashPositionResponse struct {
	BankAccountID    string    `json:"bank_account_id"`
	AccountName      string    `json:"account_name"`
	CurrencyCode     string    `json:"currency_code"`
	LedgerBalance    float64   `json:"ledger_balance"`
	AvailableBalance float64   `json:"available_balance"`
	AsOfTimestamp    time.Time `json:"as_of_timestamp"`
}

// EffectiveCashResponse output model.
//
// BNK-08's own "never show stale balance as current" rule: Components
// carries a per-source freshness record so a caller (or this handler
// itself) can tell WHICH figure might be old, not just a single
// top-level AsOfTimestamp that silently blends a fresh live query
// (AP/obligations, always as-of-now — see ComponentFreshness's own doc
// comment) with a bank balance that may be hours or days stale.
type EffectiveCashResponse struct {
	TenantID                 string                `json:"tenant_id"`
	LegalEntityID            string                `json:"legal_entity_id"`
	CurrencyCode             string                `json:"currency_code"`
	CurrentBankBalance       float64               `json:"current_bank_balance"`
	PendingAPCommitments     float64               `json:"pending_ap_commitments"`
	PayrollObligations       float64               `json:"payroll_obligations"`
	TaxLiabilities           float64               `json:"tax_liabilities"`
	ReservedPendingApprovals float64               `json:"reserved_pending_approvals"`
	EffectiveAvailableCash   float64               `json:"effective_available_cash"`
	AsOfTimestamp            time.Time             `json:"as_of_timestamp"`
	ThresholdDetails         *ThresholdAlertDetail `json:"threshold_details,omitempty"`

	Components        []ComponentFreshness `json:"components"`
	HasStaleComponent bool                 `json:"has_stale_component"`
}

// ComponentFreshness records one composed figure's own as-of-time and
// staleness verdict. AP/obligations are synchronous live queries against
// their owning service — they are always "as of now" by construction, so
// their StalenessThresholdSeconds is 0 and IsStale is always false. Only
// CurrentBankBalance carries genuine staleness risk: it is a snapshot
// (cash_balances.as_of_timestamp) that nothing guarantees is recent.
type ComponentFreshness struct {
	Component                 string    `json:"component"`
	AsOfTimestamp             time.Time `json:"as_of_timestamp"`
	StalenessThresholdSeconds int       `json:"staleness_threshold_seconds"`
	IsStale                   bool      `json:"is_stale"`
}

// BankBalanceStalenessThreshold is the default staleness window for the
// bank-balance component of EffectiveCashResponse — a snapshot older than
// this is not "current" per the doc's own rule, and GetEffectiveCash
// fails closed (503) rather than presenting it as such.
const BankBalanceStalenessThreshold = 24 * time.Hour

// ThresholdAlertDetail output sub-model.
type ThresholdAlertDetail struct {
	MinimumRequiredBalance float64 `json:"minimum_required_balance"`
	IsBreached             bool    `json:"is_breached"`
}

// InitiateTransferRequest input model.
type InitiateTransferRequest struct {
	SourceBankAccountID string  `json:"source_bank_account_id"`
	TargetBankAccountID string  `json:"target_bank_account_id"`
	Amount              float64 `json:"amount"`
	CurrencyCode        string  `json:"currency_code"`
	CorrelationID       string  `json:"correlation_id"`
	// SaveAsDraft creates the transfer in DRAFT instead of the default
	// PENDING_APPROVAL — see CreateTreasuryTransferParams.SaveAsDraft.
	SaveAsDraft bool `json:"save_as_draft,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrBankAccountNotFound     = errorString("bank account not found")
	ErrThresholdNotFound       = errorString("liquidity threshold not found")
	ErrInvalidAmount           = errorString("invalid transfer or balance amount")
	ErrStoreUnavailable        = errorString("treasury store unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for this treasury action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrMinimumBalanceBreach    = errorString("action blocked: available cash drops below minimum required threshold")
	ErrAPServiceUnavailable    = errorString("accounts-payable-svc unavailable")
	ErrARServiceUnavailable    = errorString("accounts-receivable-svc unavailable")
	ErrObligationsUnavailable  = errorString("obligations-svc unavailable")

	// ErrInvalidTransition is BNK-01's own CAS-mismatch sentinel — the
	// account exists but isn't in a state that permits the attempted
	// command (e.g. suspending a DRAFT account, or amending a CLOSED one).
	ErrInvalidTransition = errorString("bank account is not in a state that permits this action")

	// ErrSelfVerificationForbidden is BNK-01's maker-checker rule: the
	// principal who registered a bank account cannot also be the one who
	// verifies its ownership — mirrors BNK-09's ErrTransferSelfApproval.
	ErrSelfVerificationForbidden = errorString("the principal who created this bank account cannot also verify its ownership")
)

type ExpectedCashFlow struct {
	Amount   float64   `json:"amount"`
	DueDate  time.Time `json:"due_date"`
	Category string    `json:"category"` // RECEIVABLE, PAYABLE, OBLIGATION
}

type LiquidityForecastResponse struct {
	TenantID           string                 `json:"tenant_id"`
	LegalEntityID      string                 `json:"legal_entity_id"`
	CurrencyCode       string                 `json:"currency_code"`
	CurrentCashBalance float64                `json:"current_cash_balance"`
	AsOfTimestamp      time.Time              `json:"as_of_timestamp"`
	Forecast7Day       ForecastIntervalDetail `json:"forecast_7_day"`
	Forecast30Day      ForecastIntervalDetail `json:"forecast_30_day"`
	Forecast90Day      ForecastIntervalDetail `json:"forecast_90_day"`
}

type ForecastIntervalDetail struct {
	IntervalDays      int     `json:"interval_days"`
	ExpectedInflows   float64 `json:"expected_inflows"`
	ExpectedOutflows  float64 `json:"expected_outflows"`
	ForecastedBalance float64 `json:"forecasted_balance"`
}
