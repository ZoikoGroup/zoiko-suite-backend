package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
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

	// The Governance Control Plane §16 command fields authorization-svc records
	// with every privileged or destructive change (and refuses without, under
	// AUTHZ_COMMAND_CONTRACT=enforce). ReasonCode defaults per command when
	// empty — see commandBody — so no call this client makes is reasonless.
	// Purpose is the human reason, where the caller gave one.
	ReasonCode string
	Purpose    string
	// ApprovalReference names the governed request a grant was approved
	// under (this service's request id), recorded with the assignment there.
	ApprovalReference string
}

// commandBody is the §16 body of a destructive command: reason_code (the
// caller's, or defaultCode) and purpose, plus any extra fields.
func commandBody(s Scope, defaultCode string, extra map[string]any) []byte {
	code := s.ReasonCode
	if code == "" {
		code = defaultCode
	}
	body := map[string]any{"reason_code": code}
	if s.Purpose != "" {
		body["purpose"] = s.Purpose
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return b
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
	action, code := "retire", "ROLE_DEFINITION_RETIRED"
	if active {
		action, code = "reactivate", "ROLE_DEFINITION_REACTIVATED"
	}
	return c.post(ctx, fmt.Sprintf("/v1/admin/roles/%s/%s", roleID, action), commandBody(s, code, nil), s)
}

// CreatePermissionBundle calls POST /v1/admin/roles/{roleID}/permission-bundles.
func (c *AuthzAdminClient) CreatePermissionBundle(ctx context.Context, roleID, bundleCode string, permittedActions []string, s Scope) error {
	body, _ := json.Marshal(map[string]any{
		"bundle_code":       bundleCode,
		"permitted_actions": permittedActions,
	})
	return c.post(ctx, fmt.Sprintf("/v1/admin/roles/%s/permission-bundles", roleID), body, s)
}

// adminPermissionBundle mirrors authorization-svc's
// GET /v1/admin/roles/{role_id}/permission-bundles response shape. It is kept
// minimal to the fields this service reads: the two id spaces are connected by
// bundle_code, and retirement is addressed by authorization-svc's own id.
type adminPermissionBundle struct {
	PermissionBundleID string   `json:"permission_bundle_id"`
	RoleID             string   `json:"role_id"`
	BundleCode         string   `json:"bundle_code"`
	PermittedActions   []string `json:"permitted_actions"`
	ActiveFlag         bool     `json:"active_flag"`
}

// SetPermissionBundleActive retires or reactivates the bundle with bundleCode
// on roleID, scoped by the verified caller context.
//
// authorization-svc's retire/reactivate endpoints are addressed by ITS
// permission_bundle_id, which this service does not store locally — the
// role-scoped list is the join between the two id spaces. Fail-closed: if no
// bundle with that code exists there, the register and the enforcement plane
// disagree, and the caller gets domain.ErrAuthzBundleNotFound to investigate
// rather than a silent local-only change.
func (c *AuthzAdminClient) SetPermissionBundleActive(ctx context.Context, roleID, bundleCode string, active bool, s Scope) error {
	bundles, err := c.listPermissionBundles(ctx, roleID, s)
	if err != nil {
		return err
	}
	for _, b := range bundles {
		if b.BundleCode != bundleCode {
			continue
		}
		action, code := "retire", "PERMISSION_BUNDLE_RETIRED"
		if active {
			action, code = "reactivate", "PERMISSION_BUNDLE_REACTIVATED"
		}
		return c.post(ctx, fmt.Sprintf("/v1/admin/permission-bundles/%s/%s", b.PermissionBundleID, action), commandBody(s, code, nil), s)
	}
	return domain.ErrAuthzBundleNotFound
}

// listPermissionBundles calls the read that joins the two id spaces. It sends
// the five unconditionally mandatory envelope headers only — this is a read, so
// the contract requires no Content-Type, Idempotency-Key or X-Legal-Entity-Id
// (those are RequiredOnWrite; see authorization-svc's envelope policy).
func (c *AuthzAdminClient) listPermissionBundles(ctx context.Context, roleID string, s Scope) ([]adminPermissionBundle, error) {
	path := fmt.Sprintf("/v1/admin/roles/%s/permission-bundles", roleID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Principal-Id", s.PrincipalID)
	req.Header.Set("X-Tenant-Id", s.TenantID)
	correlationID := s.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorization-svc admin API unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, classify(resp.StatusCode, fmt.Errorf("authorization-svc admin API returned %d for GET %s: %s",
			resp.StatusCode, path, string(detail)))
	}
	var bundles []adminPermissionBundle
	if err := json.NewDecoder(resp.Body).Decode(&bundles); err != nil {
		return nil, fmt.Errorf("authorization-svc admin API returned an unreadable bundle list: %w", err)
	}
	return bundles, nil
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
		return classify(resp.StatusCode, fmt.Errorf("authorization-svc admin API returned %d for POST %s: %s",
			resp.StatusCode, path, string(detail)))
	}
	return nil
}

