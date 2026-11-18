package handler

// Console read models (ER-3): the registers behind the /admin/evidence tabs
// and the KPI summary, paged per API-4 (identity-envelope/apicontract):
//
//   - filters are filter[name]=value; an unknown filter is 400 unknown_filter;
//   - page_size defaults to 50, at most 200;
//   - next_cursor is an HMAC-signed token bound to the tenant, the route and
//     the filter set that produced it, so a cursor that was tampered with, or
//     is replayed by another tenant or against another query, is 400
//     invalid_cursor rather than a page of someone else's rows.
//
// Same shape as identity-context-svc's handler_lists.go.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"zoiko.io/identity-envelope/apicontract"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// listResponse is the shape of every console list (API-4).
type listResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	PageSize   int    `json:"page_size"`
}

// listRequest is a parsed, cursor-verified list query.
type listRequest struct {
	page        apicontract.Page
	tenantID    string
	principalID string
	route       string
	filterKey   string
	offset      int
}

// legacyListParams are the bare query parameters these routes took before
// API-4. Silently ignoring one would answer an unfiltered list to a caller
// that asked for a filtered one, so each is refused with the name to use.
var legacyListParams = []string{"legal_entity_id", "domain_code", "action_type", "evidence_type", "as_of", "status", "outcome", "since"}

// beginList runs the prelude every console list shares: the cursor signer is
// configured, the caller has a tenant and an identity, the filters are known,
// and any cursor verifies for this tenant, route and filter set. The caller
// then authorizes at the scope its filters name.
func (h *Handler) beginList(w http.ResponseWriter, r *http.Request, allowedFilters []string) (*listRequest, bool) {
	if h.cursors == nil {
		writeError(w, r, http.StatusNotImplemented, "not_configured", "console read models are not configured in this deployment")
		return nil, false
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	q := r.URL.Query()
	if claimed := q.Get("tenant_id"); claimed != "" && claimed != tenantID {
		writeError(w, r, http.StatusForbidden, "tenant_scope_mismatch", domain.ErrTenantScopeMismatch.Error())
		return nil, false
	}
	for _, name := range legacyListParams {
		if q.Has(name) {
			writeError(w, r, http.StatusBadRequest, "unknown_filter",
				"use filter["+name+"]="+q.Get(name)+"; bare query filters are not read by this endpoint")
			return nil, false
		}
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	page, prob := apicontract.ParsePage(r, allowedFilters, nil, "")
	if prob != nil {
		apicontract.WriteProblem(w, r, *prob)
		return nil, false
	}
	lr := &listRequest{
		page:        page,
		tenantID:    tenantID,
		principalID: principalID,
		route:       apicontract.ScopedRoute(r.URL.Path),
		filterKey:   page.FilterKey(),
	}
	if page.Cursor != "" {
		c, err := h.cursors.Verify(page.Cursor, tenantID, lr.route, lr.filterKey, page.SortKey())
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_cursor",
				"the cursor is malformed, was tampered with, or belongs to another tenant or query")
			return nil, false
		}
		lr.offset = c.Offset
	}
	return lr, true
}

// entityScope validates filter[legal_entity_id] and returns the scope a list
// is authorized at: that entity, or PLATFORM for a tenant-wide list.
func entityScope(w http.ResponseWriter, r *http.Request, lr *listRequest) (string, bool) {
	id := lr.page.Filters["legal_entity_id"]
	if id == "" {
		return platformScope, true
	}
	if !isUUID(id) {
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "filter[legal_entity_id] must be a UUID")
		return "", false
	}
	return id, true
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
			writeError(w, r, http.StatusInternalServerError, "list_paging_failed", "")
			return
		}
		resp.NextCursor = next
	}
	writeJSON(w, r, http.StatusOK, resp)
}

func (h *Handler) listFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		writeError(w, r, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
		return
	}
	h.log.Error(what+" failed — store unavailable", zap.Error(err))
	writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
}

// parseInstant reads an RFC3339 filter value; "now" is accepted where allowNow.
func parseInstant(raw string, allowNow bool) (time.Time, bool) {
	if allowNow && raw == "now" {
		return time.Now().UTC(), true
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// ── GET /v1/evidence-requirements ───────────────────────────────────────────
//
// ListRequirements is the Requirements register: the caller's own tenant's
// catalogue. The scope comes from the verified X-Tenant-Id. It once came from
// ?tenant_id=, which let any caller read another tenant's catalogue; a
// tenant_id that disagrees with the verified one is still refused (403).
//
// filter[as_of]=now (or an RFC3339 instant) is what is in force at that
// instant; without it the full history is returned, retired requirements
// included, which is what an auditor reviewing a past decision needs.
func (h *Handler) ListRequirements(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, []string{"legal_entity_id", "domain_code", "action_type", "evidence_type", "as_of"})
	if !ok {
		return
	}
	scope, ok := entityScope(w, r, lr)
	if !ok {
		return
	}
	f := domain.ListRequirementsFilter{
		TenantID:      lr.tenantID,
		LegalEntityID: lr.page.Filters["legal_entity_id"],
		DomainCode:    lr.page.Filters["domain_code"],
		ActionType:    lr.page.Filters["action_type"],
		EvidenceType:  lr.page.Filters["evidence_type"],
	}
	if raw := lr.page.Filters["as_of"]; raw != "" {
		t, ok := parseInstant(raw, true)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "filter[as_of] must be RFC3339 or 'now'")
			return
		}
		f.AsOf = t
	}
	if !h.gate(w, r, h.readMode, lr.principalID, scope, actionReadRequirement) {
		return
	}
	items, err := h.store.ListRequirementsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "requirements list", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ── GET /v1/evidence/evaluations ────────────────────────────────────────────
