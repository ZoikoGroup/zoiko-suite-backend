package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Errors from access-control-svc, as the provisioning pipeline needs to tell them apart.
var (
	// ErrAccessControlUnavailable means no answer was obtained (network, 5xx, 503): the work is retried, never
	// treated as a refusal and never as a success.
	ErrAccessControlUnavailable = errors.New("access-control-svc is unavailable")
	// ErrPrivilegedRefused is access control's own refusal to grant a non-baseline role to a federation request.
	ErrPrivilegedRefused = errors.New("access control refused: not a baseline role")
)

// RequestRefused is any other definite refusal (SoD conflict, retired role, bad request...). It is final for
// that request: retrying it would only ask the same question.
type RequestRefused struct {
	Status int
	Code   string
}

func (e *RequestRefused) Error() string {
	return fmt.Sprintf("access control refused the request (%d %s)", e.Status, e.Code)
}

// Eligibility is access control's answer on whether a role may be the target of a federation mapping.
type Eligibility struct {
	Eligible bool
	RiskTier string
	Reason   string
}

// AssignmentRequest is one governed request for baseline access.
type AssignmentRequest struct {
	TenantID          string
	LegalEntityID     string
	TargetPrincipalID string
	RoleDefinitionID  string
	Justification     string
	CorrelationID     string
}

// AssignmentOutcome is what access control answered for a request.
type AssignmentOutcome struct {
	RequestID string
	Status    string // PROVISIONED | PENDING_APPROVAL
}

// AccessControl is the part of access-control-svc federation uses. Identity never writes an assignment itself.
type AccessControl interface {
	RoleEligibility(ctx context.Context, tenantID, roleDefinitionID string) (Eligibility, error)
	RequestAssignment(ctx context.Context, req AssignmentRequest) (AssignmentOutcome, error)
	// RevokeAssignment ends an assignment. Revoking one that is already revoked, or gone, succeeds.
	RevokeAssignment(ctx context.Context, tenantID, legalEntityID, requestID, correlationID, reason string) error
}

// ACClient is the HTTP AccessControl. Calls are made as this service's own principal, which must hold
// ROLE_MANAGE on the entities its mappings name (an operator grant, never something federation gives itself).
type ACClient struct {
	baseURL   string
	principal string
	client    *http.Client
}

// NewACClient builds the client.
func NewACClient(baseURL, servicePrincipal string, timeout time.Duration) *ACClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ACClient{baseURL: strings.TrimRight(baseURL, "/"), principal: servicePrincipal, client: &http.Client{Timeout: timeout}}
}

func (c *ACClient) do(ctx context.Context, method, path, tenantID, entityID, correlationID, idem string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Workload-Id", "identity-context-svc")
	req.Header.Set("X-Request-Id", ulid.Make().String())
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "federation-provisioning")
	req.Header.Set("X-Timezone", "UTC")
	if entityID != "" {
		req.Header.Set("X-Legal-Entity-Id", entityID)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrAccessControlUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrAccessControlUnavailable, err)
	}
	return resp.StatusCode, raw, nil
}

