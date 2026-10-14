package domain

import (
	"fmt"
	"time"
)

type CertStatus string
type PolicyAction string

const (
	CertStatusActive  CertStatus = "ACTIVE"
	CertStatusExpired CertStatus = "EXPIRED"
	CertStatusRevoked CertStatus = "REVOKED"
	CertStatusPending CertStatus = "PENDING"
)

const (
	PolicyAllow PolicyAction = "ALLOW"
	PolicyDeny  PolicyAction = "DENY"
)

type MtlsCertificate struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ServiceName   string `json:"service_name"`
	CommonName    string `json:"common_name"`
	Issuer        string `json:"issuer"`
	SerialNumber  string `json:"serial_number"`
	Fingerprint   string `json:"fingerprint"`
	// CertificatePEM is the real, CA-signed X.509 certificate in PEM
	// encoding. Safe to store and return on every read — it's the public
	// half. The matching private key is NEVER stored here and is only ever
	// returned once, at issuance/rotation time — see ProvisionCertResult.
	CertificatePEM string `json:"certificate_pem"`
	// PriorSerialNumbers are the serials this record carried before each
	// rotation. Rotation does not invalidate the previous certificate (the
	// service keeps presenting it until it adopts the new one), so a later
	// revocation must cover every serial ever issued under this record — not
	// only the current one, which is all the record used to remember.
	PriorSerialNumbers []string   `json:"prior_serial_numbers,omitempty"`
	ValidFrom          time.Time  `json:"valid_from"`
	ValidTo        time.Time  `json:"valid_to"`
	RotationDays   int        `json:"rotation_days"`
	AutoRotate     bool       `json:"auto_rotate"`
	Status         CertStatus `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ProvisionCertResult is returned exactly once, from the provision/rotate
// endpoints only — never from GetCert or ListCerts. It carries the leaf
// private key the requesting service needs to actually present this
// certificate over TLS. A real CA does not retain leaf private keys, and
// neither does this store: PrivateKeyPEM exists only in this one response.
type ProvisionCertResult struct {
	Certificate   MtlsCertificate `json:"certificate"`
	PrivateKeyPEM string          `json:"private_key_pem"`
	CACertPEM     string          `json:"ca_certificate_pem"`
}

type CommunicationPolicy struct {
	ID            string       `json:"id"`
	TenantID      string       `json:"tenant_id"`
	PolicyName    string       `json:"policy_name"`
	SourceService string       `json:"source_service"`
	TargetService string       `json:"target_service"`
	Action        PolicyAction `json:"action"`
	RequiresMtls  bool         `json:"requires_mtls"`
	CreatedAt     time.Time    `json:"created_at"`
}

// AllSerialNumbers is the current serial plus every superseded one.
func (c *MtlsCertificate) AllSerialNumbers() []string {
	out := make([]string, 0, len(c.PriorSerialNumbers)+1)
	out = append(out, c.PriorSerialNumbers...)
	if c.SerialNumber != "" {
		out = append(out, c.SerialNumber)
	}
	return out
}

type ProvisionCertRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	ServiceName   string `json:"service_name"`
	CommonName    string `json:"common_name"`
	RotationDays  int    `json:"rotation_days"`
	AutoRotate    bool   `json:"auto_rotate"`
}

type CreatePolicyRequest struct {
	PolicyName    string       `json:"policy_name"`
	SourceService string       `json:"source_service"`
	TargetService string       `json:"target_service"`
	Action        PolicyAction `json:"action"`
	RequiresMtls  bool         `json:"requires_mtls"`
}

func (r *ProvisionCertRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.ServiceName == "" {
		return fmt.Errorf("service_name is required")
	}
	if r.CommonName == "" {
		return fmt.Errorf("common_name is required")
	}
	if r.RotationDays <= 0 {
		r.RotationDays = 90
	}
	return nil
}
