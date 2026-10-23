package inbound

import (
	"context"
	"errors"

	"zoiko.io/eventing/adapter/ext"
)

// VaultSecrets releases credential material through the vault's narrow "use" operation: the secret is held for the
// one verification and zeroed by the caller. A refused, foreign or unknown reference is an authentication failure;
// an outage is a retryable one.
type VaultSecrets struct {
	Vault     *ext.VaultHTTP
	Principal string
}

// NewVaultSecrets builds the adapter for the vault at base, acting as principal.
func NewVaultSecrets(base, principal string) *VaultSecrets {
	return &VaultSecrets{Vault: ext.NewVaultHTTP(base, principal), Principal: principal}
}

// Secret implements Secrets.
func (v *VaultSecrets) Secret(ctx context.Context, tenantID, credentialRef string) ([]byte, error) {
	cred, err := v.Vault.Use(ctx, credentialRef, ext.Caller{TenantID: tenantID, PrincipalID: v.Principal})
	switch {
	case err == nil:
		return cred.Secret, nil
	case errors.Is(err, ext.ErrCredentialDown):
		return nil, ErrSecretUnavailable
	default:
		return nil, ErrAuthFailed
	}
}

var _ Secrets = (*VaultSecrets)(nil)
