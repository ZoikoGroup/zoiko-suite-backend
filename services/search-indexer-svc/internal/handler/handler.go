// Package handler serves the §11.1 canonical APIs.
//
// Two planes, with different authorization models, and keeping them apart is
// the point of the split between this file and admin.go:
//
//	TENANT PLANE  (here)      /v1/search, /v1/retrieve, /v1/restrictions,
//	                          /v1/search-exports, /v1/search-evidence
//	                          Scoped by the caller's verified tenant. Results
//	                          are re-authorized per record.
//
//	CONTROL PLANE (admin.go)  /v1/search-sources, /v1/index-contracts,
//	                          /v1/index-generations, /v1/checkpoints
//	                          Platform-scoped. Every write is authorized
//	                          against PLATFORM before it is attempted.
//
// §9.1 asks for exactly this separation — "administrative engine endpoints are
// isolated from tenant/application traffic and require privileged operational
// access" — and INV-30 adds that an administrative console is "never a
// tenant-facing authority".
package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/search-client/searchclient"
	"zoiko.io/search-indexer-svc/internal/authz"
	"zoiko.io/search-indexer-svc/internal/domain"
	"zoiko.io/search-indexer-svc/internal/envelope"
	"zoiko.io/search-indexer-svc/internal/events"
	"zoiko.io/search-indexer-svc/internal/indexer"
	"zoiko.io/search-indexer-svc/internal/query"
	"zoiko.io/search-indexer-svc/internal/retrieval"
	"zoiko.io/search-indexer-svc/internal/store"
	"zoiko.io/search-indexer-svc/internal/telemetry"
)

// Authorization action names. Registered in access-control-svc's bundles; a
// caller without the grant gets a 403 from authorization-svc, not from here.
const (
	ActionSourceRegister     = "SEARCH_SOURCE_REGISTER"
	ActionContractCreate     = "SEARCH_CONTRACT_CREATE"
	ActionContractTransition = "SEARCH_CONTRACT_TRANSITION"
	ActionGenerationCreate   = "SEARCH_GENERATION_CREATE"
	ActionGenerationActivate = "SEARCH_GENERATION_ACTIVATE"
	ActionRestrictionApply   = "SEARCH_RESTRICTION_APPLY"
	// ActionExport is deliberately its own action, not implied by search.
	// INV-29: "search exports/downloads require separate export
	// authorization; search permission does not imply bulk-exfiltration
	// permission." NP-29 is the same rule stated as an attack.
	ActionExport = "SEARCH_EXPORT"
)

type Handler struct {
	store     store.Store
	engine    searchclient.Engine
	planner   *query.Planner
	retriever *retrieval.Retriever
	indexer   *indexer.Indexer
	authz     authz.Client
	events    *events.Publisher
	metrics   *telemetry.Metrics
	log       *zap.Logger

	// platformScopeID is the legal_entity_id presented for platform-scoped
	// acts. authorization-svc rejects an empty one outright.
	platformScopeID string
	// evidenceKey salts the actor hash in esr.security_filter.denied.
	evidenceKey []byte
}

type Config struct {
	Store           store.Store
	Engine          searchclient.Engine
	Planner         *query.Planner
	Retriever       *retrieval.Retriever
	Indexer         *indexer.Indexer
	AuthZ           authz.Client
	Events          *events.Publisher
	Metrics         *telemetry.Metrics
	Log             *zap.Logger
	PlatformScopeID string
	EvidenceKey     []byte
}

func New(cfg Config) *Handler {
	return &Handler{
		store: cfg.Store, engine: cfg.Engine, planner: cfg.Planner,
		retriever: cfg.Retriever, indexer: cfg.Indexer, authz: cfg.AuthZ,
		events: cfg.Events, metrics: cfg.Metrics, log: cfg.Log,
		platformScopeID: cfg.PlatformScopeID, evidenceKey: cfg.EvidenceKey,
	}
}

