package domain

// ORG-04 graph rules (ZS-SVC-I-001 s4.4): "Cycles/contradictory overlapping
// control relationships blocked".
//
// These are pure functions over the ACTIVE relationships of one tenant, so they
// can be tested without a database. The store loads the active set inside the
// same transaction that holds the ownership write lock and calls them, which
// is what makes check-then-write atomic.
//
// Time matters. A relationship only constrains another while both are in force
// at once, so a cycle exists only if every edge on it shares a common instant,
// and a 60% holding that ended last year does not count against a 60% holding
// that starts today.

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrOwnershipCycle — the relationship would make an entity its own
	// ancestor at some instant.
	ErrOwnershipCycle = errors.New("ownership cycle")
	// ErrOwnershipOverlap — an active relationship of the same parent, child
	// and kind is already in force during part of this interval.
	ErrOwnershipOverlap = errors.New("overlapping relationship")
	// ErrOwnershipExceeds — at some instant the child's owners would hold more
	// than 100% between them.
	ErrOwnershipExceeds = errors.New("ownership exceeds 100%")
)

// interval is [from, to); a nil to is open-ended.
type interval struct {
	from time.Time
	to   *time.Time
}

func intervalOf(r OwnershipRelationship) interval { return interval{r.ValidFrom, r.ValidTo} }

// intersect returns the common part of two intervals and whether it is non-empty.
func (a interval) intersect(b interval) (interval, bool) {
	out := interval{from: a.from}
	if b.from.After(out.from) {
		out.from = b.from
	}
	switch {
	case a.to == nil:
		out.to = b.to
	case b.to == nil:
		out.to = a.to
	case a.to.Before(*b.to):
		out.to = a.to
	default:
		out.to = b.to
	}
	if out.to != nil && !out.to.After(out.from) {
		return interval{}, false
	}
	return out, true
}

func (a interval) contains(t time.Time) bool {
	return !t.Before(a.from) && (a.to == nil || t.Before(*a.to))
}

// CheckOwnershipGraph decides whether candidate may become ACTIVE alongside the
// tenant's current ACTIVE relationships. exclude names a relationship being
// replaced (a supersession), which no longer counts. It returns nil, or an
// error wrapping ErrOwnershipCycle, ErrOwnershipOverlap or ErrOwnershipExceeds.
func CheckOwnershipGraph(active []OwnershipRelationship, candidate OwnershipRelationship, exclude string) error {
	live := make([]OwnershipRelationship, 0, len(active))
	for _, r := range active {
		if r.RelationshipID == exclude || r.RelationshipID == candidate.RelationshipID {
			continue
		}
		live = append(live, r)
	}
	if err := checkOverlap(live, candidate); err != nil {
		return err
	}
	if err := checkCycle(live, candidate); err != nil {
		return err
	}
	for _, pick := range []struct {
		name string
		get  func(OwnershipRelationship) *float64
	}{
		{"ownership_pct", func(r OwnershipRelationship) *float64 { return r.OwnershipPct }},
		{"voting_pct", func(r OwnershipRelationship) *float64 { return r.VotingPct }},
	} {
		if err := checkTotal(live, candidate, pick.name, pick.get); err != nil {
			return err
		}
	}
	return nil
}

func checkOverlap(live []OwnershipRelationship, c OwnershipRelationship) error {
	for _, r := range live {
		if r.ParentLegalEntityID != c.ParentLegalEntityID || r.ChildLegalEntityID != c.ChildLegalEntityID ||
			r.RelationshipKind != c.RelationshipKind {
			continue
		}
		if _, ok := intervalOf(r).intersect(intervalOf(c)); ok {
			return fmt.Errorf("%w: relationship %s already joins these entities as %s during part of this interval; supersede or end it instead",
				ErrOwnershipOverlap, r.RelationshipID, r.RelationshipKind)
		}
	}
	return nil
}

// checkCycle looks for a path child -> ... -> parent through relationships that
// are all in force together with the candidate. Together with the candidate's
// own edge parent -> child that path would close a loop.
func checkCycle(live []OwnershipRelationship, c OwnershipRelationship) error {
	const maxDepth = 128
	var walk func(node string, span interval, depth int) bool
	walk = func(node string, span interval, depth int) bool {
		if depth > maxDepth {
			return false
		}
		for _, r := range live {
			if r.ParentLegalEntityID != node {
				continue
			}
			next, ok := span.intersect(intervalOf(r))
			if !ok {
				continue
			}
			if r.ChildLegalEntityID == c.ParentLegalEntityID || walk(r.ChildLegalEntityID, next, depth+1) {
				return true
			}
		}
		return false
	}
	if walk(c.ChildLegalEntityID, intervalOf(c), 0) {
		return fmt.Errorf("%w: %s would become its own ancestor (via %s)",
			ErrOwnershipCycle, c.ParentLegalEntityID, c.ChildLegalEntityID)
	}
	return nil
}

// checkTotal sums one percentage over the child's other owners at every instant
// the candidate is in force, and refuses a total above 100. The total only
// changes where a relationship starts, so those instants are the ones to test.
func checkTotal(live []OwnershipRelationship, c OwnershipRelationship, field string, get func(OwnershipRelationship) *float64) error {
	mine := get(c)
	if mine == nil || c.RelationshipKind != RelationshipOwnership {
		return nil
	}
	var others []OwnershipRelationship
	for _, r := range live {
		if r.ChildLegalEntityID == c.ChildLegalEntityID && r.RelationshipKind == RelationshipOwnership && get(r) != nil {
			if _, ok := intervalOf(r).intersect(intervalOf(c)); ok {
				others = append(others, r)
			}
		}
	}
	points := []time.Time{c.ValidFrom}
	for _, r := range others {
		if r.ValidFrom.After(c.ValidFrom) {
			points = append(points, r.ValidFrom)
		}
	}
	const epsilon = 1e-9
	for _, t := range points {
		if !intervalOf(c).contains(t) {
			continue
		}
		total := *mine
		for _, r := range others {
			if intervalOf(r).contains(t) {
				total += *get(r)
			}
		}
		if total > 100+epsilon {
			return fmt.Errorf("%w: at %s the owners of %s would hold %.4f%% by %s",
				ErrOwnershipExceeds, t.UTC().Format("2006-01-02"), c.ChildLegalEntityID, total, field)
		}
	}
	return nil
}
