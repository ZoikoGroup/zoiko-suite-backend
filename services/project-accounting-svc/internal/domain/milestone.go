package domain

import "time"

// RecognitionMethodMilestone recognises revenue as the sum of milestones
// that are ACHIEVED (with evidence) and whose achievement has been APPROVED
// by a different principal. Billing never enters the figure.
const RecognitionMethodMilestone = "MILESTONE"

const (
	MilestoneStatusPlanned  = "PLANNED"
	MilestoneStatusAchieved = "ACHIEVED"
)

// Milestone is a contractual milestone of a project. It contributes to
// recognised revenue only when Status is ACHIEVED and ApprovedAt is set.
type Milestone struct {
	MilestoneID string  `json:"milestone_id"`
	TenantID    string  `json:"tenant_id,omitempty"`
	ProjectID   string  `json:"project_id"`
	Name        string  `json:"name"`
	Amount      float64 `json:"amount"`
	Status      string  `json:"status"`

	AchievementEvidenceRef string     `json:"achievement_evidence_ref,omitempty"`
	AchievedAt             *time.Time `json:"achieved_at,omitempty"`
	AchievedByPrincipalID  *string    `json:"achieved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`

	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// RunMilestone is one row of a recognition run's insert-only evidence.
type RunMilestone struct {
	RunID       string    `json:"run_id"`
	MilestoneID string    `json:"milestone_id"`
	Amount      float64   `json:"amount"`
	IncludedAt  time.Time `json:"included_at"`
}

type DefineMilestoneRequest struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
}

type MarkMilestoneAchievedRequest struct {
	AchievementEvidenceRef string `json:"achievement_evidence_ref"`
}

var (
	ErrMilestoneNotFound                 = errorString("milestone not found")
	ErrMilestoneNameRequired             = errorString("milestone name is required")
	ErrMilestoneAmountInvalid            = errorString("milestone amount must be positive")
	ErrDuplicateMilestoneName            = errorString("a milestone with this name already exists for this project")
	ErrMilestoneEvidenceRequired         = errorString("achievement_evidence_ref is required to mark a milestone achieved")
	ErrInvalidMilestoneTransition        = errorString("milestone is not in a status that allows this action")
	ErrSelfApprovalNotPermittedMilestone = errorString("the principal who marked this milestone achieved may not also approve it")
	// ErrMilestonesExceedContractValue is raised at calculation time: the
	// approved-achieved milestone total is greater than the run's declared
	// contract_value, so revenue would exceed the transaction price.
	ErrMilestonesExceedContractValue = errorString("approved milestone total exceeds the recognition run's contract_value")
)
