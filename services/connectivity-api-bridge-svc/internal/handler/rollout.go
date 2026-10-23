package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/rollout"
	"zoiko.io/eventing/integration"
)

// Authorization actions of the staged rollout and legacy migration API.
const (
	ActionRolloutCreate   = "XIC_ROLLOUT_CREATE"
	ActionRolloutRead     = "XIC_ROLLOUT_READ"
	ActionRolloutRequest  = "XIC_ROLLOUT_REQUEST"
	ActionRolloutDecide   = "XIC_ROLLOUT_DECIDE"
	ActionRolloutRollback = "XIC_ROLLOUT_ROLLBACK"
	ActionLegacyManage    = "XIC_LEGACY_MANAGE"
	ActionLegacyRead      = "XIC_LEGACY_READ"
	ActionLegacyRegister  = "XIC_LEGACY_REGISTER"
	ActionLegacyParity    = "XIC_LEGACY_PARITY"
	ActionLegacyRequest   = "XIC_LEGACY_REQUEST"
	ActionLegacyDecide    = "XIC_LEGACY_DECIDE"
)

// RolloutHandler serves staged rollouts, their change requests and the legacy connector migration.
type RolloutHandler struct {
	svc    *rollout.Service
	repo   rollout.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewRollout builds the handler.
func NewRollout(svc *rollout.Service, repo rollout.Repo, az *authz.Client, logger *zap.Logger) *RolloutHandler {
	return &RolloutHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterRolloutRoutes mounts the routes.
func RegisterRolloutRoutes(r chi.Router, h *RolloutHandler) {
	r.Route("/v1/rollouts", func(r chi.Router) {
		r.Post("/", h.CreateRollout)
		r.Get("/", h.ListRollouts)
		r.Get("/{id}", h.GetRollout)
		r.Get("/{id}/events", h.RolloutEvents)
		r.Get("/{id}/readiness", h.RolloutReadiness)
		r.Post("/{idcmd}", h.RolloutCommand)
	})
	r.Route("/v1/rollout-requests", func(r chi.Router) {
		r.Get("/", h.ListRequests)
		r.Get("/{id}", h.GetRequest)
		r.Post("/{idcmd}", h.RequestCommand)
	})
	r.Route("/v1/legacy-connectors", func(r chi.Router) {
		r.Post("/", h.CreateLegacy)
		r.Get("/", h.ListLegacy)
		r.Get("/{id}", h.GetLegacy)
		r.Get("/{id}/events", h.LegacyEvents)
		r.Get("/{id}/readiness", h.LegacyReadiness)
		r.Get("/{id}/parity-runs", h.ListParity)
		r.Post("/{id}/exchanges", h.RegisterExchanges)
		r.Post("/{idcmd}", h.LegacyCommand)
	})
}

func (h *RolloutHandler) actor(r *http.Request) rollout.Actor {
	return rollout.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *RolloutHandler) guard(w http.ResponseWriter, r *http.Request, action string) bool {
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

func writeRolloutProblem(w http.ResponseWriter, r *http.Request, err error) {
	var nr *rollout.NotReadyError
	switch {
	case errors.As(err, &nr):
		body := problemBase(r, http.StatusConflict, "", err.Error())
		body["readiness"] = nr.Report
		emit(w, http.StatusConflict, body, 0)
	case errors.Is(err, rollout.ErrNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, rollout.ErrSelfAction):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, rollout.ErrBadRequest):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, err.Error()), 0)
	case errors.Is(err, rollout.ErrVersionConflict), errors.Is(err, rollout.ErrState), errors.Is(err, rollout.ErrExists), errors.Is(err, rollout.ErrConflict):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	case errors.Is(err, rollout.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency is unavailable; retry"), 5)
	default:
		writeProblem(w, r, err)
	}
}

func limitOf(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

// ── rollouts ───────────────────────────────────────────────────────────────────

type createRolloutBody struct {
	ConnectionID string `json:"connection_id"`
	rollout.Terms
}

func (h *RolloutHandler) CreateRollout(w http.ResponseWriter, r *http.Request) {
	var b createRolloutBody
	if !decodeBig(w, r, &b) {
		return
	}
	if !h.guard(w, r, ActionRolloutCreate) {
		return
	}
	ro, err := h.svc.Create(r.Context(), h.actor(r), b.ConnectionID, b.Terms)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ro)
}

func (h *RolloutHandler) ListRollouts(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionRolloutRead) {
		return
	}
	q := r.URL.Query()
	list, err := h.repo.ListRollouts(r.Context(), rollout.RolloutFilter{Stage: strings.ToUpper(q.Get("stage")), ConnectionID: q.Get("connection_id"), Limit: limitOf(r)})
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollouts": list, "total": len(list)})
}

