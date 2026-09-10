// Package mtls provisions this service's own mTLS server identity from
// mtls-management-svc and builds the tls.Config the mTLS listener uses to
// require and verify a caller's client certificate.
//
// Scope: this is the material-path mTLS pilot (docs/original_doc/
// zoiko_suite_doc5.txt:76,251 mandates mTLS for "material paths", not every
// call). authorization-svc is the highest-value pilot target because every
// authorization decision in the platform routes through it. Wiring the
// other ~70 inter-service clients is deliberately out of scope here — see
// cmd/server/main.go's MTLS_ENABLED gate, which leaves the plain HTTP port
// running unchanged for every caller that hasn't migrated yet.
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
)

// ServerIdentity is this service's provisioned mTLS material: its own
// leaf certificate + private key, and the CA certificate it trusts to
// verify a caller's client certificate.
type ServerIdentity struct {
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

// ProvisionServerIdentity asks mtls-management-svc to issue a fresh leaf
// certificate for serviceName. platformScopeID is passed as legal_entity_id
// — this identity is a platform-infrastructure cert, not scoped to any one
// tenant's data, same posture as the AUTHZ_PLATFORM_SCOPE_ID convention used
// elsewhere in this codebase.
//
// bootstrapToken authenticates this self-provisioning request — at this
// point in startup, this service has no principal, no session, and no
// prior credential of its own to present (the standard PKI bootstrap
// problem). It is read from a file mtls-management-svc's own
// mtls-bootstrap-keygen init container also populates (see
// deployments/docker-compose.yml), never minted or verified by this
// service itself. An empty bootstrapToken is sent as no header at all,
// which mtls-management-svc treats as "not attempting the bootstrap
// path" — this call then falls through to that service's normal
// principal/authorize check and fails the way it always did before this
// path existed.
func ProvisionServerIdentity(ctx context.Context, mtlsServiceURL, serviceName, platformScopeID, bootstrapToken string) (*ServerIdentity, error) {
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
	req.Header.Set("X-Tenant-ID", platformScopeID)
	if bootstrapToken != "" {
		req.Header.Set("X-Mtls-Bootstrap-Token", bootstrapToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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

	return &ServerIdentity{
		CertPEM:   []byte(result.Certificate.CertificatePEM),
		KeyPEM:    []byte(result.PrivateKeyPEM),
		CACertPEM: []byte(result.CACertPEM),
	}, nil
}

// ServerTLSConfig builds a tls.Config that presents id's leaf certificate
// and requires + verifies the caller's client certificate against id's CA —
// real mutual TLS, not just server-side TLS.
func ServerTLSConfig(id *ServerIdentity) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(id.CertPEM, id.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("load server key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(id.CACertPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
