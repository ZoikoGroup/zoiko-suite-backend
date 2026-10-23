package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/integration"
)

// Authorization actions of the connection API (XIC-02). They are checked against the caller's legal entity.
const (
	ActionConnCreate        = "XIC_CONNECTION_CREATE"
	ActionConnAuthorize     = "XIC_CONNECTION_AUTHORIZE"
	ActionConnBind          = "XIC_CONNECTION_BIND"
	ActionConnValidate      = "XIC_CONNECTION_VALIDATE"
	ActionConnProbe         = "XIC_CONNECTION_PROBE"
	ActionConnCertify       = "XIC_CONNECTION_CERTIFY"
	ActionConnActivate      = "XIC_CONNECTION_ACTIVATE"
	ActionConnSuspend       = "XIC_CONNECTION_SUSPEND"
	ActionConnResume        = "XIC_CONNECTION_RESUME"
	ActionConnRevoke        = "XIC_CONNECTION_REVOKE"
	ActionConnRotate        = "XIC_CONNECTION_ROTATE"
	ActionConnScopes        = "XIC_CONNECTION_SCOPES_REPORT"
	ActionConnEvaluate      = "XIC_CONNECTION_EVALUATE"
	ActionConnMigrate       = "XIC_CONNECTION_MIGRATE_REQUEST"
	ActionConnMigrateDecide = "XIC_CONNECTION_MIGRATE_DECIDE"
	ActionConnBreakGlass    = "XIC_CONNECTION_BREAK_GLASS_REQUEST"
	ActionConnBreakDecide   = "XIC_CONNECTION_BREAK_GLASS_DECIDE"
	ActionConnRead          = "XIC_CONNECTION_READ"
)

// ConnectionHandler serves the §13 integration-connection API.
type ConnectionHandler struct {
	svc    *xicconn.Service
	repo   xicconn.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewConnection builds the connection handler.
func NewConnection(svc *xicconn.Service, repo xicconn.Repo, az *authz.Client, logger *zap.Logger) *ConnectionHandler {
	return &ConnectionHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterConnectionRoutes mounts the API.
func RegisterConnectionRoutes(r chi.Router, h *ConnectionHandler) {
	r.Route("/v1/integration-connections", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Post("/{idcmd}", h.Command)
		r.Get("/{id}/checks", h.Checks)
		r.Get("/{id}/certifications", h.Certifications)
		r.Get("/{id}/events", h.Events)
		r.Get("/{id}/requests", h.Requests)
		r.Get("/{id}/operability", h.Operability)
		r.Post("/{id}/requests/{rcmd}", h.DecideRequest)
	})
}

func (h *ConnectionHandler) actor(r *http.Request) xicconn.Actor {
	return xicconn.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

// guard resolves the caller and authorizes action for the caller's legal entity.
func (h *ConnectionHandler) guard(w http.ResponseWriter, r *http.Request, action string) bool {
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

func xicProblem(w http.ResponseWriter, r *http.Request, e *integration.XICError) {
	trace := ""
	if r != nil {
		trace = r.Header.Get("X-Correlation-ID")
	}
	body := integration.XICProblem(e, trace)
	if r != nil {
		body["instance"] = r.URL.Path
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(e.Code.Info().Status)
	_ = encodeJSON(w, body)
}

// writeConnProblem maps the connection layer's errors. Refusals that the specification names carry their XIC
// code; the rest are ordinary problems.
func writeConnProblem(w http.ResponseWriter, r *http.Request, err error) {
	var ref *xicconn.Refusal
	var val *xicconn.Validation
	switch {
	case errors.As(err, &ref):
		xicProblem(w, r, integration.NewXIC(integration.XICCode(ref.Code), ref.Detail).WithConnection(ref.ConnectionID))
	case errors.As(err, &val):
		body := problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, "the request does not satisfy the connection rules")
		body["issues"] = val.Issues
		emit(w, http.StatusUnprocessableEntity, body, 0)
	case errors.Is(err, xicconn.ErrNotFound), errors.Is(err, xicconn.ErrRequestNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, xicconn.ErrCrossTenantCredential):
		xicProblem(w, r, integration.NewXIC(integration.XICTenantScopeMismatch, "the credential reference is already bound elsewhere"))
	case errors.Is(err, xicconn.ErrNotCertified):
		xicProblem(w, r, integration.NewXIC(integration.XICCertificationStale, err.Error()))
	case errors.Is(err, xicconn.ErrContractUnavailable):
		xicProblem(w, r, integration.NewXIC(integration.XICContractVersionMismatch, err.Error()))
	case errors.Is(err, xicconn.ErrRegionDenied):
		xicProblem(w, r, integration.NewXIC(integration.XICResidencyRouteBlocked, "the region is not permitted for this connection"))
	case errors.Is(err, xicconn.ErrEgressDenied):
		xicProblem(w, r, integration.NewXIC(integration.XICEgressTargetBlocked, "the destination is not an allowed egress target"))
	case errors.Is(err, xicconn.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency needed to check this change is unavailable; retry"), 5)
	case errors.Is(err, xicconn.ErrSelfAction), errors.Is(err, xicconn.ErrBreakGlassDenied):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, xicconn.ErrCredentialExpired):
		emit(w, http.StatusUnauthorized, problemBase(r, http.StatusUnauthorized, integration.CodeCredentialInvalid, err.Error()), 0)
	case errors.Is(err, xicconn.ErrCredentialInvalid), errors.Is(err, xicconn.ErrScopesInsufficient), errors.Is(err, xicconn.ErrExcessScopes),
		errors.Is(err, xicconn.ErrAuthState), errors.Is(err, xicconn.ErrRedirectNotRegistered):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeCredentialInvalid, err.Error()), 0)
	case errors.Is(err, xicconn.ErrVersionConflict), errors.Is(err, xicconn.ErrIdempotency):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, integration.CodeIdempotencyConflict, err.Error()), 0)
	case errors.Is(err, xicconn.ErrExists), errors.Is(err, xicconn.ErrTransition), errors.Is(err, xicconn.ErrNotValidated),
		errors.Is(err, xicconn.ErrRequestState), errors.Is(err, xicconn.ErrRotation):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	default:
		writeProblem(w, r, err)
	}
}