func (h *RolloutHandler) GetRollout(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionRolloutRead) {
		return
	}
	ro, err := h.repo.GetRollout(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	reqs, _ := h.repo.ListRequests(r.Context(), rollout.RequestFilter{SubjectKind: rollout.KindRollout, SubjectID: ro.RolloutID, Limit: 20})
	used, _ := h.repo.UsedToday(r.Context(), ro.RolloutID, time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]any{"rollout": ro, "pilot_used_today": used, "requests": reqs})
}

func (h *RolloutHandler) RolloutEvents(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionRolloutRead) {
		return
	}
	list, err := h.repo.Events(r.Context(), rollout.KindRollout, chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": len(list)})
}

// RolloutReadiness shows the evidence for the next step without asking for it.
func (h *RolloutHandler) RolloutReadiness(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionRolloutRead) {
		return
	}
	ro, err := h.repo.GetRollout(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	rep, err := h.svc.Readiness(r.Context(), ro)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stage": ro.Stage, "readiness": rep})
}

type rolloutCommandBody struct {
	Attestations    []string `json:"attestations"`
	Reason          string   `json:"reason"`
	ExpectedVersion int64    `json:"expected_version"`
}

// RolloutCommand takes POST /{id}:request-advance and /{id}:rollback.
func (h *RolloutHandler) RolloutCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b rolloutCommandBody
	if !decodeBig(w, r, &b) {
		return
	}
	switch cmd {
	case "request-advance":
		if !h.guard(w, r, ActionRolloutRequest) {
			return
		}
		q, err := h.svc.RequestAdvance(r.Context(), h.actor(r), id, b.Attestations, b.Reason)
		if err != nil {
			writeRolloutProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, q)
	case "rollback":
		if !h.guard(w, r, ActionRolloutRollback) {
			return
		}
		ro, err := h.svc.Rollback(r.Context(), h.actor(r), id, b.Reason, b.ExpectedVersion)
		if err != nil {
			writeRolloutProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, ro)
	default:
		writeRolloutProblem(w, r, rollout.ErrNotFound)
	}
}

// ── change requests ────────────────────────────────────────────────────────────

func (h *RolloutHandler) ListRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := strings.ToUpper(q.Get("subject_kind"))
	if !h.guard(w, r, readActionFor(kind)) {
		return
	}
	list, err := h.repo.ListRequests(r.Context(), rollout.RequestFilter{SubjectKind: kind, SubjectID: q.Get("subject_id"), Status: strings.ToUpper(q.Get("status")), Limit: limitOf(r)})
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": list, "total": len(list)})
}

// readActionFor picks the read permission: a list that names no subject kind needs both.
func readActionFor(kind string) string {
	if kind == rollout.KindLegacy {
		return ActionLegacyRead
	}
	return ActionRolloutRead
}

func (h *RolloutHandler) GetRequest(w http.ResponseWriter, r *http.Request) {
	q, err := h.repo.GetRequest(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	if !h.guard(w, r, readActionFor(q.SubjectKind)) {
		return
	}
	writeJSON(w, http.StatusOK, q)
}

type decisionBody struct {
	Note string `json:"note"`
}

// RequestCommand takes POST /{id}:approve and /{id}:reject. The permission is the one of the request's subject.
func (h *RolloutHandler) RequestCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b decisionBody
	if !decodeBig(w, r, &b) {
		return
	}
	if cmd != "approve" && cmd != "reject" {
		writeRolloutProblem(w, r, rollout.ErrNotFound)
		return
	}
	q, err := h.repo.GetRequest(r.Context(), id)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	action := ActionRolloutDecide
	if q.SubjectKind == rollout.KindLegacy {
		action = ActionLegacyDecide
	}
	if !h.guard(w, r, action) {
		return
	}
	done, err := h.svc.Decide(r.Context(), h.actor(r), id, cmd == "approve", b.Note)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, done)
}

