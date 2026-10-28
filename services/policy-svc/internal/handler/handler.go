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
	"github.com/google/uuid"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/envelope"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
	"zoiko.io/policy-svc/internal/store"
)

// PolicyStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type PolicyStore interface {
	CreatePolicy(ctx context.Context, params domain.CreatePolicyParams) (*domain.Policy, bool, error)
	FindPolicyByID(ctx context.Context, policyID string) (*domain.Policy, error)
	CreatePolicyVersion(ctx context.Context, params domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error)
	FindPolicyVersionByID(ctx context.Context, policyVersionID string) (*domain.PolicyVersion, error)
	ListVersionHistory(ctx context.Context, policyID string) ([]*domain.PolicyVersion, error)
	ListTransitions(ctx context.Context, policyVersionID string) ([]*domain.VersionTransition, error)
	Transition(ctx context.Context, cmd domain.TransitionCommand) (*domain.TransitionResult, error)

	SaveEvaluation(ctx context.Context, ev *domain.Evaluation) (*domain.Evaluation, bool, error)
	FindEvaluationByDecision(ctx context.Context, tenantID, decisionID string) (*domain.Evaluation, error)
	FindEvaluationByID(ctx context.Context, tenantID, evaluationID string) (*domain.Evaluation, error)

	// EnqueueEvent records an event that accompanies no state change.
	EnqueueEvent(ctx context.Context, event store.OutboxEvent) error

	// ── control tests & attestations ────────────────────────────────────────
	CreateControlTestDefinition(ctx context.Context, params domain.CreateControlTestDefinitionParams) (*domain.ControlTestDefinition, bool, error)
	FindControlTestDefinitionByID(ctx context.Context, id string) (*domain.ControlTestDefinition, error)
	CreateControlTestExecution(ctx context.Context, params domain.CreateControlTestExecutionParams) (*domain.ControlTestExecution, error)
	ListControlTestExecutions(ctx context.Context, controlTestDefinitionID string) ([]*domain.ControlTestExecution, error)
	ResolveControlEffectiveness(ctx context.Context, controlRef string) (*domain.ControlEffectiveness, error)
	CreateAttestation(ctx context.Context, params domain.CreateAttestationParams) (*domain.Attestation, error)
	FindAttestationByID(ctx context.Context, id string) (*domain.Attestation, error)
	RevokeAttestation(ctx context.Context, attestationID, reason string) (*domain.Attestation, error)
}

// CandidateSource returns resolution candidates — the cache in production.
type CandidateSource interface {
	Candidates(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error)
}

// Mode is how a gate treats a missing or refused authorization.
type Mode string

const (
	// ModeEnforce refuses without a GRANTED decision.
	ModeEnforce Mode = "enforce"
	// ModeWarn checks and logs, but admits — a migration state while
	// callers and grants catch up, never a resting state.
	ModeWarn Mode = "warn"
)

// ParseMode reads a configured mode. Anything but "warn" enforces, so a typo
// cannot disable a control.
func ParseMode(s string) Mode {
	if strings.EqualFold(strings.TrimSpace(s), string(ModeWarn)) {
		return ModeWarn
	}
	return ModeEnforce
}

// Options configures a Handler.
type Options struct {
	// PlatformScopeID is the legal_entity_id presented to authorization-svc
	// for a decision not scoped to one legal entity.
	PlatformScopeID string
	// EvaluateAuthz gates POST /v1/policies/evaluate (POLICY_EVALUATE).
	EvaluateAuthz Mode
	// ReadAuthz gates the read routes (POLICY_READ).
	ReadAuthz Mode
	// Now is the clock; the store stamps lifecycle times from the same one.
	Now func() time.Time
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store      PolicyStore
	candidates CandidateSource
	authz      authz.Client
	opts       Options
	log        *zap.Logger
}

// New constructs a Handler.
func New(st PolicyStore, candidates CandidateSource, authzClient authz.Client, opts Options, log *zap.Logger) *Handler {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.EvaluateAuthz == "" {
		opts.EvaluateAuthz = ModeEnforce
	}
	if opts.ReadAuthz == "" {
		opts.ReadAuthz = ModeEnforce
	}
	return &Handler{store: st, candidates: candidates, authz: authzClient, opts: opts, log: log}
}

// ── errors ───────────────────────────────────────────────────────────────────

