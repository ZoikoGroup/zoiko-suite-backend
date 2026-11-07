// Package mtlslistener gives a service a second, mutual-TLS listener in front
// of its existing router (Security Standard §3.1 SEC-INV-05, §9;
// Group 1 register S5-B1).
//
// gateway-auth-svc's mTLS client for this service has been ready since 29 Sep
// 2026 and stayed off because nothing here could accept it. This is that
// listener: same router, a certificate issued by mtls-management-svc, and every
// caller required to present a certificate from the same CA.
//
// Opt-in (MTLS_ENABLED). The plaintext listener is untouched, so a caller
// still on http:// keeps working. Provisioning runs in the background and is
// retried: an mTLS dependency that is down must never stop a Tier-0 service
// from serving its plaintext callers. The server certificate is renewed at two
// thirds of its lifetime, so it cannot expire under a running process.
//
// The same file is used verbatim by identity-context-svc and
// tenant-entity-registry-svc; it imports nothing service-specific.
package mtlslistener

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/identity-envelope/apicontract"
)

// PlatformScopeID is the synthetic legal entity platform infrastructure
// certificates are issued under — the value every other service provisions
// with.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Options configure the listener.
type Options struct {
	// ServiceName is the service_name and CN of this service's certificate.
	ServiceName string
	// Port is the mTLS listen port (8449 by convention).
	Port int
	// ManagementURL is mtls-management-svc's base URL.
	ManagementURL string
	// BootstrapTokenPath is the file holding the self-provisioning token.
	BootstrapTokenPath string
	// AllowedClients, when non-empty, lists the client certificate CNs (the
	// calling services' names) admitted. Empty admits any certificate the
	// platform CA issued.
	AllowedClients []string
	// RotationDays is the validity requested for the server certificate.
	RotationDays int
}

// ── provisioning ─────────────────────────────────────────────────────────────

type serverIdentity struct {
	certPEM, keyPEM, caPEM []byte
}

