package domain

// ORG-04 read surfaces (ZS-SVC-I-001 s4.4): GetOwnershipGraph, GetGroupAsOf,
// ListEntityParents and ExplainControlChain.
//
// All four are pure functions over the relationships in force at one instant,
// so the answer for a date is deterministic ("deterministic hierarchy queries")
// and testable without a database. The store supplies the rows; nothing here
// reads the clock.
//
// A relationship is in force at t when it was approved (ACTIVE) or has since
// been SUPERSEDED or ENDED, and valid_from <= t < valid_to. A superseded or
// ended row carries the date it stopped, so the graph at a past date shows the
// relationship as it was then, not as it is now.

import (
	"sort"
	"time"
)

// InForceAt reports whether r is part of the group structure at t.
func InForceAt(r OwnershipRelationship, t time.Time) bool {
	switch r.Status {
	case OwnershipActive, OwnershipSuperseded, OwnershipEnded:
	default:
		return false // PROPOSED and REJECTED never took effect
	}
	return intervalOf(r).contains(t)
}

// SortOwnership orders relationships deterministically: the same rows always
// render the same graph, whatever order the database returned them in.
func SortOwnership(rs []OwnershipRelationship) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		switch {
		case a.ParentLegalEntityID != b.ParentLegalEntityID:
			return a.ParentLegalEntityID < b.ParentLegalEntityID
		case a.ChildLegalEntityID != b.ChildLegalEntityID:
			return a.ChildLegalEntityID < b.ChildLegalEntityID
		case a.RelationshipKind != b.RelationshipKind:
			return a.RelationshipKind < b.RelationshipKind
		case !a.ValidFrom.Equal(b.ValidFrom):
			return a.ValidFrom.Before(b.ValidFrom)
		}
		return a.RelationshipID < b.RelationshipID
	})
}

func inForce(all []OwnershipRelationship, t time.Time) []OwnershipRelationship {
	var out []OwnershipRelationship
	for _, r := range all {
		if InForceAt(r, t) {
			out = append(out, r)
		}
	}
	SortOwnership(out)
	return out
}

// OwnershipGraphView is the group structure at one instant.
type OwnershipGraphView struct {
	AsOf time.Time `json:"as_of"`
	// RootLegalEntityID is set when the view was limited to one entity and
	// everything it owns or controls.
	RootLegalEntityID string                  `json:"root_legal_entity_id,omitempty"`
	Entities          []string                `json:"entities"`
	Relationships     []OwnershipRelationship `json:"relationships"`
}

// GraphAsOf returns every relationship in force at asOf; with a root, only those
// reachable downwards from it.
func GraphAsOf(all []OwnershipRelationship, asOf time.Time, root string) OwnershipGraphView {
	edges := inForce(all, asOf)
	if root != "" {
		edges = reachableDown(edges, root)
	}
	view := OwnershipGraphView{AsOf: asOf.UTC(), RootLegalEntityID: root, Relationships: edges}
	if view.Relationships == nil {
		view.Relationships = []OwnershipRelationship{}
	}
	view.Entities = entitiesOf(edges, root)
	return view
}

func entitiesOf(edges []OwnershipRelationship, extra ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, e := range edges {
		add(e.ParentLegalEntityID)
		add(e.ChildLegalEntityID)
	}
	for _, id := range extra {
		add(id)
	}
	sort.Strings(out)
	return out
}

// reachableDown keeps the edges on some path starting at root.
func reachableDown(edges []OwnershipRelationship, root string) []OwnershipRelationship {
	frontier, seen := []string{root}, map[string]bool{root: true}
	var out []OwnershipRelationship
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		for _, e := range edges {
			if e.ParentLegalEntityID != node {
				continue
			}
			out = append(out, e)
			if !seen[e.ChildLegalEntityID] {
				seen[e.ChildLegalEntityID] = true
				frontier = append(frontier, e.ChildLegalEntityID)
			}
		}
	}
	SortOwnership(out)
	return out
}