// view removes what a caller does not need from a connection: only the tail of a credential reference is shown.
func view(c *xicconn.Connection) *xicconn.Connection {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Bindings = make([]xicconn.Binding, len(c.Bindings))
	for i, b := range c.Bindings {
		b.CurrentRef = mask(b.CurrentRef)
		b.PreviousRef = mask(b.PreviousRef)
		cp.Bindings[i] = b
	}
	return &cp
}

func mask(ref string) string {
	if ref == "" {
		return ""
	}
	if len(ref) <= 6 {
		return strings.Repeat("*", len(ref))
	}
	return "…" + ref[len(ref)-6:]
}

// ── create and read ──────────────────────────────────────────────────────────

func (h *ConnectionHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in xicconn.CreateInput
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionConnCreate) {
		return
	}
	c, created, err := h.svc.Create(r.Context(), h.actor(r), in, idempotencyKey(r))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, view(c))
}

func (h *ConnectionHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := xicconn.Filter{Status: strings.ToUpper(q.Get("status")), ConnectorID: q.Get("connector_id"), Environment: strings.ToUpper(q.Get("environment"))}
	list, err := h.repo.List(r.Context(), f)
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	out := make([]*xicconn.Connection, 0, len(list))
	for i := range list {
		out = append(out, view(&list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out, "total": len(out)})
}

func (h *ConnectionHandler) Get(w http.ResponseWriter, r *http.Request) {
	c, err := h.repo.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view(c))
}

func (h *ConnectionHandler) Checks(w http.ResponseWriter, r *http.Request) {
	list, err := h.repo.ListChecks(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": list, "total": len(list)})
}

func (h *ConnectionHandler) Certifications(w http.ResponseWriter, r *http.Request) {
	list, err := h.repo.ListCertifications(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"certifications": list, "total": len(list)})
}

