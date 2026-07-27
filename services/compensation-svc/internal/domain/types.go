package domain

import "time"

type CompensationStructure struct {
	StructureID        string    `json:"structure_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	Name               string    `json:"name"`
	PayType            string    `json:"pay_type"` // SALARY, HOURLY
	MinAmount          float64   `json:"min_amount"`
	MaxAmount          float64   `json:"max_amount"`
	Currency           string    `json:"currency"`
	OvertimeMultiplier float64   `json:"overtime_multiplier"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type WageRevision struct {
	RevisionID    string    `json:"revision_id"`
	TenantID      string    `json:"tenant_id"`
	EmployeeID    string    `json:"employee_id"`
	StructureID   *string   `json:"structure_id,omitempty"`
	PayType       string    `json:"pay_type"` // SALARY, HOURLY
	Amount        float64   `json:"amount"`
	Currency      string    `json:"currency"`
	EffectiveFrom string    `json:"effective_from"` // YYYY-MM-DD
	EffectiveTo   *string   `json:"effective_to,omitempty"` // YYYY-MM-DD
	Reason        string    `json:"reason"`
	RevisedBy     string    `json:"revised_by"`
	Status        string    `json:"status"` // ACTIVE, SUPERSEDED
	CreatedAt     time.Time `json:"created_at"`
}

type BonusGrant struct {
	GrantID    string    `json:"grant_id"`
	TenantID   string    `json:"tenant_id"`
	EmployeeID string    `json:"employee_id"`
	BonusType  string    `json:"bonus_type"` // PERFORMANCE, ANNUAL, SIGNING, RETENTION
	Amount     float64   `json:"amount"`
	Currency   string    `json:"currency"`
	GrantDate  string    `json:"grant_date"` // YYYY-MM-DD
	Status     string    `json:"status"`     // PENDING, APPROVED, PAID, CANCELLED
	ApprovedBy *string   `json:"approved_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type CreateStructureRequest struct {
	LegalEntityID      string   `json:"legal_entity_id"`
	Name               string   `json:"name"`
	PayType            string   `json:"pay_type"`
	MinAmount          float64  `json:"min_amount"`
	MaxAmount          float64  `json:"max_amount"`
	Currency           string   `json:"currency"`
	OvertimeMultiplier *float64 `json:"overtime_multiplier,omitempty"`
}

type ReviseWageRequest struct {
	EmployeeID    string  `json:"employee_id"`
	StructureID   *string `json:"structure_id,omitempty"`
	PayType       string  `json:"pay_type"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	EffectiveFrom string  `json:"effective_from"`
	Reason        string  `json:"reason"`
}

type GrantBonusRequest struct {
	EmployeeID string  `json:"employee_id"`
	BonusType  string  `json:"bonus_type"`
	Amount     float64 `json:"amount"`
	Currency   string  `json:"currency"`
	GrantDate  string  `json:"grant_date"`
}

type ApproveBonusRequest struct {
	ConfirmationNote string `json:"confirmation_note,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrStructureNotFound       = errorString("compensation structure not found")
	ErrWageRevisionNotFound    = errorString("active wage revision not found for employee")
	ErrBonusNotFound           = errorString("bonus grant not found")
	ErrInvalidBonusStatus      = errorString("invalid bonus grant status for operation")
	ErrEmployeeNotFound        = errorString("employee not found or inactive")
	ErrAuthorizationDenied     = errorString("authorization denied for compensation action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("compensation store unavailable")
)