// Stable error classes (GCP §16). POLICY_CONFLICTED is V-001 §5's
// applicability state CONFLICTED, which §16 has no class for.
const (
	codeContextUnresolved   = "CONTEXT_UNRESOLVED"
	codeAuthorizationDenied = "AUTHORIZATION_DENIED"
	codePolicyNotEffective  = "POLICY_NOT_EFFECTIVE"
	codeApprovalRequired    = "APPROVAL_REQUIRED"
	codeSoDConflict         = "SOD_CONFLICT"
	codeConcurrency         = "CONCURRENCY_CONFLICT"
	codeIdempotencyMismatch = "IDEMPOTENCY_MISMATCH"
	codeEvidenceIntegrity   = "EVIDENCE_INTEGRITY_FAILED"
	codePolicyConflicted    = "POLICY_CONFLICTED"
	codeInvalidRequest      = "INVALID_REQUEST"
	codeNotFound            = "NOT_FOUND"
	codeUnavailable         = "SERVICE_UNAVAILABLE"
	codeNotImplemented      = "NOT_IMPLEMENTED"
)

func writeError(w http.ResponseWriter, r *http.Request, status int, code, legacy, message string, extra map[string]any) {
	detail := message
	if detail == "" {
		detail = legacy
	}
	// code is the §16 class (this service's contract before API-2 too); the
	// specific refusal (policy_not_found, missing_field, ...) stays as
	// reason_code, which a detail message would otherwise hide.
	if legacy != "" {
		if _, taken := extra["reason_code"]; !taken {
			withReason := map[string]any{"reason_code": legacy}
			for k, v := range extra {
				withReason[k] = v
			}
			extra = withReason
		}
	}
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:    status,
		Code:      code,
		Detail:    detail,
		Retryable: status >= 500,
		Extra:     extra,
	})
}

func (h *Handler) storeUnavailable(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.log.Error(op+": store unavailable",
		zap.String("correlation_id", r.Header.Get("X-Correlation-ID")),
		zap.Error(err),
	)
	writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "", nil)
}

// ── request context ──────────────────────────────────────────────────────────

// requirePrincipal resolves the acting principal from the gateway-verified
// X-Principal-Id header, writing 401 when absent. The body never names the
// actor: a caller must not choose what the audit trail records about them.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := strings.TrimSpace(r.Header.Get("X-Principal-Id")); id != "" {
		return id, true
	}
	writeError(w, r, http.StatusUnauthorized, codeContextUnresolved, "missing_principal",
		"X-Principal-Id is required — the gateway sets it from a verified identity envelope", nil)
	return "", false
}

// requireTenant reads the caller's verified tenant scope (X-Tenant-Id). A
// non-UUID tenant is refused here rather than reaching a uuid comparison and
// coming back as a 503.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, r, http.StatusUnauthorized, codeContextUnresolved, "tenant_scope_missing", "caller tenant scope missing", nil)
		return "", false
	}
	if _, err := uuid.Parse(tenantID); err != nil {
		writeError(w, r, http.StatusBadRequest, codeContextUnresolved, "invalid_tenant_id", "X-Tenant-Id must be a UUID", nil)
		return "", false
	}
	return tenantID, true
}

// refuseForeignTenant answers 403 if claimed names a tenant other than the
// caller's verified scope. nil means GLOBAL scope, a deliberate request.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, r *http.Request, claimed *string, tenantID string) bool {
	if claimed == nil || *claimed == "" || *claimed == tenantID {
		return false
	}
	writeError(w, r, http.StatusForbidden, codeContextUnresolved, "tenant_scope_mismatch",
		"request tenant_id does not match the caller's verified tenant scope", nil)
	return true
}

// legalEntityFromRequest reconciles the body's legal_entity_id with the
// envelope's X-Legal-Entity-Id: they name one thing, so they may not differ,
// and either may be omitted. It writes 400 on a mismatch or a non-UUID.
func legalEntityFromRequest(w http.ResponseWriter, r *http.Request, body *string) (*string, bool) {
	header := ""
	if env, ok := envelope.FromContext(r.Context()); ok {
		header = env.LegalEntityID
	}
	le := body
	if (le == nil || *le == "") && header != "" && header != "PLATFORM" {
		le = &header
	}
	if le != nil && *le == "" {
		le = nil
	}
	if le != nil {
		if _, err := uuid.Parse(*le); err != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_legal_entity_id", "legal_entity_id must be a UUID", nil)
			return nil, false
		}
		if header != "" && header != "PLATFORM" && header != *le {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "legal_entity_mismatch",
				"X-Legal-Entity-Id header does not match body legal_entity_id", nil)
			return nil, false
		}
	}
	return le, true
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// requireUUID answers 400 unless value is a UUID — a malformed id is the
// caller's mistake, not an outage (audit X10).
func requireUUID(w http.ResponseWriter, r *http.Request, field, value string) bool {
	if validUUID(value) {
		return true
	}
	writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_"+field, field+" must be a valid UUID", nil)
	return false
}

