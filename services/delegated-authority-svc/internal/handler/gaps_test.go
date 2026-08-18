package handler_test

// Tests for the gaps closed in the 18 Aug pass. Each one fails against the
// previous behaviour, which is the only reason to write it down.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/handler"
	"zoiko.io/delegated-authority-svc/internal/middleware"
)

// ── a stub that can deny one specific (principal, action) pair ────────────────

// scopedAuthZ denies exactly the (principal, action) pairs it is told to, and
// records every call. The existing stubAuthZ can only deny by principal, which
// cannot express "the caller may create delegations but may NOT administer
// them" — the distinction the escalation fix turns on.
type scopedAuthZ struct {
	denied map[string]bool // "principal|action"
	calls  []string        // "principal|action", in order
}

func newScopedAuthZ(deny ...string) *scopedAuthZ {
	m := make(map[string]bool, len(deny))
	for _, d := range deny {
		m[d] = true
	}
	return &scopedAuthZ{denied: m}
}

func (a *scopedAuthZ) CheckAllowed(_ context.Context, principalID, _, actionType string) error {
	key := principalID + "|" + actionType
	a.calls = append(a.calls, key)
	if a.denied[key] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func (a *scopedAuthZ) called(principal, action string) bool {
	for _, c := range a.calls {
		if c == principal+"|"+action {
			return true
		}
	}
	return false
}

// tenantInjector stands in for the TenantContext middleware, which reads
// X-Tenant-Id off the request.
func tenantInjector(tenantID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), tenantID)))
		})
	}
}

// newRouterAuthz mirrors newRouter but takes the scoped authz double.
func newRouterAuthz(s *stubStore, pub *stubPublisher, authz handler.AuthZClient) chi.Router {
	r := chi.NewRouter()
	r.Use(tenantInjector("tenant-abc"))
	h := handler.New(s, pub, authz, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// newRouterNoTenant installs no tenant at all — the shape of a request that
// reached the service without X-Tenant-Id.
func newRouterNoTenant(s *stubStore) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func body(delegator, delegate, correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id":        "le-us",
		"delegator_principal_id": delegator,
		"delegate_principal_id":  delegate,
		"action_type":            "PAYMENT_APPROVE",
		"effective_from":         time.Now().UTC(),
		"effective_to":           time.Now().UTC().Add(24 * time.Hour),
		"correlation_id":         correlationID,
	}
}

// ── GAP 1: self-elevation via a delegator named in the request body ───────────

// A principal holding DELEGATION_CREATE — and nothing else — used to be able
// to name a colleague as delegator and themselves as delegate, and so mint
// themselves that colleague's authority. Both checks that existed passed.
func TestCreate_CannotDelegateSomeoneElsesAuthorityToYourself(t *testing.T) {
	az := newScopedAuthZ("attacker|DELEGATION_ADMINISTER")
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, az)

	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("cfo-alice", "attacker", uuid.NewString()), "attacker")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("naming another principal as delegator and yourself as delegate must be refused, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "self_dealing") {
		t.Fatalf("expected self_dealing, got %s", rr.Body.String())
	}
	// The delegator's authority must never even be consulted: the request is
	// refused on the caller/delegator relationship, before it matters whether
	// Alice actually holds PAYMENT_APPROVE.
	if az.called("cfo-alice", "PAYMENT_APPROVE") {
		t.Fatal("must not check the delegator's authority for a self-dealing request")
	}
}

// Even WITH the administer grant, routing another principal's authority to
// yourself is the same escalation by a longer route.
func TestCreate_AdministratorStillCannotNameThemselvesDelegate(t *testing.T) {
	az := newScopedAuthZ() // grants everything, including DELEGATION_ADMINISTER
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, az)

	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("cfo-alice", "admin-1", uuid.NewString()), "admin-1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-dealing even with administer, got %d: %s", rr.Code, rr.Body.String())
	}
}

