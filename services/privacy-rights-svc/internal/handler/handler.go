// Package handler exposes privacy-rights-svc's REST API — PRV-04.
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
	mountRoutes := func(r chi.Router) {
		r.Post("/", h.CreateRequest)
		r.Get("/", h.ListRequestsBySubject)
		r.Get("/{requestID}", h.GetRequest)
		r.Post("/{requestID}/identity-verification", h.RecordIdentityVerification)
		r.Post("/{requestID}/discovery-manifests", h.AttachDiscoveryManifest)
		r.Get("/{requestID}/discovery-manifests", h.ListDiscoveryManifests)
		r.Post("/{requestID}/wfc-process-ref", h.AttachWFCProcessRef)
		r.Post("/{requestID}/close", h.CloseRequest)
	}

	r.Route("/privacy/rights-requests", mountRoutes)
	r.Route("/v1/privacy/rights-requests", mountRoutes)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func hashBody(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func readBodyBytes(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func (h *Handler) checkIdempotency(w http.ResponseWriter, r *http.Request, tenantID, principalID string, body []byte) (*domain.IdempotencyRecord, bool, string, string) {
	key := r.Header.Get("Idempotency-Key")
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
			Key:          key,
			TenantID:     tenantID,
			Endpoint:     endpoint,
			RequestHash:  reqHash,
			ResponseCode: status,
			ResponseBody: body,
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

// CreateRequest handles POST /privacy/rights-requests — case intake.
// This is PRV-04's OWN privacy-meaning record; it does not create a
// workflow-svc instance (see the domain package's doc comment on
// wfc_process_ref for why).
func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, verifiedTenant, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.CreateRightsRequestRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if req.SubjectRef == "" {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if !req.RightFamily.Valid() {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}

	if req.TenantID != "" && req.TenantID != verifiedTenant {
		writeError(w, http.StatusForbidden, domain.PRV003PrivacyRoleUnresolved)
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = verifiedTenant
	}

	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestCreate) {
		return
	}

	request, err := h.store.CreateRequest(r.Context(), tenantID, req, principalID)
	if err != nil {
		h.log.Error("CreateRequest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.rights_request.received", EntityID: request.RequestID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: request,
	})

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CreateRequest", idemKey, reqHash, http.StatusCreated, request)
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
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// Check idempotency (§18.1)
	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("RecordIdentityVerification: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}
	if existing.Status == domain.StatusClosed {
		writeError(w, http.StatusConflict, domain.PRV020ImmutableEvidenceConflict)
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.RecordIdentityVerificationRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if req.Method == "" {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}

	event, request, err := h.store.RecordIdentityVerification(r.Context(), requestID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("RecordIdentityVerification: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "RecordIdentityVerification", idemKey, reqHash, http.StatusCreated, map[string]interface{}{"event": event, "request": request})
}

// AttachDiscoveryManifest handles
// POST /privacy/rights-requests/{requestID}/discovery-manifests. This
// service does not perform the search — it records the manifest a
// domain adapter already produced (§15.1).
func (h *Handler) AttachDiscoveryManifest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("AttachDiscoveryManifest: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}
	if existing.Status == domain.StatusClosed {
		writeError(w, http.StatusConflict, domain.PRV020ImmutableEvidenceConflict)
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.AttachDiscoveryManifestRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if req.Domain == "" || req.ContentHash == "" {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}

	manifest, request, err := h.store.AttachDiscoveryManifest(r.Context(), requestID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("AttachDiscoveryManifest: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "AttachDiscoveryManifest", idemKey, reqHash, http.StatusCreated, map[string]interface{}{"manifest": manifest, "request": request})
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
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("AttachWFCProcessRef: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestProcess) {
		return
	}
	if existing.Status == domain.StatusClosed {
		writeError(w, http.StatusConflict, domain.PRV020ImmutableEvidenceConflict)
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.AttachWFCProcessRefRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if req.WFCProcessRef == "" {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}

	request, err := h.store.AttachWFCProcessRef(r.Context(), requestID, req.WFCProcessRef)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("AttachWFCProcessRef: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}

	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "AttachWFCProcessRef", idemKey, reqHash, http.StatusOK, request)
}

// CloseRequest handles POST /privacy/rights-requests/{requestID}/close —
// the DISCLOSURE GATE from §15.2, enforced verbatim: FULFILLED requires
// identity assurance AND at least one discovery manifest.
// REJECTED/WITHDRAWN carry no such precondition.
// I21: response package versioning — increments on every FULFILLED closure.
func (h *Handler) CloseRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	bodyBytes, err := readBodyBytes(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	existing, err := h.store.FindRequest(r.Context(), requestID)
	if err != nil {
		if errors.Is(err, domain.ErrRequestNotFound) {
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
			return
		}
		h.log.Error("CloseRequest: lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		return
	}
	tenantID := ""
	if existing.TenantID != nil {
		tenantID = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, tenantID, PrivacyRightsRequestClose) {
		return
	}

	// Check idempotency (§18.1)
	_, handled, idemKey, reqHash := h.checkIdempotency(w, r, tenantID, principalID, bodyBytes)
	if handled {
		return
	}

	var req domain.CloseRequestRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}
	if !req.Outcome.Valid() {
		writeError(w, http.StatusBadRequest, domain.PRV005PolicyUnavailable)
		return
	}

	request, err := h.store.CloseRequest(r.Context(), requestID, req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRequestNotFound):
			writeError(w, http.StatusNotFound, domain.PRV001PurposeNotRegistered)
		case errors.Is(err, domain.ErrRequestAlreadyClosed):
			writeError(w, http.StatusConflict, domain.PRV020ImmutableEvidenceConflict)
		case errors.Is(err, domain.ErrIdentityNotVerified):
			writeError(w, http.StatusUnprocessableEntity, domain.PRV012IdentityAssuranceInsufficient)
		case errors.Is(err, domain.ErrNoDiscoveryManifest):
			writeError(w, http.StatusUnprocessableEntity, domain.PRV013ThirdPartyReviewRequired)
		default:
			h.log.Error("CloseRequest: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.PRV019PrivacyContextIndeterminate)
		}
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "privacy.rights_request.closed", EntityID: request.RequestID, TenantID: tenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: request,
	})
	h.writeJSONWithIdempotency(r.Context(), w, tenantID, principalID, "CloseRequest", idemKey, reqHash, http.StatusOK, request)
}