func optionalUUID(w http.ResponseWriter, r *http.Request, field string, value *string) bool {
	if value == nil || *value == "" {
		return true
	}
	return requireUUID(w, r, field, *value)
}

// tooLong answers 400 when value exceeds its column width, before it can
// reach Postgres and come back as a 503.
func tooLong(w http.ResponseWriter, r *http.Request, field, value string, max int) bool {
	if len(value) <= max {
		return false
	}
	writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "field_too_long", field+" exceeds "+strconv.Itoa(max)+" characters",
		map[string]any{"field": field, "max_length": max})
	return true
}

func missingField(w http.ResponseWriter, r *http.Request, field string) {
	writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "missing_field", "", map[string]any{"field": field})
}

func eventMeta(r *http.Request, actor string) domain.EventMeta {
	m := domain.EventMeta{CorrelationID: r.Header.Get("X-Correlation-ID"), ActorID: actor}
	if env, ok := envelope.FromContext(r.Context()); ok {
		m.CausationID = env.CausationID
		m.IdempotencyKey = env.IdempotencyKey
		m.JurisdictionContext = env.JurisdictionContext
		if m.CorrelationID == "" {
			m.CorrelationID = env.CorrelationID
		}
	}
	return m
}

// ── authorization ────────────────────────────────────────────────────────────

func (h *Handler) scopeFor(legalEntityID *string) string {
	if legalEntityID != nil && *legalEntityID != "" {
		return *legalEntityID
	}
	return h.opts.PlatformScopeID
}

// authorize asks authorization-svc whether principalID may perform
// actionType in the given scope, writing the fail-closed response itself
// when the answer is anything other than GRANTED.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string, legalEntityID *string) bool {
	err := h.authz.CheckAllowed(r.Context(), principalID, h.scopeFor(legalEntityID), actionType, svcmiddleware.TenantFromContext(r.Context()))
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, r, http.StatusForbidden, codeAuthorizationDenied, "authorization_denied", "", map[string]any{"action_type": actionType})
	default:
		h.log.Error("authorization check failed — refusing the request",
			zap.String("principal_id", principalID),
			zap.String("action_type", actionType),
			zap.Error(err),
		)
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "authz_unavailable", "", nil)
	}
	return false
}

// gate authorizes a read or an evaluation under mode. In warn mode a missing
// principal or a refusal is logged and admitted; in enforce mode it is 401 /
// 403 / 503 like any mutation. Returns the principal, if any.
func (h *Handler) gate(w http.ResponseWriter, r *http.Request, mode Mode, actionType string, legalEntityID *string) (string, bool) {
	principal := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if mode != ModeWarn {
		if principal == "" {
			h.requirePrincipal(w, r)
			return "", false
		}
		return principal, h.authorize(w, r, principal, actionType, legalEntityID)
	}
	if principal == "" {
		h.log.Warn("authz gate (warn mode): request has no X-Principal-Id; admitted",
			zap.String("action_type", actionType), zap.String("path", r.URL.Path))
		return "", true
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, h.scopeFor(legalEntityID), actionType, svcmiddleware.TenantFromContext(r.Context())); err != nil {
		h.log.Warn("authz gate (warn mode): would be refused; admitted",
			zap.String("principal_id", principal), zap.String("action_type", actionType), zap.Error(err))
	}
	return principal, true
}

