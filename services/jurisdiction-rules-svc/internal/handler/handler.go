package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// JurisdictionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type JurisdictionStore interface {
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)

	List(ctx context.Context, params store.ListParams) ([]*domain.Jurisdiction, error)

	FindAncestors(ctx context.Context, jurisdictionID string) ([]*domain.Jurisdiction, error)

	FindRules(ctx context.Context, params store.FindRulesParams) ([]*domain.JurisdictionRule, error)

	// FindRulePack resolves the runtime rule pack across the ancestor chain.
	FindRulePack(ctx context.Context, jurisdictionID, ruleDomain string, at time.Time) (*domain.RulePack, error)

	// CreateJurisdiction inserts a new jurisdiction idempotently.
	CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error)

	// DeactivateJurisdiction sets active_flag=false and updates audit columns.
	DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error)

	// FindRuleByID looks up a rule by ID.
	FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error)

	// CreateRule inserts a new rule idempotently.
	CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error)

	// TransitionRuleStatus atomically updates rule_status if current status is
	// in allowedPriors. The bool reports whether anything actually changed.
	TransitionRuleStatus(ctx context.Context, params store.TransitionParams) (*domain.JurisdictionRule, bool, error)

	// RecordDrift moves legal_drift_state and appends to the drift history.
	RecordDrift(ctx context.Context, params domain.RecordDriftParams) (*domain.JurisdictionRule, *domain.DriftEvent, bool, error)

	// FindDriftEvents returns the append-only drift history for a rule.
	FindDriftEvents(ctx context.Context, ruleID string, limit, offset int) ([]*domain.DriftEvent, error)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store     JurisdictionStore
	authz     authz.AuthorizationClient
	publisher events.Publisher
	log       *zap.Logger

	// authzScopeID is the legal_entity_id presented to authorization-svc.
	// See config.AuthZPlatformScopeID for why a platform-wide service needs one.
	authzScopeID string
}

// New constructs a Handler.
func New(store JurisdictionStore, authzClient authz.AuthorizationClient, publisher events.Publisher, authzScopeID string, log *zap.Logger) *Handler {
	return &Handler{
		store:        store,
		authz:        authzClient,
		publisher:    publisher,
		authzScopeID: authzScopeID,
		log:          log,
	}
}

// maxRequestBody caps admin request bodies at 256 KiB.
//
// rule_payload is caller-supplied JSON with no size bound in the schema, so
// without this an admin POST could stream an arbitrarily large body straight
// into memory and into a JSONB column.
const maxRequestBody = 256 << 10

// ruleStatusAllowedPriors defines the only legal prior rule_status for each
// target status. Nothing else in the codebase or docs/architecture defines
// this state machine, so it is owned here, at the boundary where "transition
// to X" is first expressed as a concept.
var ruleStatusAllowedPriors = map[string][]string{
	"ACTIVE":     {"DRAFT"},
	"SUPERSEDED": {"ACTIVE"},
	"RETIRED":    {"ACTIVE", "SUPERSEDED"},
}

// endDatingStatuses are the target statuses that close an open-ended rule.
// A rule left with effective_to = NULL after being superseded keeps matching
// every point-in-time query alongside the rule that replaced it.
var endDatingStatuses = map[string]bool{
	"SUPERSEDED": true,
	"RETIRED":    true,
}

// createableRuleStatuses are the statuses a rule may be created in.
//
// rule_status used to be taken verbatim from the request body, so a caller
// could POST a rule straight into ACTIVE — or into "BANANAS" — and skip the
// DRAFT→ACTIVE state machine entirely. Creation is limited to the two states
// that are not the *result* of a transition.
var createableRuleStatuses = map[string]bool{
	"DRAFT":  true,
	"ACTIVE": true,
}

// driftStates is the legal_drift_state value space (OQ-4). Same reasoning as
// ruleStatusAllowedPriors: the transition target has to be checked somewhere,
// and this is the boundary where it is first named.
var driftStates = map[string]bool{
	"CURRENT":      true,
	"DRIFTED":      true,
	"UNDER_REVIEW": true,
}