// classify marks a 403 as a refusal of the caller rather than an outage.
//
// Since 30 Sep authorization-svc gates its admin writes on iam.* actions
// (iam.role.manage, iam.permission_bundle.manage) at TENANT scope, and this
// service provisions AS the caller. A caller holding ROLE_MANAGE but not those
// grants passes this service's own check and is then refused there. That 403
// used to leave here as 503 authz_admin_unavailable, which pages on-call for
// an outage when the fix is a missing grant.
func classify(status int, err error) error {
	if status == http.StatusForbidden {
		return fmt.Errorf("%w: %v", domain.ErrProvisioningForbidden, err)
	}
	return err
}

// ── role assignments (POST/GET /v1/admin/role-assignments) ──────────────────
//
// authorization-svc holds the access assignment and enforces it; this service
// provisions the assignment a governed request (or a review decision) has
// cleared. authorization-svc requires iam.assignment.grant / .revoke at tenant
// scope for the CALLER and refuses a self-grant, so the provisioning principal
// is always the approver, never the subject.

type createAssignmentBody struct {
	PrincipalRoleAssignmentID string     `json:"principal_role_assignment_id,omitempty"`
	PrincipalID               string     `json:"principal_id"`
	RoleID                    string     `json:"role_id"`
	LegalEntityID             string     `json:"legal_entity_id,omitempty"`
	EffectiveFrom             time.Time  `json:"effective_from"`
	EffectiveTo               *time.Time `json:"effective_to,omitempty"`
	ApprovalReference         string     `json:"approval_reference,omitempty"`
}

// CreateRoleAssignment provisions principalID into roleID and returns
// authorization-svc's assignment id. assignmentID is sent as the id to use, so
// the two id spaces are joined by construction. effectiveTo nil is open-ended;
// set, authorization-svc ends the assignment at that instant.
func (c *AuthzAdminClient) CreateRoleAssignment(ctx context.Context, assignmentID, principalID, roleID, legalEntityID string, effectiveFrom time.Time, effectiveTo *time.Time, s Scope) (string, error) {
	var end *time.Time
	if effectiveTo != nil {
		u := effectiveTo.UTC()
		end = &u
	}
	body, _ := json.Marshal(createAssignmentBody{
		PrincipalRoleAssignmentID: assignmentID,
		PrincipalID:               principalID,
		RoleID:                    roleID,
		LegalEntityID:             legalEntityID,
		EffectiveFrom:             effectiveFrom.UTC(),
		EffectiveTo:               end,
		ApprovalReference:         s.ApprovalReference,
	})
	var out domain.AuthzAssignment
	if err := c.postJSON(ctx, "/v1/admin/role-assignments", body, s, &out); err != nil {
		return "", err
	}
	if out.PrincipalRoleAssignmentID == "" {
		return "", fmt.Errorf("authorization-svc admin API created an assignment without an id")
	}
	// authorization-svc answers 202 PENDING_APPROVAL when it does not accept
	// the provisioning principal as the independent approver of a privileged
	// role. Reported, not swallowed: recording it PROVISIONED here would claim
	// access that is not in force.
	if out.ApprovalStatus == "PENDING_APPROVAL" {
		return out.PrincipalRoleAssignmentID, domain.ErrAuthzApprovalPending
	}
	return out.PrincipalRoleAssignmentID, nil
}

