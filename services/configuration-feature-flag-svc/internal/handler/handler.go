package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/authz"
	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	svcmiddleware "zoiko.io/configuration-feature-flag-svc/internal/middleware"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
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

// Handler holds all HTTP handler methods.
//
// There is deliberately no event publisher here any more. Events are enqueued
// by the store, in the transaction that records the change, and drained to
// Kafka by internal/outbox. The handler used to publish directly after the
// store returned — so a broker hiccup lost the event, logged it, and answered
// 201 as though nothing were wrong. See internal/outbox for the full account.
type Handler struct {
	store   ConfigStore
	authz   authz.Client
	metrics *telemetry.Domain
	log     *zap.Logger

	// authzPlatformScopeID is the legal_entity_id used for configuration and
	// flags, which are platform-scoped rather than entity-scoped.
	// authorization-svc rejects an empty legal_entity_id.
	authzPlatformScopeID string
}

// New constructs a Handler.
func New(store ConfigStore, authzClient authz.Client, authzPlatformScopeID string, metrics *telemetry.Domain, log *zap.Logger) *Handler {
	return &Handler{
		store:                store,
		authz:                authzClient,
		metrics:              metrics,
		authzPlatformScopeID: authzPlatformScopeID,
		log:                  log,
	}
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteIdentityMissing).Inc()
		return
	}
	// tenant_id in the body used to be written straight through, so a caller
	// could overwrite another tenant's configuration value — and configuration
	// is what other services read to decide how to behave.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteIdentityMissing).Inc()
		return
	}

	var req upsertConfigEntryRequest
	if !h.decodeJSON(w, r, &req, h.metrics.ConfigWrites) {
		return
	}
	if missing := req.missingField(); missing != "" {
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteInvalidRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteTenantMismatch).Inc()
		return
	}

	// The SCOPE decides the action, and it has to be resolved before the
	// authorization check rather than after it. A write with no tenant_id is
	// the environment-wide DEFAULT: it takes effect for every tenant that has
	// not set its own value. Authorizing it as an ordinary CONFIGURATION_WRITE
	// meant a principal provisioned to manage one organisation could change
	// what every other organisation reads, and no refusal ever ran — RLS cannot
	// catch it either, because migration 000002's WITH CHECK admits a NULL
	// tenant_id unconditionally, a global row genuinely belonging to no tenant.
	global := req.TenantID == nil || *req.TenantID == ""
	action := telemetry.ActionConfigWrite
	if global {
		action = telemetry.ActionConfigGlobalWrite
	}
	if !h.authorize(w, r, principalID, "", action, h.metrics.ConfigWrites) {
		if global {
			h.metrics.GlobalScopeWrites.WithLabelValues("config", telemetry.WriteForbidden).Inc()
		}
		return
	}

	params := domain.UpsertConfigEntryParams{
		Key:                  req.Key,
		Value:                req.Value,
		Environment:          req.Environment,
		TenantID:             req.TenantID,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
		CallerTenantID:       tenantScope,
		CorrelationID:        correlationID,
	}

	entry, created, err := h.store.UpsertConfigEntry(r.Context(), params)
	if errors.Is(err, domain.ErrScopeRaceConflict) {
		// A concurrent writer created this scope first. Nothing is wrong with
		// the request and nothing is wrong with the database — retrying now
		// takes the ordinary compare-and-update path. This used to answer 503,
		// which said the opposite.
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteConflict).Inc()
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "scope_race_conflict",
			"key":     req.Key,
			"message": domain.ErrScopeRaceConflict.Error(),
		})
		return
	}
	if err != nil {
		h.metrics.ConfigWrites.WithLabelValues(telemetry.WriteStoreUnavailable).Inc()
		h.log.Error("UpsertConfigEntry: store unavailable",
			zap.String("key", req.Key),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// config.updated was enqueued by the store inside the same transaction, and
	// only on a real transition — an idempotent retry writes no row and so
	// emits no event. Nothing is published from here.
	status := http.StatusOK
	outcome := telemetry.WriteNoChange
	if created {
		status = http.StatusCreated
		outcome = telemetry.WriteCreated
	}
	h.metrics.ConfigWrites.WithLabelValues(outcome).Inc()
	if global {
		h.metrics.GlobalScopeWrites.WithLabelValues("config", outcome).Inc()
	}

	h.log.Info("config entry upserted",
		zap.String("config_id", entry.ConfigID),
		zap.String("key", entry.Key),
		zap.Bool("created", created),
		zap.Bool("global_scope", global),
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
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var tenantID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	// Omitting ?tenant_id= still asks for the GLOBAL default for this
	// environment, which is a real scope. Naming another tenant is refused.
	if h.refuseForeignTenant(w, tenantID, tenantScope) {
		return
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

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	// An absent tenant filter used to mean "no filter", i.e. every tenant's
	// configuration. It now means the caller's own tenant plus the global
	// defaults that apply to it — which is what "what applies to me" asks for,
	// and never another tenant's rows.
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantScope {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return
	}
	filter := store.ListFilter{
		Environment:   q.Get("environment"),
		TenantID:      &tenantScope,
		IncludeGlobal: true,
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteIdentityMissing).Inc()
		return
	}
	// Same as the config path: tenant_id in the body used to decide whose flag
	// was flipped, so a caller could turn a feature on or off for another
	// tenant.
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteIdentityMissing).Inc()
		return
	}

	var req upsertFeatureFlagRequest
	if !h.decodeJSON(w, r, &req, h.metrics.FlagWrites) {
		return
	}
	if missing := req.missingField(); missing != "" {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteInvalidRequest).Inc()
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
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteInvalidRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_field",
			"field":   "rollout_percentage",
			"message": "must be between 0 and 100",
		})
		return
	}

	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteTenantMismatch).Inc()
		return
	}

	// See UpsertConfigEntry for why the scope decides the action. It matters
	// more here, if anything: a global flag write is how a feature is switched
	// on or off for every organisation in an environment at once.
	global := req.TenantID == nil || *req.TenantID == ""
	action := telemetry.ActionFlagWrite
	if global {
		action = telemetry.ActionFlagGlobalWrite
	}
	if !h.authorize(w, r, principalID, "", action, h.metrics.FlagWrites) {
		if global {
			h.metrics.GlobalScopeWrites.WithLabelValues("flag", telemetry.WriteForbidden).Inc()
		}
		return
	}

	params := domain.UpsertFeatureFlagParams{
		Key:                  req.Key,
		Enabled:              *req.Enabled,
		Environment:          req.Environment,
		TenantID:             req.TenantID,
		RolloutPercentage:    rollout,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
		CallerTenantID:       tenantScope,
		CorrelationID:        correlationID,
	}

	flag, created, err := h.store.UpsertFeatureFlag(r.Context(), params)
	if errors.Is(err, domain.ErrScopeRaceConflict) {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteConflict).Inc()
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "scope_race_conflict",
			"key":     req.Key,
			"message": domain.ErrScopeRaceConflict.Error(),
		})
		return
	}
	if err != nil {
		h.metrics.FlagWrites.WithLabelValues(telemetry.WriteStoreUnavailable).Inc()
		h.log.Error("UpsertFeatureFlag: store unavailable",
			zap.String("key", req.Key),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// feature_flag.updated was enqueued by the store in the same transaction,
	// and only on a real transition. Nothing is published from here.
	status := http.StatusOK
	outcome := telemetry.WriteNoChange
	if created {
		status = http.StatusCreated
		outcome = telemetry.WriteCreated
	}
	h.metrics.FlagWrites.WithLabelValues(outcome).Inc()
	if global {
		h.metrics.GlobalScopeWrites.WithLabelValues("flag", outcome).Inc()
	}

	h.log.Info("feature flag upserted",
		zap.String("flag_id", flag.FlagID),
		zap.String("key", flag.Key),
		zap.Bool("created", created),
		zap.Bool("global_scope", global),
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
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var tenantID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	if h.refuseForeignTenant(w, tenantID, tenantScope) {
		return
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

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantScope {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return
	}
	filter := store.ListFilter{
		Environment:   q.Get("environment"),
		TenantID:      &tenantScope,
		IncludeGlobal: true,
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

// ── authorization ────────────────────────────────────────────────────────────

// Action types this service asks authorization-svc about. Changing a config
// value or flipping a feature flag alters platform behaviour at runtime, so
// both are material actions under 03-microservices.md §17.1.
//
// Defined in internal/telemetry and re-exported here rather than declared
// twice. The metric series for each action are pre-created from the same
// constants, so an action the handler checks and the metrics do not know about
// cannot exist — which is how an action name nobody grants stays invisible.
//
// There are FOUR, not two: writing the environment-wide default is a different
// act from writing one organisation's value, and must be a different grant. See
// telemetry.ActionConfigGlobalWrite for the full reasoning.
const (
	ActionConfigWrite            = telemetry.ActionConfigWrite
	ActionConfigGlobalWrite      = telemetry.ActionConfigGlobalWrite
	ActionFeatureFlagWrite       = telemetry.ActionFlagWrite
	ActionFeatureFlagGlobalWrite = telemetry.ActionFlagGlobalWrite
)

// requirePrincipal resolves the acting principal from the gateway-verified
// X-Principal-Id header, writing 401 and returning false when absent.
// requireTenant reads the caller's verified tenant scope from context (set by
// middleware.TenantContext from X-Tenant-Id).
//
// Nothing in this service used to read that header. A tenant_id in a body chose
// whose configuration to overwrite, and an ABSENT ?tenant_id= on a list route
// was documented as "entries across all tenants" — a platform-wide read of every
// tenant's configuration and feature-flag state.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context())); id != "" {
		return id, true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "tenant_scope_missing",
		"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
	})
	return "", false
}

// refuseForeignTenant reports whether claimed names a tenant other than the
// caller's verified scope, answering 403 if so. A nil or empty claimed value is
// not a disagreement: nil tenant_id means the GLOBAL default for that
// environment, which is a deliberate scope rather than an omission.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, claimed *string, tenantID string) bool {
	if claimed == nil || *claimed == "" || *claimed == tenantID {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error":   "tenant_scope_mismatch",
		"message": "request tenant_id does not match the caller's verified tenant scope",
	})
	return true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(r.Header.Get("X-Principal-Id")); id != "" {
		return id, true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "missing_principal",
		"message": "X-Principal-Id is required — the gateway sets it from a verified identity envelope",
	})
	return "", false
}

