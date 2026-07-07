package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/decisionlog"
	"zoiko.io/policy-svc/internal/domain"
)

// PolicyStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type PolicyStore interface {
	CreatePolicy(ctx context.Context, params domain.CreatePolicyParams) (*domain.Policy, bool, error)
	CreatePolicyVersion(ctx context.Context, params domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error)
	FindPolicyVersionByID(ctx context.Context, policyVersionID string) (*domain.PolicyVersion, error)
	ActivateVersion(ctx context.Context, policyVersionID, actorID string) (*domain.PolicyVersion, []*domain.PolicyVersion, bool, error)
	ListVersionHistory(ctx context.Context, policyID string) ([]*domain.PolicyVersion, error)
	FindApplicableVersions(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error)
}

// EventPublisher is the narrow interface the handler depends on for
// publishing domain events. Allows the handler to be tested without a
// real event backbone. Mirrors governance-decision-log-svc's pattern.
type EventPublisher interface {
	PublishPolicyCreated(ctx context.Context, policy domain.Policy, correlationID string) error
	PublishPolicyUpdated(ctx context.Context, version domain.PolicyVersion, correlationID string) error
	PublishVersionActivated(ctx context.Context, version domain.PolicyVersion, correlationID string) error
	PublishRuleRetired(ctx context.Context, version domain.PolicyVersion, correlationID string) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store       PolicyStore
	publisher   EventPublisher
	decisionLog decisionlog.Client
	log         *zap.Logger
}

// New constructs a Handler.
func New(store PolicyStore, publisher EventPublisher, decisionLog decisionlog.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, decisionLog: decisionLog, log: log}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path — this makes the behaviour
// testable in unit tests that build their own router via this function
// (same convention as jurisdiction-rules-svc and governance-decision-log-svc).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/policies", h.CreatePolicy)
	r.Get("/v1/policies", h.ListApplicablePolicyVersions)
	r.Post("/v1/policies/evaluate", h.Evaluate)
	r.Post("/v1/policies/{policy_id}/versions", h.CreatePolicyVersion)
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/activate", h.ActivateVersion)
	r.Get("/v1/policies/{policy_id}/versions", h.ListVersionHistory)
}

// correlationIDMiddleware echoes X-Correlation-ID from the request into the
// response on every route registered via RegisterRoutes. Injection of a
// fresh ID when absent is handled by server-level middleware in main.go.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/policies ────────────────────────────────────────────────────────

// createPolicyRequest is the wire shape for POST /v1/policies.
// PolicyID is optional — callers may supply their own idempotency key,
// mirroring CreateJurisdictionParams in jurisdiction-rules-svc.
type createPolicyRequest struct {
	PolicyID             string `json:"policy_id,omitempty"`
	PolicyCode           string `json:"policy_code"`
	PolicyName           string `json:"policy_name"`
	PolicyType           string `json:"policy_type"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
}

func (req createPolicyRequest) missingField() string {
	switch {
	case req.PolicyCode == "":
		return "policy_code"
	case req.PolicyName == "":
		return "policy_name"
	case req.PolicyType == "":
		return "policy_type"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreatePolicy handles POST /v1/policies.
//
// Idempotent on policy_code: a repeat POST with the same policy_code and
// identical policy_name/policy_type returns 200 (already exists) instead of
// creating a duplicate row. A first-time POST returns 201. A repeat POST
// with the same policy_code but a DIFFERENT policy_name/policy_type returns
// 409, per doctrine's idempotency requirement (safe retry, not silent
// overwrite of a differing definition).
//
// Response:
//
//	201 → policy created for the first time
//	200 → policy_code already existed with identical attributes; no-op
//	400 → missing required field
//	409 → policy_code already exists with differing attributes
//	503 → store unavailable
func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createPolicyRequest
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

	params := domain.CreatePolicyParams{
		PolicyID:             req.PolicyID,
		PolicyCode:           req.PolicyCode,
		PolicyName:           req.PolicyName,
		PolicyType:           req.PolicyType,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
	}

	p, created, err := h.store.CreatePolicy(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":       "policy_conflict",
				"policy_code": req.PolicyCode,
			})
		default:
			h.log.Error("CreatePolicy: store unavailable",
				zap.String("policy_code", req.PolicyCode),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		// Only the first insert is a new fact — a replayed idempotent POST
		// must not re-emit policy.created. Publish failures are logged,
		// not surfaced to the caller: the write already succeeded and
		// event delivery is a stubbed, non-blocking concern.
		if pubErr := h.publisher.PublishPolicyCreated(r.Context(), *p, correlationID); pubErr != nil {
			h.log.Error("CreatePolicy: failed to publish policy.created",
				zap.String("policy_id", p.PolicyID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("policy created",
		zap.String("policy_id", p.PolicyID),
		zap.String("policy_code", p.PolicyCode),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, p)
}

// ── POST /v1/policies/{policy_id}/versions ──────────────────────────────────

// createPolicyVersionRequest is the wire shape for
// POST /v1/policies/{policy_id}/versions. PolicyVersionID is optional —
// callers may supply their own idempotency key.
type createPolicyVersionRequest struct {
	PolicyVersionID      string          `json:"policy_version_id,omitempty"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	LegalEntityID        *string         `json:"legal_entity_id,omitempty"`
	RulePayload          json.RawMessage `json:"rule_payload,omitempty"`
	EffectiveFrom        time.Time       `json:"effective_from"`
	EffectiveTo          *time.Time      `json:"effective_to,omitempty"`
	CreatedByPrincipalID string          `json:"created_by_principal_id"`
}

