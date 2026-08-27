// Package upstream provides read-only HTTP clients to upstream Tier 0 services.
// This service NEVER writes to any upstream domain.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
)

// ErrNotImplemented is returned by a client method whose upstream service is
// not reachable in this deployment. It is deliberately an ERROR rather than an
// empty success: the resolver maps it to ErrUpstreamUnavailable and refuses the
// request, which is the fail-closed behaviour the doctrine requires. The
// previous stub returned an empty slice and nil, so a missing upstream was
// indistinguishable from "this principal holds nothing".
var ErrNotImplemented = errors.New("upstream client not implemented in this deployment")

// RegistryClient implements UpstreamRegistry against real Tier 0 service HTTP APIs.
//
// All methods are fail-closed: a network error is surfaced as an error,
// never silently swallowed. The resolver maps these to ErrUpstreamUnavailable
// and returns HTTP 503.
//
// HOW THESE CALLS ARE ADDRESSED. tenant-entity-registry-svc is called directly
// on its own port, not through the gateway. Routing an identity resolution
// through the gateway would make it depend on gateway-auth-svc, which depends
// on this service — a verification that depends on a verification. The same
// reasoning gateway-auth-svc's own compose comment gives for calling the
// registry directly.
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

// tenantView is the subset of tenant-entity-registry-svc's Tenant this service
// reads. Its `status` and `lifecycle_state` are SEPARATE fields and only the
// second has a state machine — a tenant is created ACTIVE in status while still
// ONBOARDING in lifecycle. Reading `status` here and calling it "active" would
// admit a tenant that has not finished onboarding.
type tenantView struct {
	TenantID       string `json:"tenant_id"`
	LifecycleState string `json:"lifecycle_state"`
}

// entityView is the subset of the registry's LegalEntity this service reads.
type entityView struct {
	LegalEntityID string `json:"legal_entity_id"`
	TenantID      string `json:"tenant_id"`
	EntityStatus  string `json:"entity_status"`
}

// IsTenantActive calls the Tenant & Entity Registry to verify tenant lifecycle_state.
//
//	GET {tenantRegistryURL}/v1/tenants/{tenantID}
//
// Returns false when the tenant is not ACTIVE or is not visible to its own
// scope; returns an error when the registry cannot be reached, which the
// resolver turns into a 503 rather than a denial.
func (c *RegistryClient) IsTenantActive(ctx context.Context, tenantID string) (bool, error) {
	if tenantID == "" {
		return false, nil
	}

	status, body, err := c.get(ctx, tenantID,
		fmt.Sprintf("%s/v1/tenants/%s", c.cfg.TenantRegistryURL, tenantID))
	if err != nil {
		return false, err
	}

	// 404 is a real answer, not a transport failure: the registry returns it
	// both for "no such tenant" and for a tenant outside the caller's scope,
	// deliberately, so a probe cannot tell the two apart. Either way this
	// tenant is not one we may resolve a session for.
	if status == http.StatusNotFound {
		c.log.Warn("tenant not present in registry", zap.String("tenant_id", tenantID))
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("tenant registry GET /v1/tenants/%s returned %d", tenantID, status)
	}

	var t tenantView
	if err := json.Unmarshal(body, &t); err != nil {
		return false, fmt.Errorf("tenant registry response unreadable: %w", err)
	}

	active := t.LifecycleState == "ACTIVE"
	if !active {
		c.log.Info("tenant is not ACTIVE in lifecycle",
			zap.String("tenant_id", tenantID),
			zap.String("lifecycle_state", t.LifecycleState),
		)
	}
	return active, nil
}