// CreateJurisdictionRequest is the caller-facing request body for
// POST /v1/admin/jurisdictions. Deliberately narrower than
// domain.CreateJurisdictionParams — the client must not be able to set
// jurisdiction_id, active_flag, or created_by_principal_id itself.
type CreateJurisdictionRequest struct {
	JurisdictionCode     string     `json:"jurisdiction_code"`
	JurisdictionName     string     `json:"jurisdiction_name"`
	JurisdictionType     string     `json:"jurisdiction_type"`
	ParentJurisdictionID *string    `json:"parent_jurisdiction_id"`
	AuthorityType        string     `json:"authority_type"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to"`
}

// CreateRuleRequest is the caller-facing request body for
// POST /v1/admin/jurisdictions/{jurisdiction_id}/rules.
type CreateRuleRequest struct {
	RuleDomain            string          `json:"rule_domain"`
	RuleCode              string          `json:"rule_code"`
	RuleName              string          `json:"rule_name"`
	EffectiveFrom         time.Time       `json:"effective_from"`
	EffectiveTo           *time.Time      `json:"effective_to"`
	RulePayload           json.RawMessage `json:"rule_payload"`
	SourceReference       *string         `json:"source_reference"`
	ExternalFeedReference *string         `json:"external_feed_reference"`
	RuleStatus            string          `json:"rule_status"`
}

// TransitionRuleStatusRequest is the caller-facing request body for
// POST /v1/admin/rules/{jurisdiction_rule_id}/transition. The allowed prior
// states are never client-supplied — see ruleStatusAllowedPriors.
type TransitionRuleStatusRequest struct {
	NewStatus string `json:"new_status"`

	// EffectiveTo optionally states when the rule stopped applying, for the
	// transitions that close a rule (SUPERSEDED, RETIRED). Omitted means now.
	// Ignored when the rule already carries an end date.
	EffectiveTo *time.Time `json:"effective_to"`
}

