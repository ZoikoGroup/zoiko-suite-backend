package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/authz"
	"zoiko.io/governance-decision-log-svc/internal/domain"
	svcenvelope "zoiko.io/governance-decision-log-svc/internal/envelope"
	"zoiko.io/governance-decision-log-svc/internal/events"
	svcmiddleware "zoiko.io/governance-decision-log-svc/internal/middleware"
	apicontract "zoiko.io/identity-envelope/apicontract"
	"zoiko.io/governance-decision-log-svc/internal/policyclient"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

// DecisionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type DecisionStore interface {
	RecordDecision(ctx context.Context, rec store.DecisionRecord) (*domain.GovernanceDecision, bool, error)
	FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error)
	List(ctx context.Context, params store.ListParams) ([]*domain.GovernanceDecision, error)

	// ── replay manifests (backlog item 34) ──────────────────────────────────
	CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest, event *store.ReplayEvent) (*domain.ReplayManifest, bool, error)
	ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error)

	// ── idempotency keys ──────────────────────────────────────────────────────
	CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error)
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

// maxFutureSkew bounds how far ahead of this server's clock a caller's
// decided_at may be. It allows for clock drift between hosts, not for
// recording a decision before it is made.
const maxFutureSkew = 5 * time.Minute

// decidedAtFloor is the earliest decided_at accepted: no governance decision
// in this platform predates it, and the DB check (000010) holds the same floor.
var decidedAtFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// platformScopeSentinel is authorization-svc's literal for "not scoped to one
// legal entity" (authorization-svc handler.go PlatformScopeSentinel). It is the
// one non-UUID legal_entity_id authorization-svc accepts.
const platformScopeSentinel = "PLATFORM"

// field is one input value checked against the width of the column it lands in
// (migrations 000001, 000008-000010, 000015). A value longer than its column
// used to reach Postgres and come back as 503, which a caller retries forever.
type field struct {
	name  string
	value string
	max   int
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tooLong returns the first field longer than its column, or "".
func tooLong(fields ...field) string {
	for _, f := range fields {
		if len(f.value) > f.max {
			return f.name
		}
	}
	return ""
}

func writeTooLong(w http.ResponseWriter, r *http.Request, name string, fields []field) {
	for _, f := range fields {
		if f.name == name {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: name+" exceeds "+strconv.Itoa(f.max)+" characters", Title: "Value Too Long"})
			return
		}
	}
}

// validLegalEntityID reports whether id is something authorization-svc can
// evaluate: a UUID, or its platform sentinel. Anything else it answers with
// 400, which this service used to relay as a 503 outage.
func validLegalEntityID(id string) bool {
	if id == platformScopeSentinel {
		return true
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// requestLegalEntity is the X-Legal-Entity-Id of the request: the canonical
// envelope's, else the raw header.
func requestLegalEntity(r *http.Request) string {
	if env, ok := svcenvelope.FromContext(r.Context()); ok && env.LegalEntityID != "" {
		return env.LegalEntityID
	}
	return strings.TrimSpace(r.Header.Get(svcenvelope.HeaderLegalEntityID))
}

// requestHash is what an Idempotency-Key stands for: the body AND the request
// fields that come from headers — who sent it, under which correlation,
// jurisdiction and legal entity. Hashing the body alone let the same key and
// body from a different principal, or under a different jurisdiction, replay
// as 200 instead of 409.
func requestHash(principalID, correlationID, jurisdiction, legalEntityID string, body []byte) []byte {
	h := sha256.New()
	for _, part := range []string{principalID, correlationID, jurisdiction, legalEntityID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(body)
	return h.Sum(nil)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store        DecisionStore
	authz        authz.Client
	policyClient policyclient.Client
	log          *zap.Logger

	// authzPlatformScopeID is the legal_entity_id used when a decision is
	// not scoped to one. authorization-svc rejects an empty legal_entity_id.
	authzPlatformScopeID string

	// readAuthzWarnOnly is the DECISION_LOG_READ_AUTHZ=warn migration mode
	// (see config.ReadAuthzMode).
	readAuthzWarnOnly bool

	// now is the clock decided_at is bounded against; a field so tests can pin it.
	now func() time.Time
}

// New constructs a Handler.
func New(store DecisionStore, authzClient authz.Client, policyClient policyclient.Client, authzPlatformScopeID string, log *zap.Logger) *Handler {
	return &Handler{
		store:                store,
		authz:                authzClient,
		policyClient:         policyClient,
		authzPlatformScopeID: authzPlatformScopeID,
		log:                  log,
		now:                  func() time.Time { return time.Now().UTC() },
	}
}

// WarnOnlyReadAuthz switches read authorization to the logged-not-refused
// migration mode. Writes and replays are never affected.
func (h *Handler) WarnOnlyReadAuthz() { h.readAuthzWarnOnly = true }

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
	apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Detail: "X-Principal-Id is required", Title: "Missing Principal"})
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
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusForbidden, Code: "AUTHORIZATION_DENIED", Detail: "Principal not authorized for this action", Title: "Authorization Denied"})
	default:
		h.log.Error("authorization check failed — refusing the request",
			zap.String("principal_id", principalID), zap.String("action_type", actionType), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "AUTHZ_UNAVAILABLE", Detail: "Authorization service unavailable", Title: "Authorization Service Unavailable"})
	}
	return false
}

