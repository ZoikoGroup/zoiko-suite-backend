package domain

import (
	"fmt"
	"math/big"
	"slices"
)

// GetReciprocalBalances (SPEC_DEVIATIONS A1) — ACC-11's own aggregation of
// the intercompany positions between two legal entities in one fiscal
// period: what each direction of the pair carries, by currency, split by
// match_status, and the net of the two directions per currency.
//
// These are the numbers consolidation-svc is expected to cap an elimination
// posting against (ACC-12's "Elimination exceeds matched reciprocal
// balance"), so every sum is exact. Amounts are read as decimal text and
// added as big.Rat (decimal.go), never as float64; a total that could not be
// rendered exactly is refused rather than rounded; and currencies are never
// combined with one another — a USD total and a GBP total stay separate
// entries, whichever directions and statuses they came from.

// ReciprocalBalanceGroup is one grouped row of the aggregation: a direction
// (source_legal_entity_id → target_legal_entity_id), a currency and a match
// status, with the exact sum of that group's amounts. The store produces one
// per (source_legal_entity_id, target_legal_entity_id, currency_code,
// match_status) group for the requested pair.
type ReciprocalBalanceGroup struct {
	MatchStatus         string
	SourceLegalEntityID string
	TargetLegalEntityID string
	CurrencyCode        string
	Amount              Decimal
}

// ReciprocalCurrencyTotal is one currency's exact total within one
// direction. Amount is carried as a Decimal — a JSON number of exactly its
// digits on the wire, the same form amount has always had here
// (SPEC_DEVIATIONS B7) — so the total consolidation reads is the total this
// service summed.
type ReciprocalCurrencyTotal struct {
	CurrencyCode string  `json:"currency_code"`
	Amount       Decimal `json:"amount"`
}

// ReciprocalStatusBalances is one match_status's slice of the pair's
// positions: each direction's totals by currency, and the net
// (entity A's direction minus entity B's) per currency. A currency carried
// by only one direction still gets an explicit signed net, never a sum
// across currencies.
type ReciprocalStatusBalances struct {
	MatchStatus string `json:"match_status"`

	// EntityAToEntityB totals the pairs whose source is entity_a (the
	// originating side); EntityBToEntityA totals the pairs whose source is
	// entity_b.
	EntityAToEntityB []ReciprocalCurrencyTotal `json:"entity_a_to_entity_b"`
	EntityBToEntityA []ReciprocalCurrencyTotal `json:"entity_b_to_entity_a"`

	// Net is EntityAToEntityB minus EntityBToEntityA, per currency.
	Net []ReciprocalCurrencyTotal `json:"net"`
}

// ReciprocalBalances is GetReciprocalBalances' answer.
type ReciprocalBalances struct {
	EntityA string `json:"entity_a"`
	EntityB string `json:"entity_b"`

	// FiscalPeriod is the period asked for. Only pairs carrying it are
	// aggregated: a pair with no period (created before migration 000005, or
	// by a caller that sends none) belongs to no period and is not attributed
	// to one — see SPEC_DEVIATIONS B10.
	FiscalPeriod string `json:"fiscal_period"`

	// ByMatchStatus carries one entry per status present, in the spec's own
	// state-model order. A status with no pairs does not appear; a direction
	// with no totals in a status that does appear is [] and never null.
	ByMatchStatus []ReciprocalStatusBalances `json:"by_match_status"`
}

// reciprocalStatusOrder is the spec's own state model —
// Open → AwaitingCounterparty → Matched / Mismatched / Disputed →
// Resolved/Closed — which is the order the response lists statuses in.
// UNMATCHED is the spec's "Open" under its pre-existing name (types.go).
var reciprocalStatusOrder = []string{
	MatchStatusUnmatched,
	MatchStatusAwaitingCounterparty,
	MatchStatusMatched,
	MatchStatusMismatch,
	MatchStatusDisputed,
	MatchStatusResolved,
	MatchStatusClosed,
}

