// Package ext holds the collaborators every provider adapter (bank, tax, e-invoice) needs: the INT-05 connector
// resolver and the vault credential source. A connector is only usable when it is ACTIVE, approved exactly as
// configured, for the right legal entity and holding the scope the provider profile requires.
package ext

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

	"zoiko.io/eventing/adapter"
)

// Connector is the part of an INT-05 connector the adapter needs.
type Connector struct {
	ID                  string   `json:"bridge_id"`
	LegalEntityID       string   `json:"legal_entity_id"`
	EndpointURL         string   `json:"endpoint_url"`
	AuthType            string   `json:"auth_type"`
	Status              string   `json:"status"`
	CredentialRef       string   `json:"credential_ref"`
	Scopes              []string `json:"scopes"`
	ConfigFingerprint   string   `json:"config_fingerprint"`
	ApprovedFingerprint string   `json:"approved_fingerprint"`
}

// Usable reports whether the connector may carry this payment: ACTIVE, approved as configured (the approval
// fingerprint equals the current configuration), for the same legal entity, and holding the scope the
// provider profile requires. The reason is a rule name, never connector content.
func (c Connector) Usable(legalEntityID, requiredScope string) error {
	switch {
	case c.Status != "ACTIVE":
		return fmt.Errorf("%w: connector is not ACTIVE", ErrConnectorRefused)
	case c.ConfigFingerprint == "" || c.ConfigFingerprint != c.ApprovedFingerprint:
		return fmt.Errorf("%w: connector configuration is not the approved one", ErrConnectorRefused)
	case c.LegalEntityID != "" && !strings.EqualFold(c.LegalEntityID, legalEntityID):
		return fmt.Errorf("%w: connector belongs to another legal entity", ErrConnectorRefused)
	case c.EndpointURL == "":
		return fmt.Errorf("%w: connector has no endpoint", ErrConnectorRefused)
	}
	for _, s := range c.Scopes {
		if s == requiredScope {
			return nil
		}
	}
	return fmt.Errorf("%w: connector lacks the %s scope", ErrConnectorRefused, requiredScope)
}

// Caller is who a downstream call is made for.
type Caller struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

// ConnectorResolver finds a connector. Failure means the request provably was not sent.
type ConnectorResolver interface {
	Resolve(ctx context.Context, connectorID string, c Caller) (Connector, error)
}

// CredentialSource releases the provider credential for one call, or signs for HMAC providers.
type CredentialSource interface {
	Use(ctx context.Context, policyRef string, c Caller) (adapter.Credential, error)
	Sign(ctx context.Context, policyRef string, c Caller) func(context.Context, []byte) (string, error)
}

// ── connectivity-api-bridge-svc ─────────────────────────────────────────────

// ConnectorHTTP resolves connectors from connectivity-api-bridge-svc.
type ConnectorHTTP struct {
	base string
	http *http.Client
}

func NewConnectorHTTP(base string) *ConnectorHTTP {
	return &ConnectorHTTP{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 4 * time.Second}}
}

func (c *ConnectorHTTP) Resolve(ctx context.Context, id string, caller Caller) (Connector, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Connector{}, fmt.Errorf("%w: connector id is not valid", ErrConnectorRefused)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/bridges/"+url.PathEscape(id), nil)
	if err != nil {
		return Connector{}, ErrConnectorDown
	}
	req.Header.Set("X-Tenant-Id", caller.TenantID)
	req.Header.Set("X-Principal-Id", caller.PrincipalID)
	if caller.CorrelationID != "" {
		req.Header.Set("X-Correlation-Id", caller.CorrelationID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Connector{}, ErrConnectorDown
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return Connector{}, fmt.Errorf("%w: connector not found", ErrConnectorRefused)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Connector{}, fmt.Errorf("%w: connector not visible to this tenant", ErrConnectorRefused)
	default:
		return Connector{}, ErrConnectorDown
	}
	var out Connector
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Connector{}, ErrConnectorDown
	}
	if !strings.EqualFold(out.ID, id) {
		return Connector{}, fmt.Errorf("%w: connector identity mismatch", ErrConnectorRefused)
	}
	return out, nil
}

// ── secret-vault-integration-svc ────────────────────────────────────────────

// VaultHTTP releases credentials through the vault's narrow use operation and signs through its sign
// operation. Principal is this service's workload identity, which the secret policy must allow.
type VaultHTTP struct {
	base      string
	principal string
	http      *http.Client
}

func NewVaultHTTP(base, principal string) *VaultHTTP {
	return &VaultHTTP{base: strings.TrimRight(base, "/"), principal: principal, http: &http.Client{Timeout: 4 * time.Second}}
}

