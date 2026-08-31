// Package handler exposes payment-status-svc's REST API — BNK-07.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-status-svc/internal/authz"
	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/events"
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
	pub           events.Publisher
	authz         AuthzChecker
	webhookSecret []byte
	log           *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, webhookSecret string, log *zap.Logger) *Handler {
	return &Handler{store: st, pub: pub, authz: az, webhookSecret: []byte(webhookSecret), log: log}
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
		})
	})
	r.Post("/bnk07/webhooks/provider-callback", h.ProcessProviderCallback)
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
	created, err := h.store.RecordPaymentStatus(r.Context(), verifiedTenant, req, principalID)
	if err != nil {
		h.log.Error("RecordPaymentStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: "PAYMENT_EXECUTION_STATE_RECORDED", EntityID: created.PaymentID, TenantID: verifiedTenant,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: created,
	})
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) GetPaymentStatus(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) GetStatusHistory(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	if _, ok := h.fetchPaymentForAuth(w, r, paymentID); !ok {
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
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"payment_id": paymentID, "status": p.Status, "finality_source": p.FinalitySource, "mapping_version": p.MappingVersion,
	})
}

func (h *Handler) GetProviderReference(w http.ResponseWriter, r *http.Request) {
	paymentID := chi.URLParam(r, "paymentID")
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payment_id": paymentID, "provider_request_id": p.ProviderRequestID})
}

func (h *Handler) ListUnresolvedPayments(w http.ResponseWriter, r *http.Request) {
	payments, err := h.store.ListUnresolved(r.Context())
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

	updated, applied, err := h.store.ApplyCallbackStatus(r.Context(), payload.PaymentID, payload, eventTypeFor(payload.ReportedStatus), "PROVIDER_CALLBACK", "provider-callback")
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment execution state not found")
			return
		}
		h.log.Error("ProcessProviderCallback: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	if applied {
		_ = h.pub.Publish(r.Context(), events.PublishParams{
			EventType: eventTypeFor(payload.ReportedStatus), EntityID: updated.PaymentID,
			CorrelationID: payload.ProviderEventRef, Payload: updated,
		})
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

	updated, conflict, err := h.store.LinkStatement(r.Context(), paymentID, req, principalID)
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

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	p, ok := h.fetchPaymentForAuth(w, r, paymentID)
	if !ok {
		return
	}
	if !p.HasOpenConflict {
		writeError(w, http.StatusConflict, "payment has no open conflict to resolve")
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
		return
	}

	updated, err := h.store.ResolveConflict(r.Context(), paymentID, req, principalID)
	if err != nil {
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
	if !domain.CanReturn(p.Status) {
		writeError(w, http.StatusConflict, "only a SETTLED payment may be returned")
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentFinalityConfirm) {
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

	_ = h.pub.Publish(r.Context(), events.PublishParams{
		EventType: domain.EventPaymentReturned, EntityID: updated.PaymentID, ActorID: principalID,
		CorrelationID: req.ProviderEventRef, Payload: updated,
	})
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
	if !domain.CanCancel(p.Status) {
		writeError(w, http.StatusConflict, "payment can no longer be cancelled")
		return
	}
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusResolve) {
		return
	}

	updated, err := h.store.CancelPayment(r.Context(), paymentID, req, principalID)
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
	if !h.authorize(w, r, principalID, p.LegalEntityID, PaymentStatusIngest) {
		return
	}

	updated, applied, err := h.store.ApplyCallbackStatus(r.Context(), paymentID, payload, eventTypeFor(payload.ReportedStatus), "MANUAL_POLL", principalID)
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			writeError(w, http.StatusNotFound, "payment execution state not found")
			return
		}
		h.log.Error("PollPaymentStatus: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"payment": updated, "applied": applied})
}
