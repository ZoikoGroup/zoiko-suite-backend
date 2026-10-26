package handler

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/eventing/integration"
)

// ── the public batch intake (XIC-05, §10) ────────────────────────────────────

const (
	maxManifestBytes = 1 << 20
	maxFileParts     = 200
)

// BatchIntakeHandler serves POST /v1/batch-intake/{connectorKey}/{connectionID}/{operationID}: the HTTPS object channel. A
// delivery is multipart/form-data with one part named "manifest" and one part per file, each named by its file name. Like
// the ingest endpoint it sits outside the tenant and envelope middleware: the caller is an external system whose identity
// is proven by the contract's authentication profile, here over the manifest. The connection id in the path is a lookup
// key, never an authority.
type BatchIntakeHandler struct {
	svc      *batch.Service
	logger   *zap.Logger
	maxBody  int64
	trustXFF bool
}

// NewBatchIntake builds the intake handler.
func NewBatchIntake(svc *batch.Service, maxBody int64, trustXFF bool, logger *zap.Logger) *BatchIntakeHandler {
	if maxBody <= 0 {
		maxBody = batch.DefaultConfig().MaxRequestBytes
	}
	return &BatchIntakeHandler{svc: svc, maxBody: maxBody, trustXFF: trustXFF, logger: logger}
}

// Router returns the intake routes, to be mounted at /v1/batch-intake (the tenant read API owns /v1/batches).
func (h *BatchIntakeHandler) Router() http.Handler {
	r := chi.NewRouter()
	r.Post("/{connectorKey}/{connectionID}/{operationID}", h.Receive)
	return r
}

func (h *BatchIntakeHandler) remote(r *http.Request) string {
	if h.trustXFF {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	return r.RemoteAddr
}

func tooLarge() *batch.Outcome {
	return &batch.Outcome{Status: http.StatusRequestEntityTooLarge, Detail: "the delivery is larger than this endpoint accepts"}
}

func (h *BatchIntakeHandler) Receive(w http.ResponseWriter, r *http.Request) {
	// a delivery is large and its sender may be slow: the server-wide 15s deadlines are widened for this route only
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Minute))
	// bounded before anything is parsed or any secret is fetched; a delivery past the cap is refused, not truncated
	if r.ContentLength > h.maxBody {
		writeBatchProblem(w, r, tooLarge())
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		writeBatchProblem(w, r, &batch.Outcome{Status: http.StatusUnsupportedMediaType, Detail: "a batch is delivered as multipart/form-data with a manifest part and the files"})
		return
	}
	mr := multipart.NewReader(http.MaxBytesReader(w, r.Body, h.maxBody), params["boundary"])
	var manifest []byte
	var files []batch.DeliveredFile
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeBatchProblem(w, r, h.readFailure(err))
			return
		}
		name := p.FormName()
		limit := h.maxBody
		if name == "manifest" {
			limit = maxManifestBytes
		}
		data, err := io.ReadAll(io.LimitReader(p, limit+1))
		_ = p.Close()
		if err != nil {
			writeBatchProblem(w, r, h.readFailure(err))
			return
		}
		if int64(len(data)) > limit {
			writeBatchProblem(w, r, tooLarge())
			return
		}
		switch {
		case name == "manifest":
			if manifest != nil {
				writeBatchProblem(w, r, &batch.Outcome{Status: http.StatusBadRequest, Detail: "a delivery has exactly one manifest"})
				return
			}
			manifest = data
		case p.FileName() != "":
			if len(files) >= maxFileParts {
				writeBatchProblem(w, r, tooLarge())
				return
			}
			files = append(files, batch.DeliveredFile{Name: p.FileName(), Data: data})
		}
	}
	corr := r.Header.Get("X-Correlation-Id")
	if corr == "" {
		corr = r.Header.Get("X-Request-Id")
	}
	if len(corr) > 128 {
		corr = corr[:128]
	}
	o := h.svc.Receive(r.Context(), batch.Delivery{ConnectorKey: chi.URLParam(r, "connectorKey"), ConnectionID: chi.URLParam(r, "connectionID"),
		OperationID: chi.URLParam(r, "operationID"), Channel: batch.ChannelHTTPS, Header: r.Header, Manifest: manifest, Files: files,
		RemoteAddr: h.remote(r), CorrelationID: corr})
	switch {
	case o.Status == http.StatusAccepted && o.Batch != nil:
		writeJSON(w, http.StatusAccepted, batchAck(o.Batch, false))
	case o.Duplicate && o.Batch != nil:
		writeJSON(w, http.StatusOK, batchAck(o.Batch, true))
	default:
		writeBatchProblem(w, r, o)
	}
}