// Naming someone else as delegator without the administer grant is refused,
// even when the delegate is a third party.
func TestCreate_OnBehalfOfRequiresAdministerGrant(t *testing.T) {
	az := newScopedAuthZ("clerk|DELEGATION_ADMINISTER")
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, az)

	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("cfo-alice", "bob", uuid.NewString()), "clerk")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "delegator_mismatch") {
		t.Fatalf("expected delegator_mismatch, got %s", rr.Body.String())
	}
}

// The feature still works: an administrator may set up a delegation between
// two other people. Closing the hole must not close this.
func TestCreate_AdministratorMayDelegateBetweenOtherPrincipals(t *testing.T) {
	az := newScopedAuthZ()
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, az)

	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("cfo-alice", "bob", uuid.NewString()), "admin-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	if !az.called("admin-1", "DELEGATION_ADMINISTER") {
		t.Fatal("expected the administer grant to be checked")
	}
}

// And the normal path — delegating your OWN authority — needs no administer
// grant at all.
func TestCreate_DelegatingYourOwnAuthorityNeedsNoAdministerGrant(t *testing.T) {
	az := newScopedAuthZ("cfo-alice|DELEGATION_ADMINISTER")
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, az)

	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("cfo-alice", "bob", uuid.NewString()), "cfo-alice")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	if az.called("cfo-alice", "DELEGATION_ADMINISTER") {
		t.Fatal("delegating your own authority must not require the administer grant")
	}
}

func TestCreate_DelegateMayNotBeTheDelegator(t *testing.T) {
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, newScopedAuthZ())
	rr := doReq(r, http.MethodPost, "/v1/delegations/",
		body("same-principal", "same-principal", uuid.NewString()), "same-principal")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── GAP 2: an unscoped read returned the tenant's whole register ─────────────

// seedTwo puts two delegations in the store: one the caller is party to, one
// between two other people entirely.
func seedTwo(t *testing.T, s *stubStore) {
	t.Helper()
	now := time.Now().UTC()
	mine := &domain.DelegationGrant{
		DelegationID: "d-mine", TenantID: "tenant-abc", LegalEntityID: "le-us",
		DelegatorPrincipalID: "cfo-alice", DelegatePrincipalID: "me",
		ActionType: "PAYMENT_APPROVE", Status: domain.DelegationStatusActive,
		EffectiveFrom: now, EffectiveTo: now.Add(24 * time.Hour), CorrelationID: "c1",
	}
	theirs := &domain.DelegationGrant{
		DelegationID: "d-theirs", TenantID: "tenant-abc", LegalEntityID: "le-us",
		DelegatorPrincipalID: "ceo-bob", DelegatePrincipalID: "cto-carol",
		ActionType: "CONTRACT_SIGN", Status: domain.DelegationStatusActive,
		EffectiveFrom: now, EffectiveTo: now.Add(24 * time.Hour), CorrelationID: "c2",
	}
	s.byID[mine.DelegationID] = mine
	s.byID[theirs.DelegationID] = theirs
}

func decodeList(t *testing.T, rr *httptest.ResponseRecorder) []domain.DelegationGrant {
	t.Helper()
	var out []domain.DelegationGrant
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rr.Body.String())
	}
	return out
}

// The headline read fix. Without a legal entity the answer is the caller's own
// delegations — not everyone's. This used to skip authorization entirely and
// return the tenant's complete map of who may act for whom.
func TestList_UnscopedReadReturnsOnlyYourOwnDelegations(t *testing.T) {
	s := newStubStore()
	seedTwo(t, s)
	r := newRouterAuthz(s, &stubPublisher{}, newScopedAuthZ())

	rr := doReq(r, http.MethodGet, "/v1/delegations/", nil, "me")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	list := decodeList(t, rr)
	if len(list) != 1 || list[0].DelegationID != "d-mine" {
		t.Fatalf("an unscoped read must return only the caller's own delegations, got %+v", list)
	}
}

