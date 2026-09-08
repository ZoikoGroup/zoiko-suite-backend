package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/source-authority-svc/internal/authz"
	"zoiko.io/source-authority-svc/internal/domain"
	"zoiko.io/source-authority-svc/internal/events"
	svcmiddleware "zoiko.io/source-authority-svc/internal/middleware"
	"zoiko.io/source-authority-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	SourceAuthorityMapCreate    = "SOURCE_AUTHORITY_MAP_CREATE"
	SourceAuthorityMapView      = "SOURCE_AUTHORITY_MAP_VIEW"
	SourceAuthorityMapSupersede = "SOURCE_AUTHORITY_MAP_SUPERSEDE"
	NormalizedFactRecord        = "NORMALIZED_FACT_RECORD"
	NormalizedFactView          = "NORMALIZED_FACT_VIEW"
)

// maxBodyBytes caps a request body. Without it an unbounded body — and
// fact_value is free-form JSONB, so there is no natural ceiling — is read
// straight into memory before any validation runs.
const maxBodyBytes = 1 << 20

const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/source-authority-maps", func(r chi.Router) {
		r.Post("/", h.CreateSourceAuthorityMap)
		r.Get("/", h.ListSourceAuthorityMaps)
		r.Post("/{source_authority_map_id}/supersede", h.SupersedeSourceAuthorityMap)
	})
	r.Post("/v1/normalized-facts", h.RecordFact)
	r.Get("/v1/normalized-facts", h.ListNormalizedFacts)
	r.Get("/v1/source-authority/resolve", h.Resolve)
}

// ── POST /v1/source-authority-maps ───────────────────────────────────────────

// CreateSourceAuthorityMap records a precedence rule. Platform-wide reference
// data, so no tenant scope — but a real principal and grant are still required,
// because the rule decides which connected system's value the whole platform
// trusts.
//
// Idempotent on correlation_id.
func (h *Handler) CreateSourceAuthorityMap(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateSourceAuthorityMapRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FieldFamily == "" || req.SourceSystem == "" || req.ConflictRoute == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "field_family, source_system, conflict_route, and effective_from are required")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "correlation_id is required: it is the idempotency key for this rule")
		return
	}
	if req.PrecedenceRank <= 0 {
		writeError(w, http.StatusBadRequest, "precedence_rank must be positive (1 = highest precedence)")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, SourceAuthorityMapCreate) {
		return
	}

	correlationID := req.CorrelationID
	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(),
		FieldFamily:          req.FieldFamily,
		SourceSystem:         req.SourceSystem,
		PrecedenceRank:       req.PrecedenceRank,
		ConflictRoute:        req.ConflictRoute,
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
		CorrelationID:        &correlationID,
	}
	if req.AllowedCorrectionPath != "" {
		m.AllowedCorrectionPath = &req.AllowedCorrectionPath
	}

	created, err := h.store.CreateSourceAuthorityMap(r.Context(), m)
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		h.logger.Error("create source authority map failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create source authority map")
		return
	}

	// 200 on a replay, 201 on a real write. An operator told "created" twice
	// would reasonably believe two rules now exist for the same source.
	if !created {
		writeJSON(w, http.StatusOK, m)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "source_authority_map.created", EntityID: m.SourceAuthorityMapID,
		ActorID: principalID, CorrelationID: correlationID, Payload: m,
	})
	writeJSON(w, http.StatusCreated, m)
}

// ── GET /v1/source-authority-maps ────────────────────────────────────────────

// ListSourceAuthorityMaps reads the precedence register.
//
// This used to run no authorization at all. The rules are not tenant data, but
// they are the platform's trust topology — which connected systems are believed
// over which, and by what margin — and handing that to anything that can reach
// the port is a disclosure whether or not the rows belong to anyone.
//
// field_family is now optional. Requiring it meant a caller had to already know
// the exact family string to see anything, so the register could not be browsed
// and a typo answered with an empty list.
func (h *Handler) ListSourceAuthorityMaps(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, SourceAuthorityMapView) {
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	includeSuperseded, ok := parseBool(w, r, "include_superseded")
	if !ok {
		return
	}

	maps, err := h.store.ListSourceAuthorityMaps(r.Context(), domain.ListSourceAuthorityMapsFilter{
		FieldFamily:       r.URL.Query().Get("field_family"),
		SourceSystem:      r.URL.Query().Get("source_system"),
		IncludeSuperseded: includeSuperseded,
		Limit:             limit,
		Offset:            offset,
	})
	if err != nil {
		h.logger.Error("list source authority maps failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to list source authority maps")
		return
	}
	if maps == nil {
		maps = []domain.SourceAuthorityMap{}
	}
	writeJSON(w, http.StatusOK, maps)
}

