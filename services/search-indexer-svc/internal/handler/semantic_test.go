package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/embedding"
)

type stubEmbedder struct {
	err    error
	calls  int
	inputs []string
}

func (s *stubEmbedder) Embed(_ context.Context, spec domain.EmbeddingSpec, in []string) ([][]float32, error) {
	s.calls++
	s.inputs = append(s.inputs, in...)
	if s.err != nil {
		return nil, s.err
	}
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = make([]float32, spec.Dimensions)
	}
	return out, nil
}
func (s *stubEmbedder) Configured() bool { return true }

func pin() *domain.EmbeddingSpec {
	return &domain.EmbeddingSpec{Model: "m", ModelVersion: "1", Dimensions: 3,
		SourceFields: []string{"obligation_code"}, Preprocessing: embedding.ProfileNFKCWhitespaceV1, Similarity: "cosinesimil"}
}

func semanticHarness(t *testing.T) (*harness, *stubEmbedder) {
	t.Helper()
	h := newHarness(t, nil)
	h.store.contract.Embedding = pin()
	emb := &stubEmbedder{}
	h.handler.embedder = emb
	return h, emb
}

// ── freshness gate (ESR-012) ─────────────────────────────────────────────────

// The original top gap 2: a protected scope whose index is STALE used to
// answer as if current. It is now refused, 503 — the server's state, not the
// request's.
func TestSearch_StaleProtectedScopeIsRefusedWithESR012(t *testing.T) {
	h := newHarness(t, nil)
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessStale, LagMS: 7200000, ObservedAt: time.Now()}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "ESR-012", bodyOf(t, w)["reason_code"])
}

// A never-measured generation is UNKNOWN, which blocks protected content.
func TestSearch_UnmeasuredProtectedScopeIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.store.checkpointErr = domain.ErrNotFound

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "ESR-012", bodyOf(t, w)["reason_code"])
}

// A CURRENT nobody refreshed is not evidence: past the max age it is UNKNOWN.
func TestSearch_AbandonedCheckpointReadsAsUnknown(t *testing.T) {
	h := newHarness(t, nil)
	h.handler.checkpointMaxAge = time.Minute
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessCurrent, ObservedAt: time.Now().Add(-time.Hour)}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// An R0 scope answers under STALE — flagged, never presented as complete.
func TestSearch_StaleR0ScopeAnswersFlagged(t *testing.T) {
	h := newHarness(t, nil)
	h.store.contract.RetrievalClass = domain.RetrievalR0
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessStale, LagMS: 5000, ObservedAt: time.Now()}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	body := bodyOf(t, w)
	assert.Equal(t, "STALE", body["index_freshness"])
	assert.Contains(t, body["reason_codes"], "ESR-012")
}

// LAGGING is surfaced but is not a block.
func TestSearch_LaggingIsSurfacedNotBlocked(t *testing.T) {
	h := newHarness(t, nil)
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessLagging, LagMS: 200000, ObservedAt: time.Now()}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := bodyOf(t, w)
	assert.Equal(t, "LAGGING", body["index_freshness"])
	assert.Equal(t, float64(200000), body["index_lag_ms"])
}

// ── restriction safety (ESR-018) ─────────────────────────────────────────────

// NP-59: FAILED restrictions are excluded by id at query time and the answer is
// DEGRADED with ESR-018.
func TestSearch_FailedRestrictionsAreExcludedAndReported(t *testing.T) {
	h := newHarness(t, nil)
	h.store.failedRefs = []string{"ob-9"}

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.Contains(t, bodyOf(t, w)["reason_codes"], "ESR-018")

	var excluded bool
	for _, f := range h.engine.lastPlan.MandatoryMustNot {
		if f.Field == "source_id" && len(f.Values) == 1 && f.Values[0] == "ob-9" {
			excluded = true
		}
	}
	assert.True(t, excluded, "the failed ref must be a mandatory exclusion")
}

