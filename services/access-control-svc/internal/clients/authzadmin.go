package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// AuthzAdminClient provisions role and permission-bundle definitions into
// authorization-svc's real admin API — the same endpoints
// deployments/scripts/seed-demo-rbac.ps1 calls by hand. This is what turns
// a role/bundle definition recorded here into something actually enforced
// at authorization time.
type AuthzAdminClient struct {
	baseURL string
	http    *http.Client
}

func NewAuthzAdminClient(baseURL string) *AuthzAdminClient {
	return &AuthzAdminClient{baseURL: baseURL, http: &http.Client{Timeout: 5 * time.Second}}
}

// Scope is the verified caller context every admin call must carry.
//
// It replaces the loose principalID/tenantID/correlationID string tail these
// methods used to take, for two reasons found the hard way:
//
//  1. Two of the three methods passed EMPTY strings for principal and tenant
//     ("", "", correlationID). authorization-svc's admin routes call
//     requirePrincipal and requireTenant, so bundle provisioning and
//     retirement could only ever answer 401 — reported here as
//     `authz_admin_unavailable`, which reads as an outage rather than as a
//     request this service never populated. A struct makes the omission a
//     compile error instead of an easy-to-miss positional "".
//  2. authorization-svc enforces the canonical input contract in middleware,
//     ahead of its handlers. Four more headers are mandatory beyond the three
//     that were being sent, and adding them as four more positional
//     parameters to three methods would be worse than the problem.
//
// LegalEntityID is the entity the definition belongs to. It is required by the
// contract (INV-02, "mandatory for entity-specific records") and role
// definitions in this service are always entity-scoped, so there is no
// entity-less admin call to accommodate.
type Scope struct {
	PrincipalID   string
	TenantID      string
	LegalEntityID string
	CorrelationID string
}

// CreateRole calls POST /v1/admin/roles. Idempotent server-side on
// (tenant_id, role_code) — a retried provisioning call is safe.
func (c *AuthzAdminClient) CreateRole(ctx context.Context, roleID, roleCode, roleName, roleScopeType string, s Scope) error {
	body, _ := json.Marshal(map[string]string{
		"role_id":                 roleID,
		"tenant_id":               s.TenantID,
		"role_code":               roleCode,
		"role_name":               roleName,
		"role_scope_type":         roleScopeType,
		"created_by_principal_id": s.PrincipalID,
	})
	return c.post(ctx, "/v1/admin/roles", body, s)
}

// SetRoleActive calls POST /v1/admin/roles/{roleID}/retire or /reactivate.
func (c *AuthzAdminClient) SetRoleActive(ctx context.Context, roleID string, active bool, s Scope) error {
	action := "retire"
	if active {
		action = "reactivate"
	}
	return c.post(ctx, fmt.Sprintf("/v1/admin/roles/%s/%s", roleID, action), []byte(`{}`), s)
}

// CreatePermissionBundle calls POST /v1/admin/roles/{roleID}/permission-bundles.
func (c *AuthzAdminClient) CreatePermissionBundle(ctx context.Context, roleID, bundleCode string, permittedActions []string, s Scope) error {
	body, _ := json.Marshal(map[string]any{
		"bundle_code":       bundleCode,
		"permitted_actions": permittedActions,
	})
	return c.post(ctx, fmt.Sprintf("/v1/admin/roles/%s/permission-bundles", roleID), body, s)
}

// post sends one admin call with the complete canonical envelope.
//
// Every header below is mandatory at authorization-svc's middleware, which
// runs before its handlers — so a missing one produces a 400
// `envelope_incomplete` that never reaches the admin logic at all. The
// previous version sent Content-Type plus three conditional headers, and the
// resulting 400 surfaced here as "authorization-svc admin API returned 400",
// with no indication that the request rather than the service was at fault.
// The error now carries the response body for exactly that reason.
func (c *AuthzAdminClient) post(ctx context.Context, path string, body []byte, s Scope) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Verified scope. Set unconditionally: an empty value here is a bug in the
	// caller, and sending the empty header lets authorization-svc say so
	// (`missing_tenant_scope`) rather than having the contract check report a
	// vaguer violation about an absent field.
	req.Header.Set("X-Principal-Id", s.PrincipalID)
	req.Header.Set("X-Tenant-Id", s.TenantID)
	req.Header.Set("X-Legal-Entity-Id", s.LegalEntityID)

	correlationID := s.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)

	// Per-hop id, distinct from correlation: provisioning one role definition
	// makes several admin calls (create the role, then attach its bundle) and
	// each is its own request under the one correlation id.
	req.Header.Set("X-Request-Id", uuid.NewString())

	// Service-to-service, not a user channel.
	req.Header.Set("X-Source-Channel", "system")

	// Duplicate/replay protection (INV-08), mandatory for a material state
	// change. A fresh key per attempt is right here: these endpoints are
	// idempotent server-side on their own natural keys — (tenant_id,
	// role_code) for a role, (role_id, bundle_code) for a bundle — so a retry
	// is already safe without reusing the key, and reusing one across two
	// genuinely different provisioning calls would be the actual hazard.
	req.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("authorization-svc admin API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// The body distinguishes "this service sent a malformed request" from
		// "authorization-svc is unwell". Without it both read as an outage,
		// which is what hid the missing envelope for as long as it did.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("authorization-svc admin API returned %d for POST %s: %s",
			resp.StatusCode, path, string(detail))
	}
	return nil
}
