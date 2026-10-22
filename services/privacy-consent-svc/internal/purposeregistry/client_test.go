package purposeregistry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// registry stands in for privacy-purpose-registry-svc's GET /privacy/purposes/{id}.
// requirePrincipal models the registry AS REMEDIATED: every read — including
// this one — answers 401 without X-Principal-Id.
func registry(t *testing.T, requirePrincipal bool, status int, seen *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		if requirePrincipal && r.Header.Get("X-Principal-Id") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
}

func TestIsPublished_MapsRegistryAnswers(t *testing.T) {
	var seen http.Header
	for _, tc := range []struct {
		name   string
		status int
		want   bool
		err    error
	}{
		{"published", http.StatusOK, true, nil},
		{"unknown or unpublished", http.StatusNotFound, false, nil},
		{"registry error", http.StatusInternalServerError, false, ErrPurposeRegistryUnavailable},
	} {
		srv := registry(t, false, tc.status, &seen)
		got, err := NewClient(srv.URL).IsPublished(context.Background(), "tenant-1", "principal-1", "p1")
		srv.Close()
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// The client forwards the caller's verified tenant AND principal, which is
// what a registry that requires an authenticated principal on reads (as the
// remediated PRV-01 does) needs. What remains outside this service is the
// registry-side GRANT for that principal (provisioned in authorization-svc),
// and a service-to-service identity for callers that have no end-user
// principal — docs/architecture/05-security.md §12.4 names it as a goal but
// defines no mechanism, so none is invented here.
func TestIsPublished_ForwardsTenantAndPrincipal(t *testing.T) {
	var seen http.Header
	srv := registry(t, true, http.StatusOK, &seen)
	defer srv.Close()

	ok, err := NewClient(srv.URL).IsPublished(context.Background(), "tenant-1", "principal-1", "p1")
	if !ok || err != nil {
		t.Fatalf("expected the lookup to succeed against a registry that requires a principal, got (%v, %v)", ok, err)
	}
	if seen.Get("X-Tenant-Id") != "tenant-1" || seen.Get("X-Principal-Id") != "principal-1" {
		t.Errorf("tenant and principal must be forwarded, saw tenant=%q principal=%q", seen.Get("X-Tenant-Id"), seen.Get("X-Principal-Id"))
	}
}

// With no principal at all the registry still refuses, and the client reports
// that as unavailable (fail closed) — never as "published".
func TestIsPublished_WithoutAPrincipal_IsRefusedByAnAuthenticatingRegistry(t *testing.T) {
	var seen http.Header
	srv := registry(t, true, http.StatusOK, &seen)
	defer srv.Close()
	ok, err := NewClient(srv.URL).IsPublished(context.Background(), "tenant-1", "", "p1")
	if ok || !errors.Is(err, ErrPurposeRegistryUnavailable) {
		t.Fatalf("expected a refusal, got (%v, %v)", ok, err)
	}
}
