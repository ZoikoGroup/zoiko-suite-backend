package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/query"
)

// maxRestrictionExclusions is how many FAILED-restriction refs a search will
// exclude by id before it stops trusting the exclusion list and blocks the
// scope instead. Past this, the restriction lane is not failing for a few
// records — it is failing, and §8.2's "affected search scope can be blocked
// rather than continuing known over-disclosure" applies to the scope.
const maxRestrictionExclusions = 500

// scopeHealth is the operational state a compiled plan runs under.
type scopeHealth struct {
	freshness domain.Freshness
	lagMS     int64
	// failedRefs are the source ids of this tenant's FAILED restrictions in
	// the scope.
	failedRefs []string
}

// checkScopeHealth decides whether a plan may run against the scope's active
// generation right now, and under what conditions.
//
// Two independent questions, in this order:
//
//  1. Freshness (ESR-012). The active generation's checkpoint says CURRENT,
//     LAGGING, STALE or UNKNOWN. A checkpoint older than checkpointMaxAge is
//     itself UNKNOWN: the sweep that should have refreshed it has stopped, and
//     a CURRENT written an hour ago is a claim nobody is standing behind.
//     Under STALE or UNKNOWN, a scope whose contract class is protected (R1+)
//     is REFUSED — §8.3, "protected results may require source hydration or
//     block". An R0 scope still answers, flagged, and its protected documents
//     are suppressed per hit by the retriever.
//  2. Restriction safety (ESR-018). NP-59: "search scope is marked CURRENT
//     while restriction queue is failing → health model separates normal
//     freshness from restriction safety; scope becomes unsafe/degraded". A
//     tenant's FAILED restrictions are excluded by id, and past
//     maxRestrictionExclusions the scope is refused.
//
// Every failure to READ this state fails closed, the same as the state being
// bad: an unreadable checkpoint is UNKNOWN, an unreadable restriction queue
// blocks.
func (h *Handler) checkScopeHealth(ctx context.Context, tenantID string, contract *domain.IndexContract, generation *domain.IndexGeneration) (scopeHealth, *query.Error) {
	health := scopeHealth{freshness: domain.FreshnessUnknown}

	cp, err := h.store.GetCheckpoint(ctx, contract.ScopeName, generation.PhysicalIndex)
	switch {
	case err == nil && cp != nil:
		health.freshness = cp.Freshness
		health.lagMS = cp.LagMS
		if h.checkpointMaxAge > 0 && time.Since(cp.ObservedAt) > h.checkpointMaxAge {
			health.freshness = domain.FreshnessUnknown
		}
	case errors.Is(err, domain.ErrNotFound):
		// Never measured. UNKNOWN, which is the truth.
	default:
		h.log.Error("scope health: checkpoint unreadable — treating freshness as UNKNOWN",
			zap.String("scope", contract.ScopeName), zap.Error(err))
	}

	if health.freshness.Untrusted() && contract.RetrievalClass != domain.RetrievalR0 {
		return health, &query.Error{
			Code: domain.ReasonIndexStaleForScope,
			Detail: fmt.Sprintf("scope %q is %s and serves %s (protected) content; "+
				"a protected search does not run against an index that cannot be trusted as current",
				contract.ScopeName, health.freshness, contract.RetrievalClass),
		}
	}

	failed, err := h.store.ListFailedRestrictions(ctx, tenantID, contract.ScopeName, maxRestrictionExclusions)
	if err != nil {
		h.log.Error("scope health: restriction queue unreadable — blocking",
			zap.String("scope", contract.ScopeName), zap.Error(err))
		return health, &query.Error{
			Code:   domain.ReasonRestrictionPropFailed,
			Detail: "restriction propagation state could not be read; the scope cannot be proven safe",
		}
	}
	if len(failed) > maxRestrictionExclusions {
		return health, &query.Error{
			Code: domain.ReasonRestrictionPropFailed,
			Detail: fmt.Sprintf("more than %d restrictions in scope %q have failed to propagate; "+
				"the scope is blocked until they verify", maxRestrictionExclusions, contract.ScopeName),
		}
	}
	health.failedRefs = failed
	return health, nil
}

// apply stamps the health onto a compiled plan: the freshness the retriever
// enforces per hit, and the FAILED-restriction refs as a mandatory exclusion.
func (s scopeHealth) apply(plan *query.Plan) {
	plan.Freshness = s.freshness
	plan.LagMS = s.lagMS
	if len(s.failedRefs) > 0 {
		plan.Execution.MandatoryMustNot = append(plan.Execution.MandatoryMustNot,
			searchclient.TermFilter{Field: "source_id", Values: s.failedRefs})
		plan.RestrictionExclusions = len(s.failedRefs)
	}
}