func (req createPolicyVersionRequest) missingField() string {
	switch {
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreatePolicyVersion handles POST /v1/policies/{policy_id}/versions.
// New versions are always created in DRAFT status.
//
// Response:
//
//	201 → version created for the first time
//	200 → identical version already existed; no-op
//	400 → missing required field
//	404 → policy_id not found
//	409 → dedup key matched but rule_payload differs
//	503 → store unavailable
func (h *Handler) CreatePolicyVersion(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createPolicyVersionRequest
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

	params := domain.CreatePolicyVersionParams{
		PolicyVersionID:      req.PolicyVersionID,
		PolicyID:             policyID,
		TenantID:             req.TenantID,
		LegalEntityID:        req.LegalEntityID,
		RulePayload:          []byte(req.RulePayload),
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
	}

	v, created, err := h.store.CreatePolicyVersion(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":     "policy_not_found",
				"policy_id": policyID,
			})
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":     "policy_version_conflict",
				"policy_id": policyID,
			})
		default:
			h.log.Error("CreatePolicyVersion: store unavailable",
				zap.String("policy_id", policyID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if pubErr := h.publisher.PublishPolicyUpdated(r.Context(), *v, correlationID); pubErr != nil {
			h.log.Error("CreatePolicyVersion: failed to publish policy.updated",
				zap.String("policy_version_id", v.PolicyVersionID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("policy version created",
		zap.String("policy_id", policyID),
		zap.String("policy_version_id", v.PolicyVersionID),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, v)
}

// ── POST /v1/policies/{policy_id}/versions/{version_id}/activate ───────────

// activateVersionRequest is the wire shape for the activate endpoint.
type activateVersionRequest struct {
	ActivatedByPrincipalID string `json:"activated_by_principal_id"`
}

// ActivateVersion handles
// POST /v1/policies/{policy_id}/versions/{version_id}/activate.
//
// Transitions the target version DRAFT -> ACTIVE, atomically superseding
// whatever version was previously ACTIVE in the same (policy_id, tenant_id,
// legal_entity_id) scope. Idempotent: activating an already-ACTIVE version
// returns 200 with the unchanged record, not an error.
//
// Response:
//
//	200 → activated (or already active — idempotent no-op)
//	400 → missing activated_by_principal_id
//	404 → policy_id/version_id not found, or version_id does not belong to policy_id
//	409 → version is not in DRAFT (or already ACTIVE) — illegal transition
//	503 → store unavailable
func (h *Handler) ActivateVersion(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	versionID := chi.URLParam(r, "version_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req activateVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if req.ActivatedByPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "activated_by_principal_id",
		})
		return
	}

	// Validate the version belongs to the policy in the path before mutating
	// anything — an activate call must never succeed against the wrong
	// policy_id/version_id pairing just because version_id alone resolves.
	existing, err := h.store.FindPolicyVersionByID(r.Context(), versionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyVersionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":             "policy_version_not_found",
				"policy_version_id": versionID,
			})
		default:
			h.log.Error("ActivateVersion: lookup failed",
				zap.String("policy_version_id", versionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if existing.PolicyID != policyID {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":             "policy_version_not_found",
			"policy_id":         policyID,
			"policy_version_id": versionID,
		})
		return
	}

	activated, superseded, transitioned, err := h.store.ActivateVersion(r.Context(), versionID, req.ActivatedByPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyVersionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":             "policy_version_not_found",
				"policy_version_id": versionID,
			})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":             "invalid_transition",
				"policy_version_id": versionID,
			})
		default:
			h.log.Error("ActivateVersion: store unavailable",
				zap.String("policy_version_id", versionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Only a real transition is a new fact — an idempotent retry
	// (transitioned=false, the version was already ACTIVE) must not
	// re-emit policy.version.activated or policy.rule.retired.
	if transitioned {
		if pubErr := h.publisher.PublishVersionActivated(r.Context(), *activated, correlationID); pubErr != nil {
			h.log.Error("ActivateVersion: failed to publish policy.version.activated",
				zap.String("policy_version_id", activated.PolicyVersionID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
		for _, s := range superseded {
			if pubErr := h.publisher.PublishRuleRetired(r.Context(), *s, correlationID); pubErr != nil {
				h.log.Error("ActivateVersion: failed to publish policy.rule.retired",
					zap.String("policy_version_id", s.PolicyVersionID),
					zap.String("correlation_id", correlationID),
					zap.Error(pubErr),
				)
			}
		}
	}

	h.log.Info("policy version activated",
		zap.String("policy_id", policyID),
		zap.String("policy_version_id", versionID),
		zap.Bool("transitioned", transitioned),
		zap.Int("superseded_count", len(superseded)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, activated)
}

// ── GET /v1/policies/{policy_id}/versions ───────────────────────────────────

// ListVersionHistory handles GET /v1/policies/{policy_id}/versions.
// Returns the full version history for a policy, newest first
// (by effective_from, then created_at, descending).
//
// Response:
//
//	200 → JSON array of PolicyVersion objects (may be empty)
//	404 → policy_id not found
//	503 → store unavailable
func (h *Handler) ListVersionHistory(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	results, err := h.store.ListVersionHistory(r.Context(), policyID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":     "policy_not_found",
				"policy_id": policyID,
			})
		default:
			h.log.Error("ListVersionHistory: store unavailable",
				zap.String("policy_id", policyID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.PolicyVersion{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── GET /v1/policies (applicable policy set) ────────────────────────────────

// ListApplicablePolicyVersions handles GET /v1/policies. This is the
// "get applicable policy set" API from 03-microservices.md §8.1.
//
// Query parameters:
//
//	policy_type=X          required — e.g. APPROVAL_THRESHOLD
//	tenant_id=Y             optional — omit for global-only scope
//	legal_entity_id=Z       optional
//
// Returns every currently-ACTIVE version of policy_type whose scope is
// compatible with the given tenant_id/legal_entity_id, most-specific
// scope first (see PgStore.FindApplicableVersions).
//
// Response:
//
//	200 → JSON array of ApplicablePolicyVersion (may be empty)
//	400 → missing policy_type
//	503 → store unavailable
func (h *Handler) ListApplicablePolicyVersions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	policyType := q.Get("policy_type")
	if policyType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "policy_type",
		})
		return
	}

	var tenantID, legalEntityID *string
	if v := q.Get("tenant_id"); v != "" {
		tenantID = &v
	}
	if v := q.Get("legal_entity_id"); v != "" {
		legalEntityID = &v
	}

	results, err := h.store.FindApplicableVersions(r.Context(), policyType, tenantID, legalEntityID)
	if err != nil {
		h.log.Error("ListApplicablePolicyVersions: store unavailable",
			zap.String("policy_type", policyType),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.ApplicablePolicyVersion{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── POST /v1/policies/evaluate ───────────────────────────────────────────────

// evaluateRequest is the wire shape for POST /v1/policies/evaluate.
//
// EvaluatedByPrincipalID is required — not part of the original spec's
// evaluate contract, but required by governance-decision-log-svc's
// POST /v1/decisions (actor_id is a required field there), which Evaluate
// now calls to record every evaluation as evidence (see internal/decisionlog).
// Without an actor, the evidence obligation this endpoint must satisfy
// ("preserve evaluation basis for governed decisions", §8.1) cannot be met.
//
// DecisionID is optional — callers who need exactly-once evidence should
// supply their own idempotency key; if omitted, a fresh UUID is generated
// per call, meaning a client-side retry could record a duplicate decision
// (see internal/decisionlog's doc comment).
type evaluateRequest struct {
	PolicyType             string          `json:"policy_type"`
	TenantID               *string         `json:"tenant_id,omitempty"`
	LegalEntityID          *string         `json:"legal_entity_id,omitempty"`
	ActionContext          json.RawMessage `json:"action_context,omitempty"`
	EvaluatedByPrincipalID string          `json:"evaluated_by_principal_id"`
	DecisionID             string          `json:"decision_id,omitempty"`
}

// evaluateResponse is the wire shape returned by a successful evaluation.
// Deliberately close to what governance-decision-log-svc's
// POST /v1/decisions expects — RuleBasis especially — because Evaluate
// forwards this same data there (see internal/decisionlog).
type evaluateResponse struct {
	Result          string `json:"result"`
	PolicyVersionID string `json:"policy_version_id"`
	RuleBasis       string `json:"rule_basis"`
}

// Evaluate handles POST /v1/policies/evaluate — the "evaluate policy
// against action context" API from 03-microservices.md §8.1.
//
// Scoped narrowly for v1: only policy_type=APPROVAL_THRESHOLD has real
// evaluation logic. Adding the next type is a new case in the switch
// below, not a restructure — see PROGRESS.md.
//
// This endpoint's own result is a pure read/compute with no side effects,
// so idempotency (required by the spec) falls out naturally — but see
// evaluateApprovalThreshold's decision-log recording, which is best-effort
// and not itself guaranteed idempotent unless the caller supplies DecisionID.
//
// Response:
//
//	200 → {"result": "APPROVAL_REQUIRED"|"WITHIN_THRESHOLD", "policy_version_id": "...", "rule_basis": "..."}
//	400 → missing policy_type/evaluated_by_principal_id, or (for APPROVAL_THRESHOLD) missing/invalid action_context.amount
//	404 → no applicable ACTIVE policy for that type+scope — the caller
//	      decides fail-open/fail-closed, this service does not guess
//	501 → policy_type has no evaluation logic implemented yet
//	503 → store unavailable
func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req evaluateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if req.PolicyType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "policy_type",
		})
		return
	}
	if req.EvaluatedByPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "evaluated_by_principal_id",
		})
		return
	}

	matches, err := h.store.FindApplicableVersions(r.Context(), req.PolicyType, req.TenantID, req.LegalEntityID)
	if err != nil {
		h.log.Error("Evaluate: store unavailable",
			zap.String("policy_type", req.PolicyType),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if len(matches) == 0 {
		// No applicable policy for this type+scope. This service does not
		// guess fail-open/fail-closed — that decision belongs to the caller.
		// Nothing was evaluated, so nothing is recorded as a decision.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":       "no_applicable_policy",
			"policy_type": req.PolicyType,
		})
		return
	}
	applicable := matches[0] // most specific scope match — see FindApplicableVersions ordering

	switch req.PolicyType {
	case "APPROVAL_THRESHOLD":
		h.evaluateApprovalThreshold(w, r, req, applicable, correlationID)
	default:
		// No evaluation logic exists for this type, so nothing to record.
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":       "policy_type_not_implemented",
			"policy_type": req.PolicyType,
		})
	}
}

