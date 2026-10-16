package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/paymentstatus"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPReconStore is the persistence surface of Wave 8: reconciliation, exceptions, the close
// gate, Banking outcome evidence, correction handling and the evidence package.
type WFPReconStore interface {
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error)
	GetRuleManifest(ctx context.Context, id string) (*domain.PayrollRuleManifest, error)
	CorrectionChains(ctx context.Context, runRefID string) (asOriginal, asSuperseding []domain.PayrollCorrectionChain, err error)

	ExecuteReconciliation(ctx context.Context, runRefID, actor, correlationID string, reperform bool, mk store.ReconEventBuilder) (*store.ReconOutcome, error)
	CertifyReconRun(ctx context.Context, id, actor string, mk store.ReconEventBuilder) (*domain.ReconRun, error)
	GetReconRun(ctx context.Context, id string) (*domain.ReconRun, error)
	ListReconRuns(ctx context.Context, runRefID string) ([]domain.ReconRun, error)
	ListReconExceptions(ctx context.Context, runRefID, legalEntityID, state string) ([]domain.ReconException, error)
	GetReconException(ctx context.Context, id string) (*domain.ReconException, error)
	AssignReconException(ctx context.Context, id, owner string) (*domain.ReconException, error)
	WaiveReconException(ctx context.Context, id, actor, reason, evidenceRef string, mk store.ReconEventBuilder) (*domain.ReconException, error)
	ResolveExceptionsForSupersededResult(ctx context.Context, runRefID string, mk store.ReconEventBuilder) ([]domain.ReconException, error)

	CloseGateFor(ctx context.Context, legalEntityID, period string) (*domain.CloseGate, error)
	CorrectionCompletenessFor(ctx context.Context, runRefID string) (*domain.CorrectionCompleteness, error)

	ListAccountingBridges(ctx context.Context, runRefID string) ([]domain.AccountingBridge, error)
	WithdrawAccountingBridge(ctx context.Context, id string, expectedVersion int, reason string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, error)
	SupersedeLiabilitiesForRun(ctx context.Context, runRefID string, mk store.LiabilityEventBuilder) (*store.SupersedeOutcome, error)
	SupersedeInstructionsForRun(ctx context.Context, runRefID string, mk store.PaymentEventBuilder) (*store.InstructionSupersedeOutcome, error)
	ListLiabilities(ctx context.Context, f store.LiabilityFilter) ([]domain.PayrollLiability, error)
	ListPaymentInstructions(ctx context.Context, f store.InstructionFilter) ([]domain.PaymentInstruction, error)

	GetPaymentInstruction(ctx context.Context, id string) (*domain.PaymentInstruction, error)
	ListBankingObservations(ctx context.Context, instructionID string) ([]domain.BankingObservation, error)
	RecordBankingObservation(ctx context.Context, instructionID string, o *domain.BankingObservation, actor string, mk store.ReconEventBuilder) (*domain.BankingObservation, bool, error)
	LinkInstructionPayment(ctx context.Context, id, paymentID string) (*domain.PaymentInstruction, error)
	MarkInstructionReturned(ctx context.Context, id string, expectedVersion int, reason string, mk store.ReconEventBuilder) (*domain.PaymentInstruction, error)
}

// PaymentLookup reads a Banking payment's state without judging it.
type PaymentLookup interface {
	Snapshot(ctx context.Context, tenantID, legalEntityID, paymentID string) (*paymentstatus.Snapshot, error)
}

// BankingObserver asks Banking about one instruction (the same code the background refresh uses).
type BankingObserver interface {
	Observe(ctx context.Context, in domain.PaymentInstruction) *domain.BankingObservation
}

const (
	actionReconView    = "WFP_RECON_VIEW"
	actionReconRun     = "WFP_RECON_RUN"
	actionReconCertify = "WFP_RECON_CERTIFY"
	actionReconAssign  = "WFP_RECON_ASSIGN"
	actionReconWaive   = "WFP_RECON_WAIVE"
	actionGateView     = "WFP_CLOSE_GATE_VIEW"
	actionCascade      = "WFP_CORRECTION_CASCADE"
	actionEvidence     = "WFP_EVIDENCE_VIEW"
	actionBankView     = "WFP_BANKING_VIEW"
	actionBankRefresh  = "WFP_BANKING_REFRESH"
	actionBankLink     = "WFP_PAYMENT_LINK"
	actionBankReturn   = "WFP_PAYMENT_RETURN"
)

