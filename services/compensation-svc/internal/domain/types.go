package domain

import "time"

// Typed enums for compensation service
type PayType string

const (
	PayTypeSalary PayType = "SALARY"
	PayTypeHourly PayType = "HOURLY"
)

type WageRevisionStatus string

const (
	WageRevisionStatusActive      WageRevisionStatus = "ACTIVE"
	WageRevisionStatusSuperseded  WageRevisionStatus = "SUPERSEDED"
)

type BonusStatus string

const (
	BonusStatusPending  BonusStatus = "PENDING"
	BonusStatusApproved BonusStatus = "APPROVED"
	BonusStatusPaid     BonusStatus = "PAID"
	BonusStatusCancelled BonusStatus = "CANCELLED"
)

type BonusType string

const (
	BonusTypePerformance BonusType = "PERFORMANCE"
	BonusTypeAnnual      BonusType = "ANNUAL"
	BonusTypeSigning     BonusType = "SIGNING"
	BonusTypeRetention   BonusType = "RETENTION"
)

type ComponentType string

const (
	ComponentEarning   ComponentType = "EARNING"
	ComponentDeduction ComponentType = "DEDUCTION"
)

type CalculationMethod string

const (
	MethodFixed         CalculationMethod = "FIXED"
	MethodPercentOfBase CalculationMethod = "PERCENT_OF_BASE"
)

type CompensationStructure struct {
	StructureID        string    `json:"structure_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	Name               string    `json:"name"`
	PayType            PayType   `json:"pay_type"`
	MinAmount          float64   `json:"min_amount"`
	MaxAmount          float64   `json:"max_amount"`
	Currency           string    `json:"currency"`
	OvertimeMultiplier float64   `json:"overtime_multiplier"`
	CorrelationID      string    `json:"correlation_id"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// Monolith-aligned fields
	Description        *string  `json:"description,omitempty"`
	GradeCode          *string  `json:"grade_code,omitempty"`
	LevelCode          *string  `json:"level_code,omitempty"`
	IsDefault          bool     `json:"is_default"`
	ApplicableLocation *string  `json:"applicable_location,omitempty"`
}

type WageRevision struct {
	RevisionID    string             `json:"revision_id"`
	TenantID      string             `json:"tenant_id"`
	EmployeeID    string             `json:"employee_id"`
	StructureID   *string            `json:"structure_id,omitempty"`
	PayType       PayType            `json:"pay_type"`
	Amount        float64            `json:"amount"`
	Currency      string             `json:"currency"`
	EffectiveFrom string             `json:"effective_from"`
	EffectiveTo   *string            `json:"effective_to,omitempty"`
	Reason        string             `json:"reason"`
	RevisedBy     string             `json:"revised_by"`
	Status        WageRevisionStatus `json:"status"`
	CorrelationID string             `json:"correlation_id"`
	CreatedAt     time.Time          `json:"created_at"`
	// Monolith-aligned fields
	PreviousAmount   *float64 `json:"previous_amount,omitempty"`
	PreviousCurrency *string  `json:"previous_currency,omitempty"`
	RevisionType     string   `json:"revision_type,omitempty"` // MERIT, PROMOTION, ADJUSTMENT, COLA, MARKET
	ApprovedBy       *string  `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
}

type BonusGrant struct {
	GrantID       string       `json:"grant_id"`
	TenantID      string       `json:"tenant_id"`
	EmployeeID    string       `json:"employee_id"`
	BonusType     BonusType    `json:"bonus_type"`
	Amount        float64      `json:"amount"`
	Currency      string       `json:"currency"`
	GrantDate     string       `json:"grant_date"`
	Status        BonusStatus  `json:"status"`
	ApprovedBy    *string      `json:"approved_by,omitempty"`
	CorrelationID string       `json:"correlation_id"`
	CreatedAt     time.Time    `json:"created_at"`
	// Monolith-aligned fields
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	PaymentReference *string   `json:"payment_reference,omitempty"`
	PayoutPeriod    *string    `json:"payout_period,omitempty"`
	TaxableAmount   *float64   `json:"taxable_amount,omitempty"`
	Conditions      *string    `json:"conditions,omitempty"`
	Notes           *string    `json:"notes,omitempty"`
}

type CreateStructureRequest struct {
	LegalEntityID      string   `json:"legal_entity_id"`
	Name               string   `json:"name"`
	PayType            PayType  `json:"pay_type"`
	MinAmount          float64  `json:"min_amount"`
	MaxAmount          float64  `json:"max_amount"`
	Currency           string   `json:"currency"`
	OvertimeMultiplier *float64 `json:"overtime_multiplier,omitempty"`
	CorrelationID      string   `json:"correlation_id"`
	// Monolith-aligned fields
	Description        *string `json:"description,omitempty"`
	GradeCode          *string `json:"grade_code,omitempty"`
	LevelCode          *string `json:"level_code,omitempty"`
	IsDefault          *bool   `json:"is_default,omitempty"`
	ApplicableLocation *string `json:"applicable_location,omitempty"`
}

type ReviseWageRequest struct {
	EmployeeID    string  `json:"employee_id"`
	StructureID   *string `json:"structure_id,omitempty"`
	PayType       PayType `json:"pay_type"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	EffectiveFrom string  `json:"effective_from"`
	Reason        string  `json:"reason"`
	CorrelationID string  `json:"correlation_id"`
	// Monolith-aligned fields
	RevisionType     *string `json:"revision_type,omitempty"`
	PreviousAmount   *float64 `json:"previous_amount,omitempty"`
	PreviousCurrency *string  `json:"previous_currency,omitempty"`
}

