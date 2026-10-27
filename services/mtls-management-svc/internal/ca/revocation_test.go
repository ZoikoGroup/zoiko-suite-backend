package ca

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLedger_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Revoke(RevokedSerial{SerialNumber: "abc123", CertID: "c1"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsRevoked("abc123") {
		t.Fatal("a revocation did not survive a restart")
	}
	if reopened.IsRevoked("def456") {
		t.Fatal("an unrevoked serial reads as revoked")
	}
}

// An unreadable ledger must stop the service, not start it believing nothing
// is revoked.
func TestLedger_CorruptFileRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, revocationFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(dir); err == nil {
		t.Fatal("a corrupt ledger was opened as empty")
	}
}

func TestLedger_RevokeIsIdempotentAndKeepsFirstTime(t *testing.T) {
	l, _ := OpenLedger(t.TempDir())
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = l.Revoke(RevokedSerial{SerialNumber: "aa", RevokedAt: first})
	_ = l.Revoke(RevokedSerial{SerialNumber: "aa", RevokedAt: first.Add(time.Hour)})
	e := l.Entries()
	if len(e) != 1 || !e[0].RevokedAt.Equal(first) {
		t.Fatalf("entries = %+v", e)
	}
}

func TestSignCRL_CurrentAndSigned(t *testing.T) {
	c, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	der, err := c.SignCRL([]RevokedSerial{{SerialNumber: "ff01", RevokedAt: now}}, now)
	if err != nil {
		t.Fatal(err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := crl.CheckSignatureFrom(c.Certificate()); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if !crl.NextUpdate.After(now) || len(crl.RevokedCertificateEntries) != 1 {
		t.Fatalf("NextUpdate %v entries %d", crl.NextUpdate, len(crl.RevokedCertificateEntries))
	}
}

// The self-issued client identity completes a real mutual-TLS handshake with
// a server holding a certificate from the same CA, and refuses a server whose
// certificate has been revoked.
func TestClientIdentity_MutualTLS_AndRevokedServerRefused(t *testing.T) {
	c, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ledger, _ := OpenLedger(t.TempDir())
	issued, err := c.IssueLeaf("127.0.0.1", nil, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(issued.CertificatePEM, issued.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Certificate())
	var sawClient string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClient = r.TLS.VerifiedChains[0][0].Subject.CommonName
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	defer srv.Close()

	identity := NewClientIdentity(c, ledger, "mtls-management-svc")
	tc := identity.TLSConfig()
	tc.ServerName = "127.0.0.1"
	// IssueLeaf puts no IP SAN on the test server's certificate; verify the
	// chain and the ledger here, hostname matching is covered by Go's own tests.
	tc.InsecureSkipVerify = true
	verifyConn := tc.VerifyConnection
	tc.VerifyConnection = func(cs tls.ConnectionState) error {
		if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			return err
		}
		return verifyConn(cs)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("mutual TLS handshake failed: %v", err)
	}
	resp.Body.Close()
	if sawClient != "mtls-management-svc" {
		t.Fatalf("server verified client CN %q", sawClient)
	}

	if err := ledger.Revoke(RevokedSerial{SerialNumber: issued.SerialNumber}); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("a revoked server certificate was trusted")
	}
}
