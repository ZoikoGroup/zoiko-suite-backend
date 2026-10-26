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
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/eventing/integration"
)

// ── the public ingest endpoint (XIC-03) ──────────────────────────────────────

// IngestHandler serves POST /v1/inbound/{connectorKey}/{connectionID}/{operationID}: the connector-specific inbound
// endpoint family of §13. It is deliberately outside the tenant and envelope middleware: the caller is an external
// system, not a ZoikoSuite principal, and its identity is proven by the contract's authentication profile. The
// connection id in the path is a lookup key, never an authority: nothing is trusted until the sender authenticates
// against THAT connection's credential, and the tenant comes from the connection.
type IngestHandler struct {
	svc      *inbound.Service
	logger   *zap.Logger
	maxBody  int64
	trustXFF bool
}

// NewIngest builds the ingest handler.
func NewIngest(svc *inbound.Service, maxBody int64, trustXFF bool, logger *zap.Logger) *IngestHandler {
	if maxBody <= 0 {
		maxBody = inbound.DefaultConfig().MaxBodyBytes
	}
	return &IngestHandler{svc: svc, maxBody: maxBody, trustXFF: trustXFF, logger: logger}
}

// Router returns the ingest routes, to be mounted at /v1/inbound.
func (h *IngestHandler) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/{connectorKey}/{connectionID}/{operationID}", h.Receive)
	return r
}

func (h *IngestHandler) remote(r *http.Request) string {
	if h.trustXFF {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return r.RemoteAddr
}

func (h *IngestHandler) Receive(w http.ResponseWriter, r *http.Request) {
	// bounded before anything is parsed or any secret is fetched; a body past the cap is refused, not truncated
	if r.ContentLength > h.maxBody {
		writeIngestProblem(w, r, &inbound.Outcome{Status: http.StatusRequestEntityTooLarge, Detail: "the request body is larger than this endpoint accepts"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeIngestProblem(w, r, &inbound.Outcome{Status: http.StatusRequestEntityTooLarge, Detail: "the request body is larger than this endpoint accepts"})
			return
		}
		writeIngestProblem(w, r, &inbound.Outcome{Status: http.StatusBadRequest, Detail: "the request body could not be read"})
		return
	}
	corr := r.Header.Get("X-Correlation-Id")
	if corr == "" {
		corr = r.Header.Get("X-Request-Id")
	}
	if len(corr) > 128 {
		corr = corr[:128]
	}
	o := h.svc.Receive(r.Context(), inbound.Request{ConnectorKey: chi.URLParam(r, "connectorKey"), ConnectionID: chi.URLParam(r, "connectionID"),
		OperationID: chi.URLParam(r, "operationID"), Method: r.Method, Header: r.Header, Body: body, RemoteAddr: h.remote(r), CorrelationID: corr})
	switch {
	case o.Status == http.StatusAccepted && o.Exchange != nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"exchange_id": o.Exchange.ExchangeID, "state": o.Exchange.State, "transport": "ACCEPTED", "domain": o.Exchange.DomainOutcome})
	case o.Duplicate && o.Exchange != nil:
		writeJSON(w, http.StatusOK, map[string]any{"exchange_id": o.Exchange.ExchangeID, "state": o.Exchange.State, "duplicate": true})
	default:
		writeIngestProblem(w, r, o)
	}
}

// writeIngestProblem answers a refusal with RFC 9457 problem details: a stable XIC code when the specification
// names the condition, the exchange id when one was recorded, and nothing from the payload or the credentials.
func writeIngestProblem(w http.ResponseWriter, r *http.Request, o *inbound.Outcome) {
	var body map[string]any
	status := o.Status
	if o.XIC != nil {
		trace := r.Header.Get("X-Correlation-Id")
		body = integration.XICProblem(o.XIC, trace)
		status = o.Status
		body["status"] = status
	} else {
		body = problemBase(r, status, "", nonEmpty(o.Detail, http.StatusText(status)))
	}
	if o.Reason != "" && o.XIC != nil && o.Exchange != nil {
		body["reason_code"] = o.Reason
	}
	if o.Exchange != nil {
		body["exchange_id"] = o.Exchange.ExchangeID
		body["state"] = o.Exchange.State
	}
	if o.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.RetryAfter))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(status)
	_ = encodeJSON(w, body)
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ── tenant and platform management API ───────────────────────────────────────

