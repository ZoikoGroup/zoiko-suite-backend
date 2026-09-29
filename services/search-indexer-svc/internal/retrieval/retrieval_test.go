package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/query"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeEngine struct {
	result searchclient.Result
	err    error
	// lastPlan records what was actually executed, so a test can assert on
	// the plan the retriever handed the engine rather than only on what came
	// back.
	lastPlan searchclient.ExecutionPlan
}

func (f *fakeEngine) ExecutePlan(_ context.Context, _ string, p searchclient.ExecutionPlan) (searchclient.Result, error) {
	f.lastPlan = p
	return f.result, f.err
}

// The rest of the Engine surface is unused by the retriever; these exist so
// *fakeEngine satisfies the interface.
func (f *fakeEngine) EnsureIndex(context.Context, searchclient.IndexName) error { return nil }
func (f *fakeEngine) Index(context.Context, searchclient.IndexName, searchclient.Document) error {
	return nil
}
func (f *fakeEngine) Search(context.Context, searchclient.IndexName, searchclient.SearchQuery) ([]searchclient.SearchResult, error) {
	return nil, nil
}
func (f *fakeEngine) EnsureGeneration(context.Context, string, []searchclient.FieldMapping) error {
	return nil
}
func (f *fakeEngine) ActivateGeneration(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *fakeEngine) ActiveGeneration(context.Context, string) (string, error) { return "", nil }
func (f *fakeEngine) DropIndex(context.Context, string) error                  { return nil }
func (f *fakeEngine) IndexProjection(context.Context, string, searchclient.Projection) error {
	return nil
}
func (f *fakeEngine) DeleteProjection(context.Context, string, string) error { return nil }
func (f *fakeEngine) GetProjection(context.Context, string, string) (map[string]any, bool, error) {
	return nil, false, nil
}
func (f *fakeEngine) CountProjections(context.Context, string, map[string]string) (int64, error) {
	return 0, nil
}
func (f *fakeEngine) Ping(context.Context) error { return nil }

type fakeAuthz struct {
	err   error
	calls []string
}

func (f *fakeAuthz) CheckAllowed(_ context.Context, principalID, entityID, action string) error {
	f.calls = append(f.calls, principalID+"|"+entityID+"|"+action)
	return f.err
}

type fakeHydrator struct {
	doc map[string]any
	err error
}

func (f *fakeHydrator) Hydrate(context.Context, string, string, string) (map[string]any, error) {
	return f.doc, f.err
}

// ── fixtures ─────────────────────────────────────────────────────────────────

var metricsOnce *telemetry.Metrics

func testMetrics() *telemetry.Metrics {
	// One registry per test binary: Prometheus panics on a duplicate
	// registration, and every test here wants the same collectors.
	if metricsOnce == nil {
		metricsOnce = telemetry.NewMetrics("search-indexer-svc-test")
	}
	return metricsOnce
}

func hit(sourceID string, source map[string]any) searchclient.Hit {
	base := map[string]any{
		"tenant_id":        "tenant-a",
		"legal_entity_id":  "entity-1",
		"source_type":      "obligation",
		"source_id":        sourceID,
		"source_version":   float64(1),
		"retrieval_class":  "R1",
		"index_generation": "g-1",
		"tombstoned":       false,
	}
	for k, v := range source {
		base[k] = v
	}
	return searchclient.Hit{DocID: "tenant-a:obligation:" + sourceID, Score: 1.5, Source: base}
}

func testPlan() *query.Plan {
	return &query.Plan{
		Scope:          "obligation",
		Target:         "obligation",
		AuthzAction:    "OBLIGATION_READ",
		RetrievalClass: domain.RetrievalR1,
		PartitionSet:   []string{"obligation-gabc"},
		Execution:      searchclient.ExecutionPlan{Size: 20},
		ReturnableFields: map[string]bool{
			"obligation_code": true, "obligation_status": true,
		},
		SnippetFields: map[string]bool{"obligation_code": true},
	}
}

func testTC() query.Context {
	return query.Context{
		TenantID: "tenant-a", ActorID: "principal-1",
		LegalEntityID: "entity-1", Purpose: "COMPLIANCE_REVIEW",
	}
}

func newRetriever(e *fakeEngine, a authz.Client, h Hydrator) *Retriever {
	return New(e, a, h, testMetrics(), zap.NewNop(), time.Second)
}

// ── tests ────────────────────────────────────────────────────────────────────