func TestSearch_TooManyFailedRestrictionsBlocksTheScope(t *testing.T) {
	h := newHarness(t, nil)
	refs := make([]string, maxRestrictionExclusions+1)
	for i := range refs {
		refs[i] = "ob"
	}
	h.store.failedRefs = refs

	w := do(h, req(t, http.MethodPost, "/v1/search", map[string]any{"scope": "obligation", "query": "GST"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "ESR-018", bodyOf(t, w)["reason_code"])
}

func TestRetrieve_AppliesTheSameHealthGate(t *testing.T) {
	h := newHarness(t, nil)
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessStale, ObservedAt: time.Now()}
	w := do(h, req(t, http.MethodPost, "/v1/retrieve", map[string]any{
		"scope": "obligation", "refs": []map[string]string{{"source_type": "obligation", "source_id": "ob-1"}},
	}))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// ── POST /v1/search/semantic ─────────────────────────────────────────────────

func TestSemantic_HappyPathEmbedsWithThePinAndFiltersInsideTheWalk(t *testing.T) {
	h, emb := semanticHarness(t)
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{
		"scope": "obligation", "query": "which filings are overdue?", "size": 5,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, emb.calls)
	assert.Equal(t, "m@1", bodyOf(t, w)["embedding_model"])

	plan := h.engine.lastPlan
	assert.Len(t, plan.Vector, 3)
	assert.Equal(t, 5, plan.K)
	var tenant bool
	for _, f := range plan.MandatoryFilters {
		if f.Field == "tenant_id" && f.Values[0] == tenantA {
			tenant = true
		}
	}
	assert.True(t, tenant)
	require.NotEmpty(t, h.store.evidence, "semantic searches leave evidence like lexical ones")
	assert.NotContains(t, h.store.evidence[len(h.store.evidence)-1].QueryDigest, "overdue", "digest, never text (INV-17)")
}

// A refused request never reaches the embedding provider.
func TestSemantic_RefusedRequestsAreNeverEmbedded(t *testing.T) {
	h, emb := semanticHarness(t)
	h.store.checkpoint = &domain.IndexCheckpoint{Freshness: domain.FreshnessStale, ObservedAt: time.Now()}
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{"scope": "obligation", "query": "overdue"}))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	r := req(t, http.MethodPost, "/v1/search/semantic", map[string]any{"scope": "obligation", "query": "overdue"})
	r.Header.Del("X-Purpose-Context")
	do(h, r)
	assert.Zero(t, emb.calls)
}

// Fail closed without a provider — ESR-019, never a lexical fallback.
func TestSemantic_NoProviderIsESR019(t *testing.T) {
	h := newHarness(t, nil)
	h.store.contract.Embedding = pin()
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{"scope": "obligation", "query": "overdue"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "ESR-019", bodyOf(t, w)["reason_code"])
	assert.Zero(t, h.engine.lastPlan.K, "nothing was executed")
}

// NP-35 at query time.
func TestSemantic_ProviderModelDriftIsESR019(t *testing.T) {
	h, emb := semanticHarness(t)
	emb.err = embedding.ErrModelMismatch
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{"scope": "obligation", "query": "overdue"}))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "ESR-019", bodyOf(t, w)["reason_code"])
}

func TestSemantic_CallerExpectingAnotherModelGets409(t *testing.T) {
	h, _ := semanticHarness(t)
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{
		"scope": "obligation", "query": "overdue", "model": "m@2",
	}))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "ESR-019", bodyOf(t, w)["reason_code"])
}

func TestSemantic_LexicalScopeIsNotRegistered(t *testing.T) {
	h := newHarness(t, nil)
	h.handler.embedder = &stubEmbedder{}
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{"scope": "obligation", "query": "overdue"}))
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "ESR-002", bodyOf(t, w)["reason_code"])
}

