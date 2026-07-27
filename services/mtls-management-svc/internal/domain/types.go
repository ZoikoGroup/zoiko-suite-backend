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
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id"`
	LegalEntityID   string     `json:"legal_entity_id"`
	ServiceName     string     `json:"service_name"`
	CommonName      string     `json:"common_name"`
	Issuer          string     `json:"issuer"`
	SerialNumber    string     `json:"serial_number"`
	Fingerprint     string     `json:"fingerprint"`
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         time.Time  `json:"valid_to"`
	RotationDays    int        `json:"rotation_days"`
	AutoRotate      bool       `json:"auto_rotate"`
	Status          CertStatus `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
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

// GenerateCertificate simulates certificate provisioning (no real PKI dependency)
func GenerateCertificate(req *ProvisionCertRequest, tenantID string) *MtlsCertificate {
	now := time.Now()
	return &MtlsCertificate{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		ServiceName:   req.ServiceName,
		CommonName:    req.CommonName,
		Issuer:        "ZoikoSuite Internal CA",
		SerialNumber:  fmt.Sprintf("SN-%d", now.UnixNano()),
		Fingerprint:   fmt.Sprintf("SHA256:%x", now.UnixNano()*31337),
		ValidFrom:     now,
		ValidTo:       now.AddDate(0, 0, req.RotationDays),
		RotationDays:  req.RotationDays,
		AutoRotate:    req.AutoRotate,
		Status:        CertStatusActive,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}
