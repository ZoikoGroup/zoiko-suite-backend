// Package handler exposes privacy-transfer-svc's REST API — PRV-05.
//
// Every route — read or write — runs the same pre-flight, in this order:
//
//  1. X-Tenant-Id             -> 401 (middleware.TenantContext, which wraps the
//     group and therefore runs before the handler)
//  2. X-Principal-Id          -> 401
//  3. mutating only: X-Correlation-ID -> 400
//  4. mutating only: Idempotency-Key  -> 400
//  5. authorization-svc grant on the caller's verified tenant -> 403 / 503 (fail closed)
//  6. mutating only: idempotent replay lookup (lookup failure -> 503, never "proceed")
//  7. body validation -> 400
//  8. resolve the resource (RLS-scoped) -> 404
//
// Authorization is decided on the caller's own verified tenant BEFORE any
// resource lookup, so an unauthorized caller cannot use 404-vs-403 to probe
// which ids exist, and a replayed key can never bypass authorization.
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-transfer-svc/internal/authz"
	"zoiko.io/privacy-transfer-svc/internal/domain"
	svcmiddleware "zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/purposeregistry"
	"zoiko.io/privacy-transfer-svc/internal/store"
)

const (
	PrivacyTransferRelationshipManage = "PRIVACY_TRANSFER_RELATIONSHIP_MANAGE"
	PrivacyTransferMechanismManage    = "PRIVACY_TRANSFER_MECHANISM_MANAGE"
	PrivacyTransferAssessmentRecord   = "PRIVACY_TRANSFER_ASSESSMENT_RECORD"
	// PrivacyTransferDecisionEvaluate gates EvaluateTransfer — distinct from
	// the three *_MANAGE/_RECORD actions above, which govern creating or
	// amending the underlying relationship/mechanism/assessment records.
	PrivacyTransferDecisionEvaluate = "PRIVACY_TRANSFER_DECISION_EVALUATE"
)

// Read routes reuse the existing action of the resource they expose; the
// specification names no read permission and none was invented:
//
//	relationships, subprocessors -> PRIVACY_TRANSFER_RELATIONSHIP_MANAGE
//	transfer mechanisms          -> PRIVACY_TRANSFER_MECHANISM_MANAGE
//	assessments                  -> PRIVACY_TRANSFER_ASSESSMENT_RECORD
//	decisions, trigger evaluation-> PRIVACY_TRANSFER_DECISION_EVALUATE
//
// Whether dedicated read actions are wanted is a needs-clarification item.

// §19 event types are defined in the store (they are enqueued there, inside
// the transaction of the change they announce).
const (
	EventTransferAuthorizationChanged = store.EventTransferAuthorizationChanged
	EventProcessorChanged             = store.EventProcessorChanged
	EventAssessmentExpiring           = store.EventAssessmentExpiring
)

const maxBodyBytes = 1 << 20

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real dependency on PRV-01. The caller's verified
// principal is forwarded so PRV-01 can authenticate and authorize the read.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, tenantID, principalID, activityID string) (*purposeregistry.ActivityVersion, error)
}

// Handler holds no event publisher: events are enqueued by the store, inside
// the transaction of the state change (internal/events outbox).
type Handler struct {
	store    store.Store
	authz    AuthzChecker
	purposes PurposeChecker
	log      *zap.Logger
}

func New(st store.Store, az AuthzChecker, purposes PurposeChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, purposes: purposes, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	mountRoutes := func(prefix string) {
		r.Route(prefix+"/processor-relationships", func(r chi.Router) {
			r.Post("/", h.CreateRelationship)
			r.Get("/", h.ListRelationships)
			r.Get("/{relationshipID}", h.GetRelationship)
			r.Post("/{relationshipID}/status", h.UpdateRelationshipStatus)
			r.Post("/{relationshipID}/subprocessors", h.AttachSubprocessor)
			r.Get("/{relationshipID}/subprocessors", h.ListSubprocessors)
		})
		r.Route(prefix+"/transfer-mechanisms", func(r chi.Router) {
			r.Post("/", h.CreateMechanism)
			r.Get("/{mechanismID}", h.GetMechanism)
		})
		r.Route(prefix+"/transfer-assessments", func(r chi.Router) {
			r.Post("/", h.RecordAssessment)
			r.Get("/", h.GetLatestAssessment)
			r.Post("/evaluate-triggers", h.EvaluateTriggers)
			r.Get("/triggers", h.EvaluateTriggers)
		})
		r.Route(prefix+"/transfer-decisions", func(r chi.Router) {
			r.Post("/", h.EvaluateTransfer)
			r.Get("/{decisionID}", h.GetDecision)
		})
	}

	mountRoutes("/privacy")
	mountRoutes("/v1/privacy")
}

