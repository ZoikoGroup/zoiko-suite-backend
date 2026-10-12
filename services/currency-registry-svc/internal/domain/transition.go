package domain

// transitions is the lifecycle table: Known -> Supported/Restricted ->
// Retired, plus Supported <-> Restricted. RETIRED is terminal. A self
// transition is never legal (it would bump a version for no change).
//
// KNOWN -> RETIRED is deliberately absent (spec 4.12 draws the path through
// Supported/Restricted); see SPEC_DEVIATIONS.md.
var transitions = map[Status]map[Status]bool{
	StatusKnown:      {StatusSupported: true, StatusRestricted: true},
	StatusSupported:  {StatusRestricted: true, StatusRetired: true},
	StatusRestricted: {StatusSupported: true, StatusRetired: true},
	StatusRetired:    {},
}

// CanTransition reports whether from -> to is a legal lifecycle move.
func CanTransition(from, to Status) bool {
	return transitions[from][to]
}

// ValidateTransition returns an INVALID_TRANSITION error for an illegal move.
func ValidateTransition(from, to Status) error {
	if !from.Valid() || !to.Valid() {
		return Errf(CodeInvalidTransition, "unknown status in transition %q -> %q", from, to)
	}
	if !CanTransition(from, to) {
		return Errf(CodeInvalidTransition, "cannot move a currency from %s to %s", from, to)
	}
	return nil
}