// A caller cannot supply its own vector — the body has nowhere to put one.
func TestSemantic_CallerSuppliedVectorIsRejected(t *testing.T) {
	h, emb := semanticHarness(t)
	w := do(h, req(t, http.MethodPost, "/v1/search/semantic", map[string]any{
		"scope": "obligation", "query": "overdue", "vector": []float32{1, 2, 3},
	}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Zero(t, emb.calls)
}

// ── control plane ────────────────────────────────────────────────────────────

func contractBody(emb map[string]any) map[string]any {
	body := map[string]any{
		"source_type": "obligation", "authz_action": "OBLIGATION_READ",
		"fields": []map[string]any{
			{"name": "obligation_code", "type": "TEXT", "searchable": true, "returnable": true},
			{"name": "api_secret", "type": "KEYWORD", "sensitivity_class": "SECRET_PROHIBITED"},
			{"name": "due", "type": "DATE", "returnable": true},
		},
	}
	if emb != nil {
		body["embedding"] = emb
	}
	return body
}

func TestCreateContract_EmbeddingPinIsValidated(t *testing.T) {
	cases := map[string]map[string]any{
		"no version":        {"model": "m", "dimensions": 3, "source_fields": []string{"obligation_code"}},
		"latest":            {"model": "m", "model_version": "latest", "dimensions": 3, "source_fields": []string{"obligation_code"}},
		"no width":          {"model": "m", "model_version": "1", "source_fields": []string{"obligation_code"}},
		"unregistered":      {"model": "m", "model_version": "1", "dimensions": 3, "source_fields": []string{"nope"}},
		"prohibited":        {"model": "m", "model_version": "1", "dimensions": 3, "source_fields": []string{"api_secret"}},
		"not text":          {"model": "m", "model_version": "1", "dimensions": 3, "source_fields": []string{"due"}},
		"bad preprocessing": {"model": "m", "model_version": "1", "dimensions": 3, "source_fields": []string{"obligation_code"}, "preprocessing": "raw"},
	}
	for name, emb := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.store.sources = []domain.SearchSource{{SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityRestricted}}
			w := do(h, req(t, http.MethodPost, "/v1/index-contracts", contractBody(emb)))
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, "invalid_embedding_contract", bodyOf(t, w)["error"])
		})
	}
}

func TestCreateContract_ValidPinIsStoredAndDigested(t *testing.T) {
	h := newHarness(t, nil)
	h.store.sources = []domain.SearchSource{{SourceID: "s-1", SourceType: "obligation", SensitivityCeiling: domain.SensitivityRestricted}}
	lexical := bodyOf(t, do(h, req(t, http.MethodPost, "/v1/index-contracts", contractBody(nil))))
	semantic := bodyOf(t, do(h, req(t, http.MethodPost, "/v1/index-contracts", contractBody(map[string]any{
		"model": "m", "model_version": "1", "dimensions": 3, "source_fields": []string{"obligation_code"},
	}))))
	require.NotNil(t, semantic["embedding"])
	assert.Equal(t, "nfkc-ws-v1", semantic["embedding"].(map[string]any)["preprocessing"])
	assert.NotEqual(t, lexical["schema_digest"], semantic["schema_digest"], "the pin is part of the contract's identity")
}

