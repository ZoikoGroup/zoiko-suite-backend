// Package legalbasis verifies a regulated notice legal basis against policy-svc (S7-C2, ZS-SVC-Y-001 s8.1).
//
// A notice cites the rule it rests on. That citation used to be a caller-supplied string, so any text passed. It is now
// the id of an immutable POLICY VERSION of policy type LEGAL_BASIS, which must exist, be ACTIVE and in force when the
// notice is created, and apply to the sending tenant (or globally). policy-svc owns the rules and their lifecycle (review,
// approval, activation); this service only checks that what it is told exists and is current, and keeps the id, which
// names the exact version, as the evidence. It does not interpret the rule or decide legal sufficiency (INV-22).
//
// LEGAL_BASIS is a policy_type tag in policy-svc, which stores types as data: a steward creates a policy of that type and
// activates a version of it; no change to policy-svc is needed.
package legalbasis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// PolicyType is the policy_type of a legal-basis policy in policy-svc.
const PolicyType = "LEGAL_BASIS"

var (
	// ErrNotUsable means the reference does not name a legal basis that is active and in force for the tenant now.
	ErrNotUsable = errors.New("legal basis is not usable")
	// ErrUnavailable means policy-svc could not answer.
	ErrUnavailable = errors.New("policy service unavailable")
)

// Basis is the verified legal basis, as far as this service uses it.
type Basis struct {
	PolicyVersionID string
	PolicyID        string
	PolicyCode      string
	VersionNumber   int
	EffectiveFrom   time.Time
}

// Client calls policy-svc.
type Client struct {
	baseURL   string
	principal string
	http      *http.Client
	log       *zap.Logger
}

// NewClient returns a client that asks as the given workload identity, which needs policy-svc POLICY_READ action in the
// tenant scope (an onboarding grant).
func NewClient(baseURL, principal string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), principal: principal, http: &http.Client{Timeout: timeout}, log: log}
}

func (c *Client) get(ctx context.Context, tenantID, correlationID, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Workload-Id", c.principal)
	req.Header.Set("X-Source-Channel", "system")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("policy-svc unreachable while verifying a legal basis", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: unreadable answer", ErrUnavailable)
	}
	return resp.StatusCode, nil
}

// Verify checks that versionID names an ACTIVE LEGAL_BASIS policy version in force at `at` for the tenant.
func (c *Client) Verify(ctx context.Context, tenantID, correlationID, versionID string, at time.Time) (*Basis, error) {
	var v struct {
		PolicyVersionID string     `json:"policy_version_id"`
		PolicyID        string     `json:"policy_id"`
		TenantID        *string    `json:"tenant_id"`
		EffectiveFrom   time.Time  `json:"effective_from"`
		EffectiveTo     *time.Time `json:"effective_to"`
		VersionStatus   string     `json:"version_status"`
		VersionNumber   int        `json:"version_number"`
	}
	switch code, err := c.get(ctx, tenantID, correlationID, "/v1/policy-versions/"+url.PathEscape(versionID), &v); {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound:
		return nil, fmt.Errorf("%w: policy-svc has no policy version %s", ErrNotUsable, versionID)
	case code != http.StatusOK:
		// 401 and 403 are this service lacking the grant: not an answer about the basis, and not a reason to accept it.
		return nil, fmt.Errorf("%w: policy-svc answered %d", ErrUnavailable, code)
	}
	switch {
	case v.TenantID != nil && !strings.EqualFold(*v.TenantID, tenantID):
		// Another tenant version is indistinguishable from an unknown one.
		return nil, fmt.Errorf("%w: policy-svc has no policy version %s", ErrNotUsable, versionID)
	case v.VersionStatus != "ACTIVE":
		return nil, fmt.Errorf("%w: policy version %s is %s, not ACTIVE", ErrNotUsable, versionID, v.VersionStatus)
	case v.EffectiveFrom.After(at) || (v.EffectiveTo != nil && !at.Before(*v.EffectiveTo)):
		return nil, fmt.Errorf("%w: policy version %s is not in force now", ErrNotUsable, versionID)
	}
	var p struct {
		PolicyID   string `json:"policy_id"`
		PolicyCode string `json:"policy_code"`
		PolicyType string `json:"policy_type"`
	}
	switch code, err := c.get(ctx, tenantID, correlationID, "/v1/policies/"+url.PathEscape(v.PolicyID), &p); {
	case err != nil:
		return nil, err
	case code != http.StatusOK:
		return nil, fmt.Errorf("%w: policy-svc answered %d for the policy of version %s", ErrUnavailable, code, versionID)
	}
	if p.PolicyType != PolicyType {
		return nil, fmt.Errorf("%w: policy version %s belongs to a %s policy, not a %s", ErrNotUsable, versionID, p.PolicyType, PolicyType)
	}
	return &Basis{PolicyVersionID: v.PolicyVersionID, PolicyID: v.PolicyID, PolicyCode: p.PolicyCode, VersionNumber: v.VersionNumber, EffectiveFrom: v.EffectiveFrom}, nil
}
