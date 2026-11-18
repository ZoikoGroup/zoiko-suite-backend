package mtlsclient

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
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pki struct {
	caPEM []byte
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "platform-ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &pki{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ca: ca, key: key}
}

// leaf issues a certificate for name, the way mtls-management-svc does
// (CN = DNS SAN = service name).
func (p *pki) leaf(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

// server is an mTLS server presenting a certificate for serverName and
// echoing the verified client CN.
func (p *pki) server(t *testing.T, serverName string) *httptest.Server {
	t.Helper()
	c, k := p.leaf(t, serverName)
	cert, _ := tls.X509KeyPair(c, k)
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.TLS.VerifiedChains[0][0].Subject.CommonName))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// dial points the client's URL at the test server under hostname, so the
// hostname check is exercised (the test server listens on 127.0.0.1).
func dial(srv *httptest.Server, hostname string) string {
	return strings.Replace(srv.URL, "127.0.0.1", hostname, 1)
}

func clientFor(t *testing.T, id *Identity, srv *httptest.Server) *http.Client {
	c := id.HTTPClient(5 * time.Second)
	tr := c.Transport.(*http.Transport)
	tr.DialContext = (&loopbackDialer{addr: strings.TrimPrefix(srv.URL, "https://")}).DialContext
	return c
}

func TestPresentsTheCertificateAndVerifiesTheServerByName(t *testing.T) {
	p := newPKI(t)
	srv := p.server(t, "governance-decision-log-svc")
	id := New(Options{ServiceName: "policy-svc"}, nil)
	c, k := p.leaf(t, "policy-svc")
	if err := id.Load(c, k, p.caPEM); err != nil {
		t.Fatal(err)
	}
	resp, err := clientFor(t, id, srv).Get(dial(srv, "governance-decision-log-svc"))
	if err != nil {
		t.Fatalf("a verified server must be reached: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b [64]byte
	n, _ := resp.Body.Read(b[:])
	if string(b[:n]) != "policy-svc" {
		t.Fatalf("server saw client %q, want policy-svc", string(b[:n]))
	}
}

func TestRefusesAServerWhoseCertificateNamesAnotherHost(t *testing.T) {
	p := newPKI(t)
	srv := p.server(t, "someone-else")
	id := New(Options{ServiceName: "policy-svc"}, nil)
	c, k := p.leaf(t, "policy-svc")
	_ = id.Load(c, k, p.caPEM)
	if resp, err := clientFor(t, id, srv).Get(dial(srv, "governance-decision-log-svc")); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server certificate for another name must be refused")
	}
}

func TestRefusesAServerFromAnotherCA(t *testing.T) {
	srv := newPKI(t).server(t, "governance-decision-log-svc")
	p := newPKI(t)
	id := New(Options{ServiceName: "policy-svc"}, nil)
	c, k := p.leaf(t, "policy-svc")
	_ = id.Load(c, k, p.caPEM)
	if resp, err := clientFor(t, id, srv).Get(dial(srv, "governance-decision-log-svc")); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server outside the platform CA must be refused")
	}
}

func TestBeforeProvisioningEveryCallFails(t *testing.T) {
	p := newPKI(t)
	srv := p.server(t, "governance-decision-log-svc")
	id := New(Options{ServiceName: "policy-svc"}, nil)
	if resp, err := clientFor(t, id, srv).Get(dial(srv, "governance-decision-log-svc")); err == nil {
		_ = resp.Body.Close()
		t.Fatal("no certificate yet: the call must fail (the caller's outbox retries)")
	}
}

func TestProvisionPresentsTheBootstrapToken(t *testing.T) {
	p := newPKI(t)
	c, k := p.leaf(t, "policy-svc")
	var gotToken, gotWorkload string
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotWorkload = r.Header.Get("X-Mtls-Bootstrap-Token"), r.Header.Get("X-Workload-Id")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"certificate": map[string]string{"certificate_pem": string(c)},
			"private_key_pem": string(k), "ca_certificate_pem": string(p.caPEM)})
	}))
	defer mgmt.Close()
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte("secret-token\n"), 0o600)

	id := New(Options{ServiceName: "policy-svc", ManagementURL: mgmt.URL, BootstrapTokenPath: path}, nil)
	is, err := id.provision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "secret-token" || gotWorkload != "policy-svc" {
		t.Fatalf("token %q workload %q", gotToken, gotWorkload)
	}
	if err := id.Load(is.certPEM, is.keyPEM, is.caPEM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-id.Ready():
	default:
		t.Fatal("Ready must close once a certificate is installed")
	}
	if id.renewAt().IsZero() || !id.renewAt().After(time.Now()) {
		t.Fatal("renewal is scheduled before expiry, after now")
	}
}

func TestProvisionRefusalIsAnError(t *testing.T) {
	mgmt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer mgmt.Close()
	_, err := New(Options{ServiceName: "x", ManagementURL: mgmt.URL}, nil).provision(t.Context())
	if err == nil || errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("expected a provisioning error, got %v", err)
	}
}

// loopbackDialer connects every dial to the test server, so a URL can carry
// the production hostname and the certificate check still sees it.
type loopbackDialer struct{ addr string }

func (d *loopbackDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.addr)
}
