package domain

import "sort"

// OrgUnitNode is a unit with the units placed directly under it.
type OrgUnitNode struct {
	OrgUnit
	Children []*OrgUnitNode `json:"children"`
}

// BuildOrgUnitTree turns a flat snapshot (each unit carrying its parent as of
// the snapshot date) into a forest. A unit whose parent is not in the snapshot
// becomes a root: that is how a read that leaves out retired units, or a
// subtree, still returns every remaining unit. Siblings are ordered by code.
//
// The forest must account for every unit. A unit left unreachable from any
// root can only be part of a cycle, which the store refuses to create, so it is
// reported as ErrHierarchyLoop instead of being silently dropped.
func BuildOrgUnitTree(units []OrgUnit) ([]*OrgUnitNode, error) {
	nodes := make(map[string]*OrgUnitNode, len(units))
	for i := range units {
		nodes[units[i].UnitID] = &OrgUnitNode{OrgUnit: units[i], Children: []*OrgUnitNode{}}
	}

	var roots []*OrgUnitNode
	for i := range units {
		n := nodes[units[i].UnitID]
		if p := n.ParentUnitID; p != nil {
			if parent, ok := nodes[*p]; ok {
				parent.Children = append(parent.Children, n)
				continue
			}
		}
		roots = append(roots, n)
	}

	byCode := func(s []*OrgUnitNode) {
		sort.SliceStable(s, func(a, b int) bool {
			if s[a].Code != s[b].Code {
				return s[a].Code < s[b].Code
			}
			return s[a].UnitID < s[b].UnitID
		})
	}
	byCode(roots)

	reached := 0
	var walk func(n *OrgUnitNode)
	walk = func(n *OrgUnitNode) {
		reached++
		byCode(n.Children)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	if reached != len(units) {
		return nil, ErrHierarchyLoop
	}
	if roots == nil {
		roots = []*OrgUnitNode{}
	}
	return roots, nil
}