// ── POST /v1/source-authority-maps/{id}/supersede ────────────────────────────

// SupersedeSourceAuthorityMap end-dates a precedence rule.
//
// The rule's own terms are never rewritten — only its window closes — so a
// resolution made while it was in force is still explained by the row that made
// it. This is the operation the service was missing: effective_to existed and
// the resolver honoured it, but nothing could set it, so "a changed precedence
// is a new row" left both rows live and the change had no effect.
func (h *Handler) SupersedeSourceAuthorityMap(w http.ResponseWriter, r *http.Request) {
	mapID := chi.URLParam(r, "source_authority_map_id")

	var req domain.SupersedeSourceAuthorityMapRequest
	if !decodeJSONAllowEmpty(w, r, &req) {
		return
	}

	effectiveTo := time.Now().UTC()
	if req.EffectiveTo != "" {
		parsed, err := time.Parse(time.RFC3339, req.EffectiveTo)
		if err != nil {
			writeError(w, http.StatusBadRequest, "effective_to must be RFC3339")
			return
		}
		// A past end date rewrites which rule was in force when an earlier
		// resolution was made. doc7 §D1's "never silently back-write", applied
		// to the precedence rules rather than to the facts.
		if parsed.Before(time.Now().UTC().Add(-time.Minute)) {
			writeError(w, http.StatusBadRequest, string(domain.ErrSupersedeInPast))
			return
		}
		effectiveTo = parsed
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, SourceAuthorityMapSupersede) {
		return
	}

	updated, err := h.store.SupersedeSourceAuthorityMap(r.Context(), mapID, effectiveTo, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrMapNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, domain.ErrAlreadySuperseded):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, domain.ErrSupersedeBeforeStart):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			h.logger.Error("supersede source authority map failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to supersede source authority map")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "source_authority_map.superseded", EntityID: updated.SourceAuthorityMapID,
		ActorID: principalID, CorrelationID: correlationIDOf(r, req.CorrelationID), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/normalized-facts ────────────────────────────────────────────────

// RecordFact appends one observation. Append-only — a correction is a new fact
// with a later observed_at, never an update to an existing row (doc7 §D1).
//
// Tenant-scoped: the fact is one tenant's business data, and the row now
// carries the tenant it was reported under. Idempotent on
// (tenant_id, correlation_id), which correlation_id was always meant to
// provide and never did — the field was in the request type and read by
// nothing, so a retry appended a second observation no source had made.
func (h *Handler) RecordFact(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req domain.RecordFactRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.FieldFamily == "" || req.EntityRef == "" || req.SourceSystem == "" || req.SourceRecord == "" || req.ObservedAt == "" || len(req.FactValue) == 0 {
		writeError(w, http.StatusBadRequest, "field_family, entity_ref, source_system, source_record, observed_at, and fact_value are required")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "correlation_id is required: it is the idempotency key for this observation")
		return
	}
	observedAt, err := time.Parse(time.RFC3339, req.ObservedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "observed_at must be RFC3339")
		return
	}
	effectiveAt := observedAt
	if req.EffectiveAt != "" {
		effectiveAt, err = time.Parse(time.RFC3339, req.EffectiveAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "effective_at must be RFC3339")
			return
		}
	}
	authorityClass := req.AuthorityClass
	if authorityClass == "" {
		authorityClass = domain.AuthorityClassAuthoritative
	}
	// Previously unchecked in Go and in the schema alike, so a misspelling was
	// stored as a class no consumer knows how to weigh.
	if !domain.ValidAuthorityClass(authorityClass) {
		writeError(w, http.StatusBadRequest, string(domain.ErrUnknownAuthorityClass))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, NormalizedFactRecord) {
		return
	}

	correlationID := req.CorrelationID
	f := &domain.NormalizedFact{
		NormalizedFactID:     uuid.NewString(),
		TenantID:             tenantID,
		FieldFamily:          req.FieldFamily,
		EntityRef:            req.EntityRef,
		SourceSystem:         req.SourceSystem,
		SourceRecord:         req.SourceRecord,
		FactValue:            req.FactValue,
		ObservedAt:           observedAt,
		EffectiveAt:          effectiveAt,
		AuthorityClass:       authorityClass,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
		CorrelationID:        &correlationID,
	}
	if req.SourceVersion != "" {
		f.SourceVersion = &req.SourceVersion
	}
	if req.TransformationVersion != "" {
		f.TransformationVersion = &req.TransformationVersion
	}

	created, err := h.store.RecordFact(r.Context(), f)
	if err != nil {
		h.writeStoreErr(w, err, "record normalized fact")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, f)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "normalized_fact.recorded", EntityID: f.NormalizedFactID,
		TenantID: tenantID, ActorID: principalID, CorrelationID: correlationID, Payload: f,
	})
	writeJSON(w, http.StatusCreated, f)
}