// ── routes ───────────────────────────────────────────────────────────────────

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/policies", h.CreatePolicy)
	r.Get("/v1/policies", h.ListApplicablePolicyVersions)
	r.Get("/v1/policies/resolve", h.ResolveEffectivePolicy)
	r.Get("/v1/policies/explain", h.ExplainPolicyResolution)
	r.Post("/v1/policies/evaluate", h.Evaluate)
	r.Get("/v1/policies/{policy_id}", h.GetPolicy)
	r.Post("/v1/policies/{policy_id}/versions", h.CreatePolicyVersion)
	r.Get("/v1/policies/{policy_id}/versions", h.ListVersionHistory)

	// GOV-05 lifecycle commands.
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/submit-review", h.lifecycle(domain.CommandSubmitReview))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/approve", h.lifecycle(domain.CommandApprove))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/activate", h.lifecycle(domain.CommandActivate))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/schedule", h.lifecycle(domain.CommandSchedule))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/suspend", h.lifecycle(domain.CommandSuspend))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/retire", h.lifecycle(domain.CommandRetire))
	r.Post("/v1/policies/{policy_id}/versions/{version_id}/review-suspension", h.lifecycle(domain.CommandReviewSuspension))

	r.Get("/v1/policy-versions/compare", h.ComparePolicyVersions)
	r.Get("/v1/policy-versions/{version_id}", h.GetPolicyVersionByID)
	r.Get("/v1/policy-versions/{version_id}/transitions", h.ListTransitions)
	r.Get("/v1/policy-evaluations/{evidence_id}", h.GetEvaluation)
}

// correlationIDMiddleware echoes X-Correlation-ID from the request into the
// response on every route registered via RegisterRoutes.
func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/policies ────────────────────────────────────────────────────────

type createPolicyRequest struct {
	PolicyID   string `json:"policy_id,omitempty"`
	PolicyCode string `json:"policy_code"`
	PolicyName string `json:"policy_name"`
	PolicyType string `json:"policy_type"`

	// Scope is TENANT (the default: a family owned by the caller's tenant,
	// Doc 04 §7.1) or GLOBAL (a platform family, which needs
	// POLICY_CREATE_GLOBAL).
	Scope string `json:"scope,omitempty"`

	// Accepted for backward compatibility and IGNORED — the acting principal
	// comes from X-Principal-Id.
	CreatedByPrincipalID string `json:"created_by_principal_id,omitempty"`
}

