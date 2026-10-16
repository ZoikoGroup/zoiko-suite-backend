package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/inputpull"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPInputStore is the persistence surface of WFP-03, separate from Store so the
// legacy payroll-run handlers and their stubs are untouched.
type WFPInputStore interface {
	RegisterInputCodeMapping(ctx context.Context, m *domain.InputCodeMapping, mk store.EventBuilder[domain.InputCodeMapping]) (bool, error)
	ListInputCodeMappings(ctx context.Context, kind, code string) ([]domain.InputCodeMapping, error)
	ResolveInputCodeMappings(ctx context.Context, asOf string) (map[string]domain.InputCodeMapping, error)

	CreateInputBatch(ctx context.Context, b *domain.InputBatch, lines []domain.InputLine, mk store.InputEventBuilder) (*store.InputBatchResult, error)
	SupersedeInputBatch(ctx context.Context, oldID string, expectedVersion int, nb *domain.InputBatch, lines []domain.InputLine, reason string, mk store.InputEventBuilder) (*store.InputBatchResult, error)
	FreezeInputBatch(ctx context.Context, id string, expectedVersion int, actor string, mk store.InputEventBuilder) (*domain.InputBatch, error)
	CancelInputBatch(ctx context.Context, id string, expectedVersion int, reason string, mk store.InputEventBuilder) (*domain.InputBatch, error)
	ConsumeInputBatch(ctx context.Context, id string, expectedVersion int, runID string, mk store.InputEventBuilder) (*domain.InputBatch, error)
	GetInputBatch(ctx context.Context, id string) (*domain.InputBatch, error)
	ListInputBatches(ctx context.Context, legalEntityID, periodID, state string) ([]domain.InputBatch, error)
	ListInputLines(ctx context.Context, batchID, state, workerID string) ([]domain.InputLine, error)
	ListInputControlTotals(ctx context.Context, batchID string) ([]domain.InputControlTotal, error)
	ListLiveBonusSourceLines(ctx context.Context, legalEntityID, excludePeriodID string) (map[string]bool, error)
	ManualInputStore
}

// InputPuller gathers the source facts for a batch (read-only, fail closed).
type InputPuller interface {
	Pull(ctx context.Context, req inputpull.Request) ([]domain.WorkerSource, error)
}

// RunReader reads a payroll run so a batch can be tied to the run that consumed it.
type RunReader interface {
	GetPayrollRun(ctx context.Context, id string) (*domain.PayrollRun, error)
}

const (
	actionInputView          = "WFP_INPUT_VIEW"
	actionInputCreate        = "WFP_INPUT_CREATE"
	actionInputFreeze        = "WFP_INPUT_FREEZE"
	actionInputSupersede     = "WFP_INPUT_SUPERSEDE"
	actionInputCancel        = "WFP_INPUT_CANCEL"
	actionInputConsume       = "WFP_INPUT_CONSUME"
	actionInputMappingManage = "WFP_INPUT_MAPPING_MANAGE"
)

type WFPInputHandler struct {
	store  WFPInputStore
	authz  AuthZClient
	puller InputPuller
	runs   RunReader
	log    *zap.Logger
}

func NewWFPInput(st WFPInputStore, authz AuthZClient, puller InputPuller, runs RunReader, log *zap.Logger) *WFPInputHandler {
	return &WFPInputHandler{store: st, authz: authz, puller: puller, runs: runs, log: log}
}

func RegisterWFPInputRoutes(r chi.Router, h *WFPInputHandler) {
	r.Route("/v1/payroll/wfp/input-code-mappings", func(r chi.Router) {
		r.Post("/", h.RegisterMapping)
		r.Get("/", h.ListMappings)
		r.Get("/resolve", h.ResolveMapping)
	})
	r.Route("/v1/payroll/wfp/input-batches", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.Get)
		r.Get("/{id}/validate", h.Validate)
		r.Get("/{id}/exceptions", h.Exceptions)
		r.Get("/{id}/control-totals", h.ControlTotals)
		r.Get("/{id}/workers/{worker_id}", h.WorkerInputs)
		r.Post("/{id}/freeze", h.Freeze)
		r.Post("/{id}/supersede", h.Supersede)
		r.Post("/{id}/cancel", h.Cancel)
		r.Post("/{id}/consume", h.Consume)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPInputHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPInputHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
	if strings.TrimSpace(entityID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "X-Legal-Entity-Id is required")
		return false
	}
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