func (h *BatchIntakeHandler) readFailure(err error) *batch.Outcome {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return tooLarge()
	}
	return &batch.Outcome{Status: http.StatusBadRequest, Detail: "the delivery could not be read"}
}

// batchAck is what a sender is told: the batch's identity and state and nothing about its content.
func batchAck(b *batch.Batch, duplicate bool) map[string]any {
	m := map[string]any{"batch_id": b.BatchID, "external_batch_id": b.ExternalBatchID, "state": b.State, "transport": "ACCEPTED"}
	if duplicate {
		m["duplicate"] = true
	}
	return m
}

// writeBatchProblem answers a refusal with RFC 9457 problem details: a stable XIC code when the specification names
// the condition, the batch id when one was recorded, and nothing from the files or the credentials.
func writeBatchProblem(w http.ResponseWriter, r *http.Request, o *batch.Outcome) {
	var body map[string]any
	status := o.Status
	if o.XIC != nil {
		body = integration.XICProblem(o.XIC, r.Header.Get("X-Correlation-Id"))
		body["status"] = status
	} else {
		body = problemBase(r, status, "", nonEmpty(o.Detail, http.StatusText(status)))
	}
	if o.Reason != "" && o.XIC != nil && o.Batch != nil {
		body["reason_code"] = o.Reason
	}
	if o.Batch != nil {
		body["batch_id"] = o.Batch.BatchID
		body["state"] = o.Batch.State
	}
	if o.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(o.RetryAfter))
	}
	w.Header().Set("Content-Type", integration.ProblemContentType)
	w.WriteHeader(status)
	_ = encodeJSON(w, body)
}

// ── tenant management API ────────────────────────────────────────────────────

// Authorization actions of the batch and replay management API.
const (
	ActionBatchRead     = "XIC_BATCH_READ"
	ActionReplayRead    = "XIC_REPLAY_READ"
	ActionReplayRequest = "XIC_REPLAY_REQUEST"
	ActionReplayDecide  = "XIC_REPLAY_DECIDE"
	ActionReplayCancel  = "XIC_REPLAY_CANCEL"
)

// BatchAdminHandler serves the batch read API and the replay commands.
type BatchAdminHandler struct {
	svc    *batch.Service
	repo   batch.Repo
	authz  *authz.Client
	logger *zap.Logger
}

// NewBatchAdmin builds the management handler.
func NewBatchAdmin(svc *batch.Service, repo batch.Repo, az *authz.Client, logger *zap.Logger) *BatchAdminHandler {
	return &BatchAdminHandler{svc: svc, repo: repo, authz: az, logger: logger}
}

// RegisterBatchAdminRoutes mounts the management routes.
func RegisterBatchAdminRoutes(r chi.Router, h *BatchAdminHandler) {
	r.Route("/v1/batches", func(r chi.Router) {
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/files", h.Files)
		r.Get("/{id}/rows", h.Rows)
		r.Get("/{id}/events", h.Events)
	})
	r.Route("/v1/replays", func(r chi.Router) {
		r.Post("/", h.RequestReplay)
		r.Get("/", h.ListReplays)
		r.Get("/{id}", h.GetReplay)
		r.Get("/{id}/items", h.ReplayItems)
		r.Post("/{idcmd}", h.ReplayCommand)
	})
}

func (h *BatchAdminHandler) actor(r *http.Request) batch.Actor {
	return batch.Actor{PrincipalID: r.Header.Get("X-Principal-Id"), CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *BatchAdminHandler) guard(w http.ResponseWriter, r *http.Request, action string) bool {
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

func writeBatchAdminProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, batch.ErrNotFound), errors.Is(err, batch.ErrReplayNotFound):
		emit(w, http.StatusNotFound, problemBase(r, http.StatusNotFound, "", "not found"), 0)
	case errors.Is(err, batch.ErrSelfAction):
		emit(w, http.StatusForbidden, problemBase(r, http.StatusForbidden, integration.CodeAuthDenied, err.Error()), 0)
	case errors.Is(err, batch.ErrApprovalNeeded):
		body := integration.XICProblem(integration.NewXIC(integration.XICReplayApprovalRequired, err.Error()), r.Header.Get("X-Correlation-Id"))
		body["status"] = http.StatusConflict
		emit(w, http.StatusConflict, body, 0)
	case errors.Is(err, batch.ErrBadRequest), errors.Is(err, batch.ErrReplayScope):
		emit(w, http.StatusUnprocessableEntity, problemBase(r, http.StatusUnprocessableEntity, integration.CodeSchemaMismatch, err.Error()), 0)
	case errors.Is(err, batch.ErrVersionConflict), errors.Is(err, batch.ErrTransition), errors.Is(err, batch.ErrReplayState):
		emit(w, http.StatusConflict, problemBase(r, http.StatusConflict, "", err.Error()), 0)
	case errors.Is(err, batch.ErrDependency):
		w.Header().Set("Retry-After", "5")
		emit(w, http.StatusServiceUnavailable, problemBase(r, http.StatusServiceUnavailable, "", "a dependency is unavailable; retry"), 5)
	default:
		writeProblem(w, r, err)
	}
}

