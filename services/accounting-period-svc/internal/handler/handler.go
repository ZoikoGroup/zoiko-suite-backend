// Package handler is the HTTP surface of accounting-period-svc (REF-05).
//
// Named commands use the spec's colon form (POST /v1/accounting-periods/{id}:hard-close).
// chi matches the whole path segment, so the segment is split on its LAST colon
// here rather than relying on router-specific suffix matching.
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/accounting-period-svc/internal/domain"
	svcmiddleware "zoiko.io/accounting-period-svc/internal/middleware"
	"zoiko.io/accounting-period-svc/internal/service"
	"zoiko.io/accounting-period-svc/internal/telemetry"
)

// AuthZClient asks authorization-svc whether a principal may perform an action.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Authorization actions asked of authorization-svc. PERIOD_STATE_COMMAND is the
// "period state command permission" of spec REF-05; PERIOD_MATERIALIZE is this
// service's addition for creating period instances (see SPEC_DEVIATIONS.md).
const (
	ActionStateCommand   = "PERIOD_STATE_COMMAND"
	ActionMaterialize    = "PERIOD_MATERIALIZE"
	PlatformScopeID      = "00000000-0000-0000-0000-00000000f001"
	maxBodyBytes         = 1 << 20
	defaultPageLimit     = 100
	maxPageLimit         = 500
	headerIdempotencyKey = "Idempotency-Key"
)

// Handler serves the REF-05 API.
type Handler struct {
	svc     *service.Service
	authz   AuthZClient
	log     *zap.Logger
	metrics *telemetry.Domain
}

// New builds the handler. metrics may be nil (tests).
func New(svc *service.Service, authz AuthZClient, log *zap.Logger, metrics *telemetry.Domain) *Handler {
	return &Handler{svc: svc, authz: authz, log: log, metrics: metrics}
}

// RegisterRoutes mounts the API.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/accounting-periods:materialize", h.Materialize)
	r.Get("/v1/accounting-periods:resolve", h.Resolve)
	r.Get("/v1/accounting-periods:status-by-key", h.StatusByKey)
	r.Get("/v1/accounting-periods", h.ListPeriods)
	r.Get("/v1/accounting-periods/{id}", h.GetPeriod)
	r.Get("/v1/accounting-periods/{id}/state-history", h.StateHistory)
	r.Post("/v1/accounting-periods/{idcmd}", h.PeriodCommand)
	r.Get("/v1/calendar-usage", h.CalendarUsage)
}

// ── request context ──────────────────────────────────────────────────────────

type reqCtx struct {
	tenant, actor string
	meta          service.Meta
}

func (h *Handler) context(w http.ResponseWriter, r *http.Request, needIdempotency bool) (reqCtx, bool) {
	var rc reqCtx
	rc.tenant = svcmiddleware.TenantFromContext(r.Context())
	if rc.tenant == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "tenant context missing (X-Tenant-Id)")
		return rc, false
	}
	rc.actor = r.Header.Get("X-Principal-Id")
	if rc.actor == "" {
		rc.actor = r.Header.Get("X-Workload-Id")
	}
	if rc.actor == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "caller identity missing (X-Principal-Id or X-Workload-Id)")
		return rc, false
	}
	corr := r.Header.Get("X-Correlation-ID")
	if corr == "" {
		corr = chimw.GetReqID(r.Context())
	}
	rc.meta = service.Meta{
		Actor: rc.actor, TenantID: rc.tenant, CorrelationID: corr, CausationID: r.Header.Get("X-Causation-Id"),
		IdempotencyKey: r.Header.Get(headerIdempotencyKey),
	}
	if needIdempotency && rc.meta.IdempotencyKey == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "Idempotency-Key header is required for commands")
		return rc, false
	}
	return rc, true
}

// tenantOnly is the context check for reads (no principal needed: the gate is
// called service to service).
func (h *Handler) tenantOnly(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := svcmiddleware.TenantFromContext(r.Context())
	if t == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "tenant context missing (X-Tenant-Id)")
		return "", false
	}
	return t, true
}

func readBody(w http.ResponseWriter, r *http.Request, op string) ([]byte, string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "request body unreadable or too large")
		return nil, "", false
	}
	sum := sha256.Sum256(append([]byte(op+"\n"), b...))
	return b, hex.EncodeToString(sum[:]), true
}