// Routes mounts every API route under /v1.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/v1", func(r chi.Router) {
		// ── Tenant plane ─────────────────────────────────────────────────
		r.Post("/search", h.Search)
		r.Post("/retrieve", h.Retrieve)
		r.Get("/scopes", h.ListScopes)
		r.Post("/restrictions", h.ApplyRestriction)
		r.Get("/restrictions", h.ListRestrictions)
		r.Post("/search-exports", h.RequestExport)
		r.Get("/search-evidence", h.ListEvidence)

		// ── Control plane ────────────────────────────────────────────────
		r.Post("/search-sources", h.CreateSource)
		r.Get("/search-sources", h.ListSources)
		r.Post("/index-contracts", h.CreateContract)
		r.Get("/index-contracts", h.ListContracts)
		r.Get("/index-contracts/{contractID}", h.GetContract)
		r.Post("/index-contracts/{contractID}/state", h.TransitionContract)
		r.Post("/index-generations", h.CreateGeneration)
		r.Get("/index-generations", h.ListGenerations)
		r.Post("/index-generations/{generationID}/state", h.TransitionGeneration)
		r.Get("/checkpoints", h.ListCheckpoints)
	})
}

// ── POST /v1/search ──────────────────────────────────────────────────────────

// Search executes one governed search.
//
// The whole §1.3 decision ordering runs here, in order, and the evidence row
// is written on EVERY path — including refusals. An evidence table that only
// records successful searches cannot answer "what was this actor trying to
// reach", which is the question an abuse investigation starts from.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	env := envelope.MustFromContext(r.Context())

	var req query.Request
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

	plan, err := h.planner.Compile(r.Context(), req, tc, contract, generation)
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.metrics.QueryRejectionsTotal.WithLabelValues(req.Scope, string(qerr.Code)).Inc()
			h.refuse(w, r, statusFor(qerr.Code), qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "compile query plan", err)
		return
	}

	resp, err := h.retriever.Execute(r.Context(), plan, tc, func(lastSort []any) (string, error) {
		return h.planner.NextCursor(plan, tc.TenantID, lastSort)
	})
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.refuse(w, r, statusFor(qerr.Code), qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "execute search", err)
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
	})

	if resp.Completeness != domain.CompletenessComplete && h.events != nil {
		_ = h.events.SearchDegraded(r.Context(), tc.TenantID, plan.Scope,
			resp.CompletenessDetail, string(resp.Completeness), plan.PartitionSet, env.CorrelationID)
	}

	// 206 for a partial answer, not 200. INV-24 says partial results are
	// "explicit and never silently represented as complete" — and a status
	// code is the one part of a response a caller cannot overlook, where a
	// field in the body can be and routinely is.
	status := http.StatusOK
	if resp.Completeness != domain.CompletenessComplete {
		status = http.StatusPartialContent
	}
	writeJSON(w, status, resp)
}

// ── POST /v1/retrieve ────────────────────────────────────────────────────────

type retrieveRequest struct {
	Scope string `json:"scope"`
	Refs  []struct {
		SourceType string `json:"source_type"`
		SourceID   string `json:"source_id"`
	} `json:"refs"`
}

// Retrieve re-authorizes and hydrates specific candidate refs.
//
// §11.1: "each resource re-authorized; bulk limits enforced." The bulk limit
// is what stops this becoming an export route with no export authorization —
// a caller that could name 10 000 refs in one call has bulk-exfiltration
// capability whatever the route is named (INV-29).
const maxRetrieveRefs = 50

