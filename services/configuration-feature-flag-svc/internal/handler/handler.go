package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

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

	// The AA-001 governed surface (ZS-SVC-AA-001). Writes are gated on a
	// registered definition, land in snapshot history, and enqueue their
	// event family in the same transaction — see internal/store/aa001_store.go.
	// Reads (GetDefinition, Resolve, EvaluateFlag) are snapshot-pinned.
	CreateDefinition(ctx context.Context, params domain.CreateDefinitionParams) (*domain.ConfigDefinition, error)
	GetDefinition(ctx context.Context, key string) (*domain.ConfigDefinition, error)
	PublishDefinition(ctx context.Context, params domain.PublishDefinitionParams) (*domain.ConfigDefinitionVersion, error)

	Resolve(ctx context.Context, params domain.ResolveParams) (*domain.ResolvedConfigSnapshot, error)

	ActivateOverride(ctx context.Context, params domain.ActivateOverrideParams) (*domain.ConfigEntry, error)

	CreateChange(ctx context.Context, params domain.CreateChangeParams) (*domain.ConfigChange, error)
	ApproveChange(ctx context.Context, changeID string, approval domain.ChangeApproval, callerTenantID string) (*domain.ConfigChange, error)
	ActivateChange(ctx context.Context, changeID, callerTenantID, actor string) (*domain.ConfigChange, error)

	CreateEmergencyChange(ctx context.Context, params domain.CreateEmergencyChangeParams) (*domain.EmergencyChange, error)
	ActivateEmergencyChange(ctx context.Context, emergencyChangeID, callerTenantID, actor string) (*domain.EmergencyChange, error)

	RecordAttestation(ctx context.Context, params domain.RecordAttestationParams) (*domain.RuntimeAttestation, error)

	CreateReleasePlan(ctx context.Context, params domain.CreateReleasePlanParams) (*domain.ReleasePlan, error)
	EvaluateFlag(ctx context.Context, params domain.EvaluateFlagParams) (*domain.FlagEvaluation, error)
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

	// AA-001 governed surface (ZS-SVC-AA-001): the key registry, scoped
	// resolution, override layers, governed change sets, break-glass emergency
	// changes, runtime attestation and flag release plans.
	r.Post("/v1/config/definitions", h.CreateConfigDefinition)
	r.Get("/v1/config/definitions/{key}", h.FindConfigDefinition)
	r.Post("/v1/config/definitions/{key}/publish", h.PublishConfigDefinition)

	r.Post("/v1/config/resolve", h.ResolveConfig)

	r.Put("/v1/config/overrides/{scope}", h.ActivateOverride)

	r.Post("/v1/config/changes", h.CreateChange)
	r.Post("/v1/config/changes/{change_id}/approve", h.ApproveChange)
	r.Post("/v1/config/changes/{change_id}/activate", h.ActivateChange)

	r.Post("/v1/emergency-changes", h.CreateEmergencyChange)
	r.Post("/v1/emergency-changes/{emergency_change_id}/activate", h.ActivateEmergencyChange)

	r.Post("/v1/runtime/attest", h.RecordAttestation)

	r.Post("/v1/flags/{key}/release-plans", h.CreateReleasePlan)
	r.Post("/v1/flags/{key}/evaluate", h.EvaluateFlag)
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
//
// The writes vec may be nil: the AA-001 routes count refusals on the two-label
// GovernedWrites series only from the point the action is known, so an
// invalid-JSON refusal before classification is not counted there.
func (h *Handler) decodeJSON(w http.ResponseWriter, r *http.Request, dst any, writes *prometheus.CounterVec) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			if writes != nil {
				writes.WithLabelValues(telemetry.WriteTooLarge).Inc()
			}
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
			return false
		}
		if writes != nil {
			writes.WithLabelValues(telemetry.WriteInvalidRequest).Inc()
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return false
	}
	return true
}

// ══ AA-001 governed surface (ZS-SVC-AA-001) ═════════════════════════════════
//
// Every route here is documented in openapi.yaml; every wire code the routes
// can emit is a domain sentinel below (governedCodeStatus) or a literal the
// audit already cross-checks against openapi.yaml. The governed writes count
// on GovernedWrites (action, outcome); success records WriteCreated, a no-op
// WriteNoChange, and each refusal the outcome of its HTTP class.