// RevokeRoleAssignment ends an assignment. A 404 (already ended, or never
// there) is domain.ErrAuthzAssignmentAbsent so the caller can decide whether
// that is the outcome it wanted.
func (c *AuthzAdminClient) RevokeRoleAssignment(ctx context.Context, assignmentID string, s Scope) error {
	err := c.post(ctx, fmt.Sprintf("/v1/admin/role-assignments/%s/revoke", assignmentID), commandBody(s, "ASSIGNMENT_REVOKED", nil), s)
	if err != nil && strings.Contains(err.Error(), "returned 404") {
		return fmt.Errorf("%w: %v", domain.ErrAuthzAssignmentAbsent, err)
	}
	return err
}

// ScheduleRoleAssignmentEnd ends an assignment at a future instant there
// (an effective-dated revoke). 404 is domain.ErrAuthzAssignmentAbsent.
func (c *AuthzAdminClient) ScheduleRoleAssignmentEnd(ctx context.Context, assignmentID string, at time.Time, s Scope) error {
	body := commandBody(s, "ASSIGNMENT_END_SCHEDULED", map[string]any{"effective_to": at.UTC()})
	err := c.post(ctx, fmt.Sprintf("/v1/admin/role-assignments/%s/revoke", assignmentID), body, s)
	if err != nil && strings.Contains(err.Error(), "returned 404") {
		return fmt.Errorf("%w: %v", domain.ErrAuthzAssignmentAbsent, err)
	}
	return err
}

// assignmentPageSize is the page this client asks for; authorization-svc caps
// a page at 500.
const assignmentPageSize = 500

// ListRoleAssignments reads EVERY active assignment of one role, with usage
// (last GRANTED decision, subject status) for the review signals.
//
// It pages. The plain list stops at 500 rows without saying so, and a review
// campaign built on it would have reviewed the first 500 holders of a role and
// silently left the rest unreviewed and, through the review, unremovable.
func (c *AuthzAdminClient) ListRoleAssignments(ctx context.Context, roleID string, s Scope) ([]domain.AuthzAssignment, error) {
	var all []domain.AuthzAssignment
	for offset := 0; ; offset += assignmentPageSize {
		page, err := c.listAssignmentPage(ctx, roleID, offset, s)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < assignmentPageSize {
			return all, nil
		}
	}
}

func (c *AuthzAdminClient) listAssignmentPage(ctx context.Context, roleID string, offset int, s Scope) ([]domain.AuthzAssignment, error) {
	path := fmt.Sprintf("/v1/admin/role-assignments?role_id=%s&include_usage=true&limit=%d&offset=%d",
		url.QueryEscape(roleID), assignmentPageSize, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	setReadHeaders(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorization-svc admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, classify(resp.StatusCode, fmt.Errorf("authorization-svc admin API returned %d for GET %s: %s",
			resp.StatusCode, path, string(detail)))
	}
	var out []domain.AuthzAssignment
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("authorization-svc admin API returned an unreadable assignment list: %w", err)
	}
	return out, nil
}

func setReadHeaders(req *http.Request, s Scope) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Principal-Id", s.PrincipalID)
	req.Header.Set("X-Tenant-Id", s.TenantID)
	correlationID := s.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
}

// postJSON is post plus a decoded response body.
func (c *AuthzAdminClient) postJSON(ctx context.Context, path string, body []byte, s Scope, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", s.PrincipalID)
	req.Header.Set("X-Tenant-Id", s.TenantID)
	req.Header.Set("X-Legal-Entity-Id", s.LegalEntityID)
	correlationID := s.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("authorization-svc admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return classify(resp.StatusCode, fmt.Errorf("authorization-svc admin API returned %d for POST %s: %s",
			resp.StatusCode, path, string(detail)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("authorization-svc admin API returned an unreadable body for POST %s: %w", path, err)
	}
	return nil
}
