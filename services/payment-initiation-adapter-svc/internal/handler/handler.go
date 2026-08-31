// Package handler exposes payment-initiation-adapter-svc's REST API — BNK-06.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-initiation-adapter-svc/internal/authz"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/events"
	svcmiddleware "zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/provideradapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/store"
)

// Action constants — BNK-06's own contract's "Permissions" line
// ("payment.initiate; payment.attempt.read; payment.attempt.resolve;
// payment.cancel.presubmit"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. Quarantine reuses Resolve (both are
// operator-exception actions).
const (
	PaymentInitiate        = "PAYMENT_INITIATE"
	PaymentAttemptRead     = "PAYMENT_ATTEMPT_READ"
	PaymentAttemptResolve  = "PAYMENT_ATTEMPT_RESOLVE"
	PaymentCancelPresubmit = "PAYMENT_CANCEL_PRESUBMIT"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store    store.Store
	pub      events.Publisher
	authz    AuthzChecker
	provider provideradapter.Client
	log      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, provider provideradapter.Client, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, provider: provider, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/bnk06/attempts", func(r chi.Router) {
		r.Post("/", h.PrepareAttempt)
		r.Get("/{attemptID}", h.GetPaymentAttempt)
		r.Get("/{attemptID}/receipt", h.GetProviderReceipt)
		r.Get("/{attemptID}/state", h.GetSubmissionState)
		r.Get("/{attemptID}/evidence", h.GetAttemptEvidence)
		r.Post("/{attemptID}/submit", h.SubmitAttempt)
		r.Post("/{attemptID}/retry", h.RetrySameAttempt)
		r.Post("/{attemptID}/cancel", h.CancelBeforeSubmission)
		r.Post("/{attemptID}/resolve-ambiguous", h.ResolveAmbiguousSubmission)
		r.Post("/{attemptID}/quarantine", h.QuarantineAttempt)
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

func (h *Handler) fetchAttemptForAuth(w http.ResponseWriter, r *http.Request, attemptID string) (*domain.PaymentInitiationAttempt, bool) {
	a, err := h.store.FindAttempt(r.Context(), attemptID)
	if err != nil {
		if errors.Is(err, domain.ErrAttemptNotFound) {
			writeError(w, http.StatusNotFound, "payment initiation attempt not found")
			return nil, false
		}
		h.log.Error("fetchAttemptForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return a, true
}

// PrepareAttempt is the durable-record-before-network-call guarantee: this
// commits before SubmitAttempt is ever allowed to call the provider
// adapter. Idempotent on idempotency_key — a repeat call with the same key
// returns the existing attempt (200) rather than erroring or creating a
// second row, which is what actually prevents "provider timeout triggers
// new payment ID."
func (h *Handler) PrepareAttempt(w http.ResponseWriter, r *http.Request) {
	var req domain.PrepareAttemptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.PayerAccountRef == "" || req.PayeeRef == "" || req.Amount <= 0 || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, payer_account_ref, payee_ref, positive amount and currency are required")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, domain.ErrIdempotencyKeyRequired.Error())
		return
	}
	if !req.PayerAccountVerified {
		writeError(w, http.StatusBadRequest, domain.ErrPayerAccountNotVerified.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, PaymentInitiate) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	created, err := h.store.PrepareAttempt(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
			existing, findErr := h.store.FindByIdempotencyKey(r.Context(), req.IdempotencyKey)
			if findErr != nil {
				h.log.Error("PrepareAttempt: failed to fetch existing attempt for duplicate key", zap.Error(findErr))
				writeError(w, http.StatusServiceUnavailable, "store unavailable")
				return
			}
			writeJSON(w, http.StatusOK, existing)
			return
		}
		h.log.Error("PrepareAttempt: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventInitiationPrepared, EntityID: created.AttemptID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: created,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPaymentAttempt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) GetProviderReceipt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"attempt_id": attemptID, "provider_request_id": a.ProviderRequestID, "provider_response_ref": a.ProviderResponseRef,
	})
}

func (h *Handler) GetSubmissionState(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"attempt_id": attemptID, "status": a.Status})
}

func (h *Handler) GetAttemptEvidence(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	events, err := h.store.ListEvents(r.Context(), attemptID)
	if err != nil {
		h.log.Error("GetAttemptEvidence: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.AttemptEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"attempt_id": attemptID, "authorization_fingerprint": a.AuthorizationFingerprint,
		"idempotency_key": a.IdempotencyKey, "events": events,
	})
}

