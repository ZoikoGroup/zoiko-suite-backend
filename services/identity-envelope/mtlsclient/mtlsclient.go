// Package mtlsclient is a service's outbound workload identity: a client
// certificate from mtls-management-svc, presented on every call to a peer
// that requires mutual TLS (Security Standard SEC-INV-05; identity X5).
//
// Why a client and not only a listener: a relay that acts for no signed-in
// person — policy-svc's and workflow-svc's ledger relays into
// governance-decision-log-svc — carries no identity envelope. Under
// IDENTITY_VERIFY=require the only way such a call can still be verified is
// the caller's certificate, which the receiving gate admits as a registered
// peer workload (IDENTITY_PEERS).
//
// Provisioning presents the service's own bootstrap token (register M-01) and
// runs in the background with retries: an unavailable mtls-management-svc
// delays the mTLS calls (which fail their handshake and are retried by the
// caller's own outbox) but never stops the service. The certificate is renewed
// at two thirds of its lifetime; the platform CA is re-read with it.
//
// The server is verified against the platform CA AND by hostname: the URL's
// host must be a DNS name on the peer's certificate (mtls-management-svc
// issues the service name, e.g. https://governance-decision-log-svc:8450).
package mtlsclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// PlatformScopeID is the synthetic legal entity infrastructure certificates
// are issued under.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Options configure the identity.
type Options struct {
	// ServiceName is the service_name and CN requested; the bootstrap token
	// must be the one mtls-management-svc binds to this name.
	ServiceName        string
	ManagementURL      string
	BootstrapTokenPath string
	// RotationDays is the validity requested (default 90).
	RotationDays int
}

// ErrNotProvisioned is returned by a handshake before the first certificate
// has been issued.
var ErrNotProvisioned = errors.New("mtlsclient: workload certificate not provisioned yet")

// Identity is the current certificate and platform CA, renewed in place.
type Identity struct {
	opts Options
	log  *zap.Logger
	// provision is replaceable in tests.
	provision func(ctx context.Context) (*issued, error)

	mu        sync.RWMutex
	cert      *tls.Certificate
	pool      *x509.CertPool
	notBefore time.Time
	notAfter  time.Time

	ready     chan struct{}
	readyOnce sync.Once
}

type issued struct {
	certPEM, keyPEM, caPEM []byte
}

// Start provisions in the background and keeps the certificate renewed until
// ctx is done.
func Start(ctx context.Context, opts Options, log *zap.Logger) *Identity {
	id := New(opts, log)
	go id.run(ctx)
	return id
}

// New builds an identity without starting it (tests call Load directly).
func New(opts Options, log *zap.Logger) *Identity {
	if opts.RotationDays <= 0 {
		opts.RotationDays = 90
	}
	if log == nil {
		log = zap.NewNop()
	}
	id := &Identity{opts: opts, log: log, ready: make(chan struct{})}
	id.provision = id.provisionFromManagement
	return id
}

// Ready is closed once the first certificate is installed.
func (id *Identity) Ready() <-chan struct{} { return id.ready }

// Load installs a certificate, key and CA (PEM).
func (id *Identity) Load(certPEM, keyPEM, caPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("mtlsclient: key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("mtlsclient: parse certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("mtlsclient: parse CA certificate")
	}
	id.mu.Lock()
	id.cert, id.pool, id.notBefore, id.notAfter = &cert, pool, leaf.NotBefore, leaf.NotAfter
	id.mu.Unlock()
	id.readyOnce.Do(func() { close(id.ready) })
	return nil
}

func (id *Identity) renewAt() time.Time {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return id.notBefore.Add(id.notAfter.Sub(id.notBefore) * 2 / 3)
}

func (id *Identity) run(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		is, err := id.provision(ctx)
		if err == nil {
			err = id.Load(is.certPEM, is.keyPEM, is.caPEM)
		}
		var wait time.Duration
		if err != nil {
			id.log.Warn("mtlsclient: provisioning failed; retrying", zap.String("service", id.opts.ServiceName),
				zap.Duration("in", backoff), zap.Error(err))
			wait = backoff
			if backoff < time.Minute {
				backoff *= 2
			}
		} else {
			backoff = 2 * time.Second
			wait = time.Until(id.renewAt())
			id.log.Info("mtlsclient: workload certificate installed", zap.String("service", id.opts.ServiceName),
				zap.Time("renew_at", id.renewAt()))
			if wait < time.Minute {
				wait = time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (id *Identity) provisionFromManagement(ctx context.Context) (*issued, error) {
	o := id.opts
	token := ""
	if o.BootstrapTokenPath != "" {
		if b, err := os.ReadFile(o.BootstrapTokenPath); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	body, _ := json.Marshal(map[string]any{
		"legal_entity_id": PlatformScopeID, "service_name": o.ServiceName,
		"common_name": o.ServiceName, "rotation_days": o.RotationDays,
	})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.ManagementURL, "/")+"/v1/mtls/certificates", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", PlatformScopeID)
	h.Set("X-Legal-Entity-Id", PlatformScopeID)
	h.Set("X-Workload-Id", o.ServiceName)
	h.Set("X-Request-Id", randomID())
	h.Set("X-Correlation-ID", randomID())
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", "service_identity_provisioning")
	h.Set("Idempotency-Key", randomID())
	if token != "" {
		h.Set("X-Mtls-Bootstrap-Token", token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("call mtls-management-svc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("mtls-management-svc returned %d", resp.StatusCode)
	}
	var out struct {
		Certificate struct {
			CertificatePEM string `json:"certificate_pem"`
		} `json:"certificate"`
		PrivateKeyPEM string `json:"private_key_pem"`
		CACertPEM     string `json:"ca_certificate_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode provision result: %w", err)
	}
	return &issued{certPEM: []byte(out.Certificate.CertificatePEM), keyPEM: []byte(out.PrivateKeyPEM), caPEM: []byte(out.CACertPEM)}, nil
}

// TLSConfig presents the current certificate and verifies the server against
// the current platform CA and the dialled hostname.
func (id *Identity) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			id.mu.RLock()
			defer id.mu.RUnlock()
			if id.cert == nil {
				return nil, ErrNotProvisioned
			}
			return id.cert, nil
		},
		// The CA is only known once provisioned and is re-read on renewal,
		// so the standard verification (fixed RootCAs) is replaced by the
		// same checks in VerifyConnection: chain to the platform CA, server
		// usage, and the hostname. Nothing is skipped.
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			id.mu.RLock()
			pool := id.pool
			id.mu.RUnlock()
			if pool == nil {
				return ErrNotProvisioned
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("mtlsclient: server presented no certificate")
			}
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

// HTTPClient is an *http.Client presenting this identity.
func (id *Identity) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: id.TLSConfig(), ForceAttemptHTTP2: true}}
}
