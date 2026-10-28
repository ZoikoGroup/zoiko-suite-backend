package handler

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
	"zoiko.io/connectivity-api-bridge-svc/internal/signals"
	"zoiko.io/eventing/integration"
)

// Authorization actions of the reconciliation and signals API.
const (
	ActionReconRequest      = "XIC_RECON_REQUEST"
	ActionReconRead         = "XIC_RECON_READ"
	ActionReconDomainTotals = "XIC_RECON_DOMAIN_TOTALS"
	ActionReconClose        = "XIC_RECON_CLOSE"
	ActionVarianceUpdate    = "XIC_RECON_VARIANCE_UPDATE"
	ActionVarianceAccept    = "XIC_RECON_VARIANCE_ACCEPT"
	ActionSignalsRead       = "XIC_SIGNALS_READ"
)

// ReconHandler serves the reconciliation runs and variances (§13: POST /v1/reconciliation-runs) and the tenant signals.
type ReconHandler struct {
	svc     *recon.Service
	repo    recon.Repo
	signals signals.Source
	authz   *authz.Client
	logger  *zap.Logger
}

// NewRecon builds the handler. signals may be nil, which leaves the signals route answering 404.
func NewRecon(svc *recon.Service, repo recon.Repo, src signals.Source, az *authz.Client, logger *zap.Logger) *ReconHandler {
	return &ReconHandler{svc: svc, repo: repo, signals: src, authz: az, logger: logger}
}

// RegisterReconRoutes mounts the routes.
func RegisterReconRoutes(r chi.Router, h *ReconHandler) {
	r.Route("/v1/reconciliation-runs", func(r chi.Router) {
		r.Post("/", h.RequestRun)
		r.Get("/", h.ListRuns)
		r.Get("/{id}", h.GetRun)
		r.Get("/{id}/events", h.RunEvents)
		r.Get("/{id}/variances", h.RunVariances)
		r.Post("/{idcmd}", h.RunCommand)
	})
	r.Route("/v1/reconciliation-variances", func(r chi.Router) {
		r.Get("/", h.ListVariances)
		r.Get("/{id}", h.GetVariance)
		r.Post("/{idcmd}", h.VarianceCommand)
	})
	r.Get("/v1/integration-signals", h.Signals)
}

func (h *ReconHandler) actor(r *http.Request) recon.Actor {
	return recon.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *ReconHandler) guard(w http.ResponseWriter, r *http.Request, action string) bool {
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

func writeReconProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, recon.ErrNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, recon.ErrSelfAction):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, recon.ErrNotIndependent), errors.Is(err, recon.ErrBadRequest):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, err.Error()), 0)
	case errors.Is(err, recon.ErrVersionConflict), errors.Is(err, recon.ErrState):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	case errors.Is(err, recon.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency is unavailable; retry"), 5)
	default:
		writeProblem(w, r, err)
	}
}

// RequestRun starts a run. It is asynchronous: the answer is the run id, 202 for a new run and 200 for the same
// request made again.
func (h *ReconHandler) RequestRun(w http.ResponseWriter, r *http.Request) {
	var in recon.RunRequest
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionReconRequest) {
		return
	}
	run, existing, err := h.svc.RequestRun(r.Context(), h.actor(r), in)
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	if existing {
		writeJSON(w, http.StatusOK, run)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (h *ReconHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.ListRuns(r.Context(), recon.RunFilter{State: strings.ToUpper(q.Get("state")), ConnectionID: q.Get("connection_id"), Limit: limit})
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": list, "total": len(list)})
}

func (h *ReconHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	run, err := h.repo.GetRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *ReconHandler) RunEvents(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	ev, err := h.repo.RunEvents(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "total": len(ev)})
}

func (h *ReconHandler) RunVariances(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.repo.GetRun(r.Context(), id); err != nil {
		writeReconProblem(w, r, err)
		return
	}
	list, err := h.repo.ListVariances(r.Context(), recon.VarianceFilter{RunID: id})
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variances": list, "total": len(list)})
}

type runCommandBody struct {
	TotalAmount string `json:"total_amount"`
	Currency    string `json:"currency"`
	Reference   string `json:"reference"`
	Note        string `json:"note"`
}

