package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/eventing/integration"
)

// ── the public callback endpoint (XIC-04, §9.3) ──────────────────────────────

// CallbackHandler serves POST /v1/callbacks/{connectorKey}/{connectionID}/{operationID}: where a provider reports
// on an outbound operation it was asked to perform. Like the ingest endpoint it sits outside the tenant and envelope
// middleware, because the caller is an external system whose identity is proven by the contract's callback profile.
// The connection id in the path is a lookup key, never an authority; the operation names the OUTBOUND operation whose
// callback profile governs the request.
type CallbackHandler struct {
	svc      *outbound.Service
	logger   *zap.Logger
	maxBody  int64
	trustXFF bool
}

// NewCallback builds the callback handler.
func NewCallback(svc *outbound.Service, maxBody int64, trustXFF bool, logger *zap.Logger) *CallbackHandler {
	if maxBody <= 0 {
		maxBody = outbound.DefaultConfig().CallbackMaxBodyBytes
	}
	return &CallbackHandler{svc: svc, maxBody: maxBody, trustXFF: trustXFF, logger: logger}
}

// Router returns the callback routes, to be mounted at /v1/callbacks.
func (h *CallbackHandler) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/{connectorKey}/{connectionID}/{operationID}", h.Receive)
	return r
}

func (h *CallbackHandler) remote(r *http.Request) string {
	if h.trustXFF {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return r.RemoteAddr
}

func (h *CallbackHandler) Receive(w http.ResponseWriter, r *http.Request) {
	// bounded before anything is parsed or any secret is fetched; a body past the cap is refused, not truncated
	if r.ContentLength > h.maxBody {
		writeCallbackProblem(w, r, &outbound.CallbackOutcome{Status: http.StatusRequestEntityTooLarge, Detail: "the request body is larger than this endpoint accepts"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeCallbackProblem(w, r, &outbound.CallbackOutcome{Status: http.StatusRequestEntityTooLarge, Detail: "the request body is larger than this endpoint accepts"})
			return
		}
		writeCallbackProblem(w, r, &outbound.CallbackOutcome{Status: http.StatusBadRequest, Detail: "the request body could not be read"})
		return
	}
	corr := r.Header.Get("X-Correlation-Id")
	if corr == "" {
		corr = r.Header.Get("X-Request-Id")
	}
	if len(corr) > 128 {
		corr = corr[:128]
	}
	o := h.svc.ReceiveCallback(r.Context(), outbound.CallbackRequest{ConnectorKey: chi.URLParam(r, "connectorKey"), ConnectionID: chi.URLParam(r, "connectionID"),
		OperationID: chi.URLParam(r, "operationID"), Method: r.Method, Header: r.Header, Body: body, RemoteAddr: h.remote(r), CorrelationID: corr})
	switch {
	case o.Status == http.StatusOK && o.Receipt != nil:
		// the acknowledgement is deterministic: a repeat gets the original receipt and says so
		writeJSON(w, http.StatusOK, map[string]any{"receipt_id": o.Receipt.ReceiptID, "state": o.Receipt.State, "duplicate": o.Duplicate})
	default:
		writeCallbackProblem(w, r, o)
	}
}

// writeCallbackProblem answers a refusal with RFC 9457 problem details: a stable XIC code when the specification
// names the condition, the receipt id when one was recorded, and nothing from the payload or the credentials.
func writeCallbackProblem(w http.ResponseWriter, r *http.Request, o *outbound.CallbackOutcome) {
	var body map[string]any
	status := o.Status
	if o.XIC != nil {
		body = integration.XICProblem(o.XIC, r.Header.Get("X-Correlation-Id"))
		body["status"] = status
	} else {
		body = problemBase(r, status, "", nonEmpty(o.Detail, http.StatusText(status)))
	}
	if o.Receipt != nil && o.Status != http.StatusUnauthorized {
		body["receipt_id"] = o.Receipt.ReceiptID
		if o.Reason != "" {
			body["reason_code"] = o.Reason
		}
	}
	if o.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.RetryAfter))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(status)
	_ = encodeJSON(w, body)
}

// ── receipts API (tenant) ────────────────────────────────────────────────────

// ActionCallbackRead is the authorization action of the receipts API.
const ActionCallbackRead = "XIC_CALLBACK_READ"

// CallbackAdminHandler serves the read API over callback receipts: the evidence a support engineer or the
// reconciliation process reads. A receipt holds hashes and metadata, never a body.
type CallbackAdminHandler struct {
	repo   outbound.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewCallbackAdmin builds the receipts handler.
func NewCallbackAdmin(repo outbound.Repo, az *authz.Client, logger *zap.Logger) *CallbackAdminHandler {
	return &CallbackAdminHandler{repo: repo, authz: az, logger: logger}
}

// RegisterCallbackAdminRoutes mounts the receipts routes.
func RegisterCallbackAdminRoutes(r chi.Router, h *CallbackAdminHandler) {
	r.Route("/v1/callback-receipts", func(r chi.Router) {
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
	})
}

func (h *CallbackAdminHandler) guard(w http.ResponseWriter, r *http.Request) bool {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeProblem(w, r, errNoPrincipal)
		return false
	}
	entity := r.Header.Get("X-Legal-Entity-Id")
	if entity == "" {
		entity = middleware.GetTenantID(r.Context())
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entity, ActionCallbackRead); err != nil {
		if !errors.Is(err, authz.ErrAuthorizationDenied) {
			h.logger.Error("authorization check failed", zap.Error(err))
			err = errors.Join(authz.ErrAuthzServiceUnavailable, err)
		}
		writeProblem(w, r, err)
		return false
	}
	return true
}

// List is GET /v1/callback-receipts?state=&connection_id=&attempt_id=&needs_reconciliation=true.
func (h *CallbackAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.ListReceipts(r.Context(), outbound.ReceiptFilter{State: strings.ToUpper(q.Get("state")), ConnectionID: q.Get("connection_id"),
		AttemptID: q.Get("attempt_id"), NeedsReconciliation: q.Get("needs_reconciliation") == "true", Limit: limit})
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": list, "total": len(list)})
}

// Get is GET /v1/callback-receipts/{id}.
func (h *CallbackAdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	rc, err := h.repo.GetReceipt(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, outbound.ErrReceiptNotFound) {
			emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
			return
		}
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rc)
}
