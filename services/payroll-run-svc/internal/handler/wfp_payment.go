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
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/paydeps"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFPPaymentStore is the persistence surface of WFP-07.
type WFPPaymentStore interface {
	GetPayrollRunRef(ctx context.Context, id string) (*domain.PayrollRunRef, error)
	GetPayrollResultByRef(ctx context.Context, runRefID string) (*domain.PayrollResult, error)
	ListResultWorkers(ctx context.Context, runRefID string) ([]domain.PayrollResultWorker, error)
	GetLiability(ctx context.Context, id string) (*domain.PayrollLiability, error)
	ReservedForLiability(ctx context.Context, liabilityID string) (string, error)

	CreatePaymentInstruction(ctx context.Context, in *domain.PaymentInstruction, mk store.PaymentEventBuilder) (*domain.PaymentInstruction, bool, error)
	GetPaymentInstruction(ctx context.Context, id string) (*domain.PaymentInstruction, error)
	ListPaymentInstructions(ctx context.Context, f store.InstructionFilter) ([]domain.PaymentInstruction, error)
	ValidatePaymentInstruction(ctx context.Context, id string, expectedVersion int, f store.ValidationFacts) (*domain.PaymentInstruction, error)
	ApprovePaymentInstruction(ctx context.Context, id, approver, fingerprint string, mk store.PaymentEventBuilder) (*domain.PaymentInstruction, error)
	InvalidatePaymentApproval(ctx context.Context, id, reason string, mk store.PaymentEventBuilder) (*domain.PaymentInstruction, error)
	CancelPaymentInstruction(ctx context.Context, id string, expectedVersion int, reason string, mk store.PaymentEventBuilder) (*domain.PaymentInstruction, error)
	SupersedeInstructionsForRun(ctx context.Context, runRefID string, mk store.PaymentEventBuilder) (*store.InstructionSupersedeOutcome, error)
	BeginSubmitToBanking(ctx context.Context, id string, expectedVersion int, submitter string) (*domain.PaymentInstruction, bool, error)
	RecordBankingOutcome(ctx context.Context, id, attemptID, bankingStatus, bankingErr string, mk store.PaymentEventBuilder) (*domain.PaymentInstruction, error)
	ReleaseRefusedSubmission(ctx context.Context, id, reason string) (*domain.PaymentInstruction, error)
	PaymentInstructionFingerprint(ctx context.Context, id string) (state, fingerprint string, err error)
}

type PayeeDestinations interface {
	ActiveDestination(ctx context.Context, tenantID, principalID, legalEntityID, payeeRef string) (*domain.PayeeDestination, error)
}
type WorkerPayees interface {
	PayeeRefFor(ctx context.Context, tenantID, principalID, legalEntityID, workerID, asOf string) (string, error)
}
type PayerAccountVerifier interface {
	Verify(ctx context.Context, tenantID, principalID, legalEntityID, accountID, currency string) error
}
type ApprovalThresholds interface {
	Evaluate(ctx context.Context, tenantID, principalID, legalEntityID string, amount float64) (string, string, error)
}
type BankingHandoffer interface {
	Handoff(ctx context.Context, r paydeps.HandoffRequest) (*paydeps.Attempt, error)
}

type PaymentDeps struct {
	Payees      PayeeDestinations
	Workers     WorkerPayees
	Payers      PayerAccountVerifier
	Policy      ApprovalThresholds
	Banking     BankingHandoffer
	HighValueSi int // signatures required when policy says APPROVAL_REQUIRED
}

const (
	actionPayView      = "WFP_PAYMENT_VIEW"
	actionPayCreate    = "WFP_PAYMENT_CREATE"
	actionPayValidate  = "WFP_PAYMENT_VALIDATE"
	actionPayApprove   = "WFP_PAYMENT_APPROVE"
	actionPayApproveHV = "WFP_PAYMENT_APPROVE_HIGHVALUE"
	actionPayInvalid   = "WFP_PAYMENT_INVALIDATE"
	actionPaySubmit    = "WFP_PAYMENT_SUBMIT"
	actionPayCancel    = "WFP_PAYMENT_CANCEL"
	actionPaySupersede = "WFP_PAYMENT_SUPERSEDE"
)