// governedCodeStatus maps each ZS-SVC-AA-001 refusal code to the status
// openapi.yaml documents for it. A coded refusal is a 4xx the contract names,
// never a guess; a code the map does not know (a sentinel registered before
// this map caught up) falls through to 503 so it can never be mistaken for a
// succeeded write.
var governedCodeStatus = map[string]int{
	"key_not_registered":             http.StatusBadRequest,
	"type_mismatch":                  http.StatusBadRequest,
	"value_constraint_failed":        http.StatusBadRequest,
	"secret_value_prohibited":        http.StatusBadRequest,
	"environment_boundary_violation": http.StatusBadRequest,
	"emergency_change_no_expiry":     http.StatusBadRequest,
	"context_incomplete":             http.StatusBadRequest,

	"scope_not_allowed":        http.StatusForbidden,
	"targeting_not_permitted":  http.StatusForbidden,
	"entitlement_denied":       http.StatusForbidden,
	"policy_or_privacy_denied": http.StatusForbidden,
	"emergency_scope_denied":   http.StatusForbidden,

	"override_conflict":          http.StatusConflict,
	"version_immutable":          http.StatusConflict,
	"snapshot_rollback_rejected": http.StatusConflict,
	"change_approval_required":   http.StatusConflict,
	"drift_detected":             http.StatusConflict,
	"consumer_incompatible":      http.StatusConflict,
	"flag_key_retired":           http.StatusConflict,

	"no_attested_snapshot": http.StatusNotFound,

	"snapshot_stale":       http.StatusUnprocessableEntity,
	"safe_fallback_active": http.StatusUnprocessableEntity,

	"snapshot_invalid": http.StatusInternalServerError,
}

// governedOutcome buckets a refusal's HTTP class into a GovernedWrites outcome.
func governedOutcome(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return telemetry.WriteInvalidRequest
	case http.StatusForbidden:
		return telemetry.WriteForbidden
	case http.StatusConflict, http.StatusNotFound:
		return telemetry.WriteConflict
	default:
		return telemetry.WriteStoreUnavailable
	}
}

// governedAction picks the authorization action for a configuration-family
// write by scope, matching how POST /v1/config and POST /v1/flags decide: the
// SCOPE decides, and nil tenant_id is the environment-wide default that takes
// effect for every tenant without its own value.
func governedAction(resource string, tenantID *string) string {
	global := tenantID == nil || *tenantID == ""
	if resource == "flag" {
		if global {
			return telemetry.ActionFlagGlobalWrite
		}
		return telemetry.ActionFlagWrite
	}
	if global {
		return telemetry.ActionConfigGlobalWrite
	}
	return telemetry.ActionConfigWrite
}

// authorizeGoverned is the AA-001 analogue of authorize: same fail-closed
// posture, same two-outcome separation (a denial is a permissions problem, an
// unanswerable decision is an outage). It reports into the (action, outcome)
// governed series rather than the single-label vec, so it returns the outcome
// for the caller to record, or "" when the write may proceed.
func (h *Handler) authorizeGoverned(w http.ResponseWriter, r *http.Request, principalID, action string) string {
	scope := h.authzPlatformScopeID
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, action)
	switch {
	case err == nil:
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZGranted).Inc()
		return ""
	case errors.Is(err, authz.ErrDenied):
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZDenied).Inc()
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"action":  action,
			"message": "the caller holds no grant for " + action,
		})
		return telemetry.WriteForbidden
	default:
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZUnavailable).Inc()
		h.log.Error("authorization check failed — refusing the governed write",
			zap.String("principal_id", principalID),
			zap.String("action_type", action),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authz_unavailable"})
		return telemetry.WriteAuthzUnavailable
	}
}

// governedRefusal writes the wire error for an AA-001 store return. A coded
// refusal answers the status the contract pins; anything else is a store
// outage and answers 503. action names the GovernedWrites series ("" on the
// pure-read routes, which do not count).
func (h *Handler) governedRefusal(w http.ResponseWriter, action, logOp, correlationID string, err error) {
	status := http.StatusServiceUnavailable
	code := "store_unavailable"
	if c := domain.ErrorCode(err); c != "" {
		if s, ok := governedCodeStatus[c]; ok {
			status = s
			code = c
		}
	}
	if action != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, governedOutcome(status)).Inc()
	}
	h.log.Warn(logOp+" refused",
		zap.String("correlation_id", correlationID),
		zap.Int("status", status),
		zap.Error(err),
	)
	writeJSON(w, status, map[string]string{"error": code, "message": err.Error()})
}

// ── POST /v1/config/definitions ──────────────────────────────────────────────

// createConfigDefinitionRequest is the wire shape for POST /v1/config/definitions.
type createConfigDefinitionRequest struct {
	Key                string          `json:"key"`
	Owner              string          `json:"owner"`
	ValueType          string          `json:"value_type"`
	SafetyClass        string          `json:"safety_class"`
	AllowedScopes      []string        `json:"allowed_scopes"`
	DefaultValue       json.RawMessage `json:"default_value,omitempty"`
	FallbackPolicy     string          `json:"fallback_policy"`
	Sensitivity        string          `json:"sensitivity"`
	Validation         json.RawMessage `json:"validation,omitempty"`
	EffectiveModel     string          `json:"effective_model,omitempty"`
	FlagClass          *string         `json:"flag_class,omitempty"`
	RetirementDeadline *time.Time      `json:"retirement_deadline,omitempty"`
}