// Asking after somebody else's delegations without an entity scope is refused,
// rather than quietly narrowed to the caller (which would report "none" and
// read as an answer).
func TestList_UnscopedReadOfAnotherPrincipalIsForbidden(t *testing.T) {
	s := newStubStore()
	seedTwo(t, s)
	r := newRouterAuthz(s, &stubPublisher{}, newScopedAuthZ())

	rr := doReq(r, http.MethodGet, "/v1/delegations/?delegate_principal_id=cto-carol", nil, "me")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

// With an entity scope the grant is required and actually checked.
func TestList_EntityScopedReadRequiresViewGrant(t *testing.T) {
	s := newStubStore()
	seedTwo(t, s)
	az := newScopedAuthZ("nosy|DELEGATION_VIEW")
	r := newRouterAuthz(s, &stubPublisher{}, az)

	rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us", nil, "nosy")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if !az.called("nosy", "DELEGATION_VIEW") {
		t.Fatal("expected DELEGATION_VIEW to be checked")
	}
}

func TestList_EntityScopedReadWithGrantSeesTheWholeEntity(t *testing.T) {
	s := newStubStore()
	seedTwo(t, s)
	r := newRouterAuthz(s, &stubPublisher{}, newScopedAuthZ())

	rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us", nil, "auditor")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if got := len(decodeList(t, rr)); got != 2 {
		t.Fatalf("an authorized entity read should see both rows, got %d", got)
	}
}

// ── GAP 3: a misspelled status filter answered "no delegations" ──────────────

func TestList_UnknownStatusIsRejected(t *testing.T) {
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, newScopedAuthZ())
	rr := doReq(r, http.MethodGet, "/v1/delegations/?status=ACTIVEE", nil, "me")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown_status") {
		t.Fatalf("expected unknown_status, got %s", rr.Body.String())
	}
}

// ── GAP 4: paging ────────────────────────────────────────────────────────────

func TestList_PagingValidation(t *testing.T) {
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, newScopedAuthZ())
	for _, q := range []string{"limit=abc", "limit=0", "limit=501", "offset=-1"} {
		rr := doReq(r, http.MethodGet, "/v1/delegations/?"+q, nil, "me")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400 got %d", q, rr.Code)
		}
	}
}

func TestList_LimitIsApplied(t *testing.T) {
	s := newStubStore()
	seedTwo(t, s)
	r := newRouterAuthz(s, &stubPublisher{}, newScopedAuthZ())

	rr := doReq(r, http.MethodGet, "/v1/delegations/?legal_entity_id=le-us&limit=1", nil, "auditor")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if got := len(decodeList(t, rr)); got != 1 {
		t.Fatalf("limit=1 should return one row, got %d", got)
	}
}

// ── GAP 5: a forgotten tenant header read as a database outage ───────────────

func TestRoutes_MissingTenantIs401NotServiceUnavailable(t *testing.T) {
	r := newRouterNoTenant(newStubStore())
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/delegations/", nil},
		{http.MethodGet, "/v1/delegations/some-id", nil},
		{http.MethodPost, "/v1/delegations/", body("a", "b", uuid.NewString())},
		{http.MethodPost, "/v1/delegations/some-id/revoke", nil},
	}
	for _, c := range cases {
		rr := doReq(r, c.method, c.path, c.body, "me")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 got %d: %s", c.method, c.path, rr.Code, rr.Body.String())
		}
	}
}

// ── GAP 6: an unknown field was discarded in silence ─────────────────────────

func TestCreate_UnknownFieldIsRejected(t *testing.T) {
	r := newRouterAuthz(newStubStore(), &stubPublisher{}, newScopedAuthZ())
	b := body("cfo-alice", "bob", uuid.NewString())
	b["effective_untill"] = time.Now().UTC().Add(72 * time.Hour) // misspelled
	rr := doReq(r, http.MethodPost, "/v1/delegations/", b, "cfo-alice")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}