// CreatePolicy handles POST /v1/policies.
//
// Idempotent on (owner, policy_code): a repeat with identical attributes is
// 200, with different attributes 409.
//
//	201 created · 200 replay · 400 invalid · 401 no identity · 403 denied
//	409 policy_code taken with different attributes · 503 unavailable
func (h *Handler) CreatePolicy(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.PolicyCode == "":
		missingField(w, r, "policy_code")
		return
	case req.PolicyName == "":
		missingField(w, r, "policy_name")
		return
	case req.PolicyType == "":
		missingField(w, r, "policy_type")
		return
	}
	if tooLong(w, r, "policy_code", req.PolicyCode, 128) || tooLong(w, r, "policy_type", req.PolicyType, 64) ||
		tooLong(w, r, "policy_name", req.PolicyName, 512) {
		return
	}
	if req.PolicyID != "" && !requireUUID(w, r, "policy_id", req.PolicyID) {
		return
	}

	owner := &tenantID
	action := authz.ActionPolicyCreate
	switch strings.ToUpper(req.Scope) {
	case "", domain.ScopeTypeTenant:
	case domain.ScopeTypeGlobal:
		owner, action = nil, authz.ActionPolicyCreateGlobal
	default:
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_scope", "scope must be TENANT or GLOBAL", nil)
		return
	}
	// A family is not scoped to a legal entity — scoping lives on its
	// versions — so this is a platform-scope decision.
	if !h.authorize(w, r, principalID, action, nil) {
		return
	}

	p, created, err := h.store.CreatePolicy(r.Context(), domain.CreatePolicyParams{
		PolicyID:             req.PolicyID,
		PolicyCode:           req.PolicyCode,
		PolicyName:           req.PolicyName,
		PolicyType:           req.PolicyType,
		TenantID:             owner,
		CreatedByPrincipalID: principalID,
		Meta:                 eventMeta(r, principalID),
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "policy_conflict", "", map[string]any{"policy_code": req.PolicyCode})
			return
		}
		h.storeUnavailable(w, r, "CreatePolicy", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

// GetPolicy handles GET /v1/policies/{policy_id}: a family visible to the
// caller (a platform family, or one the caller's tenant owns).
func (h *Handler) GetPolicy(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	if !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	p, err := h.store.FindPolicyByID(r.Context(), policyID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
			return
		}
		h.storeUnavailable(w, r, "GetPolicy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/policies/{policy_id}/versions ──────────────────────────────────

type createPolicyVersionRequest struct {
	PolicyVersionID string          `json:"policy_version_id,omitempty"`
	TenantID        *string         `json:"tenant_id,omitempty"`
	LegalEntityID   *string         `json:"legal_entity_id,omitempty"`
	RulePayload     json.RawMessage `json:"rule_payload,omitempty"`
	EffectiveFrom   time.Time       `json:"effective_from"`
	EffectiveTo     *time.Time      `json:"effective_to,omitempty"`
	Source          string          `json:"source,omitempty"`
	Rationale       string          `json:"rationale,omitempty"`

	// Accepted then IGNORED — see createPolicyRequest.
	CreatedByPrincipalID string `json:"created_by_principal_id,omitempty"`
}

// validateRulePayload checks a payload against what its policy type's
// evaluator will need (V-001 §10.1 step 2), so a version that could never be
// evaluated is refused at authoring time, not at the first evaluation.
func validateRulePayload(w http.ResponseWriter, r *http.Request, policyType string, payload json.RawMessage) bool {
	if len(payload) == 0 {
		if policyType == policyTypeApprovalThreshold {
			missingField(w, r, "rule_payload.threshold_amount")
			return false
		}
		return true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_rule_payload", "rule_payload must be a JSON object", nil)
		return false
	}
	if policyType == policyTypeApprovalThreshold {
		amount, present, err := decodeExactAmount(payload, "threshold_amount")
		if err != nil || !present {
			missingField(w, r, "rule_payload.threshold_amount")
			return false
		}
		if amount.Sign() < 0 {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_rule_payload", "threshold_amount must not be negative", nil)
			return false
		}
	}
	return true
}

// CreatePolicyVersion handles POST /v1/policies/{policy_id}/versions. New
// versions are always DRAFT; going live is the review → approve → activate
// lifecycle.
//
//	201 created · 200 replay of the same Idempotency-Key or id · 400 invalid
//	404 policy not found · 409 key or id reused with different content
func (h *Handler) CreatePolicyVersion(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createPolicyVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.EffectiveFrom.IsZero() {
		missingField(w, r, "effective_from")
		return
	}
	if req.EffectiveTo != nil && !req.EffectiveTo.After(req.EffectiveFrom) {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_temporal_range", "effective_to must be after effective_from", nil)
		return
	}
	if (req.PolicyVersionID != "" && !requireUUID(w, r, "policy_version_id", req.PolicyVersionID)) ||
		!optionalUUID(w, r, "tenant_id", req.TenantID) {
		return
	}
	if tooLong(w, r, "source", req.Source, 64) || tooLong(w, r, "rationale", req.Rationale, 4000) {
		return
	}
	if req.LegalEntityID != nil && *req.LegalEntityID == "" {
		req.LegalEntityID = nil
	}
	if req.LegalEntityID != nil {
		if !requireUUID(w, r, "legal_entity_id", *req.LegalEntityID) {
			return
		}
		if env, ok := envelope.FromContext(r.Context()); ok && env.LegalEntityID != "" && env.LegalEntityID != *req.LegalEntityID {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "legal_entity_mismatch",
				"X-Legal-Entity-Id header does not match body legal_entity_id", nil)
			return
		}
	}
	if h.refuseForeignTenant(w, r, req.TenantID, tenantID) {
		return
	}
	if req.TenantID != nil && *req.TenantID == "" {
		req.TenantID = nil
	}

	family, err := h.store.FindPolicyByID(r.Context(), policyID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
			return
		}
		h.storeUnavailable(w, r, "CreatePolicyVersion", err)
		return
	}
	// A tenant-owned family's versions live in its owner's scope; an omitted
	// tenant_id means that scope rather than an (impossible) global version.
	if family.TenantID != nil && req.TenantID == nil {
		req.TenantID = family.TenantID
	}
	// A global version naming one legal entity is an incoherent scope.
	if req.TenantID == nil && req.LegalEntityID != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_scope", "a global version (no tenant_id) cannot name a legal_entity_id", nil)
		return
	}
	if !validateRulePayload(w, r, family.PolicyType, req.RulePayload) {
		return
	}

	// Authorized in the scope the version will apply to; a global version
	// needs the distinct platform-wide grant.
	action := authz.ActionPolicyVersionCreate
	if req.TenantID == nil {
		action = authz.ActionPolicyVersionCreateGlobal
	}
	if !h.authorize(w, r, principalID, action, req.LegalEntityID) {
		return
	}

	meta := eventMeta(r, principalID)
	v, created, err := h.store.CreatePolicyVersion(r.Context(), domain.CreatePolicyVersionParams{
		PolicyVersionID:      req.PolicyVersionID,
		PolicyID:             policyID,
		TenantID:             req.TenantID,
		LegalEntityID:        req.LegalEntityID,
		RulePayload:          []byte(req.RulePayload),
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		Source:               req.Source,
		Rationale:            req.Rationale,
		CreatedByPrincipalID: principalID,
		RequestKey:           meta.IdempotencyKey,
		Meta:                 meta,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPolicyNotFound):
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
		case errors.Is(err, domain.ErrInvalidScope):
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_scope", "a version of a tenant-owned policy must be in its owner's scope", nil)
		case errors.Is(err, domain.ErrIdempotencyMismatch), errors.Is(err, domain.ErrConflict):
			writeError(w, r, http.StatusConflict, codeIdempotencyMismatch, "policy_version_conflict",
				"this Idempotency-Key or policy_version_id was already used with different content", map[string]any{"policy_id": policyID})
		default:
			h.storeUnavailable(w, r, "CreatePolicyVersion", err)
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeVersion(w, status, v)
}

// writeVersion writes a version with its row_version as the ETag, the value
// a caller returns in If-Match or expected_version.
func writeVersion(w http.ResponseWriter, status int, v *domain.PolicyVersion) {
	w.Header().Set("ETag", `"`+strconv.Itoa(v.RowVersion)+`"`)
	writeJSON(w, status, v)
}

// ── lifecycle commands ───────────────────────────────────────────────────────

type lifecycleRequest struct {
	// ExpectedVersion is optimistic concurrency (GCP §16 expected_version);
	// If-Match carries the same value.
	ExpectedVersion *int `json:"expected_version,omitempty"`
	// ReasonCode is required for suspend and retire (GCP §16: "purpose /
	// reason_code required for privileged, destructive or emergency
	// commands"); Reason is optional free text beside it.
	ReasonCode string `json:"reason_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// ApprovalReference links an approval to its review record (a workflow
	// instance, change ticket, ...).
	ApprovalReference string `json:"approval_reference,omitempty"`
	// Outcome is required for review-suspension: UPHELD or OVERTURNED.
	Outcome string `json:"outcome,omitempty"`

	// Accepted then IGNORED (pre-header callers of activate).
	ActivatedByPrincipalID string `json:"activated_by_principal_id,omitempty"`
}

// lifecycleAction is the authorization action for each command, tenant
// scoped and global.
var lifecycleAction = map[string][2]string{
	domain.CommandSubmitReview: {authz.ActionPolicyVersionCreate, authz.ActionPolicyVersionCreateGlobal},
	domain.CommandApprove:      {authz.ActionPolicyVersionApprove, authz.ActionPolicyVersionApproveGlobal},
	domain.CommandActivate:     {authz.ActionPolicyVersionActivate, authz.ActionPolicyVersionActivateGlobal},
	domain.CommandSchedule:     {authz.ActionPolicyVersionActivate, authz.ActionPolicyVersionActivateGlobal},
	domain.CommandSuspend:      {authz.ActionPolicyVersionSuspend, authz.ActionPolicyVersionSuspendGlobal},
	domain.CommandRetire:       {authz.ActionPolicyVersionRetire, authz.ActionPolicyVersionRetireGlobal},
	// Reviewing an emergency suspension is a checker's act, held by approvers.
	domain.CommandReviewSuspension: {authz.ActionPolicyVersionApprove, authz.ActionPolicyVersionApproveGlobal},
}

// ifMatch parses If-Match ("3" or 3) into a row_version.
func ifMatch(r *http.Request) (*int, bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return nil, true
	}
	v = strings.TrimPrefix(v, "W/")
	n, err := strconv.Atoi(strings.Trim(v, `"`))
	if err != nil {
		return nil, false
	}
	return &n, true
}

// lifecycle handles POST /v1/policies/{policy_id}/versions/{version_id}/<command>
// for the GOV-05 commands: submit-review, approve, activate, schedule,
// suspend and retire.
//
// activate makes an APPROVED version ACTIVE now, or SCHEDULED when its
// effective_from is in the future; schedule does only the latter.
// review-suspension records an independent review of a suspension.
//
// Every command needs the version's row_version (If-Match or
// expected_version; 428 without). suspend, retire and review-suspension also
// need reason_code, and review-suspension an outcome. The body is otherwise
// optional.
//
//	200 transitioned, or already in the target state (idempotent)
//	400 malformed · 401 no identity · 403 denied, or maker = checker
//	428 no expected_version
//	404 not found, or the version is not this policy's
//	409 APPROVAL_REQUIRED · CONCURRENCY_CONFLICT · POLICY_CONFLICTED
//	503 unavailable
func (h *Handler) lifecycle(command string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		policyID := chi.URLParam(r, "policy_id")
		versionID := chi.URLParam(r, "version_id")

		principalID, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		if !requireUUID(w, r, "policy_id", policyID) || !requireUUID(w, r, "version_id", versionID) {
			return
		}
		if _, ok := h.requireTenant(w, r); !ok {
			return
		}

		var req lifecycleRequest
		if !decodeOptionalJSON(w, r, &req) {
			return
		}
		expected, ok := ifMatch(r)
		if !ok {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_if_match", "If-Match must be a row_version", nil)
			return
		}
		if req.ExpectedVersion != nil {
			if expected != nil && *expected != *req.ExpectedVersion {
				writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "expected_version_mismatch", "If-Match and expected_version disagree", nil)
				return
			}
			expected = req.ExpectedVersion
		}
		// GOV-05: "Idempotency-Key + expected_version where stateful" — every
		// lifecycle command is stateful, so the version the caller decided on
		// must be named. 428 is HTTP's own answer to a missing precondition.
		if expected == nil {
			writeError(w, r, http.StatusPreconditionRequired, codeInvalidRequest, "expected_version_required",
				"send the version's row_version as If-Match or expected_version (GET the version for its ETag)", nil)
			return
		}
		reason := strings.TrimSpace(req.ReasonCode)
		if (command == domain.CommandSuspend || command == domain.CommandRetire || command == domain.CommandReviewSuspension) && reason == "" {
			missingField(w, r, "reason_code")
			return
		}
		outcome := strings.ToUpper(strings.TrimSpace(req.Outcome))
		if command == domain.CommandReviewSuspension && outcome != domain.SuspensionUpheld && outcome != domain.SuspensionOverturned {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_outcome", "outcome must be UPHELD or OVERTURNED", nil)
			return
		}
		if free := strings.TrimSpace(req.Reason); free != "" {
			if reason != "" {
				reason += ": "
			}
			reason += free
		}
		if tooLong(w, r, "reason", reason, 2000) || tooLong(w, r, "approval_reference", req.ApprovalReference, 256) {
			return
		}

		existing, err := h.store.FindPolicyVersionByID(r.Context(), versionID)
		if err != nil {
			if errors.Is(err, domain.ErrPolicyVersionNotFound) {
				writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": versionID})
				return
			}
			h.storeUnavailable(w, r, "lifecycle", err)
			return
		}
		// A command must never succeed against the wrong policy_id/version_id
		// pairing just because version_id alone resolves.
		if existing.PolicyID != policyID {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "",
				map[string]any{"policy_id": policyID, "policy_version_id": versionID})
			return
		}

		// Authorized against the scope the version actually binds, read from
		// the stored record rather than the request.
		actions := lifecycleAction[command]
		action := actions[0]
		if existing.TenantID == nil {
			action = actions[1]
		}
		// The checker's actions name the maker, so authorization-svc's
		// own-object SoD applies as well as this service's own check.
		switch command {
		case domain.CommandApprove, domain.CommandActivate, domain.CommandSchedule:
			r = r.WithContext(authz.WithResourceOwner(r.Context(), existing.CreatedByPrincipalID))
		case domain.CommandReviewSuspension:
			if existing.SuspendedByPrincipalID != nil {
				r = r.WithContext(authz.WithResourceOwner(r.Context(), *existing.SuspendedByPrincipalID))
			}
		}
		if !h.authorize(w, r, principalID, action, existing.LegalEntityID) {
			return
		}

		res, err := h.store.Transition(r.Context(), domain.TransitionCommand{
			PolicyVersionID:   versionID,
			Command:           command,
			ActorID:           principalID,
			ExpectedVersion:   expected,
			Reason:            reason,
			ApprovalReference: strings.TrimSpace(req.ApprovalReference),
			Outcome:           outcome,
			Meta:              eventMeta(r, principalID),
		})
		if err != nil {
			h.writeTransitionError(w, r, versionID, err)
			return
		}
		h.log.Info("policy version transition",
			zap.String("command", command),
			zap.String("policy_version_id", versionID),
			zap.String("status", res.Version.VersionStatus),
			zap.Bool("transitioned", res.Transitioned),
			zap.Int("superseded_count", len(res.Superseded)),
			zap.String("correlation_id", r.Header.Get("X-Correlation-ID")),
		)
		writeVersion(w, http.StatusOK, res.Version)
	}
}