func (h *Handler) Retrieve(w http.ResponseWriter, r *http.Request) {
	env := envelope.MustFromContext(r.Context())
	var req retrieveRequest
	if !decode(w, r, &req) {
		return
	}

	tc := h.trustedContext(env)
	if tc.TenantID == "" {
		h.refuse(w, r, http.StatusUnauthorized, domain.ReasonTenantContextMissing,
			"no verified tenant on the request", req.Scope, tc)
		return
	}
	if len(req.Refs) == 0 {
		writeError(w, http.StatusBadRequest, "refs_required", "at least one source ref is required")
		return
	}
	if len(req.Refs) > maxRetrieveRefs {
		h.refuse(w, r, http.StatusBadRequest, domain.ReasonExportAuthzRequired,
			"a retrieve may name at most "+strconv.Itoa(maxRetrieveRefs)+
				" refs; larger populations require an authorized export", req.Scope, tc)
		return
	}

	contract, generation, err := h.resolveScope(r.Context(), req.Scope)
	if err != nil {
		h.refuse(w, r, http.StatusNotFound, domain.ReasonScopeNotRegistered, err.Error(), req.Scope, tc)
		return
	}

	// Retrieval by ref is a search whose only filter is the ref set. Built as
	// a plan rather than as direct engine gets, so the mandatory tenant,
	// residency and tombstone filters apply identically — a second code path
	// that fetched by id would be a second place for those filters to be
	// forgotten, which is how an insecure-direct-object-reference gets in
	// (§9.1: "opaque identifiers and mandatory filters prevent insecure direct
	// object/index reference patterns").
	ids := make([]string, 0, len(req.Refs))
	for _, ref := range req.Refs {
		if ref.SourceID == "" {
			continue
		}
		sourceType := ref.SourceType
		if sourceType == "" {
			sourceType = contract.ScopeName
		}
		ids = append(ids, searchclient.ProjectionDocID(tc.TenantID, sourceType, ref.SourceID))
	}

	plan, err := h.planner.Compile(r.Context(), query.Request{
		Scope: req.Scope,
		Size:  len(ids),
	}, tc, contract, generation)
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.refuse(w, r, statusFor(qerr.Code), qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "compile retrieve plan", err)
		return
	}
	// The ref set is added as a MANDATORY filter, not a user filter: it is
	// not narrowing the caller chose from registered fields, it is the
	// request itself, and _id is not a contract field.
	plan.Execution.MandatoryFilters = append(plan.Execution.MandatoryFilters,
		searchclient.TermFilter{Field: "_id", Values: ids})

	resp, err := h.retriever.Execute(r.Context(), plan, tc, nil)
	if err != nil {
		var qerr *query.Error
		if errors.As(err, &qerr) {
			h.refuse(w, r, statusFor(qerr.Code), qerr.Code, qerr.Detail, req.Scope, tc)
			return
		}
		h.internal(w, r, "execute retrieve", err)
		return
	}

	status := http.StatusOK
	if resp.Completeness != domain.CompletenessComplete {
		status = http.StatusPartialContent
	}
	writeJSON(w, status, resp)
}

// ── POST /v1/search-exports ──────────────────────────────────────────────────

type exportRequest struct {
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
	MaxRecords int    `json:"max_records"`
}

