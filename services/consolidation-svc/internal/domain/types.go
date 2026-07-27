package domain

import "time"

type ConsolidationRun struct {
	ConsolidationRunID string     `json:"consolidation_run_id"`
	TenantID           string     `json:"tenant_id"`
	GroupLegalEntityID string     `json:"group_legal_entity_id"`
	FiscalPeriod       string     `json:"fiscal_period"`
	TargetCurrency     string     `json:"target_currency"`
	Status             string     `json:"status"` // RUNNING, COMPLETED, FAILED
	ExceptionCount     int        `json:"exception_count"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

type BalanceSnapshot struct {
	BalanceSnapshotID   string    `json:"balance_snapshot_id"`
	TenantID            string    `json:"tenant_id"`
	ConsolidationRunID  string    `json:"consolidation_run_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	FiscalPeriod        string    `json:"fiscal_period"`
	AccountCode         string    `json:"account_code"`
	ConsolidatedBalance float64   `json:"consolidated_balance"`
	CurrencyCode        string    `json:"currency_code"`
	SnapshotSignature   string    `json:"snapshot_signature"`
	GeneratedAt         time.Time `json:"generated_at"`
}

type StartConsolidationRequest struct {
	GroupLegalEntityID  string   `json:"group_legal_entity_id"`
	ChildLegalEntityIDs []string `json:"child_legal_entity_ids"`
	FiscalPeriod        string   `json:"fiscal_period"`
	TargetCurrency      string   `json:"target_currency"`
}

type ConsolidationRunResponse struct {
	ConsolidationRunID string            `json:"consolidation_run_id"`
	GroupLegalEntityID string            `json:"group_legal_entity_id"`
	FiscalPeriod       string            `json:"fiscal_period"`
	Status             string            `json:"status"`
	ExceptionCount     int               `json:"exception_count"`
	StartedAt          time.Time         `json:"started_at"`
	Snapshots          []BalanceSnapshot `json:"snapshots,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRunNotFound             = errorString("consolidation run not found")
	ErrGLServiceUnavailable    = errorString("general-ledger-svc unavailable")
	ErrIntercompanyUnavailable = errorString("intercompany-accounting-svc unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for consolidation action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("consolidation store unavailable")
)