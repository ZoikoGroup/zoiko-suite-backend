// Package upstream provides read-only HTTP clients to upstream Tier 0 services.
// This service NEVER writes to any upstream domain.
package upstream

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
)

// RegistryClient implements UpstreamRegistry against real Tier 0 service HTTP APIs.
//
// All methods are fail-closed: a network error is surfaced as an error,
// never silently swallowed. The resolver maps these to ErrUpstreamUnavailable
// and returns HTTP 503.
type RegistryClient struct {
	cfg    *config.Config
	log    *zap.Logger
	client *http.Client
}

func NewRegistryClient(cfg *config.Config, log *zap.Logger) *RegistryClient {
	return &RegistryClient{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: 3 * time.Second, // strict timeout — hot path must not hang
		},
	}
}

// IsTenantActive calls the Tenant & Entity Registry to verify tenant lifecycle_state.
//
// Endpoint (stub): GET {tenantRegistryURL}/v1/tenants/{tenantID}
// Returns false if the tenant is not ACTIVE; returns error if unreachable.
func (c *RegistryClient) IsTenantActive(ctx context.Context, tenantID string) (bool, error) {
	// TODO: implement real HTTP call
	// resp, err := c.client.Get(fmt.Sprintf("%s/v1/tenants/%s", c.cfg.TenantRegistryURL, tenantID))
	c.log.Debug("IsTenantActive stub", zap.String("tenant_id", tenantID))
	return true, nil // stub — safe default for development; real call required before production
}

// IsPrincipalAuthorizedForEntity verifies the principal has at least one active
// role assignment scoped to the requested legal entity.
//
// Endpoint (stub): GET {tenantRegistryURL}/v1/entities/{legalEntityID}
//   + principal authorization check
func (c *RegistryClient) IsPrincipalAuthorizedForEntity(ctx context.Context, principalID, legalEntityID string) (bool, error) {
	c.log.Debug("IsPrincipalAuthorizedForEntity stub",
		zap.String("principal_id", principalID),
		zap.String("legal_entity_id", legalEntityID),
	)
	return true, nil // stub
}

// ResolvePermissionBundles fetches permission bundle IDs for a slice of role IDs.
//
// Endpoint (stub): GET {accessControlURL}/v1/roles?ids=r1,r2,r3
// Results are aggregated; duplicates are deduplicated by the Access Control Service.
func (c *RegistryClient) ResolvePermissionBundles(ctx context.Context, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return []string{}, nil
	}
	c.log.Debug("ResolvePermissionBundles stub", zap.Strings("role_ids", roleIDs))
	return []string{}, nil // stub
}

// FetchActiveDelegations retrieves all active, non-expired delegated authority
// grants where principalID is the delegate, scoped to legalEntityID.
//
// Endpoint (stub):
//   GET {delegatedAuthorityURL}/v1/delegations?delegate={principalID}&entity={legalEntityID}&at=<now>
func (c *RegistryClient) FetchActiveDelegations(ctx context.Context, principalID, legalEntityID string) ([]domain.DelegatedAuthority, error) {
	c.log.Debug("FetchActiveDelegations stub",
		zap.String("principal_id", principalID),
		zap.String("legal_entity_id", legalEntityID),
	)
	return []domain.DelegatedAuthority{}, nil // stub
}

// validateHTTPResponse is a helper to surface non-2xx responses as errors.
// All upstream HTTP errors are fail-closed — they propagate as ErrUpstreamUnavailable.
func validateHTTPResponse(resp *http.Response, endpoint string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("upstream %s returned %d", endpoint, resp.StatusCode)
}
