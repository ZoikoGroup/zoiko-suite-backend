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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// crlCA is a test CA that may sign CRLs (newTestCA's may not: no CRLSign).
func crlCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "crl-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) crl(t *testing.T, number int64, nextUpdate time.Time, revoked ...int64) []byte {
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

// serverHandshake runs one mutual-TLS handshake against ri's listener with a
// client certificate of the given serial, returning the SERVER's verdict.
func serverHandshake(t *testing.T, ri *RotatingIdentity, ca *testCA, clientSerial int64) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ri.TLSConfig())
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
	cc, ck := ca.leaf(t, "ai-governance-svc", clientSerial, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth)
	clientCert, _ := tls.X509KeyPair(cc, ck)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{clientCert}})
	if err == nil {
		_ = conn.Handshake()
		conn.Close()
	}
	return <-verdict
}

func revocationSetup(t *testing.T) (*testCA, *RotatingIdentity, *RevocationChecker) {
	t.Helper()
	ca := crlCA(t)
	ri, err := NewRotatingIdentity(ca.server(t, 10, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	rc := NewRevocationChecker("unused", ri.CACertificates)
	ri.SetPeerVerifier(rc.VerifyConnection)
	return ca, ri, rc
}

func TestRevocation_RevokedClientCertificateRefused(t *testing.T) {
	ca, ri, rc := revocationSetup(t)
	if err := rc.Install(ca.crl(t, 1, time.Now().Add(time.Hour)), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := serverHandshake(t, ri, ca, 41); err != nil {
		t.Fatalf("an unrevoked certificate was refused: %v", err)
	}
	if err := rc.Install(ca.crl(t, 2, time.Now().Add(time.Hour), 41), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := serverHandshake(t, ri, ca, 41); err == nil {
		t.Fatal("a revoked client certificate was admitted (M-03)")
	}
	if err := serverHandshake(t, ri, ca, 42); err != nil {
		t.Fatalf("revoking 41 refused 42: %v", err)
	}
}

// Fail closed: before the first CRL, and after the held one's NextUpdate,
// status is unknown and every client certificate is refused.
func TestRevocation_NoCurrentCRL_FailsClosed(t *testing.T) {
	ca, ri, rc := revocationSetup(t)
	if err := serverHandshake(t, ri, ca, 41); err == nil {
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
	// The held CRL ages past its NextUpdate with no refresh (CRL times are
	// whole seconds, so this moves the clock rather than racing it).
	rc.mu.Lock()
	rc.nextUpdate = time.Now().Add(-time.Second)
	rc.mu.Unlock()
	if err := rc.Check(&x509.Certificate{SerialNumber: big.NewInt(41)}); !errors.Is(err, ErrRevocationUnknown) {
		t.Fatalf("past NextUpdate: got %v, want ErrRevocationUnknown", err)
	}
}

func TestRevocation_ForgedOrRolledBackCRLRejected(t *testing.T) {
	ca, _, rc := revocationSetup(t)
	rogue := crlCA(t)
	if err := rc.Install(rogue.crl(t, 9, time.Now().Add(time.Hour)), time.Now()); err == nil {
		t.Fatal("a CRL signed by another CA was installed")
	}
	if err := rc.Install(ca.crl(t, 5, time.Now().Add(time.Hour), 41), time.Now()); err != nil {
		t.Fatal(err)
	}
	// An older CRL — from before 41 was revoked — must not un-revoke it.
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
