package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Actions authorization-svc is asked about. Reading and writing a tenant's federation is privileged:
// the connection decides who the platform believes is who.
const (
	ActionConfigure = "FEDERATION_CONFIGURE"
	ActionApprove   = "FEDERATION_APPROVE"
	ActionSuspend   = "FEDERATION_SUSPEND"
	ActionLink      = "FEDERATION_LINK"
	ActionRead      = "FEDERATION_READ"
	ActionProvision = "FEDERATION_PROVISIONING_MANAGE"
)

// Authorizer is authorization-svc as the handlers see it (authz.Client satisfies it).
type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error
}

// Handler serves the federation routes.
type Handler struct {
	svc   *Service
	login *Login
	prov  *Provisioning
	authz Authorizer
	log   *zap.Logger
}

// NewHandler builds the handler. prov may be nil until provisioning is wired.
func NewHandler(svc *Service, login *Login, authz Authorizer, log *zap.Logger) *Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return &Handler{svc: svc, login: login, authz: authz, log: log}
}

// WithProvisioning mounts the provisioning administration routes.
func (h *Handler) WithProvisioning(p *Provisioning) *Handler { h.prov = p; return h }

// Register mounts the federation routes. The login route is unauthenticated by design (it is how a
// token is obtained) and is exempt from the envelope contract and replay protection like /v1/authenticate.
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/authenticate/federated", h.Login)

	r.Post("/v1/federation/connections", h.Configure)
	r.Get("/v1/federation/connections", h.List)
	r.Get("/v1/federation/connections/{connectionID}", h.Get)
	r.Post("/v1/federation/connections/{connectionID}/versions/{version}/approve", h.Approve)
	r.Post("/v1/federation/connections/{connectionID}/versions/{version}/reject", h.Reject)
	r.Post("/v1/federation/connections/{connectionID}/rotate-metadata", h.Rotate)
	r.Post("/v1/federation/connections/{connectionID}/keys/refresh", h.Refresh)
	r.Post("/v1/federation/connections/{connectionID}/suspend", h.Suspend)
	r.Post("/v1/federation/connections/{connectionID}/resume", h.Resume)
	r.Post("/v1/federation/connections/{connectionID}/revoke", h.Revoke)
	r.Get("/v1/federation/connections/{connectionID}/logins", h.Logins)

	r.Post("/v1/federation/links", h.CreateLink)
	r.Get("/v1/federation/links", h.ListLinks)
	r.Get("/v1/federation/links/{linkID}", h.GetLink)

	if h.prov != nil {
		h.registerProvisioning(r)
	}
}