// readPrincipal returns the caller of a GET. With no X-Principal-Id it answers
// 401 — or, in the warn migration mode, logs and admits the read (ok=true with
// an empty principal, which authorizeReadOf then admits).
func (h *Handler) readPrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID != "" {
		return principalID, true
	}
	if h.readAuthzWarnOnly {
		h.log.Warn("read admitted without X-Principal-Id (DECISION_LOG_READ_AUTHZ=warn); enforce mode answers 401",
			zap.String("path", r.URL.Path), zap.String("tenant_id", svcmiddleware.TenantFromContext(r.Context())))
		return "", true
	}
	return h.requirePrincipal(w, r)
}

// authorizeReadOf gates a read of data belonging to legalEntityID on
// GOVERNANCE_DECISION_READ (GOV-07 "read / export strongly scoped"; GCP §2
// invariant #3, sensitive reads fail closed).
//
// The read is authorized on the entity it is about, so a reader granted READ
// on one entity can read that entity's decisions and no other's. A grant on
// the platform scope is the tenant-wide reader and is tried second, so existing
// platform-scope grants keep working; it is the only scope for a read that
// names no entity. An authz outage is never treated as a denial to fall back
// from: it answers 503.
//
// In the warn migration mode a read that would have been refused is logged
// and served instead.
func (h *Handler) authorizeReadOf(w http.ResponseWriter, r *http.Request, principalID, legalEntityID string) bool {
	if principalID == "" && h.readAuthzWarnOnly {
		return true
	}
	err := authz.ErrDenied
	if legalEntityID != "" && legalEntityID != h.authzPlatformScopeID {
		err = h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, ActionReadDecision)
	}
	if errors.Is(err, authz.ErrDenied) {
		err = h.authz.CheckAllowed(r.Context(), principalID, h.authzPlatformScopeID, ActionReadDecision)
	}
	if err == nil {
		return true
	}
	if h.readAuthzWarnOnly {
		h.log.Warn("unauthorized read admitted (DECISION_LOG_READ_AUTHZ=warn); enforce mode refuses it",
			zap.String("path", r.URL.Path), zap.String("principal_id", principalID),
			zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		return true
	}
	if errors.Is(err, authz.ErrDenied) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusForbidden, Code: "AUTHORIZATION_DENIED", Detail: "Principal not authorized to read these decisions", Title: "Authorization Denied"})
	} else {
		h.log.Error("authorization check failed — refusing the read",
			zap.String("principal_id", principalID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "AUTHZ_UNAVAILABLE", Detail: "Authorization service unavailable", Title: "Authorization Service Unavailable"})
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

// callerCorrelationID returns the X-Correlation-ID the CALLER sent, or "" when
// there is none. main.go fills a missing header from the generated request id
// so every response carries one; that generated value is not the caller's
// correlation and must not be compared with the body's.
func callerCorrelationID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
	if id != "" && id == chimiddleware.GetReqID(r.Context()) {
		return ""
	}
	return id
}

