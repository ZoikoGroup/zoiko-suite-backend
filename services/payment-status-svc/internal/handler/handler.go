// Package handler exposes payment-status-svc's REST API — BNK-07.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-status-svc/internal/authz"
	"zoiko.io/payment-status-svc/internal/callbacks"
	"zoiko.io/payment-status-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-status-svc/internal/middleware"
	"zoiko.io/payment-status-svc/internal/store"
	"zoiko.io/payment-status-svc/internal/webhook"
)

// Action constants — BNK-07's own contract's "Permissions" line
// ("payment.status.read; payment.status.ingest; payment.status.resolve;
// payment.finality.confirm"), adapted to this platform's
// SCREAMING_SNAKE_CASE convention. ProcessProviderCallback needs none of
// these — it is machine-authenticated by HMAC signature, not an
// interactive principal (see internal/webhook's package doc).
const (
	PaymentStatusRead      = "PAYMENT_STATUS_READ"
	PaymentStatusIngest    = "PAYMENT_STATUS_INGEST"
	PaymentStatusResolve   = "PAYMENT_STATUS_RESOLVE"
	PaymentFinalityConfirm = "PAYMENT_FINALITY_CONFIRM"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store         store.Store
	authz         AuthzChecker
	webhookSecret []byte
	log           *zap.Logger
	// callbacks and receipts serve the profile-driven provider callback endpoint (INT-07); nil means it is off.
	callbacks *callbacks.Processor
	receipts  store.CallbackStore
}

func New(st store.Store, az AuthzChecker, webhookSecret string, log *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, webhookSecret: []byte(webhookSecret), log: log}
}

// RegisterRoutes registers every route directly on r. tenantMiddleware is
// applied only to the /bnk07/payments/* routes — the real webhook
// (/bnk07/webhooks/provider-callback) is machine-authenticated by HMAC
// signature, not by X-Tenant-Id/X-Principal-Id, so it deliberately sits
// outside that group.
func RegisterRoutes(r chi.Router, h *Handler, tenantMiddleware func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(tenantMiddleware)
		r.Route("/bnk07/payments", func(r chi.Router) {
			r.Post("/", h.RecordPaymentStatus)
			r.Get("/unresolved", h.ListUnresolvedPayments)
			r.Get("/{paymentID}", h.GetPaymentStatus)
			r.Get("/{paymentID}/history", h.GetStatusHistory)
			r.Get("/{paymentID}/finality-evidence", h.GetFinalityEvidence)
			r.Get("/{paymentID}/provider-reference", h.GetProviderReference)
			r.Post("/{paymentID}/link-statement", h.LinkStatementConfirmation)
			r.Post("/{paymentID}/resolve-conflict", h.ResolveStatusConflict)
			r.Post("/{paymentID}/return", h.RecordReturn)
			r.Post("/{paymentID}/cancel", h.CancelPaymentWhereSupported)
			r.Post("/{paymentID}/poll", h.PollPaymentStatus)
			r.Post("/{paymentID}/confirm-settlement", h.ConfirmSettlement)
			r.Post("/{paymentID}/record-rejection", h.RecordRejection)
		})
	})
	r.Post("/bnk07/webhooks/provider-callback", h.ProcessProviderCallback)
	r.Post("/bnk07/webhooks/provider-callback/{provider}/{tenantID}", h.ProviderCallback)
	r.Group(func(r chi.Router) {
		r.Use(tenantMiddleware)
		r.Get("/bnk07/callback-receipts", h.ListCallbackReceipts)
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

// requireIdempotencyKey is the command-idempotency contract for the two manual
// finality commands. Older BNK-07 commands dedupe on a natural provider/event
// reference; a manual finality decision has no provider event to use, so it
// carries the platform's explicit Idempotency-Key instead.
func (h *Handler) requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, domain.ErrIdempotencyKeyRequired.Error())
		return "", false
	}
	if len(key) > 200 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must be at most 200 characters")
		return "", false
	}
	return key, true
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