func readToken(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// provision asks mtls-management-svc for this service's certificate, with the
// full canonical envelope that service enforces (a bare request is refused
// 401 envelope_incomplete before the bootstrap token is looked at).
func provision(ctx context.Context, o Options) (*serverIdentity, error) {
	body, err := json.Marshal(map[string]any{
		"legal_entity_id": PlatformScopeID,
		"service_name":    o.ServiceName,
		"common_name":     o.ServiceName,
		"rotation_days":   o.RotationDays,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.ManagementURL, "/")+"/v1/mtls/certificates", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", PlatformScopeID)
	req.Header.Set("X-Legal-Entity-Id", PlatformScopeID)
	req.Header.Set("X-Workload-Id", o.ServiceName)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Correlation-ID", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "service_identity_provisioning")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if tok := readToken(o.BootstrapTokenPath); tok != "" {
		req.Header.Set("X-Mtls-Bootstrap-Token", tok)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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
	return &serverIdentity{certPEM: []byte(out.Certificate.CertificatePEM), keyPEM: []byte(out.PrivateKeyPEM), caPEM: []byte(out.CACertPEM)}, nil
}

// ── the rotating identity ────────────────────────────────────────────────────

type identity struct {
	mu        sync.RWMutex
	cert      *tls.Certificate
	pool      *x509.CertPool
	notBefore time.Time
	notAfter  time.Time
}

func (id *identity) update(si *serverIdentity) error {
	cert, err := tls.X509KeyPair(si.certPEM, si.keyPEM)
	if err != nil {
		return fmt.Errorf("load server key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse server certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(si.caPEM) {
		return errors.New("failed to parse CA certificate")
	}
	cert.Leaf = leaf
	id.mu.Lock()
	id.cert, id.pool, id.notBefore, id.notAfter = &cert, pool, leaf.NotBefore, leaf.NotAfter
	id.mu.Unlock()
	return nil
}

func (id *identity) renewAt() time.Time {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return id.notBefore.Add(id.notAfter.Sub(id.notBefore) * 2 / 3)
}

// tlsConfig reads the current certificate and CA on every handshake.
func (id *identity) tlsConfig() *tls.Config {
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			id.mu.RLock()
			defer id.mu.RUnlock()
			return id.cert, nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			id.mu.RLock()
			defer id.mu.RUnlock()
			return &tls.Config{
				Certificates: []tls.Certificate{*id.cert},
				ClientCAs:    id.pool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS12,
			}, nil
		},
	}
}

// ── client allow-list ────────────────────────────────────────────────────────

// verifiedClient is the CN of the certificate the handshake verified, "" when
// there is none (VerifiedChains, not PeerCertificates: the latter is whatever
// the client sent).
func verifiedClient(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	return strings.TrimSpace(r.TLS.VerifiedChains[0][0].Subject.CommonName)
}

// allowList refuses a verified client not on the list with 403, and sets
// X-Workload-Id to the verified name otherwise, so nothing downstream reads a
// claimed workload instead of the proven one.
func allowList(allowed []string, next http.Handler) http.Handler {
	set := map[string]bool{}
	for _, a := range allowed {
		if a = strings.TrimSpace(a); a != "" {
			set[a] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn := verifiedClient(r)
		if cn == "" || (len(set) > 0 && !set[cn]) {
			apicontract.WriteError(w, r, http.StatusForbidden, "workload_not_registered",
				"the client certificate does not name a service admitted on this listener")
			return
		}
		r.Header.Set("X-Workload-Id", cn)
		next.ServeHTTP(w, r)
	})
}

// ── the listener ─────────────────────────────────────────────────────────────

// Listener is a running (or still provisioning) mTLS listener.
type Listener struct {
	opts    Options
	handler http.Handler
	log     *zap.Logger

	mu  sync.Mutex
	srv *http.Server
	id  identity

	ready chan struct{}
}

// retryDelay is the wait after a failed provisioning or renewal. A variable so
// tests need not wait.
var retryDelay = func(attempt int) time.Duration {
	return min(5*time.Second<<min(attempt, 5), 5*time.Minute)
}

// Start provisions the identity in the background, then serves handler over
// mutual TLS on opts.Port, and keeps the certificate renewed until ctx ends.
// It never fails the caller: errors are logged and retried.
func Start(ctx context.Context, opts Options, handler http.Handler, log *zap.Logger) *Listener {
	if opts.RotationDays <= 0 {
		opts.RotationDays = 30
	}
	l := &Listener{opts: opts, handler: allowList(opts.AllowedClients, handler), log: log, ready: make(chan struct{})}
	go l.run(ctx)
	return l
}

// Ready is closed once the listener is serving.
func (l *Listener) Ready() <-chan struct{} { return l.ready }

func (l *Listener) provisionWithRetry(ctx context.Context) bool {
	for attempt := 0; ; attempt++ {
		si, err := provision(ctx, l.opts)
		if err == nil {
			err = l.id.update(si)
		}
		if err == nil {
			return true
		}
		l.log.Warn("mtls listener: provisioning failed — retrying; the plaintext listener is unaffected",
			zap.Error(err), zap.Int("attempt", attempt+1))
		select {
		case <-ctx.Done():
			return false
		case <-time.After(retryDelay(attempt)):
		}
	}
}

func (l *Listener) run(ctx context.Context) {
	if !l.provisionWithRetry(ctx) {
		return
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", l.opts.Port),
		Handler:           l.handler,
		TLSConfig:         l.id.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	l.mu.Lock()
	l.srv = srv
	l.mu.Unlock()
	go func() {
		l.log.Info("mtls listener serving", zap.Int("port", l.opts.Port), zap.Strings("allowed_clients", l.opts.AllowedClients))
		close(l.ready)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.log.Error("mtls listener stopped", zap.Error(err))
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(l.id.renewAt())):
		}
		if !l.provisionWithRetry(ctx) {
			return
		}
		l.log.Info("mtls listener: server certificate renewed")
	}
}

// Shutdown stops the listener if it is serving.
func (l *Listener) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	srv := l.srv
	l.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