// Authorization actions of the inbound management API.
const (
	ActionInboundRead           = "XIC_INBOUND_READ"
	ActionInboundReleaseRequest = "XIC_INBOUND_RELEASE_REQUEST"
	ActionInboundReleaseDecide  = "XIC_INBOUND_RELEASE_DECIDE"
	ActionHandoffTargetCreate   = "XIC_HANDOFF_TARGET_CREATE"
	ActionHandoffTargetApprove  = "XIC_HANDOFF_TARGET_APPROVE"
	ActionHandoffTargetSuspend  = "XIC_HANDOFF_TARGET_SUSPEND"
	ActionHandoffTargetRead     = "XIC_HANDOFF_TARGET_READ"
)

// InboundAdminHandler serves the exchange read API and the quarantine and handoff-target commands.
type InboundAdminHandler struct {
	svc    *inbound.Service
	repo   inbound.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewInboundAdmin builds the management handler.
func NewInboundAdmin(svc *inbound.Service, repo inbound.Repo, az *authz.Client, logger *zap.Logger) *InboundAdminHandler {
	return &InboundAdminHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterInboundAdminRoutes mounts the management routes.
func RegisterInboundAdminRoutes(r chi.Router, h *InboundAdminHandler) {
	r.Route("/v1/integration-exchanges", func(r chi.Router) {
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/events", h.Events)
		r.Get("/{id}/requests", h.Requests)
		r.Post("/{id}/requests", h.RequestAction)
		r.Post("/{id}/requests/{rcmd}", h.Decide)
	})
	r.Route("/v1/handoff-targets", func(r chi.Router) {
		r.Post("/", h.CreateTarget)
		r.Get("/", h.ListTargets)
		r.Get("/{id}", h.GetTarget)
		r.Post("/{idcmd}", h.TargetCommand)
	})
}

func (h *InboundAdminHandler) actor(r *http.Request) inbound.Actor {
	return inbound.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *InboundAdminHandler) guard(w http.ResponseWriter, r *http.Request, action string, platform bool) bool {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeProblem(w, r, errNoPrincipal)
		return false
	}
	entity := r.Header.Get("X-Legal-Entity-Id")
	if platform {
		entity = registry.PlatformScopeID
	} else if entity == "" {
		entity = middleware.GetTenantID(r.Context())
	}
	if err := h.authz.CheckAllowed(r.Context(), principal, entity, action); err != nil {
		if !errors.Is(err, authz.ErrAuthorizationDenied) {
			h.logger.Error("authorization check failed", zap.Error(err))
			err = errors.Join(authz.ErrAuthzServiceUnavailable, err)
		}
		writeProblem(w, r, err)
		return false
	}
	return true
}

func writeInboundProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, inbound.ErrNotFound), errors.Is(err, inbound.ErrTargetNotFound), errors.Is(err, inbound.ErrRequestNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, inbound.ErrNotPlatform):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, "only the platform scope may do that"), 0)
	case errors.Is(err, inbound.ErrSelfAction):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, inbound.ErrBadRequest):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, err.Error()), 0)
	case errors.Is(err, inbound.ErrVersionConflict):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, integration.CodeIdempotencyConflict, err.Error()), 0)
	case errors.Is(err, inbound.ErrNotReleasable), errors.Is(err, inbound.ErrNoRaw), errors.Is(err, inbound.ErrTransition), errors.Is(err, inbound.ErrRequestState),
		errors.Is(err, inbound.ErrTargetExists):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	case errors.Is(err, inbound.ErrEgress):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeResidencyDenied, "the target is not an allowed egress destination"), 0)
	case errors.Is(err, inbound.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency is unavailable; retry"), 5)
	default:
		writeProblem(w, r, err)
	}
}