func TestCreateGeneration_SemanticScopeRefusedWithoutProvider(t *testing.T) {
	h := newHarness(t, nil)
	h.store.contract.Embedding = pin()
	w := do(h, req(t, http.MethodPost, "/v1/index-generations", map[string]any{"scope": "obligation"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, "ESR-019", bodyOf(t, w)["reason_code"])
}

// §10.1: a model migration cannot reach READY without a passing evaluation.
func migrationHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.handler.embedder = &stubEmbedder{}

	serving := testContract()
	serving.Embedding = &domain.EmbeddingSpec{Model: "m", ModelVersion: "0", Dimensions: 3,
		SourceFields: []string{"obligation_code"}, Preprocessing: "nfkc-ws-v1", Similarity: "cosinesimil"}
	candidate := testContract()
	candidate.ContractID = "77777777-7777-7777-7777-777777777777"
	candidate.Version = 2
	candidate.Embedding = pin()

	candidateGen := &domain.IndexGeneration{GenerationID: "88888888-8888-8888-8888-888888888888",
		ContractID: candidate.ContractID, ScopeName: "obligation", PhysicalIndex: "obligation-gnew",
		State: domain.GenerationValidating, BackfillState: domain.BackfillComplete}

	// The real state during a migration: publishing v2 retired v1, and v1's
	// generation keeps serving with v1's own contract.
	serving.State = domain.ContractRetired
	candidate.State = domain.ContractPublished
	h.store.contract = serving
	h.store.contractsByID = map[string]*domain.IndexContract{serving.ContractID: serving, candidate.ContractID: candidate}
	h.store.generationsByID = map[string]*domain.IndexGeneration{candidateGen.GenerationID: candidateGen}
	h.engine.count = 42 // the candidate is fully backfilled: it holds what the ledger holds
	return h
}

func TestTransitionGeneration_MigrationNeedsCertification(t *testing.T) {
	h := migrationHarness(t)
	path := "/v1/index-generations/88888888-8888-8888-8888-888888888888/state"

	w := do(h, req(t, http.MethodPost, path, map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Equal(t, "retrieval_evaluation_required", bodyOf(t, w)["error"])

	h.store.evaluations = []domain.RetrievalEvaluation{{GenerationID: "88888888-8888-8888-8888-888888888888",
		PinnedModel: "m@1", K: 10, Recall: 0.4, MinRecall: 0.8, Passed: false}}
	w = do(h, req(t, http.MethodPost, path, map[string]any{"state": "READY"}))
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "retrieval_evaluation_failed", bodyOf(t, w)["error"])

	h.store.evaluations = append(h.store.evaluations, domain.RetrievalEvaluation{
		GenerationID: "88888888-8888-8888-8888-888888888888", PinnedModel: "m@1", K: 10, Recall: 0.9, MinRecall: 0.8, Passed: true})
	w = do(h, req(t, http.MethodPost, path, map[string]any{"state": "READY"}))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// recall@k is measured inside each case's tenant and model, and only numbers
// come back.
func TestEvaluateRetrieval_MeasuresRecallInsideTheTenant(t *testing.T) {
	h := migrationHarness(t)
	h.engine.result = searchclient.Result{Hits: []searchclient.Hit{
		{Source: map[string]any{"source_id": "ob-1"}},
		{Source: map[string]any{"source_id": "ob-3"}},
	}}
	w := do(h, req(t, http.MethodPost, "/v1/index-generations/88888888-8888-8888-8888-888888888888/retrieval-evaluations",
		map[string]any{"k": 5, "min_recall": 0.5, "cases": []map[string]any{
			{"query": "overdue", "expected_source_ids": []string{"ob-1", "ob-2"}},
		}}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	body := bodyOf(t, w)
	assert.Equal(t, 0.5, body["recall"])
	assert.Equal(t, true, body["passed"])
	assert.NotContains(t, w.Body.String(), "ob-3", "the evaluator learns numbers, never hits")

	var tenant, model bool
	for _, f := range h.engine.lastPlan.MandatoryFilters {
		tenant = tenant || (f.Field == "tenant_id" && f.Values[0] == tenantA)
		model = model || (f.Field == "embedding_model" && f.Values[0] == "m@1")
	}
	assert.True(t, tenant && model)
	require.Len(t, h.store.evaluations, 1)
	assert.NotContains(t, h.store.evaluations[0].CasesDigest, "overdue")
}

func TestEvaluateRetrieval_RequiresPlatformGrant(t *testing.T) {
	h := migrationHarness(t)
	h.authz.err = authz.ErrDenied
	w := do(h, req(t, http.MethodPost, "/v1/index-generations/88888888-8888-8888-8888-888888888888/retrieval-evaluations",
		map[string]any{"k": 5, "min_recall": 0.5, "cases": []map[string]any{
			{"query": "overdue", "expected_source_ids": []string{"ob-1"}},
		}}))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
