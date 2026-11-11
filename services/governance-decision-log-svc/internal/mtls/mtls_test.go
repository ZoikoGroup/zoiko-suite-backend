package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCA issues a CA and one leaf for cn, PEM-encoded.
func testCA(t *testing.T, cn string) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn, "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

// Provisioning presents the bootstrap token and the envelope
// mtls-management-svc requires — the first version sent neither.
func TestProvision_PresentsBootstrapTokenAndEnvelope(t *testing.T) {
	ca, cert, key := testCA(t, "governance-decision-log-svc")
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"certificate": map[string]string{"certificate_pem": string(cert)}, "private_key_pem": string(key), "ca_certificate_pem": string(ca)})
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Provision(t.Context(), srv.URL, "governance-decision-log-svc", "scope-1", LoadBootstrapToken(path))
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("X-Mtls-Bootstrap-Token") != "tok-123" || got.Header.Get("X-Workload-Id") != "governance-decision-log-svc" ||
		got.Header.Get("Idempotency-Key") == "" || got.Header.Get("X-Tenant-Id") != "scope-1" {
		t.Fatalf("provision request headers: %v", got.Header)
	}
	if _, err := NewRotatingIdentity(id); err != nil {
		t.Fatal(err)
	}
}

// The server half requires a client certificate from the platform CA, and a
// client presenting one is verified end to end (by CA and host name).
func TestServerRequiresAVerifiedClientCertificate(t *testing.T) {
	ca, cert, key := testCA(t, "localhost")
	ri, err := NewRotatingIdentity(&Identity{CertPEM: cert, KeyPEM: key, CACertPEM: ca})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.TLS.VerifiedChains[0][0].Subject.CommonName))
	}))
	srv.TLS = ri.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	resp, err := ri.HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("a client with a platform certificate must be admitted: %v", err)
	}
	_ = resp.Body.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if resp, err := anonymous.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a client without a certificate must be refused at the handshake")
	}
}

func TestLoadBootstrapToken_MissingIsEmpty(t *testing.T) {
	if LoadBootstrapToken("") != "" || LoadBootstrapToken(filepath.Join(t.TempDir(), "absent")) != "" {
		t.Fatal("an unset or missing token file must read as empty")
	}
}
