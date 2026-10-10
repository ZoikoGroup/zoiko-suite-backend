// Package handler is the HTTP surface of fiscal-calendar-svc (REF-04).
//
// Named commands use the spec's colon form (POST /v1/fiscal-calendar-versions/{id}:approve).
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

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	svcmiddleware "zoiko.io/fiscal-calendar-svc/internal/middleware"
	"zoiko.io/fiscal-calendar-svc/internal/periodhistory"
	"zoiko.io/fiscal-calendar-svc/internal/service"
	"zoiko.io/fiscal-calendar-svc/internal/telemetry"
)

// AuthZClient asks authorization-svc whether a principal may perform an action.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Authorization actions asked of authorization-svc.
const (
	ActionPropose  = "FISCAL_CALENDAR_PROPOSE"
	ActionApprove  = "FISCAL_CALENDAR_APPROVE"
	ActionActivate = "FISCAL_CALENDAR_ACTIVATE"

	// PlatformScopeID is the scope this service's own workload identity is registered under with mtls-management-svc.
	PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

	maxBodyBytes         = 1 << 20
	headerIdempotencyKey = "Idempotency-Key"
)

// Handler serves the REF-04 API.
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
	r.Post("/v1/fiscal-calendars", h.CreateCalendar)
	r.Get("/v1/fiscal-calendars:resolve", h.Resolve)
	r.Get("/v1/fiscal-calendars/{id}", h.GetCalendar)
	r.Get("/v1/fiscal-calendars/{id}/versions", h.ListVersions)
	r.Post("/v1/fiscal-calendars/{id}/{subcmd}", h.CalendarSubCommand)

	r.Get("/v1/fiscal-calendar-versions/{vid}", h.GetVersion)
	r.Get("/v1/fiscal-calendar-versions/{vid}/periods-preview", h.PeriodsPreview)
	r.Post("/v1/fiscal-calendar-versions/{vid}/transition-plan", h.CreateTransitionPlan)
	r.Post("/v1/fiscal-calendar-versions/{idcmd}", h.VersionCommand)

	r.Get("/v1/calendar-transition-plans/{pid}", h.GetPlan)
	r.Post("/v1/calendar-transition-plans/{idcmd}", h.PlanCommand)
}

// ── request context ──────────────────────────────────────────────────────────

type reqCtx struct {
	tenant, actor, legalEntity string
	meta                       service.Meta
}

// context extracts and validates the trusted request context shared by all
// commands. It writes the refusal itself and reports ok=false. Commands need
// the legal-entity context; queries need only the tenant.
func (h *Handler) context(w http.ResponseWriter, r *http.Request, command bool) (reqCtx, bool) {
	var rc reqCtx
	rc.tenant = svcmiddleware.TenantFromContext(r.Context())
	if rc.tenant == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "tenant context missing (X-Tenant-Id)")
		return rc, false
	}
	if !command {
		return rc, true
	}
	rc.actor = r.Header.Get("X-Principal-Id")
	if rc.actor == "" {
		rc.actor = r.Header.Get("X-Workload-Id")
	}
	if rc.actor == "" {
		writeErr(w, http.StatusUnauthorized, domain.CodeContextInvalid, "caller identity missing (X-Principal-Id or X-Workload-Id)")
		return rc, false
	}
	rc.legalEntity = r.Header.Get("X-Legal-Entity-Id")
	if rc.legalEntity == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "legal entity context missing (X-Legal-Entity-Id)")
		return rc, false
	}
	if r.Header.Get(headerIdempotencyKey) == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "Idempotency-Key header is required for commands")
		return rc, false
	}
	corr := r.Header.Get("X-Correlation-ID")
	if corr == "" {
		corr = chimw.GetReqID(r.Context())
	}
	rc.meta = service.Meta{
		Actor: rc.actor, TenantID: rc.tenant, LegalEntityID: rc.legalEntity, CorrelationID: corr,
		CausationID: r.Header.Get("X-Causation-Id"), IdempotencyKey: r.Header.Get(headerIdempotencyKey),
	}
	return rc, true
}

