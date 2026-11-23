// Package mtls provisions this service's own client-side mTLS identity from
// mtls-management-svc and builds the http.Client authz.Client uses to call
// authorization-svc over mutual TLS.
//
// Scope: this is the client half of the material-path mTLS pilot (see
// authorization-svc/internal/mtls's doc comment for the server half).
// secret-vault-integration-svc is one of a small number of callers wired for the pilot —
// most of this platform's ~70 other authorization-svc callers are
// deliberately left on the plain HTTP port for now (docs/original_doc/
// zoiko_suite_doc5.txt:76 mandates mTLS for "selected" paths, not all of them).
//
// Security Standard §9 asks for short-lived, automatically rotated workload
// certificates. The certificate used to be provisioned once at boot with a
// 90-day lifetime and never renewed: long-lived, and the process failed every
// authz call the day it expired. It is now a 1-day certificate (the issuer's
// smallest unit) renewed in-process at half-life, so a restart is never what
// keeps the identity current.
package mtls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// certLifetimeDays is the lifetime requested for this service's client
// certificate. mtls-management-svc issues in whole days; 1 is its minimum.
const certLifetimeDays = 1

// csrRequest is the body of POST /v1/mtls/certificates/csr: a certificate signing request, never a key.
type csrRequest struct {
	CSR           string `json:"csr"`
	ServiceName   string `json:"service_name"`
	LegalEntityID string `json:"legal_entity_id"`
	RotationDays  int    `json:"rotation_days"`
}

type provisionResult struct {
	Certificate struct {
		CertificatePEM string `json:"certificate_pem"`
	} `json:"certificate"`
	CACertPEM string `json:"ca_certificate_pem"`
}

// spiffeURI is the workload identity the issuer requires in the CSR: spiffe://zoiko/<env>/<service>, with the same ENV
// the issuer reads (default local).
func spiffeURI(service string) *url.URL {
	env := os.Getenv("ENV")
	if env == "" {
		env = "local"
	}
	u, _ := url.Parse("spiffe://zoiko/" + env + "/" + service)
	return u
}

// newCSR generates a fresh key in this process and a CSR for it. The private key is returned to the caller and is
// never sent anywhere (Security s9, S4-C2).
func newCSR(service string) (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate client key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: service}, DNSNames: []string{service}, URIs: []*url.URL{spiffeURI(service)},
	}, key)
	if err != nil {
		return nil, "", fmt.Errorf("create csr: %w", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
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
	key, csrPEM, err := newCSR(p.serviceName)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	reqBody, err := json.Marshal(csrRequest{CSR: csrPEM, ServiceName: p.serviceName, LegalEntityID: p.platformScopeID, RotationDays: certLifetimeDays})
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal csr request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/v1/mtls/certificates/csr", bytes.NewReader(reqBody))
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
	block, _ := pem.Decode([]byte(result.Certificate.CertificatePEM))
	if block == nil {
		return tls.Certificate{}, nil, fmt.Errorf("issuer returned no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse client leaf: %w", err)
	}
	// The certificate must be for the key generated here, or the issuer signed something else.
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return tls.Certificate{}, nil, fmt.Errorf("issued certificate does not match the generated key")
	}
	cert := tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key, Leaf: leaf}
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
// to call a peer that requires client certificates (e.g. authorization-svc's
// mTLS listener).
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
