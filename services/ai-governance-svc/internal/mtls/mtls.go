// Package mtls provisions this service's own client-side mTLS identity from
// mtls-management-svc and builds the http.Client authz.Client uses to call
// authorization-svc over mutual TLS.
//
// Scope: this is the client half of the material-path mTLS pilot (see
// authorization-svc/internal/mtls's doc comment for the server half);
// ai-governance-svc is one of the rollout targets.
package mtls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

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

// NewClientHTTPClient provisions a leaf certificate for serviceName from
// mtls-management-svc and returns an *http.Client whose Transport presents
// that certificate and trusts the issuing CA — ready to call a peer that
// requires client certificates (e.g. authorization-svc's mTLS listener).
//
// bootstrapToken authenticates this self-provisioning request — at this
// point in startup this service has no principal, no session, and no prior
// credential of its own to present. It is read from a file
// mtls-management-svc's own mtls-bootstrap-keygen init container also
// populates (see deployments/docker-compose.yml), never minted or verified
// by this service. An empty bootstrapToken sends no header at all, which
// falls through to mtls-management-svc's normal principal/authorize path.
//
// THE CANONICAL INPUT CONTRACT APPLIES TO THIS CALL TOO. mtls-management-svc
// enforces the estate envelope on every route; a request carrying only
// Content-Type and a tenant header is refused 401 envelope_incomplete
// before the bootstrap-token branch is ever reached, which reads like a
// rejected credential even though the token is never looked at (see
// authorization-svc/internal/mtls's identical fix for the same bug).
// actor_subject_id is X-Workload-Id, not X-Principal-Id — this is a service
// provisioning its own certificate during startup, with no human subject.
func NewClientHTTPClient(ctx context.Context, mtlsServiceURL, serviceName, platformScopeID, bootstrapToken string) (*http.Client, error) {
	reqBody, err := json.Marshal(provisionRequest{
		LegalEntityID: platformScopeID,
		ServiceName:   serviceName,
		CommonName:    serviceName,
		RotationDays:  90,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal provision request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mtlsServiceURL+"/v1/mtls/certificates", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", platformScopeID)
	req.Header.Set("X-Legal-Entity-Id", platformScopeID)
	req.Header.Set("X-Workload-Id", serviceName)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Correlation-ID", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "service_identity_provisioning")
	// Issuing a certificate is a material state change, so the contract wants
	// an idempotency key. A fresh one per attempt is correct here: a retry
	// after a failed provision must be allowed to mint a new leaf, not replay
	// the answer to a call whose result never arrived.
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if bootstrapToken != "" {
		req.Header.Set("X-Mtls-Bootstrap-Token", bootstrapToken)
	}

	provisionClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := provisionClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call mtls-management-svc: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("mtls-management-svc returned %d", resp.StatusCode)
	}

	var result provisionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode provision result: %w", err)
	}

	cert, err := tls.X509KeyPair([]byte(result.Certificate.CertificatePEM), []byte(result.PrivateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("load client key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(result.CACertPEM)) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS12,
			},
		},
	}, nil
}