type WFPReconHandler struct {
	store    WFPReconStore
	authz    AuthZClient
	payments PaymentLookup
	observer BankingObserver
	log      *zap.Logger
}

func NewWFPRecon(st WFPReconStore, authz AuthZClient, payments PaymentLookup, observer BankingObserver, log *zap.Logger) *WFPReconHandler {
	return &WFPReconHandler{store: st, authz: authz, payments: payments, observer: observer, log: log}
}

func RegisterWFPReconRoutes(r chi.Router, h *WFPReconHandler) {
	r.Route("/v1/payroll/wfp/reconciliation", func(r chi.Router) {
		r.Post("/runs", h.Execute)
		r.Get("/runs", h.ListRuns)
		r.Get("/runs/{id}", h.GetRun)
		r.Post("/runs/{id}/certify", h.Certify)
		r.Get("/exceptions", h.ListExceptions)
		r.Get("/exceptions/{id}", h.GetException)
		r.Post("/exceptions/{id}/assign", h.Assign)
		r.Post("/exceptions/{id}/waive", h.Waive)
		r.Get("/close-gate", h.CloseGate)
		r.Get("/corrections/{run_ref_id}/completeness", h.Completeness)
		r.Post("/corrections/{run_ref_id}/cascade", h.Cascade)
		r.Get("/evidence/{run_ref_id}", h.Evidence)
	})
	r.Route("/v1/payroll/wfp/banking-outcomes", func(r chi.Router) {
		r.Get("/{id}", h.Observations)
		r.Post("/{id}/refresh", h.Refresh)
		r.Post("/{id}/link-payment", h.LinkPayment)
		r.Post("/{id}/mark-returned", h.MarkReturned)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPReconHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPReconHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
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

func (h *WFPReconHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRunRefNotFound), errors.Is(err, domain.ErrResultNotFound), errors.Is(err, domain.ErrReconRunNotFound),
		errors.Is(err, domain.ErrReconExceptionNotFound), errors.Is(err, domain.ErrInstructionNotFound), errors.Is(err, domain.ErrPaymentNotFound),
		errors.Is(err, domain.ErrManifestNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInstructionVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrReconSoD):
		writeError(w, http.StatusForbidden, "SEPARATION_OF_DUTIES", err.Error())
	case errors.Is(err, domain.ErrReconResultNotAccepted), errors.Is(err, domain.ErrResultNotSuperseded), errors.Is(err, domain.ErrReconNotMatched),
		errors.Is(err, domain.ErrReconRunStale), errors.Is(err, domain.ErrReconAlreadyCertified), errors.Is(err, domain.ErrReconExceptionClosed),
		errors.Is(err, domain.ErrInstructionState), errors.Is(err, domain.ErrInstructionNotReturned), errors.Is(err, domain.ErrReturnedAfterSettlement),
		errors.Is(err, domain.ErrPaymentLinkMismatch), errors.Is(err, domain.ErrOutcomeUnresolved):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrPaymentEntityMismatch):
		writeError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE", err.Error())
	case errors.Is(err, domain.ErrBankingUnavailable), errors.Is(err, domain.ErrHandoffUnavailable):
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp reconciliation store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll reconciliation store unavailable")
	}
}

func (h *WFPReconHandler) recon(r *http.Request, actor string) store.ReconEventBuilder {
	return func(ch store.ReconChange) ([]domain.OutboxEvent, error) {
		return events.BuildReconEvents(ch.Kind, ch.Run, ch.Exception, ch.Instruction, ch.Observation, actor, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	}
}

// runRef loads a run reference and authorizes the action against ITS legal entity.
func (h *WFPReconHandler) runRef(w http.ResponseWriter, r *http.Request, id, action string) (*domain.PayrollRunRef, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
		return nil, "", "", false
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, action) {
		return nil, "", "", false
	}
	return ref, principal, tenant, true
}

