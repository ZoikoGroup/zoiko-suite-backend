package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

// ── stubStore: the register reads (handler.listStore) ───────────────────────

var listStub = struct {
	families    []*domain.Policy
	versions    []*domain.PolicyVersion
	evaluations []*domain.Evaluation
	definitions []*domain.ControlTestDefinition
	attestation []*domain.Attestation
	summary     *domain.PolicySummary
	err         error
	last        domain.ListQuery
	calls       int
}{}

func resetListStub() {
	listStub.families, listStub.versions, listStub.evaluations = nil, nil, nil
	listStub.definitions, listStub.attestation, listStub.summary = nil, nil, nil
	listStub.err, listStub.last, listStub.calls = nil, domain.ListQuery{}, 0
}

// window answers the slice of all that the query names, as the SQL
// LIMIT/OFFSET does.
func window[T any](all []T, q domain.ListQuery) []T {
	listStub.last, listStub.calls = q, listStub.calls+1
	if q.Offset >= len(all) {
		return nil
	}
	end := q.Offset + q.Limit
	if end > len(all) {
		end = len(all)
	}
	return all[q.Offset:end]
}

func (s *stubStore) ListPolicyFamilies(_ context.Context, q domain.ListQuery) ([]*domain.Policy, error) {
	return window(listStub.families, q), listStub.err
}
func (s *stubStore) ListPolicyVersions(_ context.Context, q domain.ListQuery) ([]*domain.PolicyVersion, error) {
	return window(listStub.versions, q), listStub.err
}
func (s *stubStore) ListEvaluations(_ context.Context, q domain.ListQuery) ([]*domain.Evaluation, error) {
	return window(listStub.evaluations, q), listStub.err
}
func (s *stubStore) ListControlTestDefinitions(_ context.Context, q domain.ListQuery) ([]*domain.ControlTestDefinition, error) {
	return window(listStub.definitions, q), listStub.err
}
func (s *stubStore) ListAttestations(_ context.Context, q domain.ListQuery) ([]*domain.Attestation, error) {
	return window(listStub.attestation, q), listStub.err
}
func (s *stubStore) PolicySummary(context.Context) (*domain.PolicySummary, error) {
	listStub.calls++
	return listStub.summary, listStub.err
}

type page struct {
	Items      []map[string]any `json:"items"`
	NextCursor string           `json:"next_cursor"`
	PageSize   int              `json:"page_size"`
}

func readPage(t *testing.T, w interface{ Result() *http.Response }, body []byte) page {
	t.Helper()
	var p page
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("not a page: %v: %s", err, body)
	}
	return p
}

func families(n int) []*domain.Policy {
	out := make([]*domain.Policy, n)
	for i := range out {
		out[i] = &domain.Policy{PolicyID: strconv.Itoa(i), PolicyCode: "P-" + strconv.Itoa(i)}
	}
	return out
}

// API-4: first page, next page by cursor, last page without one.
func TestLists_PagingFollowsTheCursor(t *testing.T) {
	resetListStub()
	listStub.families = families(5)
	g := newRig(nil, nil, nil)

	w := g.do(http.MethodGet, "/v1/policy-families?page_size=2", "")
	expect(t, w, http.StatusOK, "")
	p := readPage(t, w, w.Body.Bytes())
	if len(p.Items) != 2 || p.NextCursor == "" || p.PageSize != 2 || p.Items[0]["policy_code"] != "P-0" {
		t.Fatalf("first page: %+v", p)
	}
	if listStub.last.Limit != 3 || listStub.last.Offset != 0 {
		t.Fatalf("the store must be asked for one row more than the page: %+v", listStub.last)
	}

	w = g.do(http.MethodGet, "/v1/policy-families?page_size=2&cursor="+url.QueryEscape(p.NextCursor), "")
	p = readPage(t, w, w.Body.Bytes())
	if len(p.Items) != 2 || p.Items[0]["policy_code"] != "P-2" || p.NextCursor == "" {
		t.Fatalf("second page: %+v", p)
	}
	w = g.do(http.MethodGet, "/v1/policy-families?page_size=2&cursor="+url.QueryEscape(p.NextCursor), "")
	p = readPage(t, w, w.Body.Bytes())
	if len(p.Items) != 1 || p.Items[0]["policy_code"] != "P-4" || p.NextCursor != "" {
		t.Fatalf("last page: %+v", p)
	}
}

