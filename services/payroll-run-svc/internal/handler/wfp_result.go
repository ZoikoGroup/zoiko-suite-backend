package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
	"zoiko.io/payroll-run-svc/internal/workerctx"
)

// WFPResultStore is the persistence surface of WFP-04, separate from Store so the
// legacy payroll-run handlers and their stubs are untouched.
type WFPResultStore interface {
	GetSourceTrustProfileBySource(ctx context.Context, sourceSystemID string) (*domain.SourceTrustProfile, error)
	GetRuleManifest(ctx context.Context, id string) (*domain.PayrollRuleManifest, error)
	GetInputBatch(ctx context.Context, id string) (*domain.InputBatch, error)
	GetPayrollRun(ctx context.Context, id string) (*domain.PayrollRun, error)
	GetPaySlipsByRun(ctx context.Context, runID string) ([]domain.PaySlip, error)

	ReceivePayrollResult(ctx context.Context, in store.ReceiveResultInput, mk store.ResultEventBuilder) (*store.ReceiveResultOutput, error)
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error)
	ListResultWorkers(ctx context.Context, runRefID string) ([]domain.PayrollResultWorker, error)
	ListResultComponents(ctx context.Context, runRefID, workerID string) ([]domain.PayrollResultComponent, error)
	CorrectionChains(ctx context.Context, runRefID string) (asOriginal, asSuperseding []domain.PayrollCorrectionChain, err error)
	LatestRunRefVersion(ctx context.Context, legalEntityID, sourceSystemID, sourceRunID string) (int, error)
	RunRefVersionForPayrollRun(ctx context.Context, legalEntityID, sourceSystemID, sourceRunID, payrollRunID string) (int, bool, error)
	RecomputeResultTotals(ctx context.Context, runRefID string) ([]domain.ResultReason, error)
}

// ResultWorkerContexts asks employee-master-svc (WFP-01) about a worker.
type ResultWorkerContexts interface {
	Validate(ctx context.Context, caller workerctx.Caller, workerID, legalEntityID, asOf string) (domain.WorkerContext, error)
}

const (
	actionResultSubmit = "WFP_RESULT_SUBMIT"
	actionResultView   = "WFP_RESULT_VIEW"
)

type WFPResultHandler struct {
	store       WFPResultStore
	authz       AuthZClient
	workers     ResultWorkerContexts
	log         *zap.Logger
	concurrency int
}

func NewWFPResult(st WFPResultStore, authz AuthZClient, workers ResultWorkerContexts, log *zap.Logger) *WFPResultHandler {
	return &WFPResultHandler{store: st, authz: authz, workers: workers, log: log, concurrency: 8}
}