func (h *Handler) writeTransitionError(w http.ResponseWriter, r *http.Request, versionID string, err error) {
	extra := map[string]any{"policy_version_id": versionID}
	switch {
	case errors.Is(err, domain.ErrPolicyVersionNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", extra)
	case errors.Is(err, domain.ErrApprovalRequired):
		writeError(w, r, http.StatusConflict, codeApprovalRequired, "approval_required",
			"submit the version for review and have an independent approver approve it before activation", extra)
	case errors.Is(err, domain.ErrSoDConflict):
		writeError(w, r, http.StatusForbidden, codeSoDConflict, "maker_checker_violation",
			"the maker cannot be the checker: the author cannot approve or activate, and the suspender cannot review the suspension", extra)
	case errors.Is(err, domain.ErrPolicyConflicted):
		writeError(w, r, http.StatusConflict, codePolicyConflicted, "policy_overlap", err.Error(), extra)
	case errors.Is(err, domain.ErrConcurrencyConflict):
		writeError(w, r, http.StatusConflict, codeConcurrency, "concurrency_conflict", err.Error(), extra)
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, r, http.StatusConflict, codeConcurrency, "invalid_transition",
			"the version's current status does not allow this command", extra)
	default:
		h.storeUnavailable(w, r, "Transition", err)
	}
}

