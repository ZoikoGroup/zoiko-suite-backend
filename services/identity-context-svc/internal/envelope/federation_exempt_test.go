package envelope_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/identity-context-svc/internal/envelope"
)

// A directory's SCIM calls and a person's sign-in carry no platform identity, so they must never be held to the
// envelope that identity produces. Every other route still is.
func TestFederatedLoginIsExemptFromTheEnvelopeLikePasswordLogin(t *testing.T) {
	p := envelope.ServicePolicy()
	have := map[string]bool{}
	for _, e := range p.ExemptPaths {
		have[e] = true
	}
	for _, path := range []string{"/v1/authenticate", "/v1/authenticate/federated"} {
		if !have[path] {
			t.Errorf("%s must be exempt: it is how an identity is obtained", path)
		}
	}
	// and nothing administrative is
	for _, path := range []string{"/v1/federation/connections", "/v1/federation/links", "/v1/context/resolve"} {
		if have[path] {
			t.Errorf("%s must keep the full contract", path)
		}
	}
}

func TestAdministrativeFederationRoutesStillRequireTheEnvelope(t *testing.T) {
	h := envelope.Middleware(envelope.ServicePolicy(), nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/v1/federation/connections", "/v1/federation/links"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusBadRequest {
			t.Errorf("POST %s with no envelope: %d (must be refused before the handler)", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/authenticate/federated", nil))
	if w.Code != http.StatusOK {
		t.Errorf("the federated login reaches its handler with no envelope: %d", w.Code)
	}
}
