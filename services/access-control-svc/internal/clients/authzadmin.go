package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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

// CreateRole calls POST /v1/admin/roles. Idempotent server-side on
// (tenant_id, role_code) — a retried provisioning call is safe.
func (c *AuthzAdminClient) CreateRole(ctx context.Context, roleID, tenantID, roleCode, roleName, roleScopeType, createdByPrincipalID string) error {
	body, _ := json.Marshal(map[string]string{
		"role_id":                 roleID,
		"tenant_id":               tenantID,
		"role_code":               roleCode,
		"role_name":               roleName,
		"role_scope_type":         roleScopeType,
		"created_by_principal_id": createdByPrincipalID,
	})
	return c.post(ctx, "/v1/admin/roles", body)
}

// CreatePermissionBundle calls POST /v1/admin/roles/{roleID}/permission-bundles.
func (c *AuthzAdminClient) CreatePermissionBundle(ctx context.Context, roleID, bundleCode string, permittedActions []string) error {
	body, _ := json.Marshal(map[string]any{
		"bundle_code":       bundleCode,
		"permitted_actions": permittedActions,
	})
	return c.post(ctx, fmt.Sprintf("/v1/admin/roles/%s/permission-bundles", roleID), body)
}

func (c *AuthzAdminClient) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("authorization-svc admin API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("authorization-svc admin API returned %d for POST %s", resp.StatusCode, path)
	}
	return nil
}