// ── reads ────────────────────────────────────────────────────────────────────

// ListVersionHistory handles GET /v1/policies/{policy_id}/versions: every
// version visible to the caller's tenant, newest first.
func (h *Handler) ListVersionHistory(w http.ResponseWriter, r *http.Request) {
	policyID := chi.URLParam(r, "policy_id")
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !requireUUID(w, r, "policy_id", policyID) {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	results, err := h.store.ListVersionHistory(r.Context(), policyID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_not_found", "", map[string]any{"policy_id": policyID})
			return
		}
		h.storeUnavailable(w, r, "ListVersionHistory", err)
		return
	}
	if results == nil {
		results = []*domain.PolicyVersion{}
	}
	writeJSON(w, http.StatusOK, results)
}

// GetPolicyVersionByID handles GET /v1/policy-versions/{version_id} — the
// exact version a past decision used, whatever its status today (GOV-05
// GetPolicyVersion; historical reads never resolve through "latest").
func (h *Handler) GetPolicyVersionByID(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", versionID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.FindPolicyVersionByID(r.Context(), versionID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyVersionNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": versionID})
			return
		}
		h.storeUnavailable(w, r, "GetPolicyVersionByID", err)
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, v.LegalEntityID); !ok {
		return
	}
	writeVersion(w, http.StatusOK, v)
}