// RequestExport records an export request against its own authorization.
//
// It does NOT stream data. The route exists to make INV-29's boundary
// explicit and enforced: an export is a separately-authorized act with its own
// purpose and its own evidence, and the population it covers is recorded
// before anything moves. Wiring an actual bulk writer behind this needs the
// OD-13 decision on maximums and async thresholds, which is not this service's
// to make — so it answers 202 with the recorded authorization rather than
// inventing a limit.
func (h *Handler) RequestExport(w http.ResponseWriter, r *http.Request) {
	env := envelope.MustFromContext(r.Context())
	var req exportRequest
	if !decode(w, r, &req) {
		return
	}

	tc := h.trustedContext(env)
	if tc.TenantID == "" {
		h.refuse(w, r, http.StatusUnauthorized, domain.ReasonTenantContextMissing,
			"no verified tenant on the request", req.Scope, tc)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "reason_required",
			"an export must state its reason; search purpose does not carry over")
		return
	}

	contract, generation, err := h.resolveScope(r.Context(), req.Scope)
	if err != nil {
		h.refuse(w, r, http.StatusNotFound, domain.ReasonScopeNotRegistered, err.Error(), req.Scope, tc)
		return
	}

	entity := tc.LegalEntityID
	if entity == "" {
		entity = h.platformScopeID
	}
	if err := h.authz.CheckAllowed(r.Context(), tc.ActorID, entity, ActionExport); err != nil {
		h.metrics.AuthzDecision(ActionExport, outcomeOf(err))
		if errors.Is(err, authz.ErrDenied) {
			h.refuse(w, r, http.StatusForbidden, domain.ReasonExportAuthzRequired,
				"export is a separately authorized act and this principal does not hold "+ActionExport,
				req.Scope, tc)
			return
		}
		h.refuse(w, r, http.StatusServiceUnavailable, domain.ReasonAuthorizationIndet,
			"export authorization could not be obtained", req.Scope, tc)
		return
	}
	h.metrics.AuthzDecision(ActionExport, "allowed")

	live, _, err := h.store.CountProjections(r.Context(), contract.ScopeName)
	if err != nil {
		h.internal(w, r, "count export population", err)
		return
	}

	evidenceID := uuid.NewString()
	h.recordEvidence(r.Context(), domain.SearchEvidence{
		EvidenceID:      evidenceID,
		RequestID:       env.RequestID,
		CorrelationID:   env.CorrelationID,
		TenantID:        tc.TenantID,
		ActorID:         tc.ActorID,
		Purpose:         req.Reason,
		ScopeName:       contract.ScopeName,
		QueryDigest:     "export",
		FiltersDigest:   "export",
		PlanDigest:      "export",
		IndexGeneration: generation.GenerationID,
		PartitionSet:    []string{generation.PhysicalIndex},
		ResultCount:     int(live),
		Completeness:    domain.CompletenessUnknown,
		ReasonCodes:     []string{string(domain.ReasonExportAuthzRequired)},
	})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"export_id":           evidenceID,
		"scope":               contract.ScopeName,
		"index_generation":    generation.GenerationID,
		"eligible_population": live,
		"status":              "AUTHORIZED",
		"detail": "Export authorization and population are recorded as evidence. " +
			"Bulk delivery is gated on the OD-13 decision for export maximums and " +
			"async thresholds; no records have been transferred.",
	})
}

// ── POST /v1/restrictions ────────────────────────────────────────────────────

type restrictionRequest struct {
	Scope         string `json:"scope"`
	SourceType    string `json:"source_type"`
	SourceID      string `json:"source_id"`
	Reason        string `json:"reason"`
	SourceEventID string `json:"source_event_id"`
}