func errCode(raw []byte) string {
	var m struct {
		ErrorCode string `json:"error_code"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.ErrorCode
}

// RoleEligibility asks whether the role is baseline. Anything but a clear answer is an error, never "eligible".
func (c *ACClient) RoleEligibility(ctx context.Context, tenantID, roleID string) (Eligibility, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/v1/iam/federation/role-eligibility?role_definition_id="+url.QueryEscape(roleID),
		tenantID, "", ulid.Make().String(), "", nil)
	if err != nil {
		return Eligibility{}, err
	}
	switch {
	case status == http.StatusOK:
		var out struct {
			Eligible bool   `json:"eligible"`
			RiskTier string `json:"risk_tier"`
			Reason   string `json:"reason"`
		}
		if json.Unmarshal(raw, &out) != nil {
			return Eligibility{}, fmt.Errorf("%w: undecodable eligibility", ErrAccessControlUnavailable)
		}
		return Eligibility{Eligible: out.Eligible, RiskTier: out.RiskTier, Reason: out.Reason}, nil
	case status == http.StatusNotFound:
		return Eligibility{Eligible: false, Reason: "the role does not exist"}, nil
	case status >= 500 || status == http.StatusTooManyRequests || operatorFault(status, errCode(raw)):
		return Eligibility{}, fmt.Errorf("%w: eligibility answered %d", ErrAccessControlUnavailable, status)
	default:
		return Eligibility{}, &RequestRefused{Status: status, Code: errCode(raw)}
	}
}

// RequestAssignment makes the governed request with origin FEDERATION. The correlation id is derived from the
// work item, so a retry after a crash is a replay at access control, not a second grant.
func (c *ACClient) RequestAssignment(ctx context.Context, r AssignmentRequest) (AssignmentOutcome, error) {
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/iam/access-assignments/", r.TenantID, r.LegalEntityID, r.CorrelationID, r.CorrelationID,
		map[string]any{
			"legal_entity_id": r.LegalEntityID, "target_principal_id": r.TargetPrincipalID, "role_definition_id": r.RoleDefinitionID,
			"justification": r.Justification, "correlation_id": r.CorrelationID, "origin": "FEDERATION",
		})
	if err != nil {
		return AssignmentOutcome{}, err
	}
	switch {
	case status == http.StatusCreated || status == http.StatusOK || status == http.StatusAccepted:
		var a struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(raw, &a) != nil || a.RequestID == "" {
			return AssignmentOutcome{}, fmt.Errorf("%w: undecodable assignment", ErrAccessControlUnavailable)
		}
		return AssignmentOutcome{RequestID: a.RequestID, Status: a.Status}, nil
	case status == http.StatusConflict && errCode(raw) == "federation_privileged_refused":
		return AssignmentOutcome{}, ErrPrivilegedRefused
	case status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout:
		return AssignmentOutcome{}, fmt.Errorf("%w: answered %d %s", ErrAccessControlUnavailable, status, errCode(raw))
	case operatorFault(status, errCode(raw)):
		return AssignmentOutcome{}, fmt.Errorf("%w: answered %d", ErrAccessControlUnavailable, status)
	default:
		return AssignmentOutcome{}, &RequestRefused{Status: status, Code: errCode(raw)}
	}
}

// RevokeAssignment ends an assignment.
func (c *ACClient) RevokeAssignment(ctx context.Context, tenantID, entityID, requestID, correlationID, reason string) error {
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/iam/access-assignments/"+url.PathEscape(requestID)+":revoke",
		tenantID, entityID, correlationID, correlationID,
		map[string]any{"legal_entity_id": entityID, "reason": reason, "correlation_id": correlationID})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusNotFound:
		return nil // nothing there to revoke
	case status == http.StatusConflict:
		// a revoke of something not revocable: done only if it is already revoked
		gs, graw, gerr := c.do(ctx, http.MethodGet, "/v1/iam/access-assignments/"+url.PathEscape(requestID), tenantID, "", correlationID, "", nil)
		if gerr != nil {
			return gerr
		}
		if gs == http.StatusOK {
			var a struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(graw, &a) == nil && a.Status == "REVOKED" {
				return nil
			}
		}
		return &RequestRefused{Status: status, Code: errCode(raw)}
	case status >= 500 || status == http.StatusTooManyRequests || operatorFault(status, errCode(raw)):
		return fmt.Errorf("%w: revoke answered %d", ErrAccessControlUnavailable, status)
	default:
		return &RequestRefused{Status: status, Code: errCode(raw)}
	}
}

// operatorFault reports a refusal that is about how this service is set up, not about the request: no identity,
// or a grant (ROLE_MANAGE, provisioning rights) its operator has yet to give. It is retried and raised as an
// exception rather than recorded as a final refusal of the person's access. A segregation-of-duties conflict
// is a real answer about the request and stays final.
func operatorFault(status int, code string) bool {
	return status == http.StatusUnauthorized || (status == http.StatusForbidden && code != "sod_conflict")
}