// ListTransitions handles GET /v1/policy-versions/{version_id}/transitions —
// who moved the version through each state, when and why (GOV-05 evidence:
// approvals, activation actor/time; negative path #3, suspension evidenced).
func (h *Handler) ListTransitions(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "version_id")
	if !requireUUID(w, r, "version_id", versionID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	ts, err := h.store.ListTransitions(r.Context(), versionID)
	if err != nil {
		if errors.Is(err, domain.ErrPolicyVersionNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": versionID})
			return
		}
		h.storeUnavailable(w, r, "ListTransitions", err)
		return
	}
	if ts == nil {
		ts = []*domain.VersionTransition{}
	}
	writeJSON(w, http.StatusOK, ts)
}

// ── plumbing ─────────────────────────────────────────────────────────────────

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxRequestBytes caps a JSON request body.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a required, size-capped JSON body, answering 413 rather
// than 400 when the cap is what stopped it.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeInvalidRequest, "request_too_large", "", nil)
			return false
		}
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_json", err.Error(), nil)
		return false
	}
	return true
}

// decodeOptionalJSON is decodeJSON for a body that may be absent: an empty
// body leaves dst untouched. (decodeJSON on an empty body is io.EOF, which is
// how "the body is optional" used to answer 400 invalid_json.)
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	err := json.NewDecoder(r.Body).Decode(dst)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return true
	default:
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, codeInvalidRequest, "request_too_large", "", nil)
			return false
		}
		writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_json", err.Error(), nil)
		return false
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
