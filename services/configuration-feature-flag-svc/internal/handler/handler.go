package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ConfigStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type ConfigStore interface {
	UpsertConfigEntry(ctx context.Context, params domain.UpsertConfigEntryParams) (*domain.ConfigEntry, bool, error)
	FindCurrentConfigEntry(ctx context.Context, key, environment string, tenantID *string) (*domain.ConfigEntry, error)
	ListCurrentConfigEntries(ctx context.Context, filter store.ListFilter) ([]*domain.ConfigEntry, error)

	UpsertFeatureFlag(ctx context.Context, params domain.UpsertFeatureFlagParams) (*domain.FeatureFlag, bool, error)
	FindCurrentFeatureFlag(ctx context.Context, key, environment string, tenantID *string) (*domain.FeatureFlag, error)
	ListCurrentFeatureFlags(ctx context.Context, filter store.ListFilter) ([]*domain.FeatureFlag, error)
}

// EventPublisher is the narrow interface the handler depends on for
// publishing domain events. Allows the handler to be tested without a
// real event backbone. Mirrors policy-svc's/governance-decision-log-svc's
// pattern.
type EventPublisher interface {
	PublishConfigUpdated(ctx context.Context, entry domain.ConfigEntry, correlationID string) error
	PublishFeatureFlagUpdated(ctx context.Context, flag domain.FeatureFlag, correlationID string) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store     ConfigStore
	publisher EventPublisher
	log       *zap.Logger
}

// New constructs a Handler.
func New(store ConfigStore, publisher EventPublisher, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, log: log}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path — this makes the
// behaviour testable in unit tests that build their own router via this
// function (same convention as every other service in this repo).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/config", h.UpsertConfigEntry)
	r.Get("/v1/config", h.ListConfigEntries)
	r.Get("/v1/config/{key}", h.GetConfigEntry)

	r.Post("/v1/flags", h.UpsertFeatureFlag)
	r.Get("/v1/flags", h.ListFeatureFlags)
	r.Get("/v1/flags/{key}", h.GetFeatureFlag)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/config ─────────────────────────────────────────────────────────

