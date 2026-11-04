package context_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/federation"
	"zoiko.io/identity-context-svc/internal/store"
)

// ORG-01 reads: GetSubjectIdentity, ListIdentityLinks, GetAssuranceHistory, ListAuthenticationEvents.

type fakeIdentityReader struct {
	subject *store.SubjectIdentity
	getErr  error
	links   []federation.Link
	obs     []store.AssuranceObservation
	events  []store.AuthenticationEvent
	next    string

	tenant, principal string
	linkIssuer        string
	limit             int
	after             *store.Cursor
	filter            store.AuthenticationEventFilter
}

func (f *fakeIdentityReader) GetSubjectIdentity(_ context.Context, tenantID, principalID string) (*store.SubjectIdentity, error) {
	f.tenant, f.principal = tenantID, principalID
	return f.subject, f.getErr
}

func (f *fakeIdentityReader) ListLinks(_ context.Context, tenantID, principalID, issuer string, limit int) ([]federation.Link, error) {
	f.tenant, f.principal, f.linkIssuer, f.limit = tenantID, principalID, issuer, limit
	return f.links, nil
}

func (f *fakeIdentityReader) ListAssuranceObservations(_ context.Context, tenantID, principalID string, after *store.Cursor, limit int) ([]store.AssuranceObservation, string, error) {
	f.tenant, f.principal, f.after, f.limit = tenantID, principalID, after, limit
	return f.obs, f.next, nil
}

func (f *fakeIdentityReader) ListAuthenticationEvents(_ context.Context, tenantID, principalID string, flt store.AuthenticationEventFilter, after *store.Cursor, limit int) ([]store.AuthenticationEvent, string, error) {
	f.tenant, f.principal, f.filter, f.after, f.limit = tenantID, principalID, flt, after, limit
	return f.events, f.next, nil
}

type identityHarness struct {
	router http.Handler
	reader *fakeIdentityReader
	authz  *permittingAuthz
}

func newIdentityHarness(reader *fakeIdentityReader, authz identityctx.AuthzChecker) *identityHarness {
	pa := &permittingAuthz{}
	if authz == nil {
		authz = pa
	}
	h := identityctx.NewHandler(nil, nil, nil, &mockPrincipalStore{}, authz, zap.NewNop())
	if reader != nil {
		h.WithIdentityReader(reader)
	}
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return &identityHarness{router: r, reader: reader, authz: pa}
}

