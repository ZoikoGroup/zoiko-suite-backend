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
	"net/url"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
)

// ErrNotImplemented is returned by a client method whose upstream service is
// not reachable in this deployment. It is deliberately an ERROR rather than an
// empty success: the resolver maps it to ErrUpstreamUnavailable and refuses the
// request, which is the fail-closed behaviour the doctrine requires. The
// previous stub returned an empty slice and nil, so a missing upstream was
// indistinguishable from "this principal holds nothing".
var ErrNotImplemented = errors.New("upstream client not implemented in this deployment")

// ErrUpstreamRead is returned when an upstream answered but the answer cannot
// be trusted — a transport failure, an unexpected status, or an undecodable
// body. The resolver maps it to ErrUpstreamUnavailable and returns 503.
var ErrUpstreamRead = errors.New("upstream read failed")

// maxConcurrentBundleLookups bounds the per-resolution fan-out to
// access-control-svc. Principals hold single-digit role counts, so this is
// generous for the real workload while keeping one resolution from opening an
// unbounded number of sockets if that ever stops being true.
const maxConcurrentBundleLookups = 8

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
	// The registry treats this as MANDATORY — no LegalEntity exists without a
	// residency policy (data-model §05.3). It travels into the SessionContext
	// so the evidence record says which policy governed the session's PII.
	DataResidencyPolicyID string `json:"data_residency_policy_id"`
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

// ResolveEntityScope verifies the requested legal entity is one this
// principal's tenant may act as, and returns what that entity carries.
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
//
// It returns the scope rather than a bare bool because the same response
// carries the entity's data residency policy, which the SessionContext records
// as MANDATORY. That value was being fetched and discarded on every resolution,
// so every session was persisted with an empty residency policy — a field the
// data model says no record may lack.
func (c *RegistryClient) ResolveEntityScope(ctx context.Context, principalID, tenantID, legalEntityID string) (*domain.EntityScope, error) {
	denied := &domain.EntityScope{Authorized: false}

	if tenantID == "" || legalEntityID == "" {
		return denied, nil
	}

	status, body, err := c.get(ctx, tenantID,
		fmt.Sprintf("%s/v1/entities/%s", c.cfg.TenantRegistryURL, legalEntityID))
	if err != nil {
		return nil, err
	}

	if status == http.StatusNotFound {
		c.log.Warn("legal entity not visible to this tenant",
			zap.String("principal_id", principalID),
			zap.String("tenant_id", tenantID),
			zap.String("legal_entity_id", legalEntityID),
		)
		return denied, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("tenant registry GET /v1/entities/%s returned %d", legalEntityID, status)
	}

	var e entityView
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("entity response unreadable: %w", err)
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
		return denied, nil
	}

	// DISSOLVED, SUSPENDED and DORMANT entities exist and are readable; none of
	// them may back a new session.
	if e.EntityStatus != "ACTIVE" {
		c.log.Info("legal entity is not ACTIVE",
			zap.String("legal_entity_id", legalEntityID),
			zap.String("entity_status", e.EntityStatus),
		)
		return denied, nil
	}

	// An ACTIVE entity with no residency policy contradicts the registry's own
	// mandatory-field rule. Logged rather than refused: the session is
	// legitimate and blocking it would make this service the enforcement point
	// for another service's data-quality invariant. The empty value is carried
	// through honestly instead of being invented.
	if e.DataResidencyPolicyID == "" {
		c.log.Warn("legal entity carries no data residency policy",
			zap.String("legal_entity_id", legalEntityID),
			zap.String("tenant_id", tenantID),
		)
	}

	return &domain.EntityScope{
		Authorized:            true,
		DataResidencyPolicyID: e.DataResidencyPolicyID,
	}, nil
}

// permissionBundleView is the subset of access-control-svc's
// PermissionBundleDef this service reads.
type permissionBundleView struct {
	BundleID   string `json:"bundle_id"`
	ActiveFlag bool   `json:"active_flag"`
}

