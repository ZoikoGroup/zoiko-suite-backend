package domain

import (
	"fmt"
	"time"
)

type KeyModel string
type KeyProvider string
type KeyState string

const (
	ModelSystemManaged KeyModel = "SYSTEM_MANAGED"
	ModelBYOK          KeyModel = "BYOK"
	ModelHYOK          KeyModel = "HYOK"
)

const (
	ProviderAWSKMS     KeyProvider = "AWS_KMS"
	ProviderAzureKV    KeyProvider = "AZURE_KEY_VAULT"
	ProviderGCPKMS     KeyProvider = "GCP_KMS"
	ProviderVault      KeyProvider = "HASHICORP_VAULT"
	ProviderExternalHSM KeyProvider = "EXTERNAL_HSM"
)

const (
	StateEnabled  KeyState = "ENABLED"
	StateDisabled KeyState = "DISABLED"
	StatePendingRotation KeyState = "PENDING_ROTATION"
	StateRevoked  KeyState = "REVOKED"
)

type CustomerKey struct {
	ID            string      `json:"id"`
	TenantID      string      `json:"tenant_id"`
	LegalEntityID string      `json:"legal_entity_id"`
	KeyAlias      string      `json:"key_alias"`
	KeyModel      KeyModel    `json:"key_model"`
	KeyProvider   KeyProvider `json:"key_provider"`
	ExternalKeyARN string     `json:"external_key_arn"` // e.g. arn:aws:kms:us-east-1:123456789012:key/...
	KeyVersion    int         `json:"key_version"`
	State         KeyState    `json:"state"`
	RotationCount int         `json:"rotation_count"`
	LastRotatedAt *time.Time  `json:"last_rotated_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

type RegisterKeyRequest struct {
	LegalEntityID  string      `json:"legal_entity_id"`
	KeyAlias       string      `json:"key_alias"`
	KeyModel       KeyModel    `json:"key_model"`
	KeyProvider    KeyProvider `json:"key_provider"`
	ExternalKeyARN string      `json:"external_key_arn"`
}

func (r *RegisterKeyRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.KeyAlias == "" {
		return fmt.Errorf("key_alias is required")
	}
	if r.KeyModel == "" {
		r.KeyModel = ModelBYOK
	}
	if r.KeyProvider == "" {
		return fmt.Errorf("key_provider is required")
	}
	if (r.KeyModel == ModelBYOK || r.KeyModel == ModelHYOK) && r.ExternalKeyARN == "" {
		return fmt.Errorf("external_key_arn is required for BYOK/HYOK models")
	}
	return nil
}
