package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	"zoiko.io/evidence-requirements-svc/internal/manifest"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	entity = "22222222-2222-2222-2222-222222222222"
	id     = "33333333-3333-4333-8333-333333333333"
)

// owner serves one GET of /v1/evidence-manifests/{id} with the given status and body, and
// remembers the request it was sent.
func owner(t *testing.T, status int, body map[string]string, seen **http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyManifest(t *testing.T) {
	inScope := map[string]string{"manifest_id": id, "tenant_id": tenant, "legal_entity_id": entity}
	otherTenant := map[string]string{"manifest_id": id, "tenant_id": "99999999-9999-9999-9999-999999999999", "legal_entity_id": entity}
	otherEntity := map[string]string{"manifest_id": id, "tenant_id": tenant, "legal_entity_id": "88888888-8888-8888-8888-888888888888"}
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]string
		want   error
	}{
		{"in scope counts", http.StatusOK, inScope, nil},
		{"another tenant does not count", http.StatusOK, otherTenant, domain.ErrManifestMismatch},
		{"another entity does not count", http.StatusOK, otherEntity, domain.ErrManifestMismatch},
		{"404 is not found", http.StatusNotFound, nil, domain.ErrManifestNotFound},
		{"an empty body is not found", http.StatusOK, map[string]string{}, domain.ErrManifestNotFound},
		{"403 is unavailable, never a pass", http.StatusForbidden, nil, domain.ErrManifestServiceUnavailable},
		{"500 is unavailable", http.StatusInternalServerError, nil, domain.ErrManifestServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen *http.Request
			srv := owner(t, tc.status, tc.body, &seen)
			err := manifest.NewHTTPClient(srv.URL, zap.NewNop()).VerifyManifest(context.Background(), tenant, entity, id, "principal-1")
			if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if seen.URL.Path != "/v1/evidence-manifests/"+id {
				t.Errorf("path = %s", seen.URL.Path)
			}
			if seen.Header.Get("X-Tenant-Id") != tenant || seen.Header.Get("X-Principal-Id") != "principal-1" {
				t.Errorf("scope headers not forwarded: %v", seen.Header)
			}
		})
	}
}

func TestVerifyManifest_UnreachableIsUnavailable(t *testing.T) {
	err := manifest.NewHTTPClient("http://127.0.0.1:1", zap.NewNop()).VerifyManifest(context.Background(), tenant, entity, id, "p")
	if !errors.Is(err, domain.ErrManifestServiceUnavailable) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

// The reference is caller input: it can only name one path segment.
func TestVerifyManifest_ReferenceCannotEscapeThePath(t *testing.T) {
	var seen *http.Request
	srv := owner(t, http.StatusNotFound, nil, &seen)
	_ = manifest.NewHTTPClient(srv.URL, zap.NewNop()).VerifyManifest(context.Background(), tenant, entity, "../../admin", "p")
	if seen.URL.EscapedPath() != "/v1/evidence-manifests/..%2F..%2Fadmin" {
		t.Fatalf("escaped path = %s", seen.URL.EscapedPath())
	}
}
