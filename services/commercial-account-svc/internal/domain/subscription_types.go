// Chunk 6 — Plans, Pricing & Entitlements (doc7 §B, §N1-N3, §U1).
package domain

import "time"

type CatalogStatus string

const (
	CatalogStatusDraft     CatalogStatus = "DRAFT"
	CatalogStatusPublished CatalogStatus = "PUBLISHED"
	CatalogStatusRetired   CatalogStatus = "RETIRED"
)

// PriceCatalog is one immutable released catalog version. §U1: never edited
// in place — a change is always a new version with its own effective dates.
type PriceCatalog struct {
	CatalogVersionID     string        `json:"catalog_version_id"`
	CatalogCode          string        `json:"catalog_code"`
	Status               CatalogStatus `json:"status"`
	EffectiveFrom        time.Time     `json:"effective_from"`
	EffectiveTo          *time.Time    `json:"effective_to,omitempty"`
	CreatedAt            time.Time     `json:"created_at"`
	CreatedByPrincipalID string        `json:"created_by_principal_id"`
}

type CreatePriceCatalogRequest struct {
	CatalogCode   string `json:"catalog_code"`
	EffectiveFrom string `json:"effective_from"`
	CorrelationID string `json:"correlation_id"`
}

// Plan belongs to exactly one catalog version. PlanCode/DisplayName are DATA
// per doc7 §B1 — no handler in this service switches on either value.
type Plan struct {
	PlanID                string             `json:"plan_id"`
	CatalogVersionID      string             `json:"catalog_version_id"`
	PlanCode              string             `json:"plan_code"`
	DisplayName           string             `json:"display_name"`
	BillingInterval       string             `json:"billing_interval"` // MONTHLY | ANNUAL — data, not an enum
	BasePriceAmount       float64            `json:"base_price_amount"`
	BasePriceCurrencyCode string             `json:"base_price_currency_code"`
	MarketScope           *string            `json:"market_scope,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
	CreatedByPrincipalID  string             `json:"created_by_principal_id"`
	Limits                []EntitlementLimit `json:"limits,omitempty"`
}

type CreatePlanRequest struct {
	CatalogVersionID      string  `json:"catalog_version_id"`
	PlanCode              string  `json:"plan_code"`
	DisplayName           string  `json:"display_name"`
	BillingInterval       string  `json:"billing_interval"`
	BasePriceAmount       float64 `json:"base_price_amount"`
	BasePriceCurrencyCode string  `json:"base_price_currency_code"`
	MarketScope           string  `json:"market_scope,omitempty"`
	CorrelationID         string  `json:"correlation_id"`
}

// EntitlementLimit is one metric's allowance under a plan — collectively a
// plan's limit rows are its "entitlement_set" (doc7 §28). LimitValue nil
// means unlimited, a real distinct fact from LimitValue == 0.
type EntitlementLimit struct {
	EntitlementLimitID string `json:"entitlement_limit_id"`
	PlanID             string `json:"plan_id"`
	MetricType         string `json:"metric_type"`
	LimitValue         *int64 `json:"limit_value,omitempty"`
}

type SetEntitlementLimitRequest struct {
	MetricType string `json:"metric_type"`
	LimitValue *int64 `json:"limit_value,omitempty"`
}

// SubscriptionStatus is doc7 §29's canonical Commercial entitlement state
// machine, verbatim — not a locally invented set of statuses.
type SubscriptionStatus string

const (
	SubscriptionStatusEvaluation SubscriptionStatus = "EVALUATION"
	SubscriptionStatusActive     SubscriptionStatus = "ACTIVE"
	SubscriptionStatusPastDue    SubscriptionStatus = "PAST_DUE"
	SubscriptionStatusRestricted SubscriptionStatus = "RESTRICTED"
	SubscriptionStatusSuspended  SubscriptionStatus = "SUSPENDED"
	SubscriptionStatusCanceled   SubscriptionStatus = "CANCELED"
	SubscriptionStatusTerminated SubscriptionStatus = "TERMINATED"
)

type CommercialSubscription struct {
	SubscriptionID           string             `json:"subscription_id"`
	CommercialAccountID      string             `json:"commercial_account_id"`
	PlanID                   string             `json:"plan_id"`
	CatalogVersionID         string             `json:"catalog_version_id"`
	BillingInterval          string             `json:"billing_interval"`
	Status                   SubscriptionStatus `json:"status"`
	RenewalDate              *time.Time         `json:"renewal_date,omitempty"`
	CanceledAt               *time.Time         `json:"canceled_at,omitempty"`
	ProcessorSubscriptionRef *string            `json:"processor_subscription_ref,omitempty"`
	CreatedAt                time.Time          `json:"created_at"`
	UpdatedAt                time.Time          `json:"updated_at"`
	CreatedByPrincipalID     string             `json:"created_by_principal_id"`
}

type CreateSubscriptionRequest struct {
	CommercialAccountID string `json:"commercial_account_id"`
	PlanID              string `json:"plan_id"`
	// StartAsEvaluation starts the subscription in EVALUATION rather than
	// ACTIVE — pair with a subsequent CreateEvaluationProgram call. Doc7
	// §B3: no free trial is assumed without explicit terms, so a bare
	// EVALUATION subscription with no evaluation_programs row is a real,
	// deliberately unusual state a caller can end up in if they skip that
	// second call — not silently converted to a trial for them.
	StartAsEvaluation bool   `json:"start_as_evaluation,omitempty"`
	CorrelationID     string `json:"correlation_id"`
}

// EvaluationProgram is the trial terms doc7 §B3 requires before any
// EVALUATION-status subscription may be treated as a real trial.
type EvaluationProgram struct {
	EvaluationProgramID  string    `json:"evaluation_program_id"`
	SubscriptionID       string    `json:"subscription_id"`
	DurationDays         int       `json:"duration_days"`
	PaymentRequired      bool      `json:"payment_required"`
	ConversionPolicy     string    `json:"conversion_policy"` // AUTO_CONVERT | MANUAL | EXPIRE — data
	ExpiryAction         string    `json:"expiry_action"`     // SUSPEND | CANCEL | CONVERT — data
	StartedAt            time.Time `json:"started_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

type CreateEvaluationProgramRequest struct {
	SubscriptionID   string `json:"subscription_id"`
	DurationDays     int    `json:"duration_days"`
	PaymentRequired  bool   `json:"payment_required,omitempty"`
	ConversionPolicy string `json:"conversion_policy"`
	ExpiryAction     string `json:"expiry_action"`
	CorrelationID    string `json:"correlation_id"`
}

// ContractEntitlementOverlay overrides one metric's limit for one commercial
// account, per doc7 §B6 — bespoke enterprise terms through an approved
// overlay, never a hidden code switch.
type ContractEntitlementOverlay struct {
	OverlayID             string     `json:"overlay_id"`
	CommercialAccountID   string     `json:"commercial_account_id"`
	MetricType            string     `json:"metric_type"`
	OverrideLimitValue    *int64     `json:"override_limit_value,omitempty"`
	LegalReference        *string    `json:"legal_reference,omitempty"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	ApprovedByPrincipalID string     `json:"approved_by_principal_id"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
}

type CreateOverlayRequest struct {
	CommercialAccountID   string `json:"commercial_account_id"`
	MetricType            string `json:"metric_type"`
	OverrideLimitValue    *int64 `json:"override_limit_value,omitempty"`
	LegalReference        string `json:"legal_reference,omitempty"`
	EffectiveFrom         string `json:"effective_from"`
	EffectiveTo           string `json:"effective_to,omitempty"`
	ApprovedByPrincipalID string `json:"approved_by_principal_id"`
	CorrelationID         string `json:"correlation_id"`
}

// UsageMeterEvent is a Plane-1 metering fact, doc7 §B7/§N3: operational
// telemetry is not commercial consent — this ledger, not raw telemetry, is
// what a catalog rate/allowance is ever applied against. UsageEventID is the
// caller-supplied idempotency key: a retried metering call can never
// double-count.
type UsageMeterEvent struct {
	UsageEventID   string    `json:"usage_event_id"`
	SubscriptionID string    `json:"subscription_id"`
	MetricType     string    `json:"metric_type"`
	Quantity       float64   `json:"quantity"`
	OccurredAt     time.Time `json:"occurred_at"`
	SourceService  string    `json:"source_service"`
	BillableState  string    `json:"billable_state"` // PENDING | VALIDATED | AGGREGATED | BILLED | REJECTED — data
	CreatedAt      time.Time `json:"created_at"`
}

type RecordUsageEventRequest struct {
	UsageEventID   string  `json:"usage_event_id"`
	SubscriptionID string  `json:"subscription_id"`
	MetricType     string  `json:"metric_type"`
	Quantity       float64 `json:"quantity"`
	SourceService  string  `json:"source_service"`
}

// SubscriptionChangeRequest is the persisted quote/preview step doc7 §B4-B5
// requires before an upgrade/downgrade actually changes entitlement.
type SubscriptionChangeRequest struct {
	ChangeRequestID        string     `json:"change_request_id"`
	SubscriptionID         string     `json:"subscription_id"`
	TargetPlanID           string     `json:"target_plan_id"`
	EffectiveAt            time.Time  `json:"effective_at"`
	Status                 string     `json:"status"` // PREVIEWED | CONFIRMED | APPLIED | CANCELED — data
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	CreatedAt              time.Time  `json:"created_at"`
	AppliedAt              *time.Time `json:"applied_at,omitempty"`
}

type PreviewChangeRequest struct {
	SubscriptionID string `json:"subscription_id"`
	TargetPlanID   string `json:"target_plan_id"`
	EffectiveAt    string `json:"effective_at,omitempty"`
	CorrelationID  string `json:"correlation_id"`
}

// ResolvedEntitlement is the answer to "what may this subscription's account
// actually do for this metric right now" — the plan's own limit, overridden
// by an active contract overlay if one exists. This is the one function
// every other Plane-1 consumer should call rather than reading plan limits
// or overlays directly.
type ResolvedEntitlement struct {
	MetricType string  `json:"metric_type"`
	LimitValue *int64  `json:"limit_value,omitempty"`
	Source     string  `json:"source"` // "PLAN" | "OVERLAY"
	OverlayID  *string `json:"overlay_id,omitempty"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrPriceCatalogNotFound      = errorString("price catalog not found")
	ErrPlanNotFound              = errorString("plan not found")
	ErrSubscriptionNotFound      = errorString("subscription not found")
	ErrEvaluationProgramNotFound = errorString("evaluation program not found")
	ErrChangeRequestNotFound     = errorString("change request not found")
	// ErrActiveSubscriptionExists is returned when a commercial account
	// already has a non-terminal subscription — doc7 §P3/§33: two
	// simultaneously active subscriptions on one account is the
	// double-billing risk, not a valid state.
	ErrActiveSubscriptionExists  = errorString("conflict: commercial account already has a non-terminal subscription")
	ErrDuplicateUsageEvent       = errorString("usage event already recorded")
	ErrInvalidChangeRequestState = errorString("change request is not in a state that allows this action")
)
