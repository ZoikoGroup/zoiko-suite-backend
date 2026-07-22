package domain

import (
	"errors"
	"time"
)

var (
	ErrStatusRecordNotFound = errors.New("compliance status record not found")
	ErrGapNotFound          = errors.New("compliance gap not found")
	ErrGapAlreadyResolved   = errors.New("compliance gap is already resolved")
)

type OverallStatus string

const (
	StatusCompliant    OverallStatus = "COMPLIANT"
	StatusWarning      OverallStatus = "WARNING"
	StatusNonCompliant OverallStatus = "NON_COMPLIANT"
	StatusUnderReview  OverallStatus = "UNDER_REVIEW"
)

type GapSeverity string

const (
	SeverityLow      GapSeverity = "LOW"
	SeverityMedium   GapSeverity = "MEDIUM"
	SeverityHigh     GapSeverity = "HIGH"
	SeverityCritical GapSeverity = "CRITICAL"
)

type GapStatus string

const (
	GapOpen          GapStatus = "OPEN"
	GapInRemediation GapStatus = "IN_REMEDIATION"
	GapResolved      GapStatus = "RESOLVED"
	GapDismissed     GapStatus = "DISMISSED"
)

// ComplianceHealth represents an evaluated compliance state for an entity and domain.
type ComplianceHealth struct {
	StatusID             string        `json:"status_id"`
	TenantID             string        `json:"tenant_id"`
	LegalEntityID        string        `json:"legal_entity_id"`
	JurisdictionID       string        `json:"jurisdiction_id"`
	DomainName           string        `json:"domain_name"`
	OverallStatus        OverallStatus `json:"overall_status"`
	HealthScore          float64       `json:"health_score"`
	TotalObligations     int           `json:"total_obligations"`
	FulfilledObligations int           `json:"fulfilled_obligations"`
	PendingObligations   int           `json:"pending_obligations"`
	OverdueObligations   int           `json:"overdue_obligations"`
	OpenExceptions       int           `json:"open_exceptions"`
	LastEvaluatedAt      time.Time     `json:"last_evaluated_at"`
	Notes                string        `json:"notes,omitempty"`
	EffectiveFrom        string        `json:"effective_from"`
	EffectiveTo          *string       `json:"effective_to,omitempty"`
	CreatedBy            string        `json:"created_by"`
	CreatedAt            time.Time     `json:"created_at"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

// ComplianceGap represents an identified compliance deficit or exception.
type ComplianceGap struct {
	GapID           string      `json:"gap_id"`
	TenantID        string      `json:"tenant_id"`
	LegalEntityID   string      `json:"legal_entity_id"`
	JurisdictionID  string      `json:"jurisdiction_id"`
	DomainName      string      `json:"domain_name"`
	GapType         string      `json:"gap_type"`
	Severity        GapSeverity `json:"severity"`
	SourceReference string      `json:"source_reference,omitempty"`
	Description     string      `json:"description"`
	RemediationPlan string      `json:"remediation_plan,omitempty"`
	Status          GapStatus   `json:"status"`
	DetectedAt      time.Time   `json:"detected_at"`
	ResolvedAt      *time.Time  `json:"resolved_at,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

// EvaluateComplianceRequest is the payload to evaluate compliance status.
type EvaluateComplianceRequest struct {
	LegalEntityID        string `json:"legal_entity_id"`
	JurisdictionID       string `json:"jurisdiction_id"`
	DomainName           string `json:"domain_name"`
	TotalObligations     int    `json:"total_obligations"`
	FulfilledObligations int    `json:"fulfilled_obligations"`
	PendingObligations   int    `json:"pending_obligations"`
	OverdueObligations   int    `json:"overdue_obligations"`
	OpenExceptions       int    `json:"open_exceptions"`
	Notes                string `json:"notes,omitempty"`
	EffectiveFrom        string `json:"effective_from"`
	CreatedBy            string `json:"created_by"`
}

// CreateGapRequest is the payload to log a new compliance gap.
type CreateGapRequest struct {
	LegalEntityID   string      `json:"legal_entity_id"`
	JurisdictionID  string      `json:"jurisdiction_id"`
	DomainName      string      `json:"domain_name"`
	GapType         string      `json:"gap_type"`
	Severity        GapSeverity `json:"severity"`
	SourceReference string      `json:"source_reference,omitempty"`
	Description     string      `json:"description"`
	RemediationPlan string      `json:"remediation_plan,omitempty"`
}

// ResolveGapRequest is the payload to mark a gap resolved.
type ResolveGapRequest struct {
	RemediationNotes string `json:"remediation_notes,omitempty"`
}

// CalculateHealthScore computes numeric health score (0-100) and overall status.
func (c *ComplianceHealth) CalculateHealthScore() {
	if c.TotalObligations == 0 {
		c.HealthScore = 100.00
		c.OverallStatus = StatusCompliant
		return
	}

	score := (float64(c.FulfilledObligations) / float64(c.TotalObligations)) * 100.00
	// Deduct penalties for overdue obligations and open exceptions
	score -= float64(c.OverdueObligations) * 15.00
	score -= float64(c.OpenExceptions) * 10.00

	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	c.HealthScore = score

	switch {
	case score >= 90.00:
		c.OverallStatus = StatusCompliant
	case score >= 70.00:
		c.OverallStatus = StatusWarning
	default:
		c.OverallStatus = StatusNonCompliant
	}
}
