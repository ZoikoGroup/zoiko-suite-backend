package handler

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/callbacks"
	"zoiko.io/payment-status-svc/internal/store"
)

// WithCallbacks enables the profile-driven provider callback endpoint and the receipt listing (INT-07).
func (h *Handler) WithCallbacks(p *callbacks.Processor, receipts store.CallbackStore) *Handler {
	h.callbacks = p
	h.receipts = receipts
	return h
}

// ProviderCallback is POST /bnk07/webhooks/provider-callback/{provider}/{tenantID}. It is outside the
// tenant-context group because it is machine-authenticated: the signature, verified with the secret of THAT
// tenant and provider, is the authentication.
func (h *Handler) ProviderCallback(w http.ResponseWriter, r *http.Request) {
	if h.callbacks == nil {
		writeError(w, http.StatusNotFound, "provider callbacks are not enabled on this service")
		return
	}
	res := h.callbacks.Handle(r.Context(), chi.URLParam(r, "provider"), chi.URLParam(r, "tenantID"), r.Header, http.MaxBytesReader(w, r.Body, 256<<10))
	if res.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, res.Status, res.Body)
}

// ListCallbackReceipts is GET /bnk07/callback-receipts?legal_entity_id=...[&payment_id=...][&unmatched=true][&limit=N].
func (h *Handler) ListCallbackReceipts(w http.ResponseWriter, r *http.Request) {
	if h.receipts == nil {
		writeError(w, http.StatusNotFound, "provider callbacks are not enabled on this service")
		return
	}
	entity := r.URL.Query().Get("legal_entity_id")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "legal_entity_id must be a UUID")
		return
	}
	paymentID := r.URL.Query().Get("payment_id")
	if paymentID != "" {
		if _, err := uuid.Parse(paymentID); err != nil {
			writeError(w, http.StatusBadRequest, "payment_id must be a UUID")
			return
		}
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !h.authorize(w, r, principalID, entity, PaymentStatusRead) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.receipts.ListReceipts(r.Context(), entity, r.URL.Query().Get("unmatched") == "true", paymentID, limit)
	if err != nil {
		h.log.Error("ListCallbackReceipts: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if list == nil {
		list = []store.Receipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": list})
}
