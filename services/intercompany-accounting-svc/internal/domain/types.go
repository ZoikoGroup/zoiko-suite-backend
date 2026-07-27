package domain

import "time"

type IntercompanyEntry struct {
	IntercompanyEntryID string    `json:"intercompany_entry_id"`
	TenantID            string    `json:"tenant_id"`
	SourceLegalEntityID string    `json:"source_legal_entity_id"`
	TargetLegalEntityID string    `json:"target_legal_entity_id"`
	SourceJournalID     string    `json:"source_journal_id"`
	TargetJournalID     *string   `json:"target_journal_id,omitempty"`
	Amount              float64   `json:"amount"`
	CurrencyCode        string    `json:"currency_code"`
	MatchStatus         string    `json:"match_status"` // UNMATCHED, MATCHED, MISMATCH
	MismatchReason      *string   `json:"mismatch_reason,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type CreateEntryRequest struct {
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	Amount              float64 `json:"amount"`
	CurrencyCode        string  `json:"currency_code"`
}

type MatchEntryRequest struct {
	TargetJournalID string `json:"target_journal_id"`
}

type MatchEntryResponse struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	MatchStatus         string  `json:"match_status"`
	MismatchReason      *string `json:"mismatch_reason,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrEntryNotFound            = errorString("intercompany entry not found")
	ErrInvalidAmount            = errorString("intercompany entry amount must be greater than zero")
	ErrSameEntityForbidden      = errorString("source and target legal entities must be different")
	ErrEntryAlreadyMatched      = errorString("intercompany entry is already matched")
	ErrGLServiceUnavailable     = errorString("general-ledger-svc unavailable")
	ErrAmountMismatch           = errorString("journal amount or entity discrepancy detected")
	ErrAuthorizationDenied      = errorString("authorization denied for intercompany accounting action")
	ErrAuthzServiceUnavailable  = errorString("authorization-svc unavailable")
	ErrIdentityMissing          = errorString("caller identity missing")
	ErrStoreUnavailable         = errorString("intercompany store unavailable")
)
