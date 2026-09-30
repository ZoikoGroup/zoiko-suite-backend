package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/embedding"
	"zoiko.io/search-indexer-svc/internal/envelope"
	"zoiko.io/search-indexer-svc/internal/query"
)

// ── POST /v1/search/semantic ─────────────────────────────────────────────────

// SearchSemantic executes one governed semantic or hybrid retrieval (§11.1).
//
// "Same policy envelope as lexical search": the same trusted context, the same
// Compile ordering, the same scope-health gate, the same per-hit
// re-authorization, hydration and suppression in the retriever. What differs is
// only how candidates are found — nearest neighbours to a query vector the
// pinned model produced — and §10.1 is explicit that that difference buys no
// authority: "similarity score is not authorization."
//
// The query text is embedded only AFTER the plan compiled and the scope passed
// its health gate. A request that would be refused never reaches the embedding
// provider, so the provider never learns what an unauthorised caller was
// looking for.
func (h *Handler) SearchSemantic(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	env := envelope.MustFromContext(r.Context())

	var req query.SemanticRequest
	if !decode(w, r, &req) {
		return
	}

	tc := h.trustedContext(env)
	if tc.TenantID == "" {
		h.refuse(w, r, http.StatusUnauthorized, domain.ReasonTenantContextMissing,
			"no verified tenant on the request", req.Scope, tc)
		return
	}

	contract, generation, err := h.resolveScope(r.Context(), req.Scope)
	if err != nil {
		code := domain.ReasonScopeNotRegistered
		if errors.Is(err, errNoActiveGeneration) {
			code = domain.ReasonGenerationNotActive
		}
		h.refuse(w, r, http.StatusNotFound, code, err.Error(), req.Scope, tc)
		return
	}

	plan, err := h.planner.PlanSemantic(req, tc, contract, generation)
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.metrics.QueryRejectionsTotal.WithLabelValues(req.Scope, string(qerr.Code)).Inc()
			status := statusFor(qerr.Code)
			if qerr.Code == domain.ReasonScopeNotRegistered {
				status = http.StatusNotFound
			}
			h.refuse(w, r, status, qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "compile semantic plan", err)
		return
	}

	health, herr := h.checkScopeHealth(r.Context(), tc.TenantID, contract, generation)
	if herr != nil {
		h.metrics.QueryRejectionsTotal.WithLabelValues(req.Scope, string(herr.Code)).Inc()
		h.refuse(w, r, statusFor(herr.Code), herr.Code, herr.Detail, req.Scope, tc)
		return
	}
	health.apply(plan)

	vectors, err := h.embedder.Embed(r.Context(), *contract.Embedding, []string{plan.Execution.Text})
	if err != nil {
		// ESR-019 for all three causes, distinguished in the detail. NP-35:
		// "pinned model/version mismatch blocks ... query generation" — and a
		// pinned model that cannot be reached at all is the same refusal, not
		// a reason to fall back to a lexical answer the caller did not ask for.
		detail := "the scope's pinned embedding model could not produce a query vector: " + err.Error()
		switch {
		case errors.Is(err, embedding.ErrNoProvider):
			detail = "no embedding provider is configured (OD-10 is open); semantic retrieval is unavailable"
		case errors.Is(err, embedding.ErrModelMismatch):
			h.log.Error("NP-35: embedding provider answered with a model other than the scope's pin",
				zap.String("scope", plan.Scope), zap.String("pinned", plan.PinnedModel), zap.Error(err))
		}
		h.refuse(w, r, http.StatusServiceUnavailable, domain.ReasonSemanticModelMismatch, detail, req.Scope, tc)
		return
	}
	plan.Execution.Vector = vectors[0]

	resp, err := h.retriever.Execute(r.Context(), plan, tc, nil)
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.refuse(w, r, statusFor(qerr.Code), qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "execute semantic search", err)
		return
	}

	h.metrics.SearchDurationSecs.WithLabelValues(plan.Scope).Observe(time.Since(started).Seconds())
	h.recordEvidence(r.Context(), domain.SearchEvidence{
		EvidenceID:      uuid.NewString(),
		RequestID:       env.RequestID,
		CorrelationID:   env.CorrelationID,
		TenantID:        tc.TenantID,
		ActorID:         tc.ActorID,
		WorkloadID:      tc.WorkloadID,
		OnBehalfOfID:    tc.OnBehalfOf,
		Purpose:         tc.Purpose,
		ScopeName:       plan.Scope,
		QueryDigest:     plan.QueryDigest,
		FiltersDigest:   plan.FiltersDigest,
		PlanDigest:      plan.PlanDigest,
		IndexGeneration: generation.GenerationID,
		PartitionSet:    plan.PartitionSet,
		ComplexityScore: plan.ComplexityScore,
		ResultCount:     len(resp.Results),
		SuppressedCount: totalSuppressed(resp),
		Completeness:    resp.Completeness,
		ReasonCodes:     resp.ReasonCodes,
		DurationMS:      time.Since(started).Milliseconds(),
		EmbeddingModel:  plan.PinnedModel,
	})

	if resp.Completeness != domain.CompletenessComplete && h.events != nil {
		_ = h.events.SearchDegraded(r.Context(), tc.TenantID, plan.Scope,
			resp.CompletenessDetail, string(resp.Completeness), plan.PartitionSet, env.CorrelationID)
	}

	status := http.StatusOK
	if resp.Completeness != domain.CompletenessComplete {
		status = http.StatusPartialContent
	}
	writeJSON(w, status, resp)
}