// ── GET /v1/normalized-facts ─────────────────────────────────────────────────

// ListNormalizedFacts returns the observations behind a resolution.
//
// A resolution on its own asserts an answer; this is what makes it explainable
// — every source that reported, what each said, and when. Gated and
// tenant-scoped, because it returns raw fact_value.
func (h *Handler) ListNormalizedFacts(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, NormalizedFactView) {
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	facts, err := h.store.ListNormalizedFacts(r.Context(), domain.ListNormalizedFactsFilter{
		TenantID:     tenantID,
		FieldFamily:  r.URL.Query().Get("field_family"),
		EntityRef:    r.URL.Query().Get("entity_ref"),
		SourceSystem: r.URL.Query().Get("source_system"),
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		h.writeStoreErr(w, err, "list normalized facts")
		return
	}
	if facts == nil {
		facts = []domain.NormalizedFact{}
	}
	writeJSON(w, http.StatusOK, facts)
}

// ── GET /v1/source-authority/resolve ─────────────────────────────────────────

// Resolve answers "which value should I trust for this field, right now".
//
// AUTHORIZATION is still not gated here, and that stays a deliberate choice:
// this is the cheap, hot read every service needs before acting on a fact, the
// same posture as kill-switch-registry-svc's resolve. TENANT SCOPE is a
// different question and was never a choice — it was missing.
//
// The envelope middleware defaults to write-strict, which admits reads with no
// envelope at all, so before this pass the request needed no tenant, no
// principal and no grant, and returned raw fact_value for any entity_ref in the
// platform. entity_ref is free text that no registry constrains, so guessing one
// was the entire access control. The tenant is now required and every row read
// is filtered by it.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	fieldFamily := r.URL.Query().Get("field_family")
	entityRef := r.URL.Query().Get("entity_ref")
	if fieldFamily == "" || entityRef == "" {
		writeError(w, http.StatusBadRequest, "field_family and entity_ref query params are required")
		return
	}

	result, err := h.store.ResolveAuthoritativeFact(r.Context(), tenantID, fieldFamily, entityRef)
	if err != nil {
		h.writeStoreErr(w, err, "resolve authoritative fact")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

// requireTenant refuses a request that carries no X-Tenant-Id.
//
// Not redundant with the envelope middleware: that defaults to write-strict and
// admits reads with no envelope, so GET /resolve and GET /normalized-facts have
// nothing upstream guaranteeing a tenant. Without this the store would be asked
// for facts scoped to "" and answer with the rows no tenant owns.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, string(domain.ErrTenantMissing))
		return "", false
	}
	return tenantID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrMapNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		h.logger.Error(what+" failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to "+what)
	}
}

// decodeJSON caps the body and refuses unknown fields.
//
// A misspelled key used to be discarded in silence. On this service that is
// worse than the usual case: authority_class, effective_at and
// allowed_correction_path all have defaults or are optional, so a typo in any
// of them produced a stored row that differed from what the caller believed
// they had sent, with no error anywhere.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// decodeJSONAllowEmpty is decodeJSON for a route whose body is optional —
// supersede takes its defaults from the server when sent nothing.
func decodeJSONAllowEmpty(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			writeError(w, http.StatusBadRequest, string(domain.ErrInvalidPaging))
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, string(domain.ErrInvalidPaging))
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// parseBool refuses a value it does not understand rather than reading it as
// false. `include_superseded=yes` silently meaning "no" would answer a request
// for the full history with the narrow list, which reads as an answer.
func parseBool(w http.ResponseWriter, r *http.Request, key string) (bool, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return false, true
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		writeError(w, http.StatusBadRequest, key+" must be true or false")
		return false, false
	}
	return parsed, true
}

// correlationIDOf prefers the body's correlation id and falls back to the
// header the gateway sets, so a superseded event is traceable either way.
func correlationIDOf(r *http.Request, fromBody string) string {
	if fromBody != "" {
		return fromBody
	}
	return r.Header.Get("X-Correlation-ID")
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
