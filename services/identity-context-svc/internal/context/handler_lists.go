package context

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
	"zoiko.io/identity-envelope/apicontract"
)

// ConsoleReader backs the console read models (IC-2): the registers behind the
// People, Sessions, Support access and Sign-in events tabs, the assurance
// history in the People drawer, and the KPI summary. Implemented by
// store.PgStore.
type ConsoleReader interface {
	ListPrincipalsPage(ctx context.Context, tenantID string, f store.PrincipalListFilter, offset, limit int) ([]store.PrincipalDetail, error)
	ListSessionsPage(ctx context.Context, tenantID string, f store.SessionListFilter, offset, limit int) ([]store.SessionSummary, error)
	ListSupportContextsPage(ctx context.Context, tenantID, state string, offset, limit int) ([]domain.SupportContext, error)
	ListAuthenticationEventsPage(ctx context.Context, tenantID string, f store.AuthEventFilter, offset, limit int) ([]domain.AuthenticationEvent, error)
	ListAssuranceHistoryPage(ctx context.Context, tenantID, principalID string, offset, limit int) ([]store.AssuranceChange, error)
	ListPasswordResetsPage(ctx context.Context, tenantID string, f store.ResetListFilter, offset, limit int) ([]domain.PasswordResetRequest, error)
	GetIdentitySummary(ctx context.Context, tenantID string) (*store.IdentitySummary, error)
}

// WithConsole wires the console read models. cursors signs every next_cursor
// and binds it to the tenant, the route and the filter that produced it, so a
// cursor replayed by another tenant or against another query is refused.
func (h *Handler) WithConsole(r ConsoleReader, cursors *apicontract.CursorSigner) *Handler {
	h.console, h.cursors = r, cursors
	return h
}

// listResponse is the shape of every console list (API-4).
type listResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	PageSize   int    `json:"page_size"`
}

// listRequest is a parsed, cursor-verified list query.
type listRequest struct {
	page      apicontract.Page
	tenantID  string
	route     string
	filterKey string
	offset    int
}