// ── §10.1 retrieval-evaluation certification ─────────────────────────────────

// evaluationCase is one gold-set query. There is no tenant field: every case
// runs inside the EVALUATOR's own verified tenant. Letting the request name a
// tenant made the evaluation an existence oracle — a platform operator could
// ask "is record X near topic Y in tenant Z" two hundred times a call, with
// recall as the answer (re-audit, 30 Sep 2026). Certification needs a gold set
// in a tenant the certifier is entitled to, not a view into every tenant.
type evaluationCase struct {
	Query             string   `json:"query"`
	ExpectedSourceIDs []string `json:"expected_source_ids"`
}

type evaluationRequest struct {
	K         int              `json:"k"`
	MinRecall float64          `json:"min_recall"`
	Cases     []evaluationCase `json:"cases"`
}

const (
	maxEvaluationCases    = 200
	maxExpectedPerCase    = 100
	evaluationEngineBatch = 32
)

// EvaluateRetrieval certifies a candidate semantic generation against a gold
// set before it may cut over (§10.1: "model migration requires parallel
// rebuild and retrieval-evaluation certification before cutover").
//
// It measures recall@k: for each case, the share of the expected source ids
// found in the k nearest neighbours of the case's query, inside the case's
// tenant, among documents embedded by the generation's pinned model. The mean
// is compared with min_recall, which the operator states because OD-15 leaves
// quantitative relevance thresholds open — and which is stored beside the
// result, so a pass is never read without the bar it cleared.
//
// The response carries numbers only. A platform operator evaluating a
// generation learns how well it retrieves, never what it retrieved: the hits
// are compared with the expected ids here and discarded (INV-30 — the
// administrative plane is never a tenant-facing authority).
func (h *Handler) EvaluateRetrieval(w http.ResponseWriter, r *http.Request) {
	env, ok := h.requirePlatform(w, r, ActionGenerationCreate)
	if !ok {
		return
	}
	var req evaluationRequest
	if !decode(w, r, &req) {
		return
	}
	generationID, ok := uuidParam(w, r, "generationID")
	if !ok {
		return
	}

	if len(req.Cases) == 0 || len(req.Cases) > maxEvaluationCases {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("an evaluation needs between 1 and %d cases", maxEvaluationCases))
		return
	}
	if req.MinRecall <= 0 || req.MinRecall > 1 {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"min_recall must be in (0, 1]; the threshold is the operator's to state (OD-15)")
		return
	}
	if req.K <= 0 || req.K > h.maxResultWindow {
		writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("k must be between 1 and %d", h.maxResultWindow))
		return
	}
	if env.TenantID == "" {
		writeErrorCode(w, http.StatusUnauthorized, "tenant_context_missing",
			"an evaluation runs inside the caller's verified tenant", domain.ReasonTenantContextMissing)
		return
	}
	for i, c := range req.Cases {
		if strings.TrimSpace(c.Query) == "" || len(c.ExpectedSourceIDs) == 0 || len(c.ExpectedSourceIDs) > maxExpectedPerCase {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("case %d: needs a query and 1..%d expected_source_ids", i, maxExpectedPerCase))
			return
		}
	}

	gen, err := h.store.GetGeneration(r.Context(), generationID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such generation")
			return
		}
		h.internal(w, r, "get generation", err)
		return
	}
	switch gen.State {
	case domain.GenerationBuilding, domain.GenerationValidating, domain.GenerationReady, domain.GenerationActive:
	default:
		writeError(w, http.StatusConflict, "invalid_state",
			fmt.Sprintf("a %s generation cannot be evaluated", gen.State))
		return
	}
	contract, err := h.store.GetContract(r.Context(), gen.ContractID)
	if err != nil {
		h.internal(w, r, "get contract", err)
		return
	}
	if contract.Embedding == nil {
		writeErrorCode(w, http.StatusConflict, "not_semantic",
			"this generation's contract pins no embedding model; there is nothing to certify",
			domain.ReasonSemanticModelMismatch)
		return
	}

	queries := make([]string, len(req.Cases))
	for i, c := range req.Cases {
		queries[i] = c.Query
	}
	vectors, err := h.embedCases(r, *contract.Embedding, queries)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "embedding_unavailable",
			"the pinned model could not embed the evaluation queries: "+err.Error(),
			domain.ReasonSemanticModelMismatch)
		return
	}

	pinned := contract.Embedding.PinnedModel()
	// The same restriction-safety exclusion a search applies: a FAILED
	// restriction's record must not count toward — or be probed by — a
	// certification either.
	failed, err := h.store.ListFailedRestrictions(r.Context(), env.TenantID, gen.ScopeName, maxRestrictionExclusions)
	if err != nil {
		h.internal(w, r, "read restriction state", err)
		return
	}
	mustNot := []searchclient.TermFilter{{Field: "tombstoned", Values: []string{"true"}}}
	if len(failed) > 0 {
		mustNot = append(mustNot, searchclient.TermFilter{Field: "source_id", Values: failed})
	}

	var recallSum float64
	for i, c := range req.Cases {
		raw, err := h.engine.ExecutePlan(r.Context(), gen.PhysicalIndex, searchclient.ExecutionPlan{
			MandatoryFilters: []searchclient.TermFilter{
				{Field: "tenant_id", Values: []string{env.TenantID}},
				{Field: "embedding_model", Values: []string{pinned}},
			},
			MandatoryMustNot: mustNot,
			SourceIncludes:   []string{"source_id"},
			Vector:           vectors[i],
			K:                req.K,
			Size:             req.K,
		})
		if err != nil {
			h.metrics.EngineError("search")
			h.internal(w, r, "evaluate case", err)
			return
		}
		found := map[string]bool{}
		for _, hit := range raw.Hits {
			if id, _ := hit.Source["source_id"].(string); id != "" {
				found[id] = true
			}
		}
		hits := 0
		for _, id := range c.ExpectedSourceIDs {
			if found[id] {
				hits++
			}
		}
		recallSum += float64(hits) / float64(len(c.ExpectedSourceIDs))
	}
	recall := recallSum / float64(len(req.Cases))

	casesJSON, _ := json.Marshal(req.Cases)
	sum := sha256.Sum256(casesJSON)
	eval := domain.RetrievalEvaluation{
		EvaluationID:         uuid.NewString(),
		GenerationID:         gen.GenerationID,
		ScopeName:            gen.ScopeName,
		PinnedModel:          pinned,
		K:                    req.K,
		Cases:                len(req.Cases),
		MinRecall:            req.MinRecall,
		Recall:               recall,
		Passed:               recall >= req.MinRecall,
		CasesDigest:          hex.EncodeToString(sum[:]),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: env.ActorSubjectID,
	}
	if err := h.store.CreateRetrievalEvaluation(r.Context(), eval); err != nil {
		h.internal(w, r, "record evaluation", err)
		return
	}
	if !eval.Passed && h.events != nil {
		_ = h.events.ReindexFailed(r.Context(), gen.ScopeName, gen.GenerationID, "RETRIEVAL_EVALUATION",
			fmt.Sprintf("recall@%d %.3f below min_recall %.3f", eval.K, eval.Recall, eval.MinRecall),
			env.CorrelationID)
	}
	writeJSON(w, http.StatusCreated, eval)
}

// embedCases embeds the evaluation queries in bounded batches.
func (h *Handler) embedCases(r *http.Request, spec domain.EmbeddingSpec, queries []string) ([][]float32, error) {
	out := make([][]float32, 0, len(queries))
	for start := 0; start < len(queries); start += evaluationEngineBatch {
		end := start + evaluationEngineBatch
		if end > len(queries) {
			end = len(queries)
		}
		batch, err := h.embedder.Embed(r.Context(), spec, queries[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

// GetRetrievalEvaluation returns a generation's latest certification.
func (h *Handler) GetRetrievalEvaluation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePlatformRead(w, r, ActionGenerationCreate); !ok {
		return
	}
	generationID, ok := uuidParam(w, r, "generationID")
	if !ok {
		return
	}
	eval, err := h.store.LatestRetrievalEvaluation(r.Context(), generationID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "this generation has not been evaluated")
			return
		}
		h.internal(w, r, "get evaluation", err)
		return
	}
	writeJSON(w, http.StatusOK, eval)
}