// readBody reads the capped body and returns it with its request fingerprint.
// The fingerprint covers the method-and-path operation and the body, so the
// same Idempotency-Key reused for a different request is detected.
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
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// authorize asks authorization-svc. It writes the refusal and reports false on
// denial (403 FORBIDDEN) or when the dependency is down (503
// DEPENDENCY_UNAVAILABLE, fail closed).
func (h *Handler) authorize(w http.ResponseWriter, ctx context.Context, rc reqCtx, action, command string) bool {
	err := h.authz.CheckAllowed(ctx, rc.actor, rc.legalEntity, action)
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

// prepared is a command that has passed context, body and authorization checks.
type prepared struct {
	rc   reqCtx
	meta service.Meta
}

// prepare runs the checks every command shares, in a fixed order: trusted
// context, body decode, reason, authorization. expected_version is checked by
// the caller (it is absent from the create command).
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request, op, metric, action string, req any, reason func() string) (prepared, bool) {
	rc, ok := h.context(w, r, true)
	if !ok {
		h.countCmd(metric, telemetry.OutcomeInvalid)
		return prepared{}, false
	}
	body, hash, ok := readBody(w, r, op)
	if !ok {
		return prepared{}, false
	}
	if !decodeStrict(w, body, req) {
		h.countCmd(metric, telemetry.OutcomeInvalid)
		return prepared{}, false
	}
	if reason() == "" {
		h.countCmd(metric, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "reason is required")
		return prepared{}, false
	}
	if !h.authorize(w, r.Context(), rc, action, metric) {
		return prepared{}, false
	}
	meta := rc.meta
	meta.RequestHash, meta.Reason = hash, reason()
	return prepared{rc: rc, meta: meta}, true
}