//
// ListEvaluations is the Evaluations register, newest first. The Missing tab
// is this list with filter[outcome]=MISSING. Each row carries the artifacts
// asserted and the requirement set it was tested against, so it is read at
// the entity it concerns (EVIDENCE_EVALUATION_READ), PLATFORM when unfiltered.
func (h *Handler) ListEvaluations(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, []string{"outcome", "domain_code", "action_type", "legal_entity_id", "since"})
	if !ok {
		return
	}
	scope, ok := entityScope(w, r, lr)
	if !ok {
		return
	}
	f := domain.ListEvaluationsFilter{
		LegalEntityID: lr.page.Filters["legal_entity_id"],
		DomainCode:    lr.page.Filters["domain_code"],
		ActionType:    lr.page.Filters["action_type"],
		Outcome:       lr.page.Filters["outcome"],
	}
	switch domain.Outcome(f.Outcome) {
	case "", domain.OutcomeSatisfied, domain.OutcomeMissing, domain.OutcomeNoRequirementsDefined:
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_filter",
			"filter[outcome] must be SATISFIED, MISSING or NO_REQUIREMENTS_DEFINED")
		return
	}
	if raw := lr.page.Filters["since"]; raw != "" {
		t, ok := parseInstant(raw, false)
		if !ok {
			writeError(w, r, http.StatusBadRequest, "invalid_filter", "filter[since] must be RFC3339")
			return
		}
		f.Since = t
	}
	if !h.gate(w, r, h.readMode, lr.principalID, scope, actionReadEvaluation) {
		return
	}
	items, err := h.store.ListEvaluationsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "evaluations list", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ── GET /v1/evidence/summary ────────────────────────────────────────────────
//
// GetSummary is the console's KPI row and panels. It is tenant-wide, so it is
// authorized at PLATFORM. A count that cannot be read is a 503, never a 0.
func (h *Handler) GetSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.gate(w, r, h.readMode, principalID, platformScope, actionReadRequirement) {
		return
	}
	summary, err := h.store.GetSummary(r.Context(), tenantID)
	if err != nil {
		h.listFailed(w, r, "evidence summary", err)
		return
	}
	writeJSON(w, r, http.StatusOK, summary)
}

// ── GET /v1/pbc/requests ────────────────────────────────────────────────────
//
// ListRequests is the Evidence requests (PBC) register, soonest due first.
// Without filter[status] it lists open requests (not SATISFIED or CLOSED),
// which is what this route always returned; filter[open]=false lists all.
// It is the managers' register, so it is read under EVIDENCE_REQUEST_MANAGE
// at the entity filtered on, PLATFORM when unfiltered.
func (h *pbcHandler) ListRequests(w http.ResponseWriter, r *http.Request) {
	lr, ok := h.beginList(w, r, []string{"legal_entity_id", "status", "open"})
	if !ok {
		return
	}
	scope, ok := entityScope(w, r, lr)
	if !ok {
		return
	}
	f := domain.ListEvidenceRequestsFilter{
		LegalEntityID: lr.page.Filters["legal_entity_id"],
		Status:        strings.ToUpper(lr.page.Filters["status"]),
		OpenOnly:      true,
	}
	switch lr.page.Filters["open"] {
	case "", "true":
	case "false":
		f.OpenOnly = false
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "filter[open] must be true or false")
		return
	}
	switch domain.EvidenceRequestStatus(f.Status) {
	case "", domain.EvidenceRequestDraft, domain.EvidenceRequestSent, domain.EvidenceRequestViewed,
		domain.EvidenceRequestResponseReceived, domain.EvidenceRequestUnderEvaluation, domain.EvidenceRequestSatisfied,
		domain.EvidenceRequestClarificationRequired, domain.EvidenceRequestClosed:
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_filter", "filter[status] is not an evidence request status")
		return
	}
	if !h.gate(w, r, h.readMode, lr.principalID, scope, actionManageEvidenceRequest) {
		return
	}
	items, err := h.store.ListEvidenceRequestsPage(r.Context(), lr.tenantID, f, lr.offset, lr.page.PageSize+1)
	if err != nil {
		h.listFailed(w, r, "evidence requests list", err)
		return
	}
	finishList(h.Handler, w, r, lr, items)
}
