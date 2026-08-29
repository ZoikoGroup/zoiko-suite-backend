// Package handler exposes privacy-transfer-svc's REST API — PRV-05.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// PurposeChecker is the real dependency on PRV-01 — see the domain
// package's doc comment on why purpose_activity_refs are validated, not
// trusted as opaque strings.
type PurposeChecker interface {
	ResolveActivity(ctx context.Context, activityID string) (*purposeregistry.ActivityVersion, error)
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
	r.Route("/privacy/processor-relationships", func(r chi.Router) {
		r.Post("/", h.CreateRelationship)
		r.Get("/", h.ListRelationships)
		r.Get("/{relationshipID}", h.GetRelationship)
		r.Post("/{relationshipID}/status", h.UpdateRelationshipStatus)
		r.Post("/{relationshipID}/subprocessors", h.AttachSubprocessor)
		r.Get("/{relationshipID}/subprocessors", h.ListSubprocessors)
	})
	r.Route("/privacy/transfer-mechanisms", func(r chi.Router) {
		r.Post("/", h.CreateMechanism)
		r.Get("/{mechanismID}", h.GetMechanism)
	})
	r.Route("/privacy/transfer-assessments", func(r chi.Router) {
		r.Post("/", h.RecordAssessment)
		r.Get("/", h.GetLatestAssessment)
	})
	r.Route("/privacy/transfer-decisions", func(r chi.Router) {
		r.Post("/", h.EvaluateTransfer)
		r.Get("/{decisionID}", h.GetDecision)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
	var req domain.CreateProcessorRelationshipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	for _, activityID := range req.PurposeActivityRefs {
		activity, err := h.purposes.ResolveActivity(r.Context(), activityID)
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
	writeJSON(w, http.StatusCreated, relationship)
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
	var req domain.UpdateRelationshipStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	writeJSON(w, http.StatusOK, updated)
}

// ── subprocessors ────────────────────────────────────────────────────────────

func (h *Handler) AttachSubprocessor(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationshipID")
	var req domain.AttachSubprocessorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	writeJSON(w, http.StatusCreated, sp)
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
	var req domain.CreateTransferMechanismRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	mechanism, err := h.store.CreateMechanism(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateMechanism: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, mechanism)
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
	var req domain.RecordTransferAssessmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	assessment, err := h.store.RecordAssessment(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("RecordAssessment: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, assessment)
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

	var req domain.EvaluateTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	writeJSON(w, http.StatusOK, decision)
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
			return domain.ResultBlocked, []string{domain.ReasonRelationshipNotActive}
		}
		h.log.Error("evaluate: relationship lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, []string{domain.ReasonDependencyUnavailable}
	}
	if relationship.Status != domain.RelationshipActive {
		return domain.ResultBlocked, []string{domain.ReasonRelationshipNotActive}
	}

	mechanism, err := h.store.FindMechanism(ctx, req.TransferMechanismID)
	if err != nil {
		if errors.Is(err, domain.ErrMechanismNotFound) {
			return domain.ResultBlocked, []string{domain.ReasonMechanismNotFound}
		}
		h.log.Error("evaluate: mechanism lookup failed", zap.Error(err))
		return domain.ResultReviewRequired, []string{domain.ReasonDependencyUnavailable}
	}
	now := time.Now().UTC()
	if !mechanism.ValidAsOf(now) {
		return domain.ResultBlocked, []string{domain.ReasonMechanismExpired}
	}

	if req.AssessmentRequired {
		assessment, err := h.store.FindLatestAssessment(ctx, req.RelationshipID)
		if err != nil {
			h.log.Error("evaluate: assessment lookup failed", zap.Error(err))
			return domain.ResultReviewRequired, []string{domain.ReasonDependencyUnavailable}
		}
		if assessment == nil {
			return domain.ResultReviewRequired, []string{domain.ReasonAssessmentMissing}
		}
		decision.AssessmentID = &assessment.AssessmentID
		switch assessment.Outcome {
		case domain.AssessmentReject:
			return domain.ResultBlocked, []string{domain.ReasonAssessmentRejected}
		case domain.AssessmentRemediate:
			return domain.ResultReviewRequired, []string{domain.ReasonAssessmentRemediate}
		}
		if assessment.ExpiredAsOf(now) {
			return domain.ResultReviewRequired, []string{domain.ReasonAssessmentExpired}
		}
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