type WFPPaymentHandler struct {
	store WFPPaymentStore
	authz AuthZClient
	deps  PaymentDeps
	log   *zap.Logger
}

func NewWFPPayment(st WFPPaymentStore, authz AuthZClient, deps PaymentDeps, log *zap.Logger) *WFPPaymentHandler {
	if deps.HighValueSi < 1 {
		deps.HighValueSi = 2
	}
	return &WFPPaymentHandler{store: st, authz: authz, deps: deps, log: log}
}

func RegisterWFPPaymentRoutes(r chi.Router, h *WFPPaymentHandler) {
	r.Route("/v1/payroll/wfp/payment-instructions", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/pending", h.Pending)
		r.Post("/supersede", h.Supersede)
		r.Get("/{id}", h.Get)
		// The adapter's fingerprint re-check: service to service, tenant-scoped, read-only.
		r.Get("/{id}/fingerprint", h.ServiceFingerprint)
		r.Get("/{id}/approval-fingerprint", h.ApprovalFingerprint)
		r.Get("/{id}/banking-status", h.BankingStatus)
		r.Post("/{id}/validate", h.Validate)
		r.Post("/{id}/approve", h.Approve)
		r.Post("/{id}/invalidate", h.Invalidate)
		r.Post("/{id}/submit", h.Submit)
		r.Post("/{id}/cancel", h.Cancel)
	})
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func (h *WFPPaymentHandler) identity(w http.ResponseWriter, r *http.Request) (principal, tenant string, ok bool) {
	principal = r.Header.Get("X-Principal-Id")
	tenant = svcmiddleware.TenantFromContext(r.Context())
	if principal == "" || tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", "", false
	}
	return principal, tenant, true
}

func (h *WFPPaymentHandler) authorize(w http.ResponseWriter, r *http.Request, principal, entityID, action string) bool {
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

func (h *WFPPaymentHandler) writeErr(w http.ResponseWriter, err error) {
	var refused *paydeps.ErrRefused
	switch {
	case errors.Is(err, domain.ErrInstructionNotFound), errors.Is(err, domain.ErrRunRefNotFound), errors.Is(err, domain.ErrResultNotFound),
		errors.Is(err, domain.ErrLiabilityNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInstructionVersionConflict):
		writeError(w, http.StatusConflict, "VERSION_CONFLICT", err.Error())
	case errors.Is(err, domain.ErrApprovalInvalidated):
		writeError(w, http.StatusConflict, "APPROVAL_INVALIDATED", err.Error())
	case errors.Is(err, domain.ErrInstructionState), errors.Is(err, domain.ErrInstructionResultNotActive), errors.Is(err, domain.ErrNotApproved),
		errors.Is(err, domain.ErrResultNotSuperseded), errors.Is(err, domain.ErrAlreadyApproved), errors.Is(err, domain.ErrSubjectNotPayable):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrSoDPreparerOrReceiver), errors.Is(err, domain.ErrSoDPayeeChanger):
		writeError(w, http.StatusForbidden, "SOD_VIOLATION", err.Error())
	case errors.Is(err, domain.ErrNothingToPay), errors.Is(err, domain.ErrExceedsOutstanding), errors.Is(err, domain.ErrWorkerNotInResult),
		errors.Is(err, domain.ErrPayeeMissing), errors.Is(err, domain.ErrNoActivePayee), errors.Is(err, domain.ErrPayeeMismatch),
		errors.Is(err, domain.ErrPayerAccountInvalid):
		writeError(w, http.StatusUnprocessableEntity, "UNPROCESSABLE", err.Error())
	case errors.As(err, &refused):
		writeError(w, http.StatusUnprocessableEntity, "BANKING_REFUSED", err.Error())
	case errors.Is(err, domain.ErrPayeeUnavailable), errors.Is(err, domain.ErrPayerUnavailable), errors.Is(err, domain.ErrPolicyUnavailable),
		errors.Is(err, domain.ErrWorkerProfileUnavailable), errors.Is(err, domain.ErrHandoffUnavailable):
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("wfp-07 store failure", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "payroll payment store unavailable")
	}
}

