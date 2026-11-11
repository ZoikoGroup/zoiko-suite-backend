package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// hardCA is a CA that may also sign CRLs.
type hardCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newHardCA(t *testing.T) *hardCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "platform-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &hardCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *hardCA) identity(t *testing.T, cn string, serial int64, notBefore, notAfter time.Time) *Identity {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{cn, "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return &Identity{
		CertPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:    pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}),
		CACertPEM: ca.pem,
	}
}

func (ca *hardCA) crl(t *testing.T, number int64, nextUpdate time.Time, revoked ...int64) []byte {
	t.Helper()
	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for _, s := range revoked {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: big.NewInt(s), RevocationTime: time.Now()})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(number), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: nextUpdate,
		RevokedCertificateEntries: entries,
	}, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// handshake runs one mutual-TLS handshake against ri's listener as a client
// holding `client`, returning the server's verdict and the serial it presented.
func handshake(t *testing.T, ri *RotatingIdentity, client *Identity) (int64, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ri.ServerTLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	verdict := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			verdict <- err
			return
		}
		defer c.Close()
		verdict <- c.(*tls.Conn).Handshake()
	}()
	cert, err := tls.X509KeyPair(client.CertPEM, client.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(client.CACertPEM)
	var served int64
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}})
	if err == nil {
		_ = conn.Handshake()
		if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
			served = pc[0].SerialNumber.Int64()
		}
		conn.Close()
	}
	return served, <-verdict
}

