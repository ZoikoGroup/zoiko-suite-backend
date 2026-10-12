package retrieval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
)

func codes(resp *Response) []string { return resp.ReasonCodes }

// An R0 answer from a STALE index is DEGRADED with ESR-012 and says so in
// index_freshness — the answer a lagging index gave before looked identical to
// a current one.
func TestExecute_StaleR0AnswerIsFlaggedNotComplete(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR0
	plan.Freshness = domain.FreshnessStale
	plan.LagMS = 900000
	engine := &fakeEngine{result: searchclient.Result{Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"retrieval_class": "R0", "obligation_code": "GST"})}}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Len(t, resp.Results, 1)
	assert.Equal(t, domain.CompletenessDegraded, resp.Completeness)
	assert.Equal(t, domain.FreshnessStale, resp.IndexFreshness)
	assert.Equal(t, int64(900000), resp.IndexLagMS)
	assert.Contains(t, codes(resp), string(domain.ReasonIndexStaleForScope))
}

// UNKNOWN is never represented as CURRENT — nor as COMPLETE.
func TestExecute_UnknownFreshnessIsNeverComplete(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR0
	plan.Freshness = domain.FreshnessUnknown
	engine := &fakeEngine{result: searchclient.Result{Total: 0, Relation: "eq"}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Equal(t, domain.CompletenessUnknown, resp.Completeness)
	assert.Contains(t, codes(resp), string(domain.ReasonIndexStaleForScope))
}

// §8.3's per-document half: a document stamped R1 inside an R0 scope is
// protected, and under untrusted freshness it is withheld — without even being
// sent for re-authorization.
func TestExecute_ProtectedDocumentInR0ScopeWithheldWhenStale(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR0
	plan.Freshness = domain.FreshnessStale
	engine := &fakeEngine{result: searchclient.Result{Total: 2, Relation: "eq", Hits: []searchclient.Hit{
		hit("ob-1", map[string]any{"retrieval_class": "R0"}),
		hit("ob-2", map[string]any{"retrieval_class": "R1"}),
	}}}
	az := &fakeAuthz{}

	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "ob-1", resp.Results[0].SourceID)
	assert.Empty(t, az.calls)
	require.Len(t, resp.Suppressions, 1)
	assert.Equal(t, domain.ReasonIndexStaleForScope, resp.Suppressions[0].ReasonCode)
}

// A zero-valued freshness is untrusted: a plan that forgot to carry one fails
// closed on protected content.
func TestExecute_MissingFreshnessFailsClosed(t *testing.T) {
	plan := testPlan()
	plan.Freshness = ""
	engine := &fakeEngine{result: searchclient.Result{Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", nil)}}}
	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Results)
}

// NP-59: FAILED restrictions excluded → DEGRADED with ESR-018.
func TestExecute_RestrictionExclusionsDegradeWithESR018(t *testing.T) {
	plan := testPlan()
	plan.RestrictionExclusions = 3
	engine := &fakeEngine{result: searchclient.Result{Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", nil)}}}
	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Equal(t, domain.CompletenessDegraded, resp.Completeness)
	assert.Contains(t, codes(resp), string(domain.ReasonRestrictionPropFailed))
	assert.False(t, resp.TotalIsExact)
}

// A hydrated source object is read at its SOURCE paths, through the returnable
// allowlist — not by contract field name, which found nothing.
func TestExecute_HydratedObjectIsProjectedBySourcePath(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR2
	plan.SourcePaths = map[string]string{"obligation_code": "code.value", "obligation_status": "status"}
	engine := &fakeEngine{result: searchclient.Result{Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "STALE-INDEX-COPY"})}}}
	hyd := &fakeHydrator{doc: map[string]any{
		"code":   map[string]any{"value": "GST-CURRENT"},
		"status": "OPEN",
		"secret": "never returned",
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, hyd).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "SOURCE", resp.Results[0].Freshness)
	assert.Equal(t, map[string]any{"obligation_code": "GST-CURRENT", "obligation_status": "OPEN"}, resp.Results[0].Fields)
}

// NP-55: a source that is gone suppresses as ESR-010 — not a hydration fault,
// and never the index's copy.
func TestExecute_SourceGoneSuppressesAsESR010(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR2
	engine := &fakeEngine{result: searchclient.Result{Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", nil)}}}
	hyd := &fakeHydrator{err: domain.ErrSourceGone}

	resp, err := newRetriever(engine, &fakeAuthz{}, hyd).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Results)
	require.Len(t, resp.Suppressions, 1)
	assert.Equal(t, domain.ReasonSourceSuppressed, resp.Suppressions[0].ReasonCode)
	assert.Equal(t, domain.CompletenessComplete, resp.Completeness)
}
