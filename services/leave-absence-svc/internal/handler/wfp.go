package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/events"
	svcmiddleware "zoiko.io/leave-absence-svc/internal/middleware"
	"zoiko.io/leave-absence-svc/internal/store"
	"zoiko.io/leave-absence-svc/internal/workerctx"
)

// WFPStore is the persistence surface of WFP-02, separate from Store so the
// leave handlers and their stubs are untouched.
type WFPStore interface {
	ListApprovedLeaveForPeriod(ctx context.Context, legalEntityID, periodStart, periodEnd string) ([]domain.SourceLeave, error)
	ImportTimeLeaveBatch(ctx context.Context, b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, mk store.EventBuilder) (*store.BatchResult, error)
	SupersedeTimeLeaveBatch(ctx context.Context, oldID string, expectedVersion int, nb *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine, reason string, mk store.EventBuilder) (*store.BatchResult, error)
	ValidateTimeLeaveBatch(ctx context.Context, batchID string) ([]domain.SourceIssue, error)
	FreezeTimeLeaveBatch(ctx context.Context, batchID string, expectedVersion int, actor string, mk store.EventBuilder) (*domain.TimeLeaveBatch, error)
	QuarantineLine(ctx context.Context, batchID, lineID string, expectedVersion int, mk store.EventBuilder) (*domain.TimeLeaveBatch, *domain.TimeLeaveLine, error)
	GetTimeLeaveBatch(ctx context.Context, id string) (*domain.TimeLeaveBatch, error)
	ListTimeLeaveBatches(ctx context.Context, legalEntityID, periodID, state string) ([]domain.TimeLeaveBatch, error)
	ListBatchLines(ctx context.Context, batchID, state, workerID string) ([]domain.TimeLeaveLine, error)
	GetWorkerImportedUnits(ctx context.Context, batchID, workerID string) (*store.WorkerUnits, error)
}

// WorkerContextChecker asks employee-master-svc (WFP-01) whether a worker may
// be paid by an entity on a date.
type WorkerContextChecker interface {
	Validate(ctx context.Context, caller workerctx.Caller, workerID, legalEntityID, asOf string) (workerctx.Result, error)
}

const (
	actionWFPTLView       = "WFP_TIME_LEAVE_VIEW"
	actionWFPTLImport     = "WFP_TIME_LEAVE_IMPORT"
	actionWFPTLFreeze     = "WFP_TIME_LEAVE_FREEZE"
	actionWFPTLQuarantine = "WFP_TIME_LEAVE_QUARANTINE"
	actionWFPTLSupersede  = "WFP_TIME_LEAVE_SUPERSEDE"
)

type WFPHandler struct {
	store   WFPStore
	authz   AuthZClient
	workers WorkerContextChecker
	log     *zap.Logger
}

func NewWFP(st WFPStore, authz AuthZClient, workers WorkerContextChecker, log *zap.Logger) *WFPHandler {
	return &WFPHandler{store: st, authz: authz, workers: workers, log: log}
}

