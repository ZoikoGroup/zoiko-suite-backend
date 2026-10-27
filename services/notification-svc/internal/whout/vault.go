package whout

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"zoiko.io/eventing/signature"
)

// SigningSecretClass is the secret class the vault's sign operation accepts.
const SigningSecretClass = "WEBHOOK_SIGNING"

// VaultCaller identifies who a confirmation is made for. The vault authorizes the
// read against this principal, so a key reference is only accepted when the person
// submitting or approving it could see that policy themselves.
type VaultCaller struct {
	PrincipalID   string
	TenantID      string
	CorrelationID string
}

// KeyVerifier confirms a signing key reference before a subscription relies on it.
type KeyVerifier interface {
	Verify(ctx context.Context, keyPath string, caller VaultCaller) error
}

// Signing computes a signature with a vault-held key. The key never leaves the vault.
type Signing interface {
	Sign(ctx context.Context, tenantID, keyPath, requestID, correlationID string, msg []byte) (hexSig string, err error)
}

// ErrSignerUnavailable means the vault could not produce a signature. The delivery is
// retried; it is never sent unsigned.
var ErrSignerUnavailable = errors.New("the signing service is unavailable")

// VaultClient talks to secret-vault-integration-svc.
type VaultClient struct {
	base      string
	http      *http.Client
	principal string
}

// NewVaultClient builds a client. principal is this service's workload identity, which
// must appear in each signing policy's allowed workloads.
func NewVaultClient(baseURL string, hc *http.Client, principal string) (*VaultClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("vault base URL must be absolute")
	}
	if strings.TrimSpace(principal) == "" {
		return nil, errors.New("a signer principal is required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	if hc.Timeout <= 0 {
		return nil, errors.New("vault HTTP client needs a timeout")
	}
	return &VaultClient{base: strings.TrimRight(baseURL, "/"), http: hc, principal: principal}, nil
}

type policyVersion struct {
	SecretPath         string          `json:"secret_path"`
	SecretClass        string          `json:"secret_class"`
	VersionStatus      string          `json:"version_status"`
	TenantID           *string         `json:"tenant_id"`
	AllowedWorkloadIDs json.RawMessage `json:"allowed_workload_ids"`
}

const maxVaultResponse = 1 << 20

// Verify implements KeyVerifier: the path must name an ACTIVE signing-class policy
// that applies to the tenant and lets this service's workload identity use it, and the
// caller must be allowed to see it.
func (c *VaultClient) Verify(ctx context.Context, keyPath string, caller VaultCaller) error {
	q := url.Values{"secret_class": {SigningSecretClass}, "tenant_id": {caller.TenantID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/secret-policies?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDependency, err)
	}
	govern(req, caller.PrincipalID, caller.TenantID, caller.CorrelationID, "webhook-key-verification", uuid.NewString(), "")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: vault unreachable", ErrDependency)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: the vault refused to confirm the reference", ErrKeyUnavailable)
	default:
		return fmt.Errorf("%w: vault status %d", ErrDependency, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultResponse+1))
	if err != nil || len(body) > maxVaultResponse {
		return fmt.Errorf("%w: unreadable vault response", ErrDependency)
	}
	var versions []policyVersion
	if err := json.Unmarshal(body, &versions); err != nil {
		return fmt.Errorf("%w: malformed vault response", ErrDependency)
	}
	for _, v := range versions {
		if v.SecretPath != keyPath || v.VersionStatus != "ACTIVE" || v.SecretClass != SigningSecretClass {
			continue
		}
		if v.TenantID != nil && !strings.EqualFold(*v.TenantID, caller.TenantID) {
			continue
		}
		var allowed []string
		if json.Unmarshal(v.AllowedWorkloadIDs, &allowed) != nil {
			continue
		}
		for _, a := range allowed {
			if a == c.principal {
				return nil
			}
		}
		return fmt.Errorf("%w: this service is not an allowed signer for the reference", ErrKeyUnavailable)
	}
	return fmt.Errorf("%w: no active signing policy for the reference", ErrKeyUnavailable)
}

// Sign implements Signing.
func (c *VaultClient) Sign(ctx context.Context, tenantID, keyPath, requestID, correlationID string, msg []byte) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"secret_path": keyPath, "requested_by_principal_id": c.principal, "request_id": requestID,
		"message_base64": base64.StdEncoding.EncodeToString(msg), "algorithm": "HMAC-SHA256",
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/secrets/sign", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	govern(req, c.principal, tenantID, correlationID, "webhook-signing", requestID, requestID)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: vault unreachable", ErrSignerUnavailable)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("%w: unreadable response", ErrSignerUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: vault status %d", ErrSignerUnavailable, resp.StatusCode)
	}
	var out struct {
		SignatureHex string `json:"signature_hex"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.SignatureHex) != 64 {
		return "", fmt.Errorf("%w: malformed signature", ErrSignerUnavailable)
	}
	return out.SignatureHex, nil
}

// SignerFor adapts a Signing to the signature package's Signer for one key.
func SignerFor(s Signing, tenantID, keyPath, requestID, correlationID string) signature.Signer {
	return func(ctx context.Context, msg []byte) (string, error) {
		return s.Sign(ctx, tenantID, keyPath, requestID, correlationID, msg)
	}
}

// govern stamps the canonical input contract the vault enforces on every governed
// request: who is acting, for which tenant, why (purpose), how the request is traced,
// which channel it came through (system: an internal service call; the integration
// channel would also demand external-provenance headers), and, for a write, its
// idempotency key. The vault
// refuses a request without them (400 envelope_incomplete), so a client that omits
// them fails closed on every call and nothing is ever signed.
func govern(req *http.Request, principal, tenantID, correlationID, purpose, requestID, idempotencyKey string) {
	req.Header.Set("X-Principal-Id", principal)
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", purpose)
	if correlationID != "" {
		req.Header.Set("X-Correlation-Id", correlationID)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
}