// ApplyRestriction applies a priority visibility removal.
//
// The HTTP twin of the event-driven restriction lane, for the callers that
// have no event to emit — PRV's erasure workflow and DRC's disposition both
// need to remove search visibility for a specific record on request, and
// waiting for a domain event that may never come is not an answer to an
// erasure obligation.
func (h *Handler) ApplyRestriction(w http.ResponseWriter, r *http.Request) {
	env := envelope.MustFromContext(r.Context())
	var req restrictionRequest
	if !decode(w, r, &req) {
		return
	}

	tc := h.trustedContext(env)
	if tc.TenantID == "" {
		h.refuse(w, r, http.StatusUnauthorized, domain.ReasonTenantContextMissing,
			"no verified tenant on the request", req.Scope, tc)
		return
	}
	if req.SourceType == "" || req.SourceID == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"source_type, source_id and reason are required")
		return
	}
	if req.SourceEventID == "" {
		// §11.1: "source event ID idempotency; older writes cannot resurrect
		// visibility." Without one there is no idempotency key, so a retried
		// erasure would write a second tombstone at a new epoch — harmless
		// here, but it would also make NP-47's duplicate-replay guarantee
		// untestable. Required rather than generated.
		writeError(w, http.StatusBadRequest, "source_event_id_required",
			"a restriction must name the authoritative source event it derives from")
		return
	}

	entity := tc.LegalEntityID
	if entity == "" {
		entity = h.platformScopeID
	}
	if err := h.authz.CheckAllowed(r.Context(), tc.ActorID, entity, ActionRestrictionApply); err != nil {
		h.metrics.AuthzDecision(ActionRestrictionApply, outcomeOf(err))
		h.writeAuthzError(w, err)
		return
	}
	h.metrics.AuthzDecision(ActionRestrictionApply, "allowed")

	now := time.Now().UTC()
	tomb := domain.RestrictionTombstone{
		TenantID:      tc.TenantID,
		ScopeName:     req.Scope,
		SourceType:    req.SourceType,
		SourceID:      req.SourceID,
		Reason:        req.Reason,
		Epoch:         now.UnixMilli(),
		SourceEventID: req.SourceEventID,
		EffectiveAt:   now,
		State:         domain.PropagationPending,
	}

	applied, err := h.store.UpsertTombstone(r.Context(), tomb, uuid.NewString())
	if err != nil {
		if errors.Is(err, domain.ErrStaleEpoch) {
			// NP-11 / NP-48. A restriction older than one already applied is
			// refused, and 409 rather than 200: the caller asked for
			// something that did not happen, and telling it otherwise would
			// let a privacy workflow record an erasure that was declined.
			writeErrorCode(w, http.StatusConflict, "stale_restriction_epoch",
				err.Error(), domain.ReasonRestrictionEpochMismtch)
			return
		}
		h.internal(w, r, "record restriction", err)
		return
	}
	if !applied {
		// Idempotent replay of the same source event. 200 with applied:false
		// — the end state the caller wanted is in place, which is the honest
		// answer to a duplicate.
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": false,
			"state":   string(domain.PropagationApplied),
			"detail":  "this source event has already been applied",
		})
		return
	}

	// Apply to the index immediately rather than waiting for the sweep. §8.2:
	// "visibility-reducing events outrank normal indexing backlog", and a
	// restriction that sat in a queue behind ordinary indexing would be
	// exactly the generic eventual-consistency treatment §13.2 forbids.
	state := domain.PropagationApplied
	detail := ""
	if err := h.applyTombstoneToIndex(r.Context(), tomb); err != nil {
		state = domain.PropagationFailed
		detail = err.Error()
		h.log.Error("restriction recorded but the index write failed — the verifier will retry",
			zap.String("scope", req.Scope), zap.String("source_id", req.SourceID), zap.Error(err))
	}
	_ = h.store.MarkTombstoneState(r.Context(), tomb.TenantID, tomb.SourceType, tomb.SourceID,
		tomb.SourceEventID, state, detail)
	h.metrics.RestrictionsTotal.WithLabelValues(req.Scope, string(state)).Inc()

	status := http.StatusAccepted
	if state == domain.PropagationFailed {
		// 202 would say "accepted, in progress". It failed, and the verifier
		// retrying it later does not make the current state a success.
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"applied":  true,
		"state":    string(state),
		"epoch":    tomb.Epoch,
		"detail":   detail,
		"verified": false,
		"verified_note": "APPLIED is not VERIFIED until search invisibility has been tested; " +
			"poll GET /v1/restrictions for the verified state.",
	})
}

// applyTombstoneToIndex writes the tombstone into the scope's live generation.
func (h *Handler) applyTombstoneToIndex(ctx context.Context, t domain.RestrictionTombstone) error {
	gen, err := h.store.GetActiveGeneration(ctx, t.ScopeName)
	if err != nil {
		return err
	}
	docID := searchclient.ProjectionDocID(t.TenantID, t.SourceType, t.SourceID)
	existing, found, err := h.engine.GetProjection(ctx, gen.PhysicalIndex, docID)
	if err != nil {
		return err
	}
	if !found {
		// Nothing indexed under that ref. A tombstone is still written, so a
		// document that arrives LATER — an in-flight event, a replay — is
		// refused by the ledger's epoch comparison rather than indexed into
		// visibility. This is the out-of-order half of NP-48, and skipping it
		// because "there is nothing to remove" is how the resurrection
		// happens.
		return h.engine.IndexProjection(ctx, gen.PhysicalIndex, searchclient.Projection{
			DocID:            docID,
			TenantID:         t.TenantID,
			SourceType:       t.SourceType,
			SourceID:         t.SourceID,
			RestrictionEpoch: t.Epoch,
			IndexGeneration:  gen.GenerationID,
			Tombstoned:       true,
			TombstoneReason:  t.Reason,
			TombstoneSource:  t.SourceEventID,
			Fields:           map[string]any{},
		})
	}

	var version int64
	if v, ok := existing["source_version"].(float64); ok {
		version = int64(v)
	}
	return h.engine.IndexProjection(ctx, gen.PhysicalIndex, searchclient.Projection{
		DocID:            docID,
		TenantID:         t.TenantID,
		LegalEntityID:    str(existing, "legal_entity_id"),
		ResidencyRegion:  str(existing, "residency_region"),
		SourceType:       t.SourceType,
		SourceID:         t.SourceID,
		SourceVersion:    version,
		RestrictionEpoch: t.Epoch,
		SensitivityClass: str(existing, "sensitivity_class"),
		RetrievalClass:   str(existing, "retrieval_class"),
		IndexGeneration:  gen.GenerationID,
		Tombstoned:       true,
		TombstoneReason:  t.Reason,
		TombstoneSource:  t.SourceEventID,
		// Content dropped. The lineage stays so the tombstone can be ordered
		// and audited; the fields go, because leaving them would mean the
		// restricted content is still in the index and only a filter stands
		// between it and a query.
		Fields: map[string]any{},
	})
}