func (h *BatchAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionBatchRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.List(r.Context(), batch.Filter{State: strings.ToUpper(q.Get("state")), ConnectionID: q.Get("connection_id"), Limit: limit})
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": list, "total": len(list)})
}

func (h *BatchAdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionBatchRead) {
		return
	}
	b, err := h.repo.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// Files lists a batch's files. The stored bodies are never returned: the API shows what was received and what it was
// checked against, not the content.
func (h *BatchAdminHandler) Files(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionBatchRead) {
		return
	}
	fs, err := h.repo.Files(r.Context(), chi.URLParam(r, "id"), false)
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": fs, "total": len(fs)})
}

func (h *BatchAdminHandler) Rows(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionBatchRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	after, _ := strconv.Atoi(q.Get("after"))
	f := batch.RowFilter{After: after, Limit: limit}
	if limit < 1 || limit > 1000 {
		f.Limit = 1000
	}
	if s := q.Get("state"); s != "" {
		f.States = strings.Split(strings.ToUpper(s), ",")
	}
	rows, err := h.repo.Rows(r.Context(), chi.URLParam(r, "id"), f)
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": len(rows)})
}

func (h *BatchAdminHandler) Events(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionBatchRead) {
		return
	}
	ev, err := h.repo.Events(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "total": len(ev)})
}

// ── replay ───────────────────────────────────────────────────────────────────

func (h *BatchAdminHandler) RequestReplay(w http.ResponseWriter, r *http.Request) {
	var in batch.ReplayRequest
	if !decodeBig(w, r, &in) {
		return
	}
	if !h.guard(w, r, ActionReplayRequest) {
		return
	}
	j, err := h.svc.RequestReplay(r.Context(), h.actor(r), in)
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (h *BatchAdminHandler) ListReplays(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReplayRead) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.repo.ListReplays(r.Context(), batch.ReplayFilter{State: strings.ToUpper(q.Get("state")), BatchID: q.Get("batch_id"), Limit: limit})
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"replays": list, "total": len(list)})
}

func (h *BatchAdminHandler) GetReplay(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReplayRead) {
		return
	}
	j, err := h.repo.GetReplay(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (h *BatchAdminHandler) ReplayItems(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r, ActionReplayRead) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 1000 {
		limit = 1000
	}
	items, err := h.repo.ReplayItems(r.Context(), chi.URLParam(r, "id"), limit)
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

type replayCommand struct {
	Note   string `json:"note"`
	Reason string `json:"reason"`
}

// ReplayCommand decides or cancels a replay: POST /{id}:approve|:reject|:cancel. Approval and rejection are the
// checker's step and need an authorization of their own; the service and the database both refuse the requester.
func (h *BatchAdminHandler) ReplayCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd := split(chi.URLParam(r, "idcmd"))
	var b replayCommand
	if !decodeBig(w, r, &b) {
		return
	}
	var (
		j   *batch.ReplayJob
		err error
	)
	switch cmd {
	case "approve", "reject":
		if !h.guard(w, r, ActionReplayDecide) {
			return
		}
		j, err = h.svc.DecideReplay(r.Context(), h.actor(r), id, cmd == "approve", b.Note)
	case "cancel":
		if !h.guard(w, r, ActionReplayCancel) {
			return
		}
		j, err = h.svc.CancelReplay(r.Context(), h.actor(r), id, nonEmpty(b.Reason, b.Note))
	default:
		writeBatchAdminProblem(w, r, batch.ErrReplayNotFound)
		return
	}
	if err != nil {
		writeBatchAdminProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}