// INV-06. Every R1 hit is re-authorized before its content is returned — the
// index hit is not the permission.
func TestExecute_ReauthorizesEveryR1Hit(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 2, Relation: "eq",
		Hits: []searchclient.Hit{
			hit("ob-1", map[string]any{"obligation_code": "GST-Q4"}),
			hit("ob-2", map[string]any{"obligation_code": "VAT-Q1"}),
		},
	}}
	az := &fakeAuthz{}
	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), testPlan(), testTC(), nil)

	require.NoError(t, err)
	assert.Len(t, resp.Results, 2)
	assert.Len(t, az.calls, 2, "each candidate is its own authorization decision")
	assert.Equal(t, "principal-1|entity-1|OBLIGATION_READ", az.calls[0])
}

// INV-05 / NP-05. A DENIED decision suppresses the result and reports a
// non-sensitive code — never the source ref of the thing that was withheld.
func TestExecute_DeniedResultsAreSuppressedWithoutRevealingThem(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-secret", map[string]any{"obligation_code": "CONFIDENTIAL"})},
	}}
	az := &fakeAuthz{err: authz.ErrDenied}

	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)

	assert.Empty(t, resp.Results)
	require.Len(t, resp.Suppressions, 1)
	assert.Equal(t, domain.ReasonSourceSuppressed, resp.Suppressions[0].ReasonCode)
	assert.Equal(t, 1, resp.Suppressions[0].Count)

	// TC-06: a reason code, and nothing that names the withheld record.
	body := mustString(t, resp)
	assert.NotContains(t, body, "ob-secret")
	assert.NotContains(t, body, "CONFIDENTIAL")
}

// INV-07 / NP-04. An unreachable authorizer suppresses, and marks the whole
// answer non-exhaustive — a search that quietly dropped results while
// claiming COMPLETE would be the worst of both.
func TestExecute_UnavailableAuthorizerSuppressesAndDegrades(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "GST-Q4"})},
	}}
	az := &fakeAuthz{err: authz.ErrUnavailable}

	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)

	assert.Empty(t, resp.Results)
	assert.Equal(t, domain.CompletenessPartial, resp.Completeness)
	require.Len(t, resp.Suppressions, 1)
	assert.Equal(t, domain.ReasonAuthorizationIndet, resp.Suppressions[0].ReasonCode,
		"INDETERMINATE must be distinguishable from DENIED in the evidence")
	assert.False(t, resp.TotalIsExact)
}

// R0 skips re-authorization by design (§7.1), which is why the contract
// validator refuses to put anything above INTERNAL behind it.
func TestExecute_R0DoesNotReauthorize(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR0
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{
			"obligation_code": "PUBLIC-REF", "retrieval_class": "R0"})},
	}}
	az := &fakeAuthz{err: authz.ErrDenied}

	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	assert.Len(t, resp.Results, 1)
	assert.Empty(t, az.calls)
}

// A document stamped stricter than its contract keeps its own class. A
// contract-level R1 must not downgrade a record classified R2.
func TestExecute_DocumentClassWinsWhenStricter(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR1
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{
			"obligation_code": "MATERIAL", "retrieval_class": "R2"})},
	}}
	// No hydrator: an R2 document with no hydrator must be suppressed rather
	// than served from the index.
	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)

	assert.Empty(t, resp.Results)
	require.Len(t, resp.Suppressions, 1)
	assert.Equal(t, domain.ReasonSourceHydrationFailed, resp.Suppressions[0].ReasonCode)
}

// §7.1 R2. When hydration succeeds the SOURCE replaces the index's view, and
// the result says so.
func TestExecute_R2ReplacesIndexContentWithSource(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR2
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "STALE-CODE"})},
	}}
	hydrator := &fakeHydrator{doc: map[string]any{
		"obligation_code": "CURRENT-CODE", "obligation_status": "CLOSED",
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, hydrator).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "CURRENT-CODE", resp.Results[0].Fields["obligation_code"])
	assert.Equal(t, "SOURCE", resp.Results[0].Freshness)
}

// A hydrated document is filtered through the SAME returnable allowlist. It
// did not come from the engine and has never been filtered, so skipping the
// second pass would put the freshest, most complete data on the one path with
// no field policy.
func TestExecute_HydratedDocumentIsStillFieldFiltered(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR2
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "X"})},
	}}
	hydrator := &fakeHydrator{doc: map[string]any{
		"obligation_code":     "CURRENT",
		"internal_risk_score": 97,
		"assignee_salary":     125000,
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, hydrator).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "CURRENT", resp.Results[0].Fields["obligation_code"])
	assert.NotContains(t, resp.Results[0].Fields, "internal_risk_score")
	assert.NotContains(t, resp.Results[0].Fields, "assignee_salary")
}

