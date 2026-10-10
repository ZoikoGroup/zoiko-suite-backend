// Package handler exposes privacy-transfer-svc's REST API — PRV-05.
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
	"zoiko.io/privacy-transfer-svc/internal/events"
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
	// Evaluating a transfer authorization is a separate capability: it reads
	// those records and writes a durable, event-published decision, but
	// does not by itself let the caller manage any of them.
	PrivacyTransferDecisionEvaluate = "PRIVACY_TRANSFER_DECISION_EVALUATE"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real dependency on PRV-01 — see the domain
// package's doc comment on why purpose_activity_refs are validated, not
// trusted as opaque strings.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, tenantID, activityID string) (*purposeregistry.ActivityVersion, error)
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	purposes PurposeChecker
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, purposes PurposeChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, purposes: purposes, log: log}
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (*domain.IdempotencyRecord, bool, string, string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = r.Header.Get("X-Idempotency-Key")
	}
	if key == "" {
		return nil, false, "", ""
	}
	reqHash := hashBody(body)
	existing, err := h.store.GetIdempotency(r.Context(), tenantID, key)
	if err != nil {
		h.log.Warn("idempotency lookup error", zap.Error(err))
		return nil, false, key, reqHash
	}
	if existing != nil {
		if existing.RequestHash != reqHash {
			writeError(w, http.StatusConflict, domain.ErrIdempotencyConflict.Error())
			return existing, true, key, reqHash
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replay", "true")
		w.WriteHeader(existing.ResponseCode)
		_, _ = w.Write(existing.ResponseBody)
		return existing, true, key, reqHash
	}
	return nil, false, key, reqHash
}

