package mtlslistener

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

var serial atomic.Int64

func (ca *testCA) leaf(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial.Add(1) + 10), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

// fakeManagement is mtls-management-svc: down for the first `failures` calls,
// then issuing from ca. It checks the envelope the real service enforces.
func fakeManagement(t *testing.T, ca *testCA, failures int32) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for _, h := range []string{"X-Tenant-Id", "X-Workload-Id", "X-Request-Id", "X-Correlation-ID", "X-Source-Channel", "Idempotency-Key"} {
			if r.Header.Get(h) == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		var body struct {
			CommonName string `json:"common_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c, k := ca.leaf(t, body.CommonName)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"certificate":        map[string]string{"certificate_pem": string(c)},
			"private_key_pem":    string(k),
			"ca_certificate_pem": string(ca.pem),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func clientFor(t *testing.T, issuer, trusted *testCA, cn string) *http.Client {
	t.Helper()
	c, k := issuer.leaf(t, cn)
	pair, _ := tls.X509KeyPair(c, k)
	roots := x509.NewCertPool()
	roots.AddCert(trusted.cert)
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: "localhost"}}}
}

// SEC-INV-05: the listener comes up once mtls-management-svc answers (after
// failures), admits a listed client, names it on X-Workload-Id, and refuses an
// unlisted one and one from another CA.
func TestListener_ProvisionsWithRetryAndEnforcesClients(t *testing.T) {
	old := retryDelay
	retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { retryDelay = old })

	ca, rogue := newCA(t), newCA(t)
	mgmt, calls := fakeManagement(t, ca, 2)
	port := freePort(t)

	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "workload="+r.Header.Get("X-Workload-Id"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Start(ctx, Options{ServiceName: "identity-context-svc", Port: port, ManagementURL: mgmt.URL,
		AllowedClients: []string{"gateway-auth-svc"}}, app, zap.NewNop())
	select {
	case <-l.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("listener never came up")
	}
	if calls.Load() < 3 {
		t.Errorf("provision called %d times, want the two failures retried", calls.Load())
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/.well-known/jwks.json", port)
	time.Sleep(50 * time.Millisecond)

	resp, err := clientFor(t, ca, ca, "gateway-auth-svc").Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "workload=gateway-auth-svc" {
		t.Errorf("listed client: %d %q, want 200 and the verified name", resp.StatusCode, body)
	}

	resp, err = clientFor(t, ca, ca, "payroll-svc").Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unlisted client: %d, want 403", resp.StatusCode)
	}

	if resp, err := clientFor(t, rogue, ca, "gateway-auth-svc").Get(url); err == nil {
		resp.Body.Close()
		t.Error("a client certificate from another CA was accepted")
	}
	_ = l.Shutdown(context.Background())
}
