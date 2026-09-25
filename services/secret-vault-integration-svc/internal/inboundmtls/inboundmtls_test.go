package inboundmtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePEM(t *testing.T, path string, pemBytes []byte) {
	t.Helper()
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func testCerts(t *testing.T, dir string, cn string) (clientCertPath string) {
	t.Helper()

	// CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPath := filepath.Join(dir, "ca.pem")
	writePEM(t, caPath, caPEM)
	_ = caPath

	// Leaf.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafPath := filepath.Join(dir, "leaf.pem")
	writePEM(t, leafPath, leafPEM)

	// Server keypair (for NewServerTLSConfig).
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "svc-cert"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	serverCertPath := filepath.Join(dir, "server-cert.pem")
	serverKeyPath := filepath.Join(dir, "server-key.pem")
	writePEM(t, serverCertPath, certPEM)
	writePEM(t, serverKeyPath, keyPEM)

	// Put the leaf key next to the leaf cert to build a client tls.Certificate.
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})
	writePEM(t, filepath.Join(dir, "leaf-key.pem"), leafKeyPEM)

	return leafPath
}

func TestNewServerTLSConfig_SetsMutualAuthWhenCAProvided(t *testing.T) {
	dir := t.TempDir()
	leafPath := testCerts(t, dir, "svc-a")

	conf, err := NewServerTLSConfig(filepath.Join(dir, "server-cert.pem"), filepath.Join(dir, "server-key.pem"), filepath.Join(dir, "ca.pem"), nil)
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}
	if conf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", conf.ClientAuth)
	}
	if len(conf.Certificates) != 1 {
		t.Fatalf("expected server keypair, got %d certs", len(conf.Certificates))
	}
	_ = leafPath

	plain, err := NewServerTLSConfig(filepath.Join(dir, "server-cert.pem"), filepath.Join(dir, "server-key.pem"), "", nil)
	if err != nil {
		t.Fatalf("NewServerTLSConfig(no ca): %v", err)
	}
	if plain.ClientAuth != tls.NoClientCert {
		t.Fatalf("ClientAuth = %v, want NoClientCert for plain TLS", plain.ClientAuth)
	}
}

// requestWithCert simulates an mTLS-terminated request carrying the leaf cert
// and identity headers.
func requestWithCert(t *testing.T, certFile string, identityHeader, identityValue string) *http.Request {
	t.Helper()
	raw, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("no PEM in cert file")
	}
	certSlice, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/audit", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certSlice}}
	if identityHeader != "" {
		req.Header.Set(identityHeader, identityValue)
	}
	return req
}

func TestIdentityCheck_MatchingCertPasses(t *testing.T) {
	dir := t.TempDir()
	certFile := testCerts(t, dir, "workload-broker")
	req := requestWithCert(t, certFile, "X-Workload-Id", "workload-broker")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	w := httptest.NewRecorder()
	IdentityCheck(true, nil)(next).ServeHTTP(w, req)
	if !called {
		t.Fatal("matching cert must be allowed through")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestIdentityCheck_MismatchRefused(t *testing.T) {
	dir := t.TempDir()
	certFile := testCerts(t, dir, "workload-broker")
	// Header claims ANOTHER workload — exactly the spoof Gap 2 closes.
	req := requestWithCert(t, certFile, "X-Workload-Id", "workload-attacker")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	w := httptest.NewRecorder()
	IdentityCheck(true, nil)(next).ServeHTTP(w, req)
	if called {
		t.Fatal("mismatched cert must NOT reach the handler")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestIdentityCheck_DisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	certFile := testCerts(t, dir, "workload-broker")
	req := requestWithCert(t, certFile, "X-Workload-Id", "workload-attacker")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	w := httptest.NewRecorder()
	IdentityCheck(false, nil)(next).ServeHTTP(w, req)
	if !called {
		t.Fatal("disabled identity check must be a transparent no-op")
	}
}

func TestIdentityCheck_NoCertLetsThroughWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	_ = testCerts(t, dir, "workload-broker")
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets/audit", nil)
	req.Header.Set("X-Workload-Id", "workload-broker")

	// No client certificate is visible — this is the proxy-terminated-TLS case,
	// where the mTLS handshake that WOULD have enforced a cert happens at the
	// gateway. The middleware cannot cross-check a cert that isn't there, so it
	// defers to the verified identity header.
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	w := httptest.NewRecorder()
	IdentityCheck(true, nil)(next).ServeHTTP(w, req)
	if !called {
		t.Fatal("cert-less request with a verified identity header should pass; the handshake gate decides whether bare requests are acceptable")
	}
}

func TestIdentityCheck_CertButNoClaimRefusedWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	certFile := testCerts(t, dir, "workload-broker")
	// A client certificate is present but there is no gateway-verified identity
	// header at all — there is nothing to cross-check against.
	req := requestWithCert(t, certFile, "", "")

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	w := httptest.NewRecorder()
	IdentityCheck(true, nil)(next).ServeHTTP(w, req)
	if called {
		t.Fatal("cert without a verified identity claim must be refused")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}