// createDecisionRequest is the wire shape for POST /v1/decisions.
// DecidedAt is optional — if omitted, it defaults to server-receipt time
// (see CONTEXT.md: DecidedAt represents when the decision happened
// upstream, not when it was logged here, but callers may not always have
// a distinct timestamp to send).
// ActorID is NOT in the request — it is derived from the authenticated
// X-Principal-Id header to prevent attribution forgery.
// PolicyVersionId, ActionSubjectType, and ActionSubjectId are optional; a
// missing policy_version_id is derived from a "code:<uuid>" rule_basis.
type createDecisionRequest struct {
	DecisionID    string `json:"decision_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActionType    string `json:"action_type"`
	Outcome       string `json:"outcome"`
	RuleBasis     string `json:"rule_basis"`
	// JurisdictionContext defaults to the envelope's X-Jurisdiction-Context;
	// when both are sent they must agree.
	JurisdictionContext *string         `json:"jurisdiction_context,omitempty"`
	PolicyVersionID     *string         `json:"policy_version_id,omitempty"`
	ActionSubjectType   *string         `json:"action_subject_type,omitempty"`
	ActionSubjectID     *string         `json:"action_subject_id,omitempty"`
	EvaluationContext   json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID       string          `json:"correlation_id"`
	// WorkflowInstanceID and CausationID are optional Event Linkage Keys
	// (doctrine §3.3) — omit either when not known.
	WorkflowInstanceID *string    `json:"workflow_instance_id,omitempty"`
	CausationID        *string    `json:"causation_id,omitempty"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
}