// ── reconciliation runs ──────────────────────────────────────────────────────

type executeRequest struct {
	RunRefID  string `json:"run_ref_id"`
	Reperform bool   `json:"reperform"`
}

// Execute compares one accepted result's frozen population across WFP-04..07 and Banking.
// A difference is recorded as an exception and an event; it is never repaired.
func (h *WFPReconHandler) Execute(w http.ResponseWriter, r *http.Request) {
	var req executeRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	_, principal, _, ok := h.runRef(w, r, req.RunRefID, actionReconRun)
	if !ok {
		return
	}
	out, err := h.store.ExecuteReconciliation(r.Context(), req.RunRefID, principal, getCorrelationID(r), req.Reperform, h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *WFPReconHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := h.runRef(w, r, r.URL.Query().Get("run_ref_id"), actionReconView); !ok {
		return
	}
	out, err := h.store.ListReconRuns(r.Context(), r.URL.Query().Get("run_ref_id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *WFPReconHandler) loadRun(w http.ResponseWriter, r *http.Request, action string) (*domain.ReconRun, string, bool) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return nil, "", false
	}
	run, err := h.store.GetReconRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", false
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, action) {
		return nil, "", false
	}
	return run, principal, true
}

func (h *WFPReconHandler) GetRun(w http.ResponseWriter, r *http.Request) {
	run, _, ok := h.loadRun(w, r, actionReconView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// Certify certifies a MATCHED run whose population is still exactly what it compared.
func (h *WFPReconHandler) Certify(w http.ResponseWriter, r *http.Request) {
	run, principal, ok := h.loadRun(w, r, actionReconCertify)
	if !ok {
		return
	}
	out, err := h.store.CertifyReconRun(r.Context(), run.ReconRunID, principal, h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── exceptions ───────────────────────────────────────────────────────────────

func (h *WFPReconHandler) ListExceptions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	entity := q.Get("legal_entity_id")
	if q.Get("run_ref_id") != "" {
		ref, _, _, ok := h.runRef(w, r, q.Get("run_ref_id"), actionReconView)
		if !ok {
			return
		}
		entity = ref.LegalEntityID
	} else if !h.authorize(w, r, principal, entity, actionReconView) {
		return
	}
	out, err := h.store.ListReconExceptions(r.Context(), q.Get("run_ref_id"), q.Get("legal_entity_id"), q.Get("state"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *WFPReconHandler) loadException(w http.ResponseWriter, r *http.Request, action string) (*domain.ReconException, string, bool) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return nil, "", false
	}
	e, err := h.store.GetReconException(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", false
	}
	if !h.authorize(w, r, principal, e.LegalEntityID, action) {
		return nil, "", false
	}
	return e, principal, true
}

func (h *WFPReconHandler) GetException(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.loadException(w, r, actionReconView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, e)
}

type assignRequest struct {
	Owner string `json:"owner"`
}

func (h *WFPReconHandler) Assign(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Owner) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "owner is required")
		return
	}
	e, _, ok := h.loadException(w, r, actionReconAssign)
	if !ok {
		return
	}
	out, err := h.store.AssignReconException(r.Context(), e.ExceptionID, strings.TrimSpace(req.Owner))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type waiveRequest struct {
	Reason      string `json:"reason"`
	EvidenceRef string `json:"evidence_ref"`
}

// Waive closes an exception with a reason and an evidence reference, by a principal other
// than the one who raised it. A waived exception no longer blocks the close gate; the waiver
// and who made it stay on the record.
func (h *WFPReconHandler) Waive(w http.ResponseWriter, r *http.Request) {
	var req waiveRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Reason) == "" || strings.TrimSpace(req.EvidenceRef) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason and evidence_ref are required")
		return
	}
	e, principal, ok := h.loadException(w, r, actionReconWaive)
	if !ok {
		return
	}
	out, err := h.store.WaiveReconException(r.Context(), e.ExceptionID, principal, strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── close gate ───────────────────────────────────────────────────────────────

// CloseGate (WFP-CTRL-030): GET .../close-gate?legal_entity_id=&period_id=.
func (h *WFPReconHandler) CloseGate(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	entity, period := r.URL.Query().Get("legal_entity_id"), strings.TrimSpace(r.URL.Query().Get("period_id"))
	if period == "" || len(period) > 32 {
		writeError(w, http.StatusBadRequest, "validation_failed", "period_id is required (at most 32 characters)")
		return
	}
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "legal_entity_id must be a UUID")
		return
	}
	if !h.authorize(w, r, principal, entity, actionGateView) {
		return
	}
	g, err := h.store.CloseGateFor(r.Context(), entity, period)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// ── correction chain ─────────────────────────────────────────────────────────

func (h *WFPReconHandler) Completeness(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := h.runRef(w, r, chi.URLParam(r, "run_ref_id"), actionReconView); !ok {
		return
	}
	out, err := h.store.CorrectionCompletenessFor(r.Context(), chi.URLParam(r, "run_ref_id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type CascadeOutcome struct {
	RunRefID                string                             `json:"run_ref_id"`
	AccountingWithdrawn     []string                           `json:"accounting_withdrawn"`
	AccountingNeedsReversal []string                           `json:"accounting_needs_reversal"`
	Liabilities             *store.SupersedeOutcome            `json:"liabilities"`
	Instructions            *store.InstructionSupersedeOutcome `json:"instructions"`
	ExceptionsResolved      int                                `json:"reconciliation_exceptions_resolved"`
	Completeness            *domain.CorrectionCompleteness     `json:"completeness"`
}

// Cascade drives the downstream effects of a superseded result (WFP-RB-09). It does what
// is safe and already authorised by the correction itself: it withdraws unposted journal
// subjects, supersedes unsettled liabilities and unsent instructions, and closes the
// superseded result's reconciliation exceptions. It does NOT reverse a posted journal (a
// reversal is its own submit/SoD/dispatch path), adjust a settled liability, or touch a
// payment that already reached Banking; those are reported as still pending. It is
// idempotent: running it again changes nothing that is already done.
func (h *WFPReconHandler) Cascade(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "run_ref_id")
	ref, principal, _, ok := h.runRef(w, r, id, actionCascade)
	if !ok {
		return
	}
	if ref.AcceptanceState != domain.AcceptanceSuperseded {
		h.writeErr(w, domain.ErrResultNotSuperseded)
		return
	}
	out := &CascadeOutcome{RunRefID: id, AccountingWithdrawn: []string{}, AccountingNeedsReversal: []string{}}
	bridges, err := h.store.ListAccountingBridges(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	accEvents := func(ch store.AccountingChange) ([]domain.OutboxEvent, error) {
		return events.BuildAccountingEvents(string(ch.Kind), ch.Bridge, ch.Original, ch.Reason, principal, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	}
	for _, b := range bridges {
		if b.PostingPurpose != domain.PurposeResult {
			continue
		}
		switch b.State {
		case domain.BridgeEventCreated, domain.BridgeSubmitted, domain.BridgeRejected:
			if _, err := h.store.WithdrawAccountingBridge(r.Context(), b.BridgeID, b.Version, "result superseded by a correction", accEvents); err != nil {
				h.writeErr(w, err)
				return
			}
			out.AccountingWithdrawn = append(out.AccountingWithdrawn, b.BridgeID)
		case domain.BridgePosted:
			out.AccountingNeedsReversal = append(out.AccountingNeedsReversal, b.BridgeID)
		}
	}
	liab, err := h.store.SupersedeLiabilitiesForRun(r.Context(), id, func(ch store.LiabilityChange) ([]domain.OutboxEvent, error) {
		return events.BuildLiabilityEvents(ch.Kind, ch.Liability, ch.Exception, ch.Detail, principal, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	out.Liabilities = liab
	ins, err := h.store.SupersedeInstructionsForRun(r.Context(), id, func(ch store.PaymentChange) ([]domain.OutboxEvent, error) {
		return events.BuildPaymentEvents(ch.Kind, ch.Instruction, ch.Reason, principal, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	out.Instructions = ins
	resolved, err := h.store.ResolveExceptionsForSupersededResult(r.Context(), id, h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	out.ExceptionsResolved = len(resolved)
	if out.Completeness, err = h.store.CorrectionCompletenessFor(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Banking outcomes ─────────────────────────────────────────────────────────

func (h *WFPReconHandler) loadInstruction(w http.ResponseWriter, r *http.Request, action string) (*domain.PaymentInstruction, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	in, err := h.store.GetPaymentInstruction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, in.LegalEntityID, action) {
		return nil, "", "", false
	}
	return in, principal, tenant, true
}

func (h *WFPReconHandler) Observations(w http.ResponseWriter, r *http.Request) {
	in, _, _, ok := h.loadInstruction(w, r, actionBankView)
	if !ok {
		return
	}
	out, err := h.store.ListBankingObservations(r.Context(), in.InstructionID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instruction_id": in.InstructionID, "state": in.State, "payment_id": in.PaymentID, "observations": out})
}

// Refresh asks Banking now (the same code as the background refresh) and records what it says.
func (h *WFPReconHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	in, principal, _, ok := h.loadInstruction(w, r, actionBankRefresh)
	if !ok {
		return
	}
	if in.State != domain.PaySubmitted && in.State != domain.PayHandedOff {
		h.writeErr(w, domain.ErrInstructionState)
		return
	}
	obs := h.observer.Observe(r.Context(), *in)
	if obs == nil {
		h.writeErr(w, domain.ErrHandoffUnavailable)
		return
	}
	rec, added, err := h.store.RecordBankingObservation(r.Context(), in.InstructionID, obs, principal, h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"recorded": added, "observation": rec, "outcome": obs.Outcome})
}

type linkRequest struct {
	PaymentID string `json:"payment_id"`
}

// LinkPayment ties an instruction to the Banking payment it produced, after payment-status-svc
// confirms the payment is in the same legal entity and carries this instruction as its source.
func (h *WFPReconHandler) LinkPayment(w http.ResponseWriter, r *http.Request) {
	var req linkRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.PaymentID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "payment_id is required")
		return
	}
	in, _, tenant, ok := h.loadInstruction(w, r, actionBankLink)
	if !ok {
		return
	}
	snap, err := h.payments.Snapshot(r.Context(), tenant, in.LegalEntityID, strings.TrimSpace(req.PaymentID))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if snap.LegalEntityID != in.LegalEntityID {
		h.writeErr(w, domain.ErrPaymentEntityMismatch)
		return
	}
	if snap.SourceReference != in.InstructionID {
		h.writeErr(w, domain.ErrPaymentLinkMismatch)
		return
	}
	out, err := h.store.LinkInstructionPayment(r.Context(), in.InstructionID, snap.PaymentID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type returnRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

// MarkReturned acknowledges that Banking returned the payment, freeing the instruction's
// amount to be instructed again. Allowed only when Banking's newest observation says RETURNED.
func (h *WFPReconHandler) MarkReturned(w http.ResponseWriter, r *http.Request) {
	var req returnRequest
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
	in, principal, _, ok := h.loadInstruction(w, r, actionBankReturn)
	if !ok {
		return
	}
	out, err := h.store.MarkInstructionReturned(r.Context(), in.InstructionID, version, strings.TrimSpace(req.Reason), h.recon(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── evidence package (WFP-CTRL-025) ──────────────────────────────────────────

type evidenceManifest struct {
	RunRef       domain.PayrollRunRef            `json:"run_ref"`
	RuleManifest *domain.PayrollRuleManifest     `json:"rule_manifest,omitempty"`
	Result       map[string]any                  `json:"result"`
	Corrections  []domain.PayrollCorrectionChain `json:"correction_chains"`
	Accounting   []map[string]any                `json:"accounting"`
	Liabilities  []map[string]any                `json:"liabilities"`
	Instructions []map[string]any                `json:"instructions"`
	ReconRuns    []map[string]any                `json:"reconciliation_runs"`
	OpenExcCount int                             `json:"open_exceptions"`
}

// Evidence assembles the manifest that links one payroll result to its source run, rule
// manifest, accounting, liabilities, payments and control runs. It carries identifiers,
// states and hashes only; no amounts per worker, no bank or payee detail. Its hash lets an
// auditor detect any later difference.
func (h *WFPReconHandler) Evidence(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "run_ref_id")
	ref, _, _, ok := h.runRef(w, r, id, actionEvidence)
	if !ok {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	m := evidenceManifest{RunRef: *ref, Accounting: []map[string]any{}, Liabilities: []map[string]any{}, Instructions: []map[string]any{}, ReconRuns: []map[string]any{}}
	if man, err := h.store.GetRuleManifest(r.Context(), ref.RuleManifestID); err == nil {
		m.RuleManifest = man
	} else if !errors.Is(err, domain.ErrManifestNotFound) {
		h.writeErr(w, err)
		return
	}
	m.Result = map[string]any{
		"result_id": res.ResultID, "outcome": res.Outcome, "result_fingerprint": res.ResultFingerprint, "currency": res.Currency,
		"worker_count": res.WorkerCount, "total_gross": res.TotalGross, "total_net_pay": res.TotalNetPay,
		"total_employee_deductions": res.TotalEmployeeDeductions, "total_employer_liabilities": res.TotalEmployerLiabilities,
		"evidence_flags": res.EvidenceFlags, "source_approval_ref": res.SourceApprovalRef, "calculation_ref": res.CalculationRef,
		"input_batch_id": res.InputBatchID, "received_by": res.ReceivedBy,
	}
	orig, sup, err := h.store.CorrectionChains(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	m.Corrections = append(append([]domain.PayrollCorrectionChain{}, orig...), sup...)
	bridges, err := h.store.ListAccountingBridges(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	for _, b := range bridges {
		m.Accounting = append(m.Accounting, map[string]any{
			"bridge_id": b.BridgeID, "purpose": b.PostingPurpose, "state": b.State, "source_event_id": b.SourceEventID,
			"execution_id": b.ExecutionID, "journal_id": b.JournalID, "total_debit": b.TotalDebit, "total_credit": b.TotalCredit,
			"reconciliation_state": b.ReconciliationState, "evidence_flags": b.EvidenceFlags,
		})
	}
	libs, err := h.store.ListLiabilities(r.Context(), store.LiabilityFilter{RunRefID: id, Limit: 500})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	for _, l := range libs {
		m.Liabilities = append(m.Liabilities, map[string]any{
			"liability_id": l.LiabilityID, "class_code": l.ClassCode, "state": l.State, "amount": l.Amount, "settled_amount": l.SettledAmount,
			"due_date": l.DueDate, "rule_ref": l.RuleRef, "reconciliation_state": l.ReconciliationState,
		})
	}
	ins, err := h.store.ListPaymentInstructions(r.Context(), store.InstructionFilter{RunRefID: id, Limit: 500})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	for _, i := range ins {
		m.Instructions = append(m.Instructions, map[string]any{
			"instruction_id": i.InstructionID, "type": i.InstructionType, "state": i.State, "fingerprint": i.Fingerprint,
			"approved_fingerprint": i.ApprovedFingerprint, "attempt_id": i.AttemptID, "payment_id": i.PaymentID,
		})
	}
	runs, err := h.store.ListReconRuns(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	for _, rr := range runs {
		m.ReconRuns = append(m.ReconRuns, map[string]any{
			"recon_run_id": rr.ReconRunID, "outcome": rr.Outcome, "population_hash": rr.PopulationHash, "executed_by": rr.ExecutedBy,
			"executed_at": rr.ExecutedAt, "certified_by": rr.CertifiedBy, "certified_at": rr.CertifiedAt,
		})
	}
	open, err := h.store.ListReconExceptions(r.Context(), id, "", domain.ReconStateOpen)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	assigned, err := h.store.ListReconExceptions(r.Context(), id, "", domain.ReconStateAssigned)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	m.OpenExcCount = len(open) + len(assigned)

	raw, err := json.Marshal(m)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	sum := sha256.Sum256(raw)
	writeJSON(w, http.StatusOK, map[string]any{"manifest": m, "manifest_hash": hex.EncodeToString(sum[:]), "generated_at": time.Now().UTC()})
}