// API-4 negative paths: a tampered cursor, another tenant's cursor, a cursor
// for another filter set, an unknown filter and a bad page size are 400.
func TestLists_CursorAndFilterRefusals(t *testing.T) {
	resetListStub()
	listStub.families = families(3)
	g := newRig(nil, nil, nil)
	w := g.do(http.MethodGet, "/v1/policy-families?page_size=1&filter[scope]=TENANT", "")
	cursor := readPage(t, w, w.Body.Bytes()).NextCursor
	if cursor == "" {
		t.Fatal("setup: no cursor")
	}
	calls := listStub.calls

	expect(t, g.do(http.MethodGet, "/v1/policy-families?page_size=1&filter[scope]=TENANT&cursor="+url.QueryEscape(cursor+"x"), ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-families?page_size=1&filter[scope]=TENANT&cursor="+url.QueryEscape(cursor), "", header("X-Tenant-Id", otherTenant)), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-families?page_size=1&filter[scope]=GLOBAL&cursor="+url.QueryEscape(cursor), ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-versions?page_size=1&cursor="+url.QueryEscape(cursor), ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-families?filter[owner]=x", ""), http.StatusBadRequest, "unknown_filter")
	expect(t, g.do(http.MethodGet, "/v1/policy-families?page_size=500", ""), http.StatusBadRequest, "invalid_page_size")
	expect(t, g.do(http.MethodGet, "/v1/policy-families?filter[scope]=EVERYONE", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-versions?filter[policy_id]=not-a-uuid", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-versions?filter[version_status]=LIVE", ""), http.StatusBadRequest, "INVALID_REQUEST")
	expect(t, g.do(http.MethodGet, "/v1/policy-families", "", noTenant), http.StatusUnauthorized, "CONTEXT_UNRESOLVED")
	if listStub.calls != calls {
		t.Fatal("a refused list query reached the store")
	}
}

// Filters reach the store normalised.
func TestLists_FiltersReachTheStore(t *testing.T) {
	resetListStub()
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policy-versions?filter[version_status]=active&filter[policy_id]="+policyID, ""), http.StatusOK, "")
	if listStub.last.Filter("version_status") != "ACTIVE" || listStub.last.Filter("policy_id") != policyID {
		t.Fatalf("filters = %+v", listStub.last.Filters)
	}
}

// Each register is authorized with its own read permission.
func TestLists_AuthorizedWithTheirReadPermission(t *testing.T) {
	for path, action := range map[string]string{
		"/v1/policies/summary":         authz.ActionPolicyRead,
		"/v1/policy-families":          authz.ActionPolicyRead,
		"/v1/policy-versions":          authz.ActionPolicyRead,
		"/v1/policy-evaluations":       authz.ActionPolicyRead,
		"/v1/control-test-definitions": authz.ActionControlTestDefinitionRead,
		"/v1/attestations":             authz.ActionAttestationRead,
	} {
		resetListStub()
		listStub.summary = &domain.PolicySummary{}
		g := newRig(nil, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
		expect(t, g.do(http.MethodGet, path, ""), http.StatusForbidden, "AUTHORIZATION_DENIED")
		if g.az.actionType != action {
			t.Errorf("%s: want %s, got %s", path, action, g.az.actionType)
		}
		if listStub.calls != 0 {
			t.Errorf("%s: a denied read reached the store", path)
		}
	}
}

func TestLists_StoreUnavailableIs503(t *testing.T) {
	resetListStub()
	listStub.err = domain.ErrStoreUnavailable
	g := newRig(nil, nil, nil)
	expect(t, g.do(http.MethodGet, "/v1/policy-evaluations", ""), http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE")
}

func TestSummary_ServesTheStoreCounts(t *testing.T) {
	resetListStub()
	listStub.summary = &domain.PolicySummary{TenantID: testTenant, VersionsActive: 3, SuspensionsAwaitReview: 1}
	g := newRig(nil, nil, nil)
	body := expect(t, g.do(http.MethodGet, "/v1/policies/summary", ""), http.StatusOK, "")
	if body["versions_active"] != float64(3) || body["suspensions_awaiting_review"] != float64(1) {
		t.Fatalf("summary = %v", body)
	}
}

// The per-parent lists (a policy's versions, a version's transitions, a
// definition's executions) answer in the register shape and page too.
func TestLists_PerParentListsArePaged(t *testing.T) {
	resetListStub()
	st := &stubStore{
		history:     []*domain.PolicyVersion{{PolicyVersionID: "a"}, {PolicyVersionID: "b"}, {PolicyVersionID: "c"}},
		transitions: []*domain.VersionTransition{{TransitionID: 1}, {TransitionID: 2}},
		executions:  []*domain.ControlTestExecution{{ControlTestExecutionID: "x"}},
	}
	g := newRig(st, nil, nil)
	w := g.do(http.MethodGet, "/v1/policies/"+policyID+"/versions?page_size=2", "")
	p := readPage(t, w, w.Body.Bytes())
	if len(p.Items) != 2 || p.NextCursor == "" {
		t.Fatalf("history page: %+v", p)
	}
	w = g.do(http.MethodGet, "/v1/policies/"+policyID+"/versions?page_size=2&cursor="+url.QueryEscape(p.NextCursor), "")
	p = readPage(t, w, w.Body.Bytes())
	if len(p.Items) != 1 || p.Items[0]["policy_version_id"] != "c" || p.NextCursor != "" {
		t.Fatalf("history second page: %+v", p)
	}
	w = g.do(http.MethodGet, "/v1/policy-versions/"+versionID+"/transitions", "")
	if p = readPage(t, w, w.Body.Bytes()); len(p.Items) != 2 {
		t.Fatalf("transitions page: %+v", p)
	}
	w = g.do(http.MethodGet, "/v1/control-test-definitions/"+versionID+"/executions", "")
	if p = readPage(t, w, w.Body.Bytes()); len(p.Items) != 1 {
		t.Fatalf("executions page: %+v", p)
	}
}
