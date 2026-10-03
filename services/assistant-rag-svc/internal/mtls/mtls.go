package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"
)

func NewClientHTTPClient(ctx context.Context, managementURL, serviceName, platformScopeID string) (*http.Client, error) {
	// This is a stub for the mTLS pilot. In production, this would:
	// 1. Call the mTLS management service to provision a client certificate
	// 2. Return an http.Client configured with that certificate
	// For now, return a standard client with a longer timeout.
	return &http.Client{Timeout: 10 * time.Second}, nil
}

func LoadClientCert(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	return &cert, nil
}

func LoadCACert(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}
	return pool, nil
}