// BuildReciprocalBalances assembles the endpoint's answer from the grouped
// totals: per match status, the two directions' per-currency totals and
// their difference per currency, all summed exactly. A group that names
// neither direction of the requested pair is ignored (the store never
// returns one); a group naming a status the state model does not know is
// kept rather than dropped.
func BuildReciprocalBalances(entityA, entityB, period string, groups []ReciprocalBalanceGroup) (*ReciprocalBalances, error) {
	// status → direction ("a" | "b") → currency_code → exact total.
	totals := make(map[string]map[string]map[string]*big.Rat)
	for _, g := range groups {
		var direction string
		switch {
		case g.SourceLegalEntityID == entityA && g.TargetLegalEntityID == entityB:
			direction = "a"
		case g.SourceLegalEntityID == entityB && g.TargetLegalEntityID == entityA:
			direction = "b"
		default:
			continue
		}
		amount, err := g.Amount.Rat()
		if err != nil {
			return nil, fmt.Errorf("reciprocal balance: %w", err)
		}
		if totals[g.MatchStatus] == nil {
			totals[g.MatchStatus] = map[string]map[string]*big.Rat{"a": {}, "b": {}}
		}
		byCurrency := totals[g.MatchStatus][direction]
		if byCurrency[g.CurrencyCode] == nil {
			byCurrency[g.CurrencyCode] = new(big.Rat)
		}
		byCurrency[g.CurrencyCode].Add(byCurrency[g.CurrencyCode], amount)
	}

	out := &ReciprocalBalances{
		EntityA: entityA, EntityB: entityB, FiscalPeriod: period,
		ByMatchStatus: []ReciprocalStatusBalances{},
	}
	for _, status := range orderedStatuses(totals) {
		aToB, err := currencyTotals(totals[status]["a"])
		if err != nil {
			return nil, err
		}
		bToA, err := currencyTotals(totals[status]["b"])
		if err != nil {
			return nil, err
		}
		net, err := netTotals(totals[status]["a"], totals[status]["b"])
		if err != nil {
			return nil, err
		}
		out.ByMatchStatus = append(out.ByMatchStatus, ReciprocalStatusBalances{
			MatchStatus:      status,
			EntityAToEntityB: aToB,
			EntityBToEntityA: bToA,
			Net:              net,
		})
	}
	return out, nil
}

// orderedStatuses returns the statuses present, in the spec's state-model
// order; a status the model does not name is kept after the known ones
// (sorted) rather than dropped. The migration 000006 CHECK constraint makes
// such a status unstorable, so this is defence, not a behaviour.
func orderedStatuses(totals map[string]map[string]map[string]*big.Rat) []string {
	ordered := make([]string, 0, len(totals))
	extra := make([]string, 0)
	for status := range totals {
		if slices.Contains(reciprocalStatusOrder, status) {
			continue
		}
		extra = append(extra, status)
	}
	slices.Sort(extra)
	for _, status := range reciprocalStatusOrder {
		if _, present := totals[status]; present {
			ordered = append(ordered, status)
		}
	}
	return append(ordered, extra...)
}

// currencyTotals renders one direction's totals, currencies in sorted order,
// each amount exact.
func currencyTotals(byCurrency map[string]*big.Rat) ([]ReciprocalCurrencyTotal, error) {
	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	slices.Sort(currencies)

	out := make([]ReciprocalCurrencyTotal, 0, len(currencies))
	for _, c := range currencies {
		amount, err := ExactDecimal(byCurrency[c])
		if err != nil {
			return nil, err
		}
		out = append(out, ReciprocalCurrencyTotal{CurrencyCode: c, Amount: amount})
	}
	return out, nil
}

// netTotals is entity A's direction minus entity B's, per currency. A
// currency missing from one direction counts as zero there; the two
// directions are never combined into one number across currencies.
func netTotals(aToB, bToA map[string]*big.Rat) ([]ReciprocalCurrencyTotal, error) {
	seen := make(map[string]bool, len(aToB)+len(bToA))
	currencies := make([]string, 0, len(aToB)+len(bToA))
	for _, byCurrency := range []map[string]*big.Rat{aToB, bToA} {
		for c := range byCurrency {
			if !seen[c] {
				seen[c] = true
				currencies = append(currencies, c)
			}
		}
	}
	slices.Sort(currencies)

	out := make([]ReciprocalCurrencyTotal, 0, len(currencies))
	for _, c := range currencies {
		net := new(big.Rat)
		if r, ok := aToB[c]; ok {
			net.Add(net, r)
		}
		if r, ok := bToA[c]; ok {
			net.Sub(net, r)
		}
		amount, err := ExactDecimal(net)
		if err != nil {
			return nil, err
		}
		out = append(out, ReciprocalCurrencyTotal{CurrencyCode: c, Amount: amount})
	}
	return out, nil
}
