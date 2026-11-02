package service

import (
	"context"
	"math"
	"sort"
	"strconv"

	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
)

// historySampleSize is how many prior governance decisions the precedent
// engine looks at.
const historySampleSize = 30

// precedentResult is the precedent engine's answer and its explanation.
type precedentResult struct {
	action        domain.RecommendedAction
	confidence    float64
	rationale     string
	sampled       int
	distribution  map[string]int
	historyStatus domain.HistoryStatus
}

// computePrecedent samples up to historySampleSize prior governance decisions
// for the same tenant + legal entity + action type and recommends the most
// frequent outcome, with confidence = its share of the sample.
//
// It never fails: if governance-decision-log-svc is unreachable, or has no
// matching history, the answer is NO_HISTORY with zero confidence — "AI
// failures degrade safely to no answer, deterministic workflow or human
// review rather than fabricated authority" (Data/Search/Analytics/AI spec §2).
func (s *Service) computePrecedent(ctx context.Context, caller domain.CallerContext, legalEntityID, actionType string) precedentResult {
	decisions, err := s.history.ListPriorDecisions(ctx, clients.HistoryQuery{
		TenantID:      caller.TenantID,
		PrincipalID:   caller.PrincipalID,
		CorrelationID: caller.CorrelationID,
		RequestID:     caller.RequestID,
		LegalEntityID: legalEntityID,
		ActionType:    actionType,
		Limit:         historySampleSize,
	})
	if err != nil {
		s.log.Warn("governance-decision-log-svc unavailable; degrading to NO_HISTORY", zap.Error(err))
		return precedentResult{
			action:        domain.RecommendedActionNoHistory,
			rationale:     "governance-decision-log-svc unreachable; no historical basis available",
			distribution:  map[string]int{},
			historyStatus: domain.HistoryUnavailable,
		}
	}
	if len(decisions) == 0 {
		return precedentResult{
			action:        domain.RecommendedActionNoHistory,
			rationale:     "no prior decisions found for this legal entity and action type",
			distribution:  map[string]int{},
			historyStatus: domain.HistoryEmpty,
		}
	}

	distribution := countOutcomes(decisions)
	topOutcome, topCount, isTie := mostFrequentOutcome(distribution)
	total := len(decisions)

	result := precedentResult{
		confidence:    roundConfidence(float64(topCount) / float64(total)),
		sampled:       total,
		distribution:  distribution,
		historyStatus: domain.HistoryAvailable,
	}

	// A tie is no precedent at all: escalate to a human rather than pick a
	// side by map-iteration order.
	if isTie {
		result.action = domain.RecommendedActionEscalate
		result.rationale = "no single outcome dominates the last " + strconv.Itoa(total) +
			" decisions for this legal entity and action type (tie at " + strconv.Itoa(topCount) + ")"
		return result
	}

	result.action = recommendedActionForOutcome(topOutcome)
	result.rationale = topOutcome + " in " + strconv.Itoa(topCount) + " of the last " + strconv.Itoa(total) +
		" decisions for this legal entity and action type"
	return result
}

func countOutcomes(decisions []clients.PriorDecision) map[string]int {
	counts := map[string]int{}
	for _, decision := range decisions {
		counts[decision.Outcome]++
	}
	return counts
}

// mostFrequentOutcome returns the outcome with the highest count, that
// count, and whether another outcome has the same count. Outcomes are
// visited in sorted order so the result is deterministic.
func mostFrequentOutcome(counts map[string]int) (outcome string, count int, isTie bool) {
	outcomes := make([]string, 0, len(counts))
	for name := range counts {
		outcomes = append(outcomes, name)
	}
	sort.Strings(outcomes)

	for _, name := range outcomes {
		switch {
		case counts[name] > count:
			outcome, count, isTie = name, counts[name], false
		case counts[name] == count:
			isTie = true
		}
	}
	return outcome, count, isTie
}

func recommendedActionForOutcome(outcome string) domain.RecommendedAction {
	switch outcome {
	case "GRANTED":
		return domain.RecommendedActionApprove
	case "DENIED":
		return domain.RecommendedActionReject
	default:
		return domain.RecommendedActionEscalate
	}
}

// roundConfidence keeps three decimal places — the precision of the
// confidence_score NUMERIC(4,3) column — so the value returned on create is
// exactly the value later read back.
func roundConfidence(value float64) float64 {
	return math.Round(value*1000) / 1000
}