// loadAuthorized loads a batch and authorizes against ITS legal entity, never a
// header the caller chose.
func (h *WFPInputHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (b *domain.InputBatch, principal, tenant string, ok bool) {
	principal, tenant, ok = h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	b, err := h.store.GetInputBatch(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, b.LegalEntityID, action) {
		return nil, "", "", false
	}
	return b, principal, tenant, true
}

type sourceChanged struct{ issues []domain.InputIssue }

func (e *sourceChanged) Error() string        { return domain.ErrInputSourceChanged.Error() }
func (e *sourceChanged) Is(target error) bool { return target == domain.ErrInputSourceChanged }

func (h *WFPInputHandler) writeErr(w http.ResponseWriter, err error) {
	var changed *sourceChanged
	switch {
	case errors.As(err, &changed):
		writeJSON(w, http.StatusConflict, map[string]any{"error_code": "SOURCE_CHANGED", "error_message": err.Error(), "issues": changed.issues})
	case errors.Is(err, domain.ErrInputBatchNotFound), errors.Is(err, domain.ErrMappingNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInputBatchVersion):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrInputLiveBatchExists):
		writeError(w, http.StatusConflict, "LIVE_BATCH_EXISTS", err.Error())
	case errors.Is(err, domain.ErrInputBatchNotReady), errors.Is(err, domain.ErrInputBatchNotFrozen), errors.Is(err, domain.ErrInputNotSupersedable):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrMappingRegression):
		writeError(w, http.StatusConflict, "MAPPING_EFFECTIVE_DATE_REGRESSION", err.Error())
	case errors.Is(err, domain.ErrRunAlreadyLinked):
		writeError(w, http.StatusConflict, "RUN_ALREADY_LINKED", err.Error())
	case errors.Is(err, domain.ErrRunPeriodMismatch):
		writeError(w, http.StatusUnprocessableEntity, "RUN_PERIOD_MISMATCH", err.Error())
	case errors.Is(err, domain.ErrInputSupersedeWouldReject):
		writeError(w, http.StatusUnprocessableEntity, "REPLACEMENT_WOULD_BE_REJECTED", err.Error())
	case errors.Is(err, domain.ErrTooManyWorkers):
		writeError(w, http.StatusUnprocessableEntity, "ENTITY_TOO_LARGE_FOR_SYNC_BATCH", err.Error())
	case errors.Is(err, domain.ErrSourceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "input_source_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp-03 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll input store unavailable")
	}
}

// buildFresh pulls the current sources for an entity/period and classifies them.
func (h *WFPInputHandler) buildFresh(r *http.Request, principal, tenant, entity, periodID, start, end string) ([]domain.InputLine, error) {
	mappings, err := h.store.ResolveInputCodeMappings(r.Context(), end)
	if err != nil {
		return nil, err
	}
	excluded, err := h.store.ListLiveBonusSourceLines(r.Context(), entity, periodID)
	if err != nil {
		return nil, err
	}
	sources, err := h.puller.Pull(r.Context(), inputpull.Request{
		TenantID: tenant, PrincipalID: principal, CorrelationID: getCorrelationID(r), LegalEntityID: entity,
		PeriodStart: start, PeriodEnd: end, ExcludeBonuses: excluded,
	})
	if err != nil {
		return nil, err
	}
	manuals, err := h.store.ListApprovedManualInputs(r.Context(), entity, periodID)
	if err != nil {
		return nil, err
	}
	return domain.BuildInputLines(manualSources(manuals, sources), mappings, end), nil
}

