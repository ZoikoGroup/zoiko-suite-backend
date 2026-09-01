package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/identity"
)

// These run against a real server for the reason internal/authz's doc comment
// records: a cross-service client that is only ever replaced by a double in
// handler tests has its parsing exercised by nothing. financial-close-svc
// shipped one that decoded a field authorization-svc has never sent, and it
// failed identically to a permission nobody had granted, from both sides.

func TestResolveEmail_ReadsTheAddressAndForwardsScope(t *testing.T) {
	var gotPath, gotTenant, gotPrincipal string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotPrincipal = r.Header.Get("X-Principal-Id")
		w.Header().Set("Content-Type", "application/json")
		// The shape identity-context-svc actually sends — its domain.Principal.
		_, _ = w.Write([]byte(`{
			"principal_id": "principal-2",
			"tenant_id": "tenant-abc",
			"principal_type": "HUMAN",
			"email": "employee@example.com",
			"display_name": "A Person",
			"status": "ACTIVE"
		}`))
	}))
	defer srv.Close()

	email, err := identity.NewClient(srv.URL, zap.NewNop()).
		ResolveEmail(context.Background(), "tenant-abc", "caller-1", "principal-2")
	if err != nil {
		t.Fatalf("ResolveEmail: %v", err)
	}
	if email != "employee@example.com" {
		t.Errorf("email = %q, want employee@example.com", email)
	}
	if gotPath != "/v1/principals/principal-2" {
		t.Errorf("path = %q, want /v1/principals/principal-2", gotPath)
	}
	if gotTenant != "tenant-abc" {
		t.Errorf("X-Tenant-Id = %q; identity-context-svc scopes the read by tenant and would refuse without it", gotTenant)
	}
	// The CALLER, not the recipient. Sending the recipient here would let any
	// caller read any principal's address simply by naming it as the recipient.
	if gotPrincipal != "caller-1" {
		t.Errorf("X-Principal-Id = %q, want the calling principal caller-1", gotPrincipal)
	}
}

// The distinction the rest of the service depends on: a settled answer
// concludes a notification, an unsettled one is an outage.

func TestResolveEmail_UnknownPrincipalIsSettled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := identity.NewClient(srv.URL, zap.NewNop()).
		ResolveEmail(context.Background(), "tenant-abc", "caller-1", "ghost")
	if !errors.Is(err, domain.ErrPrincipalNotFound) {
		t.Fatalf("err = %v, want ErrPrincipalNotFound", err)
	}
	if !identity.IsSettled(err) {
		t.Error("an unknown principal was treated as retryable; it will not appear by waiting")
	}
}

func TestResolveEmail_PrincipalWithoutAddressIsSettled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A 200 with a blank email. Not an outage: the principal exists and
		// has no address, and asking again returns the same answer.
		_, _ = w.Write([]byte(`{"principal_id":"principal-2","email":""}`))
	}))
	defer srv.Close()

	_, err := identity.NewClient(srv.URL, zap.NewNop()).
		ResolveEmail(context.Background(), "tenant-abc", "caller-1", "principal-2")
	if !errors.Is(err, domain.ErrPrincipalHasNoAddress) {
		t.Fatalf("err = %v, want ErrPrincipalHasNoAddress", err)
	}
	if !identity.IsSettled(err) {
		t.Error("a principal with no address on record was treated as retryable")
	}
}

func TestResolveEmail_UnreachableServiceIsNotSettled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // nothing is listening

	_, err := identity.NewClient(srv.URL, zap.NewNop()).
		ResolveEmail(context.Background(), "tenant-abc", "caller-1", "principal-2")
	if !errors.Is(err, domain.ErrIdentityServiceUnavailable) {
		t.Fatalf("err = %v, want ErrIdentityServiceUnavailable", err)
	}
	if identity.IsSettled(err) {
		t.Error("an unreachable identity service was recorded as a fact about the recipient; " +
			"every notification during the outage would be permanently FAILED")
	}
}

// 401/403 are this service's own misconfiguration. Treating them as "no
// address" would write a permanent FAILED for every notification sent while a
// header was wrong.
func TestResolveEmail_AuthFailureIsNotSettled(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		_, err := identity.NewClient(srv.URL, zap.NewNop()).
			ResolveEmail(context.Background(), "tenant-abc", "caller-1", "principal-2")
		if identity.IsSettled(err) {
			t.Errorf("status %d was treated as a settled fact about the recipient: %v", code, err)
		}
		srv.Close()
	}
}

func TestResolveEmail_RefusesAnUnscopedLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a lookup with no tenant scope reached the network")
	}))
	defer srv.Close()

	c := identity.NewClient(srv.URL, zap.NewNop())
	for _, tc := range []struct{ tenant, caller, recipient string }{
		{"", "caller-1", "principal-2"},
		{"tenant-abc", "", "principal-2"},
		{"tenant-abc", "caller-1", ""},
	} {
		if _, err := c.ResolveEmail(context.Background(), tc.tenant, tc.caller, tc.recipient); err == nil {
			t.Errorf("ResolveEmail(%q, %q, %q) was accepted", tc.tenant, tc.caller, tc.recipient)
		}
	}
}
