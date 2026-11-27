package handler

import (
	"context"
	"fmt"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/source"
)

// checkContinuity ties each population's declared roll-forward to the previous period's
// certified closing balance (ZS-SVC-Z-001 DQC-03, cross-period roll-forward) and appends the
// outcome to the population's recorded control outcomes. A broken tie is a population that
// cannot be proven reproducible, exactly as a source whose own totals do not tie: the caller
// records a failed run, nothing is frozen. A store failure is returned as itself.
func (h *Handler) checkContinuity(ctx context.Context, tenantID, legalEntityID string, pops []domain.FrozenPopulation) error {
	for i := range pops {
		rf := pops[i].Controls.RollForward
		if rf == nil {
			continue
		}
		var prior *domain.PriorClosing
		if rf.PeriodStart != "" {
			var err error
			if prior, err = h.store.PriorClosing(ctx, tenantID, legalEntityID, pops[i].SpecRef, pops[i].Side, rf.PeriodStart); err != nil {
				return err
			}
		}
		outcome, err := domain.CheckContinuity(*rf, prior)
		if err != nil {
			return &source.PopulationError{Side: pops[i].Side,
				Err: fmt.Errorf("%w: %s: %v", source.ErrSourceInconsistent, pops[i].SpecRef, err)}
		}
		pops[i].ControlOutcomes = append(pops[i].ControlOutcomes, outcome)
	}
	return nil
}