func (h *WFPPaymentHandler) events(r *http.Request, actor string) store.PaymentEventBuilder {
	return func(ch store.PaymentChange) ([]domain.OutboxEvent, error) {
		return events.BuildPaymentEvents(ch.Kind, ch.Instruction, ch.Reason, actor, getCorrelationID(r), r.Header.Get("X-Causation-Id"))
	}
}

func (h *WFPPaymentHandler) loadAuthorized(w http.ResponseWriter, r *http.Request, action string) (*domain.PaymentInstruction, string, string, bool) {
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return nil, "", "", false
	}
	p, err := h.store.GetPaymentInstruction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principal, p.LegalEntityID, action) {
		return nil, "", "", false
	}
	return p, principal, tenant, true
}

// resolvePayee finds the beneficiary of an instruction's subject from its authority (never from
// payroll data) and checks it can receive this payment: an ACTIVE destination of this legal entity
// in this currency.
func (h *WFPPaymentHandler) resolvePayee(ctx context.Context, tenant, principal string, entityID, payDate string, i *domain.PaymentInstruction) (*domain.PayeeDestination, error) {
	payeeRef := ""
	if i.InstructionType == domain.PayTypeNetPay {
		ref, err := h.deps.Workers.PayeeRefFor(ctx, tenant, principal, entityID, *i.WorkerID, payDate)
		if err != nil {
			return nil, err
		}
		payeeRef = ref
	} else {
		l, err := h.store.GetLiability(ctx, *i.LiabilityID)
		if err != nil {
			return nil, err
		}
		payeeRef = l.AuthorityCounterpartyID
	}
	d, err := h.deps.Payees.ActiveDestination(ctx, tenant, principal, entityID, payeeRef)
	if err != nil {
		return nil, err
	}
	if d.LegalEntityID != entityID || d.Currency != i.Currency {
		return nil, domain.ErrPayeeMismatch
	}
	return d, nil
}