func (h *ConnectionHandler) Events(w http.ResponseWriter, r *http.Request) {
	list, err := h.repo.ListEvents(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": len(list)})
}

func (h *ConnectionHandler) Requests(w http.ResponseWriter, r *http.Request) {
	list, err := h.repo.ListRequests(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": list, "total": len(list)})
}

// Operability answers whether the caller's tenant may run an operation of this connection now. It never returns a
// credential reference.
func (h *ConnectionHandler) Operability(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionConnRead) {
		return
	}
	op := r.URL.Query().Get("operation")
	if op == "" {
		writeConnProblem(w, r, &xicconn.Validation{Issues: []registry.Issue{{Path: "operation", Rule: "required"}}})
		return
	}
	d, err := h.svc.Operable(r.Context(), middleware.GetTenantID(r.Context()), chi.URLParam(r, "id"), op)
	if err != nil {
		// over HTTP another tenant's connection is indistinguishable from a missing one; the XIC-008 signal was
		// already raised for operators, and server-side callers of Operable get the code itself
		var ref *xicconn.Refusal
		if errors.As(err, &ref) && ref.Code == xicconn.CodeTenantScope {
			err = xicconn.ErrNotFound
		}
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operable": true, "connection_id": d.ConnectionID, "environment": d.Environment,
		"contract_version_id": d.ContractVersionID, "operation_id": d.Operation.ID, "direction": d.Operation.Direction, "region": d.Region,
		"host": d.Host, "degraded": d.Degraded, "credential_bound": len(d.CredentialRefs) > 0})
}

// ── commands ─────────────────────────────────────────────────────────────────

