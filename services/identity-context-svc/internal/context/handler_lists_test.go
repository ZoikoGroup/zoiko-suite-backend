package context_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
	"zoiko.io/identity-envelope/apicontract"
)

// fakeConsole serves n principals and n sessions, recording the offsets and
// filters each call received.
type fakeConsole struct {
	n          int
	offsets    []int
	principalF store.PrincipalListFilter
	resetF     store.ResetListFilter
}

func (f *fakeConsole) ListPrincipalsPage(_ context.Context, _ string, flt store.PrincipalListFilter, offset, limit int) ([]store.PrincipalDetail, error) {
	f.offsets = append(f.offsets, offset)
	f.principalF = flt
	out := []store.PrincipalDetail{}
	for i := offset; i < f.n && len(out) < limit; i++ {
		out = append(out, store.PrincipalDetail{Principal: domain.Principal{PrincipalID: "p-" + string(rune('a'+i))}})
	}
	return out, nil
}
func (f *fakeConsole) ListSessionsPage(_ context.Context, _ string, _ store.SessionListFilter, offset, _ int) ([]store.SessionSummary, error) {
	f.offsets = append(f.offsets, offset)
	return nil, nil
}
func (f *fakeConsole) ListSupportContextsPage(_ context.Context, _, state string, _, _ int) ([]domain.SupportContext, error) {
	if state == "BOGUS" {
		return nil, store.ErrInvalidFilter
	}
	return nil, nil
}
func (f *fakeConsole) ListAuthenticationEventsPage(_ context.Context, _ string, _ store.AuthEventFilter, _, _ int) ([]domain.AuthenticationEvent, error) {
	return nil, nil
}
func (f *fakeConsole) ListAssuranceHistoryPage(_ context.Context, _, _ string, _, _ int) ([]store.AssuranceChange, error) {
	return nil, nil
}
func (f *fakeConsole) ListPasswordResetsPage(_ context.Context, tenantID string, flt store.ResetListFilter, offset, limit int) ([]domain.PasswordResetRequest, error) {
	f.offsets = append(f.offsets, offset)
	f.resetF = flt
	out := []domain.PasswordResetRequest{}
	for i := offset; i < f.n && len(out) < limit; i++ {
		out = append(out, domain.PasswordResetRequest{RequestID: "r-" + string(rune('a'+i)), TenantID: tenantID,
			PrincipalID: "P-ALICE", RequestedBy: "P-ADMIN", Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{}})
	}
	return out, nil
}
func (f *fakeConsole) GetIdentitySummary(_ context.Context, _ string) (*store.IdentitySummary, error) {
	return &store.IdentitySummary{PrincipalsByStatus: map[string]int{"ACTIVE": 3}}, nil
}

func consoleRouter(t *testing.T, fc *fakeConsole, az identityctx.AuthzChecker) chi.Router {
	t.Helper()
	signer, err := apicontract.NewCursorSigner([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	h := identityctx.NewHandler(nil, nil, nil, nil, az, zap.NewNop()).WithConsole(fc, signer)
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r
}

func getAs(r chi.Router, path, tenantID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "P-ADMIN")
	req.Header.Set("X-Correlation-ID", "test-correlation")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type pageBody struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor string            `json:"next_cursor"`
	PageSize   int               `json:"page_size"`
}

func TestIC2_PrincipalsFirstAndNextPage(t *testing.T) {
	fc := &fakeConsole{n: 3}
	r := consoleRouter(t, fc, &permittingAuthz{})

	w := getAs(r, "/v1/principals?page_size=2&filter[status]=ACTIVE&q=al", "tenant-a")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	assert.Len(t, first.Items, 2)
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, store.PrincipalListFilter{Status: "ACTIVE", Query: "al"}, fc.principalF)

	w = getAs(r, "/v1/principals?page_size=2&filter[status]=ACTIVE&q=al&cursor="+first.NextCursor, "tenant-a")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var second pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	assert.Len(t, second.Items, 1)
	assert.Empty(t, second.NextCursor, "the last page has no next cursor")
	assert.Equal(t, []int{0, 2}, fc.offsets)
}

func TestIC2_CursorBoundToTenantRouteAndFilter(t *testing.T) {
	fc := &fakeConsole{n: 5}
	r := consoleRouter(t, fc, &permittingAuthz{})
	w := getAs(r, "/v1/principals?page_size=2&filter[status]=ACTIVE", "tenant-a")
	require.Equal(t, http.StatusOK, w.Code)
	var first pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	c := first.NextCursor

	cases := map[string]struct{ path, tenant string }{
		"another tenant": {"/v1/principals?page_size=2&filter[status]=ACTIVE&cursor=" + c, "tenant-b"},
		"another filter": {"/v1/principals?page_size=2&filter[status]=DISABLED&cursor=" + c, "tenant-a"},
		"another search": {"/v1/principals?page_size=2&filter[status]=ACTIVE&q=x&cursor=" + c, "tenant-a"},
		"another route":  {"/v1/context/sessions?page_size=2&cursor=" + c, "tenant-a"},
		"tampered":       {"/v1/principals?page_size=2&filter[status]=ACTIVE&cursor=" + tamper(c), "tenant-a"},
		"not a cursor":   {"/v1/principals?cursor=garbage", "tenant-a"},
	}
	for name, tc := range cases {
		w := getAs(r, tc.path, tc.tenant)
		assert.Equal(t, http.StatusBadRequest, w.Code, name+": "+w.Body.String())
		assert.Contains(t, w.Body.String(), "invalid_cursor", name)
	}
}