// ResolvePermissionBundles fetches the permission bundle IDs granted by a set
// of roles.
//
//	GET {accessControlURL}/v1/role-definitions/{roleID}/permission-bundles
//
// FAN-OUT, NOT A BATCH QUERY. access-control-svc exposes bundles per role
// definition and has no ids= filter, so this issues one request per role. The
// previous signature documented `GET /v1/roles?ids=r1,r2,r3`, a route that
// exists on no service in the estate — which is part of why this returned
// ErrNotImplemented rather than calling anything.
//
// Requests run concurrently and bounded: this is on the P99 < 50ms resolve
// path, and a principal with six roles would otherwise pay six sequential
// round trips.
//
// FAILURE IS FAIL-CLOSED, per role. If any role cannot be resolved the whole
// call errors and the resolver returns 503. Dropping the failed role and
// returning the rest would put a SHORT bundle list into a signed envelope,
// which downstream reads as "this principal legitimately lacks that
// permission" — a silent, signed under-grant is worse than a loud refusal.
//
// An empty roleIDs slice is a real answer and returns empty without calling
// anything: a principal with no roles genuinely holds no bundles.
func (c *RegistryClient) ResolvePermissionBundles(ctx context.Context, tenantID string, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return []string{}, nil
	}
	if tenantID == "" {
		return nil, errors.New("ResolvePermissionBundles: tenant_id is required")
	}

	// Deduplicate: two assignments of the same role on different entities are
	// distinct rows but one role definition.
	unique := make([]string, 0, len(roleIDs))
	seen := make(map[string]struct{}, len(roleIDs))
	for _, id := range roleIDs {
		if _, dup := seen[id]; dup || id == "" {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentBundleLookups)

	perRole := make([][]string, len(unique))
	for i, roleID := range unique {
		i, roleID := i, roleID
		g.Go(func() error {
			bundles, err := c.bundlesForRole(gctx, tenantID, roleID)
			if err != nil {
				return err
			}
			perRole[i] = bundles
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Flatten, deduplicating across roles: two roles sharing a bundle must not
	// place it in the envelope twice.
	out := make([]string, 0, len(unique))
	emitted := make(map[string]struct{})
	for _, bundles := range perRole {
		for _, b := range bundles {
			if _, dup := emitted[b]; dup {
				continue
			}
			emitted[b] = struct{}{}
			out = append(out, b)
		}
	}
	return out, nil
}

// bundlesForRole resolves one role definition's active bundle ids.
func (c *RegistryClient) bundlesForRole(ctx context.Context, tenantID, roleID string) ([]string, error) {
	status, body, err := c.get(ctx, tenantID,
		fmt.Sprintf("%s/v1/role-definitions/%s/permission-bundles", c.cfg.AccessControlURL, url.PathEscape(roleID)))
	if err != nil {
		return nil, fmt.Errorf("%w: access control: %v", ErrUpstreamRead, err)
	}

	// 404 means the principal holds an assignment to a role definition that
	// does not exist upstream. That is a data inconsistency, not an empty
	// grant, and resolving it to "no bundles" would sign the inconsistency into
	// an envelope. A role that genuinely grants nothing answers 200 with [].
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: role definition %s not found in access-control-svc", ErrUpstreamRead, roleID)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: access control GET /v1/role-definitions/%s/permission-bundles returned %d",
			ErrUpstreamRead, roleID, status)
	}

	var bundles []permissionBundleView
	if err := json.Unmarshal(body, &bundles); err != nil {
		return nil, fmt.Errorf("%w: decode permission bundles for role %s: %v", ErrUpstreamRead, roleID, err)
	}

	ids := make([]string, 0, len(bundles))
	for _, b := range bundles {
		// An inactive bundle is a grant that has been withdrawn. Including it
		// would keep it in force for the life of every envelope issued.
		if !b.ActiveFlag || b.BundleID == "" {
			continue
		}
		ids = append(ids, b.BundleID)
	}
	return ids, nil
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
