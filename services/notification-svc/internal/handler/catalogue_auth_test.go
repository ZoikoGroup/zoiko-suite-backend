package handler_test

import (
	"net/http"
	"testing"
)

// The template catalogue documented itself as authenticated and was not: the
// envelope middleware is write-strict, so a GET with no identity at all was
// admitted and answered 200 (measured live 2026-09-22). The handler now checks
// principal and tenant itself.
func TestListTemplates_RequiresPrincipalAndTenant(t *testing.T) {
	anonymous := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if rr := doReq(anonymous, http.MethodGet, "/v1/notifications/templates", nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: want 401, got %d: %s", rr.Code, rr.Body.String())
	}

	noTenant := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true}, "")
	if rr := doReq(noTenant, http.MethodGet, "/v1/notifications/templates", nil, "principal-1"); rr.Code == http.StatusOK {
		t.Fatalf("no tenant: want a refusal, got 200: %s", rr.Body.String())
	}

	full := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if rr := doReq(full, http.MethodGet, "/v1/notifications/templates", nil, "principal-1"); rr.Code != http.StatusOK {
		t.Fatalf("principal and tenant: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
