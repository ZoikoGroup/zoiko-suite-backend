package handler_test

// DA-1 (tracker D1-8.F09, F10): keyset cursor paging on GET /v1/delegations
// and the ZS-IAM-001 alias /v1/iam/delegations.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/handler"
)

func seedMine(s *stubStore, ids ...string) {
	now := time.Now().UTC()
	for _, id := range ids {
		s.byID[id] = &domain.DelegationGrant{DelegationID: id, TenantID: "tenant-abc", LegalEntityID: "le-us",
			DelegatorPrincipalID: "cfo-alice", DelegatePrincipalID: "me", ActionType: "PAYMENT_APPROVE",
			Status: domain.DelegationStatusActive, EffectiveFrom: now, EffectiveTo: now.Add(24 * time.Hour),
			CorrelationID: "c-" + id, CreatedAt: now}
	}
}

func TestDA1_ListPagesByCursorAndRefusesForgedCursors(t *testing.T) {
	s := newStubStore()
	seedMine(s, "00000000-0000-0000-0000-000000000003", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000001")
	r := newRouter(s, &stubAuthZ{}, nil)

	first := doReq(r, http.MethodGet, "/v1/delegations/?limit=2", nil, "me", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	page1 := decodeList(t, first)
	next := first.Header().Get("X-Next-Cursor")
	if len(page1) != 2 || next == "" || !strings.Contains(first.Header().Get("Link"), `rel="next"`) {
		t.Fatalf("a full page carries the next cursor: %d rows, cursor %q, link %q", len(page1), next, first.Header().Get("Link"))
	}
	second := doReq(r, http.MethodGet, "/v1/delegations/?limit=2&cursor="+next, nil, "me", "")
	page2 := decodeList(t, second)
	if second.Code != http.StatusOK || len(page2) != 1 || page2[0].DelegationID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("second page: %d %+v", second.Code, page2)
	}
	if second.Header().Get("X-Next-Cursor") != "" {
		t.Fatal("the last page has no next cursor")
	}
	for name, q := range map[string]string{
		"forged":      "cursor=not-a-cursor",
		"not a uuid":  "cursor=MjAyNi0xMS0yNFQwMDowMDowMFp8bm90LWEtdXVpZA",
		"with offset": "offset=1&cursor=" + next,
		"with as_of":  "as_of=2026-11-24T00:00:00Z&cursor=" + next,
	} {
		if rr := doReq(r, http.MethodGet, "/v1/delegations/?"+q, nil, "me", ""); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", name, rr.Code)
		}
	}
}

// Every /v1/delegations route is also served at /v1/iam/delegations, by the
// same handler.
func TestDA1_IAMAliasServesEveryDelegationRoute(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{}, nil)
	routes := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	n := 0
	for rt := range routes {
		if strings.Contains(rt, " /v1/delegations") {
			n++
			alias := strings.Replace(rt, " /v1/delegations", " /v1/iam/delegations", 1)
			if !routes[alias] {
				t.Errorf("%s has no alias %s", rt, alias)
			}
		}
	}
	if n < 9 {
		t.Fatalf("only %d delegation routes walked", n)
	}
	if len(handler.DelegationRoutePrefixes) != 2 {
		t.Fatalf("prefixes: %v", handler.DelegationRoutePrefixes)
	}
	s := newStubStore()
	seedMine(s, "00000000-0000-0000-0000-000000000009")
	r = newRouter(s, &stubAuthZ{}, nil)
	a := doReq(r, http.MethodGet, "/v1/delegations/00000000-0000-0000-0000-000000000009", nil, "me", "")
	b := doReq(r, http.MethodGet, "/v1/iam/delegations/00000000-0000-0000-0000-000000000009", nil, "me", "")
	if a.Code != http.StatusOK || b.Code != http.StatusOK || a.Body.String() != b.Body.String() {
		t.Fatalf("alias answered differently: %d %d", a.Code, b.Code)
	}
}
