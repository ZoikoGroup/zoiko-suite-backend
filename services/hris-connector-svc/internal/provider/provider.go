// Package provider is the boundary between hris-connector-svc and the
// external HRIS systems it synchronises with (domain.ProviderWorkday,
// ProviderSuccessFactors, ProviderADP, ProviderGeneric).
//
// STATUS: no real provider adapter is implemented in this repository. A real
// adapter needs, per provider, an API client (Workday REST/SOAP, SAP
// SuccessFactors OData, ADP Workforce Now API), credentials resolved from
// secret-vault-integration-svc (OAuth2 client id/secret or certificate, tenant
// host), and a mapping into ZoikoSuite's worker model. None of that exists
// here, and no credentials or sandbox were available, so nothing in this
// package talks to a provider.
//
// The previous TriggerSync handler fabricated a COMPLETED sync job with
// records_synced=1 without contacting anything. That has been removed: the
// production registry is empty, so every sync resolves to
// ErrProviderNotConfigured and the API answers 503 provider_not_configured.
// Adding a provider means implementing Adapter for it and registering it in
// cmd/server/main.go — there is deliberately no stub/simulated adapter that
// could be switched on in production by configuration.
package provider

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrProviderNotConfigured: no real adapter is registered for the provider.
var ErrProviderNotConfigured = errors.New("HRIS provider adapter not configured")

// SyncRequest is one requested synchronisation run.
type SyncRequest struct {
	TenantID      string
	IntegrationID string
	Provider      string
	APIEndpoint   string
	SyncType      string
}

// SyncResult is what a real provider reported. RecordsSynced is the number
// of records the provider actually returned; it is never synthesised.
type SyncResult struct {
	RecordsSynced int
	CompletedAt   time.Time
}

// Adapter performs a sync against one external HRIS.
type Adapter interface {
	Sync(ctx context.Context, req SyncRequest) (SyncResult, error)
}

// Registry resolves an Adapter by provider name (case-insensitive).
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty registry: every provider is not configured.
func NewRegistry() *Registry { return &Registry{adapters: map[string]Adapter{}} }

// Register installs a real adapter for provider.
func (r *Registry) Register(provider string, a Adapter) {
	r.adapters[strings.ToUpper(strings.TrimSpace(provider))] = a
}

// Sync dispatches to the provider's adapter, or returns
// ErrProviderNotConfigured when none is registered.
func (r *Registry) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	if r == nil {
		return SyncResult{}, ErrProviderNotConfigured
	}
	a, ok := r.adapters[strings.ToUpper(strings.TrimSpace(req.Provider))]
	if !ok || a == nil {
		return SyncResult{}, ErrProviderNotConfigured
	}
	return a.Sync(ctx, req)
}
