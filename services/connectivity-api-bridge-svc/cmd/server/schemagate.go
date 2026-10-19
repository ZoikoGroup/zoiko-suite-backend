package main

import (
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/config"
	"zoiko.io/eventing/integration"
	"zoiko.io/eventing/outbox"
)

// schemaGate builds the relay's producer-side contract gate from configuration, or nil when no registry is configured.
// An event that does not satisfy its registered contract is quarantined before publish; a registry that cannot answer
// holds the event without spending its retry budget. It is never published unchecked because the registry was down.
func schemaGate(cfg *config.Config) (outbox.Gate, error) {
	if cfg.SchemaRegistryURL == "" {
		return nil, nil
	}
	reg, err := integration.NewHTTPRegistry(cfg.SchemaRegistryURL, cfg.SchemaGatePrincipal, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return &integration.SchemaGate{Registry: reg, AllowUnregistered: cfg.SchemaGateAllowUnregistered}, nil
}