// ListRestrictions returns this tenant's restriction propagation state.
func (h *Handler) ListRestrictions(w http.ResponseWriter, r *http.Request) {
	env := envelope.MustFromContext(r.Context())
	if env.TenantID == "" {
		writeErrorCode(w, http.StatusUnauthorized, "tenant_context_missing",
			"no verified tenant on the request", domain.ReasonTenantContextMissing)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.store.ListTombstones(r.Context(), env.TenantID, r.URL.Query().Get("scope"), limit)
	if err != nil {
		h.internal(w, r, "list restrictions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restrictions": out})
}

// ListEvidence returns this tenant's search evidence.
func (h *Handler) ListEvidence(w http.ResponseWriter, r *http.Request) {
	env := envelope.MustFromContext(r.Context())
	if env.TenantID == "" {
		writeErrorCode(w, http.StatusUnauthorized, "tenant_context_missing",
			"no verified tenant on the request", domain.ReasonTenantContextMissing)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := h.store.ListEvidence(r.Context(), env.TenantID, r.URL.Query().Get("scope"), limit)
	if err != nil {
		h.internal(w, r, "list evidence", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": out})
}

// ListScopes returns the search surfaces available to this caller.
//
// Contract metadata only — scope name, retrieval class, freshness, and the
// field capabilities a client needs to build a valid request. No counts and no
// per-tenant state: a scope listing that said how many documents a tenant has
// would be a cardinality disclosure before any query was authorized (§7.3).
func (h *Handler) ListScopes(w http.ResponseWriter, r *http.Request) {
	contracts, err := h.store.ListContracts(r.Context(), "")
	if err != nil {
		h.internal(w, r, "list scopes", err)
		return
	}

	type fieldView struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Searchable bool   `json:"searchable"`
		Filterable bool   `json:"filterable"`
		Facetable  bool   `json:"facetable"`
		Sortable   bool   `json:"sortable"`
		Returnable bool   `json:"returnable"`
		Snippet    bool   `json:"snippet_allowed"`
	}
	type scopeView struct {
		Scope           string      `json:"scope"`
		ContractVersion int         `json:"contract_version"`
		RetrievalClass  string      `json:"retrieval_class"`
		FreshnessClass  string      `json:"freshness_class"`
		Active          bool        `json:"active"`
		Fields          []fieldView `json:"fields"`
	}

	out := []scopeView{}
	for _, c := range contracts {
		if c.State != domain.ContractPublished {
			continue
		}
		view := scopeView{
			Scope:           c.ScopeName,
			ContractVersion: c.Version,
			RetrievalClass:  string(c.RetrievalClass),
			FreshnessClass:  c.FreshnessClass,
			Fields:          []fieldView{},
		}
		if _, err := h.store.GetActiveGeneration(r.Context(), c.ScopeName); err == nil {
			view.Active = true
		}
		for _, f := range c.Fields {
			if f.SensitivityClass == domain.SensitivitySecretProhibited {
				// A prohibited field is not merely unusable — it is not
				// disclosed to exist. NP-53's "reject without leaking field
				// existence" applies to the catalogue as much as to a sort
				// refusal.
				continue
			}
			view.Fields = append(view.Fields, fieldView{
				Name: f.FieldID, Type: f.Type, Searchable: f.Searchable,
				Filterable: f.Filterable, Facetable: f.Facetable, Sortable: f.Sortable,
				Returnable: f.Returnable, Snippet: f.SnippetAllowed,
			})
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scopes": out})
}

// ── shared helpers ───────────────────────────────────────────────────────────

var errNoActiveGeneration = errors.New("scope has no active index generation")

func (h *Handler) resolveScope(ctx context.Context, scope string) (*domain.IndexContract, *domain.IndexGeneration, error) {
	if strings.TrimSpace(scope) == "" {
		return nil, nil, errors.New("scope is required")
	}
	contract, err := h.store.GetPublishedContract(ctx, scope)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, errors.New("scope " + scope + " has no published index contract")
		}
		return nil, nil, err
	}
	generation, err := h.store.GetActiveGeneration(ctx, scope)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, errNoActiveGeneration
		}
		return nil, nil, err
	}
	return contract, generation, nil
}

// trustedContext builds the query context from the verified envelope ONLY.
//
// Nothing here reads the request body. NP-01 in one function: the body has no
// tenant to take, so a client that supplies one is not overridden — it is
// never consulted.
func (h *Handler) trustedContext(env envelope.Envelope) query.Context {
	return query.Context{
		TenantID:      env.TenantID,
		ActorID:       env.ActorSubjectID,
		WorkloadID:    env.WorkloadID,
		OnBehalfOf:    env.ActorSubjectID,
		LegalEntityID: env.LegalEntityID,
		Purpose:       env.PurposeContext,
		// Residency is resolved from tenant context upstream and arrives as
		// a gateway-set header, the same way tenant does. Absent means
		// unpartitioned, which is correct for a single-region deployment; it
		// is never defaulted to a region name, because a wrong region is a
		// residency violation and an absent one is only a missing narrowing.
		ResidencyRegion: env.JurisdictionContext,
	}
}

// refuse writes a reason-coded refusal AND records the evidence for it.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, status int, code domain.ReasonCode, detail, scope string, tc query.Context) {
	env := envelope.MustFromContext(r.Context())

	if tc.TenantID != "" {
		h.recordEvidence(r.Context(), domain.SearchEvidence{
			EvidenceID:    uuid.NewString(),
			RequestID:     env.RequestID,
			CorrelationID: env.CorrelationID,
			TenantID:      tc.TenantID,
			ActorID:       tc.ActorID,
			WorkloadID:    tc.WorkloadID,
			Purpose:       tc.Purpose,
			ScopeName:     scope,
			QueryDigest:   "refused",
			FiltersDigest: "refused",
			PlanDigest:    "refused",
			Completeness:  domain.CompletenessUnknown,
			ReasonCodes:   []string{string(code)},
		})
	}

	if h.events != nil {
		_ = h.events.SecurityFilterDenied(r.Context(), tc.TenantID, scope,
			string(code), h.actorHash(tc), env.CorrelationID)
	}

	writeErrorCode(w, status, "search_refused", detail, code)
}

