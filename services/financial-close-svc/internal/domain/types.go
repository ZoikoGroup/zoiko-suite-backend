package domain

import "time"

type FiscalPeriod struct {
	FiscalPeriodID     string     `json:"fiscal_period_id"`
	TenantID           string     `json:"tenant_id"`
	LegalEntityID      string     `json:"legal_entity_id"`
	PeriodName         string     `json:"period_name"`
	PeriodStart        time.Time  `json:"period_start"`
	PeriodEnd          time.Time  `json:"period_end"`
	CloseStatus        string     `json:"close_status"` // OPEN, CLOSED, LOCKED
	CloseLockedAt      *time.Time `json:"close_locked_at,omitempty"`
	EvidenceDocumentID *string    `json:"evidence_document_id,omitempty"`
}

type CloseEvidence struct {
	EvidenceID       string    `json:"evidence_id"`
	TenantID         string    `json:"tenant_id"`
	FiscalPeriodID   string    `json:"fiscal_period_id"`
	TrialBalanceHash string    `json:"trial_balance_hash"`
	Signature        string    `json:"signature"`
	GeneratedAt      time.Time `json:"generated_at"`
}

type PeriodCreateRequest struct {
	LegalEntityID string    `json:"legal_entity_id"`
	PeriodName    string    `json:"period_name"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
}

type PeriodLockResponse struct {
	FiscalPeriodID     string    `json:"fiscal_period_id"`
	PeriodName         string    `json:"period_name"`
	CloseStatus        string    `json:"close_status"`
	CloseLockedAt      time.Time `json:"close_locked_at"`
	EvidenceDocumentID string    `json:"evidence_document_id"`
	VerificationHash   string    `json:"verification_hash"`
}

type ReadinessCheckResponse struct {
	IsReady        bool     `json:"is_ready"`
	BlockingIssues []string `json:"blocking_issues"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrFiscalPeriodNotFound    = errorString("fiscal period not found")
	ErrPeriodAlreadyLocked     = errorString("fiscal period is already locked")
	ErrStoreUnavailable        = errorString("financial close store unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for financial close action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrReadinessChecksFailed   = errorString("period close blocked: unresolved balance sheet or ledger discrepancies")
	ErrGLServiceUnavailable    = errorString("general-ledger-svc unavailable")
	ErrAPServiceUnavailable    = errorString("accounts-payable-svc unavailable")
	ErrARServiceUnavailable    = errorString("accounts-receivable-svc unavailable")
	ErrVaultServiceUnavailable = errorString("document-vault-svc unavailable")

	// ErrLedgerPageTruncated is returned when the ledger answered with a full
	// page, so there may be journals this service never saw. A trial balance
	// compiled from a partial ledger is wrong, not merely incomplete, and this
	// one is about to be hashed and locked as the period's evidence — so the
	// close is refused rather than signed over an unknown.
	ErrLedgerPageTruncated = errorString("ledger returned a full page: the trial balance may be incomplete, so the close was refused")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope. Distinguished from a store outage so it answers 401 rather
	// than 503 — it is the caller's request that is wrong, not the database.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrEvidenceNotRecorded is returned when the period was locked but its
	// close evidence could not be written. See the handler for why this is
	// surfaced rather than logged and swallowed.
	ErrEvidenceNotRecorded = errorString("period locked but close evidence could not be recorded")
)
