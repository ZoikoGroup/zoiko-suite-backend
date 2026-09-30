package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
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
	ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error)

	// ── idempotency keys ──────────────────────────────────────────────────────
	CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error)
	StoreIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string, bodyHash []byte, decisionID string) error

	// ── outbox ────────────────────────────────────────────────────────────────
	EnqueueEvent(ctx context.Context, tenantID string, event store.OutboxEvent) error
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

// ActionReplayDecision is the authorization-svc action_type a caller must
// hold to replay a governance decision.
const ActionReplayDecision = "GOVERNANCE_DECISION_REPLAY"

// ActionReadDecision is the authorization-svc action_type a caller must
// hold to read from the governance ledger.
const ActionReadDecision = "GOVERNANCE_DECISION_READ"

// Handler holds all HTTP handler methods.
type Handler struct {
	store        DecisionStore
	authz        authz.Client
	policyClient policyclient.Client
	log          *zap.Logger

	// authzPlatformScopeID is the legal_entity_id used when a decision is
	// not scoped to one. authorization-svc rejects an empty legal_entity_id.
	authzPlatformScopeID string
}

// New constructs a Handler.
func New(store DecisionStore, authzClient authz.Client, policyClient policyclient.Client, authzPlatformScopeID string, log *zap.Logger) *Handler {
	return &Handler{
		store:                store,
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
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	scope := h.authzPlatformScopeID
	if legalEntityID != "" {
		scope = legalEntityID
	}
	err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType)
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
// ActorID is NOT in the request — it is derived from the authenticated
// X-Principal-Id header to prevent attribution forgery.
// PolicyVersionId, ActionSubjectType, and ActionSubjectId are optional
// fields that can be provided directly or extracted from evaluation_context.
type createDecisionRequest struct {
	DecisionID           string          `json:"decision_id"`
	TenantID             string          `json:"tenant_id"`
	LegalEntityID        string          `json:"legal_entity_id"`
	ActionType           string          `json:"action_type"`
	Outcome              string          `json:"outcome"`
	RuleBasis            string          `json:"rule_basis"`
	PolicyVersionID      *string         `json:"policy_version_id,omitempty"`
	ActionSubjectType    *string         `json:"action_subject_type,omitempty"`
	ActionSubjectID      *string         `json:"action_subject_id,omitempty"`
	EvaluationContext    json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID        string          `json:"correlation_id"`
	// WorkflowInstanceID and CausationID are optional Event Linkage Keys
	// (doctrine §3.3) — omit either when not known.
	WorkflowInstanceID *string    `json:"workflow_instance_id,omitempty"`
	CausationID        *string    `json:"causation_id,omitempty"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
}

// requiredFields lists the fields that must be non-empty. evaluation_context
// and decided_at are the only optional fields. ActorID is derived from
// the authenticated principal.
func (req createDecisionRequest) missingField() string {
	switch {
	case req.DecisionID == "":
		return "decision_id"
	// tenant_id is deliberately NOT required: the tenant is the caller's
	// verified scope, so a body omitting it is fine and a body disagreeing with
	// it is a 403, not a missing field.
	case req.LegalEntityID == "":
		return "legal_entity_id"
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

	// Read the raw body for idempotency key validation
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_idempotency_key",
			"message": "Idempotency-Key is required for material state changes",
		})
		return
	}

	// Get the raw body for hash computation
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error("CreateDecision: failed to read request body", zap.Error(err))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}

	// Re-parse the JSON from the raw body
	var req createDecisionRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "tenant_scope_missing",
			"message": domain.ErrTenantScopeMissing.Error(),
		})
		return
	}
	if req.TenantID != "" && req.TenantID != tenantID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": domain.ErrTenantScopeMismatch.Error(),
		})
		return
	}

	// Check idempotency key
	bodyHash := sha256.Sum256(rawBody)
	existingDecisionID, existingHash, err := h.store.CheckIdempotencyKey(r.Context(), tenantID, idempotencyKey)
	if err != nil {
		h.log.Error("CreateDecision: idempotency check failed", zap.String("idempotency_key", idempotencyKey), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if existingDecisionID != "" {
		// Key exists - verify body hash matches
		if !bytes.Equal(existingHash, bodyHash[:]) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":          "idempotency_mismatch",
				"idempotency_key": idempotencyKey,
				"message":        "Idempotency-Key reused with different request body",
			})
			return
		}
		// Same body - idempotent replay, return existing decision
		existingDecision, err := h.store.FindByID(r.Context(), tenantID, existingDecisionID)
		if err != nil {
			h.log.Error("CreateDecision: failed to fetch existing decision", zap.String("decision_id", existingDecisionID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, existingDecision)
		return
	}

	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	if !h.authorize(w, r, principalID, req.LegalEntityID, ActionRecordDecision) {
		return
	}

	decidedAt := time.Now().UTC()
	if req.DecidedAt != nil {
		decidedAt = req.DecidedAt.UTC()
	}

	// Validate decided_at bounds (not in future beyond clock skew, not before retention floor)
	now := time.Now().UTC()
	if decidedAt.After(now.Add(24 * time.Hour)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_decided_at",
			"message": "decided_at cannot be more than 24 hours in the future",
		})
		return
	}
	if decidedAt.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_decided_at",
			"message": "decided_at cannot be before 2020-01-01",
		})
		return
	}

	d := domain.GovernanceDecision{
		DecisionID:            req.DecisionID,
		TenantID:              tenantID,
		LegalEntityID:         req.LegalEntityID,
		ActorID:               principalID,
		ActionType:            req.ActionType,
		Outcome:               req.Outcome,
		RuleBasis:             req.RuleBasis,
		PolicyVersionID:       req.PolicyVersionID,
		EvaluationContext:     req.EvaluationContext,
		CorrelationID:         req.CorrelationID,
		WorkflowInstanceID:    req.WorkflowInstanceID,
		CausationID:           req.CausationID,
		ActionSubjectType:     req.ActionSubjectType,
		ActionSubjectID:       req.ActionSubjectID,
		DecidedAt:             decidedAt,
	}

	created, err := h.store.Insert(r.Context(), d)
	if errors.Is(err, domain.ErrDecisionIDConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":       "decision_id_conflict",
			"decision_id": d.DecisionID,
			"message":     "decision_id is already in use",
		})
		return
	}
	if err != nil {
		h.log.Error("CreateDecision: store unavailable",
			zap.String("decision_id", d.DecisionID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Store idempotency key with body hash
	if err := h.store.StoreIdempotencyKey(r.Context(), tenantID, idempotencyKey, bodyHash[:], d.DecisionID); err != nil {
		h.log.Error("CreateDecision: failed to store idempotency key", zap.String("idempotency_key", idempotencyKey), zap.Error(err))
		// Don't fail the request - the decision was already stored
	}

	// Enqueue event to outbox for reliable delivery
	if created {
		eventPayload := map[string]any{
			"decision_id":            d.DecisionID,
			"tenant_id":              d.TenantID,
			"legal_entity_id":        d.LegalEntityID,
			"actor_id":               d.ActorID,
			"action_type":            d.ActionType,
			"outcome":                d.Outcome,
			"rule_basis":             d.RuleBasis,
			"policy_version_id":      d.PolicyVersionID,
			"action_subject_type":    d.ActionSubjectType,
			"action_subject_id":      d.ActionSubjectID,
			"jurisdiction_context":   d.RuleBasis,
			"correlation_id":         d.CorrelationID,
			"decided_at":             d.DecidedAt,
		}
		eventPayloadBytes, _ := json.Marshal(eventPayload)
		event := store.OutboxEvent{
			EventType:      "governance.decision.recorded",
			Payload:        eventPayloadBytes,
			TenantID:       d.TenantID,
			LegalEntityID:  d.LegalEntityID,
			ActorID:        d.ActorID,
			CorrelationID:  d.CorrelationID,
			IdempotencyKey: idempotencyKey,
		}
		if err := h.store.EnqueueEvent(r.Context(), tenantID, event); err != nil {
			h.log.Error("CreateDecision: failed to enqueue event", zap.String("decision_id", d.DecisionID), zap.Error(err))
			// Don't fail the request - the decision was already stored, event will be retried
		}
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
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
// Requires X-Tenant-Id and X-Principal-Id. A decision belonging to a different
// tenant is indistinguishable from a nonexistent one — both return 404, never a
// 403, so this endpoint cannot be used to probe for the existence of another
// tenant's decisions. Authorization is checked via GOVERNANCE_DECISION_READ.
//
// Response:
//
//	200 → decision found
//	400 → missing X-Tenant-Id
//	401 → missing X-Principal-Id
//	403 → not authorized to read decisions
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if !h.authorize(w, r, principalID, "", ActionReadDecision) {
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
	CorrelationID string `json:"correlation_id"`
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
//	400 → missing X-Tenant-Id, missing correlation_id, or
//	      the decision's rule_basis is not parseable
//	401 → missing X-Principal-Id
//	403 → not authorized to replay
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req replayDecisionRequest
	if !decodeJSON(w, r, &req) {
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

	if !h.authorize(w, r, principalID, decision.LegalEntityID, ActionReplayDecision) {
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

	// Replayed in the decision's own tenant scope — the same tenant the decision
	// was read under, so a replay cannot reach a version outside it.
	version, err := h.policyClient.GetPolicyVersion(r.Context(), tenantID, policyVersionID)
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
		ReplayedByPrincipalID: principalID,
		TenantID:              tenantID,
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
//
// Requires X-Tenant-Id and X-Principal-Id. Authorization is checked via
// GOVERNANCE_DECISION_READ.
//
// Response:
//
//	200 → JSON array of replay manifests (may be empty), newest first
//	400 → missing X-Tenant-Id
//	401 → missing X-Principal-Id
//	403 → not authorized to read decisions
//	503 → store unavailable
func (h *Handler) ListReplayManifests(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_tenant_id"})
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if !h.authorize(w, r, principalID, "", ActionReadDecision) {
		return
	}

	results, err := h.store.ListReplayManifestsByDecision(r.Context(), tenantID, decisionID)
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
// Requires X-Tenant-Id and X-Principal-Id. Authorization is checked via
// GOVERNANCE_DECISION_READ.
//
// Response:
//
//	200 → JSON array of decisions (may be empty), newest first
//	400 → missing X-Tenant-Id, invalid from/to timestamp
//	401 → missing X-Principal-Id
//	403 → not authorized to read decisions
//	503 → store unavailable
func (h *Handler) ListDecisions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_tenant_id"})
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if !h.authorize(w, r, principalID, "", ActionReadDecision) {
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
			if n < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error":   "invalid_offset",
					"message": "offset must be a non-negative integer",
				})
				return
			}
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

// ProblemDetail represents an RFC 9457 Problem Details object.
// See https://www.rfc-editor.org/rfc/rfc9457.html and GCP §16.
type ProblemDetail struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteProblem writes an RFC 9457 compliant problem+json error response.
// The type field uses the GCP §16 stable class names (e.g., "IDEMPOTENCY_MISMATCH").
func WriteProblem(w http.ResponseWriter, status int, title, detail, instance string, problemType string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	pd := ProblemDetail{
		Type:     problemType,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
	}
	_ = json.NewEncoder(w).Encode(pd)
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return false
	}
	return true
}
