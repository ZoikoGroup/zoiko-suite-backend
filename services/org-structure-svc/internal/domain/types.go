package domain

import "time"

type Department struct {
	DepartmentID       string    `json:"department_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	Name               string    `json:"name"`
	Code               string    `json:"code"`
	CostCenterCode     string    `json:"cost_center_code"`
	ParentDepartmentID *string   `json:"parent_department_id,omitempty"`
	Status             string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Position struct {
	PositionID       string    `json:"position_id"`
	TenantID         string    `json:"tenant_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	DepartmentID     string    `json:"department_id"`
	DepartmentName   string    `json:"department_name,omitempty"`
	Title            string    `json:"title"`
	Code             string    `json:"code"`
	JobLevel         string    `json:"job_level"`
	MaxHeadcount     int       `json:"max_headcount"`
	CurrentHeadcount int       `json:"current_headcount"`
	Status           string    `json:"status"` // ACTIVE, INACTIVE
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type OrgAssignment struct {
	AssignmentID      string     `json:"assignment_id"`
	TenantID          string     `json:"tenant_id"`
	EmployeeID        string     `json:"employee_id"`
	DepartmentID      string     `json:"department_id"`
	DepartmentName    string     `json:"department_name,omitempty"`
	PositionID        string     `json:"position_id"`
	PositionTitle     string     `json:"position_title,omitempty"`
	ManagerEmployeeID *string    `json:"manager_employee_id,omitempty"`
	EffectiveFrom     string     `json:"effective_from"`
	EffectiveTo       *string    `json:"effective_to,omitempty"`
	Status            string     `json:"status"` // ACTIVE, SUPERSEDED
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateDepartmentRequest struct {
	LegalEntityID      string  `json:"legal_entity_id"`
	Name               string  `json:"name"`
	Code               string  `json:"code"`
	CostCenterCode     string  `json:"cost_center_code"`
	ParentDepartmentID *string `json:"parent_department_id,omitempty"`
}

type CreatePositionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	DepartmentID  string `json:"department_id"`
	Title         string `json:"title"`
	Code          string `json:"code"`
	JobLevel      string `json:"job_level"`
	MaxHeadcount  int    `json:"max_headcount"`
}

type AssignEmployeeRequest struct {
	EmployeeID        string  `json:"employee_id"`
	DepartmentID      string  `json:"department_id"`
	PositionID        string  `json:"position_id"`
	ManagerEmployeeID *string `json:"manager_employee_id,omitempty"`
	EffectiveFrom     string  `json:"effective_from"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrDepartmentNotFound      = errorString("department not found")
	ErrPositionNotFound        = errorString("position not found")
	ErrAssignmentNotFound      = errorString("org assignment not found")
	ErrEmployeeNotFound       = errorString("employee not found or inactive")
	ErrManagerNotFound        = errorString("manager employee not found or inactive")
	ErrAuthorizationDenied    = errorString("authorization denied for org action")
	ErrAuthzServiceUnavailable= errorString("authorization-svc unavailable")
	ErrIdentityMissing        = errorString("caller identity missing")
	ErrStoreUnavailable       = errorString("org structure store unavailable")
)