// ── responses ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) storeError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrRelationshipNotFound):
		writeError(w, http.StatusNotFound, "processor relationship not found")
	case errors.Is(err, domain.ErrMechanismNotFound):
		writeError(w, http.StatusNotFound, "transfer mechanism not found")
	case errors.Is(err, domain.ErrDecisionNotFound):
		writeError(w, http.StatusNotFound, "transfer decision not found")
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
}

// ── pre-flight ───────────────────────────────────────────────────────────────

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

type callContext struct {
	Principal   string
	Tenant      string
	Correlation string
	IdemKey     string
}

func (cc *callContext) meta() store.Meta {
	return store.Meta{PrincipalID: cc.Principal, CorrelationID: cc.Correlation}
}

// begin runs pre-flight steps 1-5. On false the response has been written.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, actionType string, mutating bool) (*callContext, bool) {
	principal := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return nil, false
	}
	tenant := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context()))
	if tenant == "" {
		writeError(w, http.StatusUnauthorized, "X-Tenant-Id header is required")
		return nil, false
	}
	cc := &callContext{Principal: principal, Tenant: tenant}
	if mutating {
		cc.Correlation = strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
		if cc.Correlation == "" {
			writeError(w, http.StatusBadRequest, "X-Correlation-ID header is required")
			return nil, false
		}
		cc.IdemKey = idempotencyKey(r)
		if cc.IdemKey == "" {
			writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
			return nil, false
		}
	}
	if !h.authorize(w, r, principal, tenant, actionType) {
		return nil, false
	}
	return cc, true
}

// idempotencyKey is the ONE place the key is read, so the mandatory check and
// the replay/claim logic cannot disagree. X-Idempotency-Key is an alias.
func idempotencyKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Idempotency-Key"))
	}
	return key
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return []byte{}, true
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, true
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