type errorBody struct {
	Error     string `json:"error"`
	ErrorCode string `json:"error_code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, ErrorCode: code})
}

// caller is the gateway-verified identity of an administrative request.
type caller struct {
	tenantID, principalID, correlationID string
}

// require resolves the tenant and principal the gateway verified and checks the action with
// authorization-svc, failing closed. ok=false means it has answered.
func (h *Handler) require(w http.ResponseWriter, r *http.Request, action string) (caller, bool) {
	c := caller{
		tenantID:      r.Header.Get("X-Tenant-Id"),
		principalID:   r.Header.Get("X-Principal-Id"),
		correlationID: strings.TrimSpace(r.Header.Get("X-Correlation-ID")),
	}
	if c.tenantID == "" || c.principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", "X-Tenant-Id and X-Principal-Id are required — the gateway sets them from a verified identity envelope")
		return c, false
	}
	if err := h.authz.CheckAllowed(r.Context(), c.principalID, c.tenantID, action, c.tenantID); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", "not authorized to perform this action")
			return c, false
		}
		h.log.Error("authorization check failed", zap.String("action", action), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", "authorization service unavailable")
		return c, false
	}
	return c, true
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// fail maps a service error to a response. Internal errors are logged, never returned.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrLinkNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval", err.Error())
	case errors.Is(err, ErrFingerprint):
		writeError(w, http.StatusConflict, "fingerprint_mismatch", err.Error())
	case errors.Is(err, ErrNotPending), errors.Is(err, ErrConflict), errors.Is(err, ErrTransition), errors.Is(err, ErrLinkConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrUnknownPrincipal):
		writeError(w, http.StatusUnprocessableEntity, "unknown_principal", err.Error())
	case errors.Is(err, ErrAccessControlUnavailable):
		writeError(w, http.StatusServiceUnavailable, "access_control_unavailable", err.Error())
	case errors.Is(err, ErrMappingState), errors.Is(err, ErrExceptionState), errors.Is(err, ErrDedupeMismatch):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrUserNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrKeysUnavailable):
		writeError(w, http.StatusServiceUnavailable, "keys_unavailable", "the identity provider's signing keys could not be obtained")
	default:
		h.log.Error("federation request failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

// ── login ────────────────────────────────────────────────────────────────────

// Login handles POST /v1/authenticate/federated.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.Header.Get("X-Correlation-ID")
	}
	resp, err := h.login.Authenticate(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrLoginRequestInvalid):
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, ErrLoginRejected):
			// one message for every refusal; the reason is in the evidence, never on the wire
			writeError(w, http.StatusUnauthorized, "", "invalid credentials")
		case errors.Is(err, ErrLoginUnavailable):
			h.log.Error("federated authentication unavailable", zap.Error(err), zap.String("correlation_id", req.CorrelationID))
			writeError(w, http.StatusServiceUnavailable, "", "authentication unavailable")
		default:
			h.log.Error("unexpected federated authentication error", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "", "internal error")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// ── connections ──────────────────────────────────────────────────────────────

// Configure handles POST /v1/federation/connections: ConfigureFederation, and a new version when it names a connection.
func (h *Handler) Configure(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionConfigure)
	if !ok {
		return
	}
	var req ConfigureRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	d, err := h.svc.Configure(r.Context(), c.tenantID, c.principalID, c.correlationID, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	// 202: it exists and awaits an independent approver
	writeJSON(w, http.StatusAccepted, d)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), c.tenantID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type decisionBody struct {
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

func versionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "version must be a positive integer")
		return 0, false
	}
	return n, true
}

// Approve handles ApproveFederation.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionApprove)
	if !ok {
		return
	}
	n, ok := versionParam(w, r)
	if !ok {
		return
	}
	var b decisionBody
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	d, err := h.svc.Approve(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), n, b.Fingerprint, b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionApprove)
	if !ok {
		return
	}
	n, ok := versionParam(w, r)
	if !ok {
		return
	}
	var b decisionBody
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	d, err := h.svc.Reject(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), n, b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Rotate handles RotateMetadataOrCertificateRef.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionConfigure)
	if !ok {
		return
	}
	var b struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	d, err := h.svc.RotateMetadata(r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), b.JWKSURI)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

// Refresh fetches the signing keys now from the approved location.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionConfigure)
	if !ok {
		return
	}
	n, err := h.svc.RefreshKeys(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection_id": chi.URLParam(r, "connectionID"), "key_count": n})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, fn func(*Service, context.Context, string, string, string, string, string) (*Connection, error)) {
	c, ok := h.require(w, r, ActionSuspend)
	if !ok {
		return
	}
	var b reasonBody
	if err := decodeBody(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	out, err := fn(h.svc, r.Context(), c.tenantID, c.principalID, c.correlationID, chi.URLParam(r, "connectionID"), b.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, (*Service).Suspend)
}
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, (*Service).Resume)
}
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, (*Service).Revoke)
}

func (h *Handler) Logins(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.Logins(r.Context(), c.tenantID, chi.URLParam(r, "connectionID"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

// ── links ────────────────────────────────────────────────────────────────────

func (h *Handler) CreateLink(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionLink)
	if !ok {
		return
	}
	var req LinkRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	l, err := h.svc.Link(r.Context(), c.tenantID, c.principalID, c.correlationID, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (h *Handler) ListLinks(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.ListLinks(r.Context(), c.tenantID, r.URL.Query().Get("principal_id"), r.URL.Query().Get("issuer"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (h *Handler) GetLink(w http.ResponseWriter, r *http.Request) {
	c, ok := h.require(w, r, ActionRead)
	if !ok {
		return
	}
	l, err := h.svc.GetLink(r.Context(), c.tenantID, chi.URLParam(r, "linkID"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// nonNil makes an empty result a JSON array, never null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
