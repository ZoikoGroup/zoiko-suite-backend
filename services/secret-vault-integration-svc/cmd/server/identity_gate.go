package main

import (
	"os"

	"zoiko.io/secret-vault-integration-svc/internal/config"
)

// gatePeers is who may vouch for X-Principal-Id without an envelope. With
// TLS_CLIENT_CA_FILE set the listener already requires a client certificate
// the platform CA issued (RequireAndVerifyClientCert), and IdentityCheck then
// requires that certificate to name the principal it carries unless it is a
// trusted forwarder; so every verified workload is a peer here, as it was
// before the gate. Without mutual TLS only the trusted forwarders are.
func gatePeers(cfg *config.Config) []string {
	if cfg.TLSClientCAFile != "" && os.Getenv("IDENTITY_TRUST_MTLS_PEERS") == "" {
		_ = os.Setenv("IDENTITY_TRUST_MTLS_PEERS", "true")
	}
	return cfg.MTLSTrustedForwarders
}
