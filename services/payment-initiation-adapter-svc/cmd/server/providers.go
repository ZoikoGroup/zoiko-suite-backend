package main

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/egress"
	"zoiko.io/payment-initiation-adapter-svc/internal/config"
	"zoiko.io/payment-initiation-adapter-svc/internal/providers"
	"zoiko.io/payment-initiation-adapter-svc/internal/store"
)

// buildProviderEngine wires the live adapter: an egress-policy HTTP client (a provider is only ever reached
// through the SSRF guard, whatever a connector says), the connector service, and the vault.
func buildProviderEngine(pc config.ProviderConfig, st *store.PgStore) (*providers.Engine, error) {
	if pc.ConnectivityURL == "" || pc.VaultURL == "" {
		return nil, errors.New("PROVIDER_ADAPTERS_ENABLED needs CONNECTIVITY_SERVICE_URL and VAULT_SERVICE_URL")
	}
	nets, err := egress.ParseCIDRs(pc.PrivateCIDRs)
	if err != nil {
		return nil, err
	}
	policy := egress.Policy{AllowHTTP: pc.AllowHTTP, AllowedPorts: pc.AllowedPorts, PrivateNets: nets}
	client := policy.NewHTTPClient(60 * time.Second)
	return &providers.Engine{
		Store:      st,
		Connectors: providers.NewConnectorHTTP(pc.ConnectivityURL),
		Creds:      providers.NewVaultHTTP(pc.VaultURL, pc.WorkloadID),
		Transport:  &adapter.Transport{Client: client, MaxBytes: pc.MaxResponse},
		Cfg: providers.Config{
			StaleAfter: pc.StaleAfter, BreakerWindow: pc.BreakerWindow, BreakerFailures: pc.BreakerFailures,
		},
	}, nil
}

// runSweeper releases attempts stuck in SUBMITTING by a process that died mid-call. It never resends.
func runSweeper(ctx context.Context, e *providers.Engine, every time.Duration, log *zap.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := e.Sweep(ctx); err != nil {
				log.Error("in-flight sweep failed", zap.Error(err))
			} else if n > 0 {
				log.Warn("released stale in-flight attempts to PENDING_UNKNOWN", zap.Int("count", n))
			}
		}
	}
}