func (h *Handler) fetchPaymentForAuth(w http.ResponseWriter, r *http.Request, paymentID string) (*domain.PaymentExecutionState, bool) {
	p, err := h.store.FindPayment(r.Context(), paymentID)
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment execution state not found")
			return nil, false
		}
		h.log.Error("fetchPaymentForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return nil, false
	}
	return p, true
}

func (h *Handler) RecordPaymentStatus(w http.ResponseWriter, r *http.Request) {
	var req domain.RecordPaymentStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, PaymentStatusIngest) {
		return
	}

	verifiedTenant := svcmiddleware.TenantFromContext(r.Context())
	created, err := h.store.RecordPaymentStatus(r.Context(), verifiedTenant, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.log.Error("RecordPaymentStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPaymentStatus(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusRead) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) GetStatusHistory(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusRead) {
		return
	}
	events, err := h.store.ListEvents(r.Context(), paymentID)
	if err != nil {
		h.log.Error("GetStatusHistory: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if events == nil {
		events = []domain.StatusEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": events, "count": len(events)})
}

func (h *Handler) GetFinalityEvidence(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusRead) {
		return
	}
	evidence, err := h.store.ListFinalityEvidence(r.Context(), paymentID)
	if err != nil {
		h.log.Error("GetFinalityEvidence: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if evidence == nil {
		evidence = []domain.FinalityEvidence{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"payment_id": paymentID, "status": p.Status, "finality_source": p.FinalitySource,
		"mapping_version": p.MappingVersion, "evidence": evidence,
	})
}

func (h *Handler) GetProviderReference(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusRead) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payment_id": paymentID, "provider_request_id": p.ProviderRequestID})
}

// ListUnresolvedPayments is scoped to one legal entity: an unscoped list would
// cross both entities and, for a principal entitled in several, leak rows they
// are not entitled to see.
func (h *Handler) ListUnresolvedPayments(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if _, err := uuid.Parse(legalEntityID); err != nil {
		writeError(w, http.StatusBadRequest, "legal_entity_id must be a UUID")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, legalEntityID, PaymentStatusRead) {
		return
	}
	payments, err := h.store.ListUnresolved(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListUnresolvedPayments: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if payments == nil {
		payments = []domain.PaymentExecutionState{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": payments, "count": len(payments)})
}

// ProcessProviderCallback is BNK-07's real webhook receiver. Signature
// verification happens over the exact raw request body, before anything
// is parsed — the literal fix for negative-path #1 ("forged callback
// accepted"). See internal/webhook's package doc for why this, not
// X-Principal-Id, is this endpoint's only authentication.
func (h *Handler) ProcessProviderCallback(w http.ResponseWriter, r *http.Request) {
	// Fail closed: without a configured secret no signature can be verified, so
	// accepting anything would accept unsigned, unattributable callbacks. The
	// endpoint is off rather than trusting a development default.
	if len(h.webhookSecret) == 0 {
		h.log.Error("ProcessProviderCallback: no WEBHOOK_SHARED_SECRET configured — refusing every callback")
		writeError(w, http.StatusServiceUnavailable, "webhook receiver is not configured")
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	signature := r.Header.Get("X-Webhook-Signature")
	if !webhook.Verify(h.webhookSecret, rawBody, signature) {
		h.log.Warn("ProcessProviderCallback: invalid or missing signature — rejecting")
		writeError(w, http.StatusUnauthorized, domain.ErrInvalidSignature.Error())
		return
	}

	var payload domain.ProviderCallbackPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid callback payload")
		return
	}
	if payload.PaymentID == "" || payload.ProviderEventRef == "" {
		writeError(w, http.StatusBadRequest, "payment_id and provider_event_ref are required")
		return
	}
	if !domain.ValidCallbackStatus(payload.ReportedStatus) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidCallbackStatus.Error())
		return
	}

	updated, applied, err := h.store.ApplyCallbackStatus(r.Context(), payload.PaymentID, payload, eventTypeFor(payload.ReportedStatus), "PROVIDER_CALLBACK", "provider-callback", payload.ProviderEventRef, true)
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment execution state not found")
			return
		}
		if errors.Is(err, domain.ErrOpenConflictExists) {
			writeError(w, http.StatusConflict, domain.ErrOpenConflictExists.Error())
			return
		}
		h.log.Error("ProcessProviderCallback: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"payment": updated, "applied": applied})
}

func eventTypeFor(s domain.ExecutionStatus) string {
	switch s {
	case domain.StatusAccepted:
		return domain.EventPaymentAccepted
	case domain.StatusPending:
		return domain.EventPaymentPending
	case domain.StatusSettled:
		return domain.EventPaymentSettled
	case domain.StatusRejected:
		return domain.EventPaymentRejected
	default:
		return "PAYMENT_STATUS_UPDATED"
	}
}

// LinkStatementConfirmation is negative-path #4's real enforcement: a
// disagreement with the current canonical status raises a blocking
// conflict rather than silently overwriting either side.
func (h *Handler) LinkStatementConfirmation(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.LinkStatementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.StatementReference == "" {
		writeError(w, http.StatusBadRequest, "statement_reference is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusIngest) {
		return
	}

	updated, conflict, err := h.store.LinkStatement(r.Context(), paymentID, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.log.Error("LinkStatementConfirmation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"payment": updated, "conflict_raised": conflict})
}

// ResolveStatusConflict requires the stronger PAYMENT_FINALITY_CONFIRM
// action — the spec's own SoD line ("manual finality override requires
// exceptional controlled workflow and evidence").
func (h *Handler) ResolveStatusConflict(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.ResolveConflictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if !domain.ValidResolutionStatus(req.FinalStatus) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidResolution.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
		return
	}
	if !p.HasOpenConflict {
		writeError(w, http.StatusConflict, "payment has no open conflict to resolve")
		return
	}
	if p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		return
	}

	updated, err := h.store.ResolveConflict(r.Context(), paymentID, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidResolution) {
			writeError(w, http.StatusBadRequest, domain.ErrInvalidResolution.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "payment has no open conflict to resolve")
			return
		}
		h.log.Error("ResolveStatusConflict: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// RecordReturn is the ONLY path that may ever move a SETTLED payment
// further — the explicit "return/reversal semantics" the spec's state
// model names as the sole exception to "governed final states cannot
// regress."
func (h *Handler) RecordReturn(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.RecordReturnRequest
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
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
		return
	}
	if !domain.CanReturn(p.Status) {
		writeError(w, http.StatusConflict, "only a SETTLED payment may be returned")
		return
	}
	if p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		return
	}

	updated, err := h.store.RecordReturn(r.Context(), paymentID, req, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "only a SETTLED payment may be returned, and each return event may only be applied once")
			return
		}
		h.log.Error("RecordReturn: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) CancelPaymentWhereSupported(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.CancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusResolve) {
		return
	}
	if !domain.CanCancel(p.Status) {
		writeError(w, http.StatusConflict, "payment can no longer be cancelled")
		return
	}

	updated, err := h.store.CancelPayment(r.Context(), paymentID, req, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			writeError(w, http.StatusConflict, "payment can no longer be cancelled")
			return
		}
		h.log.Error("CancelPaymentWhereSupported: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// PollPaymentStatus has no real provider to poll — see internal/domain's
// package doc. It is a caller-attested command (the same doctrine as
// AP-11's ReconcilePaymentRunStatus), sharing the exact same apply-status
// logic ProcessProviderCallback uses, just tagged with a different
// finality_source and requiring an interactive, authorized principal
// instead of a webhook signature.
func (h *Handler) PollPaymentStatus(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var payload domain.ProviderCallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !domain.ValidCallbackStatus(payload.ReportedStatus) {
		writeError(w, http.StatusBadRequest, domain.ErrInvalidCallbackStatus.Error())
		return
	}
	payload.PaymentID = paymentID

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	// A poll asserting a governed final status is a manual finality claim:
	// it needs the stronger payment.finality.confirm action, not merely
	// ingest. Intermediate progress reports stay on the ingest action.
	action := PaymentStatusIngest
	if domain.IsFinal(payload.ReportedStatus) {
		action = PaymentFinalityConfirm
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, action) {
		return
	}
	// SoD: the same actor who created this payment must not also be the
	// one manually asserting it into a governed final state (SETTLED/
	// REJECTED). ConfirmSettlement/RecordRejection are now the first-class
	// evidence-bearing paths, but this compatibility poll can still assert
	// a final status, so it keeps the same creator-different rule.
	// Intermediate progress reports (ACCEPTED/PENDING) are not a finality
	// claim and remain unrestricted — same scoping as
	// ResolveStatusConflict/RecordReturn, which only gate the commands that
	// actually assert/change finality.
	if domain.IsFinal(payload.ReportedStatus) && p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		return
	}

	// emitEvent=false: PollPaymentStatus never published an event in the
	// old dual-write (see ApplyCallbackStatus's own doc comment) — this
	// fix must not silently start emitting events for a path that never
	// did, which would be a scope expansion past the dual-write bug.
	updated, applied, err := h.store.ApplyCallbackStatus(r.Context(), paymentID, payload, eventTypeFor(payload.ReportedStatus), "MANUAL_POLL", principalID, "", false)
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment execution state not found")
			return
		}
		if errors.Is(err, domain.ErrOpenConflictExists) {
			writeError(w, http.StatusConflict, domain.ErrOpenConflictExists.Error())
			return
		}
		h.log.Error("PollPaymentStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payment": updated, "applied": applied})
}

// ConfirmSettlement is BNK-07's first-class, evidence-bearing manual settlement
// command. Unlike PollPaymentStatus it does not accept a bare asserted status:
// it requires a provider/bank reference, a reason, an effective date and an
// evidence reference (optionally a SHA-256 evidence hash). It always requires
// PAYMENT_FINALITY_CONFIRM and a creator-different principal, and emits
// PaymentSettled through the outbox in the same transaction as the state
// change.
func (h *Handler) ConfirmSettlement(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.ConfirmSettlementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
		return
	}
	if p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		return
	}

	updated, evidence, replay, err := h.store.ConfirmSettlement(r.Context(), paymentID, req, principalID, idempotencyKey, r.Header.Get("X-Correlation-ID"))
	h.writeFinalityResult(w, "ConfirmSettlement", updated, evidence, replay, err)
}

