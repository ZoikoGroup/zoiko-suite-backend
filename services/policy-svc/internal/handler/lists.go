package handler

// Console registers and the KPI summary (API-4, ZS-API-001), the same shape
// as evidence-requirements-svc's and identity-context-svc's lists:
//
//   - filters are filter[name]=value; an unknown filter is 400 unknown_filter;
//   - page_size defaults to 50, at most 200;
//   - next_cursor is HMAC-signed and bound to the tenant, the route and the
//     filter set, so a cursor that was tampered with, or is replayed by
//     another tenant or against another query, is 400 invalid_cursor.
//
// GET /v1/policies and GET /v1/accounting-policies are not registers: they
// are applicability sets, ordered by precedence and bounded by the scope
// asked about, and a caller needs all of them to resolve. They stay arrays
// (SPEC_DEVIATIONS.md). The registers of families and versions are
// /v1/policy-families and /v1/policy-versions.

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// listStore is the store's register reads, optional like the accounting
// ones, so a test double needs only the methods its test exercises.
type listStore interface {
	ListPolicyFamilies(ctx context.Context, q domain.ListQuery) ([]*domain.Policy, error)
	ListPolicyVersions(ctx context.Context, q domain.ListQuery) ([]*domain.PolicyVersion, error)
	ListEvaluations(ctx context.Context, q domain.ListQuery) ([]*domain.Evaluation, error)
	ListControlTestDefinitions(ctx context.Context, q domain.ListQuery) ([]*domain.ControlTestDefinition, error)
	ListAttestations(ctx context.Context, q domain.ListQuery) ([]*domain.Attestation, error)
	PolicySummary(ctx context.Context) (*domain.PolicySummary, error)
}

// listResponse is the shape of every register (API-4).
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

// query is the store's view of the page: one row more than the page size,
// so finishList can tell whether there is a next page.
func (lr *listRequest) query() domain.ListQuery {
	return domain.ListQuery{Filters: lr.page.Filters, Limit: lr.page.PageSize + 1, Offset: lr.offset}
}

// newRandomCursorSigner is the signer of a Handler built without one (tests,
// local runs): cursors then only survive this process. Production passes
// the configured CURSOR_SIGNING_KEY_HEX.
func newRandomCursorSigner() *apicontract.CursorSigner {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s, _ := apicontract.NewCursorSigner(key)
	return s
}

// filterSpec says how a filter's value is checked before it reaches SQL.
type filterSpec struct {
	name  string
	uuid  bool     // must be a UUID
	upper bool     // compared upper-case (statuses, types)
	oneOf []string // allowed values, when closed
}

// beginList runs the prelude every register shares: tenant, filters, page
// size and cursor. Authorization is the caller's, at the scope it lists.
func (h *Handler) beginList(w http.ResponseWriter, r *http.Request, filters []filterSpec) (*listRequest, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	names := make([]string, len(filters))
	for i, f := range filters {
		names[i] = f.name
	}
	page, prob := apicontract.ParsePage(r, names, nil, "")
	if prob != nil {
		apicontract.WriteProblem(w, r, *prob)
		return nil, false
	}
	for _, f := range filters {
		v, present := page.Filters[f.name]
		if !present {
			continue
		}
		if f.upper {
			v = strings.ToUpper(strings.TrimSpace(v))
			page.Filters[f.name] = v
		}
		if f.uuid && !validUUID(v) {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_filter", "filter["+f.name+"] must be a UUID", nil)
			return nil, false
		}
		if len(f.oneOf) > 0 && !contains(f.oneOf, v) {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_filter",
				"filter["+f.name+"] must be one of "+strings.Join(f.oneOf, ", "), nil)
			return nil, false
		}
		if tooLong(w, r, "filter["+f.name+"]", v, 255) {
			return nil, false
		}
	}
	lr := &listRequest{page: page, tenantID: tenantID, route: apicontract.ScopedRoute(r.URL.Path), filterKey: page.FilterKey()}
	if page.Cursor != "" {
		c, err := h.cursors.Verify(page.Cursor, tenantID, lr.route, lr.filterKey, page.SortKey())
		if err != nil {
			writeError(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid_cursor",
				"the cursor is malformed, was tampered with, or belongs to another tenant or query", nil)
			return nil, false
		}
		lr.offset = c.Offset
	}
	return lr, true
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// finishList trims the look-ahead row, signs the next cursor and writes the page.
func finishList[T any](h *Handler, w http.ResponseWriter, r *http.Request, lr *listRequest, items []T) {
	resp := listResponse[T]{Items: items, PageSize: lr.page.PageSize}
	if resp.Items == nil {
		resp.Items = []T{}
	}
	if len(resp.Items) > lr.page.PageSize {
		resp.Items = resp.Items[:lr.page.PageSize]
		next, err := h.cursors.Next(lr.page, lr.tenantID, lr.route, lr.offset+lr.page.PageSize, "")
		if err != nil {
			h.log.Error("sign next cursor failed", zap.Error(err))
			writeError(w, r, http.StatusInternalServerError, codeUnavailable, "list_paging_failed", "", nil)
			return
		}
		resp.NextCursor = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// pageOf pages a list the store already returned whole (a policy's
// versions, a version's transitions, a definition's executions: each bounded
// by its parent), so these lists answer in the same shape as the registers.
func pageOf[T any](h *Handler, w http.ResponseWriter, r *http.Request, lr *listRequest, all []T) {
	if lr.offset >= len(all) {
		finishList(h, w, r, lr, []T{})
		return
	}
	end := lr.offset + lr.page.PageSize + 1
	if end > len(all) {
		end = len(all)
	}
	finishList(h, w, r, lr, all[lr.offset:end])
}

func (h *Handler) lists(w http.ResponseWriter, r *http.Request) (listStore, bool) {
	s, ok := h.store.(listStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not serve registers", nil)
	}
	return s, ok
}

func (h *Handler) listFailed(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, domain.ErrPolicyNotFound) || errors.Is(err, domain.ErrPolicyVersionNotFound) ||
		errors.Is(err, domain.ErrControlTestDefinitionNotFound) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "not_found", "", nil)
		return
	}
	h.storeUnavailable(w, r, op, err)
}

