// Package mtls provisions this service's own client-side mTLS identity from
// mtls-management-svc and builds the http.Client the JWKS client and the
// tenant context resolver use to call their peers over mutual TLS
// (SEC-INV-05).
//
// Ported from secret-vault-integration-svc/internal/mtls, the pilot client
// that is known to work. The version this replaced (25 Sep 2026) could not
// have been turned on: it sent no bootstrap token and no envelope, so
// mtls-management-svc refused every provisioning call and the gateway would
// have exited at boot. It also asked for a 90-day certificate that was never
// renewed.
//
// Security Standard §9 asks for short-lived, automatically rotated workload
// certificates: a 1-day certificate (the issuer's smallest unit), renewed
// in-process at half-life, so a restart is never what keeps the identity
// current.
package mtls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// certLifetimeDays is the lifetime requested for this service's client
// certificate. mtls-management-svc issues in whole days; 1 is its minimum.
const certLifetimeDays = 1

type provisionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	ServiceName   string `json:"service_name"`
	CommonName    string `json:"common_name"`
	RotationDays  int    `json:"rotation_days"`
	AutoRotate    bool   `json:"auto_rotate"`
}

type provisionResult struct {
	Certificate struct {
		CertificatePEM string `json:"certificate_pem"`
	} `json:"certificate"`
	PrivateKeyPEM string `json:"private_key_pem"`
	CACertPEM     string `json:"ca_certificate_pem"`
}

// provisioner fetches a fresh leaf from mtls-management-svc.
type provisioner struct {
	url, serviceName, platformScopeID string
	// bootstrapTokenPath is the shared provisioning token file
	// (MTLS_BOOTSTRAP_TOKEN_PATH). mtls-management-svc authenticates a
	// self-provisioning service by it; without it every provisioning call
	// was refused, so AUTHZ_MTLS_ENABLED=true could never boot this service.
	// Re-read per call so a rotated token file is picked up by renewal.
	bootstrapTokenPath string
	client             *http.Client
}

func (p provisioner) bootstrapToken() string {
	if p.bootstrapTokenPath == "" {
		return ""
	}
	raw, err := os.ReadFile(p.bootstrapTokenPath)
	if err != nil {
		log.Printf("mtls: cannot read bootstrap token file %s: %v", p.bootstrapTokenPath, err)
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (p provisioner) provision(ctx context.Context) (tls.Certificate, []byte, error) {
	reqBody, err := json.Marshal(provisionRequest{
		LegalEntityID: p.platformScopeID,
		ServiceName:   p.serviceName,
		CommonName:    p.serviceName,
		RotationDays:  certLifetimeDays,
		AutoRotate:    true,
	})
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal provision request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/v1/mtls/certificates", bytes.NewReader(reqBody))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("build provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The canonical envelope mtls-management-svc requires on a write. The
	// actor is this workload (X-Workload-Id): a service provisioning its own
	// identity has no human subject. A fresh Idempotency-Key per attempt: a
	// retry after a failed provision must mint a new leaf.
	req.Header.Set("X-Tenant-Id", p.platformScopeID)
	req.Header.Set("X-Legal-Entity-Id", p.platformScopeID)
	req.Header.Set("X-Workload-Id", p.serviceName)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Correlation-ID", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "service_identity_provisioning")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if tok := p.bootstrapToken(); tok != "" {
		req.Header.Set("X-Mtls-Bootstrap-Token", tok)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("call mtls-management-svc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return tls.Certificate{}, nil, fmt.Errorf("mtls-management-svc returned %d", resp.StatusCode)
	}

	var result provisionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("decode provision result: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(result.Certificate.CertificatePEM), []byte(result.PrivateKeyPEM))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load client key pair: %w", err)
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("parse client leaf: %w", err)
		}
		cert.Leaf = leaf
	}
	return cert, []byte(result.CACertPEM), nil
}

// renewingCert holds the current leaf and replaces it once half its lifetime
// has passed. A failed renewal keeps the current (still valid) leaf and is
// retried on the next handshake; only an expired leaf is withheld.
type renewingCert struct {
	mu   sync.Mutex
	p    provisioner
	cert tls.Certificate
	now  func() time.Time
}

func renewAt(leaf *x509.Certificate) time.Time {
	return leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
}

func (r *renewingCert) get(ctx context.Context) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Before(renewAt(r.cert.Leaf)) {
		return &r.cert, nil
	}
	fresh, _, err := r.p.provision(ctx)
	if err == nil {
		r.cert = fresh
		return &r.cert, nil
	}
	if now.Before(r.cert.Leaf.NotAfter) {
		log.Printf("mtls: client certificate renewal failed, keeping current leaf until %s: %v", r.cert.Leaf.NotAfter.Format(time.RFC3339), err)
		return &r.cert, nil
	}
	return nil, fmt.Errorf("mtls: client certificate expired and renewal failed: %w", err)
}

// NewClientHTTPClient provisions a leaf certificate for serviceName from
// mtls-management-svc and returns an *http.Client whose Transport presents
// that certificate — renewed at half-life — and trusts the issuing CA, ready
// to call a peer that requires client certificates.
func NewClientHTTPClient(ctx context.Context, mtlsServiceURL, serviceName, platformScopeID, bootstrapTokenPath string) (*http.Client, error) {
	p := provisioner{url: mtlsServiceURL, serviceName: serviceName, platformScopeID: platformScopeID, bootstrapTokenPath: bootstrapTokenPath, client: &http.Client{Timeout: 10 * time.Second}}
	cert, caPEM, err := p.provision(ctx)
	if err != nil {
		return nil, err
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}
	rc := &renewingCert{p: p, cert: cert, now: time.Now}

	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return rc.get(cri.Context())
				},
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}