// RecordRejection is the rejection counterpart to ConfirmSettlement. It uses
// the same evidence and SoD rules and emits PaymentRejected through the outbox.
func (h *Handler) RecordRejection(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	var req domain.RecordRejectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
		return
	}
	if p.CreatedByPrincipalID != "" && p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		return
	}

	updated, evidence, replay, err := h.store.RecordRejection(r.Context(), paymentID, req, principalID, idempotencyKey, r.Header.Get("X-Correlation-ID"))
	h.writeFinalityResult(w, "RecordRejection", updated, evidence, replay, err)
}

func (h *Handler) writeFinalityResult(w http.ResponseWriter, operation string, updated *domain.PaymentExecutionState, evidence *domain.FinalityEvidence, replay bool, err error) {
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPaymentNotFound):
			writeError(w, http.StatusNotFound, "payment execution state not found")
		case errors.Is(err, domain.ErrSelfResolutionForbidden):
			writeError(w, http.StatusForbidden, domain.ErrSelfResolutionForbidden.Error())
		case errors.Is(err, domain.ErrOpenConflictExists):
			writeError(w, http.StatusConflict, domain.ErrOpenConflictExists.Error())
		case errors.Is(err, domain.ErrInvalidTransition):
			writeError(w, http.StatusConflict, "payment is already in a governed final state or cannot accept this finality decision")
		case errors.Is(err, domain.ErrIdempotencyKeyConflict):
			writeError(w, http.StatusConflict, domain.ErrIdempotencyKeyConflict.Error())
		case errors.Is(err, domain.ErrInvalidFinalityRequest):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			h.log.Error(operation+": store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"payment":  updated,
		"evidence": evidence,
		"replayed": replay,
	})
}
