// Package mtls provisions this service's own client-side mTLS identity from
// mtls-management-svc and builds the http.Client authz.Client uses to call
// authorization-svc over mutual TLS.
//
// Scope: this is the client half of the material-path mTLS pilot (see
// authorization-svc/internal/mtls's doc comment for the server half).
// invoice-approval-svc is one of a small number of callers wired for the
// pilot — most of this platform's ~70 other authorization-svc callers are
// deliberately left on the plain HTTP port for now (docs/original_doc/
// zoiko_suite_doc5.txt:76 mandates mTLS for "selected" paths, not all of them).
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
func NewClientHTTPClient(ctx context.Context, mtlsServiceURL, serviceName, platformScopeID string) (*http.Client, error) {
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
