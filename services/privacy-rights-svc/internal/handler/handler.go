// Package handler exposes privacy-rights-svc's REST API — PRV-04.
//
// Every route — read or write — runs the same pre-flight, in this order:
//
//  1. X-Principal-Id          -> 401 AUTHENTICATION_FAILED
//  2. X-Tenant-Id             -> 401 TENANT_SCOPE_REQUIRED (middleware.TenantContext)
//  3. mutating only: X-Correlation-ID -> 400 CORRELATION_ID_REQUIRED
//  4. mutating only: Idempotency-Key  -> 400 IDEMPOTENCY_KEY_REQUIRED
//  5. authorization-svc grant on the caller's verified tenant -> 403 / 503 (fail closed)
//  6. mutating only: idempotent replay lookup (lookup failure -> 503, never "proceed")
//  7. body validation -> 400
//  8. resolve the resource (RLS-scoped) -> 404
//
// Authorization is decided on the caller's own verified tenant BEFORE any
// resource lookup, so an unauthorized caller cannot use 404-vs-403 to probe
// which case ids exist, and a replayed key can never bypass authorization.
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
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-rights-svc/internal/authz"
	"zoiko.io/privacy-rights-svc/internal/domain"
	svcmiddleware "zoiko.io/privacy-rights-svc/internal/middleware"
	"zoiko.io/privacy-rights-svc/internal/store"
)

const (
	PrivacyRightsRequestCreate  = "PRIVACY_RIGHTS_REQUEST_CREATE"
	PrivacyRightsRequestProcess = "PRIVACY_RIGHTS_REQUEST_PROCESS"
	PrivacyRightsRequestClose   = "PRIVACY_RIGHTS_REQUEST_CLOSE"
)

// readAction is the grant the three read routes demand. The specification
// names no read permission for this service, and none is invented: reading a
// case's evidence is part of processing it, so the existing
// PRIVACY_RIGHTS_REQUEST_PROCESS action is reused (documented in the README as
// a needs-clarification item).
const readAction = PrivacyRightsRequestProcess

const maxBodyBytes = 1 << 20

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler holds no event publisher. §19 of ZS-SVC-W-001 declares no
// rights-intake or rights-closure event, so this service emits none (see the
// README, "Events").
type Handler struct {
	store store.Store
	authz AuthzChecker
	log   *zap.Logger
}

func New(st store.Store, az AuthzChecker, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, log: log}
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

// ── responses ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, ErrorCode: code})
}

func (h *Handler) storeUnavailable(w http.ResponseWriter, op string, err error) {
	h.log.Error(op+": store unavailable", zap.Error(err))
	writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, domain.PRV019PrivacyContextIndeterminate)
}

// writeStoreError maps the store's sentinel errors to the documented
// status/code contract; anything else is an outage (503).
func (h *Handler) writeStoreError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrRequestNotFound):
		writeCoded(w, http.StatusNotFound, domain.ErrCodeNotFound, domain.ErrRequestNotFound.Error())
	case errors.Is(err, domain.ErrRequestAlreadyClosed):
		writeCoded(w, http.StatusConflict, domain.ErrCodeImmutableEvidence, domain.PRV020ImmutableEvidenceConflict)
	case errors.Is(err, domain.ErrIdentityNotVerified):
		writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeIdentityInsufficient, domain.PRV012IdentityAssuranceInsufficient)
	case errors.Is(err, domain.ErrNoDiscoveryManifest):
		writeCoded(w, http.StatusUnprocessableEntity, domain.ErrCodeThirdPartyReview, domain.PRV013ThirdPartyReviewRequired)
	default:
		h.storeUnavailable(w, op, err)
	}
}

// ── pre-flight ───────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	if principalID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireTenant returns the verified tenant. svcmiddleware.TenantContext
// already refuses a request without one; this is the handler-level guard so
// no code path can run with an empty scope.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := strings.TrimSpace(svcmiddleware.TenantFromContext(r.Context()))
	if tenantID == "" {
		writeCoded(w, http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired, "X-Tenant-Id header is required")
		return "", false
	}
	return tenantID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeCoded(w, http.StatusForbidden, domain.ErrCodeAuthorizationDenied, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeAuthzUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

type callContext struct {
	Principal   string
	Tenant      string
	Correlation string // "" on reads
	IdemKey     string // "" on reads
}

// begin runs pre-flight steps 1-5. On false the response has been written.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request, actionType string, mutating bool) (*callContext, bool) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	cc := &callContext{Principal: principal, Tenant: tenant}
	if mutating {
		cc.Correlation = strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
		if cc.Correlation == "" {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeCorrelationRequired, "X-Correlation-ID header is required")
			return nil, false
		}
		cc.IdemKey = idempotencyKey(r)
		if cc.IdemKey == "" {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeIdempotencyRequired, "Idempotency-Key header is required")
			return nil, false
		}
	}
	if !h.authorize(w, r, principal, tenant, actionType) {
		return nil, false
	}
	return cc, true
}

