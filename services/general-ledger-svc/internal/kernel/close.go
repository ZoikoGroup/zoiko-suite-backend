package kernel

import (
	"context"
	"time"
)

// Period seals (standard §10, §36): the close watermark. Sealing a period records the highest ledger entry the period's entity
// had at that moment and, from then on, every posting transaction refuses to write into the period. The seal is taken under an
// exclusive lock that waits for postings already in flight, so nothing can commit into the period after the watermark.

// Seal actions.
const (
	SealActionSeal   = "SEAL"
	SealActionReopen = "REOPEN"
	SealActionReseal = "RESEAL"
)

// PeriodSeal is the state of one period's seal.
type PeriodSeal struct {
	LegalEntityID   string     `json:"legal_entity_id"`
	FiscalPeriod    string     `json:"fiscal_period"`
	Status          string     `json:"status"` // SEALED or REOPENED
	Watermark       int64      `json:"ledger_watermark"`
	SealedBy        string     `json:"sealed_by"`
	SealedAt        time.Time  `json:"sealed_at"`
	ReopenedBy      string     `json:"reopened_by,omitempty"`
	ReopenedAt      *time.Time `json:"reopened_at,omitempty"`
	ReopenAuthority string     `json:"reopen_authority,omitempty"`
	ReopenReason    string     `json:"reopen_reason,omitempty"`
	SealCount       int        `json:"seal_count"`
	// EntriesSinceReopen is how many ledger entries were written between a reopen and the re-seal (the activity of the reopen window).
	EntriesSinceReopen int64 `json:"entries_since_reopen,omitempty"`
	Existing           bool  `json:"existing,omitempty"`
}

// SealEvent is one entry of a period's seal history.
type SealEvent struct {
	Kind               string    `json:"kind"`
	Watermark          int64     `json:"ledger_watermark"`
	Actor              string    `json:"actor"`
	AuthorityRef       string    `json:"authority_ref,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	EntriesSinceReopen *int64    `json:"entries_since_reopen,omitempty"`
	At                 time.Time `json:"at"`
}

// SealRepo is the persistence boundary of period seals.
type SealRepo interface {
	ApplySeal(ctx context.Context, tenant, entity, period, action, actor, authorityRef, reason string, at time.Time) (*PeriodSeal, error)
	GetSeal(ctx context.Context, tenant, entity, period string) (*PeriodSeal, []SealEvent, error)
	ListSeals(ctx context.Context, tenant, entity string) ([]PeriodSeal, error)
}