func (req createConfigDefinitionRequest) missingField() string {
	switch {
	case req.Key == "":
		return "key"
	case req.Owner == "":
		return "owner"
	case req.ValueType == "":
		return "value_type"
	case req.SafetyClass == "":
		return "safety_class"
	case len(req.AllowedScopes) == 0:
		return "allowed_scopes"
	case req.FallbackPolicy == "":
		return "fallback_policy"
	case req.Sensitivity == "":
		return "sensitivity"
	default:
		return ""
	}
}

// CreateConfigDefinition registers a working declaration for a configuration
// key (ZS-SVC-AA-001 Table 11). The declaration is editable while unpublished;
// the store validates value_type, safety_class, sensitivity, fallback_policy
// and allowed_scopes against the DB CHECKs and enforces INV-08's aligned scopes
// at write time. EffectiveModel defaults to IMMEDIATE when omitted.
//
// Response:
//
//	201 → the working definition
//	401/403 → identity or grant refusal (authorization_denied)
//	400 → missing_field / value_constraint_failed (invalid enum or misaligned scopes)
//	503 → store or authz unavailable
func (h *Handler) CreateConfigDefinition(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req createConfigDefinitionRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	effectivemodel := domain.EffectiveModelImmediate
	if req.EffectiveModel != "" {
		effectivemodel = req.EffectiveModel
	}
	params := domain.CreateDefinitionParams{
		Key:                req.Key,
		Owner:              req.Owner,
		ValueType:          req.ValueType,
		SafetyClass:        req.SafetyClass,
		AllowedScopes:      req.AllowedScopes,
		DefaultValue:       req.DefaultValue,
		FallbackPolicy:     req.FallbackPolicy,
		Sensitivity:        req.Sensitivity,
		Validation:         req.Validation,
		EffectiveModel:     effectivemodel,
		FlagClass:          req.FlagClass,
		RetirementDeadline: req.RetirementDeadline,
		ActorPrincipalID:   principalID,
		CorrelationID:      correlationID,
	}

	if o := h.authorizeGoverned(w, r, principalID, telemetry.ActionConfigWrite); o != "" {
		h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, o).Inc()
		return
	}

	def, err := h.store.CreateDefinition(r.Context(), params)
	if err != nil {
		h.governedRefusal(w, telemetry.ActionConfigWrite, "CreateConfigDefinition", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteCreated).Inc()
	h.log.Info("config definition registered",
		zap.String("definition_id", def.DefinitionID),
		zap.String("key", req.Key),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, def)
}

// ── GET /v1/config/definitions/{key} ─────────────────────────────────────────

// FindConfigDefinition returns the ${key} working declaration (ZS-SVC-AA-001
// Table 11). A key that was never declared answers 404, not 400: nothing about
// the caller's request is invalid — the registry simply has no such key.
//
// Response:
//
//	200 → the working definition
//	404 → key_not_registered (never declared)
//	503 → store unavailable
func (h *Handler) FindConfigDefinition(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	def, err := h.store.GetDefinition(r.Context(), key)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrKeyNotRegistered):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "key_not_registered",
				"key":     key,
				"message": domain.ErrKeyNotRegistered.Error(),
			})
		default:
			h.log.Error("FindConfigDefinition: store unavailable",
				zap.String("key", key),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, def)
}

// ── POST /v1/config/definitions/{key}/publish ────────────────────────────────

// publishDefinitionRequest is the wire shape for POST /v1/config/definitions/{key}/publish.
type publishDefinitionRequest struct {
	Lifecycle string `json:"lifecycle"`
}