// RecordDriftRequest is the caller-facing request body for
// POST /v1/admin/rules/{jurisdiction_rule_id}/drift.
type RecordDriftRequest struct {
	// DriftState is one of CURRENT, DRIFTED, UNDER_REVIEW.
	DriftState string `json:"drift_state"`

	// Reason is the evidence for the change — the regulatory update that
	// diverged from the stored rule, or the review conclusion that closed it.
	Reason *string `json:"reason"`
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
	r.Get("/v1/jurisdictions/{jurisdiction_id}/rule-pack", h.GetRulePack)
	r.Get("/v1/rules/{jurisdiction_rule_id}", h.GetRule)
	r.Get("/v1/rules/{jurisdiction_rule_id}/drift-events", h.GetDriftEvents)

	// ── Admin mutations (AuthZ required on every route) ───────────────────────
	r.Post("/v1/admin/jurisdictions", h.CreateJurisdiction)
	r.Post("/v1/admin/jurisdictions/{jurisdiction_id}/deactivate", h.DeactivateJurisdiction)
	r.Post("/v1/admin/jurisdictions/{jurisdiction_id}/rules", h.CreateRule)
	r.Post("/v1/admin/rules/{jurisdiction_rule_id}/transition", h.TransitionRuleStatus)
	r.Post("/v1/admin/rules/{jurisdiction_rule_id}/drift", h.RecordDrift)
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

// ── Public reads ─────────────────────────────────────────────────────────────

// GetJurisdiction handles GET /v1/jurisdictions/{jurisdiction_id}.
//
// This is the validation endpoint called synchronously (fail-closed) by
// tenant-entity-registry-svc before persisting any EntityJurisdictionAssignment
// or TaxIdentityBundle that references a jurisdiction_id.
//
// Response contract (must match HTTPJurisdictionValidator exactly):
//
//	200 → jurisdiction known and active
//	404 → jurisdiction_id unknown, malformed, inactive, or expired
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
//	400 → limit or offset is not a non-negative integer
//	503 → store unavailable
func (h *Handler) ListJurisdictions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	limit, offset, ok := h.parsePaging(w, q)
	if !ok {
		return
	}

	params := store.ListParams{
		JurisdictionType: q.Get("type"),
		ActiveOnly:       q.Get("active") == "true",
		Limit:            limit,
		Offset:           offset,
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
		h.writeStoreError(w, err, correlationID)
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
// This is the raw, per-jurisdiction, effective-dated view — the audit and
// replay surface. It does NOT resolve inheritance; for the runtime view see
// GetRulePack.
//
// Query parameters (all optional):
//
//	domain=PAYROLL        filter by rule_domain (VARCHAR, data driven)
//	effective_at=2024-01-01T00:00:00Z  point-in-time (RFC3339). Defaults to now.
//	limit=50              page size (max 100, default 50)
//	offset=0              zero-based page offset
//
// Response:
//
//	200 → JSON array of JurisdictionRule objects (may be empty)
//	400 → malformed effective_at, limit, or offset
//	404 → jurisdiction_id not found
//	503 → store unavailable
func (h *Handler) GetRules(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	effectiveAt, ok := h.parseEffectiveAt(w, q)
	if !ok {
		return
	}
	limit, offset, ok := h.parsePaging(w, q)
	if !ok {
		return
	}

	results, err := h.store.FindRules(r.Context(), store.FindRulesParams{
		JurisdictionID: jurisdictionID,
		Domain:         q.Get("domain"), // empty string means all domains
		EffectiveAt:    effectiveAt,
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
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

// GetRulePack handles GET /v1/jurisdictions/{jurisdiction_id}/rule-pack —
// the "resolve jurisdiction set" and "fetch runtime rule pack" inbound APIs
// of 03-microservices.md §8.2.
//
// Unlike GetRules this walks the ancestor chain and returns exactly one
// winning rule per (rule_domain, rule_code): nearest jurisdiction wins, ties
// broken by the later effective_from. DRAFT and RETIRED rules never appear.
// resolved_from names the jurisdictions the pack was assembled from, so a
// caller can record the rule basis of a governed action.
//
// Query parameters (all optional):
//
//	domain=PAYROLL        restrict the pack to one rule_domain
//	effective_at=...      point-in-time (RFC3339). Defaults to now.
//
// Response:
//
//	200 → RulePack
//	400 → malformed effective_at
//	404 → jurisdiction_id unknown, inactive, or expired
//	503 → store unavailable
func (h *Handler) GetRulePack(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	effectiveAt, ok := h.parseEffectiveAt(w, q)
	if !ok {
		return
	}

	pack, err := h.store.FindRulePack(r.Context(), jurisdictionID, q.Get("domain"), effectiveAt)
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	h.log.Debug("GetRulePack",
		zap.String("jurisdiction_id", jurisdictionID),
		zap.Int("rule_count", len(pack.Rules)),
		zap.Int("chain_length", len(pack.ResolvedFrom)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, pack)
}

// GetRule handles GET /v1/rules/{jurisdiction_rule_id}.
//
// The rule id appears in every published event and in the rule basis
// recorded against governed actions, but there was no way to read a rule
// back by that id — only by listing a jurisdiction's rules and filtering.
//
// Response: 200 the rule / 404 unknown or malformed id / 503 unavailable.
func (h *Handler) GetRule(w http.ResponseWriter, r *http.Request) {
	ruleID := chi.URLParam(r, "jurisdiction_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	rule, err := h.store.FindRuleByID(r.Context(), ruleID)
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// GetDriftEvents handles GET /v1/rules/{jurisdiction_rule_id}/drift-events.
//
// The append-only legal_drift_state history (OQ-4), newest first. The
// current state on the rule says a rule has drifted; this says when, from
// what, on whose authority, and why.
//
// Response:
//
//	200 → JSON array of DriftEvent objects (may be empty)
//	400 → malformed limit or offset
//	404 → jurisdiction_rule_id not found
//	503 → store unavailable
func (h *Handler) GetDriftEvents(w http.ResponseWriter, r *http.Request) {
	ruleID := chi.URLParam(r, "jurisdiction_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}

	history, err := h.store.FindDriftEvents(r.Context(), ruleID, limit, offset)
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}
	if history == nil {
		history = []*domain.DriftEvent{}
	}
	writeJSON(w, http.StatusOK, history)
}

// ── Admin mutations ──────────────────────────────────────────────────────────

// CreateJurisdiction handles POST /v1/admin/jurisdictions.
//
// Response:
//
//	201 → new jurisdiction created
//	200 → idempotent replay of an existing jurisdiction (same dedup key, same attributes)
//	400 → malformed or incomplete request body
//	401 → no caller identity on the request
//	403 → authorization denied
//	404 → parent_jurisdiction_id does not exist
//	409 → dedup key matches an existing jurisdiction with differing attributes
//	503 → authz or store unavailable
func (h *Handler) CreateJurisdiction(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "create"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req CreateJurisdictionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if field := firstBlank(
		requiredField{"jurisdiction_code", req.JurisdictionCode},
		requiredField{"jurisdiction_name", req.JurisdictionName},
		requiredField{"jurisdiction_type", req.JurisdictionType},
		requiredField{"authority_type", req.AuthorityType},
	); field != "" {
		writeMissingField(w, field)
		return
	}
	if req.EffectiveFrom.IsZero() {
		writeMissingField(w, "effective_from")
		return
	}
	if !validEffectivePeriod(req.EffectiveFrom, req.EffectiveTo) {
		writeError(w, http.StatusBadRequest, "invalid_effective_period", "effective_to must be after effective_from")
		return
	}

	j, created, err := h.store.CreateJurisdiction(r.Context(), domain.CreateJurisdictionParams{
		JurisdictionCode:     req.JurisdictionCode,
		JurisdictionName:     req.JurisdictionName,
		JurisdictionType:     req.JurisdictionType,
		ParentJurisdictionID: req.ParentJurisdictionID,
		AuthorityType:        req.AuthorityType,
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		ActiveFlag:           true,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	status := http.StatusOK
	if created {
		// Only a real insert emits. An idempotent replay must not make
		// consumers think a second jurisdiction appeared.
		h.publish("jurisdiction.created", correlationID, func() error {
			return h.publisher.PublishJurisdictionCreated(r.Context(), *j, correlationID)
		})
		status = http.StatusCreated
	}
	h.log.Info("CreateJurisdiction",
		zap.String("jurisdiction_id", j.JurisdictionID),
		zap.Bool("created", created),
		zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, j)
}

// DeactivateJurisdiction handles POST /v1/admin/jurisdictions/{jurisdiction_id}/deactivate.
//
// Response:
//
//	200 → deactivated
//	401 → no caller identity on the request
//	403 → authorization denied
//	404 → jurisdiction_id not found
//	503 → authz or store unavailable
func (h *Handler) DeactivateJurisdiction(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "deactivate"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	j, err := h.store.DeactivateJurisdiction(r.Context(), jurisdictionID, principalID)
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	h.publish("jurisdiction.deactivated", correlationID, func() error {
		return h.publisher.PublishJurisdictionDeactivated(r.Context(), *j, correlationID)
	})

	h.log.Info("DeactivateJurisdiction",
		zap.String("jurisdiction_id", jurisdictionID),
		zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, j)
}

// CreateRule handles POST /v1/admin/jurisdictions/{jurisdiction_id}/rules.
//
// Response:
//
//	201 → new rule created
//	200 → idempotent replay of an existing rule (same dedup key, same payload/name)
//	400 → malformed or incomplete request body, or a rule_status that cannot be created directly
//	401 → no caller identity on the request
//	403 → authorization denied
//	404 → jurisdiction_id unknown or inactive
//	409 → dedup key matches an existing rule with differing payload/name, or the
//	      effective period overlaps another live rule with the same rule_code
//	503 → authz or store unavailable
func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction_rule", "create"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req CreateRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if field := firstBlank(
		requiredField{"rule_domain", req.RuleDomain},
		requiredField{"rule_code", req.RuleCode},
		requiredField{"rule_name", req.RuleName},
	); field != "" {
		writeMissingField(w, field)
		return
	}
	if req.EffectiveFrom.IsZero() {
		writeMissingField(w, "effective_from")
		return
	}
	if !validEffectivePeriod(req.EffectiveFrom, req.EffectiveTo) {
		writeError(w, http.StatusBadRequest, "invalid_effective_period", "effective_to must be after effective_from")
		return
	}

	// Default to DRAFT rather than to the empty string, which the NOT NULL
	// column happily accepted and which no query ever matches.
	ruleStatus := req.RuleStatus
	if ruleStatus == "" {
		ruleStatus = "DRAFT"
	}
	if !createableRuleStatuses[ruleStatus] {
		writeError(w, http.StatusBadRequest, "invalid_rule_status",
			"rule_status must be DRAFT or ACTIVE at creation; SUPERSEDED and RETIRED are reached only via transition")
		return
	}

	payload, err := normaliseRulePayload(req.RulePayload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rule_payload", err.Error())
		return
	}

	rule, created, err := h.store.CreateRule(r.Context(), domain.CreateRuleParams{
		JurisdictionID:        jurisdictionID,
		RuleDomain:            req.RuleDomain,
		RuleCode:              req.RuleCode,
		RuleName:              req.RuleName,
		EffectiveFrom:         req.EffectiveFrom,
		EffectiveTo:           req.EffectiveTo,
		RulePayload:           payload,
		SourceReference:       req.SourceReference,
		ExternalFeedReference: req.ExternalFeedReference,
		RuleStatus:            ruleStatus,
		CreatedByPrincipalID:  principalID,
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	status := http.StatusOK
	if created {
		h.publish("jurisdiction.rule.updated", correlationID, func() error {
			return h.publisher.PublishRuleUpdated(r.Context(), *rule, correlationID)
		})
		// A rule created directly in ACTIVE never passes through the
		// transition endpoint, so activation is announced here instead.
		if rule.RuleStatus == "ACTIVE" {
			h.publish("jurisdiction.rule.activated", correlationID, func() error {
				return h.publisher.PublishRuleActivated(r.Context(), *rule, correlationID)
			})
		}
		status = http.StatusCreated
	}
	h.log.Info("CreateRule",
		zap.String("jurisdiction_rule_id", rule.JurisdictionRuleID),
		zap.String("jurisdiction_id", jurisdictionID),
		zap.Bool("created", created),
		zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, rule)
}

// TransitionRuleStatus handles POST /v1/admin/rules/{jurisdiction_rule_id}/transition.
//
// Response:
//
//	200 → transitioned (or idempotent no-op if already in the target status)
//	400 → malformed request body, or new_status is not a recognized target state
//	401 → no caller identity on the request
//	403 → authorization denied
//	404 → jurisdiction_rule_id not found
//	409 → current status is not a legal prior state for new_status, or
//	      activating this rule would overlap another live rule with the same code
//	503 → authz or store unavailable
func (h *Handler) TransitionRuleStatus(w http.ResponseWriter, r *http.Request) {
	ruleID := chi.URLParam(r, "jurisdiction_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction_rule", "transition"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req TransitionRuleStatusRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	allowedPriors, known := ruleStatusAllowedPriors[req.NewStatus]
	if !known {
		writeError(w, http.StatusBadRequest, "invalid_status", "new_status must be one of ACTIVE, SUPERSEDED, RETIRED")
		return
	}

	rule, transitioned, err := h.store.TransitionRuleStatus(r.Context(), store.TransitionParams{
		RuleID:        ruleID,
		NewStatus:     req.NewStatus,
		AllowedPriors: allowedPriors,
		EndDate:       endDatingStatuses[req.NewStatus],
		EffectiveTo:   req.EffectiveTo,
		ActorID:       principalID,
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	// A replayed transition must not re-emit — a consumer that saw
	// rule.activated twice would double-apply whatever it does on activation.
	if transitioned {
		h.publish("jurisdiction.rule.updated", correlationID, func() error {
			return h.publisher.PublishRuleUpdated(r.Context(), *rule, correlationID)
		})
		if req.NewStatus == "ACTIVE" {
			h.publish("jurisdiction.rule.activated", correlationID, func() error {
				return h.publisher.PublishRuleActivated(r.Context(), *rule, correlationID)
			})
		}
	}

	h.log.Info("TransitionRuleStatus",
		zap.String("jurisdiction_rule_id", ruleID),
		zap.String("new_status", req.NewStatus),
		zap.Bool("transitioned", transitioned),
		zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, rule)
}

// RecordDrift handles POST /v1/admin/rules/{jurisdiction_rule_id}/drift.
//
// This is the write side of Legal Drift Detection (03-microservices.md §8.2's
// Critical Enhancement): an external regulatory feed, or a human reviewer,
// declares that a stored rule has diverged from applicable legal reality.
// The state change and its justification are recorded together, append-only,
// and legal.drift.detected is published for anything moving away from CURRENT.
//
// Response:
//
//	200 → drift state recorded, or an idempotent no-op if already in that state
//	400 → malformed body, or drift_state outside CURRENT|DRIFTED|UNDER_REVIEW
//	401 → no caller identity on the request
//	403 → authorization denied
//	404 → jurisdiction_rule_id not found
//	503 → authz or store unavailable
func (h *Handler) RecordDrift(w http.ResponseWriter, r *http.Request) {
	ruleID := chi.URLParam(r, "jurisdiction_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction_rule", "record_drift"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req RecordDriftRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !driftStates[req.DriftState] {
		writeError(w, http.StatusBadRequest, "invalid_drift_state", "drift_state must be one of CURRENT, DRIFTED, UNDER_REVIEW")
		return
	}

	rule, event, changed, err := h.store.RecordDrift(r.Context(), domain.RecordDriftParams{
		JurisdictionRuleID:    ruleID,
		ToState:               req.DriftState,
		Reason:                req.Reason,
		RecordedByPrincipalID: principalID,
		CorrelationID:         correlationID,
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	// Only a real divergence is news. Returning to CURRENT is a resolution,
	// carried by the history rather than by a "drift detected" fact.
	if changed && req.DriftState != "CURRENT" {
		h.publish("legal.drift.detected", correlationID, func() error {
			return h.publisher.PublishLegalDriftDetected(r.Context(), *rule, *event, correlationID)
		})
	}

	h.log.Info("RecordDrift",
		zap.String("jurisdiction_rule_id", ruleID),
		zap.String("drift_state", req.DriftState),
		zap.Bool("changed", changed),
		zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"rule":        rule,
		"drift_event": event,
		"changed":     changed,
	})
}

// ── Shared helpers ───────────────────────────────────────────────────────────

// requirePrincipal resolves the acting principal from the gateway-verified
// identity headers, writing 401 and returning false when there is none.
//
// This used to fall back to the literal string "system" when no header was
// present, so every unattributed mutation was written into the audit columns
// as if the platform itself had made it. It also read X-Actor-Principal-ID,
// a header nothing sets — the gateway's ForwardAuth middleware publishes
// X-Principal-Id (see the authResponseHeaders label in docker-compose.yml),
// so in practice the audit trail recorded "system" for every write.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	for _, header := range []string{"X-Principal-Id", "X-Actor-Principal-ID"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			return id, true
		}
	}
	writeError(w, http.StatusUnauthorized, "missing_principal",
		"X-Principal-Id is required — this header is set by the gateway from a verified identity envelope")
	return "", false
}

// checkAuthz asks the AuthorizationClient for a decision. Doctrine: no domain
// service self-authorizes a material action.
func (h *Handler) checkAuthz(r *http.Request, principalID, resource, action string) error {
	return h.authz.Authorize(r.Context(), principalID, h.authzScopeID, resource, action)
}

// writeAuthzError maps an AuthorizationClient error to an HTTP response.
// Both the explicit denial and the unavailable case fail closed — no
// mutation proceeds without a positive authz decision.
func (h *Handler) writeAuthzError(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrUnauthorized) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authz_unavailable"})
}

// writeStoreError maps a store error to an HTTP response.
func (h *Handler) writeStoreError(w http.ResponseWriter, err error, correlationID string) {
	switch {
	case errors.Is(err, domain.ErrJurisdictionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "jurisdiction_not_found"})
	case errors.Is(err, domain.ErrParentNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "parent_jurisdiction_not_found"})
	case errors.Is(err, domain.ErrRuleNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule_not_found"})
	case errors.Is(err, domain.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
	case errors.Is(err, domain.ErrOverlappingRule):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "overlapping_rule"})
	case errors.Is(err, domain.ErrCyclicHierarchy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "cyclic_hierarchy"})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
	case errors.Is(err, domain.ErrInvalidEffectivePeriod):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_effective_period"})
	default:
		h.log.Error("store operation failed",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

// publish runs an event emission, logging rather than failing the request if
// the broker is unreachable. The write is already committed; refusing the
// response would tell the caller nothing happened when something did.
func (h *Handler) publish(eventType, correlationID string, emit func() error) {
	if err := emit(); err != nil {
		h.log.Error("failed to publish event",
			zap.String("event_type", eventType),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
	}
}

// parsePaging reads limit and offset, rejecting anything that is not a
// non-negative integer.
//
// Both used to be parsed with the error discarded, so "limit=abc" silently
// fell back to the default and "offset=-1" reached Postgres, which rejects a
// negative OFFSET — surfacing as 503 store_unavailable, i.e. a client typo
// presenting as an outage.
func (h *Handler) parsePaging(w http.ResponseWriter, q map[string][]string) (limit, offset int, ok bool) {
	limit, ok = parseNonNegativeInt(w, q, "limit")
	if !ok {
		return 0, 0, false
	}
	offset, ok = parseNonNegativeInt(w, q, "offset")
	if !ok {
		return 0, 0, false
	}
	return limit, offset, true
}

func parseNonNegativeInt(w http.ResponseWriter, q map[string][]string, key string) (int, bool) {
	vals, present := q[key]
	if !present || len(vals) == 0 || vals[0] == "" {
		return 0, true
	}
	n, err := strconv.Atoi(vals[0])
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "invalid_"+key, key+" must be a non-negative integer")
		return 0, false
	}
	return n, true
}

// parseEffectiveAt reads the point-in-time parameter. A zero value means
// "now", resolved in the store.
func (h *Handler) parseEffectiveAt(w http.ResponseWriter, q map[string][]string) (time.Time, bool) {
	vals, present := q["effective_at"]
	if !present || len(vals) == 0 || vals[0] == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, vals[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be a valid RFC3339 timestamp")
		return time.Time{}, false
	}
	return t, true
}

// decodeJSON reads a size-limited request body and rejects unknown fields.
//
// Unknown fields are an error rather than being ignored: a caller that
// misspells "rule_payload" would otherwise get a 201 for a rule with an
// empty payload, and only discover it much later.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
				"request body exceeds "+strconv.Itoa(maxRequestBody)+" bytes")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request_body", err.Error())
		return false
	}
	// Reject trailing content — two concatenated JSON objects would otherwise
	// decode as just the first.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request_body", "body must contain exactly one JSON object")
		return false
	}
	return true
}

// normaliseRulePayload validates that the payload is a JSON object and
// defaults an omitted one to {}.
//
// The column is JSONB NOT NULL DEFAULT '{}', but the insert always supplied a
// value: an omitted payload sent empty bytes, which Postgres rejects as
// invalid JSON and which surfaced as 503. A scalar payload ("x", 7, null)
// was accepted outright, even though every documented payload — and every
// consumer that reads applies_to_entity_types or filing_frequency out of it —
// assumes an object.
func normaliseRulePayload(raw json.RawMessage) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return []byte(`{}`), nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil, errors.New("rule_payload must be a JSON object")
	}
	return []byte(trimmed), nil
}

// requiredField pairs a JSON field name with its submitted value.
type requiredField struct {
	name  string
	value string
}

// firstBlank returns the name of the first blank required field, in the order
// given — so the error a caller gets is deterministic and matches the order
// the fields appear in the request contract.
func firstBlank(fields ...requiredField) string {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return f.name
		}
	}
	return ""
}

// validEffectivePeriod reports whether effective_to, when present, is
// strictly after effective_from.
func validEffectivePeriod(from time.Time, to *time.Time) bool {
	return to == nil || to.After(from)
}

func writeMissingField(w http.ResponseWriter, field string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": "missing_field",
		"field": field,
	})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": code}
	if message != "" {
		body["message"] = message
	}
	writeJSON(w, status, body)
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