var versionStatuses = []string{
	domain.StatusDraft, domain.StatusReview, domain.StatusApproved, domain.StatusScheduled,
	domain.StatusActive, domain.StatusSuperseded, domain.StatusSuspended, domain.StatusRetired,
}

// RegisterListRoutes mounts the registers and the summary.
func RegisterListRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/policies/summary", h.GetPolicySummary)
	r.Get("/v1/policy-families", h.ListPolicyFamilies)
	r.Get("/v1/policy-versions", h.ListPolicyVersions)
	r.Get("/v1/policy-evaluations", h.ListEvaluations)
	r.Get("/v1/control-test-definitions", h.ListControlTestDefinitions)
	r.Get("/v1/attestations", h.ListAttestations)
}

// GetPolicySummary handles GET /v1/policies/summary: the console KPI row.
func (h *Handler) GetPolicySummary(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	sum, err := s.PolicySummary(r.Context())
	if err != nil {
		h.storeUnavailable(w, r, "PolicySummary", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// ListPolicyFamilies handles GET /v1/policy-families.
func (h *Handler) ListPolicyFamilies(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	lr, ok := h.beginList(w, r, []filterSpec{
		{name: "policy_type", upper: true},
		{name: "scope", upper: true, oneOf: []string{domain.ScopeTypeTenant, domain.ScopeTypeGlobal}},
		{name: "policy_status", upper: true},
	})
	if !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	items, err := s.ListPolicyFamilies(r.Context(), lr.query())
	if err != nil {
		h.listFailed(w, r, "ListPolicyFamilies", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListPolicyVersions handles GET /v1/policy-versions.
func (h *Handler) ListPolicyVersions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	lr, ok := h.beginList(w, r, []filterSpec{
		{name: "policy_id", uuid: true},
		{name: "policy_type", upper: true},
		{name: "version_status", upper: true, oneOf: versionStatuses},
		{name: "legal_entity_id", uuid: true},
	})
	if !ok {
		return
	}
	entity := lr.page.Filters["legal_entity_id"]
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, blankToNil(&entity)); !ok {
		return
	}
	items, err := s.ListPolicyVersions(r.Context(), lr.query())
	if err != nil {
		h.listFailed(w, r, "ListPolicyVersions", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListEvaluations handles GET /v1/policy-evaluations: the caller tenant's
// evaluation evidence.
func (h *Handler) ListEvaluations(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	lr, ok := h.beginList(w, r, []filterSpec{
		{name: "policy_type", upper: true},
		{name: "result", upper: true},
		{name: "legal_entity_id", uuid: true},
		{name: "decision_id"},
		{name: "policy_version_id", uuid: true},
	})
	if !ok {
		return
	}
	entity := lr.page.Filters["legal_entity_id"]
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, blankToNil(&entity)); !ok {
		return
	}
	items, err := s.ListEvaluations(r.Context(), lr.query())
	if err != nil {
		h.listFailed(w, r, "ListEvaluations", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListControlTestDefinitions handles GET /v1/control-test-definitions.
func (h *Handler) ListControlTestDefinitions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	lr, ok := h.beginList(w, r, []filterSpec{{name: "control_ref"}, {name: "design_status", upper: true}})
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionControlTestDefinitionRead, nil) {
		return
	}
	items, err := s.ListControlTestDefinitions(r.Context(), lr.query())
	if err != nil {
		h.listFailed(w, r, "ListControlTestDefinitions", err)
		return
	}
	finishList(h, w, r, lr, items)
}

// ListAttestations handles GET /v1/attestations.
func (h *Handler) ListAttestations(w http.ResponseWriter, r *http.Request) {
	s, ok := h.lists(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	lr, ok := h.beginList(w, r, []filterSpec{
		{name: "subject_ref"},
		{name: "attestation_status", upper: true, oneOf: []string{"ACTIVE", "CHALLENGED", "REVOKED"}},
		{name: "signer_principal_id"},
	})
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.ActionAttestationRead, nil) {
		return
	}
	items, err := s.ListAttestations(r.Context(), lr.query())
	if err != nil {
		h.listFailed(w, r, "ListAttestations", err)
		return
	}
	finishList(h, w, r, lr, items)
}
