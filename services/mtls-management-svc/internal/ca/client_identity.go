package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ClientIdentity is this service's own mTLS client certificate, for its calls
// to authorization-svc's mTLS listener.
//
// This service is a policy enforcement point too: every route asks
// authorization-svc for a decision. It used to ask over plaintext only, so
// with authorization-svc's evaluation gate in require mode (the default
// outside ENV=local) every mtls-management-svc route answered 503. Unlike the
// other PEPs (register M-02) it needs no bootstrap token: it IS the CA, so it
// issues its own certificate, short-lived, and re-issues it before it ages.
type ClientIdentity struct {
	ca       *CA
	ledger   *Ledger
	service  string
	lifetime time.Duration

	mu      sync.Mutex
	current *tls.Certificate
	renewAt time.Time
}

// ClientCertLifetime is the self-issued client certificate's validity. Short,
// because nothing outside this process holds the key and re-issuing is free.
const ClientCertLifetime = 24 * time.Hour

// NewClientIdentity builds a ClientIdentity for service.
func NewClientIdentity(c *CA, ledger *Ledger, service string) *ClientIdentity {
	return &ClientIdentity{ca: c, ledger: ledger, service: service, lifetime: ClientCertLifetime}
}

// Certificate returns the current client certificate, issuing a new one when
// none exists or the current one is past two thirds of its life.
func (ci *ClientIdentity) Certificate() (*tls.Certificate, error) {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if ci.current != nil && time.Now().Before(ci.renewAt) {
		return ci.current, nil
	}
	issued, err := ci.ca.IssueLeaf(ci.service, []string{ci.service}, ci.lifetime)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(issued.CertificatePEM, issued.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load self-issued client certificate: %w", err)
	}
	ci.current = &cert
	ci.renewAt = issued.NotBefore.Add(issued.NotAfter.Sub(issued.NotBefore) * 2 / 3)
	return ci.current, nil
}

// TLSConfig presents the client certificate and trusts only this CA. The
// server's certificate is also checked against the revocation ledger: a
// revoked authorization-svc certificate must not be trusted with decisions.
func (ci *ClientIdentity) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ci.ca.cert)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return ci.Certificate()
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			if serial := cs.PeerCertificates[0].SerialNumber.Text(16); ci.ledger.IsRevoked(serial) {
				return fmt.Errorf("server certificate %s is revoked", serial)
			}
			return nil
		},
	}
}