func quarantinedInput(lines []domain.InputLine) []domain.InputLine {
	out := []domain.InputLine{}
	for _, l := range lines {
		if l.State == domain.InputLineQuarantined {
			out = append(out, l)
		}
	}
	return out
}

// inputEvents turns a committed change into events. No amounts and no personal
// data travel; worker ids are opaque references.
func inputEventBuilder(r *http.Request, actor, tenant string) store.InputEventBuilder {
	return func(ch store.InputChange) ([]domain.OutboxEvent, error) {
		corr, cause := getCorrelationID(r), r.Header.Get("X-Causation-Id")
		var out []domain.OutboxEvent
		add := func(b *domain.InputBatch, typ string, payload map[string]any) error {
			body, err := events.BuildWFPEnvelope(typ, corr, cause, actor, tenant, b.LegalEntityID, "", payload)
			if err != nil {
				return err
			}
			out = append(out, domain.OutboxEvent{TenantID: tenant, ObjectID: b.BatchID, EventType: typ, Payload: body})
			return nil
		}
		summary := func(b *domain.InputBatch) map[string]any {
			return map[string]any{
				"batch_id": b.BatchID, "legal_entity_id": b.LegalEntityID, "payroll_period_id": b.PayrollPeriodID,
				"batch_version": b.BatchVersion, "state": b.State, "worker_count": b.WorkerCount, "line_count": b.LineCount,
				"accepted_line_count": b.AcceptedLineCount, "quarantined_line_count": b.QuarantinedLineCount,
				"source_hash": b.SourceHash, "supersedes_batch_id": b.SupersedesBatchID, "payroll_run_id": b.PayrollRunID,
			}
		}
		created := func(b *domain.InputBatch, lines []domain.InputLine) error {
			if err := add(b, domain.EventInputCreated, summary(b)); err != nil {
				return err
			}
			for _, l := range lines {
				if l.State != domain.InputLineQuarantined {
					continue
				}
				if err := add(b, domain.EventInputLineQuarantined, map[string]any{
					"batch_id": b.BatchID, "line_id": l.LineID, "source_line_id": l.SourceLineID,
					"worker_id": l.WorkerID, "source_kind": l.SourceKind, "reason": l.Quarantine,
				}); err != nil {
					return err
				}
			}
			if b.State == domain.InputBatchRejected {
				return add(b, domain.EventInputBatchRejected, summary(b))
			}
			return add(b, domain.EventInputBatchValidated, summary(b))
		}
		switch ch.Kind {
		case store.InputChangeCreate:
			return out, created(ch.Batch, ch.Lines)
		case store.InputChangeSupersede:
			p := summary(ch.Superseded)
			p["superseded_by"], p["reason"] = ch.Batch.BatchID, ch.Reason
			if err := add(ch.Superseded, domain.EventInputSuperseded, p); err != nil {
				return nil, err
			}
			return out, created(ch.Batch, ch.Lines)
		case store.InputChangeFreeze:
			return out, add(ch.Batch, domain.EventPayrollInputBatchFrozen, summary(ch.Batch))
		case store.InputChangeCancel:
			p := summary(ch.Batch)
			p["reason"] = ch.Reason
			return out, add(ch.Batch, domain.EventInputBatchCancelled, p)
		case store.InputChangeConsume:
			return out, add(ch.Batch, domain.EventInputBatchConsumed, summary(ch.Batch))
		}
		return nil, errors.New("unknown change kind")
	}
}

// ── mappings ─────────────────────────────────────────────────────────────────

