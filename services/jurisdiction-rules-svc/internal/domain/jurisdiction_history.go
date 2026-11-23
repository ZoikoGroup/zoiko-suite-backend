package domain

// REF-01 point-in-time reads and retirement evidence (ZS-SVC-I-001 s4.11).
//
// jurisdictions holds the current state; jurisdiction_versions (migration
// 000027) holds the effective-dated history beside it. The reads here answer
// from the history, so "what was this jurisdiction, and where did it sit in the
// hierarchy, on date X" has an exact answer even after it was renamed, moved or
// retired.

import (
	"fmt"
	"time"
)

// JurisdictionAsOf is a jurisdiction as it was at one instant.
type JurisdictionAsOf struct {
	JurisdictionID   string `json:"jurisdiction_id"`
	JurisdictionCode string `json:"jurisdiction_code"`

	// The attributes of the version in force at AsOf.
	JurisdictionName     string  `json:"jurisdiction_name"`
	JurisdictionType     string  `json:"jurisdiction_type"`
	ParentJurisdictionID *string `json:"parent_jurisdiction_id"`
	AuthorityType        string  `json:"authority_type"`

	AsOf      time.Time  `json:"as_of"`
	VersionNo int        `json:"version_no"`
	ValidFrom time.Time  `json:"valid_from"`
	ValidTo   *time.Time `json:"valid_to"`

	// Retired is true when the jurisdiction has since been retired, which is
	// not the same as being retired at AsOf: it was in force then.
	Retired                    bool       `json:"retired"`
	RetiredAt                  *time.Time `json:"retired_at,omitempty"`
	RetirementReason           *string    `json:"retirement_reason,omitempty"`
	SupersededByJurisdictionID *string    `json:"superseded_by_jurisdiction_id,omitempty"`
}

// HierarchyNode is one jurisdiction in a tree, as it stood at the as-of instant.
type HierarchyNode struct {
	JurisdictionID   string           `json:"jurisdiction_id"`
	JurisdictionCode string           `json:"jurisdiction_code"`
	JurisdictionName string           `json:"jurisdiction_name"`
	JurisdictionType string           `json:"jurisdiction_type"`
	AuthorityType    string           `json:"authority_type"`
	Depth            int              `json:"depth"`
	Children         []*HierarchyNode `json:"children"`
}

// Hierarchy is the tree below one jurisdiction at one instant.
type Hierarchy struct {
	AsOf time.Time      `json:"as_of"`
	Root *HierarchyNode `json:"root"`
	// Count is the number of jurisdictions in the tree, root included.
	Count int `json:"count"`
}

// HierarchyRow is one row of the flattened walk the store returns, before it is
// nested into a tree.
type HierarchyRow struct {
	HierarchyNode
	ParentJurisdictionID *string
}

// BuildHierarchy nests the flat rows of a walk. rows must contain the root (the
// row with Depth 0). It is pure so the tree rules are testable without a
// database. A row whose parent is not in the set is dropped: it was reached
// through a path the walk did not keep.
func BuildHierarchy(asOf time.Time, rows []HierarchyRow) *Hierarchy {
	h := &Hierarchy{AsOf: asOf.UTC()}
	byID := make(map[string]*HierarchyNode, len(rows))
	for i := range rows {
		n := rows[i].HierarchyNode
		n.Children = []*HierarchyNode{}
		byID[n.JurisdictionID] = &n
	}
	for _, r := range rows {
		n := byID[r.JurisdictionID]
		if r.Depth == 0 {
			h.Root = n
			continue
		}
		if r.ParentJurisdictionID == nil {
			continue
		}
		if parent, ok := byID[*r.ParentJurisdictionID]; ok {
			parent.Children = append(parent.Children, n)
		}
	}
	h.Count = len(byID)
	return h
}

// RetireJurisdictionParams is the RetireJurisdiction command: what is retired,
// by whom, why, on what evidence and (optionally) what replaces it.
type RetireJurisdictionParams struct {
	JurisdictionID string
	ActorID        string
	Reason         string
	EvidenceRef    string
	// SupersededBy names the active jurisdiction that takes over; nil when the
	// jurisdiction is simply withdrawn.
	SupersededBy *string
	// ExpectedRowVersion is the caller's If-Match version (API-3): when set,
	// the retirement applies only to that version. nil is unconditional.
	ExpectedRowVersion *int64
}

// NotEffectiveError says a jurisdiction exists but was not in force at the
// instant asked about, and when it was. Distinct from not-found: a caller
// resolving a historical document needs to know the code was real.
type NotEffectiveError struct {
	JurisdictionID string
	AsOf           time.Time
	EffectiveFrom  time.Time
	EffectiveTo    *time.Time
}

func (e *NotEffectiveError) Error() string {
	if e.AsOf.Before(e.EffectiveFrom) {
		return fmt.Sprintf("jurisdiction %s was not yet in force at %s (effective from %s)",
			e.JurisdictionID, e.AsOf.UTC().Format(time.RFC3339), e.EffectiveFrom.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("jurisdiction %s was no longer in force at %s (retired %s)",
		e.JurisdictionID, e.AsOf.UTC().Format(time.RFC3339), e.EffectiveTo.UTC().Format(time.RFC3339))
}

// Is lets errors.Is(err, ErrJurisdictionNotEffective) match.
func (e *NotEffectiveError) Is(target error) bool { return target == ErrJurisdictionNotEffective }

// ErrJurisdictionNotEffective is the sentinel NotEffectiveError matches.
var ErrJurisdictionNotEffective = errorString("jurisdiction was not in force at that time")

// ErrInvalidSuccessor: the replacement named for a retired jurisdiction does
// not exist, is not active, or is the jurisdiction itself.
var ErrInvalidSuccessor = errorString("superseded_by must name a different, active jurisdiction")

// ErrHierarchyTooLarge: the walk would return more jurisdictions than a single
// answer may carry. Narrow it with a smaller depth or a deeper root.
var ErrHierarchyTooLarge = errorString("the hierarchy is too large to return in one answer")

// ErrInvalidDepth: the depth asked for is outside 0..MaxHierarchyDepth.
var ErrInvalidDepth = errorString("depth must be between 0 and the maximum hierarchy depth")

// MaxHierarchyDepth bounds how far below the root a walk goes. Real jurisdiction
// hierarchies are three or four levels deep.
const MaxHierarchyDepth = 10

// MaxHierarchyRows bounds one answer.
const MaxHierarchyRows = 5000
