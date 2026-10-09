package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
)

// ── POST /v1/notifications/suppression ─────────────────────────────────────
//
// AddSuppression adds or updates an email suppression entry for the tenant.
// Idempotent on (tenant_id, recipient_email, source_stream).
//
// Only principals holding NOTIFICATION_SUPPRESS on the tenant's default legal
// entity may write suppressions. The suppression list is a platform-level
// safety control (bounce/complaint/admin). Keeping it behind a privileged
// action prevents a regular NOTIFICATION_SEND caller from inadvertently or
// maliciously adding suppressions they should not control.
func (h *Handler) AddSuppression(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req struct {
		RecipientEmail string `json:"recipient_email"`
		Reason         string `json:"reason"`
		SourceStream   string `json:"source_stream,omitempty"`
		ProviderName   string `json:"provider_name,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.RecipientEmail = strings.ToLower(strings.TrimSpace(req.RecipientEmail))
	if req.RecipientEmail == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "recipient_email and reason are required")
		return
	}

	// A suppression entry has no legal_entity_id of its own — it is a tenant-
	// level safety record. We use the tenant ID as the resource target for the
	// authz check. The tenant scope is already enforced by RLS; this gate is
	// about which principal can create records in it.
	if h.authz != nil {
		if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionSuppressionManage); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	if h.suppressions == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "suppression store not configured")
		return
	}

	supp := &ledger.EmailSuppression{
		TenantID:       tenantID,
		RecipientEmail: req.RecipientEmail,
		Reason:         ledger.SuppressionReason(req.Reason),
		SourceStream:   req.SourceStream,
		CreatedAt:      time.Now().UTC(),
	}
	if req.ProviderName != "" {
		supp.ProviderName = &req.ProviderName
	}
	if err := h.suppressions.AddSuppression(r.Context(), supp); err != nil {
		h.log.Error("failed to add suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, supp)
}

// ── GET /v1/notifications/suppression ──────────────────────────────────────
//
// ListSuppressions returns the current suppression list for the caller's
// tenant, paginated, newest-first.
func (h *Handler) ListSuppressions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if h.authz != nil {
		if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionSuppressionManage); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}

	if h.suppressions == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "suppression store not configured")
		return
	}

	list, err := h.suppressions.ListSuppressions(r.Context(), tenantID, limit, offset)
	if err != nil {
		h.log.Error("failed to list suppressions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []*ledger.EmailSuppression{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/notifications/unsubscribe ─────────────────────────────────────
//
// HandleUnsubscribe is the RFC 8058 one-click unsubscribe receiver. Mail clients
// POST to the List-Unsubscribe URL with no ZoikoSuite headers, so the route is
// exempt from the envelope (main.go) and the token in the URL is the ONLY
// credential: it is opened by internal/unsubscribe and names the tenant and
// address. Nothing the request says outside the token is believed — a
// tenant_id or email parameter is ignored, because accepting them is exactly
// how one anonymous POST used to suppress any address in any tenant (and,
// through the upsert, rewrite a recorded hard bounce as an unsubscribe).
//
// A refused token is 403 and writes nothing. Not configured is 503: the
// service does not send marketing mail it cannot honour an unsubscribe for,
// so no valid link can exist. Repeating a valid request is harmless — the
// suppression store never weakens a stronger reason and an unsubscribe is
// already the weakest.
func (h *Handler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if h.suppressions == nil || h.unsubscribe == nil {
		h.log.Error("unsubscribe receiver called but unsubscribe is not configured")
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "unsubscribe is not configured")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data") {
			if err := r.ParseForm(); err == nil {
				token = r.PostFormValue("token")
			}
		} else {
			var req struct {
				Token string `json:"token"`
			}
			_ = decodeJSONNoResponse(r, &req)
			token = req.Token
		}
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "an unsubscribe token is required")
		return
	}

	claims, err := h.unsubscribe.Open(token)
	if err != nil {
		h.log.Warn("unsubscribe refused: token does not verify", zap.String("remote", r.RemoteAddr))
		writeError(w, http.StatusForbidden, "invalid_token", "the unsubscribe link is not valid")
		return
	}

	providerName := "rfc8058_one_click"
	supp := &ledger.EmailSuppression{
		TenantID:       claims.TenantID,
		RecipientEmail: claims.Email,
		Reason:         ledger.SuppressionReasonUnsubscribe,
		SourceStream:   "ALL",
		ProviderName:   &providerName,
		CreatedAt:      time.Now().UTC(),
	}
	if err := h.suppressions.AddSuppression(r.Context(), supp); err != nil {
		h.log.Error("failed to record unsubscribe suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "the unsubscribe could not be recorded; try again")
		return
	}

	// The address is not logged (§13.3): the tenant is enough to find the row.
	h.log.Info("unsubscribe recorded", zap.String("tenant_id", claims.TenantID))
	w.WriteHeader(http.StatusOK)
}

func decodeJSONNoResponse(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
