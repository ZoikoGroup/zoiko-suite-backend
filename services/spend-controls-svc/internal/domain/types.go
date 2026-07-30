package domain

import "time"

type SpendPolicy struct {
	SpendPolicyID        string    `json:"spend_policy_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	Category             string    `json:"category"`
	Period               string    `json:"period"` // PER_TRANSACTION, MONTHLY, ANNUAL
	ThresholdAmount      float64   `json:"threshold_amount"`
	CurrencyCode         string    `json:"currency_code"`
	ActiveFlag           bool      `json:"active_flag"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type SpendConsumption struct {
	ConsumptionID         string    `json:"consumption_id"`
	TenantID              string    `json:"tenant_id"`
	LegalEntityID         string    `json:"legal_entity_id"`
	SpendPolicyID         string    `json:"spend_policy_id"`
	Amount                float64   `json:"amount"`
	CurrencyCode          string    `json:"currency_code"`
	SourceReference       string    `json:"source_reference,omitempty"`
	CorrelationID         string    `json:"correlation_id"`
	DecisionOutcome       string    `json:"decision_outcome"` // ALLOWED, BLOCKED
	RecordedByPrincipalID string    `json:"recorded_by_principal_id"`
	RecordedAt            time.Time `json:"recorded_at"`
}

type CreatePolicyRequest struct {
	LegalEntityID   string  `json:"legal_entity_id"`
	Category        string  `json:"category"`
	Period          string  `json:"period"`
	ThresholdAmount float64 `json:"threshold_amount"`
	CurrencyCode    string  `json:"currency_code"`
}

type SpendCheckRequest struct {
	LegalEntityID   string  `json:"legal_entity_id"`
	Category        string  `json:"category"`
	Amount          float64 `json:"amount"`
	CurrencyCode    string  `json:"currency_code"`
	SourceReference string  `json:"source_reference,omitempty"`
	CorrelationID   string  `json:"correlation_id"`
}

type SpendCheckResponse struct {
	DecisionOutcome  string  `json:"decision_outcome"` // ALLOWED, BLOCKED
	DecisionBasis    string  `json:"decision_basis"`
	SpendPolicyID    string  `json:"spend_policy_id,omitempty"`
	PriorConsumption float64 `json:"prior_consumption"`
	ThresholdAmount  float64 `json:"threshold_amount,omitempty"`
	ConsumptionID    string  `json:"consumption_id,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrPolicyNotFound          = errorString("spend policy not found")
	ErrAuthorizationDenied     = errorString("authorization denied for spend controls action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("spend controls store unavailable")
)