// expectedVersion reads expected_version from the body, falling back to the
// canonical X-Expected-Version header.
func expectedVersion(r *http.Request, body *int64) (int64, bool) {
	if body != nil {
		return *body, true
	}
	if v := r.Header.Get("X-Expected-Version"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func (h *Handler) needVersion(w http.ResponseWriter, r *http.Request, body *int64, metric string) (int64, bool) {
	ev, ok := expectedVersion(r, body)
	if !ok || ev < 1 {
		h.countCmd(metric, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "expected_version is required (body field or X-Expected-Version) and must be >= 1")
		return 0, false
	}
	return ev, true
}

func splitCommand(seg string) (id, cmd string, ok bool) {
	i := strings.LastIndex(seg, ":")
	if i <= 0 || i == len(seg)-1 {
		return "", "", false
	}
	return seg[:i], seg[i+1:], true
}

// ── commands ─────────────────────────────────────────────────────────────────

type definitionRequest struct {
	Pattern              json.RawMessage `json:"pattern"`
	FiscalYearStartMonth int             `json:"fiscal_year_start_month"`
	FiscalYearStartDay   int             `json:"fiscal_year_start_day"`
	EffectiveFrom        domain.Date     `json:"effective_from"`
	EffectiveTo          *domain.Date    `json:"effective_to"`
}

func (d definitionRequest) spec() service.VersionSpec {
	return service.VersionSpec{
		Pattern: d.Pattern, StartMonth: d.FiscalYearStartMonth, StartDay: d.FiscalYearStartDay,
		EffectiveFrom: d.EffectiveFrom, EffectiveTo: d.EffectiveTo,
	}
}

type createRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Code          string `json:"code"`
	Scope         string `json:"scope"`
	Reason        string `json:"reason"`
	definitionRequest
}

// CreateCalendar is CreateFiscalCalendar (POST /v1/fiscal-calendars).
func (h *Handler) CreateCalendar(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	p, ok := h.prepare(w, r, "POST /v1/fiscal-calendars", telemetry.CmdCreate, ActionPropose, &req, func() string { return req.Reason })
	if !ok {
		return
	}
	if req.LegalEntityID != p.rc.legalEntity {
		h.countCmd(telemetry.CmdCreate, telemetry.OutcomeInvalid)
		writeErr(w, http.StatusUnprocessableEntity, domain.CodeContextInvalid, "legal_entity_id must equal the trusted X-Legal-Entity-Id context")
		return
	}
	res, replayed, err := h.svc.CreateFiscalCalendar(r.Context(), service.CreateCalendarInput{
		Meta: p.meta, Code: req.Code, Scope: req.Scope, VersionSpec: req.spec(),
	})
	if err != nil {
		h.fail(w, telemetry.CmdCreate, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(telemetry.CmdCreate, telemetry.OutcomeReplayed)
		writeJSON(w, http.StatusOK, res)
		return
	}
	h.countCmd(telemetry.CmdCreate, telemetry.OutcomeOK)
	w.Header().Set("ETag", etag(res.Calendar.CalendarID, res.Calendar.Version))
	writeJSON(w, http.StatusCreated, res)
}

type proposeRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	Reason          string `json:"reason"`
	definitionRequest
}

// CalendarSubCommand dispatches POST /v1/fiscal-calendars/{id}/versions:propose-change.
func (h *Handler) CalendarSubCommand(w http.ResponseWriter, r *http.Request) {
	sub := chi.URLParam(r, "subcmd")
	if sub != "versions:propose-change" {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected versions:propose-change")
		return
	}
	id := chi.URLParam(r, "id")
	var req proposeRequest
	p, ok := h.prepare(w, r, "POST /v1/fiscal-calendars/"+id+"/"+sub, telemetry.CmdProposeChange, ActionPropose, &req, func() string { return req.Reason })
	if !ok {
		return
	}
	ev, ok := h.needVersion(w, r, req.ExpectedVersion, telemetry.CmdProposeChange)
	if !ok {
		return
	}
	v, replayed, err := h.svc.ProposeCalendarChange(r.Context(), service.ProposeChangeInput{
		Meta: p.meta, CalendarID: id, ExpectedVersion: ev, VersionSpec: req.spec(),
	})
	if err != nil {
		h.fail(w, telemetry.CmdProposeChange, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(telemetry.CmdProposeChange, telemetry.OutcomeReplayed)
		writeJSON(w, http.StatusOK, v)
		return
	}
	h.countCmd(telemetry.CmdProposeChange, telemetry.OutcomeOK)
	w.Header().Set("ETag", etag(v.VersionID, v.Version))
	writeJSON(w, http.StatusCreated, v)
}

type decisionRequest struct {
	ExpectedVersion *int64 `json:"expected_version"`
	Reason          string `json:"reason"`
}

// VersionCommand dispatches POST /v1/fiscal-calendar-versions/{id}:approve|:activate.
func (h *Handler) VersionCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd, ok := splitCommand(chi.URLParam(r, "idcmd"))
	action, metric := "", ""
	switch cmd {
	case service.CommandApprove:
		action, metric = ActionApprove, telemetry.CmdApproveVersion
	case service.CommandActivate:
		action, metric = ActionActivate, telemetry.CmdActivateVersion
	}
	if !ok || action == "" {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected {id}:approve or {id}:activate")
		return
	}
	var req decisionRequest
	p, ok := h.prepare(w, r, "POST /v1/fiscal-calendar-versions/"+id+":"+cmd, metric, action, &req, func() string { return req.Reason })
	if !ok {
		return
	}
	ev, ok := h.needVersion(w, r, req.ExpectedVersion, metric)
	if !ok {
		return
	}
	in := service.VersionCommandInput{Meta: p.meta, VersionID: id, ExpectedVersion: ev}
	var (
		res      *domain.FiscalCalendarVersion
		replayed bool
		err      error
	)
	if cmd == service.CommandApprove {
		res, replayed, err = h.svc.ApproveVersion(r.Context(), in)
	} else {
		res, replayed, err = h.svc.ActivateVersion(periodhistory.WithCorrelation(r.Context(), p.meta.CorrelationID), in)
	}
	if err != nil {
		h.fail(w, metric, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(metric, telemetry.OutcomeReplayed)
	} else {
		h.countCmd(metric, telemetry.OutcomeOK)
	}
	w.Header().Set("ETag", etag(res.VersionID, res.Version))
	writeJSON(w, http.StatusOK, res)
}

type planRequest struct {
	FromVersionID        string          `json:"from_version_id"`
	ExpectedVersion      *int64          `json:"expected_version"`
	ImpactAssessment     json.RawMessage `json:"impact_assessment"`
	Mapping              json.RawMessage `json:"mapping"`
	AffectsPostedPeriods bool            `json:"affects_posted_periods"`
	Reason               string          `json:"reason"`
}

// CreateTransitionPlan is POST /v1/fiscal-calendar-versions/{vid}/transition-plan.
func (h *Handler) CreateTransitionPlan(w http.ResponseWriter, r *http.Request) {
	vid := chi.URLParam(r, "vid")
	var req planRequest
	p, ok := h.prepare(w, r, "POST /v1/fiscal-calendar-versions/"+vid+"/transition-plan", telemetry.CmdCreatePlan, ActionPropose, &req, func() string { return req.Reason })
	if !ok {
		return
	}
	ev, ok := h.needVersion(w, r, req.ExpectedVersion, telemetry.CmdCreatePlan)
	if !ok {
		return
	}
	plan, replayed, err := h.svc.CreateTransitionPlan(r.Context(), service.CreatePlanInput{
		Meta: p.meta, ToVersionID: vid, FromVersionID: req.FromVersionID, ExpectedVersion: ev,
		ImpactAssessment: req.ImpactAssessment, Mapping: req.Mapping, AffectsPostedPeriods: req.AffectsPostedPeriods,
	})
	if err != nil {
		h.fail(w, telemetry.CmdCreatePlan, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(telemetry.CmdCreatePlan, telemetry.OutcomeReplayed)
		writeJSON(w, http.StatusOK, plan)
		return
	}
	h.countCmd(telemetry.CmdCreatePlan, telemetry.OutcomeOK)
	w.Header().Set("ETag", etag(plan.PlanID, plan.Version))
	writeJSON(w, http.StatusCreated, plan)
}

// PlanCommand dispatches POST /v1/calendar-transition-plans/{id}:approve|:reject.
func (h *Handler) PlanCommand(w http.ResponseWriter, r *http.Request) {
	id, cmd, ok := splitCommand(chi.URLParam(r, "idcmd"))
	if !ok || (cmd != "approve" && cmd != "reject") {
		writeErr(w, http.StatusNotFound, domain.CodeNotFound, "unknown command; expected {id}:approve or {id}:reject")
		return
	}
	metric := telemetry.CmdApprovePlan
	var req decisionRequest
	p, ok := h.prepare(w, r, "POST /v1/calendar-transition-plans/"+id+":"+cmd, metric, ActionApprove, &req, func() string { return req.Reason })
	if !ok {
		return
	}
	ev, ok := h.needVersion(w, r, req.ExpectedVersion, metric)
	if !ok {
		return
	}
	plan, replayed, err := h.svc.DecideTransitionPlan(r.Context(), service.PlanDecisionInput{
		Meta: p.meta, PlanID: id, ExpectedVersion: ev, Approve: cmd == "approve",
	})
	if err != nil {
		h.fail(w, metric, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replay", "true")
		h.countCmd(metric, telemetry.OutcomeReplayed)
	} else {
		h.countCmd(metric, telemetry.OutcomeOK)
	}
	w.Header().Set("ETag", etag(plan.PlanID, plan.Version))
	writeJSON(w, http.StatusOK, plan)
}

// ── queries ──────────────────────────────────────────────────────────────────

// GetCalendar is GetFiscalCalendar / GetCalendarAsOf (GET /v1/fiscal-calendars/{id}?as_of=YYYY-MM-DD).
func (h *Handler) GetCalendar(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	var asOf *domain.Date
	if v := r.URL.Query().Get("as_of"); v != "" {
		d, err := domain.ParseDate(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "as_of must be a YYYY-MM-DD date")
			return
		}
		asOf = &d
	}
	view, err := h.svc.GetCalendar(r.Context(), rc.tenant, chi.URLParam(r, "id"), asOf)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag(view.Calendar.CalendarID, view.Calendar.Version))
	writeJSON(w, http.StatusOK, view)
}

// ListVersions is ListCalendarVersions.
func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	vs, err := h.svc.ListVersions(r.Context(), rc.tenant, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if vs == nil {
		vs = []domain.FiscalCalendarVersion{}
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"items": vs})
}

// GetVersion returns one version.
func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	v, err := h.svc.GetVersion(r.Context(), rc.tenant, chi.URLParam(r, "vid"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag(v.VersionID, v.Version))
	writeJSON(w, http.StatusOK, v)
}

// GetPlan returns one transition plan.
func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	p, err := h.svc.GetPlan(r.Context(), rc.tenant, chi.URLParam(r, "pid"))
	if err != nil {
		h.fail(w, "", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag(p.PlanID, p.Version))
	writeJSON(w, http.StatusOK, p)
}

// PeriodsPreview is PreviewPeriods
// (GET /v1/fiscal-calendar-versions/{vid}/periods-preview?fiscal_year=YYYY): the
// contract REF-05 consumes. See openapi.yaml for the exact shape.
func (h *Handler) PeriodsPreview(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	fy, err := strconv.Atoi(r.URL.Query().Get("fiscal_year"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "fiscal_year is required and must be a year such as 2026")
		return
	}
	pv, err := h.svc.PreviewPeriods(r.Context(), rc.tenant, chi.URLParam(r, "vid"), fy)
	if err != nil {
		h.fail(w, "", err)
		return
	}
	if pv.Periods == nil {
		pv.Periods = []domain.Period{}
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, pv)
}

// Resolve is GET /v1/fiscal-calendars:resolve?legal_entity_id=&scope=&date=.
// No match is 404 NOT_FOUND; there is no fallback.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	rc, ok := h.context(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	date, err := domain.ParseDate(q.Get("date"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "date is required and must be YYYY-MM-DD")
		return
	}
	if q.Get("legal_entity_id") == "" || q.Get("scope") == "" {
		writeErr(w, http.StatusBadRequest, domain.CodeContextInvalid, "legal_entity_id and scope are required")
		return
	}
	res, err := h.svc.Resolve(r.Context(), rc.tenant, q.Get("legal_entity_id"), q.Get("scope"), date)
	if err != nil {
		if de, typed := domain.AsError(err); typed && de.Code == domain.CodeNotFound && h.metrics != nil {
			h.metrics.Resolutions.WithLabelValues("not_found").Inc()
		}
		h.fail(w, "", err)
		return
	}
	if h.metrics != nil {
		h.metrics.Resolutions.WithLabelValues("resolved").Inc()
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, res)
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func etag(id string, version int64) string {
	return `"` + id + "-v" + strconv.FormatInt(version, 10) + `"`
}

// statusFor maps a typed error to its HTTP status.
func statusFor(code domain.Code) int {
	switch code {
	case domain.CodeContextInvalid, domain.CodeSourceUnverified:
		return http.StatusUnprocessableEntity
	case domain.CodeVersionConflict, domain.CodeInvalidTransition, domain.CodeDuplicateCandidate, domain.CodeReferenceRetired,
		domain.CodeTransitionPlanRequired, domain.CodeRuleAmbiguous:
		return http.StatusConflict
	case domain.CodeSoDDenied, domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
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
	h.log.Error("fiscal-calendar store error", zap.Error(err))
	writeErr(w, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable, "store unavailable")
}

func (h *Handler) countCmd(command, outcome string) {
	if h.metrics != nil && command != "" {
		h.metrics.Commands.WithLabelValues(command, outcome).Inc()
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