// idempotencyKey is the ONE place the key is read, so the mandatory check and
// the replay/claim logic can never disagree about whether a key was supplied.
// X-Idempotency-Key is accepted as an alias.
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
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "failed to read request body")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, true
}

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

// requestHash binds a key to method + path + body. The /v1 alias prefix is
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
// authorization). A lookup failure refuses the request (503): carrying on
// without the lookup would execute a request that may be a replay.
// handled=true means a response was written.
func (h *Handler) replayOrContinue(w http.ResponseWriter, r *http.Request, cc *callContext, hash string) (handled bool) {
	existing, err := h.store.GetIdempotency(r.Context(), cc.Tenant, cc.Principal, cc.IdemKey)
	if err != nil {
		h.log.Error("idempotency lookup failed — refusing the request", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable, "idempotency store unavailable")
		return true
	}
	if existing != nil {
		writeReplay(w, existing, hash)
		return true
	}
	return false
}

// writeReplay returns the stored original response, or 409 PRV-020 when the
// key was used with a different request.
func writeReplay(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string) {
	if existing.RequestHash != hash {
		writeCoded(w, http.StatusConflict, domain.ErrCodeImmutableEvidence, domain.ErrIdempotencyConflict.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotency-Replay", "true")
	w.WriteHeader(existing.ResponseCode)
	_, _ = w.Write(existing.ResponseBody)
}

// claim builds the atomic idempotency claim for one mutation. render turns the
// operation's typed result into the response that is both stored for replay
// and returned to the first caller.
func (h *Handler) claim(cc *callContext, endpoint, hash string, status int, render func(result any) any) *store.Idem {
	return &store.Idem{
		Key: cc.IdemKey, PrincipalID: cc.Principal, Endpoint: endpoint, RequestHash: hash,
		Render: func(result any) (int, []byte, error) {
			b, err := json.Marshal(render(result))
			return status, b, err
		},
	}
}

// respond writes the first-execution response (identical bytes to what was
// stored for replay) or, when a concurrent request won the claim, the replay.
func respond(w http.ResponseWriter, existing *domain.IdempotencyRecord, hash string, status int, v any) {
	if existing != nil {
		writeReplay(w, existing, hash)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		writeCoded(w, http.StatusInternalServerError, domain.ErrCodeInternal, "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ── validation ───────────────────────────────────────────────────────────────

var sha256Digest = regexp.MustCompile(`^(?i:sha256:)?[0-9a-fA-F]{64}$`)

// ── handlers ─────────────────────────────────────────────────────────────────

// CreateRequest handles POST /privacy/rights-requests — case intake.
func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	cc, ok := h.begin(w, r, PrivacyRightsRequestCreate, true)
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

	var req domain.CreateRightsRequestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.TenantID != "" && req.TenantID != cc.Tenant {
		writeCoded(w, http.StatusForbidden, domain.ErrCodeTenantScopeMismatch, "tenant_id does not match the verified X-Tenant-Id")
		return
	}
	if req.SubjectRef == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "subject_ref is required")
		return
	}
	if !req.RightFamily.Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "right_family is not a recognized right family")
		return
	}

	idem := h.claim(cc, "CreateRequest", hash, http.StatusCreated, func(res any) any {
		return res.(*domain.RightsRequest).WithCanonicalResponseFields()
	})
	request, existing, err := h.store.CreateRequest(r.Context(), cc.Tenant, req, cc.Principal, idem)
	if err != nil {
		h.writeStoreError(w, "CreateRequest", err)
		return
	}
	var out any
	if request != nil {
		out = request.WithCanonicalResponseFields()
	}
	respond(w, existing, hash, http.StatusCreated, out)
}

func (h *Handler) GetRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, readAction, false); !ok {
		return
	}
	request, err := h.store.FindRequest(r.Context(), chi.URLParam(r, "requestID"))
	if err != nil {
		h.writeStoreError(w, "GetRequest", err)
		return
	}
	writeJSON(w, http.StatusOK, request.WithCanonicalResponseFields())
}

func (h *Handler) ListRequestsBySubject(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, readAction, false); !ok {
		return
	}
	subjectRef := r.URL.Query().Get("subject_ref")
	if subjectRef == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "subject_ref query parameter is required")
		return
	}
	requests, err := h.store.ListRequestsBySubject(r.Context(), subjectRef)
	if err != nil {
		h.writeStoreError(w, "ListRequestsBySubject", err)
		return
	}
	if requests == nil {
		requests = []domain.RightsRequest{}
	}
	for i := range requests {
		requests[i] = *requests[i].WithCanonicalResponseFields()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": requests, "count": len(requests)})
}

