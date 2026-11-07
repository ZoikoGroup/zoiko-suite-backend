package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

func getStatus(t *testing.T, s *stubStore, target string, mutate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := authed(httptest.NewRequest(http.MethodGet, target, nil))
	if mutate != nil {
		req = mutate(req)
	}
	w := httptest.NewRecorder()
	newTestRouter(s, &stubVault{}, &stubPublisher{}).ServeHTTP(w, req)
	return w
}

// S3-B3: a path with an applicable policy answers 200 with metadata only; no policy is a 404; an outage is a 503.
func TestSecretPathStatus_AnswersWithMetadataOnly(t *testing.T) {
	av := &domain.ApplicableSecretPolicyVersion{SecretClass: "PROVIDER_CREDENTIAL"}
	av.SecretPolicyVersionID = "v-1"
	av.AllowedWorkloadIDs = json.RawMessage(`["secret-workload"]`)
	s := &stubStore{applicableByPath: av}
	w := getStatus(t, s, "/v1/secret-paths/status?secret_path=kv/db", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-workload") || strings.Contains(w.Body.String(), "allowed_workload") {
		t.Fatalf("the answer must not reveal who may use the secret: %s", w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["applicable"] != true || got["secret_class"] != "PROVIDER_CREDENTIAL" || got["secret_policy_version_id"] != "v-1" {
		t.Fatalf("%v", got)
	}

	s = &stubStore{applicableByPathErr: domain.ErrSecretPolicyNotFound}
	if w = getStatus(t, s, "/v1/secret-paths/status?secret_path=kv/none", nil); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "no_applicable_secret_policy") {
		t.Fatalf("deny-by-absence: %d %s", w.Code, w.Body.String())
	}
	s = &stubStore{applicableByPathErr: errors.New("db down")}
	if w = getStatus(t, s, "/v1/secret-paths/status?secret_path=kv/x", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("an outage is not an answer: %d", w.Code)
	}
}

func TestSecretPathStatus_RefusesBadRequestsAndForeignScope(t *testing.T) {
	s := &stubStore{applicableByPath: &domain.ApplicableSecretPolicyVersion{SecretClass: "X"}}
	if w := getStatus(t, s, "/v1/secret-paths/status", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("secret_path is required: %d", w.Code)
	}
	noPrincipal := func(r *http.Request) *http.Request { r.Header.Del("X-Principal-Id"); return r }
	if w := getStatus(t, s, "/v1/secret-paths/status?secret_path=kv/db", noPrincipal); w.Code != http.StatusUnauthorized {
		t.Fatalf("a caller must be identified: %d", w.Code)
	}
	if w := getStatus(t, s, "/v1/secret-paths/status?secret_path=kv/db&tenant_id=another-tenant", nil); w.Code == http.StatusOK {
		t.Fatalf("another tenant scope must be refused: %d", w.Code)
	}
}