func RegisterWFPResultRoutes(r chi.Router, h *WFPResultHandler) {
	r.Route("/v1/payroll/wfp/results", func(r chi.Router) {
		r.Post("/", h.Submit)
		r.Post("/from-run", h.FromRun)
		r.Get("/{run_ref_id}", h.Get)
		r.Get("/{run_ref_id}/workers", h.Workers)
		r.Get("/{run_ref_id}/workers/{worker_id}", h.Worker)
		r.Get("/{run_ref_id}/components", h.Components)
		r.Get("/{run_ref_id}/control-totals", h.ControlTotals)
		r.Get("/{run_ref_id}/revalidate-totals", h.Revalidate)
		r.Get("/{run_ref_id}/evidence", h.Evidence)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPResultHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPResultHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
	if strings.TrimSpace(entityID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal entity is required")
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

// loadRef loads a run reference and authorizes against ITS legal entity.
func (h *WFPResultHandler) loadRef(w http.ResponseWriter, r *http.Request, action string) (*domain.PayrollRunRef, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), chi.URLParam(r, "run_ref_id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, action) {
		return nil, "", "", false
	}
	return ref, principal, tenant, true
}

func (h *WFPResultHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRunRefNotFound), errors.Is(err, domain.ErrResultNotFound), errors.Is(err, domain.ErrManifestNotFound),
		errors.Is(err, domain.ErrTrustProfileNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrRunIdentityConflict):
		writeError(w, http.StatusConflict, "RUN_IDENTITY_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrResultFingerprintConflict):
		writeError(w, http.StatusConflict, "RESULT_FINGERPRINT_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrPriorVersionMissing):
		writeError(w, http.StatusConflict, "PRIOR_VERSION_MISSING", err.Error())
	case errors.Is(err, domain.ErrInvalidAcceptanceMove):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrRunNotFinal):
		writeError(w, http.StatusConflict, "RUN_NOT_FINAL", err.Error())
	case errors.Is(err, domain.ErrSourceNotTrusted):
		writeError(w, http.StatusUnprocessableEntity, "SOURCE_NOT_TRUSTED", err.Error())
	case errors.Is(err, domain.ErrManifestNotEffective):
		writeError(w, http.StatusUnprocessableEntity, "RULE_MANIFEST_NOT_EFFECTIVE", err.Error())
	case errors.Is(err, domain.ErrManifestSourceMismatch):
		writeError(w, http.StatusUnprocessableEntity, "RULE_MANIFEST_SOURCE_MISMATCH", err.Error())
	case errors.Is(err, domain.ErrShadowRunNotPayable):
		writeError(w, http.StatusUnprocessableEntity, "SHADOW_RUN_NOT_PAYABLE", err.Error())
	case errors.Is(err, domain.ErrCorrectionBaseMissing):
		writeError(w, http.StatusUnprocessableEntity, "CORRECTION_BASE_MISSING", err.Error())
	case errors.Is(err, domain.ErrTooManyResultWorkers):
		writeError(w, http.StatusUnprocessableEntity, "TOO_MANY_WORKERS", err.Error())
	case errors.Is(err, domain.ErrSourceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "worker_context_unavailable", err.Error())
	case errors.Is(err, domain.ErrPayrollRunNotFound):
		writeError(w, http.StatusUnprocessableEntity, "RUN_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp-04 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll result store unavailable")
	}
}

// workerContexts asks employee-master-svc about every distinct worker, with
// bounded concurrency. Any unreachable answer fails the whole command: an
// unverifiable worker is never assumed valid.
func (h *WFPResultHandler) workerContexts(r *http.Request, principal, tenant string, req domain.ReceivePayrollResultRequest) (map[string]domain.ResultWorkerContext, error) {
	ids := map[string]bool{}
	for _, wk := range req.Workers {
		ids[wk.WorkerID] = true
	}
	out := make(map[string]domain.ResultWorkerContext, len(ids))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		sem      = make(chan struct{}, h.concurrency)
	)
	caller := workerctx.Caller{TenantID: tenant, PrincipalID: principal, LegalEntityID: req.LegalEntityID, CorrelationID: getCorrelationID(r)}
	for id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			wc, err := h.workers.Validate(r.Context(), caller, id, req.LegalEntityID, req.PayDate)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = domain.ErrSourceUnavailable
				}
				return
			}
			out[id] = domain.ResultWorkerContext{Valid: wc.Valid, Reason: wc.Reason, PayCurrencies: wc.PayCurrencies}
		}(id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// inputBatchFinding returns why a named input batch cannot back this result, or "".
func (h *WFPResultHandler) inputBatchFinding(r *http.Request, req domain.ReceivePayrollResultRequest) (string, error) {
	if req.InputBatchID == nil {
		return "", nil
	}
	b, err := h.store.GetInputBatch(r.Context(), *req.InputBatchID)
	switch {
	case errors.Is(err, domain.ErrInputBatchNotFound):
		return "NOT_FOUND", nil
	case err != nil:
		return "", err
	case b.LegalEntityID != req.LegalEntityID:
		return "ENTITY_MISMATCH", nil
	case b.PayrollPeriodID != req.PayrollPeriodID:
		return "PERIOD_MISMATCH", nil
	case b.State != domain.InputBatchFrozen && b.State != domain.InputBatchConsumed:
		return "NOT_FROZEN:" + b.State, nil
	}
	return "", nil
}

// resultEvents turns a committed change into events. No amounts and no personal
// detail travel; consumers read the result through the API.
func (h *WFPResultHandler) resultEvents(r *http.Request, actor, tenant string) store.ResultEventBuilder {
	return func(ch store.ResultChange) ([]domain.OutboxEvent, error) {
		corr, cause := getCorrelationID(r), r.Header.Get("X-Causation-Id")
		var out []domain.OutboxEvent
		add := func(ref *domain.PayrollRunRef, typ string, payload map[string]any) error {
			body, err := events.BuildWFPEnvelope(typ, corr, cause, actor, tenant, ref.LegalEntityID, "", payload)
			if err != nil {
				return err
			}
			// Keyed by source run so every version of one run stays in order.
			out = append(out, domain.OutboxEvent{TenantID: tenant, ObjectID: ref.SourceSystemID + "|" + ref.SourceRunID, EventType: typ, Payload: body})
			return nil
		}
		ref, res := ch.Ref, ch.Result
		base := map[string]any{
			"run_ref_id": ref.RunRefID, "result_id": res.ResultID, "source_system_id": ref.SourceSystemID, "source_run_id": ref.SourceRunID,
			"source_run_version": ref.SourceRunVersion, "run_kind": ref.RunKind, "payroll_period_id": ref.PayrollPeriodID,
			"pay_date": ref.PayDate, "rule_manifest_id": ref.RuleManifestID, "worker_count": res.WorkerCount,
			"currency": res.Currency, "result_fingerprint": res.ResultFingerprint, "evidence_flags": res.EvidenceFlags,
			"acceptance_state": ref.AcceptanceState,
		}
		if ch.Registered {
			if err := add(ref, domain.EventPayrollResultReceived, base); err != nil {
				return nil, err
			}
		}
		if res.Outcome == domain.ResultAccepted {
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			p["supersedes_run_ref_id"] = ref.SupersedesRunRefID
			if err := add(ref, domain.EventPayrollResultAccepted, p); err != nil {
				return nil, err
			}
		} else {
			codes := make([]string, 0, len(res.RejectionReasons))
			for _, rr := range res.RejectionReasons {
				codes = append(codes, rr.Code)
			}
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			p["reason_codes"] = codes
			if err := add(ref, domain.EventPayrollResultRejected, p); err != nil {
				return nil, err
			}
		}
		if ch.Superseded != nil {
			if err := add(ch.Superseded, domain.EventPayrollResultSuperseded, map[string]any{
				"run_ref_id": ch.Superseded.RunRefID, "superseded_by_run_ref_id": ref.RunRefID, "source_run_id": ref.SourceRunID,
				"superseded_version": ch.Superseded.SourceRunVersion, "superseding_version": ref.SourceRunVersion,
			}); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// ── commands ─────────────────────────────────────────────────────────────────

// Submit is ReceivePayrollResult for an external payroll provider.
func (h *WFPResultHandler) Submit(w http.ResponseWriter, r *http.Request) {
	var req domain.ReceivePayrollResultRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok || !h.authorize(w, r, principal, req.LegalEntityID, actionResultSubmit) {
		return
	}
	h.process(w, r, principal, tenant, req, "")
}

type fromRunRequest struct {
	PayrollRunID         string  `json:"payroll_run_id"`
	PayrollPeriodID      string  `json:"payroll_period_id"`
	RuleManifestID       string  `json:"rule_manifest_id"`
	CorrectsPayrollRunID string  `json:"corrects_payroll_run_id,omitempty"`
	InputBatchID         *string `json:"input_batch_id,omitempty"`
	Reason               string  `json:"reason,omitempty"`
}

// FromRun is ReceivePayrollResult for this platform's own payroll: it promotes a
// COMPLETED run, with its payslips, to a WFP result. A correction names the run it
// corrects and becomes the next source_run_version of the ORIGINAL run.
func (h *WFPResultHandler) FromRun(w http.ResponseWriter, r *http.Request) {
	var body fromRunRequest
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	for name, v := range map[string]string{"payroll_run_id": body.PayrollRunID, "rule_manifest_id": body.RuleManifestID} {
		if _, err := uuid.Parse(v); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", name+" must be a UUID")
			return
		}
	}
	if strings.TrimSpace(body.PayrollPeriodID) == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "payroll_period_id is required")
		return
	}
	if body.CorrectsPayrollRunID != "" {
		if _, err := uuid.Parse(body.CorrectsPayrollRunID); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "corrects_payroll_run_id must be a UUID")
			return
		}
		if strings.TrimSpace(body.Reason) == "" {
			writeError(w, http.StatusBadRequest, "validation_failed", "reason is required for a correction")
			return
		}
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetPayrollRun(r.Context(), body.PayrollRunID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, actionResultSubmit) {
		return
	}

	params := domain.ZoikoResultParams{
		PayrollPeriodID: body.PayrollPeriodID, RuleManifestID: body.RuleManifestID, SourceRunID: run.RunID,
		SourceRunVersion: 1, RunKind: domain.RunKindRegular, InputBatchID: body.InputBatchID,
	}
	if body.CorrectsPayrollRunID != "" {
		original, err := h.store.GetPayrollRun(r.Context(), body.CorrectsPayrollRunID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if original.LegalEntityID != run.LegalEntityID {
			writeError(w, http.StatusUnprocessableEntity, "CORRECTION_ENTITY_MISMATCH", "the correction and the original run belong to different legal entities")
			return
		}
		version, found, err := h.store.RunRefVersionForPayrollRun(r.Context(), run.LegalEntityID, domain.SourceZoikoPayroll, original.RunID, run.RunID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if !found {
			latest, err := h.store.LatestRunRefVersion(r.Context(), run.LegalEntityID, domain.SourceZoikoPayroll, original.RunID)
			if err != nil {
				h.writeErr(w, err)
				return
			}
			if latest == 0 {
				h.writeErr(w, domain.ErrCorrectionBaseMissing)
				return
			}
			version = latest + 1
		}
		params.SourceRunID, params.SourceRunVersion, params.RunKind = original.RunID, version, domain.RunKindCorrection
	}

	slips, err := h.store.GetPaySlipsByRun(r.Context(), run.RunID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	req, err := domain.BuildZoikoResultRequest(*run, slips, params)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if err := req.Validate(); err != nil {
		// The run's own figures could not be expressed as a valid submission.
		writeError(w, http.StatusUnprocessableEntity, "RUN_NOT_EXPRESSIBLE", err.Error())
		return
	}
	h.process(w, r, principal, tenant, req, body.Reason)
}

// process runs the gates and controls and stores the outcome. Gate failures
// (untrusted source, missing or ineffective rule manifest, unreachable worker
// service) answer without storing anything; control failures store a REJECTED
// result as evidence.
func (h *WFPResultHandler) process(w http.ResponseWriter, r *http.Request, principal, tenant string, req domain.ReceivePayrollResultRequest, supersedeReason string) {
	profile, err := h.store.GetSourceTrustProfileBySource(r.Context(), req.SourceSystemID)
	if err != nil {
		if errors.Is(err, domain.ErrTrustProfileNotFound) {
			err = domain.ErrSourceNotTrusted
		}
		h.writeErr(w, err)
		return
	}
	if !profile.Allows(domain.CapPayrollResult) {
		h.writeErr(w, domain.ErrSourceNotTrusted)
		return
	}
	manifest, err := h.store.GetRuleManifest(r.Context(), req.RuleManifestID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if manifest.SourceSystemID != req.SourceSystemID {
		h.writeErr(w, domain.ErrManifestSourceMismatch)
		return
	}
	if !manifest.EffectiveOn(req.PayDate) {
		h.writeErr(w, domain.ErrManifestNotEffective)
		return
	}

	ctxs, err := h.workerContexts(r, principal, tenant, req)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	batchReason, err := h.inputBatchFinding(r, req)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	eval := domain.EvaluateResult(tenant, req, ctxs, batchReason)

	ref := domain.PayrollRunRef{
		RunRefID: uuid.NewString(), LegalEntityID: req.LegalEntityID, SourceSystemID: req.SourceSystemID, SourceRunID: req.SourceRunID,
		SourceRunVersion: req.SourceRunVersion, RunKind: req.RunKind, PayrollPeriodID: req.PayrollPeriodID, PayDate: req.PayDate,
		RuleManifestID: req.RuleManifestID, PayrollRunID: req.PayrollRunID,
		IdentityFingerprint: domain.RunIdentityFingerprint(tenant, req.RunRef()), CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	if c := r.Header.Get("X-Causation-Id"); c != "" {
		ref.CausationID = &c
	}
	out, err := h.store.ReceivePayrollResult(r.Context(), store.ReceiveResultInput{
		Ref: ref, Request: req, Evaluation: eval, ReceivedBy: principal, CorrelationID: getCorrelationID(r), SupersedeReason: supersedeReason,
	}, h.resultEvents(r, principal, tenant))
	if err != nil {
		h.writeErr(w, err)
		return
	}

	body := map[string]any{"run_ref": out.Ref, "result": out.Result}
	switch {
	case out.Result.Outcome == domain.ResultRejected:
		body["error_code"], body["error_message"] = "RESULT_REJECTED", "the result failed one or more controls; see result.rejection_reasons"
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case out.Created:
		writeJSON(w, http.StatusCreated, body)
	default:
		writeJSON(w, http.StatusOK, body) // replay of an identical result
	}
}

// ── queries ──────────────────────────────────────────────────────────────────

// Get is GetPayrollResult.
func (h *WFPResultHandler) Get(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_ref": ref, "result": res})
}

func (h *WFPResultHandler) Workers(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	ws, err := h.store.ListResultWorkers(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_ref_id": ref.RunRefID, "workers": ws})
}

// Worker is GetWorkerResult: one worker's row and components.
func (h *WFPResultHandler) Worker(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	workerID := chi.URLParam(r, "worker_id")
	if _, err := uuid.Parse(workerID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "worker_id must be a UUID")
		return
	}
	ws, err := h.store.ListResultWorkers(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	for _, wk := range ws {
		if wk.WorkerID == workerID {
			cs, err := h.store.ListResultComponents(r.Context(), ref.RunRefID, workerID)
			if err != nil {
				h.writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"run_ref_id": ref.RunRefID, "worker": wk, "components": cs})
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "that worker is not in this result")
}

// Components is ListResultComponents.
func (h *WFPResultHandler) Components(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	worker := r.URL.Query().Get("worker_id")
	if worker != "" {
		if _, err := uuid.Parse(worker); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "worker_id must be a UUID")
			return
		}
	}
	cs, err := h.store.ListResultComponents(r.Context(), ref.RunRefID, worker)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_ref_id": ref.RunRefID, "components": cs})
}

// ControlTotals is GetRunControlTotals: what was computed, what the source declared.
func (h *WFPResultHandler) ControlTotals(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_ref_id": ref.RunRefID, "acceptance_state": ref.AcceptanceState, "outcome": res.Outcome, "currency": res.Currency,
		"computed": domain.ComputedTotals{
			WorkerCount: res.WorkerCount, Gross: res.TotalGross, TaxablePay: res.TotalTaxablePay,
			EmployeeDeductions: res.TotalEmployeeDeductions, EmployerLiabilities: res.TotalEmployerLiabilities, NetPay: res.TotalNetPay,
		},
		"declared": res.DeclaredTotals, "result_fingerprint": res.ResultFingerprint,
	})
}

// Revalidate is RevalidateControlTotals: re-derive every total from the stored rows.
// It changes nothing.
func (h *WFPResultHandler) Revalidate(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	diffs, err := h.store.RecomputeResultTotals(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_ref_id": ref.RunRefID, "valid": len(diffs) == 0, "differences": diffs})
}

// Evidence is GetResultEvidence: the run identity, the exact rule manifest, the
// result and its provenance, the input batch it names, and its correction chain.
func (h *WFPResultHandler) Evidence(w http.ResponseWriter, r *http.Request) {
	ref, _, _, ok := h.loadRef(w, r, actionResultView)
	if !ok {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	manifest, err := h.store.GetRuleManifest(r.Context(), ref.RuleManifestID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	var batch *domain.InputBatch
	if res.InputBatchID != nil {
		if batch, err = h.store.GetInputBatch(r.Context(), *res.InputBatchID); err != nil && !errors.Is(err, domain.ErrInputBatchNotFound) {
			h.writeErr(w, err)
			return
		}
	}
	asOriginal, asSuperseding, err := h.store.CorrectionChains(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_ref": ref, "rule_manifest": manifest, "result": res, "input_batch": batch,
		"correction_chain": map[string]any{"supersedes": asSuperseding, "superseded_by": asOriginal},
	})
}