type GrantBonusRequest struct {
	EmployeeID    string    `json:"employee_id"`
	BonusType     BonusType `json:"bonus_type"`
	Amount        float64   `json:"amount"`
	Currency      string    `json:"currency"`
	GrantDate     string    `json:"grant_date"`
	CorrelationID string    `json:"correlation_id"`
	// Monolith-aligned fields
	PayoutPeriod  *string `json:"payout_period,omitempty"`
	TaxableAmount *float64 `json:"taxable_amount,omitempty"`
	Conditions    *string `json:"conditions,omitempty"`
	Notes         *string `json:"notes,omitempty"`
}

type ApproveBonusRequest struct {
	ConfirmationNote string `json:"confirmation_note,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrStructureNotFound        = errorString("compensation structure not found")
	ErrWageRevisionNotFound     = errorString("active wage revision not found for employee")
	ErrBonusNotFound            = errorString("bonus grant not found")
	ErrInvalidBonusStatus       = errorString("invalid bonus grant status for operation")
	ErrEmployeeNotFound         = errorString("employee not found or inactive")
	ErrAuthorizationDenied      = errorString("authorization denied for compensation action")
	ErrAuthzServiceUnavailable  = errorString("authorization-svc unavailable")
	ErrIdentityMissing          = errorString("caller identity missing")
	ErrStoreUnavailable         = errorString("compensation store unavailable")
	ErrInvalidPayType           = errorString("pay_type must be SALARY or HOURLY")
	ErrInvalidBonusType         = errorString("bonus_type must be PERFORMANCE, ANNUAL, SIGNING, or RETENTION")
	ErrInvalidWageRevisionStatus = errorString("wage revision status must be ACTIVE or SUPERSEDED")
	ErrInvalidBonusStatusValue  = errorString("bonus status must be PENDING, APPROVED, PAID, or CANCELLED")

	// ErrConcurrentWageRevision means another revision for the same
	// employee committed first (the unique index on (tenant_id,
	// employee_id) WHERE status='ACTIVE' rejected this insert) — a
	// genuine concurrent race, not a client error. Safe to retry.
	ErrConcurrentWageRevision = errorString("a concurrent wage revision for this employee was committed first, retry")

	// ErrEmployeeValidationFailed means employee-master-svc could not be
	// reached to confirm the employee's real legal entity — fail closed
	// rather than proceeding with a placeholder entity that authorization
	// would evaluate meaninglessly.
	ErrEmployeeValidationFailed = errorString("failed to verify employee's legal entity: employee-master-svc unavailable")
)