// Package provider is the seam between this service and the external
// e-signature providers it orchestrates (DocuSign, Adobe Sign — see
// domain.ProviderDocuSign / domain.ProviderAdobeSign).
//
// STATUS: no real provider adapter is implemented in this codebase. There is
// no DocuSign eSignature REST client and no Adobe Sign REST client here, no
// OAuth/JWT-grant credential handling, and no signed webhook receiver. The
// registry is therefore empty in every deployment, and every operation that
// would assert a provider fact (an envelope was SENT, a signer VIEWED/SIGNED/
// DECLINED) is refused with ErrNotConfigured, which handlers map to 503
// "esignature_provider_not_configured".
//
// Previously CreateEnvelope stored every envelope as SENT and published
// esignature.envelope.sent without contacting anyone — the "signature
// theater" ZS-SVC-S-001 §DRC-05 forbids. Failing closed is the honest answer
// until an adapter exists.
//
// Adding a real adapter: implement Adapter, register its constructor in
// builtin, and list its name in ESIGN_PROVIDERS. A name listed in
// ESIGN_PROVIDERS with no implementation fails startup (see Build), so a
// misconfiguration is loud rather than silently fake.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotConfigured means no adapter is configured for the requested provider.
var ErrNotConfigured = errors.New("esignature provider not configured")

// SendRequest is what an adapter needs to create and send an envelope.
type SendRequest struct {
	TenantID      string
	EnvelopeID    string
	DocumentTitle string
	SignerEmail   string
	SignerName    string
}

// SendResult carries the provider's own transaction/envelope id.
type SendResult struct {
	ExternalRef string
}

// Adapter is a real provider integration.
type Adapter interface {
	Name() string
	Send(ctx context.Context, req SendRequest) (SendResult, error)
}

// Registry resolves a provider name to its adapter.
type Registry struct {
	adapters map[string]Adapter
	order    []string
}

// NewRegistry builds a registry from adapters (used by Build and by tests).
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range adapters {
		name := strings.ToUpper(a.Name())
		if _, dup := r.adapters[name]; !dup {
			r.order = append(r.order, name)
		}
		r.adapters[name] = a
	}
	return r
}

// builtin lists the adapters compiled into this binary. Empty: see the
// package comment.
var builtin = map[string]func() (Adapter, error){}

// Build returns the registry for the comma-separated ESIGN_PROVIDERS value.
// Empty means no provider (fail closed). A listed name without a compiled-in
// adapter is a configuration error.
func Build(names string) (*Registry, error) {
	var adapters []Adapter
	for _, n := range strings.Split(names, ",") {
		n = strings.ToUpper(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		ctor, ok := builtin[n]
		if !ok {
			return nil, fmt.Errorf("ESIGN_PROVIDERS lists %q but no adapter for it is implemented in this build", n)
		}
		a, err := ctor()
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", n, err)
		}
		adapters = append(adapters, a)
	}
	return NewRegistry(adapters...), nil
}

// Configured reports whether any provider adapter is available.
func (r *Registry) Configured() bool { return r != nil && len(r.adapters) > 0 }

// Resolve returns the adapter for name; an empty name selects the first
// configured adapter. ErrNotConfigured when there is none.
func (r *Registry) Resolve(name string) (Adapter, error) {
	if !r.Configured() {
		return nil, ErrNotConfigured
	}
	if name == "" {
		return r.adapters[r.order[0]], nil
	}
	a, ok := r.adapters[strings.ToUpper(name)]
	if !ok {
		return nil, ErrNotConfigured
	}
	return a, nil
}
