package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// JurisdictionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type JurisdictionStore interface {
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)
	List(ctx context.Context, params store.ListParams) ([]*domain.Jurisdiction, error)
	FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error)
	FindRules(ctx context.Context, params store.FindRulesParams) ([]*domain.JurisdictionRule, error)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store JurisdictionStore
	log   *zap.Logger
}

// New constructs a Handler.
func New(store JurisdictionStore, log *zap.Logger) *Handler {
	return &Handler{store: store, log: log}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path — this makes the behaviour
// testable in unit tests that build their own router via this function.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	// ── Public read (no AuthZ required) ──────────────────────────────────────
	r.Get("/v1/jurisdictions", h.ListJurisdictions)
	r.Get("/v1/jurisdictions/{jurisdiction_id}", h.GetJurisdiction)
	r.Get("/v1/jurisdictions/{jurisdiction_id}/ancestors", h.GetAncestors)
	r.Get("/v1/jurisdictions/{jurisdiction_id}/rules", h.GetRules)

	// ── Admin mutations (AuthZ required — wired in next scaffold step) ────────
	// r.Post("/v1/admin/jurisdictions", h.CreateJurisdiction)
	// r.Post("/v1/admin/jurisdictions/{jurisdiction_id}/deactivate", h.DeactivateJurisdiction)
}

// correlationIDMiddleware echoes X-Correlation-ID from the request into the
// response on every route registered via RegisterRoutes. If the header is
// absent the response will carry an empty string — injection of a fresh ID
// when absent is handled by the server-level middleware in main.go.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// GetJurisdiction handles GET /v1/jurisdictions/{jurisdiction_id}.
//
// This is the validation endpoint called synchronously (fail-closed) by
// tenant-entity-registry-svc before persisting any EntityJurisdictionAssignment
// or TaxIdentityBundle that references a jurisdiction_id.
//
// Response contract (must match HTTPJurisdictionValidator exactly):
//
//	200 → jurisdiction known and active
//	404 → jurisdiction_id unknown, inactive, or expired
//	503 → store unavailable — callers MUST reject the assignment fail-closed
func (h *Handler) GetJurisdiction(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	j, err := h.store.FindByID(r.Context(), jurisdictionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrJurisdictionNotFound):
			h.log.Debug("jurisdiction not found",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
			)
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":           "jurisdiction_not_found",
				"jurisdiction_id": jurisdictionID,
			})
		default:
			// Store unavailable — log ERROR, return 503.
			// Callers (tenant-entity-registry-svc) must fail-closed on 503.
			h.log.Error("jurisdiction store unavailable",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "store_unavailable",
			})
		}
		return
	}

	h.log.Debug("jurisdiction validated",
		zap.String("jurisdiction_id", jurisdictionID),
		zap.String("jurisdiction_code", j.JurisdictionCode),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, j)
}

// ListJurisdictions handles GET /v1/jurisdictions.
//
// Query parameters (all optional):
//
//	type=COUNTRY          filter by jurisdiction_type (VARCHAR, data driven)
//	active=true           limit to active_flag=true and non-expired rows
//	limit=50              page size (max 200, default 50)
//	offset=0              zero-based page offset
//
// Response:
//
//	200 → JSON array of Jurisdiction objects (may be empty)
//	503 → store unavailable
func (h *Handler) ListJurisdictions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	params := store.ListParams{
		JurisdictionType: q.Get("type"),
		ActiveOnly:       q.Get("active") == "true",
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	results, err := h.store.List(r.Context(), params)
	if err != nil {
		h.log.Error("ListJurisdictions: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.Jurisdiction{}
	}
	h.log.Debug("ListJurisdictions",
		zap.Int("count", len(results)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, results)
}

// GetAncestors handles GET /v1/jurisdictions/{jurisdiction_id}/ancestors.
//
// Returns the ancestor chain from immediate parent to root, ordered nearest
// first. The jurisdiction itself is NOT included in the response.
//
// Response:
//
//	200 → JSON array of Jurisdiction objects (empty if root jurisdiction)
//	404 → jurisdiction_id not found
//	503 → store unavailable
func (h *Handler) GetAncestors(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	ancestors, err := h.store.FindAncestors(r.Context(), jurisdictionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrJurisdictionNotFound):
			h.log.Debug("GetAncestors: jurisdiction not found",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
			)
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":           "jurisdiction_not_found",
				"jurisdiction_id": jurisdictionID,
			})
		default:
			h.log.Error("GetAncestors: store unavailable",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Always return an array — never null.
	if ancestors == nil {
		ancestors = []*domain.Jurisdiction{}
	}
	h.log.Debug("GetAncestors",
		zap.String("jurisdiction_id", jurisdictionID),
		zap.Int("ancestor_count", len(ancestors)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, ancestors)
}

// GetRules handles GET /v1/jurisdictions/{jurisdiction_id}/rules.
//
// Query parameters (all optional):
//
//	domain=PAYROLL        filter by rule_domain (VARCHAR, data driven)
//	effective_at=2024-01-01T00:00:00Z  point-in-time (ISO 8601). If omitted, now is used.
//	limit=50              page size (max 100, default 50)
//	offset=0              zero-based page offset
//
// Response:
//
//	200 → JSON array of JurisdictionRule objects (may be empty)
//	404 → jurisdiction_id not found
//	503 → store unavailable
func (h *Handler) GetRules(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	params := store.FindRulesParams{
		JurisdictionID: jurisdictionID,
		Domain:         q.Get("domain"), // empty string means all domains
	}
	if v := q.Get("effective_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			// Return 400 Bad Request for invalid effective_at format
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_effective_at",
				"message": "effective_at must be a valid RFC3339 timestamp",
			})
			return
		}
		params.EffectiveAt = t
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Offset = n
		}
	}

	results, err := h.store.FindRules(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrJurisdictionNotFound):
			h.log.Debug("GetRules: jurisdiction not found",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
			)
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":           "jurisdiction_not_found",
				"jurisdiction_id": jurisdictionID,
			})
		default:
			h.log.Error("GetRules: store unavailable",
				zap.String("jurisdiction_id", jurisdictionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.JurisdictionRule{}
	}
	h.log.Debug("GetRules",
		zap.String("jurisdiction_id", jurisdictionID),
		zap.Int("count", len(results)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, results)
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// At this point headers are already sent — log only.
		_ = err
	}
}