// ParentsAsOf lists an entity's direct owners and controllers at asOf.
func ParentsAsOf(all []OwnershipRelationship, asOf time.Time, entity string) []OwnershipRelationship {
	out := []OwnershipRelationship{}
	for _, e := range inForce(all, asOf) {
		if e.ChildLegalEntityID == entity {
			out = append(out, e)
		}
	}
	return out
}

// GroupView is the corporate group an entity belongs to at one instant: every
// entity connected to it through relationships in force, and the entities at
// the top (those with no parent inside the group).
type GroupView struct {
	AsOf            time.Time               `json:"as_of"`
	LegalEntityID   string                  `json:"legal_entity_id"`
	UltimateParents []string                `json:"ultimate_parents"`
	Members         []string                `json:"members"`
	Relationships   []OwnershipRelationship `json:"relationships"`
}

// GroupAsOf reconstructs the group of entity at asOf. An entity with no
// relationships in force is a group of one: it is its own ultimate parent.
func GroupAsOf(all []OwnershipRelationship, asOf time.Time, entity string) GroupView {
	edges := inForce(all, asOf)

	// Connected component, ignoring edge direction.
	member := map[string]bool{entity: true}
	for changed := true; changed; {
		changed = false
		for _, e := range edges {
			p, c := member[e.ParentLegalEntityID], member[e.ChildLegalEntityID]
			if p != c {
				member[e.ParentLegalEntityID], member[e.ChildLegalEntityID] = true, true
				changed = true
			}
		}
	}

	view := GroupView{AsOf: asOf.UTC(), LegalEntityID: entity, Relationships: []OwnershipRelationship{}}
	hasParent := map[string]bool{}
	for _, e := range edges {
		if member[e.ParentLegalEntityID] {
			view.Relationships = append(view.Relationships, e)
			hasParent[e.ChildLegalEntityID] = true
		}
	}
	view.Members = []string{}
	view.UltimateParents = []string{}
	for id := range member {
		view.Members = append(view.Members, id)
		if !hasParent[id] {
			view.UltimateParents = append(view.UltimateParents, id)
		}
	}
	sort.Strings(view.Members)
	sort.Strings(view.UltimateParents)
	return view
}

// ChainLink is one relationship on a path, as it was at the as-of date. It
// deliberately carries no beneficial notes: this view is for explaining control,
// not for reading the sensitive attachments.
type ChainLink struct {
	RelationshipID      string           `json:"relationship_id"`
	ParentLegalEntityID string           `json:"parent_legal_entity_id"`
	ChildLegalEntityID  string           `json:"child_legal_entity_id"`
	RelationshipKind    RelationshipKind `json:"relationship_kind"`
	OwnershipPct        *float64         `json:"ownership_pct,omitempty"`
	VotingPct           *float64         `json:"voting_pct,omitempty"`
	ControlType         *ControlType     `json:"control_type,omitempty"`
	Controlling         bool             `json:"controlling"`
	EvidenceRef         string           `json:"evidence_ref"`
}

// ControlPath is one chain from an ultimate parent down to the entity.
type ControlPath struct {
	Links []ChainLink `json:"links"`
	// EffectiveOwnershipPct is the product of the equity along the path, as a
	// percentage; nil when the path crosses a CONTROL link, which carries no equity.
	EffectiveOwnershipPct *float64 `json:"effective_ownership_pct"`
	// Controls is true when every link on the path is controlling.
	Controls bool `json:"controls"`
}

// UltimateParentChain gathers every path from one ultimate parent.
type UltimateParentChain struct {
	UltimateParentLegalEntityID string        `json:"ultimate_parent_legal_entity_id"`
	EffectiveOwnershipPct       float64       `json:"effective_ownership_pct"`
	Controls                    bool          `json:"controls"`
	Paths                       []ControlPath `json:"paths"`
}

