package handler_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func csrFor(t *testing.T, service string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("spiffe://zoiko/local/" + service)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: service}, DNSNames: []string{service}, URIs: []*url.URL{u}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func postCSR(t *testing.T, router http.Handler, token, service, csrService string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"csr": csrFor(t, csrService), "service_name": service, "legal_entity_id": "LE-1", "rotation_days": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates/csr", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	if token != "" {
		req.Header.Set("X-Mtls-Bootstrap-Token", token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// S4-C2: a service signs its own first certificate from a CSR, the private key never leaving it, and a bootstrap
// token buys only its own service's certificate.
func TestSignCSR_BootstrapTokenSignsOnlyItsOwnServiceAndReturnsNoKey(t *testing.T) {
	router := newRouterWithBootstrapToken(t, "shared-secret-123") // the token belongs to authorization-svc, authz denies everything else

	w := postCSR(t, router, "shared-secret-123", "authorization-svc", "authorization-svc")
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a CSR via bootstrap token with no principal, got %d: %s", w.Code, w.Body)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("PRIVATE KEY")) || bytes.Contains(w.Body.Bytes(), []byte(`"private_key_pem":"-`)) {
		t.Fatalf("a CSR response must carry no private key: %s", w.Body)
	}

	if w = postCSR(t, router, "shared-secret-123", "ledger-svc", "ledger-svc"); w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("bootstrap_identity_mismatch")) {
		t.Fatalf("a token must not sign another service: got %d: %s", w.Code, w.Body)
	}
	if w = postCSR(t, router, "wrong", "authorization-svc", "authorization-svc"); w.Code != http.StatusUnauthorized || !bytes.Contains(w.Body.Bytes(), []byte("invalid_bootstrap_token")) {
		t.Fatalf("an unknown token is refused: got %d: %s", w.Code, w.Body)
	}
	// Without a token the normal path applies: no principal, no certificate.
	if w = postCSR(t, router, "", "authorization-svc", "authorization-svc"); w.Code == http.StatusCreated {
		t.Fatalf("no token and no principal must not be signed: %s", w.Body)
	}
}