func (v *VaultHTTP) post(ctx context.Context, path string, body any, c Caller, purpose string) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	reqID := uuid.NewString()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", c.TenantID)
	// the vault authenticates the workload; the claimed requester must equal it
	req.Header.Set("X-Principal-Id", v.principal)
	req.Header.Set("X-Request-Id", reqID)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", purpose)
	req.Header.Set("Idempotency-Key", reqID)
	if c.CorrelationID != "" {
		req.Header.Set("X-Correlation-Id", c.CorrelationID)
	}
	return v.http.Do(req)
}

func (v *VaultHTTP) Use(ctx context.Context, policyRef string, c Caller) (adapter.Credential, error) {
	resp, err := v.post(ctx, "/v1/secrets/use", map[string]string{
		"secret_policy_id": policyRef, "requested_by_principal_id": v.principal, "request_id": uuid.NewString(),
	}, c, "provider-credential-use")
	if err != nil {
		return adapter.Credential{}, ErrCredentialDown
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return adapter.Credential{}, ErrCredentialRefused
	default:
		return adapter.Credential{}, ErrCredentialDown
	}
	var out struct {
		CredentialBase64 string `json:"credential_base64"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return adapter.Credential{}, ErrCredentialDown
	}
	cred, err := adapter.DecodeCredential(out.CredentialBase64)
	if err != nil {
		return adapter.Credential{}, ErrCredentialDown
	}
	return cred, nil
}

// Sign returns a signer bound to a policy; the vault computes the HMAC and the key never leaves it. The vault
// signs under the policy's secret path, so the policy reference is resolved there.
func (v *VaultHTTP) Sign(_ context.Context, policyRef string, c Caller) func(context.Context, []byte) (string, error) {
	return func(ctx context.Context, body []byte) (string, error) {
		resp, err := v.post(ctx, "/v1/secrets/sign", map[string]string{
			"secret_policy_id": policyRef, "requested_by_principal_id": v.principal, "request_id": uuid.NewString(),
			"message_base64": base64.StdEncoding.EncodeToString(body),
		}, c, "provider-request-signing")
		if err != nil {
			return "", ErrCredentialDown
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", ErrCredentialRefused
		}
		var out struct {
			SignatureHex string `json:"signature_hex"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.SignatureHex == "" {
			return "", errors.New("vault returned no signature")
		}
		return out.SignatureHex, nil
	}
}

// Errors shared by every adapter that resolves a connector and a credential. Each maps to one response and
// none carries connector, provider or credential content.
var (
	ErrConnectorRefused  = errors.New("the connector is not usable for this payment")
	ErrConnectorDown     = errors.New("the connector service is unavailable")
	ErrCredentialRefused = errors.New("the vault refused the provider credential")
	ErrCredentialDown    = errors.New("the vault is unavailable")
)

// ErrSecretUnknown: the vault has no usable callback secret for this tenant and path (or refused to release it).
// ErrSecretUnavailable: the vault could not answer; the caller should make the sender retry.
var (
	ErrSecretUnknown     = errors.New("no callback secret is configured for this tenant and provider")
	ErrSecretUnavailable = errors.New("callback secret unavailable")
)

// VaultSecrets fetches callback secrets through the vault's narrow use operation. The secret is held for the one
// verification and zeroed by the caller after.
type VaultSecrets struct {
	Base      string
	Principal string
	HTTP      *http.Client
}

func NewVaultSecrets(base, principal string) *VaultSecrets {
	return &VaultSecrets{Base: strings.TrimRight(base, "/"), Principal: principal, HTTP: &http.Client{Timeout: 4 * time.Second}}
}

func (v *VaultSecrets) Secrets(ctx context.Context, tenantID, secretPath string) ([][]byte, error) {
	raw, _ := json.Marshal(map[string]string{
		"secret_path": secretPath, "requested_by_principal_id": v.Principal, "request_id": uuid.NewString(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.Base+"/v1/secrets/use", bytes.NewReader(raw))
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	rid := uuid.NewString()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", v.Principal)
	req.Header.Set("X-Request-Id", rid)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Purpose-Context", "provider-callback-verification")
	req.Header.Set("Idempotency-Key", rid)
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		return nil, ErrSecretUnknown
	default:
		return nil, ErrSecretUnavailable
	}
	var out struct {
		CredentialBase64 string `json:"credential_base64"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return nil, ErrSecretUnavailable
	}
	b, err := base64.StdEncoding.DecodeString(out.CredentialBase64)
	if err != nil || len(b) == 0 {
		return nil, ErrSecretUnavailable
	}
	return [][]byte{b}, nil
}