// ── legacy connectors ──────────────────────────────────────────────────────────

func (h *RolloutHandler) CreateLegacy(w http.ResponseWriter, r *http.Request) {
	var b rollout.LegacyInput
	if !decodeBig(w, r, &b) {
		return
	}
	if !h.guard(w, r, ActionLegacyManage) {
		return
	}
	l, err := h.svc.RegisterLegacy(r.Context(), h.actor(r), b)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (h *RolloutHandler) ListLegacy(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionLegacyRead) {
		return
	}
	list, err := h.repo.ListLegacy(r.Context(), rollout.LegacyFilter{State: strings.ToUpper(r.URL.Query().Get("state")), Limit: limitOf(r)})
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"legacy_connectors": list, "total": len(list)})
}

func (h *RolloutHandler) GetLegacy(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionLegacyRead) {
		return
	}
	l, err := h.repo.GetLegacy(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *RolloutHandler) LegacyEvents(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionLegacyRead) {
		return
	}
	list, err := h.repo.Events(r.Context(), rollout.KindLegacy, chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": list, "total": len(list)})
}

func (h *RolloutHandler) LegacyReadiness(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionLegacyRead) {
		return
	}
	rep, to, err := h.svc.LegacyReadiness(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"next_state": to, "readiness": rep})
}

func (h *RolloutHandler) ListParity(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionLegacyRead) {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.repo.GetLegacy(r.Context(), id); err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	list, err := h.repo.ListParity(r.Context(), id, limitOf(r))
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"parity_runs": list, "total": len(list)})
}

type registerBody struct {
	Exchanges []rollout.LegacyExchange `json:"exchanges"`
}

// RegisterExchanges is how the owning service puts its attempts into the generic history.
func (h *RolloutHandler) RegisterExchanges(w http.ResponseWriter, r *http.Request) {
	var b registerBody
	if !decodeBig(w, r, &b) {
		return
	}
	if !h.guard(w, r, ActionLegacyRegister) {
		return
	}
	stored, dups, err := h.svc.RegisterExchanges(r.Context(), h.actor(r), chi.URLParam(r, "id"), b.Exchanges)
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": stored, "duplicates": dups})
}

type legacyCommandBody struct {
	ConnectionID    string    `json:"connection_id"`
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	Reason          string    `json:"reason"`
	ExpectedVersion int64     `json:"expected_version"`
}

// LegacyCommand takes POST /{id}:map, :start-shadow, :stop-shadow, :parity-run, :request-advance and :revert-cutover.
func (h *RolloutHandler) LegacyCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b legacyCommandBody
	if !decodeBig(w, r, &b) {
		return
	}
	a := h.actor(r)
	var (
		out any
		err error
	)
	switch cmd {
	case "map":
		if !h.guard(w, r, ActionLegacyManage) {
			return
		}
		out, err = h.svc.MapLegacy(r.Context(), a, id, b.ConnectionID, b.ExpectedVersion)
	case "start-shadow":
		if !h.guard(w, r, ActionLegacyManage) {
			return
		}
		out, err = h.svc.StartShadow(r.Context(), a, id, b.ExpectedVersion)
	case "stop-shadow":
		if !h.guard(w, r, ActionLegacyManage) {
			return
		}
		out, err = h.svc.StopShadow(r.Context(), a, id, b.ExpectedVersion)
	case "revert-cutover":
		if !h.guard(w, r, ActionLegacyManage) {
			return
		}
		out, err = h.svc.RevertCutover(r.Context(), a, id, b.Reason, b.ExpectedVersion)
	case "parity-run":
		if !h.guard(w, r, ActionLegacyParity) {
			return
		}
		var p *rollout.ParityRun
		p, err = h.svc.RunParity(r.Context(), a, id, rollout.ParityInput{From: b.From, To: b.To})
		if err == nil {
			writeJSON(w, http.StatusCreated, p)
			return
		}
	case "request-advance":
		if !h.guard(w, r, ActionLegacyRequest) {
			return
		}
		var q *rollout.Request
		q, err = h.svc.RequestLegacyAdvance(r.Context(), a, id, b.Reason)
		if err == nil {
			writeJSON(w, http.StatusAccepted, q)
			return
		}
	default:
		writeRolloutProblem(w, r, rollout.ErrNotFound)
		return
	}
	if err != nil {
		writeRolloutProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