// The listener reads its certificate per handshake: an Update is served on the
// next connection with no restart (a frozen tls.Config expired on day 91).
func TestRotation_ListenerServesTheRenewedCertificate(t *testing.T) {
	ca := newHardCA(t)
	ri, err := NewRotatingIdentity(ca.identity(t, "governance-decision-log-svc", 10, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	client := ca.identity(t, "policy-svc", 50, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if s, err := handshake(t, ri, client); err != nil || s != 10 {
		t.Fatalf("first handshake: serial %d, %v", s, err)
	}
	if err := ri.Update(ca.identity(t, "governance-decision-log-svc", 11, time.Now().Add(-time.Minute), time.Now().Add(2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if s, err := handshake(t, ri, client); err != nil || s != 11 {
		t.Fatalf("after renewal the listener must present serial 11, got %d (%v)", s, err)
	}
}

// RenewAt is two thirds through the validity; Run renews then, and a failed
// renewal is retried while the current certificate keeps serving.
func TestRotation_RunRenewsAtTwoThirdsAndRetriesFailures(t *testing.T) {
	ca := newHardCA(t)
	nb := time.Now().Add(-2 * time.Hour)
	ri, err := NewRotatingIdentity(ca.identity(t, "governance-decision-log-svc", 10, nb, nb.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	// Certificate times are whole seconds: compare within two.
	if got, want := ri.RenewAt(), nb.Add(2*time.Hour); got.Sub(want).Abs() > 2*time.Second {
		t.Fatalf("RenewAt = %s, want %s", got, want)
	}
	orig := renewRetry
	renewRetry = func(int) time.Duration { return time.Millisecond }
	defer func() { renewRetry = orig }()

	var calls atomic.Int32
	renewed := make(chan struct{})
	provision := func(context.Context) (*Identity, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("mtls-management-svc unavailable")
		}
		defer close(renewed)
		return ca.identity(t, "governance-decision-log-svc", 12, time.Now().Add(-time.Minute), time.Now().Add(90*24*time.Hour)), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ri.Run(ctx, provision, zap.NewNop())
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("the certificate past its renewal point was not renewed")
	}
	cancel()
	if calls.Load() < 2 {
		t.Fatalf("a failed renewal must be retried; provision called %d times", calls.Load())
	}
	deadline := time.Now().Add(time.Second)
	for ri.NotAfter().Before(time.Now().Add(80*24*time.Hour)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ri.NotAfter().Before(time.Now().Add(80 * 24 * time.Hour)) {
		t.Fatal("the renewed certificate was not installed")
	}
}

// The client half verifies the server by the platform CA AND the dialled name:
// a platform certificate issued to another service is refused.
func TestRotation_ClientVerifiesServerName(t *testing.T) {
	ca := newHardCA(t)
	me, _ := NewRotatingIdentity(ca.identity(t, "governance-decision-log-svc", 10, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	server, _ := NewRotatingIdentity(ca.identity(t, "authorization-svc", 20, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.TLS = server.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()

	// 127.0.0.1 is on the certificate: admitted.
	resp, err := me.HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("server on its own name must verify: %v", err)
	}
	_ = resp.Body.Close()

	// The same server reached under a name it was not issued for: refused.
	c := me.HTTPClient()
	c.Transport.(*http.Transport).TLSClientConfig.ServerName = "policy-svc"
	if resp, err := c.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server certificate not issued for the dialled name was accepted")
	}

	// A server from another CA: refused.
	rogue := newHardCA(t)
	other, _ := NewRotatingIdentity(rogue.identity(t, "localhost", 30, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	srv2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv2.TLS = other.ServerTLSConfig()
	srv2.StartTLS()
	defer srv2.Close()
	if resp, err := me.HTTPClient().Get(srv2.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a server certificate from another CA was accepted")
	}
}

func revocationSetup(t *testing.T) (*hardCA, *RotatingIdentity, *RevocationChecker) {
	t.Helper()
	ca := newHardCA(t)
	ri, err := NewRotatingIdentity(ca.identity(t, "governance-decision-log-svc", 10, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRevocationChecker("unused", ri.CACertificates)
	ri.SetPeerVerifier(rc.VerifyConnection)
	return ca, ri, rc
}

func TestRevocation_RevokedPeerCertificateRefusedAtTheHandshake(t *testing.T) {
	ca, ri, rc := revocationSetup(t)
	policy := ca.identity(t, "policy-svc", 41, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	workflow := ca.identity(t, "workflow-svc", 42, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err := rc.Install(ca.crl(t, 1, time.Now().Add(time.Hour)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := handshake(t, ri, policy); err != nil {
		t.Fatalf("an unrevoked peer was refused: %v", err)
	}
	if err := rc.Install(ca.crl(t, 2, time.Now().Add(time.Hour), 41), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := handshake(t, ri, policy); err == nil {
		t.Fatal("a revoked peer certificate was admitted to :8450")
	}
	if _, err := handshake(t, ri, workflow); err != nil {
		t.Fatalf("revoking 41 refused 42: %v", err)
	}
}

// Fail closed: before the first CRL and after the held one's NextUpdate every
// client certificate is refused.
func TestRevocation_NoCurrentCRL_FailsClosed(t *testing.T) {
	ca, ri, rc := revocationSetup(t)
	policy := ca.identity(t, "policy-svc", 41, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if _, err := handshake(t, ri, policy); err == nil {
		t.Fatal("a client certificate was admitted with no CRL held")
	}
	if err := rc.Install(ca.crl(t, 1, time.Now().Add(-time.Second)), time.Now()); err == nil {
		t.Fatal("an expired CRL was installed")
	}
	if err := rc.Install(ca.crl(t, 1, time.Now().Add(time.Hour)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := rc.Check(&x509.Certificate{SerialNumber: big.NewInt(41)}); err != nil {
		t.Fatalf("current CRL: %v", err)
	}
	rc.mu.Lock()
	rc.nextUpdate = time.Now().Add(-time.Second)
	rc.mu.Unlock()
	if err := rc.Check(&x509.Certificate{SerialNumber: big.NewInt(41)}); !errors.Is(err, ErrRevocationUnknown) {
		t.Fatalf("past NextUpdate: got %v, want ErrRevocationUnknown", err)
	}
}

func TestRevocation_ForgedOrRolledBackCRLRejected(t *testing.T) {
	ca, _, rc := revocationSetup(t)
	if err := rc.Install(newHardCA(t).crl(t, 9, time.Now().Add(time.Hour)), time.Now()); err == nil {
		t.Fatal("a CRL signed by another CA was installed")
	}
	if err := rc.Install(ca.crl(t, 5, time.Now().Add(time.Hour), 41), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := rc.Install(ca.crl(t, 4, time.Now().Add(time.Hour)), time.Now()); err == nil {
		t.Fatal("an older CRL replaced a newer one")
	}
	if err := rc.Check(&x509.Certificate{SerialNumber: big.NewInt(41)}); err == nil {
		t.Fatal("rollback un-revoked serial 41")
	}
}

func TestRevocation_RefreshFetchesFromManagementService(t *testing.T) {
	ca, _, rc := revocationSetup(t)
	der := ca.crl(t, 1, time.Now().Add(time.Hour), 77)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mtls/crl" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pkix-crl")
		_, _ = w.Write(der)
	}))
	defer srv.Close()
	rc.url = srv.URL + "/v1/mtls/crl"
	if err := rc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rc.Check(&x509.Certificate{SerialNumber: big.NewInt(77)}); err == nil {
		t.Fatal("fetched CRL not applied")
	}
}
