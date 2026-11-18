// Package mtls is this service's workload identity: one leaf certificate,
// provisioned from mtls-management-svc, used both ways.
//
//   - Server: a second listener (MTLS_PORT) that requires and verifies a
//     client certificate from the platform CA. The identity gate admits a
//     verified client whose certificate name is in IDENTITY_PEERS as a peer
//     workload vouching for X-Principal-Id (identity-envelope BasisPeer) —
//     how policy-svc's and workflow-svc's ledger relays, which act for no
//     signed-in person, can be admitted once IDENTITY_VERIFY=require (X5).
//   - Client: the http.Client authz.Client uses to call authorization-svc's
//     mTLS listener (AUTHZ_MTLS_ENABLED).
//
// Both halves read the certificate from a RotatingIdentity, renewed at two
// thirds of its lifetime, and the listener refuses client certificates on
// mtls-management-svc's CRL (RevocationChecker).
//
// Provisioning presents this service's own bootstrap token (register M-01:
// mtls-management-svc binds each token to exactly one service and refuses a
// provision without it). The first version of this package sent none, so
// AUTHZ_MTLS_ENABLED=true could never start.
package mtls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Identity is a provisioned leaf certificate, its key and the issuing CA.
type Identity struct {
	CertPEM   []byte
	KeyPEM    []byte
	CACertPEM []byte
}

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

// LoadBootstrapToken reads the service's bootstrap token file; "" when the
// path is unset or unreadable (provisioning then fails loudly at
// mtls-management-svc rather than here).
func LoadBootstrapToken(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Provision issues a leaf certificate for serviceName (its common name) from
// mtls-management-svc, presenting the service's bootstrap token and the
// canonical envelope that service requires.
func Provision(ctx context.Context, mtlsServiceURL, serviceName, platformScopeID, bootstrapToken string) (*Identity, error) {
	body, err := json.Marshal(provisionRequest{
		LegalEntityID: platformScopeID,
		ServiceName:   serviceName,
		CommonName:    serviceName,
		RotationDays:  90,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal provision request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(mtlsServiceURL, "/")+"/v1/mtls/certificates", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build provision request: %w", err)
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("X-Tenant-Id", platformScopeID)
	h.Set("X-Legal-Entity-Id", platformScopeID)
	h.Set("X-Workload-Id", serviceName)
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("X-Correlation-ID", uuid.NewString())
	h.Set("X-Source-Channel", "system")
	h.Set("X-Purpose-Context", "service_identity_provisioning")
	h.Set("Idempotency-Key", uuid.NewString())
	if bootstrapToken != "" {
		h.Set("X-Mtls-Bootstrap-Token", bootstrapToken)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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
	return &Identity{
		CertPEM:   []byte(result.Certificate.CertificatePEM),
		KeyPEM:    []byte(result.PrivateKeyPEM),
		CACertPEM: []byte(result.CACertPEM),
	}, nil
}