// requestHash binds a key to method + path + body; the /v1 alias prefix is
// stripped so the same logical request through either mount hashes alike.
func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimPrefix(r.URL.Path, "/v1")))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// replayOrContinue answers an already-seen key (after authentication and
// authorization). A lookup failure refuses the request (503). handled=true
// means a response was written.
func (h *Handler) replayOrContinue(w http.ResponseWriter, r *http.Request, cc *callContext, hash string) bool {
	existing, err := h.store.GetIdempotency(r.Context(), cc.Tenant, cc.Principal, cc.IdemKey)
	if err != nil {
		h.log.Error("idempotency lookup failed — refusing the request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "idempotency store unavailable")
		return true
	}
	if existing != nil {
		writeReplay(w, existing, hash)
		return true
	}
	return false
}

func writeReplay(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string) {
	if existing.RequestHash != hash {
		writeError(w, http.StatusConflict, domain.ErrIdempotencyConflict.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotency-Replay", "true")
	w.WriteHeader(existing.ResponseCode)
	_, _ = w.Write(existing.ResponseBody)
}

// claim builds the atomic idempotency claim of one mutation.
func (h *Handler) claim(cc *callContext, endpoint, hash string, status int, render func(result any) any) *store.Idem {
	return &store.Idem{
		Key: cc.IdemKey, PrincipalID: cc.Principal, Endpoint: endpoint, RequestHash: hash,
		Render: func(result any) (int, []byte, error) {
			b, err := json.Marshal(render(result))
			return status, b, err
		},
	}
}

// respond writes the first-execution response, or the replay when a concurrent
// request won the claim.
func respond(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string, status int, v any) {
	if existing != nil {
		writeReplay(w, existing, hash)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func identity(res any) any { return res }

// tenantMismatch refuses a body tenant_id that disagrees with the verified one.
func tenantMismatch(w http.ResponseWriter, bodyTenant string, cc *callContext) bool {
	if bodyTenant != "" && bodyTenant != cc.Tenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return true
	}
	return false
}

// ── processor relationships ──────────────────────────────────────────────────

// CreateRelationship handles POST /privacy/processor-relationships.
// purpose_activity_refs, if supplied, are validated against a REAL call to
// privacy-purpose-registry-svc — each must resolve to ACTIVE.
func (h *Handler) CreateRelationship(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyTransferRelationshipManage, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.CreateProcessorRelationshipRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ControllerRef == "" || req.ProcessorRef == "" || req.Service == "" {
		writeError(w, http.StatusBadRequest, "controller_ref, processor_ref and service are required")
		return
	}
	if tenantMismatch(w, req.TenantID, cc) {
		return
	}

	for _, activityID := range req.PurposeActivityRefs {
		activity, err := h.purposes.ResolveActivity(r.Context(), cc.Tenant, cc.Principal, activityID)
		if err != nil {
			h.log.Error("CreateRelationship: purpose registry unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "purpose registry unavailable")
			return
		}
		if activity == nil || activity.VersionStatus != "ACTIVE" {
			writeError(w, http.StatusUnprocessableEntity, "purpose_activity_refs: "+activityID+" does not resolve to an ACTIVE processing activity")
			return
		}
	}

	rel, existing, err := h.store.CreateRelationship(r.Context(), cc.Tenant, req, cc.meta(), h.claim(cc, "CreateRelationship", hash, http.StatusCreated, identity))
	if err != nil {
		h.storeError(w, "CreateRelationship", err)
		return
	}
	respond(w, existing, hash, http.StatusCreated, rel)
}

func (h *Handler) GetRelationship(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferRelationshipManage, false); !ok {
		return
	}
	rel, err := h.store.FindRelationship(r.Context(), chi.URLParam(r, "relationshipID"))
	if err != nil {
		h.storeError(w, "GetRelationship", err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (h *Handler) ListRelationships(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferRelationshipManage, false); !ok {
		return
	}
	relationships, err := h.store.ListRelationships(r.Context())
	if err != nil {
		h.storeError(w, "ListRelationships", err)
		return
	}
	if relationships == nil {
		relationships = []domain.ProcessorRelationship{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": relationships, "count": len(relationships)})
}

func (h *Handler) UpdateRelationshipStatus(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	cc, ok := h.begin(w, r, PrivacyTransferRelationshipManage, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.UpdateRelationshipStatusRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Status.Valid() {
		writeError(w, http.StatusBadRequest, "status must be ACTIVE or INACTIVE")
		return
	}

	updated, existing, err := h.store.UpdateRelationshipStatus(r.Context(), relationshipID, req.Status, cc.meta(), h.claim(cc, "UpdateRelationshipStatus", hash, http.StatusOK, identity))
	if err != nil {
		h.storeError(w, "UpdateRelationshipStatus", err)
		return
	}
	respond(w, existing, hash, http.StatusOK, updated)
}

// ── subprocessors ────────────────────────────────────────────────────────────

func (h *Handler) AttachSubprocessor(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	cc, ok := h.begin(w, r, PrivacyTransferRelationshipManage, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.AttachSubprocessorRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProviderIdentity == "" || req.Service == "" {
		writeError(w, http.StatusBadRequest, "provider_identity and service are required")
		return
	}

	sp, existing, err := h.store.AttachSubprocessor(r.Context(), relationshipID, req, cc.meta(), h.claim(cc, "AttachSubprocessor", hash, http.StatusCreated, identity))
	if err != nil {
		h.storeError(w, "AttachSubprocessor", err)
		return
	}
	respond(w, existing, hash, http.StatusCreated, sp)
}

func (h *Handler) ListSubprocessors(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferRelationshipManage, false); !ok {
		return
	}
	subs, err := h.store.ListSubprocessors(r.Context(), chi.URLParam(r, "relationshipID"))
	if err != nil {
		h.storeError(w, "ListSubprocessors", err)
		return
	}
	if subs == nil {
		subs = []domain.Subprocessor{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": subs, "count": len(subs)})
}

// ── transfer mechanisms ──────────────────────────────────────────────────────

func (h *Handler) CreateMechanism(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyTransferMechanismManage, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.CreateTransferMechanismRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MechanismType == "" {
		writeError(w, http.StatusBadRequest, "mechanism_type is required")
		return
	}
	if req.ValidUntil != nil && req.ValidFrom != nil && req.ValidUntil.Before(*req.ValidFrom) {
		writeError(w, http.StatusBadRequest, "valid_until must not be before valid_from")
		return
	}
	if tenantMismatch(w, req.TenantID, cc) {
		return
	}

	mechanism, existing, err := h.store.CreateMechanism(r.Context(), cc.Tenant, req, cc.meta(), h.claim(cc, "CreateMechanism", hash, http.StatusCreated, identity))
	if err != nil {
		h.storeError(w, "CreateMechanism", err)
		return
	}
	respond(w, existing, hash, http.StatusCreated, mechanism)
}

func (h *Handler) GetMechanism(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferMechanismManage, false); !ok {
		return
	}
	mechanism, err := h.store.FindMechanism(r.Context(), chi.URLParam(r, "mechanismID"))
	if err != nil {
		h.storeError(w, "GetMechanism", err)
		return
	}
	writeJSON(w, http.StatusOK, mechanism)
}

// ── transfer assessments ─────────────────────────────────────────────────────

func (h *Handler) RecordAssessment(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyTransferAssessmentRecord, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.RecordTransferAssessmentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RelationshipID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id is required")
		return
	}
	if !req.Outcome.Valid() {
		writeError(w, http.StatusBadRequest, "outcome must be APPROVE, REMEDIATE or REJECT")
		return
	}
	// The relationship must exist in the caller's tenant (RLS-scoped lookup).
	if _, err := h.store.FindRelationship(r.Context(), req.RelationshipID); err != nil {
		h.storeError(w, "RecordAssessment", err)
		return
	}

	assessment, existing, err := h.store.RecordAssessment(r.Context(), cc.Tenant, req, cc.meta(), h.claim(cc, "RecordAssessment", hash, http.StatusCreated, identity))
	if err != nil {
		h.storeError(w, "RecordAssessment", err)
		return
	}
	respond(w, existing, hash, http.StatusCreated, assessment)
}

func (h *Handler) GetLatestAssessment(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferAssessmentRecord, false); !ok {
		return
	}
	relationshipID := r.URL.Query().Get("relationship_id")
	if relationshipID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id query parameter is required")
		return
	}
	assessment, err := h.store.FindLatestAssessment(r.Context(), relationshipID)
	if err != nil {
		h.storeError(w, "GetLatestAssessment", err)
		return
	}
	if assessment == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"relationship_id": relationshipID, "assessment": nil})
		return
	}
	writeJSON(w, http.StatusOK, assessment)
}

// ── transfer decisions ───────────────────────────────────────────────────────

// EvaluateTransfer handles POST /privacy/transfer-decisions — §16/§17's
// runtime evaluation. The decision, its privacy.transfer.authorization_changed
// event and the replay response are committed in one transaction.
func (h *Handler) EvaluateTransfer(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyTransferDecisionEvaluate, true)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	hash := requestHash(r, body)
	if h.replayOrContinue(w, r, cc, hash) {
		return
	}

	var req domain.EvaluateTransferRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RelationshipID == "" || req.TransferMechanismID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id and transfer_mechanism_id are required")
		return
	}
	if tenantMismatch(w, req.TenantID, cc) {
		return
	}
	if req.ReassessmentTrigger != "" && !domain.ValidTrigger(req.ReassessmentTrigger) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidTrigger.Error())
		return
	}

	// A decision is durable evidence that references the relationship and the
	// mechanism by foreign key, so one that names an id which does not exist (or
	// is not visible to this tenant) cannot be recorded. Answer with the
	// service's usual 404 for an unknown id rather than letting the foreign-key
	// violation surface as 503 "store unavailable". A lookup that fails for any
	// other reason falls through to evaluate, which fails closed as before.
	if _, err := h.store.FindRelationship(r.Context(), req.RelationshipID); errors.Is(err, domain.ErrRelationshipNotFound) {
		h.storeError(w, "EvaluateTransfer", err)
		return
	}
	if _, err := h.store.FindMechanism(r.Context(), req.TransferMechanismID); errors.Is(err, domain.ErrMechanismNotFound) {
		h.storeError(w, "EvaluateTransfer", err)
		return
	}

	decision := &domain.TransferDecision{
		RelationshipID: req.RelationshipID, TransferMechanismID: req.TransferMechanismID,
		DestinationJurisdiction: req.DestinationJurisdiction, ActorPrincipalID: cc.Principal, CorrelationID: cc.Correlation,
		// §16.1 TransferContext — recorded verbatim as authorization
		// evidence. Whitespace-only declarations normalise to "absent".
		ExporterRole:         strings.TrimSpace(req.ExporterRole),
		ImporterRole:         strings.TrimSpace(req.ImporterRole),
		OriginJurisdiction:   strings.TrimSpace(req.OriginJurisdiction),
		RouteProvider:        strings.TrimSpace(req.RouteProvider),
		FrequencyVolumeClass: strings.TrimSpace(req.FrequencyVolumeClass),
	}

	result, reasonCodes := h.evaluate(r.Context(), &req, decision)
	decision.Result = result
	decision.ReasonCodes = reasonCodes

	render := func(res any) any { return res }
	existing, err := h.store.RecordDecision(r.Context(), cc.Tenant, decision, cc.meta(), h.claim(cc, "EvaluateTransfer", hash, http.StatusOK, render))
	if err != nil {
		h.storeError(w, "EvaluateTransfer", err)
		return
	}
	// §18 critical response field authorization_id is always present.
	if decision.AuthorizationID == "" {
		decision.AuthorizationID = decision.DecisionID
	}
	respond(w, existing, hash, http.StatusOK, decision)
}