// NP-20. Hydration failure suppresses; it never falls back to index content.
func TestExecute_HydrationFailureSuppressesRatherThanServingStaleIndex(t *testing.T) {
	plan := testPlan()
	plan.RetrievalClass = domain.RetrievalR2
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "STALE"})},
	}}
	hydrator := &fakeHydrator{err: errors.New("source service unreachable")}

	resp, err := newRetriever(engine, &fakeAuthz{}, hydrator).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)

	assert.Empty(t, resp.Results)
	assert.Equal(t, domain.CompletenessPartial, resp.Completeness)
	assert.NotContains(t, mustString(t, resp), "STALE")
}

// NP-24 and §7.2. Stored markup is escaped; only the engine's match markers
// become real tags. The ORDER is the control — escape first, then substitute.
func TestExecute_SnippetsEscapeStoredMarkupAndKeepOnlyOurMarks(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{{
			DocID: "tenant-a:obligation:ob-1", Score: 1,
			Source: hit("ob-1", map[string]any{"obligation_code": "x"}).Source,
			Highlights: map[string][]string{
				"obligation_code": {
					"<img src=x onerror=alert(1)> filing for " +
						searchclient.HighlightOpen + "GST" + searchclient.HighlightClose,
				},
			},
		}},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)

	fragment := resp.Results[0].Snippets["obligation_code"][0]
	assert.Contains(t, fragment, "<mark>GST</mark>", "our own match markers become real markup")
	assert.NotContains(t, fragment, "<img", "stored markup must not survive as markup")
	assert.Contains(t, fragment, "&lt;img", "stored markup is escaped to visible text")
	assert.NotContains(t, fragment, "onerror=alert(1)>")
}

// §7.2 / NP-25. A highlight for a field the plan did not mark snippet-allowed
// is dropped entirely — the client is not even told which field matched.
func TestExecute_SnippetsFromNonSnippetFieldsAreDropped(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{{
			DocID: "d", Score: 1,
			Source: hit("ob-1", nil).Source,
			Highlights: map[string][]string{
				"internal_risk_score": {searchclient.HighlightOpen + "97" + searchclient.HighlightClose},
			},
		}},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Nil(t, resp.Results[0].Snippets)
	assert.NotContains(t, mustString(t, resp), "internal_risk_score")
}

// NP-19 / INV-24. A partial engine answer is reported as partial, and the
// total stops claiming to be exact.
func TestExecute_PartialEngineAnswerIsReportedAsPartial(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 10, Relation: "eq", Partial: true, PartialReason: "2 of 5 shards failed",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "GST"})},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)

	assert.Equal(t, domain.CompletenessPartial, resp.Completeness)
	assert.Equal(t, "2 of 5 shards failed", resp.CompletenessDetail)
	assert.False(t, resp.TotalIsExact)
	assert.Contains(t, resp.ReasonCodes, string(domain.ReasonSearchDegradedPartial))
}

// INV-24's monotonic half: a later COMPLETE cannot overwrite an earlier
// PARTIAL. The answer is as incomplete as its worst part.
func TestExecute_CompletenessOnlyEverDegrades(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 2, Relation: "eq", Partial: true, PartialReason: "shard failure",
		Hits: []searchclient.Hit{
			hit("ob-1", map[string]any{"obligation_code": "A"}),
			hit("ob-2", map[string]any{"obligation_code": "B"}),
		},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	assert.Len(t, resp.Results, 2, "both results were authorized")
	assert.Equal(t, domain.CompletenessPartial, resp.Completeness,
		"a fully authorized page is still PARTIAL when the engine answered partially")
}

// NP-40 / §7.3. Once anything is suppressed the pre-authorization total no
// longer describes what the caller may see, and saying so is the honest
// answer — recomputing it downward would tell the caller exactly how many
// records it was not allowed to see.
func TestExecute_TotalBecomesInexactOnceAnythingIsSuppressed(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 100, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "A"})},
	}}
	az := &fakeAuthz{err: authz.ErrDenied}

	resp, err := newRetriever(engine, az, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	assert.False(t, resp.TotalIsExact)
	assert.Equal(t, int64(100), resp.Total,
		"the engine's count is reported as-is and flagged inexact, not silently adjusted")
}