// PublishConfigDefinition copies the ${key} working declaration into an
// immutable ConfigDefinitionVersion and advances the working row's lifecycle
// (INV-04). Publishing is a platform-wide act — the declaration every future
// read and write will be gated against — so it is authorized as
// CONFIGURATION_GLOBAL_WRITE and always records the verified principal as the
// publisher.
//
// Response:
//
//	201 → the immutable published version
//	400 → missing lifecycle, or value_constraint_failed (lifecycle not PUBLISHED/DEPRECATED/RETIRED)
//	404 → key_not_registered (no declaration to publish)
//	503 → store or authz unavailable
func (h *Handler) PublishConfigDefinition(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	key := chi.URLParam(r, "key")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req publishDefinitionRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if req.Lifecycle == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "lifecycle",
		})
		return
	}

	if outcome := h.authorizeGoverned(w, r, principalID, telemetry.ActionConfigGlobalWrite); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigGlobalWrite, outcome).Inc()
		return
	}

	def, err := h.store.GetDefinition(r.Context(), key)
	if err != nil {
		if errors.Is(err, domain.ErrKeyNotRegistered) {
			h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigGlobalWrite, telemetry.WriteConflict).Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "key_not_registered",
				"key":     key,
				"message": domain.ErrKeyNotRegistered.Error(),
			})
			return
		}
		h.governedRefusal(w, telemetry.ActionConfigGlobalWrite, "PublishConfigDefinition:resolve", correlationID, err)
		return
	}

	version, err := h.store.PublishDefinition(r.Context(), domain.PublishDefinitionParams{
		DefinitionID:     def.DefinitionID,
		Lifecycle:        req.Lifecycle,
		ActorPrincipalID: principalID,
		CorrelationID:    correlationID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrKeyNotRegistered) {
			h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigGlobalWrite, telemetry.WriteConflict).Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "key_not_registered",
				"key":     key,
				"message": domain.ErrKeyNotRegistered.Error(),
			})
			return
		}
		h.governedRefusal(w, telemetry.ActionConfigGlobalWrite, "PublishConfigDefinition", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigGlobalWrite, telemetry.WriteCreated).Inc()
	h.log.Info("config definition published",
		zap.String("version_id", version.VersionID),
		zap.String("key", key),
		zap.String("lifecycle", version.Lifecycle),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, version)
}

// ── POST /v1/config/resolve ──────────────────────────────────────────────────

// resolveRequest is the wire shape for POST /v1/config/resolve: the scope and
// an optional key allowlist. Keys empty means every key applicable to the
// scope.
type resolveRequest struct {
	Environment string   `json:"environment"`
	TenantID    *string  `json:"tenant_id,omitempty"`
	Keys        []string `json:"keys,omitempty"`
}