// authorize fails closed on both a denial and an unobtainable decision.
//
// The two outcomes are counted separately, and that separation is the point: a
// denial is a permissions problem and an unobtainable decision is an outage,
// they need opposite responses, and on the wire they are a 403 and a 503 that
// http_requests_total cannot distinguish from any other refusal this service
// makes.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string, writes *prometheus.CounterVec) bool {
	scope := h.authzPlatformScopeID
	if legalEntityID != "" {
		scope = legalEntityID
	}
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType)
	switch {
	case err == nil:
		h.metrics.AuthZDecisions.WithLabelValues(actionType, telemetry.AuthZGranted).Inc()
		return true
	case errors.Is(err, authz.ErrDenied):
		h.metrics.AuthZDecisions.WithLabelValues(actionType, telemetry.AuthZDenied).Inc()
		writes.WithLabelValues(telemetry.WriteForbidden).Inc()
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"action":  actionType,
			"message": "the caller holds no grant for " + actionType,
		})
	default:
		h.metrics.AuthZDecisions.WithLabelValues(actionType, telemetry.AuthZUnavailable).Inc()
		writes.WithLabelValues(telemetry.WriteAuthzUnavailable).Inc()
		h.log.Error("authorization check failed — refusing the mutation",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authz_unavailable"})
	}
	return false
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func (h *Handler) decodeJSON(w http.ResponseWriter, r *http.Request, dst any, writes *prometheus.CounterVec) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writes.WithLabelValues(telemetry.WriteTooLarge).Inc()
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
			return false
		}
		writes.WithLabelValues(telemetry.WriteInvalidRequest).Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return false
	}
	return true
}