func tamper(c string) string {
	payload, mac, _ := strings.Cut(c, ".")
	b := []byte(payload)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b) + "." + mac
}

func TestIC2_UnknownOrInvalidFilterRefused(t *testing.T) {
	r := consoleRouter(t, &fakeConsole{}, &permittingAuthz{})
	for _, path := range []string{
		"/v1/principals?filter[tenant_id]=tenant-b",
		"/v1/principals?filter[status]=MAYBE",
		"/v1/context/sessions?filter[ip]=1.2.3.4",
		"/v1/context/support?filter[status]=BOGUS",
		"/v1/authentication-events?filter[outcome]=MAYBE",
		"/v1/authentication-events?filter[since]=yesterday",
		"/v1/password-resets?filter[status]=MAYBE",
		"/v1/password-resets?filter[requested_by]=P-ADMIN",
		"/v1/principals?page_size=500",
	} {
		w := getAs(r, path, "tenant-a")
		assert.Equal(t, http.StatusBadRequest, w.Code, path+": "+w.Body.String())
	}
}

func TestIC2_ListsAuthorizeAtTheirReadAction(t *testing.T) {
	az := &permittingAuthz{}
	r := consoleRouter(t, &fakeConsole{}, az)
	for _, path := range []string{"/v1/principals", "/v1/context/sessions", "/v1/context/support",
		"/v1/authentication-events", "/v1/principals/p-1/assurance-history", "/v1/identity/summary", "/v1/password-resets"} {
		w := getAs(r, path, "tenant-a")
		assert.Equal(t, http.StatusOK, w.Code, path+": "+w.Body.String())
	}
	assert.Equal(t, []string{"IDENTITY_PRINCIPAL_READ", "IDENTITY_SESSION_READ", "IDENTITY_SUPPORT_CONTEXT_REVIEW",
		"IDENTITY_AUTH_EVENTS_READ", "IDENTITY_PRINCIPAL_READ", "IDENTITY_PRINCIPAL_READ", "IDENTITY_PRINCIPAL_READ"}, az.seen)

	denied := consoleRouter(t, &fakeConsole{}, &denyingAuthz{})
	for _, path := range []string{"/v1/principals", "/v1/identity/summary", "/v1/authentication-events", "/v1/password-resets"} {
		w := getAs(denied, path, "tenant-a")
		assert.Equal(t, http.StatusForbidden, w.Code, path)
	}
}

func TestIC2_EmptyListIsAnArrayNeverNull(t *testing.T) {
	r := consoleRouter(t, &fakeConsole{}, &permittingAuthz{})
	w := getAs(r, "/v1/context/sessions", "tenant-a")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"items":[]`)
}

func TestIC2_PasswordResetsRegisterPagesAndNamesTheRequester(t *testing.T) {
	fc := &fakeConsole{n: 3}
	r := consoleRouter(t, fc, &permittingAuthz{})

	w := getAs(r, "/v1/password-resets?page_size=2&filter[status]=PENDING_APPROVAL&filter[principal_id]=P-ALICE", "tenant-a")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	assert.Len(t, first.Items, 2)
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, store.ResetListFilter{Status: "PENDING_APPROVAL", PrincipalID: "P-ALICE"}, fc.resetF)
	// The console hides approve/reject from the requester: each row says who that is.
	assert.Contains(t, string(first.Items[0]), `"requested_by":"P-ADMIN"`)
	assert.Contains(t, string(first.Items[0]), `"approvals":[]`)

	w = getAs(r, "/v1/password-resets?page_size=2&filter[status]=PENDING_APPROVAL&filter[principal_id]=P-ALICE&cursor="+first.NextCursor, "tenant-a")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var second pageBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	assert.Len(t, second.Items, 1)
	assert.Empty(t, second.NextCursor)

	// A cursor from another tenant or another filter is refused.
	for _, tc := range []struct{ path, tenant string }{
		{"/v1/password-resets?page_size=2&filter[status]=PENDING_APPROVAL&filter[principal_id]=P-ALICE&cursor=" + first.NextCursor, "tenant-b"},
		{"/v1/password-resets?page_size=2&filter[status]=ISSUED&filter[principal_id]=P-ALICE&cursor=" + first.NextCursor, "tenant-a"},
	} {
		w := getAs(r, tc.path, tc.tenant)
		assert.Equal(t, http.StatusBadRequest, w.Code, tc.path)
		assert.Contains(t, w.Body.String(), "invalid_cursor")
	}
}