// beginList runs the common prelude of every console list: tenant, caller,
// throttle, authorization at action, then filter and cursor validation.
// extraScope is folded into the cursor's filter binding for query parameters
// that are not filter[...] (the People search box).
func (h *Handler) beginList(w http.ResponseWriter, r *http.Request, op, action string,
	allowedFilters []string, extraScope string) (*listRequest, bool) {
	if h.console == nil || h.cursors == nil {
		writeCoded(w, r, http.StatusNotImplemented, "not_configured", "console read models are not configured in this deployment")
		return nil, false
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	if _, ok := h.throttle(w, r, op, tenantID); !ok {
		return nil, false
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, action) {
		return nil, false
	}
	page, prob := apicontract.ParsePage(r, allowedFilters, nil, "")
	if prob != nil {
		apicontract.WriteProblem(w, r, *prob)
		return nil, false
	}
	lr := &listRequest{
		page:      page,
		tenantID:  tenantID,
		route:     apicontract.ScopedRoute(r.URL.Path),
		filterKey: page.FilterKey() + "|" + extraScope,
	}
	if page.Cursor != "" {
		c, err := h.cursors.Verify(page.Cursor, tenantID, lr.route, lr.filterKey, page.SortKey())
		if err != nil {
			writeCoded(w, r, http.StatusBadRequest, "invalid_cursor",
				"the cursor is malformed, was tampered with, or belongs to another tenant or query")
			return nil, false
		}
		lr.offset = c.Offset
	}
	return lr, true
}

// finishList trims the look-ahead row, signs the next cursor and writes the page.
func finishList[T any](h *Handler, w http.ResponseWriter, r *http.Request, lr *listRequest, items []T) {
	resp := listResponse[T]{Items: items, PageSize: lr.page.PageSize}
	if resp.Items == nil {
		resp.Items = []T{}
	}
	if len(resp.Items) > lr.page.PageSize {
		resp.Items = resp.Items[:lr.page.PageSize]
		next, err := h.cursors.Sign(apicontract.Cursor{TenantID: lr.tenantID, Route: lr.route,
			Filter: lr.filterKey, Sort: lr.page.SortKey(), Offset: lr.offset + lr.page.PageSize})
		if err != nil {
			h.log.Error("sign next cursor failed", zap.Error(err))
			writeError(w, r, http.StatusInternalServerError, "list paging failed")
			return
		}
		resp.NextCursor = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) listFailed(w http.ResponseWriter, r *http.Request, what, tenantID string, err error) {
	if errors.Is(err, store.ErrInvalidFilter) {
		writeCoded(w, r, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	h.log.Error(what+" failed", zap.String("tenant_id", tenantID), zap.Error(err))
	writeError(w, r, http.StatusInternalServerError, what+" failed")
}

// ListPrincipals is GET /v1/principals: the People register.
func (h *Handler) ListPrincipals(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	lr, ok := h.beginList(w, r, "list_principals", IdentityPrincipalRead,
		[]string{"status", "principal_type"}, "q="+q)
	if !ok {
		return
	}
	f := store.PrincipalListFilter{Status: lr.page.Filters["status"], PrincipalType: lr.page.Filters["principal_type"], Query: q}
	if f.Status != "" && !oneOf(f.Status, "PENDING_ACTIVATION", "ACTIVE", "SUSPENDED", "DISABLED") {
		writeCoded(w, r, http.StatusBadRequest, "invalid_filter", "filter[status] must be PENDING_ACTIVATION, ACTIVE, SUSPENDED or DISABLED")
		return
	}
	if f.PrincipalType != "" && !oneOf(f.PrincipalType, "HUMAN", "SERVICE_ACCOUNT", "API_CLIENT") {
		writeCoded(w, r, http.StatusBadRequest, "invalid_filter", "filter[principal_type] must be HUMAN, SERVICE_ACCOUNT or API_CLIENT")
		return
	}
	items, err := h.console.ListPrincipalsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "principals list", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListSessions is GET /v1/context/sessions: the Sessions register.
func (h *Handler) ListSessions(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, "list_sessions", IdentitySessionRead, []string{"principal_id", "status"}, "")
	if !ok {
		return
	}
	f := store.SessionListFilter{PrincipalID: lr.page.Filters["principal_id"], State: lr.page.Filters["status"]}
	items, err := h.console.ListSessionsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "sessions list", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListSupportContexts is GET /v1/context/support: the Support access register.
func (h *Handler) ListSupportContexts(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, "list_support_contexts", ActionReviewSupportContext, []string{"status"}, "")
	if !ok {
		return
	}
	items, err := h.console.ListSupportContextsPage(r.Context(), lr.tenantID, lr.page.Filters["status"], lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "support contexts list", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListAuthenticationEvents is GET /v1/authentication-events (ORG-1
// ListAuthenticationEvents): the Sign-in events register.
func (h *Handler) ListAuthenticationEvents(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, "list_authentication_events", IdentityAuthEventsRead,
		[]string{"principal_id", "outcome", "since"}, "")
	if !ok {
		return
	}
	f := store.AuthEventFilter{PrincipalID: lr.page.Filters["principal_id"], Outcome: lr.page.Filters["outcome"]}
	if f.Outcome != "" && !oneOf(f.Outcome, "SUCCESS", "FAILURE") {
		writeCoded(w, r, http.StatusBadRequest, "invalid_filter", "filter[outcome] must be SUCCESS or FAILURE")
		return
	}
	if raw := lr.page.Filters["since"]; raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeCoded(w, r, http.StatusBadRequest, "invalid_filter", "filter[since] must be an RFC 3339 timestamp")
			return
		}
		f.Since = &since
	}
	items, err := h.console.ListAuthenticationEventsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "authentication events list", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// GetAssuranceHistory is GET /v1/principals/{id}/assurance-history (ORG-1
// GetAssuranceHistory).
func (h *Handler) GetAssuranceHistory(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, "assurance_history", IdentityPrincipalRead, nil, "")
	if !ok {
		return
	}
	items, err := h.console.ListAssuranceHistoryPage(r.Context(), lr.tenantID, chi.URLParam(r, "principalID"), lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "assurance history", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListPasswordResets is GET /v1/password-resets: the Password resets
// register. Each row names its requester and approvers, so the console can
// keep the approve and reject buttons from the person who asked.
func (h *Handler) ListPasswordResets(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, "list_password_resets", IdentityPrincipalRead, []string{"status", "principal_id"}, "")
	if !ok {
		return
	}
	f := store.ResetListFilter{Status: lr.page.Filters["status"], PrincipalID: lr.page.Filters["principal_id"]}
	if f.Status != "" && !oneOf(f.Status, "PENDING_APPROVAL", "ISSUED", "REJECTED", "COMPLETED", "EXPIRED") {
		writeCoded(w, r, http.StatusBadRequest, "invalid_filter",
			"filter[status] must be PENDING_APPROVAL, ISSUED, REJECTED, COMPLETED or EXPIRED")
		return
	}
	items, err := h.console.ListPasswordResetsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "password resets list", lr.tenantID, err)
		return
	}
	finishList(h, w, r, lr, items)
}

// GetIdentitySummary is GET /v1/identity/summary: the console KPI counts.
func (h *Handler) GetIdentitySummary(w http.ResponseWriter, r *http.Request) {
	if h.console == nil {
		writeCoded(w, r, http.StatusNotImplemented, "not_configured", "console read models are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "identity_summary", tenantID); !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalRead) {
		return
	}
	sum, err := h.console.GetIdentitySummary(r.Context(), tenantID)
	if err != nil {
		h.listFailed(w, r, "identity summary", tenantID, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, sum)
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
