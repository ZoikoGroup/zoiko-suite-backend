// Package vaultref checks that a secret REFERENCE names a path the vault governs (INV-10, S3-B3).
//
// Configuration stores references of the form secret://<environment>/<path> and never material. A reference that
// names nothing is a dangling pointer a consumer discovers at start-up, so it is refused when it is written. The
// check asks secret-vault-integration-svc whether an ACTIVE policy governs the path (GET /v1/secret-paths/status).
// It receives metadata only: no value, no lease, and not the policy's allowed workloads. A consumer fetches the
// secret itself from the vault under its own identity; this service never holds or relays it.
//
// An unreachable vault is an error and the write is refused: a reference is never accepted on an assumption.
package vaultref

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ErrUnavailable means the vault could not say whether the path is governed.
var ErrUnavailable = errors.New("secret vault unavailable")

// Verifier says whether a policy governs a vault path in a tenant's scope. false is an answer; an error is
// ErrUnavailable and the caller refuses the write.
type Verifier interface {
	Governed(ctx context.Context, tenantID, correlationID, path string) (bool, error)
}

// Client calls secret-vault-integration-svc.
type Client struct {
	baseURL  string
	workload string
	http     *http.Client
	log      *zap.Logger
}

// NewClient returns a client that asks the vault as the given workload identity. That identity needs the vault's
// SECRET_POLICY_LIST action in the tenant's scope (an onboarding grant, as for the other service principals).
func NewClient(baseURL, workload string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), workload: workload, http: &http.Client{Timeout: timeout}, log: log}
}

// Governed implements Verifier.
func (c *Client) Governed(ctx context.Context, tenantID, correlationID, path string) (bool, error) {
	q := url.Values{"secret_path": {path}, "tenant_id": {tenantID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/secret-paths/status?"+q.Encode(), nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", c.workload)
	req.Header.Set("X-Workload-Id", c.workload)
	req.Header.Set("X-Source-Channel", "system")
	if correlationID != "" {
		req.Header.Set("X-Correlation-ID", correlationID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("secret vault unreachable while checking a secret reference", zap.Error(err))
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		// 401 and 403 are this service lacking the grant: not an answer about the path, and not a reason to accept it.
		c.log.Warn("secret vault answered a reference check with an unexpected status", zap.Int("status", resp.StatusCode))
		return false, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
}