// evaluate is §17.2's fail-closed doctrine encoded literally: "If a required
// DPIA/TIA, transfer mechanism... approval is missing, expired or conflicted,
// PRV-05 returns BLOCKED or REVIEW_REQUIRED." Any dependency being
// unreachable ALSO fails closed, as REVIEW_REQUIRED.
func (h *Handler) evaluate(ctx context.Context, req *domain.EvaluateTransferRequest, decision *domain.TransferDecision) (domain.AuthorizationResult, []string) {
	relationship, err := h.store.FindRelationship(ctx, req.RelationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			return domain.ResultBlocked, makeReasons(domain.ReasonRelationshipNotActive, "PROCESSOR_RELATIONSHIP_NOT_ACTIVE")
		}
		h.log.Error("evaluate: relationship lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, makeReasons(domain.ReasonDependencyUnavailable, "DEPENDENCY_UNAVAILABLE")
	}
	if relationship.Status != domain.RelationshipActive {
		return domain.ResultBlocked, makeReasons(domain.ReasonRelationshipNotActive, "PROCESSOR_RELATIONSHIP_NOT_ACTIVE")
	}
	if strings.TrimSpace(relationship.ProcessingInstructions) == "" {
		return domain.ResultBlocked, makeReasons(domain.ReasonInstructionMissing, "PROCESSOR_INSTRUCTION_MISSING")
	}

	// §17.2 — a subprocessor with no approval model on file is an unapproved
	// subprocessor: BLOCKED with PRV-018 (OD-10's capability matrix is still
	// open, so the safer explicit state is used).
	subprocessors, err := h.store.ListSubprocessors(ctx, req.RelationshipID)
	if err != nil {
		h.log.Error("evaluate: subprocessor lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, makeReasons(domain.ReasonDependencyUnavailable, "DEPENDENCY_UNAVAILABLE")
	}
	for _, sp := range subprocessors {
		if strings.TrimSpace(sp.NotificationApprovalModel) == "" {
			return domain.ResultBlocked, makeReasons(domain.ReasonSubprocessorNotApproved, "SUBPROCESSOR_NOT_APPROVED")
		}
	}

	mechanism, err := h.store.FindMechanism(ctx, req.TransferMechanismID)
	if err != nil {
		if errors.Is(err, domain.ErrMechanismNotFound) {
			return domain.ResultBlocked, makeReasons(domain.ReasonMechanismNotFound, "TRANSFER_MECHANISM_NOT_FOUND")
		}
		h.log.Error("evaluate: mechanism lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, makeReasons(domain.ReasonDependencyUnavailable, "DEPENDENCY_UNAVAILABLE")
	}
	now := time.Now().UTC()
	if !mechanism.ValidAsOf(now) {
		return domain.ResultBlocked, makeReasons(domain.ReasonMechanismExpired, "TRANSFER_MECHANISM_INVALID_OR_EXPIRED")
	}
	if mechanism.ValidUntil != nil {
		decision.ExpiresAt = mechanism.ValidUntil
	}

	// The latest assessment is trigger EVIDENCE (§17.1 triggers 3, 4 and 8 are
	// measured against it) and is therefore read whether or not the caller
	// declared an assessment required — otherwise this evaluation and the
	// trigger endpoint would answer differently for the same stored evidence.
	// assessment_required only controls whether the assessment GATE below
	// (missing / rejected / remediate / expired) is enforced and whether the
	// assessment's measures and review date enter the decision. A lookup failure
	// fails closed either way.
	triggerAssessment, err := h.store.FindLatestAssessment(ctx, req.RelationshipID)
	if err != nil {
		h.log.Error("evaluate: assessment lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, makeReasons(domain.ReasonDependencyUnavailable, "DEPENDENCY_UNAVAILABLE")
	}

	var latestAssessment *domain.TransferAssessment
	if req.AssessmentRequired {
		assessment := triggerAssessment
		if assessment == nil {
			return domain.ResultReviewRequired, makeReasons(domain.ReasonAssessmentMissing, "ASSESSMENT_REQUIRED_NOT_FOUND")
		}
		decision.AssessmentID = &assessment.AssessmentID
		latestAssessment = assessment

		switch assessment.Outcome {
		case domain.AssessmentReject:
			return domain.ResultBlocked, makeReasons(domain.ReasonAssessmentRejected, "ASSESSMENT_REJECTED")
		case domain.AssessmentRemediate:
			return domain.ResultReviewRequired, makeReasons(domain.ReasonAssessmentRemediate, "ASSESSMENT_REQUIRES_REMEDIATION")
		}

		// §17.1 trigger 8: expiry / review date reached.
		if assessment.ExpiredAsOf(now) {
			return domain.ResultReviewRequired, makeReasons(domain.ReasonAssessmentExpired, "ASSESSMENT_EXPIRED")
		}
		if assessment.ReviewTriggerAt != nil {
			if decision.ExpiresAt == nil || assessment.ReviewTriggerAt.Before(*decision.ExpiresAt) {
				decision.ExpiresAt = assessment.ReviewTriggerAt
			}
		}
	}

	// §17.1 reassessment triggers — the same evaluation the trigger endpoint
	// uses. Triggers established from stored evidence win over a declaration.
	outcomes := computeTriggers(triggerInput{
		Relationship: relationship, Mechanism: mechanism, Assessment: triggerAssessment, Subprocessors: subprocessors,
		Destination: req.DestinationJurisdiction, DataCats: req.DataCategories, SubjectCls: req.SubjectClasses,
		Declared: "", Now: now,
	})
	// changed mechanism (4), new processor/jurisdiction (3), minors (6), sensitive
	// category (1), expiry/review date reached (8). With assessment_required the
	// expiry already returned above, so trigger 8 only decides here when it was not.
	for _, idx := range []int{3, 2, 5, 0, 7} {
		if o := outcomes[idx]; o.Derived && o.Triggered {
			return domain.ResultReviewRequired, makeReasons(o.ReasonCode, o.Trigger)
		}
	}
	if req.ReassessmentTrigger != "" {
		return domain.ResultReviewRequired, makeReasons("PRV-016: REASSESSMENT_TRIGGER_"+req.ReassessmentTrigger, req.ReassessmentTrigger)
	}

	// §16.1 & §18: CONDITIONAL vs AUTHORIZED.
	var conds []string
	if latestAssessment != nil {
		if latestAssessment.TechnicalMeasures != "" {
			conds = append(conds, "Technical: "+latestAssessment.TechnicalMeasures)
		}
		if latestAssessment.OrganizationalMeasures != "" {
			conds = append(conds, "Organizational: "+latestAssessment.OrganizationalMeasures)
		}
	}
	if req.Conditions != "" {
		conds = append(conds, req.Conditions)
	}
	if req.EnforceConditions && mechanism.Conditions != "" {
		conds = append(conds, mechanism.Conditions)
	}
	if len(conds) > 0 {
		decision.Conditions = strings.Join(conds, "; ")
		return domain.ResultConditional, []string{}
	}
	return domain.ResultAuthorized, []string{}
}

func (h *Handler) GetDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferDecisionEvaluate, false); !ok {
		return
	}
	d, err := h.store.FindDecision(r.Context(), chi.URLParam(r, "decisionID"))
	if err != nil {
		h.storeError(w, "GetDecision", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// EvaluateTriggers evaluates the eight mandatory reassessment triggers (§17.1)
// for a relationship. It writes nothing; it is authenticated, tenant-scoped
// and authorized like every other route, and a trigger the caller declares is
// validated against the eight §17.1 names.
func (h *Handler) EvaluateTriggers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, PrivacyTransferDecisionEvaluate, false); !ok {
		return
	}
	var req domain.EvaluateTriggersRequest
	if r.Method == http.MethodPost {
		if body, ok := readBody(w, r); !ok {
			return
		} else if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid request body")
				return
			}
		}
	}
	q := r.URL.Query()
	if req.RelationshipID == "" {
		req.RelationshipID = q.Get("relationship_id")
	}
	if req.RelationshipID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id is required")
		return
	}
	if req.TransferMechanismID == "" {
		req.TransferMechanismID = q.Get("transfer_mechanism_id")
	}
	if req.DestinationJurisdiction == "" {
		req.DestinationJurisdiction = q.Get("destination_jurisdiction")
	}
	if req.DeclaredTrigger == "" {
		req.DeclaredTrigger = q.Get("declared_trigger")
	}
	if req.DeclaredTrigger != "" && !domain.ValidTrigger(req.DeclaredTrigger) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidTrigger.Error())
		return
	}

	relationship, err := h.store.FindRelationship(r.Context(), req.RelationshipID)
	if err != nil {
		h.storeError(w, "EvaluateTriggers", err)
		return
	}
	// Evidence lookups fail closed: an outage is an error, never "no trigger".
	assessment, err := h.store.FindLatestAssessment(r.Context(), req.RelationshipID)
	if err != nil {
		h.storeError(w, "EvaluateTriggers", err)
		return
	}
	subs, err := h.store.ListSubprocessors(r.Context(), req.RelationshipID)
	if err != nil {
		h.storeError(w, "EvaluateTriggers", err)
		return
	}
	var mechanism *domain.TransferMechanism
	if req.TransferMechanismID != "" {
		mechanism, err = h.store.FindMechanism(r.Context(), req.TransferMechanismID)
		if err != nil && !errors.Is(err, domain.ErrMechanismNotFound) {
			h.storeError(w, "EvaluateTriggers", err)
			return
		}
	}

	outcomes := computeTriggers(triggerInput{
		Relationship: relationship, Mechanism: mechanism, Assessment: assessment, Subprocessors: subs,
		Destination: req.DestinationJurisdiction, DataCats: req.DataCategories, SubjectCls: req.SubjectClasses,
		Declared: req.DeclaredTrigger, Now: time.Now().UTC(),
	})
	evals := make([]domain.TriggerEvaluation, 0, len(outcomes))
	active := []string{}
	for _, o := range outcomes {
		evals = append(evals, o.TriggerEvaluation)
		if o.Triggered {
			active = append(active, o.Trigger)
		}
	}
	writeJSON(w, http.StatusOK, domain.EvaluateTriggersResponse{
		RelationshipID:     req.RelationshipID,
		ReassessmentNeeded: len(active) > 0,
		ActiveTriggers:     active,
		TriggerEvaluations: evals,
	})
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(item)) {
			return true
		}
	}
	return false
}

func makeReasons(code, symbol string) []string {
	if code == symbol || symbol == "" {
		return []string{code}
	}
	return []string{code, symbol}
}
