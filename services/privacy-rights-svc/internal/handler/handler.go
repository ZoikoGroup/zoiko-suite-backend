// Package handler exposes privacy-rights-svc's REST API — PRV-04.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-rights-svc/internal/authz"
	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/events"
	svcmiddleware "zoiko.io/privacy-rights-svc/internal/middleware"
	"zoiko.io/privacy-rights-svc/internal/store"
)

const (
	PrivacyRightsRequestCreate  = "PRIVACY_RIGHTS_REQUEST_CREATE"
	PrivacyRightsRequestProcess = "PRIVACY_RIGHTS_REQUEST_PROCESS"
	PrivacyRightsRequestClose   = "PRIVACY_RIGHTS_REQUEST_CLOSE"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store store.Store
	pub   events.Publisher
	authz AuthzChecker
	log   *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/privacy/rights-requests", func(r chi.Router) {
		r.Post("/", h.CreateRequest)
		r.Get("/", h.ListRequestsBySubject)
		r.Get("/{requestID}", h.GetRequest)
		r.Post("/{requestID}/identity-verification", h.RecordIdentityVerification)
		r.Post("/{requestID}/discovery-manifests", h.AttachDiscoveryManifest)
		r.Get("/{requestID}/discovery-manifests", h.ListDiscoveryManifests)
		r.Post("/{requestID}/wfc-process-ref", h.AttachWFCProcessRef)
		r.Post("/{requestID}/close", h.CloseRequest)
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

// CreateRequest handles POST /privacy/rights-requests — case intake.
// This is PRV-04's OWN privacy-meaning record; it does not create a
// workflow-svc instance (see the domain package's doc comment on
// wfc_process_ref for why).
func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRightsRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SubjectRef == "" {
		writeError(w, http.StatusBadRequest, "subject_ref is required")
		return
	}
	if !req.RightFamily.Valid() {
		writeError(w, http.StatusBadRequest, "right_family is missing or not a recognized value")
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
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestCreate) {
		return
	}

	request, err := h.store.CreateRequest(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.rights_request.received", EntityID: request.RequestID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: request,
	})
	writeJSON(w, http.StatusCreated, request)
}

func (h *Handler) GetRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	request, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("GetRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, request)
}

func (h *Handler) ListRequestsBySubject(w http.ResponseWriter, r *http.Request) {
	subjectRef := r.URL.Query().Get("subject_ref")
	if subjectRef == "" {
		writeError(w, http.StatusBadRequest, "subject_ref query parameter is required")
		return
	}
	requests, err := h.store.ListRequestsBySubject(r.Context(), subjectRef)
	if err != nil {
		h.log.Error("ListRequestsBySubject: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if requests == nil {
		requests = []domain.RightsRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": requests, "count": len(requests)})
}

// RecordIdentityVerification handles
// POST /privacy/rights-requests/{requestID}/identity-verification.
// §15.1: identity assurance is a caller-declared fact this service
// records as evidence, not something it performs itself — see the
// domain package's doc comment. A failed attempt (verified=false) is
// still recorded, but must never advance the case status.
func (h *Handler) RecordIdentityVerification(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	var req domain.RecordIdentityVerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Method == "" {
		writeError(w, http.StatusBadRequest, "method is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("RecordIdentityVerification: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}

	event, request, err := h.store.RecordIdentityVerification(r.Context(), requestID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("RecordIdentityVerification: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"event": event, "request": request})
}

// AttachDiscoveryManifest handles
// POST /privacy/rights-requests/{requestID}/discovery-manifests. This
// service does not perform the search — it records the manifest a
// domain adapter already produced (§15.1).
func (h *Handler) AttachDiscoveryManifest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	var req domain.AttachDiscoveryManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Domain == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, "domain and content_hash are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("AttachDiscoveryManifest: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}

	manifest, request, err := h.store.AttachDiscoveryManifest(r.Context(), requestID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("AttachDiscoveryManifest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"manifest": manifest, "request": request})
}

func (h *Handler) ListDiscoveryManifests(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	manifests, err := h.store.ListDiscoveryManifests(r.Context(), requestID)
	if err != nil {
		h.log.Error("ListDiscoveryManifests: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if manifests == nil {
		manifests = []domain.DiscoveryManifest{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": manifests, "count": len(manifests)})
}

// AttachWFCProcessRef handles
// POST /privacy/rights-requests/{requestID}/wfc-process-ref — records a
// reference to a workflow-svc instance that some OTHER process created
// (see the domain package's doc comment). This service never creates
// that instance itself.
func (h *Handler) AttachWFCProcessRef(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	var req domain.AttachWFCProcessRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.WFCProcessRef == "" {
		writeError(w, http.StatusBadRequest, "wfc_process_ref is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("AttachWFCProcessRef: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}

	request, err := h.store.AttachWFCProcessRef(r.Context(), requestID, req.WFCProcessRef)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("AttachWFCProcessRef: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, request)
}

// CloseRequest handles POST /privacy/rights-requests/{requestID}/close —
// the DISCLOSURE GATE from §15.2, enforced verbatim: FULFILLED requires
// identity assurance AND at least one discovery manifest.
// REJECTED/WITHDRAWN carry no such precondition.
func (h *Handler) CloseRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	var req domain.CloseRequestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Outcome.Valid() {
		writeError(w, http.StatusBadRequest, "outcome is missing or not a recognized value")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, "rights request not found")
			return
		}
		h.log.Error("CloseRequest: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestClose) {
		return
	}

	request, err := h.store.CloseRequest(r.Context(), requestID, req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRequestNotFound):
			writeError(w, http.StatusNotFound, "rights request not found")
		case errors.Is(err, domain.ErrRequestAlreadyClosed):
			writeError(w, http.StatusConflict, "rights request is already closed")
		case errors.Is(err, domain.ErrIdentityNotVerified):
			writeError(w, http.StatusUnprocessableEntity, "DISCLOSURE GATE: identity has not been verified — cannot close as FULFILLED")
		case errors.Is(err, domain.ErrNoDiscoveryManifest):
			writeError(w, http.StatusUnprocessableEntity, "DISCLOSURE GATE: no discovery manifest recorded — cannot close as FULFILLED")
		default:
			h.log.Error("CloseRequest: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.rights_request.closed", EntityID: request.RequestID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: request,
	})
	writeJSON(w, http.StatusOK, request)
}