// SubmitAttempt is where the actual (stub) provider call happens — always
// against an already-durable PREPARED row, never before one exists. See
// internal/domain and internal/provideradapter's package docs for the full
// reasoning on what's real here versus the documented Provider Adapter
// boundary.
func (h *Handler) SubmitAttempt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	if !domain.CanSubmit(a.Status) {
		writeError(w, http.StatusConflict, "attempt is not in a submittable state")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentInitiate) {
		return
	}

	updated := h.submitToProvider(r.Context(), a, principalID)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) submitToProvider(ctx context.Context, a *domain.PaymentInitiationAttempt, principalID string) *domain.PaymentInitiationAttempt {
	result, err := h.provider.Submit(ctx, provideradapter.SubmitRequest{
		IdempotencyKey: a.IdempotencyKey, PayerAccountRef: a.PayerAccountRef, PayeeRef: a.PayeeRef,
		Amount: a.Amount, Currency: a.Currency, PaymentReference: a.PaymentReference,
	})
	if err != nil {
		h.log.Warn("provider adapter unreachable — treating as PENDING_UNKNOWN, never Rejected", zap.Error(err))
		updated, err := h.store.MarkPendingUnknown(ctx, a.AttemptID, principalID)
		if err != nil {
			h.log.Error("failed to record PENDING_UNKNOWN", zap.Error(err))
			return a
		}
		return updated
	}

	var updated *domain.PaymentInitiationAttempt
	switch result.Outcome {
	case provideradapter.OutcomeSubmitted:
		updated, err = h.store.MarkSubmitted(ctx, a.AttemptID, result.ProviderRequestID, result.ResponseRef, principalID)
	case provideradapter.OutcomeTimeout:
		updated, err = h.store.MarkPendingUnknown(ctx, a.AttemptID, principalID)
	case provideradapter.OutcomeRejected:
		updated, err = h.store.MarkRejected(ctx, a.AttemptID, result.RejectionReason, principalID)
	default:
		updated, err = h.store.MarkPendingUnknown(ctx, a.AttemptID, principalID)
	}
	if err != nil {
		h.log.Error("failed to record submission outcome", zap.Error(err))
		return a
	}
	return updated
}

// RetrySameAttempt reuses the SAME AttemptID/IdempotencyKey — there is no
// code path in this service that creates a second attempt for one payment
// intent, which is what makes "provider timeout triggers new payment ID"
// structurally impossible rather than merely discouraged.
func (h *Handler) RetrySameAttempt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	if !domain.CanRetry(a.Status) {
		writeError(w, http.StatusConflict, "attempt is not in a retryable (PENDING_UNKNOWN) state")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentInitiate) {
		return
	}

	updated := h.submitToProvider(r.Context(), a, principalID)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) CancelBeforeSubmission(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	if !domain.CanCancelBeforeSubmission(a.Status) {
		writeError(w, http.StatusConflict, "attempt has already been submitted and can no longer be cancelled")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentCancelPresubmit) {
		return
	}

	updated, err := h.store.CancelAttempt(r.Context(), attemptID, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "attempt has already been submitted and can no longer be cancelled")
			return
		}
		h.log.Error("CancelBeforeSubmission: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ResolveAmbiguousSubmission(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	var req domain.ResolveAmbiguousRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ResolvedStatus != domain.StatusSubmitted && req.ResolvedStatus != domain.StatusRejectedBeforeSubmission {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidResolution.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	if !domain.CanResolveAmbiguous(a.Status) {
		writeError(w, http.StatusConflict, "attempt is not PENDING_UNKNOWN")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAttemptResolve) {
		return
	}

	updated, err := h.store.ResolveAmbiguous(r.Context(), attemptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "attempt is not PENDING_UNKNOWN")
			return
		}
		h.log.Error("ResolveAmbiguousSubmission: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) QuarantineAttempt(w http.ResponseWriter, r *http.Request) {
	attemptID := chi.URLParam(r, "attemptID")
	var req domain.QuarantineRequest
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
	a, ok := h.fetchAttemptForAuth(w, r, attemptID)
	if !ok {
		return
	}
	if !domain.CanQuarantine(a.Status) {
		writeError(w, http.StatusConflict, "attempt is not in a quarantinable state")
		return
	}
	if !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAttemptResolve) {
		return
	}

	updated, err := h.store.QuarantineAttempt(r.Context(), attemptID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "attempt is not in a quarantinable state")
			return
		}
		h.log.Error("QuarantineAttempt: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
