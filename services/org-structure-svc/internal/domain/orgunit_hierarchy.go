package domain

// ReparentOrgUnitRequest moves a unit under a new parent from EffectiveAt on.
// A nil NewParentUnitID makes the unit a root.
type ReparentOrgUnitRequest struct {
	NewParentUnitID *string `json:"new_parent_unit_id"`
	EffectiveAt     string  `json:"effective_at"`
	Reason          string  `json:"reason"`
}

var (
	// ErrHierarchyLoop: the move would make a unit its own ancestor.
	ErrHierarchyLoop = errorString("reparent would create a hierarchy loop")
	// ErrNoHierarchyChange: the unit already sits under that parent.
	ErrNoHierarchyChange = errorString("org unit is already under that parent")
	// ErrEffectiveDateInvalid: the date is not after the start of the unit's
	// current placement. Changing history is a correction, not a reparent.
	ErrEffectiveDateInvalid = errorString("effective_at must be after the start of the unit's current placement")
)

// ReachesUnit reports whether target is start or one of start's ancestors,
// following parentOf upward. It is the in-memory form of the store's recursive
// query: reparenting unit U under P is a loop exactly when ReachesUnit(P, U).
// A visited set stops it on data that already contains a cycle.
func ReachesUnit(start, target string, parentOf func(id string) *string) bool {
	seen := map[string]bool{}
	for cur := &start; cur != nil && !seen[*cur]; cur = parentOf(*cur) {
		if *cur == target {
			return true
		}
		seen[*cur] = true
	}
	return false
}
