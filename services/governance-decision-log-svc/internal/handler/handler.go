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
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/authz"
	"zoiko.io/governance-decision-log-svc/internal/domain"
	svcmiddleware "zoiko.io/governance-decision-log-svc/internal/middleware"
	"zoiko.io/governance-decision-log-svc/internal/policyclient"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// DecisionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type DecisionStore interface {
	Insert(ctx context.Context, d domain.GovernanceDecision) (created bool, err error)
	FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error)
	List(ctx context.Context, params store.ListParams) ([]*domain.GovernanceDecision, error)

	// ── replay manifests (backlog item 34) ──────────────────────────────────
	CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest) error
	ListReplayManifestsByDecision(ctx context.Context, decisionID string) ([]*domain.ReplayManifest, error)
}

// EventPublisher is the narrow interface the handler depends on for
// publishing governance.decision.recorded. Allows the handler to be tested
// without a real event backbone.
type EventPublisher interface {
	PublishDecisionRecorded(ctx context.Context, d domain.GovernanceDecision) error
}

// ActionRecordDecision is the authorization-svc action_type a caller must
// hold to append to the governance ledger.
const ActionRecordDecision = "GOVERNANCE_DECISION_RECORD"

// Handler holds all HTTP handler methods.
type Handler struct {
	store        DecisionStore
	publisher    EventPublisher
	authz        authz.Client
	policyClient policyclient.Client
	log          *zap.Logger

	// authzPlatformScopeID is the legal_entity_id used when a decision is
	// not scoped to one. authorization-svc rejects an empty legal_entity_id.
	authzPlatformScopeID string
}

// New constructs a Handler.
func New(store DecisionStore, publisher EventPublisher, authzClient authz.Client, policyClient policyclient.Client, authzPlatformScopeID string, log *zap.Logger) *Handler {
	return &Handler{
		store:                store,
		publisher:            publisher,
		authz:                authzClient,
		policyClient:         policyClient,
		authzPlatformScopeID: authzPlatformScopeID,
		log:                  log,
	}
}

// requirePrincipal resolves the calling principal from X-Principal-Id.
//
// This ledger is append-only evidence. Without an authenticated caller,
// anything able to reach the port could forge a governance decision — which
// defeats the point of keeping the ledger. Service callers (policy-svc)
// forward the acting principal in the same header.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(r.Header.Get("X-Principal-Id")); id != "" {
		return id, true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error":   "missing_principal",
		"message": "X-Principal-Id is required to append to the governance ledger",
	})
	return "", false
}

