// Package handler exposes the REST API for jurisdiction-rules-svc via chi.
//
// Validation endpoint (GET /v1/jurisdictions/{id}) is the priority —
// it unblocks tenant-entity-registry-svc immediately. The contract matches
// exactly what HTTPJurisdictionValidator in tenant-entity-registry-svc expects:
//
// correlationIDMiddleware is applied inside RegisterRoutes so that the echo
// behaviour is exercised by handler-level tests without requiring the full
// main.go server stack.
//
//	200 OK        → jurisdiction exists, active_flag=true, not expired
//	404 Not Found → jurisdiction_id unknown, inactive, or expired
//	503           → database unavailable (callers must fail-closed)
//
// This endpoint is read-only. No Authorization header required.
// Admin mutating endpoints (POST /v1/admin/...) will require AuthZ — they are
// added in subsequent steps after this endpoint is verified in CI.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// JurisdictionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type JurisdictionStore interface {
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)
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
	r.Get("/v1/jurisdictions/{jurisdiction_id}", h.GetJurisdiction)

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

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// At this point headers are already sent — log only.
		_ = err
	}
}