func decodeStrict(w http.ResponseWriter, body []byte, dst any) bool {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// authorize asks authorization-svc. It writes the refusal and reports false on
// denial (403 FORBIDDEN) or when the dependency is down (503
// DEPENDENCY_UNAVAILABLE, fail closed).
func (h *Handler) authorize(w http.ResponseWriter, ctx context.Context, rc reqCtx, legalEntityID, action, command string) bool {
	err := h.authz.CheckAllowed(ctx, rc.actor, legalEntityID, action)
	switch {
	case err == nil:
		h.countAuthz(action, telemetry.AuthZGranted)
		return true
	case errors.Is(err, domain.ErrAuthorizationDenied):
		h.countAuthz(action, telemetry.AuthZDenied)
		h.countCmd(command, telemetry.OutcomeForbidden)
		writeErr(w, http.StatusForbidden, domain.CodeForbidden, "not permitted to perform "+action)
	default:
		h.countAuthz(action, telemetry.AuthZUnavailable)
		h.countCmd(command, telemetry.OutcomeUnavailable)
		h.log.Error("authorization-svc unavailable", zap.Error(err))
		writeErr(w, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "authorization-svc unavailable; failing closed")
	}
	return false
}

// ── POST /v1/accounting-periods:materialize ──────────────────────────────────

type materializeRequest struct {
	LegalEntityID     string `json:"legal_entity_id"`
	CalendarID        string `json:"calendar_id"`
	FiscalYear        int    `json:"fiscal_year"`
	BookScope         string `json:"book_scope"`
	CalendarVersionID string `json:"calendar_version_id"`
	ResolveDate       string `json:"resolve_date"`
	Reason            string `json:"reason"`
}

// Materialize is MaterializePeriods.
func (h *Handler) Materialize(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(telemetry.CmdMaterialize, telemetry.OutcomeInvalid)
		return
	}
	body, hash, ok := readBody(w, r, "POST /v1/accounting-periods:materialize")
	if !ok {
		return
	}
	var req materializeRequest
	if !decodeStrict(w, body, &req) {
		h.countCmd(telemetry.CmdMaterialize, telemetry.OutcomeInvalid)
		return
	}
	if req.Reason == "" || req.LegalEntityID == "" {
		h.countCmd(telemetry.CmdMaterialize, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason and legal_entity_id are required")
		return
	}
	if !h.authorize(w, r.Context(), rc, req.LegalEntityID, ActionMaterialize, telemetry.CmdMaterialize) {
		return
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, req.Reason
	res, replayed, err := h.svc.MaterializePeriods(r.Context(), service.MaterializeInput{
		Meta: meta, LegalEntityID: req.LegalEntityID, CalendarID: req.CalendarID, FiscalYear: req.FiscalYear,
		BookScope: req.BookScope, CalendarVersionID: req.CalendarVersionID, ResolveDate: req.ResolveDate,
	})
	if err != nil {
		h.fail(w, telemetry.CmdMaterialize, err)
		return
	}
	status := http.StatusOK
	switch {
	case replayed:
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(telemetry.CmdMaterialize, telemetry.OutcomeReplayed)
	default:
		h.countCmd(telemetry.CmdMaterialize, telemetry.OutcomeOK)
		if res.Created > 0 {
			status = http.StatusCreated
		}
	}
	writeJSON(w, status, res)
}

// ── POST /v1/accounting-periods/{id}:request-soft-close|:hard-close|:authorize-reopen|:reclose ──

type commandRequest struct {
	ExpectedVersion    *int64     `json:"expected_version"`
	Reason             string     `json:"reason"`
	Acc14WorkflowRef   string     `json:"acc14_workflow_ref"`
	ControlSnapshotRef string     `json:"control_snapshot_ref"`
	ReopenScope        *scopeBody `json:"reopen_scope"`
	ExpiresAt          *time.Time `json:"expires_at"`
}

type scopeBody struct {
	BookScope   string `json:"book_scope"`
	ModuleScope string `json:"module_scope"`
}

var commandByName = map[string]struct {
	cmd    domain.Command
	metric string
}{
	"request-soft-close": {domain.CmdSoftClose, telemetry.CmdSoftClose},
	"hard-close":         {domain.CmdHardClose, telemetry.CmdHardClose},
	"authorize-reopen":   {domain.CmdAuthorizeReopen, telemetry.CmdAuthorizeReopen},
	"reclose":            {domain.CmdReclose, telemetry.CmdReclose},
}

func splitCommand(seg string) (id, cmd string, ok bool) {
	i := strings.LastIndex(seg, ":")
	if i <= 0 || i == len(seg)-1 {
		return "", "", false
	}
	return seg[:i], seg[i+1:], true
}

// PeriodCommand dispatches the colon-suffixed state commands.
//
// Deliberately, the handler does NOT reject missing ACC-14 refs itself: the
// service does, so that the rejection is recorded as a PeriodCommandRejected
// control event (negative paths 30 and 31).
func (h *Handler) PeriodCommand(w http.ResponseWriter, r *http.Request) {
	id, name, ok := splitCommand(chi.URLParam(r, "idcmd"))
	spec, known := commandByName[name]
	if !ok || !known {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected {id}:request-soft-close, :hard-close, :authorize-reopen or :reclose")
		return
	}
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(spec.metric, telemetry.OutcomeInvalid)
		return
	}
	body, hash, ok := readBody(w, r, "POST /v1/accounting-periods/"+id+":"+name)
	if !ok {
		return
	}
	var req commandRequest
	if !decodeStrict(w, body, &req) {
		h.countCmd(spec.metric, telemetry.OutcomeInvalid)
		return
	}
	ev := int64(0)
	if req.ExpectedVersion != nil {
		ev = *req.ExpectedVersion
	} else if v := r.Header.Get("X-Expected-Version"); v != "" {
		ev, _ = strconv.ParseInt(v, 10, 64)
	}
	if ev < 1 {
		h.countCmd(spec.metric, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "expected_version is required (body field or X-Expected-Version) and must be >= 1")
		return
	}
	if req.Reason == "" {
		h.countCmd(spec.metric, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason is required")
		return
	}
	// The period's legal entity is the authorization scope.
	p, err := h.svc.GetPeriod(r.Context(), rc.tenant, id)
	if err != nil {
		h.fail(w, spec.metric, err)
		return
	}
	if !h.authorize(w, r.Context(), rc, p.LegalEntityID, ActionStateCommand, spec.metric) {
		return
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, req.Reason
	in := service.CommandInput{
		Meta: meta, PeriodID: id, Command: spec.cmd, ExpectedVersion: ev,
		Acc14WorkflowRef: req.Acc14WorkflowRef, ControlSnapshotRef: req.ControlSnapshotRef,
	}
	if spec.cmd == domain.CmdAuthorizeReopen {
		in.Reopen = &service.ReopenRequest{ExpiresAt: req.ExpiresAt}
		if req.ReopenScope != nil {
			in.Reopen.BookScope, in.Reopen.ModuleScope = req.ReopenScope.BookScope, req.ReopenScope.ModuleScope
		} else {
			in.Reopen.ExpiresAt = nil // a reopen without a scope is refused by the service
		}
	}
	res, replayed, err := h.svc.Command(r.Context(), in)
	if err != nil {
		h.fail(w, spec.metric, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(spec.metric, telemetry.OutcomeReplayed)
	} else {
		h.countCmd(spec.metric, telemetry.OutcomeOK)
	}
	w.Header().Set("ETag", etag(res))
	writeJSON(w, http.StatusOK, res)
}

// ── queries ──────────────────────────────────────────────────────────────────

// Resolve is the POSTING GATE (ResolvePeriodByDate):
// GET /v1/accounting-periods:resolve?legal_entity_id=&date=&book_scope=&module_scope=&purpose=post
//
// Every response, success or failure, carries Cache-Control: no-store, and a
// failure body always includes "posting_allowed": false. There is no path on
// which an error is read as permission.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	res, err := h.svc.ResolvePeriodByDate(r.Context(), service.ResolveInput{
		TenantID: tenant, LegalEntityID: q.Get("legal_entity_id"), Date: q.Get("date"),
		BookScope: q.Get("book_scope"), ModuleScope: q.Get("module_scope"), Purpose: q.Get("purpose"),
		Kind: q.Get("kind"), SoftCloseException: q.Get("soft_close_exception") == "true",
	})
	if err != nil {
		result := telemetry.GateError
		status, code, msg := http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "store unavailable; posting is not allowed"
		if de, ok := domain.AsError(err); ok {
			status, code, msg = statusFor(de.Code), de.Code, de.Message
			switch de.Code {
			case domain.CodePeriodNotFound:
				result = telemetry.GateNotFound
			case domain.CodeRuleAmbiguous:
				result = telemetry.GateAmbiguous
			}
		} else {
			h.log.Error("accounting-period gate store error", zap.Error(err))
		}
		h.countGate(result)
		writeJSON(w, status, map[string]any{"code": string(code), "message": msg, "posting_allowed": false})
		return
	}
	switch {
	case !res.PostingAllowed:
		h.countGate(telemetry.GateBlocked)
	case res.PostingMode == domain.ModeRestricted:
		h.countGate(telemetry.GateRestricted)
	default:
		h.countGate(telemetry.GateAllowed)
	}
	writeJSON(w, http.StatusOK, res)
}

// StatusByKey is the compat read shaped like financial-close-svc's
// GET /v1/close/periods/status: {"close_status": "OPEN|CLOSED|LOCKED"}.
// An unknown period is 404 PERIOD_NOT_FOUND, never OPEN.
func (h *Handler) StatusByKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	key := q.Get("period_key")
	if key == "" {
		key = q.Get("period_name") // accepted alias: the legacy close contract's parameter name
	}
	st, err := h.svc.StatusByKey(r.Context(), tenant, q.Get("legal_entity_id"), key)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"period_key": key, "close_status": st})
}

