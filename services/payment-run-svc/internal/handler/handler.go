// Package handler exposes payment-run-svc's REST API — AP-11.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-run-svc/internal/authz"
	"zoiko.io/payment-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/payableopenitem"
	"zoiko.io/payment-run-svc/internal/paymentauthorization"
	"zoiko.io/payment-run-svc/internal/paymentproposal"
	"zoiko.io/payment-run-svc/internal/paymentstatus"
	"zoiko.io/payment-run-svc/internal/provideradapter"
	"zoiko.io/payment-run-svc/internal/store"
)

// Action constants — AP-11's own contract's "Authorization / permissions"
// line ("paymentrun.read/create/submit/reconcile;
// paymentrun.exception.resolve"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. Create/Validate/Lock/Cancel/Close all
// reuse Create; RetrySafeInstruction and the manual EXCEPTION flag reuse
// ExceptionResolve.
const (
	PaymentRunRead             = "PAYMENT_RUN_READ"
	PaymentRunCreate           = "PAYMENT_RUN_CREATE"
	PaymentRunSubmit           = "PAYMENT_RUN_SUBMIT"
	PaymentRunReconcile        = "PAYMENT_RUN_RECONCILE"
	PaymentRunExceptionResolve = "PAYMENT_RUN_EXCEPTION_RESOLVE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Clients groups the outbound service clients AP-11 depends on.
type Clients struct {
	Authorization paymentauthorization.Client // AP-10
	Proposal      paymentproposal.Client      // AP-09
	Payables      payableopenitem.Client      // AP-08
	Provider      provideradapter.Client      // BNK-06
	Status        paymentstatus.Client        // BNK-07
}

type Handler struct {
	store     store.Store
	authz     AuthzChecker
	auth      paymentauthorization.Client
	proposals paymentproposal.Client
	payables  payableopenitem.Client
	provider  provideradapter.Client
	status    paymentstatus.Client
	log       *zap.Logger
}

func New(st store.Store, az AuthzChecker, c Clients, log *zap.Logger) *Handler {
	return &Handler{
		store: st, authz: az, auth: c.Authorization, proposals: c.Proposal,
		payables: c.Payables, provider: c.Provider, status: c.Status, log: log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/ap11/runs", func(r chi.Router) {
		r.Post("/", h.CreateRun)
		r.Get("/{runID}", h.GetRun)
		r.Get("/{runID}/instructions", h.ListRunInstructions)
		r.Post("/{runID}/validate", h.ValidatePaymentRun)
		r.Post("/{runID}/lock", h.LockPaymentRun)
		r.Post("/{runID}/submit", h.SubmitPaymentRun)
		r.Post("/{runID}/cancel", h.CancelUnsubmittedRun)
		r.Post("/{runID}/close", h.ClosePaymentRun)
		r.Get("/{runID}/external-status", h.GetExternalStatus)
		r.Get("/{runID}/exceptions", h.GetRunExceptions)
		r.Get("/{runID}/accounting-status", h.GetAccountingStatus)
		r.Post("/{runID}/accounting/requeue", h.RequeueAccounting)
		r.Get("/{runID}/available-actions", h.GetAvailableActions)
		r.Get("/{runID}/history", h.GetRunHistory)
	})
	r.Route("/ap11/instructions", func(r chi.Router) {
		r.Post("/{instructionID}/reconcile", h.ReconcilePaymentRunStatus)
		r.Post("/{instructionID}/poll", h.PollInstructionStatus)
		r.Post("/{instructionID}/retry", h.RetrySafeInstruction)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

func (h *Handler) fetchRunForAuth(w http.ResponseWriter, r *http.Request, runID string) (*domain.PaymentRun, bool) {
	run, err := h.store.FindRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "payment run not found")
			return nil, false
		}
		h.log.Error("fetchRunForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return run, true
}

func (h *Handler) fetchInstruction(w http.ResponseWriter, r *http.Request, instructionID string) (*domain.RunInstruction, bool) {
	ins, err := h.store.FindInstruction(r.Context(), instructionID)
	if err != nil {
		if errors.Is(err, domain.ErrInstructionNotFound) {
			writeError(w, http.StatusNotFound, "run instruction not found")
			return nil, false
		}
		h.log.Error("fetchInstruction: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return ins, true
}

func (h *Handler) writeAuthorizationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationNotEligible):
		writeError(w, http.StatusBadRequest, "an authorization does not exist, does not belong to this legal entity, or is not APPROVED")
	case errors.Is(err, domain.ErrAuthorizedSubjectMismatch):
		writeError(w, http.StatusConflict, domain.ErrAuthorizedSubjectMismatch.Error())
	case errors.Is(err, domain.ErrProposalServiceUnavailable):
		h.log.Error("payment-proposal-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrProposalServiceUnavailable.Error())
	default:
		h.log.Error("payment-authorization-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "payment-authorization-svc unavailable")
	}
}

// ── creation ─────────────────────────────────────────────────────────────────

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

func sameDay(a, b time.Time) bool {
	return a.UTC().Format("2006-01-02") == b.UTC().Format("2006-01-02")
}

// instructionsForAuthorization builds one instruction per payee from the
// authorized AP-09 proposal. It refuses the authorization unless the
// proposal it reads is exactly the subject AP-10 approved (FROZEN, same
// fingerprint, same entity) and the run does not change any authorized
// field (paying account, currency, method, value date). The per-payee net
// totals must add up to the authorization's net amount to the cent.
func instructionsForAuthorization(run domain.CreateRunRequest, a *paymentauthorization.Authorization, p *paymentproposal.Proposal) ([]domain.RunInstruction, error) {
	if p.Status != "FROZEN" || p.Fingerprint == "" || p.Fingerprint != a.ProposalFingerprint || p.LegalEntityID != a.LegalEntityID {
		return nil, domain.ErrAuthorizedSubjectMismatch
	}
	if p.PayingBankAccountRef != run.PayingBankAccountRef || p.PaymentMethod != run.PaymentMethod ||
		p.Currency != run.Currency || a.Currency != run.Currency || !sameDay(p.PaymentDate, run.ValueDate) {
		return nil, domain.ErrAuthorizedSubjectMismatch
	}
	if len(p.Items) == 0 {
		return nil, domain.ErrAuthorizedSubjectMismatch
	}

	byPayee := map[string]*domain.RunInstruction{}
	var payees []string
	var totalCents int64
	for _, it := range p.Items {
		if it.PayeeRef == "" || it.Currency != run.Currency || toCents(it.NetAmount) <= 0 {
			return nil, domain.ErrAuthorizedSubjectMismatch
		}
		ins, ok := byPayee[it.PayeeRef]
		if !ok {
			ins = &domain.RunInstruction{
				AuthorizationID: a.AuthorizationID, AuthorizationFingerprint: a.ProposalFingerprint,
				PayeeRef: it.PayeeRef, Currency: run.Currency,
			}
			byPayee[it.PayeeRef] = ins
			payees = append(payees, it.PayeeRef)
		}
		ins.NetAmount = float64(toCents(ins.NetAmount)+toCents(it.NetAmount)) / 100
		ins.Payables = append(ins.Payables, domain.InstructionPayable{
			PayableSource: it.PayableSource, SourceReference: it.PayableID,
			GrossAmount: it.GrossAmount, WithholdingAmount: it.WithholdingAmount, NetAmount: it.NetAmount,
		})
		totalCents += toCents(it.NetAmount)
	}
	if totalCents != toCents(a.NetAmount) {
		return nil, domain.ErrAuthorizedSubjectMismatch
	}

	sort.Strings(payees)
	out := make([]domain.RunInstruction, 0, len(payees))
	for _, payee := range payees {
		out = append(out, *byPayee[payee])
	}
	return out, nil
}

// CreateRun is where negative-path scenario #4 ("cross-tenant payable
// included in run") is enforced: every authorization named is fetched live
// from AP-10, and its own LegalEntityID/TenantID must match the run's. Each
// authorization is then expanded into one instruction per payee from the
// authorized AP-09 proposal (see instructionsForAuthorization).
func (h *Handler) CreateRun(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.PayingBankAccountRef == "" || req.Currency == "" || req.PaymentMethod == "" || req.ValueDate.IsZero() {
		writeError(w, http.StatusBadRequest, "legal_entity_id, paying_bank_account_ref, currency, payment_method and value_date are required")
		return
	}
	if len(req.AuthorizationIDs) == 0 {
		writeError(w, http.StatusBadRequest, domain.ErrNoAuthorizationIDs.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, PaymentRunCreate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	var instructions []domain.RunInstruction
	seen := map[string]bool{}
	for _, authID := range req.AuthorizationIDs {
		if seen[authID] {
			continue
		}
		seen[authID] = true
		a, err := h.auth.GetApprovedAuthorization(r.Context(), verifiedTenant, req.LegalEntityID, authID)
		if err != nil {
			h.writeAuthorizationErr(w, err)
			return
		}
		proposal, err := h.proposals.GetFrozenProposal(r.Context(), verifiedTenant, principalID, a.ProposalID)
		if err != nil {
			h.writeAuthorizationErr(w, err)
			return
		}
		built, err := instructionsForAuthorization(req, a, proposal)
		if err != nil {
			writeError(w, http.StatusConflict, "authorization "+authID+": "+err.Error())
			return
		}
		instructions = append(instructions, built...)
	}

	run, createdInstructions, err := h.store.CreateRun(r.Context(), verifiedTenant, req, instructions, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrAuthorizationNotEligible) {
			writeError(w, http.StatusConflict, "one of these authorizations is already used by another run")
			return
		}
		h.log.Error("CreateRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"run": run, "instructions": createdInstructions})
}

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("GetRun: failed to list instructions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if instructions == nil {
		instructions = []domain.RunInstruction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"run": run, "instructions": instructions})
}

func (h *Handler) ListRunInstructions(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if _, ok := h.fetchRunForAuth(w, r, runID); !ok {
		return
	}
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("ListRunInstructions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if instructions == nil {
		instructions = []domain.RunInstruction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": instructions, "count": len(instructions)})
}

// distinctAuthorizations returns each authorization once, in instruction
// order — several payee instructions share one authorization, which must be
// validated and consumed exactly once.
func distinctAuthorizations(instructions []domain.RunInstruction) []string {
	seen := map[string]bool{}
	var out []string
	for _, ins := range instructions {
		if !seen[ins.AuthorizationID] {
			seen[ins.AuthorizationID] = true
			out = append(out, ins.AuthorizationID)
		}
	}
	return out
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (h *Handler) ValidatePaymentRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !domain.CanValidate(run.Status) {
		writeError(w, http.StatusConflict, "run must be DRAFT to validate")
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunCreate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("ValidatePaymentRun: failed to list instructions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	for _, authID := range distinctAuthorizations(instructions) {
		valid, err := h.auth.ValidateAuthorization(r.Context(), verifiedTenant, authID)
		if err != nil {
			h.writeAuthorizationErr(w, err)
			return
		}
		if !valid {
			writeError(w, http.StatusConflict, domain.ErrAuthorizationNoLongerValid.Error())
			return
		}
	}

	updated, err := h.store.ValidateRun(r.Context(), runID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "run must be DRAFT to validate")
			return
		}
		h.log.Error("ValidatePaymentRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// LockPaymentRun consumes each distinct AP-10 authorization exactly once
// (several payee instructions can share one). If any consumption fails
// partway through, the run moves to EXCEPTION rather than silently leaving
// some authorizations consumed and others not. Nothing has been sent to
// Banking at this point.
func (h *Handler) LockPaymentRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !domain.CanLock(run.Status) {
		writeError(w, http.StatusConflict, "run must be VALIDATED to lock")
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunCreate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("LockPaymentRun: failed to list instructions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	for _, authID := range distinctAuthorizations(instructions) {
		if err := h.auth.ConsumeAuthorization(r.Context(), verifiedTenant, principalID, authID); err != nil {
			h.log.Error("LockPaymentRun: authorization consumption failed — raising EXCEPTION", zap.String("authorization_id", authID), zap.Error(err))
			if _, exErr := h.store.MarkRunException(r.Context(), runID, "failed to consume authorization "+authID+": "+err.Error(), principalID); exErr != nil {
				h.log.Error("LockPaymentRun: failed to record EXCEPTION", zap.Error(exErr))
			}
			writeError(w, http.StatusConflict, domain.ErrAuthorizationConsumeFailed.Error())
			return
		}
	}
	for _, ins := range instructions {
		if err := h.store.MarkInstructionConsumed(r.Context(), ins.InstructionID); err != nil && !errors.Is(err, domain.ErrInstructionNotFound) {
			h.log.Error("LockPaymentRun: failed to record consumption", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
	}

	updated, err := h.store.LockRun(r.Context(), runID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "run must be VALIDATED to lock")
			return
		}
		h.log.Error("LockPaymentRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// ── Banking hand-off ─────────────────────────────────────────────────────────

// setInstructionStatus records one instruction status change as an
// append-only reconciliation event. ref is deterministic per real-world
// fact, so recording the same fact twice is a no-op.
func (h *Handler) setInstructionStatus(ctx context.Context, principalID, correlationID string, ins *domain.RunInstruction, status domain.InstructionStatus, ref, reason string) error {
	if ins.Status == status {
		return nil
	}
	// The store publishes the instruction event (and, for SETTLED, records the
	// accounting posting request) in the same transaction as the status change.
	updated, _, err := h.store.ReconcileInstruction(ctx, domain.ReconcileInstructionRequest{
		InstructionID: ins.InstructionID, ExternalStatus: status, ProviderEventRef: ref, Reason: reason,
	}, principalID)
	if err != nil {
		return err
	}
	*ins = *updated
	return nil
}

// applyAttemptOutcome maps BNK-06's authoritative attempt status onto the
// instruction. Only SUBMITTED proceeds to BNK-07; PENDING_UNKNOWN (and
// anything this service does not recognize) stays unknown, never failed.
func (h *Handler) applyAttemptOutcome(ctx context.Context, tenantID, principalID, correlationID string, run *domain.PaymentRun, ins *domain.RunInstruction, attempt *provideradapter.Attempt) error {
	ref := "bnk06:" + attempt.AttemptID + ":" + attempt.Status
	switch attempt.Status {
	case provideradapter.AttemptSubmitted:
		if err := h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionPending, ref, "submitted to Banking"); err != nil {
			return err
		}
		if ins.Bnk07PaymentID != "" {
			return nil
		}
		state, err := h.status.RecordInitialStatus(ctx, tenantID, principalID, paymentstatus.RecordInitialStatusRequest{
			LegalEntityID:     run.LegalEntityID,
			ProviderRequestID: attempt.ProviderRequestID,
			SourceReference:   ins.InstructionID,
		})
		if err != nil {
			// The payment is at the bank; only BNK-07's record is missing.
			// The instruction keeps its attempt id, and the next poll
			// records BNK-07 from BNK-06's attempt.
			h.log.Warn("BNK-07 initial status not recorded yet — poll will complete it", zap.String("instruction_id", ins.InstructionID), zap.Error(err))
			return nil
		}
		if err := h.store.SetInstructionBnk07PaymentID(ctx, ins.InstructionID, state.PaymentID); err != nil {
			return err
		}
		ins.Bnk07PaymentID = state.PaymentID
		return nil
	case provideradapter.AttemptRejectedBeforeSubmission:
		return h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionRejected, ref,
			"Banking rejected before submission: "+attempt.RejectionReason)
	case provideradapter.AttemptCancelled, provideradapter.AttemptQuarantined:
		return h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionException, ref,
			"Banking attempt "+attempt.Status+"; needs investigation")
	default: // PENDING_UNKNOWN, an unexpected PREPARED after submit, or an unknown status
		return h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionPendingUnknown,
			"bnk06:"+attempt.AttemptID+":"+provideradapter.AttemptPendingUnknown,
			"Banking has not given an authoritative answer (BNK-06 status "+attempt.Status+")")
	}
}

// markUnknown records that a call to Banking was made but its outcome is
// unknown (timeout, lost response). The attempt id is already persisted, so
// a poll or RetrySafeInstruction can resolve it against the same attempt.
func (h *Handler) markUnknown(ctx context.Context, principalID, correlationID string, ins *domain.RunInstruction, cause error) error {
	return h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionPendingUnknown,
		"bnk06:"+ins.ProviderAttemptID+":"+provideradapter.AttemptPendingUnknown,
		"no authoritative response from Banking: "+cause.Error())
}

// handOff takes one instruction as far as Banking will take it. It returns
// an error only when nothing has been sent to the bank for this instruction
// and the caller should retry later (BNK-06 PrepareAttempt unreachable, or
// a store failure before submit). Every outcome after a submit call — even
// a timeout — is recorded on the instruction, never returned as retryable.
//
// The order is what makes a lost response safe: the BNK-06 attempt is
// prepared and its id persisted before SubmitAttempt is called, so a
// resumed call re-reads that same attempt instead of creating a payment.
func (h *Handler) handOff(ctx context.Context, tenantID, principalID, correlationID string, run *domain.PaymentRun, ins *domain.RunInstruction) error {
	if ins.Status != domain.InstructionPending || ins.Bnk07PaymentID != "" {
		return nil // already handed off or resolved
	}

	var attempt *provideradapter.Attempt
	if ins.ProviderAttemptID == "" {
		a, err := h.provider.Prepare(ctx, tenantID, principalID, provideradapter.PrepareRequest{
			LegalEntityID:   run.LegalEntityID,
			SourceReference: ins.InstructionID,
			PayerAccountRef: run.PayingBankAccountRef,
			PayeeRef:        ins.PayeeRef,
			Amount:          ins.NetAmount,
			Currency:        ins.Currency,
			ExecutionDate:   run.ValueDate,
			// The fingerprint captured from AP-10 at CreateRun, plus which
			// service BNK-06 should re-verify it against.
			AuthorizationID:          ins.AuthorizationID,
			AuthorizationFingerprint: ins.AuthorizationFingerprint,
			AuthorizationSource:      "PAYMENT_AUTHORIZATION_SVC",
			// Caller attestation only: BNK-06 independently verifies the
			// payer account against treasury-svc in PrepareAttempt.
			PayerAccountVerified: true,
			IdempotencyKey:       run.IdempotencyKey + ":" + ins.InstructionID,
		})
		if errors.Is(err, domain.ErrBankingPrepareRejected) {
			return h.setInstructionStatus(ctx, principalID, correlationID, ins, domain.InstructionException,
				"ap11:prepare-rejected:"+ins.InstructionID,
				"Banking refused to prepare the payment (authorization fingerprint or payer account check failed); nothing was sent")
		}
		if err != nil {
			return err
		}
		if err := h.store.SetInstructionAttemptID(ctx, ins.InstructionID, a.AttemptID); err != nil {
			return err
		}
		ins.ProviderAttemptID = a.AttemptID
		attempt = a
	} else {
		a, err := h.provider.GetAttempt(ctx, tenantID, principalID, ins.ProviderAttemptID)
		if err != nil {
			return err
		}
		attempt = a
	}

	if attempt.Status == provideradapter.AttemptPrepared {
		submitted, err := h.provider.Submit(ctx, tenantID, principalID, attempt.AttemptID)
		if errors.Is(err, domain.ErrBankingAttemptConflict) {
			submitted, err = h.provider.GetAttempt(ctx, tenantID, principalID, attempt.AttemptID)
		}
		if err != nil {
			return h.markUnknown(ctx, principalID, correlationID, ins, err)
		}
		attempt = submitted
	}
	return h.applyAttemptOutcome(ctx, tenantID, principalID, correlationID, run, ins, attempt)
}

// SubmitPaymentRun hands every instruction to Banking (BNK-06 then BNK-07).
//
// Negative-path #1 (replay after timeout): the idempotency key is bound to
// the run before anything is sent, a different key is refused, and a replay
// with the same key resumes — instructions already at Banking are skipped,
// so no payment is ever initiated twice.
//
// A Banking timeout is never treated as a failure: that instruction becomes
// PENDING_UNKNOWN and the run follows it. Only "nothing was sent" errors
// leave instructions for a later replay; the response is then 503 and says
// so. Negative-path #3 holds structurally: no path here sets ACCEPTED or
// SETTLED; those come only from BNK-07 via PollInstructionStatus.
func (h *Handler) SubmitPaymentRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	var req domain.SubmitRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, domain.ErrIdempotencyKeyRequired.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunSubmit) {
		return
	}
	if run.IdempotencyKey != "" && run.IdempotencyKey != req.IdempotencyKey {
		writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyMismatch.Error())
		return
	}
	if run.IdempotencyKey == req.IdempotencyKey && run.SubmittedAt != nil && !domain.CanResumeSubmit(run.Status) {
		writeJSON(w, http.StatusOK, run) // replay of a submit that has since progressed
		return
	}
	if !domain.CanResumeSubmit(run.Status) {
		writeError(w, http.StatusConflict, "run must be LOCKED to submit")
		return
	}

	run, err := h.store.BindSubmitKey(r.Context(), runID, req.IdempotencyKey)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrIdempotencyKeyMismatch):
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyMismatch.Error())
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "run must be LOCKED to submit")
		default:
			h.log.Error("SubmitPaymentRun: failed to bind idempotency key", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	correlationID := r.Header.Get("X-Correlation-ID")
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("SubmitPaymentRun: failed to list instructions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	deferred := 0
	for i := range instructions {
		if err := h.handOff(r.Context(), verifiedTenant, principalID, correlationID, run, &instructions[i]); err != nil {
			h.log.Warn("SubmitPaymentRun: instruction not handed off; nothing sent for it", zap.String("instruction_id", instructions[i].InstructionID), zap.Error(err))
			deferred++
		}
	}

	if deferred < len(instructions) && run.Status == domain.StatusLocked {
		run, err = h.store.SubmitRun(r.Context(), runID, req.IdempotencyKey, principalID)
		if err != nil && !errors.Is(err, domain.ErrInvalidTransition) {
			h.log.Error("SubmitPaymentRun: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
	}
	run = h.refreshRunStatus(r.Context(), runID, principalID)

	if deferred > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": fmt.Sprintf("%d instruction(s) could not be handed to Banking and nothing was sent for them; retry SubmitPaymentRun with the same idempotency key", deferred),
			"run":   run,
		})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *Handler) CancelUnsubmittedRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	var req domain.CancelRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !domain.CanCancel(run.Status) {
		writeError(w, http.StatusConflict, "run is not in a cancellable (unsubmitted) state")
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunCreate) {
		return
	}

	updated, err := h.store.CancelRun(r.Context(), runID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "run is not in a cancellable (unsubmitted) state")
			return
		}
		h.log.Error("CancelUnsubmittedRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ClosePaymentRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	var req domain.CloseRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !domain.CanClose(run.Status) {
		writeError(w, http.StatusConflict, "run is not in a closable state")
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunCreate) {
		return
	}

	updated, err := h.store.CloseRun(r.Context(), runID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "run is not in a closable state")
			return
		}
		h.log.Error("ClosePaymentRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── reconciliation ───────────────────────────────────────────────────────────

// ReconcilePaymentRunStatus is the manual operator path. Per invariants
// #18/#19 it can only flag an instruction EXCEPTION for investigation, with
// a reason and an evidence reference (provider_event_ref). ACCEPTED,
// REJECTED and SETTLED come only from Banking via PollInstructionStatus —
// a typed-in SETTLED would close payables without money moving, and a
// typed-in REJECTED could get a payment that did go out paid again.
func (h *Handler) ReconcilePaymentRunStatus(w http.ResponseWriter, r *http.Request) {
	instructionID := chi.URLParam(r, "instructionID")
	var req domain.ReconcileInstructionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExternalStatus != domain.InstructionException {
		writeError(w, http.StatusBadRequest, domain.ErrManualStatusNotAllowed.Error())
		return
	}
	if req.ProviderEventRef == "" {
		writeError(w, http.StatusBadRequest, "provider_event_ref (evidence reference) is required")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, domain.ErrReasonRequired.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ins, ok := h.fetchInstruction(w, r, instructionID)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, ins.RunID)
	if !ok {
		return
	}
	if !domain.CanReconcile(run.Status) {
		writeError(w, http.StatusConflict, "run is not in a reconcilable state")
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunExceptionResolve) {
		return
	}

	updated, applied, err := h.store.ReconcileInstruction(r.Context(), domain.ReconcileInstructionRequest{
		InstructionID: instructionID, ExternalStatus: domain.InstructionException,
		ProviderEventRef: "manual:" + req.ProviderEventRef, Reason: req.Reason,
	}, principalID)
	if err != nil {
		h.log.Error("ReconcilePaymentRunStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if !applied {
		writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": updated, "applied": false, "note": domain.ErrProviderEventAlreadyApplied.Error()})
		return
	}
	h.refreshRunStatus(r.Context(), ins.RunID, principalID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": updated, "applied": true})
}

// mapBankingExecutionStatus maps BNK-07's ExecutionStatus to this service's
// InstructionStatus. PREPARED/SUBMITTED/PENDING report nothing new;
// RETURNED/CANCELLED after hand-off need an operator, so they are
// EXCEPTION.
func mapBankingExecutionStatus(s string) (domain.InstructionStatus, bool) {
	switch s {
	case "ACCEPTED":
		return domain.InstructionAccepted, true
	case "SETTLED":
		return domain.InstructionSettled, true
	case "REJECTED":
		return domain.InstructionRejected, true
	case "RETURNED", "CANCELLED":
		return domain.InstructionException, true
	default: // PREPARED, PENDING, SUBMITTED
		return "", false
	}
}

// settlePayables applies a SETTLED instruction to every AP-08 payable it
// pays (net + withholding, keyed by BNK-07's payment id). A payable AP-08
// did not accept stays unapplied and is retried on the next poll. Returns
// how many are still unapplied.
func (h *Handler) settlePayables(ctx context.Context, tenantID, principalID string, ins *domain.RunInstruction) int {
	pending, err := h.store.ListUnappliedPayables(ctx, ins.InstructionID)
	if err != nil {
		h.log.Error("settlePayables: store unavailable", zap.Error(err))
		return -1
	}
	remaining := 0
	for _, p := range pending {
		err := h.payables.ApplyConfirmedPayment(ctx, tenantID, principalID, payableopenitem.ApplyRequest{
			PayableSource: p.PayableSource, SourceReference: p.SourceReference,
			NetAmount: p.NetAmount, WithholdingAmount: p.WithholdingAmount, Bnk07PaymentID: ins.Bnk07PaymentID,
		})
		if err == nil {
			err = h.store.MarkPayableApplied(ctx, ins.InstructionID, p.PayableSource, p.SourceReference)
		}
		if err != nil {
			h.log.Warn("settlePayables: AP-08 settlement not applied yet; will retry on next poll",
				zap.String("instruction_id", ins.InstructionID), zap.String("source_reference", p.SourceReference), zap.Error(err))
			remaining++
		}
	}
	return remaining
}

// PollInstructionStatus reconciles an instruction from Banking's own
// records:
//
//  1. If BNK-07 has no record yet (hand-off interrupted, or the attempt was
//     PENDING_UNKNOWN), it re-reads the same BNK-06 attempt and applies its
//     authoritative status — never re-submitting.
//  2. It then reads BNK-07's canonical status and applies it. The event ref
//     ("<payment_id>:<status>") is deterministic, so polling the same status
//     twice is idempotent (negative-path #2).
//  3. A SETTLED instruction settles its AP-08 payables; any AP-08 failure is
//     retried on the next poll.
func (h *Handler) PollInstructionStatus(w http.ResponseWriter, r *http.Request) {
	instructionID := chi.URLParam(r, "instructionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ins, ok := h.fetchInstruction(w, r, instructionID)
	if !ok {
		return
	}
	if ins.ProviderAttemptID == "" {
		writeError(w, http.StatusConflict, domain.ErrInstructionNotSubmitted.Error())
		return
	}
	run, ok := h.fetchRunForAuth(w, r, ins.RunID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunReconcile) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	correlationID := r.Header.Get("X-Correlation-ID")
	before := ins.Status

	if ins.Bnk07PaymentID == "" {
		attempt, err := h.provider.GetAttempt(r.Context(), verifiedTenant, principalID, ins.ProviderAttemptID)
		if err != nil {
			h.log.Error("PollInstructionStatus: BNK-06 attempt lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, domain.ErrProviderAdapterUnavailable.Error())
			return
		}
		if attempt.Status == provideradapter.AttemptPrepared {
			writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": ins, "applied": false,
				"note": "the Banking attempt is prepared but was never submitted; call SubmitPaymentRun with the run's idempotency key"})
			return
		}
		if err := h.applyAttemptOutcome(r.Context(), verifiedTenant, principalID, correlationID, run, ins, attempt); err != nil {
			h.log.Error("PollInstructionStatus: failed to apply BNK-06 outcome", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
		if ins.Bnk07PaymentID == "" {
			h.refreshRunStatus(r.Context(), ins.RunID, principalID)
			writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": ins, "applied": ins.Status != before,
				"note": "Banking has no execution record yet (BNK-06 status " + attempt.Status + ")"})
			return
		}
	}

	state, err := h.status.GetStatus(r.Context(), verifiedTenant, ins.Bnk07PaymentID)
	if err != nil {
		h.log.Error("PollInstructionStatus: payment-status-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, domain.ErrPaymentStatusUnavailable.Error())
		return
	}
	note := ""
	if state.HasOpenConflict {
		note = "BNK-07 reports conflicting evidence for this payment; not applied until BNK-07 resolves it"
	} else if mapped, hasNews := mapBankingExecutionStatus(state.Status); hasNews {
		if err := h.setInstructionStatus(r.Context(), principalID, correlationID, ins, mapped,
			ins.Bnk07PaymentID+":"+state.Status, "BNK-07 status "+state.Status); err != nil {
			h.log.Error("PollInstructionStatus: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
			return
		}
	} else {
		note = "Banking has not reached a status this service reconciles on yet (" + state.Status + ")"
	}

	resp := map[string]interface{}{"instruction": ins, "applied": ins.Status != before}
	if note != "" {
		resp["note"] = note
	}
	if ins.Status == domain.InstructionSettled {
		resp["payables_pending_settlement"] = h.settlePayables(r.Context(), verifiedTenant, principalID, ins)
	}
	h.refreshRunStatus(r.Context(), ins.RunID, principalID)
	writeJSON(w, http.StatusOK, resp)
}

// recomputeRunStatus derives the run's aggregate status from its
// instructions — the only way a run's status moves past SUBMITTED
// (negative-path #3). Unknown outranks pending: while any instruction's
// outcome is unknown, the run is PENDING_UNKNOWN.
func recomputeRunStatus(instructions []domain.RunInstruction) (domain.RunStatus, bool) {
	if len(instructions) == 0 {
		return "", false
	}
	var pending, unknown, rejected, settled, exception int
	for _, ins := range instructions {
		switch ins.Status {
		case domain.InstructionPending:
			pending++
		case domain.InstructionPendingUnknown:
			unknown++
		case domain.InstructionRejected:
			rejected++
		case domain.InstructionSettled:
			settled++
		case domain.InstructionException:
			exception++
		}
	}
	switch {
	case exception > 0:
		return domain.StatusException, true
	case unknown > 0:
		return domain.StatusPendingUnknown, true
	case pending > 0:
		return domain.StatusSubmitted, true
	case settled == len(instructions):
		return domain.StatusSettled, true
	case rejected == len(instructions):
		return domain.StatusRejected, true
	case rejected > 0:
		return domain.StatusPartiallyAccepted, true
	default:
		return domain.StatusAccepted, true
	}
}

// refreshRunStatus recomputes a submitted run's aggregate status and stores
// it when it changed. A run that was never submitted is left alone (the
// store refuses the update).
func (h *Handler) refreshRunStatus(ctx context.Context, runID, principalID string) *domain.PaymentRun {
	run, err := h.store.FindRun(ctx, runID)
	if err != nil {
		h.log.Error("refreshRunStatus: failed to read run", zap.Error(err))
		return nil
	}
	if run.SubmittedAt == nil {
		return run
	}
	instructions, err := h.store.ListInstructions(ctx, runID)
	if err != nil {
		h.log.Error("refreshRunStatus: failed to list instructions", zap.Error(err))
		return run
	}
	target, ok := recomputeRunStatus(instructions)
	if !ok || target == run.Status {
		return run
	}
	updated, err := h.store.UpdateRunAggregateStatus(ctx, runID, target, principalID)
	if err != nil {
		if !errors.Is(err, domain.ErrInvalidTransition) {
			h.log.Error("refreshRunStatus: failed to update run aggregate status", zap.Error(err))
		}
		return run
	}
	return updated
}

// RetrySafeInstruction re-sends a PENDING_UNKNOWN instruction through BNK-06
// RetrySameAttempt — the same attempt and idempotency key, so the provider
// sees a duplicate of the original request, never a second payment.
func (h *Handler) RetrySafeInstruction(w http.ResponseWriter, r *http.Request) {
	instructionID := chi.URLParam(r, "instructionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ins, ok := h.fetchInstruction(w, r, instructionID)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, ins.RunID)
	if !ok {
		return
	}
	if !domain.CanRetry(run.Status) {
		writeError(w, http.StatusConflict, "run is not in a retryable state")
		return
	}
	if ins.Status != domain.InstructionPendingUnknown || ins.ProviderAttemptID == "" {
		writeError(w, http.StatusConflict, domain.ErrInstructionNotRetryable.Error())
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunExceptionResolve) {
		return
	}

	if err := h.store.RetryInstruction(r.Context(), instructionID, principalID); err != nil {
		h.log.Error("RetrySafeInstruction: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	correlationID := r.Header.Get("X-Correlation-ID")
	attempt, err := h.provider.Retry(r.Context(), verifiedTenant, principalID, ins.ProviderAttemptID)
	if errors.Is(err, domain.ErrBankingAttemptConflict) {
		// The attempt already left PENDING_UNKNOWN; read where it went.
		attempt, err = h.provider.GetAttempt(r.Context(), verifiedTenant, principalID, ins.ProviderAttemptID)
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "Banking did not answer; the instruction remains PENDING_UNKNOWN", "instruction": ins,
		})
		return
	}
	if err := h.applyAttemptOutcome(r.Context(), verifiedTenant, principalID, correlationID, run, ins, attempt); err != nil {
		h.log.Error("RetrySafeInstruction: failed to apply BNK-06 outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	h.refreshRunStatus(r.Context(), ins.RunID, principalID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": ins, "attempt_status": attempt.Status})
}

// ── queries ──────────────────────────────────────────────────────────────────

func (h *Handler) GetExternalStatus(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("GetExternalStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if instructions == nil {
		instructions = []domain.RunInstruction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"run_id": runID, "status": run.Status, "instructions": instructions})
}

func (h *Handler) GetRunExceptions(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("GetRunExceptions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	exceptions := []domain.RunInstruction{}
	unknown := []domain.RunInstruction{}
	for _, ins := range instructions {
		switch ins.Status {
		case domain.InstructionException:
			exceptions = append(exceptions, ins)
		case domain.InstructionPendingUnknown:
			unknown = append(unknown, ins)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"run_id": runID, "run_status": run.Status, "run_exception_reason": run.ExceptionReason,
		"instruction_exceptions": exceptions, "instructions_pending_unknown": unknown,
	})
}

// GetAccountingStatus is a read-only projection of this service's own
// state: what has settled at the bank, how much of it AP-08 has recorded, and
// where each settlement's ACC-04 posting request stands (PENDING, POSTED,
// FAILED, QUARANTINED). AP-11 never writes the ledger itself.
func (h *Handler) GetAccountingStatus(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	instructions, err := h.store.ListInstructions(r.Context(), runID)
	if err != nil {
		h.log.Error("GetAccountingStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	requests, err := h.store.ListAccountingRequests(r.Context(), runID)
	if err != nil {
		h.log.Error("GetAccountingStatus: accounting requests unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	postings := make([]map[string]interface{}, 0, len(requests))
	counts := map[string]int{}
	for _, q := range requests {
		counts[q.Status]++
		postings = append(postings, map[string]interface{}{
			"instruction_id": q.InstructionID, "source_event_id": q.SourceEventID, "status": q.Status,
			"attempts": q.Attempts, "last_error": q.LastError, "posting_execution_id": q.PostingExecutionID,
		})
	}
	var settledCents int64
	var payablesApplied, payablesPending int
	for _, ins := range instructions {
		if ins.Status != domain.InstructionSettled {
			continue
		}
		settledCents += toCents(ins.NetAmount)
		for _, p := range ins.Payables {
			if p.PayableAppliedAt != nil {
				payablesApplied++
			} else {
				payablesPending++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"run_id": runID, "run_status": run.Status, "settled_total": float64(settledCents) / 100, "currency": run.Currency,
		"payables_settled_in_ap": payablesApplied, "payables_pending_settlement": payablesPending,
		"postings": postings, "posting_counts": counts,
	})
}

// RequeueAccounting puts the run's FAILED/QUARANTINED posting requests back to
// PENDING once an operator has fixed the cause (for example a missing ACC-02
// mapping). A POSTED request is never touched. Requeueing re-submits the same
// source_event_id, which the ledger treats idempotently.
func (h *Handler) RequeueAccounting(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunExceptionResolve) {
		return
	}
	n, err := h.store.RequeueAccountingRequests(r.Context(), runID)
	if err != nil {
		if errors.Is(err, domain.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "payment run not found")
			return
		}
		h.log.Error("RequeueAccounting: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"run_id": runID, "requeued": n})
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, ok := h.fetchRunForAuth(w, r, runID)
	if !ok {
		return
	}
	var actions []string
	if domain.CanValidate(run.Status) {
		actions = append(actions, "ValidatePaymentRun")
	}
	if domain.CanLock(run.Status) {
		actions = append(actions, "LockPaymentRun")
	}
	if domain.CanSubmit(run.Status) {
		actions = append(actions, "SubmitPaymentRun")
	}
	if domain.CanCancel(run.Status) {
		actions = append(actions, "CancelUnsubmittedRun")
	}
	if domain.CanReconcile(run.Status) {
		actions = append(actions, "ReconcilePaymentRunStatus", "PollInstructionStatus")
	}
	if domain.CanRetry(run.Status) {
		actions = append(actions, "RetrySafeInstruction")
	}
	if domain.CanClose(run.Status) {
		actions = append(actions, "ClosePaymentRun")
	}
	if actions == nil {
		actions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"run_id": runID, "status": run.Status, "available_actions": actions})
}

func (h *Handler) GetRunHistory(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if _, ok := h.fetchRunForAuth(w, r, runID); !ok {
		return
	}
	events, err := h.store.ListEvents(r.Context(), runID)
	if err != nil {
		h.log.Error("GetRunHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.RunEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}
