package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/provider"
	"zoiko.io/esignature-integration-svc/internal/store"
)

const (
	ENVELOPE_CREATE        = "ENVELOPE_CREATE"
	ENVELOPE_STATUS_UPDATE = "ENVELOPE_STATUS_UPDATE"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
	// providers holds the real e-signature adapters. nil or empty means no
	// provider is configured, and every provider-asserted operation fails
	// closed with 503 (see internal/provider).
	providers *provider.Registry
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

// WithProviders sets the provider registry. Without it the handler is fail-closed.
func (h *Handler) WithProviders(reg *provider.Registry) *Handler {
	h.providers = reg
	return h
}

// providerAsserted lists the envelope statuses only a provider can make true.
// VOIDED is the one status this service may decide on its own.
var providerAsserted = map[string]bool{
	domain.EnvelopeSent:      true,
	domain.EnvelopeDelivered: true,
	domain.EnvelopeSigned:    true,
}

var settableStatus = map[string]bool{
	domain.EnvelopeSent:      true,
	domain.EnvelopeDelivered: true,
	domain.EnvelopeSigned:    true,
	domain.EnvelopeVoided:    true,
}

func writeProviderNotConfigured(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error":   "esignature_provider_not_configured",
		"message": "no e-signature provider adapter is configured for this deployment; nothing was sent and no provider state was recorded",
	})
}

func RegisterRoutes(r chi.Router, h *Handler) {
	// Flat patterns, not r.Route sub-routers: RegisterOrchestrationRoutes adds
	// more /v1/esignature/envelopes/{id}/... routes, and two chi sub-routers
	// mounted on overlapping prefixes shadow each other (the more specific
	// mount 404'd GET /envelopes/{id} and POST /envelopes/{id}/status).
	r.Post("/v1/esignature/envelopes", h.CreateEnvelope)
	r.Get("/v1/esignature/envelopes", h.ListEnvelopes)
	r.Get("/v1/esignature/envelopes/{id}", h.GetByID)
	r.Post("/v1/esignature/envelopes/{id}/status", h.UpdateStatus)
	r.Patch("/v1/esignature/envelopes/{id}/status", h.UpdateStatus)
}

func (h *Handler) CreateEnvelope(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateEnvelopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.DocumentTitle == "" || req.SignerEmail == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, document_title, and signer_email are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, ENVELOPE_CREATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Resolve the provider BEFORE anything is persisted: with none configured
	// the request is refused outright instead of recording an envelope as
	// SENT that no provider ever received (the previous behaviour).
	adapter, err := h.providers.Resolve(req.Provider)
	if err != nil {
		writeProviderNotConfigured(w)
		return
	}

	// Durable record first (PENDING), then the provider call, then SENT with
	// the provider's own reference (ZS-SVC-S-001 §7.5: store the provider
	// transaction id before assuming success).
	env := &domain.SignatureEnvelope{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Provider:      adapter.Name(),
		DocumentTitle: req.DocumentTitle,
		SignerEmail:   req.SignerEmail,
		SignerName:    req.SignerName,
		Status:        domain.EnvelopePending,
	}

	if err := h.store.CreateEnvelope(r.Context(), env); err != nil {
		h.logger.Error("failed to create envelope", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create envelope")
		return
	}

	res, err := adapter.Send(r.Context(), provider.SendRequest{
		TenantID: tenantID, EnvelopeID: env.EnvelopeID, DocumentTitle: env.DocumentTitle,
		SignerEmail: env.SignerEmail, SignerName: env.SignerName,
	})
	if err != nil || res.ExternalRef == "" {
		// The envelope stays PENDING; the outcome at the provider is not
		// claimed either way.
		h.logger.Error("provider send failed", zap.String("envelope_id", env.EnvelopeID), zap.Error(err))
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"error": "provider_send_failed", "envelope_id": env.EnvelopeID, "status": env.Status,
		})
		return
	}
	sent, err := h.store.UpdateEnvelopeStatus(r.Context(), env.EnvelopeID, &domain.UpdateStatusRequest{
		Status: domain.EnvelopeSent, ExternalRef: res.ExternalRef,
	})
	if err != nil {
		h.logger.Error("failed to record provider reference", zap.String("envelope_id", env.EnvelopeID), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "envelope sent but provider reference could not be recorded")
		return
	}
	env = sent

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "esignature.envelope.sent", AggregateID: env.EnvelopeID, TenantID: tenantID,
		LegalEntityID: env.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: env,
	})
	writeJSON(w, http.StatusCreated, env)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	env, err := h.store.GetEnvelopeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get envelope")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (h *Handler) ListEnvelopes(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	envelopes, err := h.store.ListEnvelopes(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list envelopes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"envelopes": envelopes, "total": len(envelopes)})
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	if !settableStatus[req.Status] {
		writeError(w, http.StatusBadRequest, "status must be one of SENT, DELIVERED, SIGNED, VOIDED")
		return
	}

	existing, err := h.store.GetEnvelopeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get envelope")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, ENVELOPE_STATUS_UPDATE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// SENT/DELIVERED/SIGNED are facts about the provider. With no adapter
	// for this envelope's provider there is nothing that could have produced
	// them, so recording one would be fabricated.
	if providerAsserted[req.Status] {
		if _, err := h.providers.Resolve(existing.Provider); err != nil {
			writeProviderNotConfigured(w)
			return
		}
	}

	env, err := h.store.UpdateEnvelopeStatus(r.Context(), id, &req)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return
		}
		if errors.Is(err, domain.ErrInvalidEnvelopeTransition) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update envelope status")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "esignature.envelope." + req.Status, AggregateID: id, TenantID: tenantID,
		LegalEntityID: env.LegalEntityID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: env,
	})
	writeJSON(w, http.StatusOK, env)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}
