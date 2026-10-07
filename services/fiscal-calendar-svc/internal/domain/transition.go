package domain

// versionTransitions is the version lifecycle table:
// DRAFT -> APPROVED -> ACTIVE -> SUPERSEDED. SUPERSEDED is terminal and there
// is no way back: a correction is a new version, never an edit.
var versionTransitions = map[VersionStatus]map[VersionStatus]bool{
	VersionDraft:      {VersionApproved: true},
	VersionApproved:   {VersionActive: true},
	VersionActive:     {VersionSuperseded: true},
	VersionSuperseded: {},
}

// CanTransition reports whether from -> to is a legal version move.
func CanTransition(from, to VersionStatus) bool { return versionTransitions[from][to] }

// ValidateTransition returns INVALID_TRANSITION for an illegal version move.
func ValidateTransition(from, to VersionStatus) error {
	if !from.Valid() || !to.Valid() {
		return Errf(CodeInvalidTransition, "unknown status in transition %q -> %q", from, to)
	}
	if !CanTransition(from, to) {
		return Errf(CodeInvalidTransition, "cannot move a calendar version from %s to %s", from, to)
	}
	return nil
}

// ValidatePlanDecision returns INVALID_TRANSITION unless a plan is PROPOSED:
// APPROVED and REJECTED are both terminal.
func ValidatePlanDecision(from PlanStatus, to PlanStatus) error {
	if from != PlanProposed || (to != PlanApproved && to != PlanRejected) {
		return Errf(CodeInvalidTransition, "cannot move a transition plan from %s to %s", from, to)
	}
	return nil
}