// ControlChainView explains who owns and controls an entity at one instant.
type ControlChainView struct {
	AsOf          time.Time             `json:"as_of"`
	LegalEntityID string                `json:"legal_entity_id"`
	Chains        []UltimateParentChain `json:"chains"`
}

// controlling is the rule for a single link: control-type relationships control;
// an equity link controls when it holds a majority of the votes (voting_pct,
// or ownership_pct where no separate voting figure is recorded).
func controlling(r OwnershipRelationship) bool {
	if r.RelationshipKind == RelationshipControl {
		return true
	}
	votes := r.VotingPct
	if votes == nil {
		votes = r.OwnershipPct
	}
	return votes != nil && *votes > 50
}

// ExplainControlChain finds every path from an ultimate parent down to entity
// through the relationships in force at asOf, with the effective ownership each
// path gives and whether it amounts to control.
//
// An entity with nobody above it has no chains. Effective ownership across
// several paths from one parent is the sum, capped at 100.
func ExplainControlChain(all []OwnershipRelationship, asOf time.Time, entity string) ControlChainView {
	edges := inForce(all, asOf)
	view := ControlChainView{AsOf: asOf.UTC(), LegalEntityID: entity, Chains: []UltimateParentChain{}}

	byChild := map[string][]OwnershipRelationship{}
	for _, e := range edges {
		byChild[e.ChildLegalEntityID] = append(byChild[e.ChildLegalEntityID], e)
	}

	paths := map[string][]ControlPath{} // ultimate parent -> paths
	var climb func(node string, below []OwnershipRelationship, visited map[string]bool)
	climb = func(node string, below []OwnershipRelationship, visited map[string]bool) {
		parents := byChild[node]
		if len(parents) == 0 {
			if len(below) > 0 {
				paths[node] = append(paths[node], buildPath(below))
			}
			return
		}
		for _, e := range parents {
			if visited[e.ParentLegalEntityID] {
				continue // cannot happen in a checked graph; never loop on a corrupt one
			}
			visited[e.ParentLegalEntityID] = true
			climb(e.ParentLegalEntityID, append([]OwnershipRelationship{e}, below...), visited)
			delete(visited, e.ParentLegalEntityID)
		}
	}
	climb(entity, nil, map[string]bool{entity: true})

	tops := make([]string, 0, len(paths))
	for top := range paths {
		tops = append(tops, top)
	}
	sort.Strings(tops)
	for _, top := range tops {
		chain := UltimateParentChain{UltimateParentLegalEntityID: top, Paths: paths[top]}
		for _, p := range chain.Paths {
			if p.EffectiveOwnershipPct != nil {
				chain.EffectiveOwnershipPct += *p.EffectiveOwnershipPct
			}
			chain.Controls = chain.Controls || p.Controls
		}
		if chain.EffectiveOwnershipPct > 100 {
			chain.EffectiveOwnershipPct = 100
		}
		view.Chains = append(view.Chains, chain)
	}
	return view
}

func buildPath(links []OwnershipRelationship) ControlPath {
	p := ControlPath{Controls: true}
	equity, hasEquity := 1.0, true
	for _, l := range links {
		link := ChainLink{
			RelationshipID: l.RelationshipID, ParentLegalEntityID: l.ParentLegalEntityID,
			ChildLegalEntityID: l.ChildLegalEntityID, RelationshipKind: l.RelationshipKind,
			OwnershipPct: l.OwnershipPct, VotingPct: l.VotingPct, ControlType: l.ControlType,
			Controlling: controlling(l), EvidenceRef: l.EvidenceRef,
		}
		p.Links = append(p.Links, link)
		p.Controls = p.Controls && link.Controlling
		if l.RelationshipKind != RelationshipOwnership || l.OwnershipPct == nil {
			hasEquity = false
			continue
		}
		equity *= *l.OwnershipPct / 100
	}
	if hasEquity {
		pct := equity * 100
		p.EffectiveOwnershipPct = &pct
	}
	return p
}
