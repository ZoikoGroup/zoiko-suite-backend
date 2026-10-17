// Package vault confirms that a connector's credential reference names a real,
// active secret policy in secret-vault-integration-svc (INT-CTRL-013). The
// connector stores only the reference; this package never sees, requests or
// returns secret material.
package vault

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

	"github.com/google/uuid"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
)

var (
	// ErrNotFound: the vault has no such secret policy (or not for this tenant).
	ErrNotFound = errors.New("credential reference does not name a secret policy")
	// ErrNotActive: the policy exists but has no active version, so nothing could
	// broker it.
	ErrNotActive = errors.New("credential reference has no active secret policy version")
	// ErrForbidden: the vault refused the caller. The approver must be allowed to
	// see the policy they are approving a reference to.
	ErrForbidden = errors.New("the vault refused to confirm the credential reference")
	// ErrUnavailable: the vault could not answer. Approval fails closed.
	ErrUnavailable = errors.New("secret vault unavailable")
	// ErrInvalidRef: the reference is not a policy id.
	ErrInvalidRef = errors.New("credential reference is not a secret policy id")
)

// Caller identifies who the confirmation is made for. The vault authorizes the
// read against this principal, so a reference is only accepted when the person
// creating or approving it could see that policy themselves.
type Caller struct {
	PrincipalID   string
	TenantID      string
	CorrelationID string
}

// Verifier confirms a credential reference.
type Verifier interface {
	Verify(ctx context.Context, ref string, caller Caller) error
}

// Client is the HTTP Verifier.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a client with an explicit timeout (no infinite waits, §8).
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("vault base URL must be absolute")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 3 * time.Second}
	}
	if httpClient.Timeout <= 0 {
		return nil, errors.New("vault HTTP client needs a timeout")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}, nil
}

type policyVersion struct {
	VersionStatus string  `json:"version_status"`
	TenantID      *string `json:"tenant_id"`
}

// maxResponseBytes bounds what is read back; a version list is small.
const maxResponseBytes = 1 << 20

// Verify implements Verifier. It reads the policy's version history, which the
// vault scopes to the caller's tenant, and requires at least one ACTIVE version
// that applies globally or to this tenant.
func (c *Client) Verify(ctx context.Context, ref string, caller Caller) error {
	if !domain.ValidCredentialRef(ref) {
		return ErrInvalidRef
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/secret-policies/"+url.PathEscape(strings.ToLower(ref))+"/versions", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// The vault refuses a governed read without its canonical input contract (400
	// envelope_incomplete): request id, source channel and purpose are mandatory. Found
	// against the real vault; a stub does not enforce it.
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "connector-credential-verification")
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-Id", caller.CorrelationID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	default:
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return fmt.Errorf("%w: unreadable response", ErrUnavailable)
	}
	var versions []policyVersion
	if err := json.Unmarshal(body, &versions); err != nil {
		return fmt.Errorf("%w: malformed response", ErrUnavailable)
	}
	for _, v := range versions {
		if v.VersionStatus != "ACTIVE" {
			continue
		}
		if v.TenantID == nil || strings.EqualFold(*v.TenantID, caller.TenantID) {
			return nil
		}
	}
	return ErrNotActive
}