// RecordIdentityVerification handles
// POST /privacy/rights-requests/{requestID}/identity-verification.
// §15.1: identity assurance is a caller-declared fact this service records as
// evidence. A failed attempt (verified=false) is recorded but never advances
// the case.
func (h *Handler) RecordIdentityVerification(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	cc, ok := h.begin(w, r, PrivacyRightsRequestProcess, true)
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

	var req domain.RecordIdentityVerificationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.Method == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "method is required")
		return
	}

	render := func(res any) any {
		x := res.(*store.IdentityResult)
		return map[string]interface{}{"event": x.Event, "request": x.Request.WithCanonicalResponseFields()}
	}
	res, existing, err := h.store.RecordIdentityVerification(r.Context(), requestID, req, cc.Principal, h.claim(cc, "RecordIdentityVerification", hash, http.StatusCreated, render))
	if err != nil {
		h.writeStoreError(w, "RecordIdentityVerification", err)
		return
	}
	var out any
	if res != nil {
		out = render(res)
	}
	respond(w, existing, hash, http.StatusCreated, out)
}

// AttachDiscoveryManifest handles
// POST /privacy/rights-requests/{requestID}/discovery-manifests. This service
// records the manifest a domain adapter already produced (§15.1); it does not
// perform the search.
func (h *Handler) AttachDiscoveryManifest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	cc, ok := h.begin(w, r, PrivacyRightsRequestProcess, true)
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

	var req domain.AttachDiscoveryManifestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.Domain == "" || req.ContentHash == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "domain and content_hash are required")
		return
	}
	// §15.1 "signed/hashed discovery manifests": a manifest counts toward the
	// disclosure gate only if it carries a real SHA-256 digest, not any
	// non-empty string.
	if !sha256Digest.MatchString(strings.TrimSpace(req.ContentHash)) {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, domain.ErrInvalidManifestHash.Error())
		return
	}
	if req.CandidateCount < 0 {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "candidate_count must not be negative")
		return
	}
	req.ContentHash = strings.TrimSpace(req.ContentHash)

	render := func(res any) any {
		x := res.(*store.ManifestResult)
		return map[string]interface{}{"manifest": x.Manifest.WithCanonicalResponseFields(), "request": x.Request.WithCanonicalResponseFields()}
	}
	res, existing, err := h.store.AttachDiscoveryManifest(r.Context(), requestID, req, cc.Principal, h.claim(cc, "AttachDiscoveryManifest", hash, http.StatusCreated, render))
	if err != nil {
		h.writeStoreError(w, "AttachDiscoveryManifest", err)
		return
	}
	var out any
	if res != nil {
		out = render(res)
	}
	respond(w, existing, hash, http.StatusCreated, out)
}

func (h *Handler) ListDiscoveryManifests(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, readAction, false); !ok {
		return
	}
	manifests, err := h.store.ListDiscoveryManifests(r.Context(), chi.URLParam(r, "requestID"))
	if err != nil {
		h.writeStoreError(w, "ListDiscoveryManifests", err)
		return
	}
	if manifests == nil {
		manifests = []domain.DiscoveryManifest{}
	}
	for i := range manifests {
		manifests[i] = *manifests[i].WithCanonicalResponseFields()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": manifests, "count": len(manifests)})
}

// AttachWFCProcessRef handles
// POST /privacy/rights-requests/{requestID}/wfc-process-ref — records a
// reference to a workflow-svc instance some OTHER process created.
func (h *Handler) AttachWFCProcessRef(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	cc, ok := h.begin(w, r, PrivacyRightsRequestProcess, true)
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

	var req domain.AttachWFCProcessRefRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if req.WFCProcessRef == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "wfc_process_ref is required")
		return
	}

	render := func(res any) any { return res.(*domain.RightsRequest).WithCanonicalResponseFields() }
	request, existing, err := h.store.AttachWFCProcessRef(r.Context(), requestID, req.WFCProcessRef, h.claim(cc, "AttachWFCProcessRef", hash, http.StatusOK, render))
	if err != nil {
		h.writeStoreError(w, "AttachWFCProcessRef", err)
		return
	}
	var out any
	if request != nil {
		out = render(request)
	}
	respond(w, existing, hash, http.StatusOK, out)
}

// CloseRequest handles POST /privacy/rights-requests/{requestID}/close — the
// DISCLOSURE GATE of §15.2 as far as this service can verify it (see
// store.CloseRequest). REJECTED/WITHDRAWN carry no such precondition.
func (h *Handler) CloseRequest(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	cc, ok := h.begin(w, r, PrivacyRightsRequestClose, true)
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

	var req domain.CloseRequestRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "invalid request body")
		return
	}
	if !req.Outcome.Valid() {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeValidationFailed, "outcome must be one of FULFILLED, REJECTED, WITHDRAWN")
		return
	}

	render := func(res any) any { return res.(*domain.RightsRequest).WithCanonicalResponseFields() }
	request, existing, err := h.store.CloseRequest(r.Context(), requestID, req, cc.Principal, h.claim(cc, "CloseRequest", hash, http.StatusOK, render))
	if err != nil {
		h.writeStoreError(w, "CloseRequest", err)
		return
	}
	var out any
	if request != nil {
		out = render(request)
	}
	respond(w, existing, hash, http.StatusOK, out)
}