func (h *WFPInputHandler) RegisterMapping(w http.ResponseWriter, r *http.Request) {
	var req domain.RegisterInputCodeMappingRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	entity := scopeEntity(r)
	if !ok || !h.authorize(w, r, principal, entity, actionInputMappingManage) {
		return
	}
	m := &domain.InputCodeMapping{
		MappingID: uuid.NewString(), SourceKind: req.SourceKind, SourceCode: req.SourceCode, PayrollCode: req.PayrollCode,
		Category: req.Category, TaxTreatment: req.TaxTreatment, Active: req.IsActive(), EffectiveFrom: req.EffectiveFrom, RegisteredBy: principal,
	}
	created, err := h.store.RegisterInputCodeMapping(r.Context(), m, func(m *domain.InputCodeMapping) (domain.OutboxEvent, error) {
		body, err := events.BuildWFPEnvelope(domain.EventInputMappingRegistered, getCorrelationID(r), r.Header.Get("X-Causation-Id"), principal, tenant, entity, "", map[string]any{
			"mapping_id": m.MappingID, "source_kind": m.SourceKind, "source_code": m.SourceCode, "mapping_version": m.MappingVersion,
			"payroll_code": m.PayrollCode, "category": m.Category, "tax_treatment": m.TaxTreatment, "active": m.Active, "effective_from": m.EffectiveFrom,
		})
		return domain.OutboxEvent{TenantID: tenant, ObjectID: m.MappingID, EventType: domain.EventInputMappingRegistered, Payload: body}, err
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, m)
}

func (h *WFPInputHandler) ListMappings(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok || !h.authorize(w, r, principal, scopeEntity(r), actionInputView) {
		return
	}
	q := r.URL.Query()
	ms, err := h.store.ListInputCodeMappings(r.Context(), q.Get("source_kind"), q.Get("source_code"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ms == nil {
		ms = []domain.InputCodeMapping{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mappings": ms})
}

// ResolveMapping answers which mapping governed a source code on a date. as_of is
// mandatory: "current" by omission is not a historical answer.
func (h *WFPInputHandler) ResolveMapping(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok || !h.authorize(w, r, principal, scopeEntity(r), actionInputView) {
		return
	}
	q := r.URL.Query()
	kind, code, asOf := q.Get("source_kind"), q.Get("source_code"), q.Get("as_of")
	if _, err := time.Parse("2006-01-02", asOf); err != nil || kind == "" || code == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "source_kind, source_code and as_of (YYYY-MM-DD) are required")
		return
	}
	all, err := h.store.ResolveInputCodeMappings(r.Context(), asOf)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	m, ok := all[domain.MappingKey(kind, code)]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", domain.ErrMappingNotFound.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── batches ──────────────────────────────────────────────────────────────────

func (h *WFPInputHandler) writeBatchResult(w http.ResponseWriter, res *store.InputBatchResult) {
	body := map[string]any{"batch": res.Batch, "quarantined_lines": quarantinedInput(res.Lines)}
	switch {
	case res.Batch.State == domain.InputBatchRejected:
		body["error_code"], body["error_message"] = "BATCH_REJECTED", "invalid lines under REJECT_BATCH policy; fix the source or create with QUARANTINE_LINES"
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case res.Created:
		writeJSON(w, http.StatusCreated, body)
	default:
		writeJSON(w, http.StatusOK, body) // replay of an identical population
	}
}

// Create is CreatePayrollInputBatch: pull, classify and snapshot the inputs of an
// entity for a payroll period.
func (h *WFPInputHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInputBatchRequest
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
	if !ok || !h.authorize(w, r, principal, req.LegalEntityID, actionInputCreate) {
		return
	}
	lines, err := h.buildFresh(r, principal, tenant, req.LegalEntityID, req.PayrollPeriodID, req.PeriodStart, req.PeriodEnd)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	b := &domain.InputBatch{
		BatchID: uuid.NewString(), LegalEntityID: req.LegalEntityID, PayrollPeriodID: req.PayrollPeriodID,
		PeriodStart: req.PeriodStart, PeriodEnd: req.PeriodEnd, OnInvalid: req.OnInvalid,
		NormalizationVersion: domain.InputNormalizationVersion, CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	res, err := h.store.CreateInputBatch(r.Context(), b, lines, h.inputEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.writeBatchResult(w, res)
}

func (h *WFPInputHandler) transitionBody(w http.ResponseWriter, r *http.Request, needReason bool) (domain.InputBatchTransitionRequest, int, bool) {
	var req domain.InputBatchTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return req, 0, false
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return req, 0, false
	}
	if needReason && strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return req, 0, false
	}
	return req, version, true
}

// Freeze is FreezePayrollInputBatch. It re-pulls the sources and refuses to
// freeze a population that no longer matches them.
func (h *WFPInputHandler) Freeze(w http.ResponseWriter, r *http.Request) {
	_, version, ok := h.transitionBody(w, r, false)
	if !ok {
		return
	}
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionInputFreeze)
	if !ok {
		return
	}
	if b.State != domain.InputBatchReady {
		h.writeErr(w, domain.ErrInputBatchNotReady)
		return
	}
	if err := h.checkAgainstSource(r, b, principal, tenant); err != nil {
		h.writeErr(w, err)
		return
	}
	frozen, err := h.store.FreezeInputBatch(r.Context(), b.BatchID, version, principal, h.inputEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, frozen)
}

// checkAgainstSource compares the stored population with a fresh pull.
func (h *WFPInputHandler) checkAgainstSource(r *http.Request, b *domain.InputBatch, principal, tenant string) error {
	issues, err := h.diff(r, b, principal, tenant)
	if err != nil {
		return err
	}
	if len(issues) > 0 {
		return &sourceChanged{issues: issues}
	}
	return nil
}

func (h *WFPInputHandler) diff(r *http.Request, b *domain.InputBatch, principal, tenant string) ([]domain.InputIssue, error) {
	stored, err := h.store.ListInputLines(r.Context(), b.BatchID, "", "")
	if err != nil {
		return nil, err
	}
	// The batch's own live bonus lines belong to its period, so they are not excluded.
	fresh, err := h.buildFresh(r, principal, tenant, b.LegalEntityID, b.PayrollPeriodID, b.PeriodStart, b.PeriodEnd)
	if err != nil {
		return nil, err
	}
	return domain.DiffInputLines(stored, fresh), nil
}

// Validate is ValidatePayrollInputBatch: report differences between the batch and
// its sources. It changes nothing.
func (h *WFPInputHandler) Validate(w http.ResponseWriter, r *http.Request) {
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionInputView)
	if !ok {
		return
	}
	issues, err := h.diff(r, b, principal, tenant)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if issues == nil {
		issues = []domain.InputIssue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "valid": len(issues) == 0, "issues": issues})
}

// Supersede is SupersedePayrollInput at batch level: a late change arrives as a
// new batch version built from the CURRENT sources; the old one is kept.
func (h *WFPInputHandler) Supersede(w http.ResponseWriter, r *http.Request) {
	req, version, ok := h.transitionBody(w, r, true)
	if !ok {
		return
	}
	if req.OnInvalid != "" && req.OnInvalid != domain.OnInvalidQuarantineLines && req.OnInvalid != domain.OnInvalidRejectBatch {
		writeError(w, http.StatusBadRequest, "validation_failed", "on_invalid must be QUARANTINE_LINES or REJECT_BATCH")
		return
	}
	old, principal, tenant, ok := h.loadAuthorized(w, r, actionInputSupersede)
	if !ok {
		return
	}
	if old.State != domain.InputBatchReady && old.State != domain.InputBatchFrozen {
		h.writeErr(w, domain.ErrInputNotSupersedable)
		return
	}
	lines, err := h.buildFresh(r, principal, tenant, old.LegalEntityID, old.PayrollPeriodID, old.PeriodStart, old.PeriodEnd)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	policy := old.OnInvalid
	if req.OnInvalid != "" {
		policy = req.OnInvalid
	}
	nb := &domain.InputBatch{
		BatchID: uuid.NewString(), OnInvalid: policy, NormalizationVersion: domain.InputNormalizationVersion,
		CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	res, err := h.store.SupersedeInputBatch(r.Context(), old.BatchID, version, nb, lines, req.Reason, h.inputEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.writeBatchResult(w, res)
}

// Cancel is CancelUnfrozenBatch.
func (h *WFPInputHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	req, version, ok := h.transitionBody(w, r, true)
	if !ok {
		return
	}
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionInputCancel)
	if !ok {
		return
	}
	out, err := h.store.CancelInputBatch(r.Context(), b.BatchID, version, req.Reason, h.inputEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Consume records that a payroll run took this FROZEN batch as its input. It does
// not change how the run calculates (Wave 4).
func (h *WFPInputHandler) Consume(w http.ResponseWriter, r *http.Request) {
	req, version, ok := h.transitionBody(w, r, false)
	if !ok {
		return
	}
	if _, err := uuid.Parse(req.PayrollRunID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "payroll_run_id (UUID) is required")
		return
	}
	b, principal, tenant, ok := h.loadAuthorized(w, r, actionInputConsume)
	if !ok {
		return
	}
	run, err := h.runs.GetPayrollRun(r.Context(), req.PayrollRunID)
	if err != nil {
		if errors.Is(err, domain.ErrPayrollRunNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "RUN_NOT_FOUND", err.Error())
			return
		}
		h.writeErr(w, err)
		return
	}
	if run.LegalEntityID != b.LegalEntityID || run.PayPeriodStart != b.PeriodStart || run.PayPeriodEnd != b.PeriodEnd {
		h.writeErr(w, domain.ErrRunPeriodMismatch)
		return
	}
	out, err := h.store.ConsumeInputBatch(r.Context(), b.BatchID, version, req.PayrollRunID, h.inputEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *WFPInputHandler) Get(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionInputView)
	if !ok {
		return
	}
	lines, err := h.store.ListInputLines(r.Context(), b.BatchID, "", "")
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": b, "lines": lines})
}

func (h *WFPInputHandler) List(w http.ResponseWriter, r *http.Request) {
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
	if !h.authorize(w, r, principal, entity, actionInputView) {
		return
	}
	bs, err := h.store.ListInputBatches(r.Context(), entity, q.Get("payroll_period_id"), q.Get("state"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if bs == nil {
		bs = []domain.InputBatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": bs})
}

// Exceptions is ListInputExceptions.
func (h *WFPInputHandler) Exceptions(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionInputView)
	if !ok {
		return
	}
	lines, err := h.store.ListInputLines(r.Context(), b.BatchID, domain.InputLineQuarantined, "")
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if lines == nil {
		lines = []domain.InputLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "exceptions": lines})
}

// ControlTotals is GetInputControlTotals: per category and currency, never summed
// across currencies.
func (h *WFPInputHandler) ControlTotals(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionInputView)
	if !ok {
		return
	}
	totals, err := h.store.ListInputControlTotals(r.Context(), b.BatchID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": b.BatchID, "state": b.State, "batch_version": b.BatchVersion, "worker_count": b.WorkerCount,
		"line_count": b.LineCount, "accepted_line_count": b.AcceptedLineCount, "quarantined_line_count": b.QuarantinedLineCount,
		"source_hash": b.SourceHash, "normalization_version": b.NormalizationVersion, "totals": totals,
	})
}

// WorkerInputs is GetWorkerInputs: one worker's ACCEPTED lines.
func (h *WFPInputHandler) WorkerInputs(w http.ResponseWriter, r *http.Request) {
	b, _, _, ok := h.loadAuthorized(w, r, actionInputView)
	if !ok {
		return
	}
	workerID := chi.URLParam(r, "worker_id")
	if _, err := uuid.Parse(workerID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "worker_id must be a UUID")
		return
	}
	lines, err := h.store.ListInputLines(r.Context(), b.BatchID, domain.InputLineAccepted, workerID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if lines == nil {
		lines = []domain.InputLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch_id": b.BatchID, "worker_id": workerID, "lines": lines})
}

// inputEvents is the handler's own builder (the package function is shared with the payroll calculation).
func (h *WFPInputHandler) inputEvents(r *http.Request, actor, tenant string) store.InputEventBuilder {
	return inputEventBuilder(r, actor, tenant)
}