// missingField names the first required field that is empty. tenant_id is
// deliberately NOT required: the tenant is the caller's verified scope, so a
// body omitting it is fine and a body disagreeing with it is a 403.
func (req createDecisionRequest) missingField() string {
	switch {
	case req.DecisionID == "":
		return "decision_id"
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
// Idempotent two ways, and truthful in both:
//   - Idempotency-Key: the same key with the same body replays the stored
//     decision (200); the same key with a different body is 409
//     IDEMPOTENCY_MISMATCH.
//   - decision_id: a repeat of a decision_id this tenant already recorded
//     answers 200 with the STORED row only when the content matches, and 409
//     DECISION_ID_CONFLICT when it does not. It never echoes a body that was
//     not stored.
//
// Response:
//
//	201 → decision recorded for the first time
//	200 → replay of a decision already recorded; body is the stored row
//	400 → missing required field, invalid JSON, decided_at out of bounds,
//	      body correlation_id disagrees with X-Correlation-ID,
//	      missing Idempotency-Key
//	401 → missing X-Principal-Id or X-Tenant-Id
//	403 → tenant scope mismatch, authorization denied
//	409 → idempotency mismatch, decision_id recorded with different content
//	413 → request body too large
//	503 → store unavailable, authorization service unavailable
func (h *Handler) CreateDecision(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusUnauthorized, Code: "TENANT_SCOPE_MISSING", Detail: domain.ErrTenantScopeMissing.Error(), Title: "Tenant Scope Missing"})
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "IDEMPOTENCY_KEY_REQUIRED", Detail: "Idempotency-Key is required for material state changes", Title: "Idempotency Key Required"})
		return
	}

	rawBody, ok := readBody(w, r)
	if !ok {
		return
	}
	var req createDecisionRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: err.Error(), Title: "Invalid JSON"})
		return
	}
	if req.TenantID != "" && req.TenantID != tenantID {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusForbidden, Code: "TENANT_SCOPE_MISMATCH", Detail: domain.ErrTenantScopeMismatch.Error(), Title: "Tenant Scope Mismatch"})
		return
	}

	// The envelope's X-Legal-Entity-Id and the body's legal_entity_id name the
	// same thing; the body's is what is authorized and stored, so the two may
	// not disagree. The body's may be omitted.
	headerEntity := requestLegalEntity(r)
	switch {
	case req.LegalEntityID == "":
		req.LegalEntityID = headerEntity
	case headerEntity != "" && req.LegalEntityID != headerEntity:
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "LEGAL_ENTITY_MISMATCH", Detail: "body legal_entity_id does not match the X-Legal-Entity-Id header", Title: "Legal Entity Mismatch"})
		return
	}

	// One correlation id per request (GCP §16 "propagated end-to-end"). The
	// body's is optional; when both are sent they must agree, or the stored
	// correlation would trace a different flow than the request did.
	headerCorrelation := callerCorrelationID(r)
	switch {
	case req.CorrelationID == "":
		req.CorrelationID = headerCorrelation
	case headerCorrelation != "" && req.CorrelationID != headerCorrelation:
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "CORRELATION_MISMATCH", Detail: "body correlation_id does not match the X-Correlation-ID header", Title: "Correlation Mismatch"})
		return
	}

	// Jurisdiction context (Doc 03 §19; Doc 04 §7.1 jurisdiction_rule_basis):
	// the request's own, by the same one-source rule as correlation.
	jurisdiction := requestJurisdiction(r)
	switch {
	case req.JurisdictionContext == nil || *req.JurisdictionContext == "":
		req.JurisdictionContext = nil
		if jurisdiction != "" {
			req.JurisdictionContext = &jurisdiction
		}
	case jurisdiction != "" && *req.JurisdictionContext != jurisdiction:
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "JURISDICTION_MISMATCH", Detail: "body jurisdiction_context does not match the X-Jurisdiction-Context header", Title: "Jurisdiction Mismatch"})
		return
	}

	if missing := req.missingField(); missing != "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "Field '"+missing+"' is required", Title: "Missing Required Field"})
		return
	}

	// Malformed input is the caller's fault and is answered 400 here, before it
	// can reach authorization-svc or Postgres and come back as a 503 outage.
	limits := []field{
		{"X-Tenant-Id", tenantID, 64},
		{"X-Principal-Id", principalID, 64},
		{"Idempotency-Key", idempotencyKey, 256},
		{"decision_id", req.DecisionID, 64},
		{"legal_entity_id", req.LegalEntityID, 64},
		{"action_type", req.ActionType, 128},
		{"outcome", req.Outcome, 32},
		{"rule_basis", req.RuleBasis, 256},
		{"correlation_id", req.CorrelationID, 64},
		{"jurisdiction_context", ptr(req.JurisdictionContext), 64},
		{"policy_version_id", ptr(req.PolicyVersionID), 64},
		{"action_subject_type", ptr(req.ActionSubjectType), 64},
		{"action_subject_id", ptr(req.ActionSubjectID), 64},
	}
	if name := tooLong(limits...); name != "" {
		writeTooLong(w, r, name, limits)
		return
	}
	if !validLegalEntityID(req.LegalEntityID) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "legal_entity_id must be a UUID or "+platformScopeSentinel, Title: "Invalid Legal Entity"})
		return
	}

	// Authorization comes before any read of existing state, including the
	// idempotency replay: a replay returns a stored decision, and only an
	// authorized writer may see one through this endpoint.
	if !h.authorize(w, r, principalID, req.LegalEntityID, ActionRecordDecision) {
		return
	}

	bodyHash := requestHash(principalID, req.CorrelationID, ptr(req.JurisdictionContext), req.LegalEntityID, rawBody)
	if h.replayByIdempotencyKey(w, r, tenantID, idempotencyKey, bodyHash) {
		return
	}

	now := h.now()
	decidedAt := now
	if req.DecidedAt != nil {
		decidedAt = req.DecidedAt.UTC()
	}
	if decidedAt.After(now.Add(maxFutureSkew)) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "decided_at cannot be more than 5 minutes ahead of the server clock", Title: "Invalid Decision Time"})
		return
	}
	if decidedAt.Before(decidedAtFloor) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "decided_at cannot be before 2020-01-01", Title: "Invalid Decision Time"})
		return
	}

	policyVersionID := req.PolicyVersionID
	if policyVersionID == nil || *policyVersionID == "" {
		policyVersionID = policyVersionFromRuleBasis(req.RuleBasis)
	}

	d := domain.GovernanceDecision{
		DecisionID:          req.DecisionID,
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		ActorID:             principalID,
		ActionType:          req.ActionType,
		Outcome:             req.Outcome,
		RuleBasis:           req.RuleBasis,
		JurisdictionContext: req.JurisdictionContext,
		PolicyVersionID:     policyVersionID,
		EvaluationContext:   req.EvaluationContext,
		CorrelationID:       req.CorrelationID,
		WorkflowInstanceID:  req.WorkflowInstanceID,
		CausationID:         req.CausationID,
		ActionSubjectType:   req.ActionSubjectType,
		ActionSubjectID:     req.ActionSubjectID,
		DecidedAt:           decidedAt,
	}
	event, err := events.DecisionRecorded(d)
	if err != nil {
		h.log.Error("CreateDecision: build event failed", zap.String("decision_id", d.DecisionID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusInternalServerError, Code: "INTERNAL", Detail: "could not build the decision event", Title: "Internal Error"})
		return
	}

	stored, created, err := h.store.RecordDecision(r.Context(), store.DecisionRecord{
		Decision:       d,
		IdempotencyKey: idempotencyKey,
		BodyHash:       bodyHash,
		Event:          event,
	})
	if errors.Is(err, domain.ErrIdempotencyKeyInUse) {
		// A concurrent request with this key committed first; answer as its replay.
		if !h.replayByIdempotencyKey(w, r, tenantID, idempotencyKey, bodyHash) {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		}
		return
	}
	if err != nil {
		h.log.Error("CreateDecision: store unavailable",
			zap.String("decision_id", d.DecisionID),
			zap.String("correlation_id", d.CorrelationID),
			zap.Error(err),
		)
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return
	}

	if !created && !sameDecision(d, stored, req.DecidedAt != nil) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusConflict, Code: "DECISION_ID_CONFLICT", Detail: "decision_id is already recorded with different content", Title: "Decision ID Conflict"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.log.Info("governance decision recorded",
		zap.String("decision_id", stored.DecisionID),
		zap.String("tenant_id", stored.TenantID),
		zap.String("outcome", stored.Outcome),
		zap.Bool("created", created),
		zap.String("correlation_id", stored.CorrelationID),
	)
	writeJSON(w, status, stored)
}

