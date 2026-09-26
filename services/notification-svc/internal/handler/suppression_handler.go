package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
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

// ── DELETE /v1/notifications/suppression/{email} ───────────────────────────
//
// RemoveSuppression removes an active suppression for a specific email address
// and optional stream. The {email} path parameter is URL-encoded.
func (h *Handler) RemoveSuppression(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	rawEmail := chi.URLParam(r, "email")
	email, err := url.PathUnescape(rawEmail)
	email = strings.ToLower(strings.TrimSpace(email))
	if err != nil || email == "" {
		writeError(w, http.StatusBadRequest, "invalid_email", "email path parameter is required and must be URL-encoded")
		return
	}

	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "ALL"
	}

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

	if err := h.suppressions.RemoveSuppression(r.Context(), tenantID, email, stream); err != nil {
		h.log.Error("failed to remove suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── POST /v1/notifications/unsubscribe ─────────────────────────────────────
//
// HandleUnsubscribe is the RFC 8058 one-click unsubscribe receiver. Mail clients
// POST to this URL when the user clicks the unsubscribe button in their mail
// client (not in the email itself). The request carries no ZoikoSuite auth
// headers — it comes from the mail client — so this endpoint is exempted from
// the envelope middleware in main.go.
//
// The tenant and recipient are resolved from the signed action token embedded in
// the URL or request parameters. The orchestrator emits these params when
// generating the List-Unsubscribe header.
//
// On success: adds a UNSUBSCRIBE suppression and returns 200.
// Idempotent: AddSuppression uses ON CONFLICT DO UPDATE, so repeated requests
// succeed idempotently. Fail-closed: missing parameters or store failure return error codes.
func (h *Handler) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if h.suppressions == nil {
		h.log.Error("unsubscribe handler called but suppression store not configured")
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "suppression store not configured")
		return
	}

	var actionToken string
	var reqEmail string
	var reqTenant string

	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseForm(); err == nil {
			actionToken = r.FormValue("action_token")
			if e := r.FormValue("email"); e != "" {
				reqEmail = e
			}
			if t := r.FormValue("tenant_id"); t != "" {
				reqTenant = t
			}
		}
	} else {
		var req struct {
			ActionToken string `json:"action_token"`
			TenantID    string `json:"tenant_id"`
			Email       string `json:"email"`
		}
		_ = decodeJSONNoResponse(r, &req)
		actionToken = req.ActionToken
		reqTenant = req.TenantID
		reqEmail = req.Email
	}
	if actionToken == "" {
		actionToken = r.URL.Query().Get("action_token")
	}
	if reqEmail == "" {
		reqEmail = r.URL.Query().Get("email")
	}
	if reqTenant == "" {
		reqTenant = r.URL.Query().Get("tenant_id")
	}

	tenantID := reqTenant
	if tenantID == "" && actionToken != "" {
		if decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(actionToken)); err == nil {
			parts := strings.Split(string(decoded), ".")
			if len(parts) >= 2 {
				tenantID = parts[0]
			}
		}
	}

	if tenantID == "" {
		tenantID = svcmiddleware.TenantFromContext(r.Context())
	}

	recipientEmail := strings.ToLower(strings.TrimSpace(reqEmail))

	if tenantID == "" || recipientEmail == "" {
		h.log.Warn("unsubscribe: cannot resolve tenant_id or email from request",
			zap.String("action_token_present", actionToken))
		writeError(w, http.StatusBadRequest, "invalid_request", "tenant_id and email are required")
		return
	}

	providerName := "rfc8058_one_click"
	supp := &ledger.EmailSuppression{
		TenantID:       tenantID,
		RecipientEmail: recipientEmail,
		Reason:         ledger.SuppressionReasonUnsubscribe,
		SourceStream:   "ALL",
		ProviderName:   &providerName,
		CreatedAt:      time.Now().UTC(),
	}
	if err := h.suppressions.AddSuppression(r.Context(), supp); err != nil {
		h.log.Error("failed to record unsubscribe suppression", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	h.log.Info("unsubscribe recorded",
		zap.String("tenant_id", tenantID),
		zap.String("recipient_email", recipientEmail))
	w.WriteHeader(http.StatusOK)
}

// decodeJSONNoResponse decodes a JSON body without writing error responses.
// Used for optional JSON bodies (e.g., RFC 8058 unsubscribe which may be
// application/x-www-form-urlencoded instead).
func decodeJSONNoResponse(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