// actorHash is a keyed, stable hash of the acting identity.
//
// Keyed rather than a plain SHA-256, because the input space is UUIDs a
// determined reader could enumerate offline — an unkeyed hash of a principal
// id is a principal id with extra steps. §11.2 asks for "actor/workload hash"
// on this event precisely so the abuse stream can correlate without naming.
func (h *Handler) actorHash(tc query.Context) string {
	actor := tc.ActorID
	if actor == "" {
		actor = tc.WorkloadID
	}
	if actor == "" {
		return ""
	}
	mac := hmac.New(sha256.New, h.evidenceKey)
	mac.Write([]byte(tc.TenantID + "|" + actor))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// recordEvidence writes an evidence row, never failing the request.
//
// A search that succeeded must not be turned into a 500 because its audit row
// could not be written — but the failure is logged at Error, because evidence
// that stopped being recorded is a compliance gap and silence about it is the
// failure mode policy-svc's governance-log write already demonstrated.
func (h *Handler) recordEvidence(ctx context.Context, e domain.SearchEvidence) {
	if e.TenantID == "" {
		return
	}
	if err := h.store.RecordEvidence(ctx, e); err != nil {
		h.log.Error("SEARCH EVIDENCE NOT RECORDED — the search proceeded but is not auditable",
			zap.String("scope", e.ScopeName),
			zap.String("request_id", e.RequestID),
			zap.Error(err))
	}
}

func totalSuppressed(resp *retrieval.Response) int {
	total := 0
	for _, s := range resp.Suppressions {
		total += s.Count
	}
	return total
}

// statusFor maps a reason code to an HTTP status.
func statusFor(code domain.ReasonCode) int {
	switch code {
	case domain.ReasonTenantContextMissing:
		return http.StatusUnauthorized
	case domain.ReasonPartitionNotAuthorized, domain.ReasonPrivacyPurposeBlocked,
		domain.ReasonExportAuthzRequired:
		return http.StatusForbidden
	case domain.ReasonScopeNotRegistered, domain.ReasonGenerationNotActive:
		return http.StatusNotFound
	case domain.ReasonAuthorizationIndet:
		return http.StatusServiceUnavailable
	case domain.ReasonSearchDegradedPartial:
		return http.StatusPartialContent
	default:
		// ESR-003/004/005/006/015 are all "your request was not acceptable",
		// which is a 400. Not a 422: the request is syntactically fine and
		// semantically refused, and 400 is what every other service here uses
		// for that.
		return http.StatusBadRequest
	}
}

func outcomeOf(err error) string {
	switch {
	case err == nil:
		return "allowed"
	case errors.Is(err, authz.ErrDenied):
		return "denied"
	default:
		return "unavailable"
	}
}

func (h *Handler) writeAuthzError(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrDenied) {
		writeError(w, http.StatusForbidden, "authorization_denied",
			"this principal is not authorized for that action")
		return
	}
	writeErrorCode(w, http.StatusServiceUnavailable, "authorization_unavailable",
		"no authorization decision could be obtained; failing closed",
		domain.ReasonAuthorizationIndet)
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.log.Error("request failed", zap.String("operation", what),
		zap.String("path", r.URL.Path), zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	// Unknown fields are an ERROR, not ignored. A caller that sent
	// {"tenant_id": "..."} in a search body is asserting something this
	// service will never honour, and silently dropping it would let them
	// believe a cross-tenant search had been performed and returned nothing —
	// rather than that their request was refused (NP-01).
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"error": code, "detail": detail})
}