// ResolveConfig returns the snapshot-pinned value for this scope
// (ZS-SVC-AA-001 Table 6/14). A body-carrying read: it is a resolve, not a
// material write, so the envelope policy classifies it as a non-write and no
// idempotency key is demanded. It requires a tenant (the gateway always
// provides one) and never leaks a foreign tenant's resolution.
//
// Response:
//
//	200 → the resolved snapshot (values, per-key outcome, freshness deadline)
//	404 → no_attested_snapshot (nothing pinned in this environment yet)
//	400 → missing environment / scope_not_allowed on a requested key
//	503 → store unavailable
func (h *Handler) ResolveConfig(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req resolveRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if req.Environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "environment",
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	snap, err := h.store.Resolve(r.Context(), domain.ResolveParams{
		Environment:   req.Environment,
		TenantID:      req.TenantID,
		Keys:          req.Keys,
		CorrelationID: correlationID,
	})
	if err != nil {
		h.governedRefusal(w, "", "ResolveConfig", correlationID, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ── PUT /v1/config/overrides/{scope} ─────────────────────────────────────────

// activateOverrideRequest is the wire shape for PUT /v1/config/overrides/{scope}.
// The layer comes from the path — environment or tenant — and ScopeID names the
// layer's instance (half of the {scope} being "tenant", the tenant id the
// caller is verified for).
type activateOverrideRequest struct {
	Key         string          `json:"key"`
	Environment string          `json:"environment"`
	Value       json.RawMessage `json:"value"`
	ScopeID     *string         `json:"scope_id,omitempty"`
}

func (req activateOverrideRequest) missingField() string {
	switch {
	case req.Key == "":
		return "key"
	case req.Environment == "":
		return "environment"
	case len(req.Value) == 0:
		return "value"
	default:
		return ""
	}
}

// ActivateOverride sets an override at one of the five precedence layers of
// INV-07. The definition's allowed_scopes is an allowlist, not a hint (INV-08):
// a key whose definition does not name the requested layer is refused before
// any value is written. The scope decides the action like every other write —
// a tenant-layer override is CONFIGURATION_WRITE, the environment layer is
// CONFIGURATION_GLOBAL_WRITE.
//
// Response:
//
//	200 → the config entry now effective at this layer
//	400 → missing_field / key_not_registered / type_mismatch / value_constraint_failed
//	403 → scope_not_allowed / authorization_denied
//	409 → override_conflict (superseded concurrently)
//	503 → store or authz unavailable
func (h *Handler) ActivateOverride(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	scope := chi.URLParam(r, "scope")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var layer string
	switch scope {
	case "tenant":
		layer = domain.ScopeTenant
	case "environment":
		layer = domain.ScopeEnvironment
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_field",
			"field":   "scope",
			"message": `must be one of "tenant", "environment"`,
		})
		return
	}

	var req activateOverrideRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	// For the tenant layer the override belongs to ONE tenant and that tenant
	// is the caller — a tenant-layer override for any other tenant is refused
	// here, before anything is written.
	if layer == domain.ScopeTenant {
		if h.refuseForeignTenant(w, req.ScopeID, tenantScope) {
			return
		}
	} else if req.ScopeID != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_field",
			"field":   "scope_id",
			"message": "scope_id is only valid for the tenant override layer",
		})
		return
	}

	action := governedAction("config", tenantIDForOverride(layer, req))
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}

	created, err := h.store.ActivateOverride(r.Context(), domain.ActivateOverrideParams{
		Key:              req.Key,
		Layer:            layer,
		Environment:      req.Environment,
		ScopeID:          req.ScopeID,
		Value:            req.Value,
		CallerTenantID:   tenantScope,
		ActorPrincipalID: principalID,
		CorrelationID:    correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "ActivateOverride", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("config override activated",
		zap.String("config_id", created.ConfigID),
		zap.String("key", req.Key),
		zap.String("layer", layer),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, created)
}

// tenantIDForOverride is the tenant the override applies to: at the tenant
// layer it is the scope the caller claimed; at the environment layer
// overrides are global, so the write is a global-scope act.
func tenantIDForOverride(layer string, req activateOverrideRequest) *string {
	if layer == domain.ScopeTenant {
		return req.ScopeID
	}
	return nil
}

// ── POST /v1/config/changes ──────────────────────────────────────────────────

// changePartRequest is one mutation inside a change request's parts array.
type changePartRequest struct {
	Kind               string                 `json:"kind"`
	Key                string                 `json:"key"`
	Scope              changePartScopeRequest `json:"scope"`
	NewValue           json.RawMessage        `json:"new_value,omitempty"`
	NewEnabled         *bool                  `json:"new_enabled,omitempty"`
	RolloutPercentage  *int                   `json:"rollout_percentage,omitempty"`
	ExpectedBeforeHash *string                `json:"expected_before_hash,omitempty"`
}

type changePartScopeRequest struct {
	Environment string  `json:"environment"`
	TenantID    *string `json:"tenant_id,omitempty"`
}

// createChangeRequest is the wire shape for POST /v1/config/changes.
type createChangeRequest struct {
	ChangeClass        string              `json:"change_class"`
	Environment        string              `json:"environment"`
	TenantID           *string             `json:"tenant_id,omitempty"`
	Parts              []changePartRequest `json:"parts"`
	ApprovalRequired   bool                `json:"approval_required,omitempty"`
	PlannedEffectiveAt *time.Time          `json:"planned_effective_at,omitempty"`
	RollbackChangeID   *string             `json:"rollback_change_id,omitempty"`
}

func (req createChangeRequest) missingField() string {
	switch {
	case req.ChangeClass == "":
		return "change_class"
	case req.Environment == "":
		return "environment"
	case len(req.Parts) == 0:
		return "parts"
	default:
		return ""
	}
}

// CreateChange records a governed ChangeSet (ZS-SVC-AA-001 Table 7/21). The
// parts are ordered; each part is gated against the key's definition and the
// caller's scope at creation time, and re-gated at activation so a stale
// change cannot apply. C2/C3 classes bind an approval before activation
// (TC-04) — the response's approval_required tells the caller whether theirs
// is one of them.
//
// Response:
//
//	201 → the recorded change (status PROPOSED unless approval binds at creation)
//	400 → missing_field / key_not_registered / type_mismatch / value_constraint_failed
//	403 → scope_not_allowed / authorization_denied
//	409 → override_conflict / change_approval_required
//	503 → store or authz unavailable
func (h *Handler) CreateChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createChangeRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	action := governedAction("config", req.TenantID)
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}

	parts := make([]domain.ChangePart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, domain.ChangePart{
			Kind:               p.Kind,
			Key:                p.Key,
			Scope:              domain.ChangePartScope{Environment: p.Scope.Environment, TenantID: p.Scope.TenantID},
			NewValue:           p.NewValue,
			NewEnabled:         p.NewEnabled,
			RolloutPercentage:  p.RolloutPercentage,
			ExpectedBeforeHash: p.ExpectedBeforeHash,
		})
	}

	created, err := h.store.CreateChange(r.Context(), domain.CreateChangeParams{
		ChangeClass:        req.ChangeClass,
		Environment:        req.Environment,
		TenantID:           req.TenantID,
		Parts:              parts,
		ApprovalRequired:   req.ApprovalRequired,
		Approval:           domain.ChangeApproval{Approved: false},
		PlannedEffectiveAt: req.PlannedEffectiveAt,
		RollbackChangeID:   req.RollbackChangeID,
		CallerTenantID:     tenantScope,
		ActorPrincipalID:   principalID,
		CorrelationID:      correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "CreateChange", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("config change recorded",
		zap.String("change_id", created.ChangeID),
		zap.String("change_class", created.ChangeClass),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, created)
}

// ── POST /v1/config/changes/{change_id}/approve ──────────────────────────────

// approveChangeRequest is the wire shape for POST /v1/config/changes/{change_id}/approve.
// `approved` defaults to true; the approver is the verified principal, never a
// body-supplied name.
type approveChangeRequest struct {
	Approved     *bool     `json:"approved,omitempty"`
	ApprovedAt   time.Time `json:"approved_at,omitempty"`
	WFCReference *string   `json:"wfc_reference,omitempty"`
}

// ApproveChange binds (or records the rejection of) an approval for ${change_id}.
// A rejected approval keeps the change PROPOSED — there is no REJECTED state —
// so an approver who reconsiders can re-approve.
//
// Response:
//
//	200 → the change with its new status (APPROVED, or PROPOSED when rejected)
//	404 → change_not_found
//	400 → value_constraint_failed (empty approver)
//	403 → authorization_denied
//	503 → store or authz unavailable
func (h *Handler) ApproveChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	changeID := chi.URLParam(r, "change_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req approveChangeRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	approved := true
	if req.Approved != nil {
		approved = *req.Approved
	}
	approvedAt := time.Now().UTC()
	if !req.ApprovedAt.IsZero() {
		approvedAt = req.ApprovedAt
	}

	if outcome := h.authorizeGoverned(w, r, principalID, telemetry.ActionConfigWrite); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, outcome).Inc()
		return
	}

	updated, err := h.store.ApproveChange(r.Context(), changeID, domain.ChangeApproval{
		Approved:      approved,
		ByPrincipalID: principalID,
		ApprovedAt:    approvedAt,
		WFCReference:  req.WFCReference,
	}, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrChangeNotFound) {
			h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteConflict).Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":     "change_not_found",
				"change_id": changeID,
			})
			return
		}
		h.governedRefusal(w, telemetry.ActionConfigWrite, "ApproveChange", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteCreated).Inc()
	h.log.Info("config change approval recorded",
		zap.String("change_id", changeID),
		zap.Bool("approved", approved),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/config/changes/{change_id}/activate ─────────────────────────────

// ActivateChange applies an APPROVED change's parts to the live value tables,
// mints the after snapshot and marks the change VERIFIED with both imprints
// (INV-25/26). A change that never cleared approval is refused (TC-04); the
// parts are re-gated against the definitions at activation time so a change
// written against yesterday's registry cannot slip through today's.
//
// Response:
//
//	200 → the verified change
//	404 → change_not_found
//	409 → change_approval_required
//	400 → key_not_registered / type_mismatch / value_constraint_failed / override_conflict
//	403 → scope_not_allowed / authorization_denied
//	503 → store or authz unavailable
func (h *Handler) ActivateChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	changeID := chi.URLParam(r, "change_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if outcome := h.authorizeGoverned(w, r, principalID, telemetry.ActionConfigWrite); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, outcome).Inc()
		return
	}

	updated, err := h.store.ActivateChange(r.Context(), changeID, tenantScope, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrChangeNotFound) {
			h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteConflict).Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":     "change_not_found",
				"change_id": changeID,
			})
			return
		}
		h.governedRefusal(w, telemetry.ActionConfigWrite, "ActivateChange", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteCreated).Inc()
	h.log.Info("config change verified",
		zap.String("change_id", changeID),
		zap.String("proposed_snapshot_id", derefString(updated.ProposedSnapshotID)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/emergency-changes ───────────────────────────────────────────────

// createEmergencyChangeRequest is the wire shape for POST /v1/emergency-changes.
type createEmergencyChangeRequest struct {
	Key         string          `json:"key"`
	Environment string          `json:"environment"`
	TenantID    *string         `json:"tenant_id,omitempty"`
	NewValue    json.RawMessage `json:"new_value"`
	Reason      string          `json:"reason"`
	IncidentID  string          `json:"incident_id"`
	ExpiresAt   *time.Time      `json:"expires_at"`
}

func (req createEmergencyChangeRequest) missingField() string {
	switch {
	case req.Key == "":
		return "key"
	case req.Environment == "":
		return "environment"
	case len(req.NewValue) == 0:
		return "new_value"
	case req.Reason == "":
		return "reason"
	case req.IncidentID == "":
		return "incident_id"
	case req.ExpiresAt == nil:
		return "expires_at"
	default:
		return ""
	}
}

// CreateEmergencyChange records a time-boxed break-glass mutation (INV-15,
// NP-42/43). Break-glass must still record an approval and a quarantine until
// the emergent value is attested inside its own window. Expiry is mandatory —
// a permanent break-glass override is not break-glass. The existing scope-deed
// rule applies to the action: no tenant_id means the change targets the
// environment-wide default and demands the global grant.
//
// Response:
//
//	201 → the recorded emergency change (status OPEN)
//	400 → missing_field / key_not_registered / type_mismatch / value_constraint_failed / emergency_change_no_expiry
//	403 → emergency_scope_denied / scope_not_allowed / authorization_denied
//	503 → store or authz unavailable
func (h *Handler) CreateEmergencyChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createEmergencyChangeRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	action := governedAction("config", req.TenantID)
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}

	created, err := h.store.CreateEmergencyChange(r.Context(), domain.CreateEmergencyChangeParams{
		Key:              req.Key,
		Environment:      req.Environment,
		TenantID:         req.TenantID,
		NewValue:         req.NewValue,
		Reason:           req.Reason,
		IncidentID:       req.IncidentID,
		ActorPrincipalID: principalID,
		ExpiresAt:        req.ExpiresAt.UTC(),
		CallerTenantID:   tenantScope,
		CorrelationID:    correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "CreateEmergencyChange", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("emergency change recorded",
		zap.String("emergency_change_id", created.EmergencyChangeID),
		zap.String("key", req.Key),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, created)
}

// ── POST /v1/emergency-changes/{emergency_change_id}/activate ────────────────

// ActivateEmergencyChange applies an OPEN emergency change immediately and
// pins it into a snapshot so the next attestation either sees it or is
// visible as missing. The actor is the verified principal.
//
// Response:
//
//	200 → the active emergency change
//	404 → change_not_found
//	403 → authorization_denied
//	400 → key_not_registered / type_mismatch / value_constraint_failed
//	503 → store or authz unavailable
func (h *Handler) ActivateEmergencyChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	emergencyChangeID := chi.URLParam(r, "emergency_change_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if outcome := h.authorizeGoverned(w, r, principalID, telemetry.ActionConfigWrite); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, outcome).Inc()
		return
	}

	activated, err := h.store.ActivateEmergencyChange(r.Context(), emergencyChangeID, tenantScope, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrChangeNotFound) {
			h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteConflict).Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":               "change_not_found",
				"emergency_change_id": emergencyChangeID,
			})
			return
		}
		h.governedRefusal(w, telemetry.ActionConfigWrite, "ActivateEmergencyChange", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(telemetry.ActionConfigWrite, telemetry.WriteCreated).Inc()
	h.log.Info("emergency change activated",
		zap.String("emergency_change_id", emergencyChangeID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, activated)
}

// ── POST /v1/runtime/attest ──────────────────────────────────────────────────

// recordAttestationRequest is the wire shape for POST /v1/runtime/attest.
type recordAttestationRequest struct {
	RuntimeID          string          `json:"runtime_id"`
	AttestKey          string          `json:"attest_key"`
	Environment        string          `json:"environment"`
	TenantID           *string         `json:"tenant_id,omitempty"`
	ObservedSnapshotID *string         `json:"observed_snapshot_id,omitempty"`
	ObservedEpoch      int64           `json:"observed_epoch"`
	ObservedDigest     string          `json:"observed_digest"`
	ObservedVersions   json.RawMessage `json:"observed_versions,omitempty"`
}

func (req recordAttestationRequest) missingField() string {
	switch {
	case req.RuntimeID == "":
		return "runtime_id"
	case req.AttestKey == "":
		return "attest_key"
	case req.Environment == "":
		return "environment"
	case req.ObservedDigest == "":
		return "observed_digest"
	default:
		return ""
	}
}

// RecordAttestation records a workload's claim about which snapshot it serves
// (TC-07, Table 22). The (runtime_id, attest_key) pair is single-use: a
// captured attestation cannot be replayed (NP-26) — a retried claim of the
// same key is refused, so a workload re-attests with a fresh key.
//
// Response:
//
//	201 → the recorded attestation (reported_at, freshness_deadline)
//	400 → missing_field / no known snapshot for the claimed environment
//	403 → authorization_denied (config write grant, tenant-scoped)
//	503 → store or authz unavailable
func (h *Handler) RecordAttestation(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req recordAttestationRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	action := telemetry.ActionConfigWrite
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}

	attestation, err := h.store.RecordAttestation(r.Context(), domain.RecordAttestationParams{
		RuntimeID:          req.RuntimeID,
		AttestKey:          req.AttestKey,
		Environment:        req.Environment,
		TenantID:           req.TenantID,
		ObservedSnapshotID: req.ObservedSnapshotID,
		ObservedEpoch:      req.ObservedEpoch,
		ObservedDigest:     req.ObservedDigest,
		ObservedVersions:   req.ObservedVersions,
		CallerTenantID:     tenantScope,
	})
	if err != nil {
		h.governedRefusal(w, action, "RecordAttestation", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("runtime attestation recorded",
		zap.String("attestation_id", attestation.AttestationID),
		zap.String("runtime_id", req.RuntimeID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, attestation)
}

// ── POST /v1/flags/{key}/release-plans ───────────────────────────────────────

// createReleasePlanRequest is the wire shape for POST /v1/flags/{key}/release-plans.
type createReleasePlanRequest struct {
	Environment    string          `json:"environment"`
	TenantID       *string         `json:"tenant_id,omitempty"`
	Strategy       string          `json:"strategy"`
	Salt           string          `json:"salt,omitempty"`
	BucketCount    int             `json:"bucket_count,omitempty"`
	TargetingRules json.RawMessage `json:"targeting_rules,omitempty"`
}

// CreateReleasePlan publishes an immutable rollout/targeting plan for this
// flag (INV-04/07/19, Table 16/17). Targeting rules are immutable once
// published; PERCENTAGE bucketing is deterministic for a stable subject key +
// salt (INV-07), and ALL_OR_NOTHING is the only legal strategy for
// regulated/rights-affecting outcomes (INV-19). Authorized on the flag
// action family — the tenant-scoped flag write for a tenant plan, the global
// one otherwise.
//
// Response:
//
//	201 → the published plan
//	400 → missing_field / key_not_registered / value_constraint_failed (strategy not legal)
//	403 → authorization_denied / scope_not_allowed
//	503 → store or authz unavailable
func (h *Handler) CreateReleasePlan(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	key := chi.URLParam(r, "key")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createReleasePlanRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if req.Environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "environment",
		})
		return
	}
	if req.Strategy == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "strategy",
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	action := governedAction("flag", req.TenantID)
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}

	plan, err := h.store.CreateReleasePlan(r.Context(), domain.CreateReleasePlanParams{
		FlagKey:          key,
		Environment:      req.Environment,
		TenantID:         req.TenantID,
		Strategy:         req.Strategy,
		Salt:             req.Salt,
		BucketCount:      req.BucketCount,
		TargetingRules:   req.TargetingRules,
		CallerTenantID:   tenantScope,
		ActorPrincipalID: principalID,
		CorrelationID:    correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "CreateReleasePlan", correlationID, err)
		return
	}

	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("release plan published",
		zap.String("release_plan_id", plan.ReleasePlanID),
		zap.String("flag_key", key),
		zap.String("strategy", plan.Strategy),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, plan)
}

