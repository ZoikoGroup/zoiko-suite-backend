package clients

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrVaultUnavailable is returned on any network error or unexpected
// non-200/404 response from secret-vault-integration-svc.
var ErrVaultUnavailable = errors.New("secret-vault-integration-svc unavailable")

// VaultClient is BNK-02's real, but deliberately narrow, integration with
// secret-vault-integration-svc.
//
// HONEST SCOPE: minting a NEW lease requires a pre-provisioned, ACTIVE
// SecretPolicyVersion for the target secret_path ("deny-by-absence" —
// see secret-vault-integration-svc's own Broker handler doc) plus
// PLATFORM-scoped authorization (SECRET_POLICY_CREATE,
// SECRET_MATERIAL_WRITE) this service does not run under — the same gap
// documented in treasury-svc's BNK-01 work. CompleteConnectionAuthorization
// therefore accepts a caller-supplied token_lease_ref (obtained out of
// band, e.g. by an operator/admin flow already holding platform scope)
// rather than minting one inline.
//
// What IS real: revoking a lease you already hold the ID for is a single,
// simple call with no provisioning prerequisite — RevokeConnection and
// SuspendConnection call this for real, so "revoked consent still used"
// (the spec's own named negative path) is backed by an actual vault call,
// not just a local status flip.
type VaultClient interface {
	RevokeLease(ctx context.Context, tenantID, leaseID, principalID, correlationID string) error
}

type VaultHTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewVaultHTTPClient(baseURL string) *VaultHTTPClient {
	return &VaultHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *VaultHTTPClient) RevokeLease(ctx context.Context, tenantID, leaseID, principalID, correlationID string) error {
	if leaseID == "" {
		return nil // nothing to revoke — a connection that never completed authorization has no lease
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/secrets/leases/"+leaseID+"/revoke", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return ErrVaultUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		// 404 (lease already gone/expired) is treated as success — the
		// end state (no live lease) is what matters, not the path there.
		return ErrVaultUnavailable
	}
	return nil
}

// StubVaultClient is a deterministic, clearly-labeled-as-not-real
// implementation for local/CI use where secret-vault-integration-svc
// isn't reachable — same posture as payment-initiation-adapter-svc's
// StubProviderAdapter.
type StubVaultClient struct{}

func NewStubVaultClient() *StubVaultClient { return &StubVaultClient{} }

func (s *StubVaultClient) RevokeLease(_ context.Context, _, _, _, _ string) error { return nil }