// upsertConfigEntryRequest is the wire shape for POST /v1/config.
type upsertConfigEntryRequest struct {
	Key                  string          `json:"key"`
	Value                json.RawMessage `json:"value"`
	Environment          string          `json:"environment"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
}

func (req upsertConfigEntryRequest) missingField() string {
	switch {
	case req.Key == "":
		return "key"
	case len(req.Value) == 0:
		return "value"
	case req.Environment == "":
		return "environment"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// UpsertConfigEntry handles POST /v1/config.
//
// Upsert semantics (context.md §7.3): setting a (key, environment,
// tenant_id) scope to the value it's already at is a safe, idempotent
// no-op. Setting it to a genuinely new value ends the current row and
// inserts a new one.
//
// Response:
//
//	201 → a real transition happened (first write for this scope, or a new value)
//	200 → value unchanged from what's currently effective; no-op
//	400 → missing required field / invalid JSON
//	503 → store unavailable
func (h *Handler) UpsertConfigEntry(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req upsertConfigEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	params := domain.UpsertConfigEntryParams{
		Key:                  req.Key,
		Value:                req.Value,
		Environment:          req.Environment,
		TenantID:             req.TenantID,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
	}

	entry, created, err := h.store.UpsertConfigEntry(r.Context(), params)
	if err != nil {
		h.log.Error("UpsertConfigEntry: store unavailable",
			zap.String("key", req.Key),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		// Only a real transition is a new fact — an idempotent retry
		// (same value already effective) must not re-emit config.updated.
		if pubErr := h.publisher.PublishConfigUpdated(r.Context(), *entry, correlationID); pubErr != nil {
			h.log.Error("UpsertConfigEntry: failed to publish config.updated",
				zap.String("config_id", entry.ConfigID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("config entry upserted",
		zap.String("config_id", entry.ConfigID),
		zap.String("key", entry.Key),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, entry)
}

// ── GET /v1/config/{key} ─────────────────────────────────────────────────────

// GetConfigEntry handles GET /v1/config/{key}?environment=X&tenant_id=Y.
// Returns the row currently effective for that exact tuple — no fallback
// from a tenant-specific miss to a global default (context.md §7.2).
//
// Response:
//
//	200 → found
//	400 → missing environment
//	404 → nothing currently effective for this exact tuple
//	503 → store unavailable
func (h *Handler) GetConfigEntry(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	environment := q.Get("environment")
	if environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "environment",
		})
		return
	}
	var tenantID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}

	entry, err := h.store.FindCurrentConfigEntry(r.Context(), key, environment, tenantID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConfigEntryNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "config_entry_not_found",
				"key":   key,
			})
		default:
			h.log.Error("GetConfigEntry: store unavailable",
				zap.String("key", key),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// ── GET /v1/config ───────────────────────────────────────────────────────────

// ListConfigEntries handles GET /v1/config?environment=X&tenant_id=Y.
// Both filters are optional; omitting one means "no filter on that
// dimension" (e.g. omitting tenant_id returns entries across all
// tenants, not just global ones).
//
// Response:
//
//	200 → JSON array (may be empty)
//	503 → store unavailable
func (h *Handler) ListConfigEntries(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	filter := store.ListFilter{Environment: q.Get("environment")}
	if v := q.Get("tenant_id"); v != "" {
		filter.TenantID = &v
	}

	results, err := h.store.ListCurrentConfigEntries(r.Context(), filter)
	if err != nil {
		h.log.Error("ListConfigEntries: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.ConfigEntry{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── POST /v1/flags ───────────────────────────────────────────────────────────

// upsertFeatureFlagRequest is the wire shape for POST /v1/flags.
// Enabled is *bool so an explicit `false` is distinguishable from an
// omitted field.
type upsertFeatureFlagRequest struct {
	Key                  string  `json:"key"`
	Enabled              *bool   `json:"enabled"`
	Environment          string  `json:"environment"`
	TenantID             *string `json:"tenant_id,omitempty"`
	RolloutPercentage    *int    `json:"rollout_percentage,omitempty"`
	CreatedByPrincipalID string  `json:"created_by_principal_id"`
}

func (req upsertFeatureFlagRequest) missingField() string {
	switch {
	case req.Key == "":
		return "key"
	case req.Enabled == nil:
		return "enabled"
	case req.Environment == "":
		return "environment"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// UpsertFeatureFlag handles POST /v1/flags. Same upsert semantics as
// UpsertConfigEntry, comparing (enabled, rollout_percentage) for equality
// instead of a JSON value.
//
// Response:
//
//	201 → a real transition happened
//	200 → (enabled, rollout_percentage) unchanged; no-op
//	400 → missing required field / invalid JSON / rollout_percentage out of [0,100]
//	503 → store unavailable
func (h *Handler) UpsertFeatureFlag(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req upsertFeatureFlagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	rollout := 100
	if req.RolloutPercentage != nil {
		rollout = *req.RolloutPercentage
	}
	if rollout < 0 || rollout > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_field",
			"field":   "rollout_percentage",
			"message": "must be between 0 and 100",
		})
		return
	}

	params := domain.UpsertFeatureFlagParams{
		Key:                  req.Key,
		Enabled:              *req.Enabled,
		Environment:          req.Environment,
		TenantID:             req.TenantID,
		RolloutPercentage:    rollout,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
	}

	flag, created, err := h.store.UpsertFeatureFlag(r.Context(), params)
	if err != nil {
		h.log.Error("UpsertFeatureFlag: store unavailable",
			zap.String("key", req.Key),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if pubErr := h.publisher.PublishFeatureFlagUpdated(r.Context(), *flag, correlationID); pubErr != nil {
			h.log.Error("UpsertFeatureFlag: failed to publish feature_flag.updated",
				zap.String("flag_id", flag.FlagID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("feature flag upserted",
		zap.String("flag_id", flag.FlagID),
		zap.String("key", flag.Key),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, flag)
}

// ── GET /v1/flags/{key} ──────────────────────────────────────────────────────

// GetFeatureFlag handles GET /v1/flags/{key}?environment=X&tenant_id=Y.
//
// Response:
//
//	200 → found
//	400 → missing environment
//	404 → nothing currently effective for this exact tuple
//	503 → store unavailable
func (h *Handler) GetFeatureFlag(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	environment := q.Get("environment")
	if environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "environment",
		})
		return
	}
	var tenantID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}

	flag, err := h.store.FindCurrentFeatureFlag(r.Context(), key, environment, tenantID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrFeatureFlagNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "feature_flag_not_found",
				"key":   key,
			})
		default:
			h.log.Error("GetFeatureFlag: store unavailable",
				zap.String("key", key),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, flag)
}

// ── GET /v1/flags ────────────────────────────────────────────────────────────

// ListFeatureFlags handles GET /v1/flags?environment=X&tenant_id=Y.
//
// Response:
//
//	200 → JSON array (may be empty)
//	503 → store unavailable
func (h *Handler) ListFeatureFlags(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	filter := store.ListFilter{Environment: q.Get("environment")}
	if v := q.Get("tenant_id"); v != "" {
		filter.TenantID = &v
	}

	results, err := h.store.ListCurrentFeatureFlags(r.Context(), filter)
	if err != nil {
		h.log.Error("ListFeatureFlags: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.FeatureFlag{}
	}
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
