package domain

import (
	"errors"
	"time"
)

var (
	ErrObligationNotFound         = errors.New("obligation not found")
	ErrObligationAlreadyFulfilled = errors.New("obligation is already fulfilled")
	ErrObligationAlreadyBreached  = errors.New("obligation is already breached")
)

// ObligationType is deliberately narrow: this service owns contract-derived
// obligations only (docs/original_doc/zoiko_suite_doc4.txt §12.3 "Obligation
// Tracking Service... Extracts, stores, and manages contract-linked
// obligations"). Statutory/regulatory/internal-policy obligations are
// obligations-svc's domain (§6.6/§8.5) — the platform-wide umbrella the spec
// says to route to "by source class" (doc4.txt:531). REGULATORY, STATUTORY,
// and INTERNAL_POLICY previously existed here too, duplicating that scope;
// removed rather than left unused, since an unused-but-present enum value
// invites exactly the duplication this is fixing.
type ObligationType string

const (
	ObligationTypeContractual ObligationType = "CONTRACTUAL"
)

// SourceType enumerates where a contract-linked obligation was extracted
// from — always something contract-lifecycle-svc owns. A source type outside
// this set (e.g. a bare regulation, with no contract involved) belongs in
// obligations-svc instead.
type SourceType string

const (
	SourceTypeContract SourceType = "CONTRACT"
	SourceTypeClause   SourceType = "CLAUSE"
)

func (t SourceType) Valid() bool {
	switch t {
	case SourceTypeContract, SourceTypeClause:
		return true
	default:
		return false
	}
}

type ObligationStatus string

const (
	ObligationStatusPending    ObligationStatus = "PENDING"
	ObligationStatusInProgress ObligationStatus = "IN_PROGRESS"
	ObligationStatusFulfilled  ObligationStatus = "FULFILLED"
	ObligationStatusBreached   ObligationStatus = "BREACHED"
	ObligationStatusWaived     ObligationStatus = "WAIVED"
)

type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "LOW"
	RiskLevelMedium   RiskLevel = "MEDIUM"
	RiskLevelHigh     RiskLevel = "HIGH"
	RiskLevelCritical RiskLevel = "CRITICAL"
)

type Obligation struct {
	ObligationID    string           `json:"obligation_id"`
	TenantID        string           `json:"tenant_id"`
	LegalEntityID   string           `json:"legal_entity_id"`
	SourceType      string           `json:"source_type"` // CONTRACT, REGULATION, CLAUSE
	SourceID        string           `json:"source_id"`
	Title           string           `json:"title"`
	Description     string           `json:"description,omitempty"`
	ObligationType  ObligationType   `json:"obligation_type"`
	RiskLevel       RiskLevel        `json:"risk_level"`
	Status          ObligationStatus `json:"status"`
	DueDate         string           `json:"due_date"`
	AssignedTo      string           `json:"assigned_to,omitempty"`
	FulfilledAt     *time.Time       `json:"fulfilled_at,omitempty"`
	FulfilledBy     *string          `json:"fulfilled_by,omitempty"`
	FulfillmentNote *string          `json:"fulfillment_note,omitempty"`
	EffectiveFrom   string           `json:"effective_from"`
	EffectiveTo     *string          `json:"effective_to,omitempty"`
	CreatedBy       string           `json:"created_by"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

type CreateObligationRequest struct {
	LegalEntityID  string         `json:"legal_entity_id"`
	SourceType     string         `json:"source_type"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Description    string         `json:"description,omitempty"`
	ObligationType ObligationType `json:"obligation_type"`
	RiskLevel      RiskLevel      `json:"risk_level"`
	DueDate        string         `json:"due_date"`
	AssignedTo     string         `json:"assigned_to,omitempty"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	CreatedBy      string         `json:"created_by"`
}

type UpdateObligationRequest struct {
	Title          string           `json:"title,omitempty"`
	Description    string           `json:"description,omitempty"`
	ObligationType ObligationType   `json:"obligation_type,omitempty"`
	RiskLevel      RiskLevel        `json:"risk_level,omitempty"`
	DueDate        string           `json:"due_date,omitempty"`
	AssignedTo     string           `json:"assigned_to,omitempty"`
	Status         ObligationStatus `json:"status,omitempty"`
	EffectiveTo    *string          `json:"effective_to,omitempty"`
	UpdatedBy      string           `json:"updated_by"`
}

type FulfillObligationRequest struct {
	FulfilledBy     string `json:"fulfilled_by"`
	FulfillmentNote string `json:"fulfillment_note,omitempty"`
}
