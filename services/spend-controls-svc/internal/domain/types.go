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

// PolicyUsageTotal is committed spend and refusals for one policy, aggregated in
// the database over the window that policy is enforced on.
//
// Exists so a caller can draw a budget meter without fetching the underlying
// consumption rows — which grow one per spend check, forever — and so the figure it
// shows is the same one enforcement compares against.
type PolicyUsageTotal struct {
	SpendPolicyID string  `json:"spend_policy_id"`
	Consumed      float64 `json:"consumed"`
	RefusedCount  int     `json:"refused_count"`
}

// CreatePolicyResponse embeds the policy so no existing key moves, and adds how
// many limits this one replaced. Without it the console had to read the register
// first to find out — a second round trip whose answer could change in between.
type CreatePolicyResponse struct {
	*SpendPolicy
	Superseded int `json:"superseded"`
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
	// ProjectedTotal is prior consumption plus this attempt — the number the
	// threshold was actually compared against. Without it a BLOCKED decision
	// states a threshold and a prior total and leaves the reader to add up the
	// figures the service already computed.
	ProjectedTotal float64 `json:"projected_total"`
	// CurrencyCode is the currency the decision was made in. Every figure above
	// is in it, and the service refuses to compare across currencies, so naming
	// it removes any question about which one they are.
	CurrencyCode string `json:"currency_code,omitempty"`
	// Replayed reports that this response came from a stored prior decision
	// rather than a fresh evaluation. A retry must not read as a second
	// successful spend against the budget.
	Replayed bool `json:"replayed"`
}

// SpendEvaluation is the input to the one atomic store operation that decides
// and records a spend check.
//
// It exists because the decision and the record have to be the same database
// transaction: evaluating in one statement and inserting in another lets two
// concurrent checks both read the same prior total, both conclude they fit, and
// both be recorded — so a threshold can be exceeded by any number of
// simultaneous requests. For a service whose entire purpose is refusing spend
// above a limit, that is the defect that matters most.
type SpendEvaluation struct {
	LegalEntityID   string
	Category        string
	Amount          float64
	CurrencyCode    string
	SourceReference string
	CorrelationID   string
	PrincipalID     string
}

// SpendDecision is the outcome of an atomic evaluation.
type SpendDecision struct {
	Outcome          string // ALLOWED or BLOCKED
	Basis            string
	Policy           *SpendPolicy // nil when no policy is configured
	PriorConsumption float64
	ProjectedTotal   float64
	ConsumptionID    string
	// Replayed is true when an existing record for this correlation id was
	// returned instead of a new evaluation.
	Replayed bool
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrPolicyNotFound          = errorString("spend policy not found")
	ErrAuthorizationDenied     = errorString("authorization denied for spend controls action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("spend controls store unavailable")

	// ErrTenantMissing is returned by every store method when no tenant scope
	// reached it. Distinct from ErrStoreUnavailable, which it used to be
	// reported as: a request with no X-Tenant-Id header answered 503, so a
	// missing header read as a database outage and sent the caller to check
	// infrastructure over a header they had simply not sent.
	ErrTenantMissing = errorString("no tenant scope on the request")

	// ErrCurrencyMismatch is returned when a check's currency differs from its
	// policy's. Nothing in this platform holds an FX rate, so the two amounts
	// cannot be compared — and comparing them as bare numbers, which is what
	// used to happen, silently lets a 9,000 USD spend through a 10,000 GBP
	// threshold and books it against that budget. A refusal the caller can see
	// is the only honest answer.
	ErrCurrencyMismatch = errorString("spend currency does not match the policy currency")
)