// replayByIdempotencyKey answers the request from a recorded Idempotency-Key
// and reports whether it did. Same key and body → 200 with the stored
// decision; same key, different body → 409.
func (h *Handler) replayByIdempotencyKey(w http.ResponseWriter, r *http.Request, tenantID, key string, bodyHash []byte) bool {
	decisionID, storedHash, err := h.store.CheckIdempotencyKey(r.Context(), tenantID, key)
	if err != nil {
		h.log.Error("idempotency check failed", zap.String("idempotency_key", key), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return true
	}
	if decisionID == "" {
		return false
	}
	if !bytes.Equal(storedHash, bodyHash) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusConflict, Code: "IDEMPOTENCY_MISMATCH", Detail: "Idempotency-Key reused with different request body", Title: "Idempotency Mismatch"})
		return true
	}
	existing, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if err != nil {
		h.log.Error("idempotent replay: fetch stored decision failed", zap.String("decision_id", decisionID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return true
	}
	writeJSON(w, http.StatusOK, existing)
	return true
}

// sameDecision reports whether a repeat POST describes the decision already
// stored. decided_at is compared only when the caller sent one: an omitted
// decided_at defaults to receipt time, which differs on every retry.
func sameDecision(req domain.GovernanceDecision, stored *domain.GovernanceDecision, decidedAtSupplied bool) bool {
	if req.LegalEntityID != stored.LegalEntityID ||
		req.ActorID != stored.ActorID ||
		req.ActionType != stored.ActionType ||
		req.Outcome != stored.Outcome ||
		req.RuleBasis != stored.RuleBasis ||
		req.CorrelationID != stored.CorrelationID ||
		!sameOptional(req.JurisdictionContext, stored.JurisdictionContext) ||
		!sameOptional(req.PolicyVersionID, stored.PolicyVersionID) ||
		!sameOptional(req.ActionSubjectType, stored.ActionSubjectType) ||
		!sameOptional(req.ActionSubjectID, stored.ActionSubjectID) ||
		!sameOptional(req.WorkflowInstanceID, stored.WorkflowInstanceID) ||
		!sameOptional(req.CausationID, stored.CausationID) ||
		!sameJSON(req.EvaluationContext, stored.EvaluationContext) {
		return false
	}
	// Postgres keeps microseconds; compare at that precision.
	return !decidedAtSupplied ||
		req.DecidedAt.Truncate(time.Microsecond).Equal(stored.DecidedAt.Truncate(time.Microsecond))
}

// requestJurisdiction is the jurisdiction context of the request: the
// canonical envelope's (which the envelope middleware may have resolved from
// the legal entity), else the raw header.
func requestJurisdiction(r *http.Request) string {
	if env, ok := svcenvelope.FromContext(r.Context()); ok && env.JurisdictionContext != "" {
		return env.JurisdictionContext
	}
	return strings.TrimSpace(r.Header.Get(svcenvelope.HeaderJurisdictionContext))
}

func sameOptional(a, b *string) bool {
	av, bv := "", ""
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

// sameJSON compares two JSON documents by value: JSONB does not keep the key
// order or whitespace the caller sent.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b) || (isJSONNull(a) && isJSONNull(b))
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func isJSONNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

// policyVersionFromRuleBasis promotes the version from a policy-svc rule_basis
// ("policyCode:<policy_version_id>") into its own column (Doc 04 §7.1
// policy_version_id). Other callers write rule_basis values of other shapes,
// so only a UUID after the last colon counts as a version.
func policyVersionFromRuleBasis(ruleBasis string) *string {
	_, versionID, ok := policyclient.ParseRuleBasis(ruleBasis)
	if !ok {
		return nil
	}
	if _, err := uuid.Parse(versionID); err != nil {
		return nil
	}
	return &versionID
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
//	503 → store unavailable, authorization service unavailable
func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "TENANT_SCOPE_MISSING", Detail: "X-Tenant-Id is required", Title: "Tenant Scope Missing"})
		return
	}
	principalID, ok := h.readPrincipal(w, r)
	if !ok {
		return
	}

	d, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDecisionNotFound):
			// Only a tenant-wide reader learns that an id is unused; anyone
			// else gets the same 403 a real decision would give them.
			if h.authorizeReadOf(w, r, principalID, "") {
				apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusNotFound, Code: "NOT_FOUND", Detail: "No decision with this decision_id for this tenant", Title: "Decision Not Found"})
			}
		default:
			h.log.Error("GetDecision: store unavailable",
				zap.String("decision_id", decisionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		}
		return
	}
	if !h.authorizeReadOf(w, r, principalID, d.LegalEntityID) {
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
// Idempotent on Idempotency-Key: a retry with the same key and body returns
// the manifest the first call recorded (200) without re-evaluating; the same
// key with a different body is 409.
//
// Scoped narrowly for v1, same as policy-svc's own Evaluate: only
// action_type=APPROVAL_THRESHOLD has replay logic implemented.
//
// Response:
//
//	201 → replay performed, manifest recorded
//	200 → retry of a replay already recorded under this Idempotency-Key
//	400 → missing X-Tenant-Id, missing Idempotency-Key, invalid JSON, or
//	      the decision has no resolvable policy version
//	401 → missing X-Principal-Id
//	403 → not authorized to replay
//	404 → decision not found, or its policy version no longer resolvable
//	409 → Idempotency-Key reused with a different body
//	413 → request body too large
//	501 → replay not implemented for this decision's action_type
//	503 → store, policy-svc, or authz unavailable
func (h *Handler) ReplayDecision(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "TENANT_SCOPE_MISSING", Detail: "X-Tenant-Id is required", Title: "Tenant Scope Missing"})
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "IDEMPOTENCY_KEY_REQUIRED", Detail: "Idempotency-Key is required for material state changes", Title: "Idempotency Key Required"})
		return
	}

	rawBody, ok := readBody(w, r)
	if !ok {
		return
	}
	var req replayDecisionRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: err.Error(), Title: "Invalid JSON"})
		return
	}
	replayCorrelation := req.CorrelationID
	if replayCorrelation == "" {
		replayCorrelation = correlationID
	}
	limits := []field{
		{"X-Principal-Id", principalID, 64},
		{"Idempotency-Key", idempotencyKey, 256},
		{"correlation_id", replayCorrelation, 64},
	}
	if name := tooLong(limits...); name != "" {
		writeTooLong(w, r, name, limits)
		return
	}
	// The key is scoped to one decision and one caller, so both are part of
	// what the key stands for.
	replayHash := requestHash(principalID, replayCorrelation, "", decisionID, rawBody)

	decision, err := h.store.FindByID(r.Context(), tenantID, decisionID)
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusNotFound, Code: "NOT_FOUND", Detail: "No decision with this decision_id for this tenant", Title: "Decision Not Found"})
			return
		}
		h.log.Error("ReplayDecision: fetch decision failed", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return
	}

	if !h.authorize(w, r, principalID, decision.LegalEntityID, ActionReplayDecision) {
		return
	}

	// A retry is answered from the manifest already recorded, without
	// re-evaluating: the answer must not depend on policy-svc being up twice.
	prior, err := h.store.ListReplayManifestsByDecision(r.Context(), tenantID, decisionID)
	if err != nil {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return
	}
	for _, m := range prior {
		if m.IdempotencyKey == idempotencyKey {
			h.writeReplayRetry(w, r, m, replayHash)
			return
		}
	}

	if decision.ActionType != "APPROVAL_THRESHOLD" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusNotImplemented, Code: "NOT_IMPLEMENTED", Detail: "Replay not implemented for action type: "+decision.ActionType, Title: "Replay Not Implemented"})
		return
	}

	// The column is authoritative; rows recorded before it existed carry the
	// version only inside rule_basis.
	policyVersionID := ""
	if decision.PolicyVersionID != nil {
		policyVersionID = *decision.PolicyVersionID
	} else if _, v, ok := policyclient.ParseRuleBasis(decision.RuleBasis); ok {
		policyVersionID = v
	}
	if policyVersionID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "Decision records no policy version to replay against (rule_basis: "+decision.RuleBasis+")", Title: "No Policy Version"})
		return
	}

	// Replayed in the decision's own tenant scope — the same tenant the decision
	// was read under, so a replay cannot reach a version outside it.
	version, err := h.policyClient.GetPolicyVersion(r.Context(), tenantID, policyVersionID)
	if err != nil {
		if errors.Is(err, policyclient.ErrPolicyVersionNotFound) {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusNotFound, Code: "NOT_FOUND", Detail: "Policy version no longer resolvable: "+policyVersionID, Title: "Policy Version Not Found"})
			return
		}
		h.log.Error("ReplayDecision: policy-svc unavailable", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Policy service unavailable", Title: "Service Unavailable"})
		return
	}

	replayedOutcome, err := replayApprovalThreshold(version.RulePayload, decision.EvaluationContext)
	if err != nil {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: err.Error(), Title: "Invalid Replay Input"})
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
		IdempotencyKey:        idempotencyKey,
		RequestHash:           replayHash,
	}
	event, err := events.DecisionReplayed(*decision, *manifest, replayCorrelation)
	if err != nil {
		h.log.Error("ReplayDecision: build event failed", zap.String("decision_id", decisionID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusInternalServerError, Code: "INTERNAL", Detail: "could not build the replay event", Title: "Internal Error"})
		return
	}
	stored, created, err := h.store.CreateReplayManifest(r.Context(), manifest, &store.ReplayEvent{
		Envelope:      event,
		LegalEntityID: decision.LegalEntityID,
		CorrelationID: replayCorrelation,
	})
	if err != nil {
		h.log.Error("ReplayDecision: failed to record manifest", zap.String("decision_id", decisionID), zap.String("correlation_id", correlationID), zap.Error(err))
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return
	}
	if !created {
		// A concurrent retry with this key recorded first.
		h.writeReplayRetry(w, r, stored, replayHash)
		return
	}

	h.log.Info("decision replayed",
		zap.String("decision_id", decisionID),
		zap.String("policy_version_id", policyVersionID),
		zap.Bool("outcomes_match", stored.OutcomesMatch),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, stored)
}