// reverify re-resolves the facts an approval covers and reports whether they still match what
// was pinned: the payee destination (id, version, bank-detail digest), the paying account, the
// payee reference, and the state of the subject.
func (h *WFPPaymentHandler) reverify(ctx context.Context, tenant, principal string, i *domain.PaymentInstruction) (*domain.PayeeDestination, bool, error) {
	ref, err := h.store.GetPayrollRunRef(ctx, i.RunRefID)
	if err != nil {
		return nil, false, err
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted {
		return nil, false, domain.ErrInstructionResultNotActive
	}
	if i.LiabilityID != nil {
		l, err := h.store.GetLiability(ctx, *i.LiabilityID)
		if err != nil {
			return nil, false, err
		}
		if l.ExceptionState != domain.LiabExcNone {
			return nil, false, domain.ErrSubjectNotPayable // a disputed or under-review liability is not paid
		}
	}
	d, err := h.resolvePayee(ctx, tenant, principal, i.LegalEntityID, ref.PayDate, i)
	if err != nil {
		return nil, false, err
	}
	if err := h.deps.Payers.Verify(ctx, tenant, principal, i.LegalEntityID, i.PayerAccountRef, i.Currency); err != nil {
		if errors.Is(err, domain.ErrPayerAccountInvalid) {
			return d, false, nil // the paying account stopped being usable: the approval no longer applies
		}
		return nil, false, err
	}
	same := d.PayeeRef == i.PayeeRef && d.DestinationID == i.DestinationID && d.Version == i.DestinationVersion && d.Fingerprint == i.DestinationFingerprint
	return d, same, nil
}

// ── create ───────────────────────────────────────────────────────────────────

// Create is CreatePayrollPaymentInstruction: a DRAFT built from server-resolved facts only. The
// amount of a NET_PAY is the accepted net pay of the worker; that of a LIABILITY is its outstanding
// amount (less what live instructions already cover). Bank details are never accepted.
func (h *WFPPaymentHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePaymentInstructionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if _, err := uuid.Parse(req.RunRefID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), req.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, actionPayCreate) {
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), ref.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted || res.Outcome != domain.ResultAccepted {
		h.writeErr(w, domain.ErrInstructionResultNotActive)
		return
	}

	in := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: tenant, LegalEntityID: ref.LegalEntityID, InstructionType: req.InstructionType,
		RunRefID: ref.RunRefID, ResultID: res.ResultID, WorkerID: req.WorkerID, LiabilityID: req.LiabilityID,
		PayerAccountRef: req.PayerAccountRef, ValueDate: req.ValueDate, PaymentMethod: req.PaymentMethod,
		CorrelationID: getCorrelationID(r), CreatedBy: principal,
	}
	if req.IdempotencyKey != "" {
		k := req.IdempotencyKey
		in.IdempotencyKey = &k
	}
	classCode := ""
	if req.InstructionType == domain.PayTypeNetPay {
		workers, err := h.store.ListResultWorkers(r.Context(), ref.RunRefID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		found := false
		for _, wk := range workers {
			if wk.WorkerID == *req.WorkerID {
				found = true
				in.Amount, in.Currency = wk.NetPay, wk.Currency
			}
		}
		if !found {
			h.writeErr(w, domain.ErrWorkerNotInResult)
			return
		}
	} else {
		l, err := h.store.GetLiability(r.Context(), *req.LiabilityID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if l.RunRefID != ref.RunRefID {
			h.writeErr(w, domain.ErrSubjectNotPayable)
			return
		}
		classCode, in.Currency = l.ClassCode, l.Currency
		in.Amount = req.Amount
		if in.Amount == "" {
			reserved, err := h.store.ReservedForLiability(r.Context(), l.LiabilityID)
			if err != nil {
				h.writeErr(w, err)
				return
			}
			in.Amount, err = domain.Outstanding(l.OutstandingAmount, "0", reserved)
			if err != nil {
				h.writeErr(w, err)
				return
			}
		}
	}
	if a, err := domain.ParseDecimal(in.Amount); err != nil || a.Sign() <= 0 {
		h.writeErr(w, domain.ErrNothingToPay)
		return
	}
	in.PaymentReference = domain.PaymentReferenceFor(req.InstructionType, ref.PayrollPeriodID, classCode)

	d, err := h.resolvePayee(r.Context(), tenant, principal, ref.LegalEntityID, ref.PayDate, in)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if err := h.deps.Payers.Verify(r.Context(), tenant, principal, ref.LegalEntityID, in.PayerAccountRef, in.Currency); err != nil {
		h.writeErr(w, err)
		return
	}
	in.PayeeRef, in.DestinationID, in.DestinationVersion, in.DestinationFingerprint = d.PayeeRef, d.DestinationID, d.Version, d.Fingerprint
	in.Fingerprint = domain.ComputeInstructionFingerprint(in)

	out, created, err := h.store.CreatePaymentInstruction(r.Context(), in, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

// ── validate / approve / invalidate / cancel ─────────────────────────────────

// Validate is ValidatePayrollPaymentInstruction: re-resolves the beneficiary and paying account,
// asks policy-svc whether the amount needs the high-value approval, and freezes the instruction
// with the signature quorum it requires. An unreachable dependency blocks it.
func (h *WFPPaymentHandler) Validate(w http.ResponseWriter, r *http.Request) {
	var req domain.PaymentTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	p, principal, tenant, ok := h.loadAuthorized(w, r, actionPayValidate)
	if !ok {
		return
	}
	if p.State != domain.PayDraft {
		h.writeErr(w, domain.ErrInstructionState)
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), p.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ref.AcceptanceState != domain.AcceptanceAccepted {
		h.writeErr(w, domain.ErrInstructionResultNotActive)
		return
	}
	if p.LiabilityID != nil {
		l, err := h.store.GetLiability(r.Context(), *p.LiabilityID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if l.ExceptionState != domain.LiabExcNone {
			h.writeErr(w, domain.ErrSubjectNotPayable)
			return
		}
	}
	d, err := h.resolvePayee(r.Context(), tenant, principal, p.LegalEntityID, ref.PayDate, p)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if err := h.deps.Payers.Verify(r.Context(), tenant, principal, p.LegalEntityID, p.PayerAccountRef, p.Currency); err != nil {
		h.writeErr(w, err)
		return
	}
	amount, err := domain.ToLedgerAmount(p.Amount)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	result, policyVersion, err := h.deps.Policy.Evaluate(r.Context(), tenant, principal, p.LegalEntityID, amount)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	sigs := 1
	if result == domain.PolicyApproval {
		sigs = h.deps.HighValueSi
	}
	out, err := h.store.ValidatePaymentInstruction(r.Context(), p.InstructionID, version, store.ValidationFacts{
		Payee: *d, PolicyResult: result, PolicyVersionID: policyVersion, RequiredSignatures: sigs,
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type approveRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// Approve is ApprovePayrollPaymentInstruction. The approver approves the fingerprint they saw;
// a different current fingerprint is a conflict. The preparer, the principal who received the
// result and anyone who changed the payee's bank destination cannot approve. The beneficiary is
// re-resolved first: if it, the paying account or the subject changed since validation, the
// approval is invalidated instead of recorded. A high-value amount (policy) needs the high-value
// action and the configured quorum of distinct approvers.
func (h *WFPPaymentHandler) Approve(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Fingerprint) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "fingerprint is required: approve what you saw")
		return
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetPaymentInstruction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	action := actionPayApprove
	if p.PolicyResult != nil && *p.PolicyResult == domain.PolicyApproval {
		action = actionPayApproveHV
	}
	if !h.authorize(w, r, principal, p.LegalEntityID, action) {
		return
	}
	if p.State != domain.PayValidated {
		h.writeErr(w, domain.ErrInstructionState)
		return
	}
	if req.Fingerprint != p.Fingerprint {
		h.writeErr(w, domain.ErrApprovalInvalidated)
		return
	}
	res, err := h.store.GetPayrollResultByRef(r.Context(), p.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if principal == p.CreatedBy || principal == res.ReceivedBy {
		h.writeErr(w, domain.ErrSoDPreparerOrReceiver)
		return
	}
	d, same, err := h.reverify(r.Context(), tenant, principal, p)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !same {
		if _, ierr := h.store.InvalidatePaymentApproval(r.Context(), p.InstructionID, "PROTECTED_SUBJECT_CHANGED", h.events(r, principal)); ierr != nil {
			h.writeErr(w, ierr)
			return
		}
		h.writeErr(w, domain.ErrApprovalInvalidated)
		return
	}
	if d.ChangedByPrincipal(principal) {
		h.writeErr(w, domain.ErrSoDPayeeChanger)
		return
	}
	out, err := h.store.ApprovePaymentInstruction(r.Context(), p.InstructionID, principal, req.Fingerprint, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Invalidate is InvalidatePaymentApproval: withdraw approval so the instruction returns to DRAFT and
// must be validated and approved again at its new fingerprint.
func (h *WFPPaymentHandler) Invalidate(w http.ResponseWriter, r *http.Request) {
	var req domain.PaymentTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	p, principal, _, ok := h.loadAuthorized(w, r, actionPayInvalid)
	if !ok {
		return
	}
	out, err := h.store.InvalidatePaymentApproval(r.Context(), p.InstructionID, req.Reason, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// Cancel is CancelUnsubmittedInstruction.
func (h *WFPPaymentHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	var req domain.PaymentTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	version, ok := expectedVersion(r, req.ExpectedVersion)
	if !ok || strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) and reason are required")
		return
	}
	p, principal, _, ok := h.loadAuthorized(w, r, actionPayCancel)
	if !ok {
		return
	}
	out, err := h.store.CancelPaymentInstruction(r.Context(), p.InstructionID, version, req.Reason, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── submit to Banking ────────────────────────────────────────────────────────

// Submit is SubmitToBanking: hands an APPROVED instruction to Banking's initiation adapter and
// records the attempt. It never executes, settles or finalizes a payment. The submission is committed
// BEFORE the call, and Banking's idempotency key is deterministic per instruction, so a retry (after a
// timeout, crash or second replica) reuses the same attempt and cannot pay twice. A Banking timeout
// or missing answer leaves the instruction pending (SUBMITTED_TO_BANKING): it is not failed and not
// paid. Only a definitive refusal before any attempt exists releases it back to APPROVED.
func (h *WFPPaymentHandler) Submit(w http.ResponseWriter, r *http.Request) {
	var req domain.PaymentTransitionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	p, principal, tenant, ok := h.loadAuthorized(w, r, actionPaySubmit)
	if !ok {
		return
	}
	if p.State == domain.PayHandedOff {
		writeJSON(w, http.StatusOK, p)
		return
	}
	if p.State != domain.PayApproved && p.State != domain.PaySubmitted {
		h.writeErr(w, domain.ErrNotApproved)
		return
	}
	version, hasVersion := expectedVersion(r, req.ExpectedVersion)
	if p.State == domain.PayApproved && !hasVersion {
		writeError(w, http.StatusBadRequest, "missing_fields", "expected_version (or If-Match) is required")
		return
	}
	if p.State == domain.PayApproved {
		// The facts the approval covers must still hold right now.
		_, same, err := h.reverify(r.Context(), tenant, principal, p)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if !same {
			if _, ierr := h.store.InvalidatePaymentApproval(r.Context(), p.InstructionID, "PROTECTED_SUBJECT_CHANGED", h.events(r, principal)); ierr != nil {
				h.writeErr(w, ierr)
				return
			}
			h.writeErr(w, domain.ErrApprovalInvalidated)
			return
		}
	}
	cur, _, err := h.store.BeginSubmitToBanking(r.Context(), p.InstructionID, version, principal)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	valueDate, err := time.Parse("2006-01-02", cur.ValueDate)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	amount, err := domain.ToLedgerAmount(cur.Amount)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	approved := ""
	if cur.ApprovedFingerprint != nil {
		approved = *cur.ApprovedFingerprint
	}
	attempt, herr := h.deps.Banking.Handoff(r.Context(), paydeps.HandoffRequest{
		TenantID: tenant, PrincipalID: principal, CorrelationID: getCorrelationID(r), InstructionID: cur.InstructionID, LegalEntityID: cur.LegalEntityID,
		Fingerprint: approved, PayerAccountRef: cur.PayerAccountRef, PayeeRef: cur.PayeeRef, Amount: amount, Currency: cur.Currency,
		ValueDate: valueDate, Reference: cur.PaymentReference,
	})
	var refused *paydeps.ErrRefused
	switch {
	case errors.As(herr, &refused):
		if _, rerr := h.store.ReleaseRefusedSubmission(r.Context(), cur.InstructionID, refused.Reason); rerr != nil && !errors.Is(rerr, domain.ErrInstructionState) {
			h.writeErr(w, rerr)
			return
		}
		h.writeErr(w, herr)
	case herr != nil: // unreachable or 5xx: unknown, not failed
		out, rerr := h.store.RecordBankingOutcome(r.Context(), cur.InstructionID, "", "UNKNOWN", herr.Error(), h.events(r, principal))
		if rerr != nil {
			h.writeErr(w, rerr)
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	default:
		out, rerr := h.store.RecordBankingOutcome(r.Context(), cur.InstructionID, attempt.AttemptID, attempt.Status, "", h.events(r, principal))
		if rerr != nil {
			h.writeErr(w, rerr)
			return
		}
		status := http.StatusAccepted // PREPARED / PENDING_UNKNOWN: Banking has it, the provider handoff is not confirmed
		if out.State == domain.PayHandedOff {
			status = http.StatusOK
		}
		writeJSON(w, status, out)
	}
}

// Supersede follows a result's supersession: instructions not yet at Banking become SUPERSEDED; one
// that reached Banking comes back under needs_banking_attention (its original attempt is resolved with
// Banking before anything replaces it).
func (h *WFPPaymentHandler) Supersede(w http.ResponseWriter, r *http.Request) {
	var req domain.SupersedeInstructionsRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	if _, err := uuid.Parse(req.RunRefID); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
		return
	}
	ref, err := h.store.GetPayrollRunRef(r.Context(), req.RunRefID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, ref.LegalEntityID, actionPaySupersede) {
		return
	}
	out, err := h.store.SupersedeInstructionsForRun(r.Context(), req.RunRefID, h.events(r, principal))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ── reads ────────────────────────────────────────────────────────────────────

func (h *WFPPaymentHandler) list(w http.ResponseWriter, r *http.Request, pending bool) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := strings.TrimSpace(q.Get("legal_entity_id"))
	runRef := strings.TrimSpace(q.Get("run_ref_id"))
	if runRef != "" {
		if _, err := uuid.Parse(runRef); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "run_ref_id must be a UUID")
			return
		}
		ref, err := h.store.GetPayrollRunRef(r.Context(), runRef)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		entity = ref.LegalEntityID
	}
	if !h.authorize(w, r, principal, entity, actionPayView) {
		return
	}
	out, err := h.store.ListPaymentInstructions(r.Context(), store.InstructionFilter{
		RunRefID: runRef, LegalEntityID: entity, State: q.Get("state"), LiabilityID: q.Get("liability_id"), PendingOnly: pending,
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "count": len(out)})
}

func (h *WFPPaymentHandler) List(w http.ResponseWriter, r *http.Request)    { h.list(w, r, false) }
func (h *WFPPaymentHandler) Pending(w http.ResponseWriter, r *http.Request) { h.list(w, r, true) }

func (h *WFPPaymentHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := h.loadAuthorized(w, r, actionPayView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *WFPPaymentHandler) ApprovalFingerprint(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := h.loadAuthorized(w, r, actionPayView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instruction_id": p.InstructionID, "state": p.State, "fingerprint": p.Fingerprint, "approved_fingerprint": p.ApprovedFingerprint,
		"required_signatures": p.RequiredSignatures, "signatures_collected": p.SignaturesCollected, "version": p.Version,
	})
}

func (h *WFPPaymentHandler) BankingStatus(w http.ResponseWriter, r *http.Request) {
	p, _, _, ok := h.loadAuthorized(w, r, actionPayView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instruction_id": p.InstructionID, "state": p.State, "attempt_id": p.AttemptID, "banking_status": p.BankingStatus,
		"banking_error": p.BankingError, "submitted_at": p.SubmittedAt, "handed_off_at": p.HandedOffAt,
		"note": "settlement and finality are Banking's: read them from payment-status-svc, not from this record",
	})
}

// ServiceFingerprint is the adapter's re-check of an instruction before it prepares an attempt. It is
// service to service (tenant-scoped like the Treasury transfer fingerprint) and read-only: it returns
// the state and the APPROVED fingerprint (empty when the instruction is not approved).
func (h *WFPPaymentHandler) ServiceFingerprint(w http.ResponseWriter, r *http.Request) {
	tenant := svcmiddleware.TenantFromContext(r.Context())
	if tenant == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return
	}
	id := chi.URLParam(r, "id")
	state, fp, err := h.store.PaymentInstructionFingerprint(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instruction_id": id, "state": state, "fingerprint": fp})
}
