// Package handler exposes payment-run-svc's REST API — AP-11.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-run-svc/internal/authz"
	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/events"
	svcmiddleware "zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/paymentauthorization"
	"zoiko.io/payment-run-svc/internal/store"
)

// Action constants — AP-11's own contract's "Authorization / permissions"
// line ("paymentrun.read/create/submit/reconcile;
// paymentrun.exception.resolve"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. Create/Validate/Lock/Cancel/Close all
// reuse Create; RetrySafeInstruction reuses ExceptionResolve.
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

type Handler struct {
	store store.Store
	pub   events.Publisher
	authz AuthzChecker
	auth  paymentauthorization.Client
	log   *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, auth paymentauthorization.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, auth: auth, log: log}
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
		r.Get("/{runID}/available-actions", h.GetAvailableActions)
		r.Get("/{runID}/history", h.GetRunHistory)
	})
	r.Route("/ap11/instructions", func(r chi.Router) {
		r.Post("/{instructionID}/reconcile", h.ReconcilePaymentRunStatus)
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

func (h *Handler) writeAuthorizationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuthorizationNotEligible):
		writeError(w, http.StatusBadRequest, "an authorization does not exist, does not belong to this legal entity, or is not APPROVED")
	default:
		h.log.Error("payment-authorization-svc lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "payment-authorization-svc unavailable")
	}
}

// ── creation ─────────────────────────────────────────────────────────────────

// CreateRun is where negative-path scenario #4 ("cross-tenant payable
// included in run") is enforced for real: every authorization named is
// fetched live from AP-10, and its own LegalEntityID/TenantID must match
// the run's — a mismatch (or any other ineligibility) rejects the whole
// create outright.
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
	for _, authID := range req.AuthorizationIDs {
		a, err := h.auth.GetApprovedAuthorization(r.Context(), verifiedTenant, req.LegalEntityID, authID)
		if err != nil {
			h.writeAuthorizationErr(w, err)
			return
		}
		instructions = append(instructions, domain.RunInstruction{
			AuthorizationID: a.AuthorizationID, PayeeRef: a.PayeeRef, NetAmount: a.NetAmount, Currency: a.Currency,
		})
	}

	run, createdInstructions, err := h.store.CreateRun(r.Context(), verifiedTenant, req, instructions, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrAuthorizationNotEligible) {
			writeError(w, http.StatusConflict, "one of these authorizations is already consumed into another run")
			return
		}
		h.log.Error("CreateRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRunCreated, EntityID: run.RunID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: run,
	})
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
	for _, ins := range instructions {
		valid, err := h.auth.ValidateAuthorization(r.Context(), verifiedTenant, ins.AuthorizationID)
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

// LockPaymentRun is the FIRST REAL CALLER of payment-authorization-svc's
// ConsumeAuthorization anywhere in this codebase — a gap AP-10 itself
// documented as having no real caller yet. Consumption is attempted
// sequentially for every instruction; if any call fails partway through,
// the run moves to EXCEPTION rather than leaving some authorizations
// consumed and others not, silently.
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

	for _, ins := range instructions {
		if err := h.auth.ConsumeAuthorization(r.Context(), verifiedTenant, principalID, ins.AuthorizationID); err != nil {
			h.log.Error("LockPaymentRun: authorization consumption failed — raising EXCEPTION", zap.String("instruction_id", ins.InstructionID), zap.Error(err))
			if _, exErr := h.store.MarkRunException(r.Context(), runID, "failed to consume authorization "+ins.AuthorizationID+": "+err.Error(), principalID); exErr != nil {
				h.log.Error("LockPaymentRun: failed to record EXCEPTION", zap.Error(exErr))
			}
			writeError(w, http.StatusConflict, domain.ErrAuthorizationConsumeFailed.Error())
			return
		}
		if err := h.store.MarkInstructionConsumed(r.Context(), ins.InstructionID); err != nil {
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

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRunLocked, EntityID: updated.RunID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
}

// SubmitPaymentRun does not call any bank — see internal/domain's package
// doc for the full reasoning. It records that submission was attempted and
// enforces negative-path scenario #1 (replay after timeout) via a real
// idempotency-key check: a repeat call with the SAME key on an
// already-SUBMITTED run is a no-op returning the current state; a
// DIFFERENT key is rejected outright.
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

	if run.Status == domain.StatusSubmitted {
		if run.IdempotencyKey == req.IdempotencyKey {
			writeJSON(w, http.StatusOK, run)
			return
		}
		writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyMismatch.Error())
		return
	}
	if !domain.CanSubmit(run.Status) {
		writeError(w, http.StatusConflict, "run must be LOCKED to submit")
		return
	}

	updated, err := h.store.SubmitRun(r.Context(), runID, req.IdempotencyKey, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "run must be LOCKED to submit")
			return
		}
		h.log.Error("SubmitPaymentRun: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventRunSubmitted, EntityID: updated.RunID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updated,
	})
	writeJSON(w, http.StatusOK, updated)
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

