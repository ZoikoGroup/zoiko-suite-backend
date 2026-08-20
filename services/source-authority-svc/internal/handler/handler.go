package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/source-authority-svc/internal/authz"
	"zoiko.io/source-authority-svc/internal/domain"
	"zoiko.io/source-authority-svc/internal/events"
	"zoiko.io/source-authority-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	SourceAuthorityMapCreate = "SOURCE_AUTHORITY_MAP_CREATE"
	NormalizedFactRecord     = "NORMALIZED_FACT_RECORD"
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

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
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

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/source-authority-maps", func(r chi.Router) {
		r.Post("/", h.CreateSourceAuthorityMap)
		r.Get("/", h.ListSourceAuthorityMaps)
	})
	r.Post("/v1/normalized-facts", h.RecordFact)
	r.Get("/v1/source-authority/resolve", h.Resolve)
}

// CreateSourceAuthorityMap handles POST /v1/source-authority-maps.
func (h *Handler) CreateSourceAuthorityMap(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateSourceAuthorityMapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FieldFamily == "" || req.SourceSystem == "" || req.ConflictRoute == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "field_family, source_system, conflict_route, and effective_from are required")
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

	m := &domain.SourceAuthorityMap{
		SourceAuthorityMapID: uuid.NewString(),
		FieldFamily:          req.FieldFamily,
		SourceSystem:         req.SourceSystem,
		PrecedenceRank:       req.PrecedenceRank,
		ConflictRoute:        req.ConflictRoute,
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.AllowedCorrectionPath != "" {
		m.AllowedCorrectionPath = &req.AllowedCorrectionPath
	}
	if err := h.store.CreateSourceAuthorityMap(r.Context(), m); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		h.logger.Error("create source authority map failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create source authority map")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "source_authority_map.created", EntityID: m.SourceAuthorityMapID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: m,
	})
	writeJSON(w, http.StatusCreated, m)
}

// ListSourceAuthorityMaps handles GET /v1/source-authority-maps?field_family=.
func (h *Handler) ListSourceAuthorityMaps(w http.ResponseWriter, r *http.Request) {
	fieldFamily := r.URL.Query().Get("field_family")
	if fieldFamily == "" {
		writeError(w, http.StatusBadRequest, "field_family query param is required")
		return
	}
	maps, err := h.store.ListSourceAuthorityMaps(r.Context(), fieldFamily)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list source authority maps")
		return
	}
	if maps == nil {
		maps = []domain.SourceAuthorityMap{}
	}
	writeJSON(w, http.StatusOK, maps)
}

// RecordFact handles POST /v1/normalized-facts. Append-only — a
// correction is a new fact with a later observed_at, never an update to
// an existing row (doc7 §D1).
func (h *Handler) RecordFact(w http.ResponseWriter, r *http.Request) {
	var req domain.RecordFactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FieldFamily == "" || req.EntityRef == "" || req.SourceSystem == "" || req.SourceRecord == "" || req.ObservedAt == "" || len(req.FactValue) == 0 {
		writeError(w, http.StatusBadRequest, "field_family, entity_ref, source_system, source_record, observed_at, and fact_value are required")
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
		authorityClass = "AUTHORITATIVE"
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, NormalizedFactRecord) {
		return
	}

	f := &domain.NormalizedFact{
		NormalizedFactID:     uuid.NewString(),
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
	}
	if req.SourceVersion != "" {
		f.SourceVersion = &req.SourceVersion
	}
	if req.TransformationVersion != "" {
		f.TransformationVersion = &req.TransformationVersion
	}
	if err := h.store.RecordFact(r.Context(), f); err != nil {
		h.logger.Error("record normalized fact failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to record normalized fact")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "normalized_fact.recorded", EntityID: f.NormalizedFactID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: f,
	})
	writeJSON(w, http.StatusCreated, f)
}

// Resolve handles GET /v1/source-authority/resolve?field_family=&entity_ref=
// — "which value should I trust for this field, right now." No authz
// gate: a read-only check every service needs cheaply and often, same
// posture as kill-switch-registry-svc's resolve endpoint.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	fieldFamily := r.URL.Query().Get("field_family")
	entityRef := r.URL.Query().Get("entity_ref")
	if fieldFamily == "" || entityRef == "" {
		writeError(w, http.StatusBadRequest, "field_family and entity_ref query params are required")
		return
	}

	result, err := h.store.ResolveAuthoritativeFact(r.Context(), fieldFamily, entityRef)
	if err != nil {
		h.logger.Error("resolve authoritative fact failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve authoritative fact")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
