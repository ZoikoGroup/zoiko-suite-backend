package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/integration"
)

// Authorization actions of the outbound API (ZS-SVC-T-001 section 13).
const (
	ActionOutboundReserve = "XIC_OUTBOUND_RESERVE"
	ActionOutboundSubmit  = "XIC_OUTBOUND_SUBMIT"
	ActionOutboundResolve = "XIC_OUTBOUND_RESOLVE"
	ActionOutboundCancel  = "XIC_OUTBOUND_CANCEL"
	ActionOutboundRead    = "XIC_OUTBOUND_READ"
)

// OutboundHandler serves the outbound attempt API: reserve, submit or resume, resolve, cancel and read.
type OutboundHandler struct {
	svc    *outbound.Service
	repo   outbound.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewOutbound builds the handler.
func NewOutbound(svc *outbound.Service, repo outbound.Repo, az *authz.Client, logger *zap.Logger) *OutboundHandler {
	return &OutboundHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterOutboundRoutes mounts the routes.
func RegisterOutboundRoutes(r chi.Router, h *OutboundHandler) {
	r.Route("/v1/outbound-attempts", func(r chi.Router) {
		r.Post("/", h.Reserve)
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/events", h.Events)
		r.Post("/{idcmd}", h.Command)
	})
}

func (h *OutboundHandler) actor(r *http.Request) outbound.Actor {
	return outbound.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *OutboundHandler) guard(w http.ResponseWriter, r *http.Request, action string) bool {
	principal := r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeProblem(w, r, errNoPrincipal)
		return false
	}
	entity := r.Header.Get("X-Legal-Entity-Id")
	if entity == "" {
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

func writeOutboundProblem(w http.ResponseWriter, r *http.Request, err error) {
	var ref *xicconn.Refusal
	switch {
	case errors.As(err, &ref):
		writeConnProblem(w, r, err)
	case errors.Is(err, outbound.ErrNotFound), errors.Is(err, xicconn.ErrNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, outbound.ErrBadRequest), errors.Is(err, outbound.ErrAuthUnsupported), errors.Is(err, outbound.ErrOperationDirection),
		errors.Is(err, outbound.ErrBodyNotHeld), errors.Is(err, outbound.ErrBodyMismatch):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, err.Error()), 0)
	case errors.Is(err, outbound.ErrPayloadConflict):
		body := problemBase(r, http.StatusConflict, integration.CodeIdempotencyConflict, err.Error())
		emit(w, http.StatusConflict, body, 0)
	case errors.Is(err, outbound.ErrSelfAction):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, outbound.ErrBlocked):
		xicProblem(w, r, integration.NewXIC(integration.XICEgressTargetBlocked, "the call was not sent: the destination or credential check refused it. The attempt is unchanged"))
	case errors.Is(err, outbound.ErrUnknownMustBeResolved), errors.Is(err, outbound.ErrTransition), errors.Is(err, outbound.ErrNotResolvable),
		errors.Is(err, outbound.ErrVersionConflict):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	case errors.Is(err, outbound.ErrDependency), errors.Is(err, xicconn.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency is unavailable; retry"), 5)
	default:
		writeProblem(w, r, err)
	}
}

// Reserve is POST /v1/outbound-attempts: durably reserve the logical operation. It answers 201 for a new attempt and
// 200 with the original for a repeat of the same domain request. Nothing is sent to the provider.
func (h *OutboundHandler) Reserve(w http.ResponseWriter, r *http.Request) {
	var in outbound.ReserveInput
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionOutboundReserve) {
		return
	}
	a, created, err := h.svc.Reserve(r.Context(), h.actor(r), in)
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, a)
}

// Command serves POST /v1/outbound-attempts/{id}:submit|resolve|cancel.
func (h *OutboundHandler) Command(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	switch cmd {
	case "submit":
		h.submit(w, r, id)
	case "resolve":
		h.resolve(w, r, id)
	case "cancel":
		h.cancel(w, r, id)
	default:
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	}
}

type submitBody struct {
	Payload json.RawMessage `json:"payload,omitempty"`
}

// submit submits or resumes the ORIGINAL attempt. The payload is needed only when the attempt did not keep its body.
func (h *OutboundHandler) submit(w http.ResponseWriter, r *http.Request, id string) {
	var b submitBody
	if !decodeBig(w, r, &b) {
		return
	}
	if !h.guard(w, r, ActionOutboundSubmit) {
		return
	}
	a, err := h.svc.Submit(r.Context(), h.actor(r), id, b.Payload)
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *OutboundHandler) resolve(w http.ResponseWriter, r *http.Request, id string) {
	var in outbound.ResolveInput
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionOutboundResolve) {
		return
	}
	a, err := h.svc.Resolve(r.Context(), h.actor(r), id, in)
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *OutboundHandler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionOutboundCancel) {
		return
	}
	a, err := h.svc.Cancel(r.Context(), h.actor(r), id, in.Reason)
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// List is GET /v1/outbound-attempts.
func (h *OutboundHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionOutboundRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.List(r.Context(), outbound.Filter{State: strings.ToUpper(q.Get("state")), ConnectionID: q.Get("connection_id"),
		DomainRequestRef: q.Get("domain_request_ref"), Limit: limit})
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": list, "total": len(list)})
}

// Get is GET /v1/outbound-attempts/{id}. The request body is never returned.
func (h *OutboundHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionOutboundRead) {
		return
	}
	a, err := h.repo.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// Events is GET /v1/outbound-attempts/{id}/events.
func (h *OutboundHandler) Events(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionOutboundRead) {
		return
	}
	ev, err := h.repo.Events(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeOutboundProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "total": len(ev)})
}