func RegisterWFPRoutes(r chi.Router, h *WFPHandler) {
	r.Route("/v1/wfp/time-leave-batches", func(r chi.Router) {
		r.Post("/", h.Import)
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/validate", h.Validate)
		r.Get("/{id}/exceptions", h.Exceptions)
		r.Get("/{id}/control-totals", h.ControlTotals)
		r.Get("/{id}/workers/{worker_id}", h.WorkerUnits)
		r.Post("/{id}/freeze", h.Freeze)
		r.Post("/{id}/lines/{line_id}/quarantine", h.QuarantineLine)
		r.Post("/{id}/supersede", h.Supersede)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

// identity authenticates the caller; the tenant is always the authenticated one.
func (h *WFPHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, entityID, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return false
	}
	return true
}

// loadAuthorized loads a batch and authorizes the action against ITS legal
// entity (not a header the caller chose), so scope cannot be asserted by the caller.
func (h *WFPHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (b *domain.TimeLeaveBatch, principal, tenant string, ok bool) {
	principal, tenant, ok = h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	b, err := h.store.GetTimeLeaveBatch(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeWFPError(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, b.LegalEntityID, action) {
		return nil, "", "", false
	}
	return b, principal, tenant, true
}

func (h *WFPHandler) writeWFPError(w http.ResponseWriter, err error) {
	var changed *store.SourceChangedError
	switch {
	case errors.As(err, &changed):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error_code": "SOURCE_CHANGED", "error_message": err.Error(), "issues": changed.Issues,
		})
	case errors.Is(err, domain.ErrBatchNotFound), errors.Is(err, domain.ErrLineNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrBatchVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrLiveBatchExists):
		writeError(w, http.StatusConflict, "LIVE_BATCH_EXISTS", err.Error())
	case errors.Is(err, domain.ErrBatchNotAccepted), errors.Is(err, domain.ErrBatchNotSupersedable), errors.Is(err, domain.ErrLineNotAccepted):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrSupersedeWouldReject):
		writeError(w, http.StatusUnprocessableEntity, "REPLACEMENT_WOULD_BE_REJECTED", err.Error())
	case errors.Is(err, domain.ErrWorkerContextUnavailable):
		writeError(w, http.StatusServiceUnavailable, "worker_context_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		// The raw error is not echoed: a store error can carry row content.
		h.log.Error("wfp-02 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "time/leave batch store unavailable")
	}
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// expectedVersion reads the optimistic-concurrency token from If-Match
// (`"3"` or `3`); the body value is the fallback.
func expectedVersion(r *http.Request, bodyValue int) (int, bool) {
	if m := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); m != "" {
		n, err := strconv.Atoi(m)
		return n, err == nil && n > 0
	}
	return bodyValue, bodyValue > 0
}

func (h *WFPHandler) caller(r *http.Request, principal, tenant, entity string) workerctx.Caller {
	return workerctx.Caller{TenantID: tenant, PrincipalID: principal, LegalEntityID: entity, CorrelationID: getCorrelationID(r)}
}

// buildLines classifies every approved leave in scope. Worker context is asked
// once per distinct worker. An unreachable answer fails the whole command: a
// batch is never built on a guess about who may be paid.
func (h *WFPHandler) buildLines(r *http.Request, principal, tenant, entity, periodStart, periodEnd string) ([]domain.TimeLeaveLine, error) {
	sources, err := h.store.ListApprovedLeaveForPeriod(r.Context(), entity, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	reasons := map[string]string{}
	for _, s := range sources {
		if _, done := reasons[s.WorkerID]; done {
			continue
		}
		res, err := h.workers.Validate(r.Context(), h.caller(r, principal, tenant, entity), s.WorkerID, entity, periodEnd)
		if err != nil {
			return nil, err
		}
		reason := ""
		if !res.Valid {
			reason = "WORKER_CONTEXT_INVALID"
			if len(res.Reasons) > 0 {
				reason = res.Reasons[0]
			}
		}
		reasons[s.WorkerID] = reason
	}

	lines := make([]domain.TimeLeaveLine, 0, len(sources))
	for _, s := range sources {
		reason := domain.ClassifyLine(s, periodStart, periodEnd, reasons[s.WorkerID])
		l := domain.TimeLeaveLine{
			LineID: domain.NewID(), SourceLineID: s.RequestID, WorkerID: s.WorkerID, LeaveTypeID: s.LeaveTypeID,
			LeaveTypeCode: s.LeaveTypeCode, IsPaid: s.IsPaid, StartDate: s.StartDate, EndDate: s.EndDate,
			Units: s.Units, UnitOfMeasure: domain.UnitHours, ApprovedBy: s.ApprovedBy, ApprovedAt: s.ApprovedAt,
			State: domain.LineAccepted, LineHash: domain.LineHash(s),
		}
		if reason != "" {
			l.State, l.QuarantineReason = domain.LineQuarantined, &reason
		}
		lines = append(lines, l)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].SourceLineID < lines[j].SourceLineID })
	return lines, nil
}

func quarantined(lines []domain.TimeLeaveLine) []domain.TimeLeaveLine {
	out := []domain.TimeLeaveLine{}
	for _, l := range lines {
		if l.State == domain.LineQuarantined {
			out = append(out, l)
		}
	}
	return out
}

// batchEvents turns a committed change into events. Worker ids appear (they are
// opaque references); names, hours-per-person detail and reasons for leave never do.
func (h *WFPHandler) batchEvents(r *http.Request, actor, tenant string) store.EventBuilder {
	return func(ch store.BatchChange) ([]domain.OutboxEvent, error) {
		corr, cause := getCorrelationID(r), r.Header.Get("X-Causation-Id")
		var out []domain.OutboxEvent
		add := func(b *domain.TimeLeaveBatch, typ string, payload map[string]any) error {
			body, err := events.BuildWFPEnvelope(typ, corr, cause, actor, tenant, b.LegalEntityID, "", payload)
			if err != nil {
				return err
			}
			out = append(out, domain.OutboxEvent{TenantID: tenant, ObjectID: b.BatchID, EventType: typ, Payload: body})
			return nil
		}
		summary := func(b *domain.TimeLeaveBatch) map[string]any {
			return map[string]any{
				"batch_id": b.BatchID, "legal_entity_id": b.LegalEntityID, "payroll_period_id": b.PayrollPeriodID,
				"batch_version": b.BatchVersion, "state": b.State, "line_count": b.LineCount,
				"accepted_line_count": b.AcceptedLineCount, "quarantined_line_count": b.QuarantinedLineCount,
				"total_units": b.TotalUnits, "paid_units": b.PaidUnits, "unpaid_units": b.UnpaidUnits,
				"unit_of_measure": b.UnitOfMeasure, "source_hash": b.SourceHash, "supersedes_batch_id": b.SupersedesBatchID,
			}
		}
		lineEvents := func(b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine) error {
			for _, l := range lines {
				if l.State != domain.LineQuarantined {
					continue
				}
				if err := add(b, domain.EventLineQuarantined, map[string]any{
					"batch_id": b.BatchID, "line_id": l.LineID, "source_line_id": l.SourceLineID,
					"worker_id": l.WorkerID, "reason": l.QuarantineReason,
				}); err != nil {
					return err
				}
			}
			return nil
		}
		importEvents := func(b *domain.TimeLeaveBatch, lines []domain.TimeLeaveLine) error {
			if err := add(b, domain.EventBatchReceived, summary(b)); err != nil {
				return err
			}
			if err := lineEvents(b, lines); err != nil {
				return err
			}
			if b.State == domain.BatchRejected {
				return add(b, domain.EventBatchRejected, summary(b))
			}
			return add(b, domain.EventBatchAccepted, summary(b))
		}

		switch ch.Kind {
		case store.ChangeImport:
			return out, importEvents(ch.Batch, ch.Lines)
		case store.ChangeSupersede:
			p := summary(ch.Superseded)
			p["superseded_by"], p["reason"] = ch.Batch.BatchID, ch.Reason
			if err := add(ch.Superseded, domain.EventBatchSuperseded, p); err != nil {
				return nil, err
			}
			return out, importEvents(ch.Batch, ch.Lines)
		case store.ChangeFreeze:
			return out, add(ch.Batch, domain.EventBatchFrozen, summary(ch.Batch))
		case store.ChangeQuarantine:
			return out, lineEvents(ch.Batch, ch.Lines)
		}
		return nil, errors.New("unknown change kind")
	}
}

// ── commands ─────────────────────────────────────────────────────────────────

// Import is ImportTimeLeaveBatch: snapshot approved leave for a payroll period.
func (h *WFPHandler) Import(w http.ResponseWriter, r *http.Request) {
	var req domain.ImportTimeLeaveBatchRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	req = req.WithDefaults()
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok || !h.authorize(w, r, principal, req.LegalEntityID, actionWFPTLImport) {
		return
	}
	lines, err := h.buildLines(r, principal, tenant, req.LegalEntityID, req.PeriodStart, req.PeriodEnd)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	b := &domain.TimeLeaveBatch{
		BatchID: domain.NewID(), LegalEntityID: req.LegalEntityID, PayrollPeriodID: req.PayrollPeriodID,
		PeriodStart: req.PeriodStart, PeriodEnd: req.PeriodEnd, SourceSystemID: domain.SourceLeaveAbsence,
		OnInvalid: req.OnInvalid, NormalizationVersion: domain.NormalizationVersion,
		CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	res, err := h.store.ImportTimeLeaveBatch(r.Context(), b, lines, h.batchEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	h.writeBatchResult(w, res)
}

func (h *WFPHandler) writeBatchResult(w http.ResponseWriter, res *store.BatchResult) {
	body := map[string]any{"batch": res.Batch, "quarantined_lines": quarantined(res.Lines)}
	switch {
	case res.Batch.State == domain.BatchRejected:
		body["error_code"], body["error_message"] = "BATCH_REJECTED", "invalid lines under REJECT_BATCH policy; fix the source or import with QUARANTINE_LINES"
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case res.Created:
		writeJSON(w, http.StatusCreated, body)
	default:
		writeJSON(w, http.StatusOK, body) // replay of an identical population
	}
}

// Supersede is SupersedeBatch: a late change arrives as a new batch version, the
// old one is kept and linked, never edited.
func (h *WFPHandler) Supersede(w http.ResponseWriter, r *http.Request) {
	var req domain.BatchTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	if req.OnInvalid != "" && req.OnInvalid != domain.OnInvalidQuarantineLines && req.OnInvalid != domain.OnInvalidRejectBatch {
		writeError(w, http.StatusBadRequest, "validation_failed", "on_invalid must be QUARANTINE_LINES or REJECT_BATCH")
		return
	}
	old, principal, tenant, ok := h.loadAuthorized(w, r, actionWFPTLSupersede)
	if !ok {
		return
	}
	lines, err := h.buildLines(r, principal, tenant, old.LegalEntityID, old.PeriodStart, old.PeriodEnd)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	policy := old.OnInvalid
	if req.OnInvalid != "" {
		policy = req.OnInvalid
	}
	nb := &domain.TimeLeaveBatch{
		BatchID: domain.NewID(), OnInvalid: policy, NormalizationVersion: domain.NormalizationVersion,
		CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	res, err := h.store.SupersedeTimeLeaveBatch(r.Context(), old.BatchID, version, nb, lines, req.Reason, h.batchEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	h.writeBatchResult(w, res)
}

func (h *WFPHandler) Freeze(w http.ResponseWriter, r *http.Request) {
	var req domain.BatchTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionWFPTLFreeze)
	if !ok {
		return
	}
	frozen, err := h.store.FreezeTimeLeaveBatch(r.Context(), b.BatchID, version, principal, h.batchEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, frozen)
}

// QuarantineLine is the manual QuarantineLine command on an unfrozen batch.
func (h *WFPHandler) QuarantineLine(w http.ResponseWriter, r *http.Request) {
	var req domain.BatchTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionWFPTLQuarantine)
	if !ok {
		return
	}
	batch, line, err := h.store.QuarantineLine(r.Context(), b.BatchID, chi.URLParam(r, "line_id"), version, h.batchEvents(r, principal, tenant))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": batch, "line": line})
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *WFPHandler) Get(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionWFPTLView)
	if !ok {
		return
	}
	lines, err := h.store.ListBatchLines(r.Context(), b.BatchID, "", "")
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": b, "lines": lines})
}

func (h *WFPHandler) List(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("legal_entity_id"))
	if entity == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if !h.authorize(w, r, principal, entity, actionWFPTLView) {
		return
	}
	bs, err := h.store.ListTimeLeaveBatches(r.Context(), entity, q.Get("payroll_period_id"), q.Get("state"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if bs == nil {
		bs = []domain.TimeLeaveBatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": bs})
}

// Validate is ValidateTimeLeaveBatch: re-check the batch against its source and
// report differences. It changes nothing.
func (h *WFPHandler) Validate(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionWFPTLView)
	if !ok {
		return
	}
	issues, err := h.store.ValidateTimeLeaveBatch(r.Context(), b.BatchID)
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if issues == nil {
		issues = []domain.SourceIssue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "valid": len(issues) == 0, "issues": issues})
}

func (h *WFPHandler) Exceptions(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionWFPTLView)
	if !ok {
		return
	}
	lines, err := h.store.ListBatchLines(r.Context(), b.BatchID, domain.LineQuarantined, "")
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	if lines == nil {
		lines = []domain.TimeLeaveLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "exceptions": lines})
}

func (h *WFPHandler) ControlTotals(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionWFPTLView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": b.BatchID, "state": b.State, "batch_version": b.BatchVersion,
		"line_count": b.LineCount, "accepted_line_count": b.AcceptedLineCount,
		"quarantined_line_count": b.QuarantinedLineCount, "total_units": b.TotalUnits,
		"paid_units": b.PaidUnits, "unpaid_units": b.UnpaidUnits, "unit_of_measure": b.UnitOfMeasure,
		"source_hash": b.SourceHash, "normalization_version": b.NormalizationVersion,
	})
}

func (h *WFPHandler) WorkerUnits(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionWFPTLView)
	if !ok {
		return
	}
	units, err := h.store.GetWorkerImportedUnits(r.Context(), b.BatchID, chi.URLParam(r, "worker_id"))
	if err != nil {
		h.writeWFPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, units)
}
