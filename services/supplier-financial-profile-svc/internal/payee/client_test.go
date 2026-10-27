package payee

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestHTTPClient_GetActiveDestination(t *testing.T) {
	var gotPath, gotTenant, gotPrincipal string
	status := http.StatusOK
	body := `{"DestinationID":"d-1","LegalEntityID":"le-1","PartyRef":"sup","Status":"ACTIVE","UpdatedAt":"2026-05-04T03:02:01Z"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant, gotPrincipal = r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())
	ctx := context.Background()

	d, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-1", "sup")
	if err != nil || d.DestinationID != "d-1" || d.UpdatedAt.IsZero() {
		t.Fatalf("got %v %+v", err, d)
	}
	if gotPath != "/org10/parties/sup/active" || gotTenant != "t-1" || gotPrincipal != "p-1" {
		t.Fatalf("wrong request: %s tenant=%q principal=%q", gotPath, gotTenant, gotPrincipal)
	}

	// Another legal entity's destination is not usable.
	if _, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-OTHER", "sup"); !errors.Is(err, ErrNoActiveDestination) {
		t.Fatalf("expected ErrNoActiveDestination, got %v", err)
	}
	// 404 -> no active destination (an answer, not a failure).
	status, body = http.StatusNotFound, `{}`
	if _, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-1", "sup"); !errors.Is(err, ErrNoActiveDestination) {
		t.Fatalf("expected ErrNoActiveDestination, got %v", err)
	}
	// Anything else fails closed.
	status = http.StatusInternalServerError
	if _, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-1", "sup"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	status, body = http.StatusOK, `not json`
	if _, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-1", "sup"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on a malformed body, got %v", err)
	}
	// Unreachable -> fail closed.
	srv.Close()
	if _, err := c.GetActiveDestination(ctx, "t-1", "p-1", "le-1", "sup"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable when ORG-10 is down, got %v", err)
	}
}