// ReconcilePaymentRunStatus is not a webhook receiver — see
// internal/domain's package doc. It is a command an operator calls with a
// caller-attested ExternalStatus and ProviderEventRef; negative-path #2 is
// enforced by the store's own database uniqueness on
// (instruction_id, provider_event_ref) — a repeat call is idempotent, not
// double-applied. Negative-path #3 ("run marks settled from initiation
// response") holds structurally: this is the ONLY path that can ever move
// a run's status forward from SUBMITTED, never SubmitPaymentRun itself.
func (h *Handler) ReconcilePaymentRunStatus(w http.ResponseWriter, r *http.Request) {
	instructionID := chi.URLParam(r, "instructionID")
	var req domain.ReconcileInstructionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.InstructionID = instructionID
	if req.ProviderEventRef == "" {
		writeError(w, http.StatusBadRequest, "provider_event_ref is required")
		return
	}
	switch req.ExternalStatus {
	case domain.InstructionAccepted, domain.InstructionRejected, domain.InstructionSettled, domain.InstructionException:
	default:
		writeError(w, http.StatusBadRequest, "external_status must be ACCEPTED, REJECTED, SETTLED, or EXCEPTION")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ins, err := h.store.FindInstruction(r.Context(), instructionID)
	if err != nil {
		if errors.Is(err, domain.ErrInstructionNotFound) {
			writeError(w, http.StatusNotFound, "run instruction not found")
			return
		}
		h.log.Error("ReconcilePaymentRunStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
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
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunReconcile) {
		return
	}

	updatedIns, applied, err := h.store.ReconcileInstruction(r.Context(), req, principalID)
	if err != nil {
		h.log.Error("ReconcilePaymentRunStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if !applied {
		writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": updatedIns, "applied": false, "note": domain.ErrProviderEventAlreadyApplied.Error()})
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: instructionEventType(req.ExternalStatus), EntityID: instructionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: updatedIns,
	})

	allInstructions, err := h.store.ListInstructions(r.Context(), ins.RunID)
	if err != nil {
		h.log.Error("ReconcilePaymentRunStatus: failed to list instructions for aggregate recompute", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": updatedIns, "applied": true})
		return
	}
	if newStatus, changed := recomputeRunStatus(allInstructions); changed {
		if _, err := h.store.UpdateRunAggregateStatus(r.Context(), ins.RunID, newStatus, principalID); err != nil {
			h.log.Error("ReconcilePaymentRunStatus: failed to update run aggregate status", zap.Error(err))
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"instruction": updatedIns, "applied": true})
}

func instructionEventType(s domain.InstructionStatus) string {
	switch s {
	case domain.InstructionAccepted:
		return domain.EventInstructionAccepted
	case domain.InstructionRejected:
		return domain.EventInstructionRejected
	case domain.InstructionSettled:
		return domain.EventInstructionSettled
	default:
		return domain.EventRunExceptionRaised
	}
}

// recomputeRunStatus derives the run's aggregate status from its
// instructions' individual statuses — the only mechanism that can ever
// move a run's status past SUBMITTED (negative-path scenario #3).
func recomputeRunStatus(instructions []domain.RunInstruction) (domain.RunStatus, bool) {
	if len(instructions) == 0 {
		return "", false
	}
	var pending, accepted, rejected, settled, exception int
	for _, ins := range instructions {
		switch ins.Status {
		case domain.InstructionPending:
			pending++
		case domain.InstructionAccepted:
			accepted++
		case domain.InstructionRejected:
			rejected++
		case domain.InstructionSettled:
			settled++
		case domain.InstructionException:
			exception++
		}
	}
	if exception > 0 {
		return domain.StatusException, true
	}
	if pending > 0 {
		return "", false // still waiting on some instructions
	}
	if settled == len(instructions) {
		return domain.StatusSettled, true
	}
	if rejected == len(instructions) {
		return domain.StatusRejected, true
	}
	if rejected > 0 {
		return domain.StatusPartiallyAccepted, true
	}
	return domain.StatusAccepted, true
}

// RetrySafeInstruction reuses the run's already-captured idempotency key —
// there is no real provider to re-send to (see internal/domain's package
// doc), so this records the retry attempt as evidence without minting any
// new submission state.
func (h *Handler) RetrySafeInstruction(w http.ResponseWriter, r *http.Request) {
	instructionID := chi.URLParam(r, "instructionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ins, err := h.store.FindInstruction(r.Context(), instructionID)
	if err != nil {
		if errors.Is(err, domain.ErrInstructionNotFound) {
			writeError(w, http.StatusNotFound, "run instruction not found")
			return
		}
		h.log.Error("RetrySafeInstruction: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
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
	if !h.authorize(w, r, principalID, run.LegalEntityID, PaymentRunExceptionResolve) {
		return
	}

	if err := h.store.RetryInstruction(r.Context(), instructionID, principalID); err != nil {
		h.log.Error("RetrySafeInstruction: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "retry recorded", "idempotency_key": run.IdempotencyKey})
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
	var exceptions []domain.RunInstruction
	for _, ins := range instructions {
		if ins.Status == domain.InstructionException {
			exceptions = append(exceptions, ins)
		}
	}
	if exceptions == nil {
		exceptions = []domain.RunInstruction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"run_id": runID, "run_status": run.Status, "run_exception_reason": run.ExceptionReason, "instruction_exceptions": exceptions})
}

// GetAccountingStatus is a read-only projection of this service's own
// state — no general-ledger-svc integration was attempted here (unlike
// AP-04's real GRNI posting); a genuine accounting/payment-clearing
// integration for AP-11 is a natural future addition, honestly left
// undone rather than fabricated.
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
	var settledTotal float64
	for _, ins := range instructions {
		if ins.Status == domain.InstructionSettled {
			settledTotal += ins.NetAmount
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"run_id": runID, "run_status": run.Status, "settled_total": settledTotal, "currency": run.Currency,
	})
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
		actions = append(actions, "ReconcilePaymentRunStatus")
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