// evaluateApprovalThreshold implements the APPROVAL_THRESHOLD evaluation
// rule: compare action_context.amount against the matched version's
// rule_payload.threshold_amount. amount > threshold => APPROVAL_REQUIRED;
// amount <= threshold (including exactly equal) => WITHIN_THRESHOLD.
//
// After a successful evaluation, records the decision in
// governance-decision-log-svc (best-effort — see internal/decisionlog's
// doc comment on HTTPClient) before responding to the caller.
func (h *Handler) evaluateApprovalThreshold(w http.ResponseWriter, r *http.Request, req evaluateRequest, applicable *domain.ApplicablePolicyVersion, correlationID string) {
	var rule struct {
		ThresholdAmount *float64 `json:"threshold_amount"`
	}
	if err := json.Unmarshal(applicable.RulePayload, &rule); err != nil || rule.ThresholdAmount == nil {
		h.log.Error("evaluateApprovalThreshold: policy version has invalid/missing threshold_amount",
			zap.String("policy_version_id", applicable.PolicyVersionID),
			zap.String("correlation_id", correlationID),
		)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid_policy_payload"})
		return
	}

	var action struct {
		Amount *float64 `json:"amount"`
	}
	if err := json.Unmarshal(req.ActionContext, &action); err != nil || action.Amount == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "action_context.amount",
		})
		return
	}

	result := "WITHIN_THRESHOLD"
	if *action.Amount > *rule.ThresholdAmount {
		result = "APPROVAL_REQUIRED"
	}
	ruleBasis := fmt.Sprintf("%s:%s", applicable.PolicyCode, applicable.PolicyVersionID)

	decisionID := req.DecisionID
	if decisionID == "" {
		decisionID = uuid.New().String()
	}
	if err := h.decisionLog.RecordDecision(r.Context(), decisionlog.RecordDecisionParams{
		DecisionID:        decisionID,
		TenantID:          req.TenantID,
		LegalEntityID:     req.LegalEntityID,
		ActorID:           req.EvaluatedByPrincipalID,
		ActionType:        req.PolicyType,
		Outcome:           result,
		RuleBasis:         ruleBasis,
		EvaluationContext: req.ActionContext,
		CorrelationID:     correlationID,
	}); err != nil {
		// Best-effort: the evaluation result is still correct and still
		// returned to the caller. Evaluate's own availability must not
		// depend on the evidence store's uptime (see HTTPClient doc comment).
		h.log.Error("evaluateApprovalThreshold: failed to record decision in governance-decision-log-svc",
			zap.String("policy_version_id", applicable.PolicyVersionID),
			zap.String("decision_id", decisionID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
	}

	writeJSON(w, http.StatusOK, evaluateResponse{
		Result:          result,
		PolicyVersionID: applicable.PolicyVersionID,
		RuleBasis:       ruleBasis,
	})
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
