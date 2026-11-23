package domain

import (
	"fmt"
	"strings"
	"time"
)

// ManualMatch is a human-asserted pairing of records a run's automatic
// matching left unpaired (SPEC_DEVIATIONS A5; ZS-CONTROL-001 §10 "Manual
// match: requires permission, reason and evidence; cannot hide amount
// difference"). It is kept in its own table, not MatchResult: a manual
// match has no algorithm version to record (Invariant 16 requires an
// automated control to prove the algorithm that produced its result), and
// recording it as one would misrepresent a human decision as an algorithmic
// one.
//
// Not enforced: §27's "blocked if own-transaction rule applies". No
// population record this service receives carries who created or posted it
// (control-population-contract.md defines no such field), so there is no
// input to enforce that rule against — the same class of gap as A8
// (cross-currency, blocked on an FX-rate capability this service lacks).
type ManualMatch struct {
	ManualMatchID  string    `json:"manual_match_id"`
	TenantID       string    `json:"tenant_id"`
	RunID          string    `json:"run_id"`
	SideARecordIDs []string  `json:"side_a_record_ids"`
	SideBRecordIDs []string  `json:"side_b_record_ids"`
	AmountA        string    `json:"amount_a"`
	AmountB        string    `json:"amount_b"`
	Difference     string    `json:"difference"` // A - B, exact decimal; never hidden
	Currency       string    `json:"currency"`
	Reason         string    `json:"reason"`
	EvidenceRef    string    `json:"evidence_ref"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

type CreateManualMatchRequest struct {
	SideARecordIDs []string `json:"side_a_record_ids"`
	SideBRecordIDs []string `json:"side_b_record_ids"`
	Reason         string   `json:"reason"`
	EvidenceRef    string   `json:"evidence_ref"`
}

const (
	maxManualMatchReasonLen      = 2000
	maxManualMatchEvidenceRefLen = 500
	maxManualMatchRecordIDs      = 100
)

func (r *CreateManualMatchRequest) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidArgument, msg) }
	if len(r.SideARecordIDs) == 0 {
		return bad("side_a_record_ids is required")
	}
	if len(r.SideBRecordIDs) == 0 {
		return bad("side_b_record_ids is required")
	}
	if len(r.SideARecordIDs) > maxManualMatchRecordIDs || len(r.SideBRecordIDs) > maxManualMatchRecordIDs {
		return bad(fmt.Sprintf("at most %d record ids per side", maxManualMatchRecordIDs))
	}
	for _, id := range r.SideARecordIDs {
		if strings.TrimSpace(id) == "" {
			return bad("side_a_record_ids cannot contain a blank id")
		}
	}
	for _, id := range r.SideBRecordIDs {
		if strings.TrimSpace(id) == "" {
			return bad("side_b_record_ids cannot contain a blank id")
		}
	}
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > maxManualMatchReasonLen {
		return bad(fmt.Sprintf("reason is required (§10/§27), at most %d characters", maxManualMatchReasonLen))
	}
	if strings.TrimSpace(r.EvidenceRef) == "" || len(r.EvidenceRef) > maxManualMatchEvidenceRefLen {
		return bad(fmt.Sprintf("evidence_ref is required (§10/§27), at most %d characters", maxManualMatchEvidenceRefLen))
	}
	return nil
}