type commandBody struct {
	ExpectedVersion int64 `json:"expected_version"`
	// per-command payloads, decoded from the same object
	xicconn.AuthorizeInput
	Bind        *xicconn.BindInput       `json:"bind,omitempty"`
	Probe       *xicconn.ProbeInput      `json:"probe,omitempty"`
	Certify     *xicconn.CertifyInput    `json:"certify,omitempty"`
	Rotate      *xicconn.RotateInput     `json:"rotate,omitempty"`
	Migration   *xicconn.MigrationInput  `json:"migration,omitempty"`
	BreakGlass  *xicconn.BreakGlassInput `json:"break_glass,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	AuthProfile string                   `json:"auth_profile,omitempty"`
	Scopes      []string                 `json:"scopes,omitempty"`
}

var connCommands = map[string]string{
	"authorize": ActionConnAuthorize, "bind": ActionConnBind, "validate": ActionConnValidate, "probe": ActionConnProbe,
	"certify": ActionConnCertify, "activate": ActionConnActivate, "suspend": ActionConnSuspend, "resume": ActionConnResume,
	"revoke": ActionConnRevoke, "rotate": ActionConnRotate, "end-grace": ActionConnRotate, "report-scopes": ActionConnScopes,
	"evaluate": ActionConnEvaluate, "migrate": ActionConnMigrate, "break-glass": ActionConnBreakGlass,
}

func (h *ConnectionHandler) Command(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	action, ok := connCommands[cmd]
	if !ok {
		writeConnProblem(w, r, xicconn.ErrNotFound)
		return
	}
	var b commandBody
	if !decodeBig(w, r, &b) {
		return
	}
	n, err := expectedVersion(r, b.ExpectedVersion)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	needsVersion := cmd != "probe" && cmd != "certify" && cmd != "evaluate" && cmd != "migrate"
	if needsVersion && n < 1 && !(cmd == "break-glass") {
		writeProblem(w, r, errNeedVersion)
		return
	}
	if !h.guard(w, r, action) {
		return
	}
	a, ctx := h.actor(r), r.Context()
	reply := func(c *xicconn.Connection, err error) {
		if err != nil {
			writeConnProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, view(c))
	}
	need := func(ok bool, field string) bool {
		if !ok {
			writeConnProblem(w, r, &xicconn.Validation{Issues: []registry.Issue{{Path: field, Rule: "required for this command"}}})
		}
		return ok
	}
	switch cmd {
	case "authorize":
		res, err := h.svc.Authorize(ctx, a, id, n, b.AuthorizeInput)
		if err != nil {
			writeConnProblem(w, r, err)
			return
		}
		res.Connection = view(res.Connection)
		writeJSON(w, http.StatusOK, res)
	case "bind":
		if need(b.Bind != nil, "bind") {
			c, err := h.svc.Bind(ctx, a, id, n, *b.Bind)
			reply(view2(c), err)
		}
	case "validate":
		res, err := h.svc.Validate(ctx, a, id, n, idempotencyKey(r))
		var val *xicconn.Validation
		if err != nil && errors.As(err, &val) && res != nil {
			body := problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, "the connection did not pass validation")
			body["issues"], body["check_id"] = val.Issues, res.Check.CheckID
			emit(w, http.StatusUnprocessableEntity, body, 0)
			return
		}
		if err != nil {
			writeConnProblem(w, r, err)
			return
		}
		res.Connection = view(res.Connection)
		writeJSON(w, http.StatusOK, res)
	case "probe":
		if need(b.Probe != nil, "probe") {
			ck, err := h.svc.ReportProbe(ctx, a, id, n, *b.Probe)
			if err != nil {
				writeConnProblem(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, ck)
		}
	case "certify":
		if need(b.Certify != nil, "certify") {
			cert, err := h.svc.Certify(ctx, a, id, *b.Certify)
			if err != nil {
				writeConnProblem(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, cert)
		}
	case "activate":
		c, err := h.svc.Activate(ctx, a, id, n)
		reply(view2(c), err)
	case "suspend":
		c, err := h.svc.Suspend(ctx, a, id, n, b.Reason)
		reply(view2(c), err)
	case "resume":
		c, err := h.svc.Resume(ctx, a, id, n)
		reply(view2(c), err)
	case "revoke":
		c, err := h.svc.Revoke(ctx, a, id, n, b.Reason)
		reply(view2(c), err)
	case "rotate":
		if need(b.Rotate != nil, "rotate") {
			c, err := h.svc.Rotate(ctx, a, id, n, *b.Rotate)
			reply(view2(c), err)
		}
	case "end-grace":
		if need(b.AuthProfile != "", "auth_profile") {
			c, err := h.svc.EndGrace(ctx, a, id, n, b.AuthProfile)
			reply(view2(c), err)
		}
	case "report-scopes":
		if need(b.AuthProfile != "", "auth_profile") {
			c, err := h.svc.ReportScopes(ctx, a, id, n, b.AuthProfile, b.Scopes)
			reply(view2(c), err)
		}
	case "evaluate":
		res, err := h.svc.Evaluate(ctx, a, id)
		if err != nil {
			writeConnProblem(w, r, err)
			return
		}
		res.Connection = view(res.Connection)
		writeJSON(w, http.StatusOK, res)
	case "migrate":
		if need(b.Migration != nil, "migration") {
			req, err := h.svc.RequestMigration(ctx, a, id, *b.Migration)
			if err != nil {
				writeConnProblem(w, r, err)
				return
			}
			writeJSON(w, http.StatusAccepted, req)
		}
	case "break-glass":
		if need(b.BreakGlass != nil, "break_glass") {
			req, c, err := h.svc.RequestBreakGlass(ctx, a, id, n, *b.BreakGlass)
			if err != nil {
				writeConnProblem(w, r, err)
				return
			}
			if req != nil {
				writeJSON(w, http.StatusAccepted, req)
				return
			}
			writeJSON(w, http.StatusOK, view(c))
		}
	}
}

// view2 is view for a possibly-nil connection returned alongside an error.
func view2(c *xicconn.Connection) *xicconn.Connection { return view(c) }

// DecideRequest approves or rejects a migration or break-glass request: POST /{id}/requests/{rid}:approve|:reject.
func (h *ConnectionHandler) DecideRequest(w http.ResponseWriter, r *http.Request) {
	rid, cmd := split(chi.URLParam(r, "rcmd"))
	if cmd != "approve" && cmd != "reject" {
		writeConnProblem(w, r, xicconn.ErrRequestNotFound)
		return
	}
	id := chi.URLParam(r, "id")
	reqs, err := h.repo.ListRequests(r.Context(), id)
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	var kind string
	for _, q := range reqs {
		if q.RequestID == rid {
			kind = q.Kind
		}
	}
	if kind == "" {
		writeConnProblem(w, r, xicconn.ErrRequestNotFound)
		return
	}
	action := ActionConnMigrateDecide
	if kind == xicconn.RequestBreakGlass {
		action = ActionConnBreakDecide
	}
	if !h.guard(w, r, action) {
		return
	}
	var out *xicconn.Request
	if kind == xicconn.RequestMigration {
		out, err = h.svc.DecideMigration(r.Context(), h.actor(r), id, rid, cmd == "approve")
	} else {
		out, err = h.svc.DecideBreakGlass(r.Context(), h.actor(r), id, rid, cmd == "approve")
	}
	if err != nil {
		writeConnProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var _ = time.Second