func (h *identityHarness) get(path, caller string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	if caller != "" {
		req.Header.Set("X-Principal-Id", caller)
	}
	req.Header.Set("X-Correlation-ID", "corr-1")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func subject(id string) *store.SubjectIdentity {
	s := &store.SubjectIdentity{}
	s.PrincipalID = id
	return s
}

func TestIdentityReads_UnconfiguredIs501(t *testing.T) {
	h := newIdentityHarness(nil, nil)
	for _, p := range []string{"", "/links", "/assurance-history", "/authentication-events"} {
		assert.Equal(t, http.StatusNotImplemented, h.get("/v1/identities/p-1"+p, "admin").Code, p)
	}
}

func TestIdentityReads_RequireVerifiedCaller(t *testing.T) {
	h := newIdentityHarness(&fakeIdentityReader{subject: subject("p-1")}, nil)
	assert.Equal(t, http.StatusUnauthorized, h.get("/v1/identities/p-1", "").Code, "no X-Principal-Id")
}

func TestGetSubjectIdentity_ReadingAnotherNeedsAGrantReadingYourOwnDoesNot(t *testing.T) {
	reader := &fakeIdentityReader{subject: subject("p-1")}
	h := newIdentityHarness(reader, nil)

	assert.Equal(t, http.StatusOK, h.get("/v1/identities/p-1", "p-1").Code)
	assert.Empty(t, h.authz.seen, "your own identity is not a privileged read")

	assert.Equal(t, http.StatusOK, h.get("/v1/identities/p-1", "admin").Code)
	assert.Equal(t, []string{identityctx.ActionIdentityRead}, h.authz.seen)
	assert.Equal(t, "tenant-a", reader.tenant)
}

func TestIdentityReads_DeniedWithoutTheGrant(t *testing.T) {
	h := newIdentityHarness(&fakeIdentityReader{subject: subject("p-1")}, denyingAuthz{})
	for _, p := range []string{"", "/links", "/assurance-history", "/authentication-events"} {
		assert.Equal(t, http.StatusForbidden, h.get("/v1/identities/p-1"+p, "admin").Code, p)
	}
}

func TestIdentityReads_AuthzOutageFailsClosed(t *testing.T) {
	h := newIdentityHarness(&fakeIdentityReader{subject: subject("p-1")}, outageAuthz{})
	assert.Equal(t, http.StatusServiceUnavailable, h.get("/v1/identities/p-1", "admin").Code)
}

type outageAuthz struct{}

func (outageAuthz) CheckAllowed(context.Context, string, string, string, string) error {
	return errors.New("authorization-svc unreachable")
}

func TestGetSubjectIdentity_UnknownIs404(t *testing.T) {
	h := newIdentityHarness(&fakeIdentityReader{}, nil)
	assert.Equal(t, http.StatusNotFound, h.get("/v1/identities/nobody", "admin").Code)
}

func TestGetSubjectIdentity_StoreFailureIs503(t *testing.T) {
	h := newIdentityHarness(&fakeIdentityReader{getErr: errors.New("db down")}, nil)
	assert.Equal(t, http.StatusServiceUnavailable, h.get("/v1/identities/p-1", "admin").Code)
}

func TestListIdentityLinks_PassesIssuerAndLimitAndNeverNull(t *testing.T) {
	reader := &fakeIdentityReader{subject: subject("p-1")}
	h := newIdentityHarness(reader, nil)

	w := h.get("/v1/identities/p-1/links?issuer="+url.QueryEscape("https://idp.example.com")+"&limit=7", "admin")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"items":[]}`, w.Body.String(), "no links is an empty array, not null")
	assert.Equal(t, "p-1", reader.principal)
	assert.Equal(t, "https://idp.example.com", reader.linkIssuer)
	assert.Equal(t, 7, reader.limit)

	reader.links = []federation.Link{{LinkID: "l-1", PrincipalID: "p-1", Issuer: "https://idp.example.com", Subject: "u-1"}}
	w = h.get("/v1/identities/p-1/links", "admin")
	assert.Contains(t, w.Body.String(), `"link_id":"l-1"`)

	reader.subject = nil
	assert.Equal(t, http.StatusNotFound, h.get("/v1/identities/p-1/links", "admin").Code, "links of an unknown subject are 404, not an empty list")
}

func TestGetAssuranceHistory_PagingAndRefusals(t *testing.T) {
	reader := &fakeIdentityReader{subject: subject("p-1"), next: "next-token",
		obs: []store.AssuranceObservation{{ObservationID: "o-1", AssuranceLevel: "MFA", ChangeKind: "STEP_UP", AuthMethods: []string{"pwd", "otp"}}}}
	h := newIdentityHarness(reader, nil)

	w := h.get("/v1/identities/p-1/assurance-history?limit=5", "admin")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"next_cursor":"next-token"`)
	assert.Contains(t, w.Body.String(), `"change_kind":"STEP_UP"`)
	assert.Equal(t, 5, reader.limit)
	assert.Nil(t, reader.after)

	c := store.Cursor{At: time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC), ID: "o-9"}
	w = h.get("/v1/identities/p-1/assurance-history?cursor="+url.QueryEscape(c.Encode()), "admin")
	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, reader.after)
	assert.Equal(t, "o-9", reader.after.ID)

	for _, bad := range []string{"limit=0", "limit=201", "limit=abc", "cursor=garbage"} {
		assert.Equal(t, http.StatusBadRequest, h.get("/v1/identities/p-1/assurance-history?"+bad, "admin").Code, bad)
	}
}

func TestListAuthenticationEvents_NeedsItsOwnGrantAndFilters(t *testing.T) {
	reader := &fakeIdentityReader{subject: subject("p-1"),
		events: []store.AuthenticationEvent{{EventID: "e-1", EventType: "AUTHENTICATE", Outcome: "DENIED", Source: "DECISION_LOG"}}}
	h := newIdentityHarness(reader, nil)

	q := "?event_type=AUTHENTICATE&outcome=DENIED&since=" + url.QueryEscape("2026-11-01T00:00:00Z") +
		"&until=" + url.QueryEscape("2026-11-02T00:00:00Z")
	w := h.get("/v1/identities/p-1/authentication-events"+q, "admin")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{identityctx.ActionAuthEventsRead}, h.authz.seen, "a separate, more sensitive action")
	assert.Equal(t, "AUTHENTICATE", reader.filter.EventType)
	assert.Equal(t, "DENIED", reader.filter.Outcome)
	require.NotNil(t, reader.filter.Since)
	require.NotNil(t, reader.filter.Until)
	assert.Equal(t, 2026, reader.filter.Since.Year())
	assert.Equal(t, 2, reader.filter.Until.Day())

	assert.Equal(t, http.StatusBadRequest, h.get("/v1/identities/p-1/authentication-events?since=yesterday", "admin").Code)
	assert.Equal(t, http.StatusBadRequest, h.get("/v1/identities/p-1/authentication-events?until=2026-11-02", "admin").Code)

	reader.events = nil
	w = h.get("/v1/identities/p-1/authentication-events", "p-1")
	assert.Contains(t, w.Body.String(), `"items":[]`)
}

func TestAuthenticationEvents_CarryNoSecretFields(t *testing.T) {
	// The read model's shape is the evidence rule: identifiers, methods, outcomes. A field that could hold a
	// code, token or password would have to be added here deliberately.
	reader := &fakeIdentityReader{subject: subject("p-1"), events: []store.AuthenticationEvent{{EventID: "e-1"}}}
	body := newIdentityHarness(reader, nil).get("/v1/identities/p-1/authentication-events", "admin").Body.String()
	for _, banned := range []string{"password", "secret", "token", "code\"", "credential"} {
		assert.NotContains(t, body, banned)
	}
}