// authorize fails closed on both a denial and an unobtainable decision.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID string) bool {
	scope := h.authzPlatformScopeID
	if legalEntityID != "" {
		scope = legalEntityID
	}
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, ActionRecordDecision)
	switch {
	case err == nil:
		return true
	case errors.Is(err, authz.ErrDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied"})
	default:
		h.log.Error("authorization check failed — refusing the write",
			zap.String("principal_id", principalID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authz_unavailable"})
	}
	return false
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path — this makes the
// behaviour testable in unit tests that build their own router via this
// function (same convention as jurisdiction-rules-svc).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Post("/v1/decisions", h.CreateDecision)
	r.Get("/v1/decisions", h.ListDecisions)
	r.Get("/v1/decisions/{decision_id}", h.GetDecision)
	r.Post("/v1/decisions/{decision_id}/replay", h.ReplayDecision)
	r.Get("/v1/decisions/{decision_id}/replay-manifests", h.ListReplayManifests)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// createDecisionRequest is the wire shape for POST /v1/decisions.
// DecidedAt is optional — if omitted, it defaults to server-receipt time
// (see CONTEXT.md: DecidedAt represents when the decision happened
// upstream, not when it was logged here, but callers may not always have
// a distinct timestamp to send).
type createDecisionRequest struct {
	DecisionID        string          `json:"decision_id"`
	TenantID          string          `json:"tenant_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	ActorID           string          `json:"actor_id"`
	ActionType        string          `json:"action_type"`
	Outcome           string          `json:"outcome"`
	RuleBasis         string          `json:"rule_basis"`
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID     string          `json:"correlation_id"`
	// WorkflowInstanceID and CausationID are optional Event Linkage Keys
	// (doctrine §3.3) — omit either when not known.
	WorkflowInstanceID *string    `json:"workflow_instance_id,omitempty"`
	CausationID        *string    `json:"causation_id,omitempty"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
}

// requiredFields lists the fields that must be non-empty. evaluation_context
// and decided_at are the only optional fields.
func (req createDecisionRequest) missingField() string {
	switch {
	case req.DecisionID == "":
		return "decision_id"
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.ActorID == "":
		return "actor_id"
	case req.ActionType == "":
		return "action_type"
	case req.Outcome == "":
		return "outcome"
	case req.RuleBasis == "":
		return "rule_basis"
	case req.CorrelationID == "":
		return "correlation_id"
	default:
		return ""
	}
}

// CreateDecision handles POST /v1/decisions.
//
// Idempotent on decision_id: a repeat POST with the same decision_id
// returns 200 (already recorded) instead of creating a duplicate row.
// A first-time POST returns 201.
//
// Response:
//
//	201 → decision recorded for the first time
//	200 → decision_id already existed; no-op, not an error
//	400 → missing required field
//	503 → store unavailable
func (h *Handler) CreateDecision(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req createDecisionRequest
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

	if !h.authorize(w, r, principalID, req.LegalEntityID) {
		return
	}

	decidedAt := time.Now().UTC()
	if req.DecidedAt != nil {
		decidedAt = req.DecidedAt.UTC()
	}

	d := domain.GovernanceDecision{
		DecisionID:         req.DecisionID,
		TenantID:           req.TenantID,
		LegalEntityID:      req.LegalEntityID,
		ActorID:            req.ActorID,
		ActionType:         req.ActionType,
		Outcome:            req.Outcome,
		RuleBasis:          req.RuleBasis,
		EvaluationContext:  req.EvaluationContext,
		CorrelationID:      req.CorrelationID,
		WorkflowInstanceID: req.WorkflowInstanceID,
		CausationID:        req.CausationID,
		DecidedAt:          decidedAt,
	}

	created, err := h.store.Insert(r.Context(), d)
	if err != nil {
		h.log.Error("CreateDecision: store unavailable",
			zap.String("decision_id", d.DecisionID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		// Only the first insert is a new fact — a replayed idempotent POST
		// must not re-emit governance.decision.recorded. Publish failures
		// are logged, not surfaced to the caller: the write already
		// succeeded and event delivery is a stubbed, non-blocking concern
		// (see events.Publisher doc comment).
		if pubErr := h.publisher.PublishDecisionRecorded(r.Context(), d); pubErr != nil {
			h.log.Error("CreateDecision: failed to publish governance.decision.recorded",
				zap.String("decision_id", d.DecisionID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("governance decision recorded",
		zap.String("decision_id", d.DecisionID),
		zap.String("tenant_id", d.TenantID),
		zap.String("outcome", d.Outcome),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, d)
}

// GetDecision handles GET /v1/decisions/{decision_id}.
//
// Requires X-Tenant-Id. A decision belonging to a different tenant is
// indistinguishable from a nonexistent one — both return 404, never a 403,
// so this endpoint cannot be used to probe for the existence of another
// tenant's decisions.
//
// Response:
//
//	200 → decision found
//	400 → missing X-Tenant-Id
//	404 → no decision with this decision_id for this tenant
//	503 → store unavailable
func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_tenant_id"})
		return
	}

	d, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDecisionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":       "decision_not_found",
				"decision_id": decisionID,
			})
		default:
			h.log.Error("GetDecision: store unavailable",
				zap.String("decision_id", decisionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── POST /v1/decisions/{decision_id}/replay ─────────────────────────────────

type replayDecisionRequest struct {
	ReplayedByPrincipalID string `json:"replayed_by_principal_id"`
	CorrelationID         string `json:"correlation_id"`
}

// ReplayDecision handles POST /v1/decisions/{decision_id}/replay — doc7's
// reproducibility requirement (backlog item 34). Re-fetches the EXACT
// policy version the original decision used (via policyClient, never
// "whatever is active now"), re-runs the same evaluation logic against
// the SAME evaluation_context that was recorded, and records a permanent
// replay_manifest stating whether the outcome reproduced.
//
// Scoped narrowly for v1, same as policy-svc's own Evaluate: only
// action_type=APPROVAL_THRESHOLD has replay logic implemented.
//
// Response:
//
//	201 → replay performed, manifest recorded
//	400 → missing X-Tenant-Id, missing replayed_by_principal_id, or
//	      the decision's rule_basis is not parseable
//	404 → decision not found, or its policy version no longer resolvable
//	501 → replay not implemented for this decision's action_type
//	503 → store, policy-svc, or authz unavailable
func (h *Handler) ReplayDecision(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_tenant_id"})
		return
	}

	var req replayDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.ReplayedByPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "replayed_by_principal_id"})
		return
	}

	decision, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "decision_not_found", "decision_id": decisionID})
			return
		}
		h.log.Error("ReplayDecision: fetch decision failed", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if decision.ActionType != "APPROVAL_THRESHOLD" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":       "replay_not_implemented",
			"action_type": decision.ActionType,
		})
		return
	}

	_, policyVersionID, ok := policyclient.ParseRuleBasis(decision.RuleBasis)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":      "unparseable_rule_basis",
			"rule_basis": decision.RuleBasis,
		})
		return
	}

	version, err := h.policyClient.GetPolicyVersion(r.Context(), policyVersionID)
	if err != nil {
		if errors.Is(err, policyclient.ErrPolicyVersionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":             "policy_version_not_found",
				"policy_version_id": policyVersionID,
			})
			return
		}
		h.log.Error("ReplayDecision: policy-svc unavailable", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "policy_service_unavailable"})
		return
	}

	replayedOutcome, err := replayApprovalThreshold(version.RulePayload, decision.EvaluationContext)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_replay_input", "message": err.Error()})
		return
	}

	manifest := &domain.ReplayManifest{
		ReplayManifestID:      uuid.NewString(),
		DecisionID:            decisionID,
		PolicyVersionID:       policyVersionID,
		ReplayedOutcome:       replayedOutcome,
		OriginalOutcome:       decision.Outcome,
		OutcomesMatch:         replayedOutcome == decision.Outcome,
		ReplayedAt:            time.Now().UTC(),
		ReplayedByPrincipalID: req.ReplayedByPrincipalID,
	}
	if err := h.store.CreateReplayManifest(r.Context(), manifest); err != nil {
		h.log.Error("ReplayDecision: failed to record manifest", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	h.log.Info("decision replayed",
		zap.String("decision_id", decisionID),
		zap.String("policy_version_id", policyVersionID),
		zap.Bool("outcomes_match", manifest.OutcomesMatch),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, manifest)
}

// replayApprovalThreshold mirrors policy-svc's evaluateApprovalThreshold
// comparison exactly, including its canonicalOutcome mapping
// (WITHIN_THRESHOLD -> GRANTED, APPROVAL_REQUIRED -> ESCALATED) — a
// replay must reproduce the SAME canonical vocabulary the original
// decision was recorded with, or "outcomes_match" would compare
// incompatible representations.
func replayApprovalThreshold(rulePayload, evaluationContext json.RawMessage) (string, error) {
	var rule struct {
		ThresholdAmount *float64 `json:"threshold_amount"`
	}
	if err := json.Unmarshal(rulePayload, &rule); err != nil || rule.ThresholdAmount == nil {
		return "", errors.New("policy version has invalid/missing threshold_amount")
	}

	var action struct {
		Amount *float64 `json:"amount"`
	}
	if err := json.Unmarshal(evaluationContext, &action); err != nil || action.Amount == nil {
		return "", errors.New("decision's evaluation_context is missing amount")
	}

	if *action.Amount > *rule.ThresholdAmount {
		return "ESCALATED", nil
	}
	return "GRANTED", nil
}

// ListReplayManifests handles GET /v1/decisions/{decision_id}/replay-manifests
// — the full replay history for one decision, newest first.
func (h *Handler) ListReplayManifests(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	results, err := h.store.ListReplayManifestsByDecision(r.Context(), decisionID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if results == nil {
		results = []*domain.ReplayManifest{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ListDecisions handles GET /v1/decisions.
//
// Query parameters (all optional, compose with AND semantics):
//
//	actor=actor-1                filter by actor_id
//	entity=entity-1              filter by legal_entity_id
//	action=PAYROLL_RELEASE       filter by action_type
//	rule_basis=policy-v3-sod     filter by rule_basis
//	from=2024-01-01T00:00:00Z    decided_at lower bound (RFC3339, inclusive)
//	to=2024-12-31T23:59:59Z      decided_at upper bound (RFC3339, inclusive)
//	limit=50                     page size (max 200, default 50)
//	offset=0                     zero-based page offset
//
// Response:
//
//	200 → JSON array of decisions (may be empty), newest first
//	400 → invalid from/to timestamp
//	503 → store unavailable
func (h *Handler) ListDecisions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_tenant_id"})
		return
	}
	q := r.URL.Query()

	params := store.ListParams{
		TenantID:      tenantID,
		ActorID:       q.Get("actor"),
		LegalEntityID: q.Get("entity"),
		ActionType:    q.Get("action"),
		RuleBasis:     q.Get("rule_basis"),
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_from",
				"message": "from must be a valid RFC3339 timestamp",
			})
			return
		}
		params.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_to",
				"message": "to must be a valid RFC3339 timestamp",
			})
			return
		}
		params.To = t
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
		h.log.Error("ListDecisions: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.GovernanceDecision{}
	}
	writeJSON(w, http.StatusOK, results)
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