// GetPeriod is GetPeriod.
func (h *Handler) GetPeriod(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetPeriod(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", etag(p))
	writeJSON(w, http.StatusOK, p)
}

// ListPeriods is ListOpenPeriods (?state=OPEN) and the general listing.
func (h *Handler) ListPeriods(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	items, err := h.svc.ListPeriods(r.Context(), tenant, q.Get("legal_entity_id"), q.Get("state"), limit, offset)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if items == nil {
		items = []domain.Period{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit, "offset": offset})
}

// StateHistory is GetPeriodStateHistory.
func (h *Handler) StateHistory(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	hist, err := h.svc.GetPeriodStateHistory(r.Context(), tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if hist == nil {
		hist = []domain.HistoryEntry{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"items": hist})
}

// CalendarUsage answers fiscal-calendar-svc before it approves a calendar change.
func (h *Handler) CalendarUsage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenantOnly(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	res, err := h.svc.CalendarUsage(r.Context(), tenant, q.Get("calendar_id"), q.Get("calendar_version_id"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func etag(p *domain.Period) string {
	return `"` + p.PeriodID + "-v" + strconv.FormatInt(p.Version, 10) + `"`
}

func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "limit must be between 1 and 500")
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "offset must not be negative")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// statusFor maps a typed error to its HTTP status.
func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeContextInvalid, domain.CodeSourceUnverified:
		return http.StatusUnprocessableEntity
	case domain.CodeVersionConflict, domain.CodeInvalidTransition, domain.CodeDuplicateCandidate,
		domain.CodeReferenceRetired, domain.CodeRuleAmbiguous:
		return http.StatusConflict
	case domain.CodeSoDDenied, domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound, domain.CodePeriodNotFound:
		return http.StatusNotFound
	case domain.CodeDependencyUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// fail renders a service error. Typed errors keep their code; anything else is
// an internal fault reported as DEPENDENCY_UNAVAILABLE (the database), never
// leaking its text.
func (h *Handler) fail(w http.ResponseWriter, command string, err error) {
	if de, ok := domain.AsError(err); ok {
		outcome := telemetry.OutcomeRefused
		switch de.Code {
		case domain.CodeContextInvalid:
			outcome = telemetry.OutcomeInvalid
		case domain.CodeDependencyUnavailable:
			outcome = telemetry.OutcomeUnavailable
		}
		h.countCmd(command, outcome)
		writeErr(w, statusFor(de.Code), de.Code, de.Message)
		return
	}
	h.countCmd(command, telemetry.OutcomeUnavailable)
	h.log.Error("accounting-period store error", zap.Error(err))
	writeErr(w, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "store unavailable")
}

func (h *Handler) countCmd(command, outcome string) {
	if h.metrics != nil && command != "" {
		h.metrics.Commands.WithLabelValues(command, outcome).Inc()
	}
}

func (h *Handler) countGate(result string) {
	if h.metrics != nil {
		h.metrics.GateDecisions.WithLabelValues(result).Inc()
	}
}

func (h *Handler) countAuthz(action, outcome string) {
	if h.metrics != nil {
		h.metrics.AuthZDecisions.WithLabelValues(action, outcome).Inc()
	}
}

func writeErr(w http.ResponseWriter, status int, code domain.Code, msg string) {
	writeJSON(w, status, map[string]string{"code": string(code), "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