// ── POST /v1/flags/{key}/evaluate ────────────────────────────────────────────

// evaluateRequest is the wire shape for POST /v1/flags/{key}/evaluate: the
// scope, a stable subject key for deterministic bucketing, and the context
// attributes the eligibility filters read.
type evaluateRequest struct {
	Environment string         `json:"environment"`
	TenantID    *string        `json:"tenant_id,omitempty"`
	SubjectKey  string         `json:"subject_key"`
	Context     map[string]any `json:"context,omitempty"`
}

// EvaluateFlag evaluates this flag for one subject against the pinned snapshot
// (INV-12). Like resolve, it is a body-carrying read: the envelope policy
// classifies /v1/flags/{key}/evaluate as a non-write, so no idempotency key is
// demanded. It requires a tenant and never evaluates a foreign tenant's scope.
//
// Response:
//
//	200 → the enabled decision, bucket, variant and the exact snapshot that produced it
//	400 → missing environment / subject_key / context_incomplete
//	404 → no_attested_snapshot / flag_key_retired
//	422 → snapshot_stale / safe_fallback_active
//	503 → store unavailable
func (h *Handler) EvaluateFlag(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	key := chi.URLParam(r, "key")

	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req evaluateRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if req.Environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "environment",
		})
		return
	}
	if req.SubjectKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "subject_key",
		})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	result, err := h.store.EvaluateFlag(r.Context(), domain.EvaluateFlagParams{
		Key:           key,
		Environment:   req.Environment,
		TenantID:      req.TenantID,
		SubjectKey:    req.SubjectKey,
		Context:       req.Context,
		CorrelationID: correlationID,
	})
	if err != nil {
		h.governedRefusal(w, "", "EvaluateFlag", correlationID, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// derefString is a defensive pointer dereference for the log field the change
// activation paths report; a nil snapshot link prints as "-".
func derefString(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}
