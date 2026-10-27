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

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) leaf(t *testing.T, cn string, serial int64, notBefore, notAfter time.Time, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func (ca *testCA) server(t *testing.T, serial int64, notBefore, notAfter time.Time) *ServerIdentity {
	c, k := ca.leaf(t, "authorization-svc", serial, notBefore, notAfter, x509.ExtKeyUsageServerAuth)
	return &ServerIdentity{CertPEM: c, KeyPEM: k, CACertPEM: ca.pem}
}

// handshakeSerial dials a listener using ri and returns the serial the server
// presented, with a client certificate from ca.
func handshakeSerial(t *testing.T, ri *RotatingIdentity, ca *testCA) int64 {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ri.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	cc, ck := ca.leaf(t, "access-control-svc", 99, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth)
	clientCert, _ := tls.X509KeyPair(cc, ck)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// Governance audit N-11: the identity was frozen at boot. After Update, the
// next handshake presents the new certificate.
func TestRotatingIdentity_NextHandshakeUsesTheNewCertificate(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	ri, err := NewRotatingIdentity(ca.server(t, 10, now.Add(-time.Hour), now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if got := handshakeSerial(t, ri, ca); got != 10 {
		t.Fatalf("serial %d, want 10", got)
	}
	if err := ri.Update(ca.server(t, 11, now.Add(-time.Minute), now.Add(90*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got := handshakeSerial(t, ri, ca); got != 11 {
		t.Errorf("after rotation the listener presented serial %d, want 11", got)
	}
}

func TestRotatingIdentity_RenewsAtTwoThirds(t *testing.T) {
	ca := newTestCA(t)
	start := time.Now().Add(-time.Hour)
	ri, _ := NewRotatingIdentity(ca.server(t, 1, start, start.Add(90*time.Hour)))
	// Certificate times are whole seconds, so allow one.
	if want := start.Add(60 * time.Hour); ri.RenewAt().Sub(want).Abs() > time.Second {
		t.Errorf("RenewAt %v, want %v", ri.RenewAt(), want)
	}
}

// The per-handshake config still requires and verifies a client certificate
// against the current CA: one from another CA fails the handshake.
func TestRotatingIdentity_ClientFromAnotherCARefused(t *testing.T) {
	ca, rogue := newTestCA(t), newTestCA(t)
	ri, _ := NewRotatingIdentity(ca.server(t, 30, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ri.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		serverErr <- c.(*tls.Conn).Handshake()
		c.Close()
	}()
	cc, ck := rogue.leaf(t, "access-control-svc", 99, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth)
	clientCert, _ := tls.X509KeyPair(cc, ck)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, ServerName: "localhost"}); err == nil {
		_, _ = conn.Read(make([]byte, 1)) // TLS 1.3: the server's refusal arrives after the client's handshake
		conn.Close()
	}
	if err := <-serverErr; err == nil {
		t.Fatal("the server accepted a client certificate from another CA")
	}
}

// Run renews when due; a failed renewal is retried and the current
// certificate keeps serving.
func TestRotatingIdentity_RunRenewsAndRetries(t *testing.T) {
	old := renewRetry
	renewRetry = func(int) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() { renewRetry = old })

	ca := newTestCA(t)
	now := time.Now()
	// Already past two thirds: due at once.
	ri, _ := NewRotatingIdentity(ca.server(t, 20, now.Add(-2*time.Hour), now.Add(30*time.Minute)))

	var calls atomic.Int32
	renewed := make(chan struct{})
	provision := func(context.Context) (*ServerIdentity, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("mtls-management-svc unavailable")
		}
		defer func() {
			select {
			case <-renewed:
			default:
				close(renewed)
			}
		}()
		return ca.server(t, 21, time.Now().Add(-time.Minute), time.Now().Add(90*24*time.Hour)), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ri.Run(ctx, provision, zap.NewNop())

	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("not renewed")
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() < 2 {
		t.Errorf("provision called %d times, want the failure retried", calls.Load())
	}
	if got := handshakeSerial(t, ri, ca); got != 21 {
		t.Errorf("serial %d after renewal, want 21", got)
	}
}
