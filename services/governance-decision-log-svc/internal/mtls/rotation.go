package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RotatingIdentity serves this service's workload identity and replaces it
// before it expires (production hardening, 11 Nov 2026).
//
// The identity used to be provisioned once at boot and frozen into a static
// tls.Config, for both the :8450 listener and the client that calls
// authorization-svc. mtls-management-svc issues it for 90 days, so on day 91
// every peer write (policy-svc's and workflow-svc's ledger relays) and every
// authorization check would fail until a restart. Both halves now read the
// certificate and the trusted CA from this holder on each handshake, and Run
// re-provisions at two thirds of the lifetime, so a failing renewal has the
// last third to recover. Same design as authorization-svc's listener.
type RotatingIdentity struct {
	mu        sync.RWMutex
	cert      *tls.Certificate
	pool      *x509.CertPool
	caCerts   []*x509.Certificate
	notBefore time.Time
	notAfter  time.Time

	// verifyPeer, when set, runs on every inbound handshake after chain
	// verification (the revocation check).
	verifyPeer func(tls.ConnectionState) error
}

// NewRotatingIdentity loads the first identity.
func NewRotatingIdentity(id *Identity) (*RotatingIdentity, error) {
	ri := &RotatingIdentity{}
	if err := ri.Update(id); err != nil {
		return nil, err
	}
	return ri, nil
}

// Update installs a new identity; the next handshake uses it. Connections
// already established keep the certificate they negotiated.
func (ri *RotatingIdentity) Update(id *Identity) error {
	if id == nil {
		return errors.New("mtls: no identity to install")
	}
	cert, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return fmt.Errorf("load key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
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
		return errors.New("parse CA certificate")
	}
	cert.Leaf = leaf
	ri.mu.Lock()
	ri.cert, ri.pool, ri.caCerts, ri.notBefore, ri.notAfter = &cert, pool, caCerts, leaf.NotBefore, leaf.NotAfter
	ri.mu.Unlock()
	return nil
}

// SetPeerVerifier installs a per-handshake check on the verified client
// certificate. Set it before the listener starts.
func (ri *RotatingIdentity) SetPeerVerifier(f func(tls.ConnectionState) error) {
	ri.mu.Lock()
	ri.verifyPeer = f
	ri.mu.Unlock()
}

// CACertificates returns the CA certificates currently trusted.
func (ri *RotatingIdentity) CACertificates() []*x509.Certificate {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return append([]*x509.Certificate(nil), ri.caCerts...)
}

// NotAfter is the current certificate's expiry.
func (ri *RotatingIdentity) NotAfter() time.Time {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.notAfter
}

// RenewAt is two thirds of the way through the current certificate's validity.
func (ri *RotatingIdentity) RenewAt() time.Time {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.notBefore.Add(ri.notAfter.Sub(ri.notBefore) * 2 / 3)
}

// ServerTLSConfig is the :8450 listener's configuration: a client certificate
// from the current platform CA is required, the current certificate is
// presented, and the revocation check runs — all read per handshake. Which
// verified clients are trusted peers is the identity gate's decision
// (IDENTITY_PEERS), not the handshake's.
func (ri *RotatingIdentity) ServerTLSConfig() *tls.Config {
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
		// GetCertificate as well, so http.Server.ListenAndServeTLS("", "")
		// sees a certificate source; GetConfigForClient carries the CA too.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			ri.mu.RLock()
			defer ri.mu.RUnlock()
			return ri.cert, nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return current(), nil },
	}
}

// ClientTLSConfig presents the current certificate and verifies the server
// against the current platform CA and the dialled host name. The standard
// chain check is replaced (not skipped) by the same checks in
// VerifyConnection, because the trusted pool can change on renewal.
func (ri *RotatingIdentity) ClientTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection against the rotating CA pool and the server name
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			ri.mu.RLock()
			defer ri.mu.RUnlock()
			return ri.cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("mtls: server presented no certificate")
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

// HTTPClient presents the rotating identity to a peer that requires client
// certificates (authorization-svc's :8449).
func (ri *RotatingIdentity) HTTPClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: ri.ClientTLSConfig(), ForceAttemptHTTP2: true},
	}
}

// renewRetry is the wait after a failed renewal: doubling from a minute,
// capped at fifteen. A variable so tests need not wait.
var renewRetry = func(attempt int) time.Duration {
	return min(time.Minute<<min(attempt, 4), 15*time.Minute)
}

// Run renews the identity with provision until ctx ends. A failed renewal is
// logged and retried; the current certificate keeps serving meanwhile, and
// the log escalates once it is inside its last day.
func (ri *RotatingIdentity) Run(ctx context.Context, provision func(context.Context) (*Identity, error), log *zap.Logger) {
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
				log.Error("mtls: workload certificate renewal failing and the certificate expires within a day — the :8450 listener and the authorization-svc client fail when it does", fields...)
			} else {
				log.Warn("mtls: workload certificate renewal failed — retrying; the current certificate keeps serving", fields...)
			}
			continue
		}
		attempt = 0
		log.Info("mtls: workload certificate renewed", zap.Time("not_after", ri.NotAfter()))
	}
}
