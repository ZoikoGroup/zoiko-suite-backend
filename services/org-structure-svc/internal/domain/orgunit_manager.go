package domain

// ChangeManagerRequest names the unit's manager from EffectiveAt on. The
// manager is a reference to an employee; ORG-05 does not own the person.
type ChangeManagerRequest struct {
	ManagerEmployeeID string `json:"manager_employee_id"`
	EffectiveAt       string `json:"effective_at"`
	Reason            string `json:"reason"`
}

// ErrNoManagerChange: the named employee already manages the unit.
var ErrNoManagerChange = errorString("employee already manages this org unit")
