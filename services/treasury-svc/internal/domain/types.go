package domain

import "time"

// BankAccount represents a registered bank account.
type BankAccount struct {
	BankAccountID       string    `json:"bank_account_id"`
	TenantID            string    `json:"tenant_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	AccountName         string    `json:"account_name"`
	MaskedAccountNumber string    `json:"masked_account_number"`
	BankIdentifier      string    `json:"bank_identifier"`
	CurrencyCode        string    `json:"currency_code"`
	AccountStatus       string    `json:"account_status"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
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

// RegisterBankAccountRequest input model.
type RegisterBankAccountRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	AccountName         string `json:"account_name"`
	MaskedAccountNumber string `json:"masked_account_number"`
	BankIdentifier      string `json:"bank_identifier"`
	CurrencyCode        string `json:"currency_code"`
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
type EffectiveCashResponse struct {
	TenantID                 string               `json:"tenant_id"`
	LegalEntityID            string               `json:"legal_entity_id"`
	CurrencyCode             string               `json:"currency_code"`
	CurrentBankBalance       float64              `json:"current_bank_balance"`
	PendingAPCommitments     float64              `json:"pending_ap_commitments"`
	PayrollObligations       float64              `json:"payroll_obligations"`
	TaxLiabilities           float64              `json:"tax_liabilities"`
	ReservedPendingApprovals float64              `json:"reserved_pending_approvals"`
	EffectiveAvailableCash   float64              `json:"effective_available_cash"`
	AsOfTimestamp            time.Time            `json:"as_of_timestamp"`
	ThresholdDetails         *ThresholdAlertDetail `json:"threshold_details,omitempty"`
}

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
