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
		got, err := NewClient(srv.URL).IsPublished(context.Background(), "tenant-1", "p1")
		srv.Close()
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// This is the integration state, pinned so it cannot be forgotten or hidden:
// the client forwards the caller's tenant and NOTHING identifying a caller. A
// registry that requires a principal on reads therefore refuses it with 401,
// which the client reports as "unavailable" — and RecordConsent turns that into
// 503 UPSTREAM_UNAVAILABLE. Making it work needs a service-to-service identity
// that the architecture names as a goal (docs/architecture/05-security.md
// §12.4 "traceable service-principal identity") but does not yet define as a
// mechanism; inventing one here is out of scope. The registry's authorization
// requirement must not be weakened to make this pass.
func TestIsPublished_AgainstARegistryThatRequiresAPrincipal_IsRefused(t *testing.T) {
	var seen http.Header
	srv := registry(t, true, http.StatusOK, &seen)
	defer srv.Close()

	ok, err := NewClient(srv.URL).IsPublished(context.Background(), "tenant-1", "p1")
	if ok || !errors.Is(err, ErrPurposeRegistryUnavailable) {
		t.Fatalf("expected the lookup to be refused (registry 401 -> unavailable), got (%v, %v)", ok, err)
	}
	if seen.Get("X-Tenant-Id") != "tenant-1" {
		t.Errorf("the verified tenant must still be forwarded, saw %q", seen.Get("X-Tenant-Id"))
	}
	if seen.Get("X-Principal-Id") != "" {
		t.Errorf("this client is documented as sending no principal; if that changed, update the dependency documentation")
	}
}