func (h *InboundAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionInboundRead, false) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.List(r.Context(), inbound.Filter{State: strings.ToUpper(q.Get("state")), ConnectionID: q.Get("connection_id"), Class: strings.ToUpper(q.Get("class")), Limit: limit})
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exchanges": list, "total": len(list)})
}

func (h *InboundAdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionInboundRead, false) {
		return
	}
	x, err := h.repo.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *InboundAdminHandler) Events(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionInboundRead, false) {
		return
	}
	ev, err := h.repo.Events(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "total": len(ev)})
}

func (h *InboundAdminHandler) Requests(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionInboundRead, false) {
		return
	}
	rq, err := h.repo.Requests(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rq, "total": len(rq)})
}

type actionBody struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

func (h *InboundAdminHandler) RequestAction(w http.ResponseWriter, r *http.Request) {
	var b actionBody
	if !decodeBig(w, r, &b) {
		return
	}
	if !h.guard(w, r, ActionInboundReleaseRequest, false) {
		return
	}
	req, err := h.svc.RequestAction(r.Context(), h.actor(r), chi.URLParam(r, "id"), b.Action, b.Reason)
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, req)
}

// Decide approves or rejects a release or dismissal: POST /{id}/requests/{rid}:approve|:reject.
func (h *InboundAdminHandler) Decide(w http.ResponseWriter, r *http.Request) {
	rid, cmd := split(chi.URLParam(r, "rcmd"))
	if cmd != "approve" && cmd != "reject" {
		writeInboundProblem(w, r, inbound.ErrRequestNotFound)
		return
	}
	if !h.guard(w, r, ActionInboundReleaseDecide, false) {
		return
	}
	x, req, err := h.svc.DecideAction(r.Context(), h.actor(r), chi.URLParam(r, "id"), rid, cmd == "approve")
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": req, "exchange": x})
}

// ── handoff targets (platform) ──────────────────────────────────────────────

func (h *InboundAdminHandler) CreateTarget(w http.ResponseWriter, r *http.Request) {
	var in inbound.CreateTargetInput
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionHandoffTargetCreate, true) {
		return
	}
	t, err := h.svc.CreateTarget(r.Context(), middleware.GetTenantID(r.Context()), h.actor(r), in)
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *InboundAdminHandler) ListTargets(w http.ResponseWriter, r *http.Request) {
	if middleware.GetTenantID(r.Context()) != registry.PlatformScopeID { // a target's address is not tenant data
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
		return
	}
	if !h.guard(w, r, ActionHandoffTargetRead, true) {
		return
	}
	list, err := h.repo.ListTargets(r.Context())
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": list, "total": len(list)})
}

func (h *InboundAdminHandler) GetTarget(w http.ResponseWriter, r *http.Request) {
	if middleware.GetTenantID(r.Context()) != registry.PlatformScopeID {
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
		return
	}
	if !h.guard(w, r, ActionHandoffTargetRead, true) {
		return
	}
	t, err := h.repo.GetTarget(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type targetCommand struct {
	ExpectedVersion int64 `json:"expected_version"`
}

func (h *InboundAdminHandler) TargetCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b targetCommand
	if !decodeBig(w, r, &b) {
		return
	}
	n, err := expectedVersion(r, b.ExpectedVersion)
	if err != nil || n < 1 {
		writeProblem(w, r, errNeedVersion)
		return
	}
	tenant := middleware.GetTenantID(r.Context())
	var t *inbound.Target
	switch cmd {
	case "approve":
		if !h.guard(w, r, ActionHandoffTargetApprove, true) {
			return
		}
		t, err = h.svc.ApproveTarget(r.Context(), tenant, h.actor(r), id, n)
	case "suspend", "retire":
		if !h.guard(w, r, ActionHandoffTargetSuspend, true) {
			return
		}
		t, err = h.svc.SetTargetStatus(r.Context(), tenant, h.actor(r), id, n, map[string]string{"suspend": inbound.TargetSuspended, "retire": inbound.TargetRetired}[cmd])
	default:
		writeInboundProblem(w, r, inbound.ErrTargetNotFound)
		return
	}
	if err != nil {
		writeInboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