func (h *Handler) writeJSONWithIdempotency(ctx context.Context, w http.ResponseWriter, tenantID, principalID, endpoint, key, reqHash string, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal response")
		return
	}
	if key != "" {
		_ = h.store.SaveIdempotency(ctx, domain.IdempotencyRecord{
			IdempotencyKey: key,
			TenantID:       tenantID,
			Endpoint:       endpoint,
			RequestHash:    reqHash,
			ResponseCode:   status,
			ResponseBody:   body,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
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

// ── processor relationships ──────────────────────────────────────────────────

// CreateRelationship handles POST /privacy/processor-relationships.
// purpose_activity_refs, if supplied, are validated against a REAL call
// to privacy-purpose-registry-svc — each must resolve to ACTIVE.
func (h *Handler) CreateRelationship(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreateProcessorRelationshipRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ControllerRef == "" || req.ProcessorRef == "" || req.Service == "" {
		writeError(w, http.StatusBadRequest, "controller_ref, processor_ref and service are required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferRelationshipManage) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	for _, activityID := range req.PurposeActivityRefs {
		activity, err := h.purposes.ResolveActivity(r.Context(), tenantID, activityID)
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

	relationship, err := h.store.CreateRelationship(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateRelationship: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.processor_relationship.created", EntityID: relationship.RelationshipID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: relationship,
	})
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreateRelationship", idemKey, reqHash, http.StatusCreated, relationship)
}

func (h *Handler) GetRelationship(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	relationship, err := h.store.FindRelationship(r.Context(), relationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("GetRelationship: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, relationship)
}

func (h *Handler) ListRelationships(w http.ResponseWriter, r *http.Request) {
	relationships, err := h.store.ListRelationships(r.Context())
	if err != nil {
		h.log.Error("ListRelationships: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if relationships == nil {
		relationships = []domain.ProcessorRelationship{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": relationships, "count": len(relationships)})
}

func (h *Handler) UpdateRelationshipStatus(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.UpdateRelationshipStatusRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Status.Valid() {
		writeError(w, http.StatusBadRequest, "status must be ACTIVE or INACTIVE")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, err := h.store.FindRelationship(r.Context(), relationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("UpdateRelationshipStatus: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferRelationshipManage) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	updated, err := h.store.UpdateRelationshipStatus(r.Context(), relationshipID, req.Status)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("UpdateRelationshipStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "UpdateRelationshipStatus", idemKey, reqHash, http.StatusOK, updated)
}

// ── subprocessors ────────────────────────────────────────────────────────────

func (h *Handler) AttachSubprocessor(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.AttachSubprocessorRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProviderIdentity == "" || req.Service == "" {
		writeError(w, http.StatusBadRequest, "provider_identity and service are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	existing, err := h.store.FindRelationship(r.Context(), relationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("AttachSubprocessor: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferRelationshipManage) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	sp, err := h.store.AttachSubprocessor(r.Context(), relationshipID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("AttachSubprocessor: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "AttachSubprocessor", idemKey, reqHash, http.StatusCreated, sp)
}

func (h *Handler) ListSubprocessors(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	subs, err := h.store.ListSubprocessors(r.Context(), relationshipID)
	if err != nil {
		h.log.Error("ListSubprocessors: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if subs == nil {
		subs = []domain.Subprocessor{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": subs, "count": len(subs)})
}

// ── transfer mechanisms ──────────────────────────────────────────────────────

func (h *Handler) CreateMechanism(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.CreateTransferMechanismRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
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

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferMechanismManage) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	mechanism, err := h.store.CreateMechanism(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateMechanism: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreateMechanism", idemKey, reqHash, http.StatusCreated, mechanism)
}

func (h *Handler) GetMechanism(w http.ResponseWriter, r *http.Request) {
	mechanismID := chi.URLParam(r, "mechanismID")
	mechanism, err := h.store.FindMechanism(r.Context(), mechanismID)
	if err != nil {
		if errors.Is(err, domain.ErrMechanismNotFound) {
			writeError(w, http.StatusNotFound, "transfer mechanism not found")
			return
		}
		h.log.Error("GetMechanism: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, mechanism)
}

// ── transfer assessments ─────────────────────────────────────────────────────

func (h *Handler) RecordAssessment(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.RecordTransferAssessmentRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	relationship, err := h.store.FindRelationship(r.Context(), req.RelationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		h.log.Error("RecordAssessment: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if relationship.TenantID != nil {
		tenantID = *relationship.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferAssessmentRecord) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	assessment, err := h.store.RecordAssessment(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("RecordAssessment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "RecordAssessment", idemKey, reqHash, http.StatusCreated, assessment)
}

func (h *Handler) GetLatestAssessment(w http.ResponseWriter, r *http.Request) {
	relationshipID := r.URL.Query().Get("relationship_id")
	if relationshipID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id query parameter is required")
		return
	}
	assessment, err := h.store.FindLatestAssessment(r.Context(), relationshipID)
	if err != nil {
		h.log.Error("GetLatestAssessment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
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
// runtime evaluation, scoped to what this version can determine from
// real evidence. See the domain package's doc comment for exactly which
// checks are implemented (relationship active, mechanism valid, and an
// opt-in assessment check) and which (CONDITIONAL's machine-enforceable
// constraints) are not.
func (h *Handler) EvaluateTransfer(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req domain.EvaluateTransferRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RelationshipID == "" || req.TransferMechanismID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id and transfer_mechanism_id are required")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	if !h.authorize(w, r, principalID, tenantID, PrivacyTransferDecisionEvaluate) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	decision := &domain.TransferDecision{
		RelationshipID: req.RelationshipID, TransferMechanismID: req.TransferMechanismID,
		DestinationJurisdiction: req.DestinationJurisdiction, ActorPrincipalID: principalID, CorrelationID: correlationID,
	}

	result, reasonCodes := h.evaluate(r.Context(), &req, decision)
	decision.Result = result
	decision.ReasonCodes = reasonCodes

	if err := h.store.RecordDecision(r.Context(), tenantID, decision); err != nil {
		h.log.Error("EvaluateTransfer: failed to record decision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.transfer_decision.evaluated", EntityID: decision.DecisionID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: correlationID, Payload: decision,
	})
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "EvaluateTransfer", idemKey, reqHash, http.StatusOK, decision)
}

// evaluate is §17.2's fail-closed doctrine encoded literally: "If a
// required DPIA/TIA, transfer mechanism... approval is missing, expired
// or conflicted, PRV-05 returns BLOCKED or REVIEW_REQUIRED." Any
// dependency being unreachable ALSO fails closed, as REVIEW_REQUIRED —
// unlike PRV-03's INDETERMINATE-only posture, this service has a defined
// "needs a human" outcome the spec itself names for exactly this
// situation, so an unreachable dependency routes there rather than to a
// separate ad hoc state.
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

	var latestAssessment *domain.TransferAssessment
	if req.AssessmentRequired {
		assessment, err := h.store.FindLatestAssessment(ctx, req.RelationshipID)
		if err != nil {
			h.log.Error("evaluate: assessment lookup failed", zap.Error(err))
			return domain.ResultReviewRequired, makeReasons(domain.ReasonDependencyUnavailable, "DEPENDENCY_UNAVAILABLE")
		}
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

		// §17.1 Mandatory Reassessment Trigger 8: Expiry review date reached
		if assessment.ExpiredAsOf(now) {
			return domain.ResultReviewRequired, makeReasons(domain.ReasonAssessmentExpired, "ASSESSMENT_EXPIRED")
		}
		if assessment.ReviewTriggerAt != nil {
			if decision.ExpiresAt == nil || assessment.ReviewTriggerAt.Before(*decision.ExpiresAt) {
				decision.ExpiresAt = assessment.ReviewTriggerAt
			}
		}

		// §17.1 Mandatory Reassessment Trigger 4: Changed transfer mechanism since assessment
		if mechanism.CreatedAt.After(assessment.CreatedAt) {
			return domain.ResultReviewRequired, makeReasons("PRV-016: REASSESSMENT_TRIGGER_CHANGED_TRANSFER_MECHANISM", "CHANGED_TRANSFER_MECHANISM_OR_INSTRUCTION")
		}
	}

	// §17.1 Mandatory Reassessment Trigger 3: New destination jurisdiction outside declared scope
	if req.DestinationJurisdiction != "" && len(relationship.Jurisdictions) > 0 && !contains(relationship.Jurisdictions, req.DestinationJurisdiction) {
		return domain.ResultReviewRequired, makeReasons("PRV-016: REASSESSMENT_TRIGGER_NEW_DESTINATION_JURISDICTION", "NEW_PROCESSOR_OR_DESTINATION_JURISDICTION")
	}

	// §17.1 Mandatory Reassessment Trigger 6: New minors context
	if (contains(req.SubjectClasses, "MINORS") || contains(req.SubjectClasses, "CHILDREN")) && !contains(relationship.SubjectClasses, "MINORS") && !contains(relationship.SubjectClasses, "CHILDREN") {
		return domain.ResultReviewRequired, makeReasons("PRV-016: REASSESSMENT_TRIGGER_NEW_MINORS_CONTEXT", "NEW_MINORS_CONTEXT")
	}

	// §17.1 Mandatory Reassessment Trigger 1/2/5/7: Explicit declared trigger
	if req.ReassessmentTrigger != "" {
		code := "PRV-016: REASSESSMENT_TRIGGER_" + req.ReassessmentTrigger
		return domain.ResultReviewRequired, makeReasons(code, req.ReassessmentTrigger)
	}

	// §16.1 & §18: Evaluate CONDITIONAL outcome vs AUTHORIZED
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
	decisionID := chi.URLParam(r, "decisionID")
	d, err := h.store.FindDecision(r.Context(), decisionID)
	if err != nil {
		if errors.Is(err, domain.ErrDecisionNotFound) {
			writeError(w, http.StatusNotFound, "transfer decision not found")
			return
		}
		h.log.Error("GetDecision: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// EvaluateTriggers evaluates all 8 mandatory reassessment triggers (§17.1) for a relationship.
func (h *Handler) EvaluateTriggers(w http.ResponseWriter, r *http.Request) {
	var req domain.EvaluateTriggersRequest
	if r.Method == http.MethodPost {
		bodyBytes, err := readBody(r)
		if err == nil && len(bodyBytes) > 0 {
			_ = json.Unmarshal(bodyBytes, &req)
		}
	}
	if req.RelationshipID == "" {
		req.RelationshipID = r.URL.Query().Get("relationship_id")
	}
	if req.RelationshipID == "" {
		writeError(w, http.StatusBadRequest, "relationship_id is required")
		return
	}
	if req.TransferMechanismID == "" {
		req.TransferMechanismID = r.URL.Query().Get("transfer_mechanism_id")
	}
	if req.DestinationJurisdiction == "" {
		req.DestinationJurisdiction = r.URL.Query().Get("destination_jurisdiction")
	}
	if req.DeclaredTrigger == "" {
		req.DeclaredTrigger = r.URL.Query().Get("declared_trigger")
	}

	relationship, err := h.store.FindRelationship(r.Context(), req.RelationshipID)
	if err != nil {
		if errors.Is(err, domain.ErrRelationshipNotFound) {
			writeError(w, http.StatusNotFound, "processor relationship not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	assessment, _ := h.store.FindLatestAssessment(r.Context(), req.RelationshipID)

	var mechanism *domain.TransferMechanism
	if req.TransferMechanismID != "" {
		mechanism, _ = h.store.FindMechanism(r.Context(), req.TransferMechanismID)
	}

	now := time.Now().UTC()
	var evals []domain.TriggerEvaluation
	var activeTriggers []string

	// Trigger 1: New purpose or sensitive category
	t1 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerNewPurposeOrSensitiveCategory,
		Description: "New purpose, materially expanded purpose or new sensitive/special data category",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_NEW_PURPOSE_OR_SENSITIVE_CATEGORY",
	}
	for _, cat := range req.DataCategories {
		if containsSensitive(cat) && !contains(relationship.DataCategories, cat) {
			t1.Triggered = true
			break
		}
	}
	if req.DeclaredTrigger == domain.TriggerNewPurposeOrSensitiveCategory {
		t1.Triggered = true
	}
	if t1.Triggered {
		activeTriggers = append(activeTriggers, t1.Trigger)
	}
	evals = append(evals, t1)

	// Trigger 2: New automated decisioning
	t2 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerAutomatedDecisioning,
		Description: "New automated decisioning/profiling or materially increased effect on individuals",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_AUTOMATED_DECISIONING",
		Triggered:   req.DeclaredTrigger == domain.TriggerAutomatedDecisioning,
	}
	if t2.Triggered {
		activeTriggers = append(activeTriggers, t2.Trigger)
	}
	evals = append(evals, t2)

	// Trigger 3: New processor or destination jurisdiction
	t3 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerNewProcessorOrJurisdiction,
		Description: "New processor/subprocessor or destination jurisdiction/region",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_NEW_DESTINATION_JURISDICTION",
	}
	if req.DestinationJurisdiction != "" && len(relationship.Jurisdictions) > 0 && !contains(relationship.Jurisdictions, req.DestinationJurisdiction) {
		t3.Triggered = true
	}
	if req.DeclaredTrigger == domain.TriggerNewProcessorOrJurisdiction {
		t3.Triggered = true
	}
	if t3.Triggered {
		activeTriggers = append(activeTriggers, t3.Trigger)
	}
	evals = append(evals, t3)

	// Trigger 4: Changed transfer mechanism or instruction
	t4 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerChangedTransferMechanism,
		Description: "Changed transfer mechanism, legal validity, processing instruction or recipient category",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_CHANGED_TRANSFER_MECHANISM",
	}
	if mechanism != nil && assessment != nil && mechanism.CreatedAt.After(assessment.CreatedAt) {
		t4.Triggered = true
	}
	if req.DeclaredTrigger == domain.TriggerChangedTransferMechanism {
		t4.Triggered = true
	}
	if t4.Triggered {
		activeTriggers = append(activeTriggers, t4.Trigger)
	}
	evals = append(evals, t4)

	// Trigger 5: Material architecture change
	t5 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerMaterialArchitectureChange,
		Description: "Material architecture change affecting data access, observability, model training or re-identification risk",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_MATERIAL_ARCHITECTURE_CHANGE",
		Triggered:   req.DeclaredTrigger == domain.TriggerMaterialArchitectureChange,
	}
	if t5.Triggered {
		activeTriggers = append(activeTriggers, t5.Trigger)
	}
	evals = append(evals, t5)

	// Trigger 6: New minors context
	t6 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerNewMinorsContext,
		Description: "New children/minors context or materially changed age-assurance model",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_NEW_MINORS_CONTEXT",
	}
	if (contains(req.SubjectClasses, "MINORS") || contains(req.SubjectClasses, "CHILDREN")) && !contains(relationship.SubjectClasses, "MINORS") && !contains(relationship.SubjectClasses, "CHILDREN") {
		t6.Triggered = true
	}
	if req.DeclaredTrigger == domain.TriggerNewMinorsContext {
		t6.Triggered = true
	}
	if t6.Triggered {
		activeTriggers = append(activeTriggers, t6.Trigger)
	}
	evals = append(evals, t6)

	// Trigger 7: Security or privacy incident
	t7 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerSecurityPrivacyIncident,
		Description: "Privacy/security incident revealing an unmodeled risk or control failure",
		ReasonCode:  "PRV-016: REASSESSMENT_TRIGGER_SECURITY_PRIVACY_INCIDENT",
		Triggered:   req.DeclaredTrigger == domain.TriggerSecurityPrivacyIncident,
	}
	if t7.Triggered {
		activeTriggers = append(activeTriggers, t7.Trigger)
	}
	evals = append(evals, t7)

	// Trigger 8: Expiry review date reached
	t8 := domain.TriggerEvaluation{
		Trigger:     domain.TriggerExpiryReviewDateReached,
		Description: "Expiry/review date reached or PDC legal-rule package marks prior authorization stale",
		ReasonCode:  "PRV-016: ASSESSMENT_EXPIRED",
	}
	if assessment != nil && assessment.ExpiredAsOf(now) {
		t8.Triggered = true
	}
	if req.DeclaredTrigger == domain.TriggerExpiryReviewDateReached {
		t8.Triggered = true
	}
	if t8.Triggered {
		activeTriggers = append(activeTriggers, t8.Trigger)
	}
	evals = append(evals, t8)

	resp := domain.EvaluateTriggersResponse{
		RelationshipID:     req.RelationshipID,
		ReassessmentNeeded: len(activeTriggers) > 0,
		ActiveTriggers:     activeTriggers,
		TriggerEvaluations: evals,
	}
	writeJSON(w, http.StatusOK, resp)
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}

func containsSensitive(cat string) bool {
	upper := strings.ToUpper(cat)
	return strings.Contains(upper, "HEALTH") ||
		strings.Contains(upper, "BIOMETRIC") ||
		strings.Contains(upper, "GENETIC") ||
		strings.Contains(upper, "SPECIAL") ||
		strings.Contains(upper, "CRIMINAL") ||
		strings.Contains(upper, "SEXUAL") ||
		strings.Contains(upper, "RELIGIOUS") ||
		strings.Contains(upper, "POLITICAL")
}

func makeReasons(code, symbol string) []string {
	if code == symbol || symbol == "" {
		return []string{code}
	}
	return []string{code, symbol}
}