// RunCommand takes POST /{id}:domain-totals (the owning domain's side of an amount comparison) and /{id}:close.
func (h *ReconHandler) RunCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b runCommandBody
	if !decodeBig(w, r, &b) {
		return
	}
	var (
		run *recon.Run
		err error
	)
	switch cmd {
	case "domain-totals":
		if !h.guard(w, r, ActionReconDomainTotals) {
			return
		}
		run, err = h.svc.SupplyDomainTotals(r.Context(), h.actor(r), id, recon.DomainTotals{TotalAmount: b.TotalAmount, Currency: b.Currency, Reference: b.Reference})
	case "close":
		if !h.guard(w, r, ActionReconClose) {
			return
		}
		run, err = h.svc.CloseRun(r.Context(), h.actor(r), id, b.Note)
	default:
		writeReconProblem(w, r, recon.ErrNotFound)
		return
	}
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *ReconHandler) ListVariances(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.ListVariances(r.Context(), recon.VarianceFilter{State: strings.ToUpper(q.Get("state")), RunID: q.Get("run_id"),
		ConnectionID: q.Get("connection_id"), Severity: strings.ToUpper(q.Get("severity")), Limit: limit})
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variances": list, "total": len(list)})
}

func (h *ReconHandler) GetVariance(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReconRead) {
		return
	}
	v, err := h.repo.GetVariance(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type varianceCommandBody struct {
	ExpectedVersion int64  `json:"expected_version"`
	CaseRef         string `json:"case_ref"`
	Resolution      string `json:"resolution"`
	Note            string `json:"note"`
}

// VarianceCommand takes POST /{id}:case (attach the case), :resolve (settle at the source) and :accept (live with the
// difference). None of them touches an exchange, attempt, batch or receipt.
func (h *ReconHandler) VarianceCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b varianceCommandBody
	if !decodeBig(w, r, &b) {
		return
	}
	var (
		v   *recon.Variance
		err error
	)
	switch cmd {
	case "case":
		if !h.guard(w, r, ActionVarianceUpdate) {
			return
		}
		v, err = h.svc.LinkCase(r.Context(), h.actor(r), id, b.CaseRef, b.ExpectedVersion)
	case "resolve":
		if !h.guard(w, r, ActionVarianceUpdate) {
			return
		}
		v, err = h.svc.Resolve(r.Context(), h.actor(r), id, b.Resolution, b.Note, b.ExpectedVersion)
	case "accept":
		if !h.guard(w, r, ActionVarianceAccept) {
			return
		}
		v, err = h.svc.Accept(r.Context(), h.actor(r), id, b.CaseRef, b.Note, b.ExpectedVersion)
	default:
		writeReconProblem(w, r, recon.ErrNotFound)
		return
	}
	if err != nil {
		writeReconProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Signals returns the tenant's operational signals over a window (default 24h, at most 31 days).
func (h *ReconHandler) Signals(w http.ResponseWriter, r *http.Request) {
	if h.signals == nil {
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
		return
	}
	if !h.guard(w, r, ActionSignalsRead) {
		return
	}
	window := 24 * time.Hour
	if s := r.URL.Query().Get("window"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Minute || d > 31*24*time.Hour {
			emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, "a window is between 1m and 744h"), 0)
			return
		}
		window = d
	}
	snap, err := h.signals.Snapshot(r.Context(), time.Now().UTC().Add(-window))
	if err != nil {
		h.logger.Error("signals", zap.Error(err))
		writeReconProblem(w, r, recon.ErrDependency)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ── platform metrics ───────────────────────────────────────────────────────

// MetricsHandler serves /metrics in the Prometheus text format: the process counters and platform-wide gauges. It
// carries no tenant identifier or payload, and it answers only a caller presenting the configured bearer token; with no
// token configured it is not mounted.
type MetricsHandler struct {
	m      *signals.Metrics
	src    signals.Source
	token  []byte
	window time.Duration
	logger *zap.Logger

	mu     sync.Mutex
	cached *signals.Snapshot
	at     time.Time
}

// NewMetrics builds the handler.
func NewMetrics(m *signals.Metrics, src signals.Source, token string, window time.Duration, logger *zap.Logger) *MetricsHandler {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &MetricsHandler{m: m, src: src, token: []byte(token), window: window, logger: logger}
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(h.token) == 0 || subtle.ConstantTimeCompare([]byte(got), h.token) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	if h.src != nil && (h.cached == nil || time.Since(h.at) > 15*time.Second) {
		if s, err := h.src.Platform(r.Context(), time.Now().UTC().Add(-h.window)); err == nil {
			h.cached, h.at = s, time.Now()
		} else {
			h.logger.Warn("platform signals", zap.Error(err))
		}
	}
	snap := h.cached
	h.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.m.Write(w, snap)
}