// writeErrorCode writes a refusal carrying its §11.3 reason code.
//
// The code and its canonical meaning are BOTH sent. A caller branching on
// "ESR-007" does not need the meaning; a human reading a response body in a
// terminal does, and shipping only the code turns every refusal into a lookup
// in a document they may not have.
func writeErrorCode(w http.ResponseWriter, status int, code, detail string, reason domain.ReasonCode) {
	writeJSON(w, status, map[string]any{
		"error":       code,
		"detail":      detail,
		"reason_code": string(reason),
		"reason":      domain.ReasonMeaning(reason),
	})
}

// chiParam is a thin wrapper so admin.go reads the same way as this file.
func chiParam(r *http.Request, name string) string { return chi.URLParam(r, name) }

// uuidParam reads a UUID path parameter, or reports that it is not one.
//
// Every id in this service's paths is a uuid column in Postgres, and an
// unparseable value reaches the driver as `invalid input syntax for type uuid`
// — a 500. That is the wrong answer twice over: the request failed because the
// CALLER sent a bad id, and a server fault is indistinguishable in monitoring
// from a real outage. tenant-entity-registry-svc's store carries the same fix
// for the same reason.
//
// 404 rather than 400, deliberately: a malformed id and an id that does not
// exist are the same fact from a caller's side — there is no such resource —
// and answering them differently would let a caller tell a well-formed
// stranger's id apart from noise.
func uuidParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	raw := chi.URLParam(r, name)
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such resource")
		return "", false
	}
	return raw, true
}
