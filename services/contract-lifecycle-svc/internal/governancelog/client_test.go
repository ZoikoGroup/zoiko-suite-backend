package governancelog

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
	"testing"
	"time"

	"zoiko.io/identity-envelope/mtlsclient"

	svcenvelope "zoiko.io/contract-lifecycle-svc/internal/envelope"
)

// pki issues a CA and leaves for the test listener and this service.
type pki struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	caPEM []byte
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "platform-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &pki{ca: ca, key: key, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (p *pki) leaf(t *testing.T, cn string, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{cn}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

// decisionLog is an mTLS listener like the decision log's :8450: it requires
// a platform client certificate and records what it was sent.
func decisionLog(t *testing.T, p *pki, status int, body any) (*httptest.Server, *http.Request, *string) {
	t.Helper()
	got := &http.Request{}
	peer := new(string)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		*peer = r.TLS.VerifiedChains[0][0].Subject.CommonName
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	certPEM, keyPEM := p.leaf(t, "governance-decision-log-svc", 10)
	cert, _ := tls.X509KeyPair(certPEM, keyPEM)
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, got, peer
}

func identity(t *testing.T, p *pki) *mtlsclient.Identity {
	t.Helper()
	certPEM, keyPEM := p.leaf(t, "contract-lifecycle-svc", 20)
	id := mtlsclient.New(mtlsclient.Options{ServiceName: "contract-lifecycle-svc"}, nil)
	if err := id.Load(certPEM, keyPEM, p.caPEM); err != nil {
		t.Fatal(err)
	}
	return id
}

// D2-2.F07: the read presents this service's certificate, names its own
// principal, and carries the full input envelope (D-04) with a read purpose.
func TestVerifyGranted_ReadsOverMTLSAsTheServicePrincipalWithTheFullEnvelope(t *testing.T) {
	p := newPKI(t)
	srv, got, peer := decisionLog(t, p, http.StatusOK, map[string]string{
		"decision_id": "dec-1", "legal_entity_id": "le-1", "action_type": "CONTRACT_APPROVE", "outcome": "GRANTED"})
	c := NewClientWithHTTPClient(srv.URL, identity(t, p).HTTPClient(5*time.Second), "svc-contract-lifecycle")

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{CorrelationID: "corr-activation"})
	if err := c.VerifyGranted(ctx, "tenant-1", "dec-1", "le-1", "CONTRACT_APPROVE"); err != nil {
		t.Fatalf("a granted decision must verify: %v", err)
	}
	if *peer != "contract-lifecycle-svc" {
		t.Fatalf("decision log saw peer %q", *peer)
	}
	h := got.Header
	for k, want := range map[string]string{
		"X-Tenant-Id": "tenant-1", "X-Principal-Id": "svc-contract-lifecycle", "X-Correlation-ID": "corr-activation",
		"X-Source-Channel": "system", "X-Purpose-Context": PurposeContext, "X-Legal-Entity-Id": "le-1",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	if h.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing")
	}
	if got.URL.Path != "/v1/decisions/dec-1" {
		t.Errorf("path %s", got.URL.Path)
	}
}

func TestVerifyGranted_FailsClosed(t *testing.T) {
	p := newPKI(t)
	cases := []struct {
		name   string
		status int
		body   any
		want   error
	}{
		{"denied decision", http.StatusOK, map[string]string{"legal_entity_id": "le-1", "action_type": "CONTRACT_APPROVE", "outcome": "DENIED"}, ErrDecisionNotGranted},
		{"other entity", http.StatusOK, map[string]string{"legal_entity_id": "le-2", "action_type": "CONTRACT_APPROVE", "outcome": "GRANTED"}, ErrDecisionNotGranted},
		{"not found", http.StatusNotFound, map[string]string{}, ErrDecisionNotFound},
		{"identity refused (401)", http.StatusUnauthorized, map[string]string{}, ErrServiceUnavailable},
		{"read not granted (403)", http.StatusForbidden, map[string]string{}, ErrServiceUnavailable},
	}
	for _, tc := range cases {
		srv, _, _ := decisionLog(t, p, tc.status, tc.body)
		c := NewClientWithHTTPClient(srv.URL, identity(t, p).HTTPClient(5*time.Second), "")
		if err := c.VerifyGranted(context.Background(), "t", "dec-1", "le-1", "CONTRACT_APPROVE"); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}

	// No client certificate: the handshake is refused, so activation is too.
	srv, _, _ := decisionLog(t, p, http.StatusOK, map[string]string{})
	if err := NewClient(srv.URL).VerifyGranted(context.Background(), "t", "dec-1", "le-1", "CONTRACT_APPROVE"); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("no certificate: got %v", err)
	}
}