// A tombstoned document that somehow survived the mandatory filter is
// suppressed rather than returned. Reaching this branch means the query layer
// and the document disagree, which is a correctness failure and not a
// routine skip.
func TestExecute_TombstonedDocumentIsSuppressed(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{
			"obligation_code": "REMOVED", "tombstoned": true})},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Results)
	assert.NotContains(t, mustString(t, resp), "REMOVED")
}

// A hit with no source ref cannot be traced (TC-01) or re-authorized, so it
// is suppressed rather than returned untraceable.
func TestExecute_HitWithNoSourceRefIsSuppressed(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{{DocID: "d", Score: 1, Source: map[string]any{"tenant_id": "tenant-a"}}},
	}}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Results)
	require.Len(t, resp.Suppressions, 1)
}

// INV-28 / NP-31. A workload's authorization question names the REAL actor,
// never the service identity — that is what stops an AI path widening what it
// can retrieve by virtue of who it is.
func TestExecute_WorkloadAuthorizesAsTheRealPrincipal(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{hit("ob-1", map[string]any{"obligation_code": "GST"})},
	}}
	az := &fakeAuthz{}
	tc := query.Context{
		TenantID: "tenant-a", WorkloadID: "workload-rag",
		OnBehalfOf: "human-principal-7", LegalEntityID: "entity-1", Purpose: "AI",
	}

	_, err := newRetriever(engine, az, nil).Execute(context.Background(), testPlan(), tc, nil)
	require.NoError(t, err)
	require.Len(t, az.calls, 1)
	assert.True(t, strings.HasPrefix(az.calls[0], "human-principal-7|"),
		"the decision must be about the human, not the workload; got %q", az.calls[0])
	assert.NotContains(t, az.calls[0], "workload-rag")
}

// ESR-011. A scope whose alias resolves to nothing is an error, not an empty
// result — a caller told "no matches" would conclude the data does not exist.
func TestExecute_NoActiveGenerationIsAnErrorNotAnEmptyResult(t *testing.T) {
	engine := &fakeEngine{err: searchclient.ErrNoActiveGeneration}

	_, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), testPlan(), testTC(), nil)
	require.Error(t, err)
	var qerr *query.Error
	require.ErrorAs(t, err, &qerr)
	assert.Equal(t, domain.ReasonGenerationNotActive, qerr.Code)
}

// A cursor is minted only when a FULL page came back. Minting one for a short
// page hands the caller a token that walks into nothing and looks like loss.
func TestExecute_CursorOnlyMintedForAFullPage(t *testing.T) {
	plan := testPlan()
	plan.Execution.Size = 2

	short := &fakeEngine{result: searchclient.Result{
		Total: 1, Relation: "eq",
		Hits: []searchclient.Hit{{DocID: "d", Score: 1, Source: hit("ob-1", nil).Source, Sort: []any{1.0}}},
	}}
	resp, err := newRetriever(short, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(),
		func([]any) (string, error) { return "CURSOR", nil })
	require.NoError(t, err)
	assert.Empty(t, resp.NextCursor)

	full := &fakeEngine{result: searchclient.Result{
		Total: 5, Relation: "eq",
		Hits: []searchclient.Hit{
			{DocID: "d1", Score: 1, Source: hit("ob-1", nil).Source, Sort: []any{1.0}},
			{DocID: "d2", Score: 1, Source: hit("ob-2", nil).Source, Sort: []any{2.0}},
		},
	}}
	resp, err = newRetriever(full, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(),
		func([]any) (string, error) { return "CURSOR", nil })
	require.NoError(t, err)
	assert.Equal(t, "CURSOR", resp.NextCursor)
}

// The engine's ".keyword" suffix is an implementation detail the planner
// added; a caller asked for the field by its contract name.
func TestExecute_FacetNamesAreReportedByContractName(t *testing.T) {
	engine := &fakeEngine{result: searchclient.Result{
		Total: 0, Relation: "eq",
		Facets: map[string][]searchclient.FacetBucket{
			"obligation_code.keyword": {{Value: "GST", Count: 4}},
		},
	}}
	plan := testPlan()
	plan.Execution.Facets = []string{"obligation_code.keyword"}

	resp, err := newRetriever(engine, &fakeAuthz{}, nil).Execute(context.Background(), plan, testTC(), nil)
	require.NoError(t, err)
	require.Contains(t, resp.Facets, "obligation_code")
	assert.NotContains(t, resp.Facets, "obligation_code.keyword")
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
