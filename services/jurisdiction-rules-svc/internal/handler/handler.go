package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
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
  
	// CreateJurisdiction inserts a new jurisdiction idempotently.
	CreateJurisdiction(ctx context.Context, params domain.CreateJurisdictionParams) (*domain.Jurisdiction, bool, error)

	// DeactivateJurisdiction sets active_flag=false and updates audit columns.
	DeactivateJurisdiction(ctx context.Context, jurisdictionID, actorID string) (*domain.Jurisdiction, error)

	// FindRuleByID looks up a rule by ID.
	FindRuleByID(ctx context.Context, ruleID string) (*domain.JurisdictionRule, error)

	// CreateRule inserts a new rule idempotently.
	CreateRule(ctx context.Context, params domain.CreateRuleParams) (*domain.JurisdictionRule, bool, error)

	// TransitionRuleStatus atomically updates rule_status if current status is in allowedPriors.
	TransitionRuleStatus(ctx context.Context, ruleID, newStatus string, allowedPriors []string, actorID string) (*domain.JurisdictionRule, error)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store JurisdictionStore
	authz authz.AuthorizationClient
	log   *zap.Logger
}

// New constructs a Handler.
func New(store JurisdictionStore, authzClient authz.AuthorizationClient, log *zap.Logger) *Handler {
	return &Handler{store: store, authz: authzClient, log: log}
}

// ruleStatusAllowedPriors defines the only legal prior rule_status for each
// target status. Nothing else in the codebase or docs/architecture defines
// this state machine, so it is owned here, at the boundary where "transition
// to X" is first expressed as a concept.
var ruleStatusAllowedPriors = map[string][]string{
	"ACTIVE":     {"DRAFT"},
	"SUPERSEDED": {"ACTIVE"},
	"RETIRED":    {"ACTIVE", "SUPERSEDED"},
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

	// ── Admin mutations (AuthZ required on every route) ───────────────────────
	r.Post("/v1/admin/jurisdictions", h.CreateJurisdiction)
	r.Post("/v1/admin/jurisdictions/{jurisdiction_id}/deactivate", h.DeactivateJurisdiction)
	r.Post("/v1/admin/jurisdictions/{jurisdiction_id}/rules", h.CreateRule)
	r.Post("/v1/admin/rules/{jurisdiction_rule_id}/transition", h.TransitionRuleStatus)
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

// ── Admin mutations ──────────────────────────────────────────────────────────

// CreateJurisdiction handles POST /v1/admin/jurisdictions.
//
// Response:
//
//	201 → new jurisdiction created
//	200 → idempotent replay of an existing jurisdiction (same dedup key, same attributes)
//	400 → malformed request body
//	403 → authorization denied
//	409 → dedup key matches an existing jurisdiction with differing attributes
//	503 → authz or store unavailable
func (h *Handler) CreateJurisdiction(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	if err := h.checkAuthz(r, "jurisdiction", "create"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req CreateJurisdictionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
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
		CreatedByPrincipalID: actorIDFromRequest(r),
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.log.Info("CreateJurisdiction",
		zap.String("jurisdiction_id", j.JurisdictionID),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, j)
}

// DeactivateJurisdiction handles POST /v1/admin/jurisdictions/{jurisdiction_id}/deactivate.
//
// Response:
//
//	200 → deactivated
//	403 → authorization denied
//	404 → jurisdiction_id not found
//	503 → authz or store unavailable
func (h *Handler) DeactivateJurisdiction(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if err := h.checkAuthz(r, "jurisdiction", "deactivate"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	j, err := h.store.DeactivateJurisdiction(r.Context(), jurisdictionID, actorIDFromRequest(r))
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	h.log.Info("DeactivateJurisdiction",
		zap.String("jurisdiction_id", jurisdictionID),
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
//	400 → malformed request body
//	403 → authorization denied
//	409 → dedup key matches an existing rule with differing payload/name
//	503 → authz or store unavailable
func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	jurisdictionID := chi.URLParam(r, "jurisdiction_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if err := h.checkAuthz(r, "jurisdiction_rule", "create"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req CreateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}

	rule, created, err := h.store.CreateRule(r.Context(), domain.CreateRuleParams{
		JurisdictionID:        jurisdictionID,
		RuleDomain:            req.RuleDomain,
		RuleCode:              req.RuleCode,
		RuleName:              req.RuleName,
		EffectiveFrom:         req.EffectiveFrom,
		EffectiveTo:           req.EffectiveTo,
		RulePayload:           req.RulePayload,
		SourceReference:       req.SourceReference,
		ExternalFeedReference: req.ExternalFeedReference,
		RuleStatus:            req.RuleStatus,
		CreatedByPrincipalID:  actorIDFromRequest(r),
	})
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.log.Info("CreateRule",
		zap.String("jurisdiction_rule_id", rule.JurisdictionRuleID),
		zap.String("jurisdiction_id", jurisdictionID),
		zap.Bool("created", created),
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
//	403 → authorization denied
//	404 → jurisdiction_rule_id not found
//	409 → current status is not a legal prior state for new_status
//	503 → authz or store unavailable
func (h *Handler) TransitionRuleStatus(w http.ResponseWriter, r *http.Request) {
	ruleID := chi.URLParam(r, "jurisdiction_rule_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if err := h.checkAuthz(r, "jurisdiction_rule", "transition"); err != nil {
		h.writeAuthzError(w, err)
		return
	}

	var req TransitionRuleStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request_body"})
		return
	}

	allowedPriors, ok := ruleStatusAllowedPriors[req.NewStatus]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}

	rule, err := h.store.TransitionRuleStatus(r.Context(), ruleID, req.NewStatus, allowedPriors, actorIDFromRequest(r))
	if err != nil {
		h.writeStoreError(w, err, correlationID)
		return
	}

	h.log.Info("TransitionRuleStatus",
		zap.String("jurisdiction_rule_id", ruleID),
		zap.String("new_status", req.NewStatus),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, rule)
}

// ── Shared admin helpers ─────────────────────────────────────────────────────

// actorIDFromRequest extracts the acting principal from the trusted
// X-Actor-Principal-ID header, falling back to "system" if absent — the same
// convention used in identity-context-svc and tenant-entity-registry-svc.
func actorIDFromRequest(r *http.Request) string {
	if id := r.Header.Get("X-Actor-Principal-ID"); id != "" {
		return id
	}
	return "system"
}

// checkAuthz extracts the caller's identity envelope from the Authorization
// header and asks the AuthorizationClient for a decision. Doctrine: no domain
// service self-authorizes a material action.
func (h *Handler) checkAuthz(r *http.Request, resource, action string) error {
	envelopeJWT := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return h.authz.Authorize(r.Context(), envelopeJWT, resource, action)
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

// writeStoreError maps a store error to an HTTP response, extending the same
// pattern already used by the read handlers above with the two admin-only
// error types.
func (h *Handler) writeStoreError(w http.ResponseWriter, err error, correlationID string) {
	switch {
	case errors.Is(err, domain.ErrJurisdictionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "jurisdiction_not_found"})
	case errors.Is(err, domain.ErrRuleNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "rule_not_found"})
	case errors.Is(err, domain.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
	default:
		h.log.Error("admin store operation failed",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
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