// IsPrincipalAuthorizedForEntity verifies the requested legal entity is one this
// principal's tenant may act as.
//
//	GET {tenantRegistryURL}/v1/entities/{legalEntityID}
//
// tenantID is the principal's own verified tenant, and the registry scopes the
// read to it, so an entity belonging to another tenant answers 404 rather than
// returning the row. That is the isolation boundary being enforced here: before
// this was implemented the method returned true unconditionally, so any
// principal could obtain a signed envelope naming ANY legal entity on the
// platform — and every downstream service authorizes against exactly that
// claim.
//
// Whether the principal holds a ROLE on the entity is a separate question,
// answered from this service's own principal_role_assignments in the resolver's
// Dimension 4. This method answers the prior one: is the entity real, active,
// and inside the principal's tenant.
func (c *RegistryClient) IsPrincipalAuthorizedForEntity(ctx context.Context, principalID, tenantID, legalEntityID string) (bool, error) {
	if tenantID == "" || legalEntityID == "" {
		return false, nil
	}

	status, body, err := c.get(ctx, tenantID,
		fmt.Sprintf("%s/v1/entities/%s", c.cfg.TenantRegistryURL, legalEntityID))
	if err != nil {
		return false, err
	}

	if status == http.StatusNotFound {
		c.log.Warn("legal entity not visible to this tenant",
			zap.String("principal_id", principalID),
			zap.String("tenant_id", tenantID),
			zap.String("legal_entity_id", legalEntityID),
		)
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("tenant registry GET /v1/entities/%s returned %d", legalEntityID, status)
	}

	var e entityView
	if err := json.Unmarshal(body, &e); err != nil {
		return false, fmt.Errorf("entity response unreadable: %w", err)
	}

	// Belt and braces. The registry already scopes the read by the header, but
	// this claim ends up inside a signed envelope that every other service
	// trusts, so the tenant is re-checked against the body rather than assumed
	// from the fact that a row came back.
	if e.TenantID != tenantID {
		c.log.Error("registry returned an entity outside the requested tenant — refusing",
			zap.String("requested_tenant", tenantID),
			zap.String("entity_tenant", e.TenantID),
			zap.String("legal_entity_id", legalEntityID),
		)
		return false, nil
	}

	// DISSOLVED, SUSPENDED and DORMANT entities exist and are readable; none of
	// them may back a new session.
	if e.EntityStatus != "ACTIVE" {
		c.log.Info("legal entity is not ACTIVE",
			zap.String("legal_entity_id", legalEntityID),
			zap.String("entity_status", e.EntityStatus),
		)
		return false, nil
	}
	return true, nil
}

// ResolvePermissionBundles fetches permission bundle IDs for a slice of role IDs.
//
//	GET {accessControlURL}/v1/roles?ids=r1,r2,r3
//
// NOT IMPLEMENTED, and deliberately an error rather than an empty slice.
// access-control-svc owns the role -> permission-bundle mapping and is not part
// of this deployment; ACCESS_CONTROL_URL currently points back at this service,
// which serves no such route. Returning []string{}, nil — as the previous stub
// did — puts an empty permission_bundle_ids into a SIGNED envelope, which reads
// downstream as "this principal legitimately holds no permissions" rather than
// as "nobody asked". An empty roleIDs slice is still a real answer and returns
// empty without calling anything.
func (c *RegistryClient) ResolvePermissionBundles(ctx context.Context, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return []string{}, nil
	}
	c.log.Error("permission bundle resolution requested but access-control-svc is not wired",
		zap.Strings("role_ids", roleIDs),
		zap.String("access_control_url", c.cfg.AccessControlURL),
	)
	return nil, fmt.Errorf("%w: access-control-svc (ACCESS_CONTROL_URL=%s)", ErrNotImplemented, c.cfg.AccessControlURL)
}

// get performs one scoped read against an upstream service.
//
// X-Tenant-Id is what actually scopes the read: tenant-entity-registry-svc
// compares it against the tenant in the path and answers 404 on a mismatch, and
// a read that omits it returns 404 rather than data. The rest are the §4
// canonical envelope fields. The actor slot is filled with X-Workload-Id rather
// than X-Principal-Id because the caller here is this service, not the human
// whose session is being resolved — attributing an internal lookup to that human
// would put a request they never made under their name in the upstream's log.
func (c *RegistryClient) get(ctx context.Context, tenantID, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}

	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Workload-Id", "identity-context-svc")
	req.Header.Set("X-Request-Id", ulid.Make().String())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Accept", "application/json")
	if cid, ok := ctx.Value(correlationKey{}).(string); ok && cid != "" {
		req.Header.Set("X-Correlation-ID", cid)
	} else {
		req.Header.Set("X-Correlation-ID", ulid.Make().String())
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded read: an upstream that starts streaming must not be able to
	// exhaust this service's memory on the session hot path.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read %s: %w", url, err)
	}
	return resp.StatusCode, body, nil
}

// correlationKey carries the caller's correlation id into upstream requests so
// one business operation is followable across services. Unexported so nothing
// outside this package can put a value under it.
type correlationKey struct{}

// WithCorrelationID returns a context whose upstream calls carry cid.
func WithCorrelationID(ctx context.Context, cid string) context.Context {
	return context.WithValue(ctx, correlationKey{}, cid)
}