func (h *Handler) writeReplayRetry(w http.ResponseWriter, r *http.Request, m *domain.ReplayManifest, requestHash []byte) {
	if !bytes.Equal(m.RequestHash, requestHash) {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusConflict, Code: "IDEMPOTENCY_MISMATCH", Detail: "Idempotency-Key reused with different request body", Title: "Idempotency Mismatch"})
		return
	}
	writeJSON(w, http.StatusOK, m)
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
//	503 → store unavailable, authorization service unavailable
func (h *Handler) ListReplayManifests(w http.ResponseWriter, r *http.Request) {
	decisionID := chi.URLParam(r, "decision_id")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "TENANT_SCOPE_MISSING", Detail: "X-Tenant-Id is required", Title: "Tenant Scope Missing"})
		return
	}
	principalID, ok := h.readPrincipal(w, r)
	if !ok {
		return
	}

	// Manifests are authorized on the entity of the decision they replay. An
	// unknown decision is authorized tenant-wide and answers an empty history,
	// as before.
	entity := ""
	switch d, err := h.store.FindByID(r.Context(), tenantID, decisionID); {
	case err == nil:
		entity = d.LegalEntityID
	case !errors.Is(err, domain.ErrDecisionNotFound):
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
		return
	}
	if !h.authorizeReadOf(w, r, principalID, entity) {
		return
	}

	results, err := h.store.ListReplayManifestsByDecision(r.Context(), tenantID, decisionID)
	if err != nil {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
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
//	limit=50                     page size, a positive integer (above 200 is capped to 200; default 50)
//	offset=0                     zero-based page offset, a non-negative integer
//
// Requires X-Tenant-Id and X-Principal-Id. Authorization is checked via
// GOVERNANCE_DECISION_READ.
//
// Response:
//
//	200 → JSON array of decisions (may be empty), newest first
//	400 → missing X-Tenant-Id, invalid from/to timestamp, invalid limit/offset
//	401 → missing X-Principal-Id
//	403 → not authorized to read decisions
//	503 → store unavailable, authorization service unavailable
func (h *Handler) ListDecisions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "TENANT_SCOPE_MISSING", Detail: "X-Tenant-Id is required", Title: "Tenant Scope Missing"})
		return
	}
	principalID, ok := h.readPrincipal(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	// Filtered to one entity → authorized on that entity; unfiltered reads
	// every entity in the tenant and needs the tenant-wide (platform) grant.
	if !h.authorizeReadOf(w, r, principalID, q.Get("entity")) {
		return
	}

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
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "from must be a valid RFC3339 timestamp", Title: "Invalid Timestamp"})
			return
		}
		params.From = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "to must be a valid RFC3339 timestamp", Title: "Invalid Timestamp"})
			return
		}
		params.To = t
	}
	// A malformed limit or offset is refused, not ignored: silently serving
	// page one to a caller that asked for something else reads as a complete
	// answer, and a negative offset used to reach Postgres and come back as a
	// 503 outage.
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "limit must be a positive integer", Title: "Invalid Limit"})
			return
		}
		params.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "offset must be a non-negative integer", Title: "Invalid Offset"})
			return
		}
		params.Offset = n
	}

	results, err := h.store.List(r.Context(), params)
	if err != nil {
		h.log.Error("ListDecisions: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusServiceUnavailable, Code: "SERVICE_UNAVAILABLE", Detail: "Store unavailable", Title: "Service Unavailable"})
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
	_ = json.NewEncoder(w).Encode(v)
}


// maxRequestBytes caps a JSON request body. Reading until EOF with no cap lets
// a single request make the service allocate whatever the client is willing to
// send -- no auth needed, and nothing in the metrics to distinguish it from
// load.
const maxRequestBytes = 256 << 10 // 256 KiB

// readBody reads a size-capped request body, answering 413 rather than 400
// when the cap is what stopped it: "too large" and "malformed" are different
// faults and a caller can only act on the difference. The raw bytes are kept
// because the idempotency hash is taken over exactly what was sent.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusRequestEntityTooLarge, Code: "PAYLOAD_TOO_LARGE", Detail: "Request body exceeds maximum allowed size", Title: "Payload Too Large"})
			return nil, false
		}
		apicontract.WriteProblem(w, r, apicontract.Problem{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Detail: "Failed to read request body", Title: "Invalid Request Body"})
		return nil, false
	}
	return raw, true
}
