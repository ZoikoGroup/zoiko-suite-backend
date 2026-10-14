package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RotatingIdentity serves this service's mTLS identity and replaces it before
// it expires (Governance audit N-11, 13 Oct 2026).
//
// The identity used to be provisioned once at boot and frozen into a static
// tls.Config. mtls-management-svc issues it for 90 days, so on day 91 every
// handshake on the mTLS listener failed — and with AUTHZ_EVALUATION_MTLS=
// require, that is every authorization decision in the platform, until a
// restart. The listener now reads the certificate and the trusted CA from this
// holder on each handshake, and Run re-provisions at two thirds of the
// certificate's lifetime, so a renewal failure has the last third to recover.
type RotatingIdentity struct {
	mu        sync.RWMutex
	cert      *tls.Certificate
	pool      *x509.CertPool
	caCerts   []*x509.Certificate
	notBefore time.Time
	notAfter  time.Time

	// verifyPeer, when set, runs on every handshake after chain verification
	// (the revocation check, M-03).
	verifyPeer func(tls.ConnectionState) error
}

// SetPeerVerifier installs a per-handshake check on the verified client
// certificate. Set it before the listener starts.
func (ri *RotatingIdentity) SetPeerVerifier(f func(tls.ConnectionState) error) {
	ri.mu.Lock()
	ri.verifyPeer = f
	ri.mu.Unlock()
}

// CACertificates returns the CA certificates the listener currently trusts.
func (ri *RotatingIdentity) CACertificates() []*x509.Certificate {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return append([]*x509.Certificate(nil), ri.caCerts...)
}

// NewRotatingIdentity loads the first identity.
func NewRotatingIdentity(id *ServerIdentity) (*RotatingIdentity, error) {
	ri := &RotatingIdentity{}
	if err := ri.Update(id); err != nil {
		return nil, err
	}
	return ri, nil
}

// Update installs a new identity; the next handshake uses it. Connections
// already established keep the certificate they negotiated.
func (ri *RotatingIdentity) Update(id *ServerIdentity) error {
	cert, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return fmt.Errorf("load server key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse server certificate: %w", err)
	}
	pool := x509.NewCertPool()
	var caCerts []*x509.Certificate
	for rest := id.CACertPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			pool.AddCert(c)
			caCerts = append(caCerts, c)
		}
	}
	if len(caCerts) == 0 {
		return fmt.Errorf("failed to parse CA certificate")
	}
	cert.Leaf = leaf
	ri.mu.Lock()
	ri.cert, ri.pool, ri.caCerts, ri.notBefore, ri.notAfter = &cert, pool, caCerts, leaf.NotBefore, leaf.NotAfter
	ri.mu.Unlock()
	return nil
}

// NotAfter is the current certificate's expiry.
func (ri *RotatingIdentity) NotAfter() time.Time {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.notAfter
}

// RenewAt is when the current certificate should be replaced: two thirds of
// the way through its validity.
func (ri *RotatingIdentity) RenewAt() time.Time {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.notBefore.Add(ri.notAfter.Sub(ri.notBefore) * 2 / 3)
}

// TLSConfig is the listener's configuration: mutual TLS against the current
// CA, presenting the current certificate, both read per handshake.
func (ri *RotatingIdentity) TLSConfig() *tls.Config {
	current := func() *tls.Config {
		ri.mu.RLock()
		defer ri.mu.RUnlock()
		return &tls.Config{
			Certificates:     []tls.Certificate{*ri.cert},
			ClientCAs:        ri.pool,
			ClientAuth:       tls.RequireAndVerifyClientCert,
			MinVersion:       tls.VersionTLS12,
			VerifyConnection: ri.verifyPeer,
		}
	}
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
		// GetCertificate as well, so http.Server.ServeTLS sees a certificate
		// source; GetConfigForClient is what carries the current CA too.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			ri.mu.RLock()
			defer ri.mu.RUnlock()
			return ri.cert, nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return current(), nil },
	}
}

// renewRetry is the wait after a failed renewal: doubling from a minute,
// capped at fifteen. A variable so tests need not wait.
var renewRetry = func(attempt int) time.Duration {
	return min(time.Minute<<min(attempt, 4), 15*time.Minute)
}

// Run renews the identity with provision until ctx ends. A failed renewal is
// logged and retried; the current certificate keeps serving meanwhile, and the
// log escalates once it is inside its last day.
func (ri *RotatingIdentity) Run(ctx context.Context, provision func(context.Context) (*ServerIdentity, error), log *zap.Logger) {
	attempt := 0
	for {
		wait := time.Until(ri.RenewAt())
		if attempt > 0 {
			wait = renewRetry(attempt - 1)
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		id, err := provision(ctx)
		if err == nil {
			err = ri.Update(id)
		}
		if err != nil {
			attempt++
			fields := []zap.Field{zap.Error(err), zap.Time("not_after", ri.NotAfter()), zap.Int("attempt", attempt)}
			if time.Until(ri.NotAfter()) < 24*time.Hour {
				log.Error("mtls: server certificate renewal failing and the certificate expires within a day — the mTLS listener will refuse every handshake when it does", fields...)
			} else {
				log.Warn("mtls: server certificate renewal failed — retrying; the current certificate keeps serving", fields...)
			}
			continue
		}
		attempt = 0
		log.Info("mtls: server certificate renewed", zap.Time("not_after", ri.NotAfter()))
	}
}

// ClientTLSConfig presents the identity get returns as a CLIENT certificate
// and verifies the server against that identity's current platform CA.
//
// get is called per handshake, so a rotated certificate or CA is used as soon
// as it is installed, and it may return nil before the identity is provisioned:
// the handshake then fails rather than falling back to plaintext or to
// unverified TLS. Used to fetch identity-context-svc's JWKS from its mTLS
// listener, which allow-lists authorization-svc (register A-01).
//
// InsecureSkipVerify only disables the default chain check so that
// VerifyConnection can run it against the CURRENT rotating pool; every
// connection is still verified, including the server name.
func ClientTLSConfig(get func() *RotatingIdentity) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection against the rotating CA pool
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			ri := get()
			if ri == nil {
				return &tls.Certificate{}, nil
			}
			ri.mu.RLock()
			defer ri.mu.RUnlock()
			return ri.cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			ri := get()
			if ri == nil {
				return fmt.Errorf("mtls client identity not provisioned yet")
			}
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			ri.mu.RLock()
			pool := ri.pool
			ri.mu.RUnlock()
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: pool, Intermediates: inter, DNSName: cs.ServerName,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}
}
