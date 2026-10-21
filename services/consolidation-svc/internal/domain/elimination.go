package domain

import (
	"fmt"
	"strings"
)

// GetReciprocalBalances (intercompany-accounting-svc's ACC-11 A1) is the
// authoritative aggregation of a pair's positions for one period. These are
// the fields consolidation-svc reads: each direction's totals by currency,
// split by match status. Amount is Decimal, so the numbers this service
// explains are the exact numbers intercompany-accounting-svc summed — never
// float64 and never combined across currencies.
type ReciprocalCurrencyTotal struct {
	CurrencyCode string  `json:"currency_code"`
	Amount       Decimal `json:"amount"`
}

type ReciprocalStatusBalances struct {
	MatchStatus string `json:"match_status"`

	EntityAToEntityB []ReciprocalCurrencyTotal `json:"entity_a_to_entity_b"`
	EntityBToEntityA []ReciprocalCurrencyTotal `json:"entity_b_to_entity_a"`
	Net              []ReciprocalCurrencyTotal `json:"net"`
}

type ReciprocalBalances struct {
	EntityA       string                     `json:"entity_a"`
	EntityB       string                     `json:"entity_b"`
	FiscalPeriod  string                     `json:"fiscal_period"`
	ByMatchStatus []ReciprocalStatusBalances `json:"by_match_status"`
}

// EliminationRuleMatchedOrClosed is the only elimination predicate ACC-12
// applies here: a pair is eliminable when it is MATCHED, or CLOSED (matched
// first, then closed). UNMATCHED, AWAITING_COUNTERPARTY, MISMATCH, DISPUTED
// and RESOLVED are not eliminated (SPEC_DEVIATIONS B6).
const EliminationRuleMatchedOrClosed = "MATCHED_OR_CLOSED"

const (
	eliminationStatusMatched = "MATCHED"
	eliminationStatusClosed  = "CLOSED"
)

// EliminationExplanation is the answer to ACC-12's ExplainElimination query:
// the rule's decision for one entity pair in one period, and the reciprocal
// balance it was checked against.
//
// Eliminable is the rule's current answer for the pair, not a replay of a
// past run's arithmetic: this service does not yet persist per-run
// elimination rows (that is A2's remaining work). The reciprocal balance is
// returned whole so the caller can see both directions and every currency,
// rather than a single number that would hide how it was reached.
type EliminationExplanation struct {
	GroupLegalEntityID string `json:"group_legal_entity_id"`
	EntityA            string `json:"entity_a"`
	EntityB            string `json:"entity_b"`
	FiscalPeriod       string `json:"fiscal_period"`

	Eliminable      bool     `json:"eliminable"`
	Rule            string   `json:"rule"`
	MatchedStatuses []string `json:"matched_statuses"`
	Reason          string   `json:"reason"`

	ReciprocalBalance *ReciprocalBalances `json:"reciprocal_balance"`
}

// BuildEliminationExplanation applies the MATCHED-or-CLOSED rule to the
// reciprocal balance intercompany-accounting-svc returned for the pair. A
// status only appears in ByMatchStatus when the pair carries at least one
// entry in it, so presence is the fact the rule tests; the amounts are
// evidence, not the predicate.
func BuildEliminationExplanation(groupLegalEntityID, entityA, entityB, fiscalPeriod string, balances *ReciprocalBalances) *EliminationExplanation {
	out := &EliminationExplanation{
		GroupLegalEntityID: groupLegalEntityID,
		EntityA:            entityA,
		EntityB:            entityB,
		FiscalPeriod:       fiscalPeriod,
		Rule:               EliminationRuleMatchedOrClosed,
		MatchedStatuses:    []string{},
		ReciprocalBalance:  balances,
	}

	present := make([]string, 0)
	if balances != nil {
		for _, s := range balances.ByMatchStatus {
			switch s.MatchStatus {
			case eliminationStatusMatched, eliminationStatusClosed:
				out.MatchedStatuses = append(out.MatchedStatuses, s.MatchStatus)
			default:
				present = append(present, s.MatchStatus)
			}
		}
	}

	out.Eliminable = len(out.MatchedStatuses) > 0
	switch {
	case out.Eliminable:
		out.Reason = fmt.Sprintf(
			"the pair is eliminable: its reciprocal balance carries %s, and ACC-12 eliminates MATCHED and CLOSED pairs (B6)",
			strings.Join(out.MatchedStatuses, ", "))
	case len(present) > 0:
		out.Reason = fmt.Sprintf(
			"the pair is not eliminable: its reciprocal balance carries no MATCHED or CLOSED entries (statuses present: %s)",
			strings.Join(present, ", "))
	default:
		out.Reason = "the pair is not eliminable: no intercompany entries for this pair in this period"
	}
	return out
}
