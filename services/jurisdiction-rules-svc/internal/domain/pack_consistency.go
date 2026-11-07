package domain

import (
	"fmt"
	"sort"
	"time"
)

// ZS-JUR-001 s10, s21 and s23: two release-time controls that sit across packs.
//
//	CrossPackConflicts   s10 "Two active rules overlap: certification blocker unless explicit precedence/combination rule exists." Precedence is declared inside a pack;
//	                     between packs the resolver refuses ANY overlap (JUR-NEG-24), so a conflicting
//	                     pack would only fail when used. Checking at release stops it reaching production at all.
//	SupersessionBlockers s23 "Superseded: newer version exists; historical resolution still permitted." A superseded version is no longer
//	                     eligible for NEW decisions, so superseding is allowed only when nothing loses service: the newer version must be
//	                     released, cover the same period, jurisdictions, rules and obligations, and be deployed wherever the older is.

// PackWithDoc is a pack version's window and verified artifact content.
type PackWithDoc struct {
	Ref     string
	Version string
	From    time.Time
	To      *time.Time
	Doc     *ArtifactDoc
}

type span struct {
	from time.Time
	to   *time.Time
}

func (s span) intersect(o span) span {
	r := span{from: s.from, to: s.to}
	if o.from.After(r.from) {
		r.from = o.from
	}
	if o.to != nil && (r.to == nil || o.to.Before(*r.to)) {
		t := *o.to
		r.to = &t
	}
	return r
}

// overlaps reports whether the half-open span is non-empty.
func (s span) overlaps() bool { return s.to == nil || s.from.Before(*s.to) }

func (s span) String() string {
	if s.to == nil {
		return s.from.Format(dateLayout) + " onward"
	}
	return s.from.Format(dateLayout) + " to " + s.to.Format(dateLayout)
}

// hierarchy answers whether two jurisdictions are the same or one is an ancestor of the other, over the union of the packs' scopes.
type hierarchy map[string]ArtifactScopeEntry

func newHierarchy(docs ...*ArtifactDoc) hierarchy {
	h := hierarchy{}
	for _, d := range docs {
		for _, j := range d.Jurisdictions {
			h[j.ID] = j
		}
	}
	return h
}

func (h hierarchy) ancestorOf(anc, desc string) bool {
	cur, ok := h[desc]
	for hops := 0; ok && hops < 64; hops++ {
		if cur.ParentID == nil {
			return false
		}
		if *cur.ParentID == anc {
			return true
		}
		cur, ok = h[*cur.ParentID]
	}
	return false
}

// related is true when a jurisdiction chain can contain both: the runtime would then see both rules as candidates.
func (h hierarchy) related(a, b string) bool {
	return a == b || h.ancestorOf(a, b) || h.ancestorOf(b, a)
}

func (h hierarchy) code(id string) string {
	if j, ok := h[id]; ok {
		return j.Code
	}
	return id
}

// CrossPackConflicts lists rules and obligation rules of doc that overlap, for a related jurisdiction and an overlapping period, a rule of a
// DIFFERENT pack. Declared precedence does not resolve it: the resolver honours precedence only among the rules of one pack and refuses any
// overlap between packs (JUR-NEG-24), so checking at release must match. Versions of the same pack never conflict with each other: the resolver
// takes the highest version in force per pack. The result is sorted and deduplicated, empty when there is no conflict.
func CrossPackConflicts(doc PackWithDoc, others []PackWithDoc) []string {
	set := map[string]bool{}
	for _, o := range others {
		if o.Ref == doc.Ref || o.Doc == nil || doc.Doc == nil {
			continue
		}
		h := newHierarchy(doc.Doc, o.Doc)
		docWin, otherWin := span{doc.From, doc.To}, span{o.From, o.To}
		for _, a := range doc.Doc.Rules {
			for _, b := range o.Doc.Rules {
				if a.RuleDomain != b.RuleDomain || a.RuleCode != b.RuleCode || !h.related(a.JurisdictionID, b.JurisdictionID) {
					continue
				}
				win := span{a.EffectiveFrom, a.EffectiveTo}.intersect(docWin).intersect(span{b.EffectiveFrom, b.EffectiveTo}).intersect(otherWin)
				if !win.overlaps() {
					continue
				}
				set[fmt.Sprintf("rule %s/%s (%s) also defined by %s@%s (%s) over %s: the resolver honours precedence only inside one pack, so rules that must override each other belong in the same pack",
					a.RuleDomain, a.RuleCode, h.code(a.JurisdictionID), o.Ref, o.Version, h.code(b.JurisdictionID), win)] = true
			}
		}
		for _, a := range doc.Doc.Obligations {
			for _, b := range o.Doc.Obligations {
				if a.ObligationCode != b.ObligationCode || !h.related(a.JurisdictionID, b.JurisdictionID) {
					continue
				}
				win := span{a.EffectiveFrom, a.EffectiveTo}.intersect(docWin).intersect(span{b.EffectiveFrom, b.EffectiveTo}).intersect(otherWin)
				if !win.overlaps() {
					continue
				}
				set[fmt.Sprintf("obligation %s (%s) also defined by %s@%s (%s) over %s: obligation rules carry no precedence, so an overlap is always ambiguous",
					a.ObligationCode, h.code(a.JurisdictionID), o.Ref, o.Version, h.code(b.JurisdictionID), win)] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RingRegion is one place a version is deployed.
type RingRegion struct{ Ring, Region string }

// SupersessionInput is everything the supersede check needs about the older and the newer version.
type SupersessionInput struct {
	Old, New               PackWithDoc
	OldStatus, NewStatus   string
	OldScope, NewScope     []string
	OldDeploys, NewDeploys []RingRegion
}

type ruleKey struct{ kind, jurisdiction, domain, code string }

func carried(d *ArtifactDoc) map[ruleKey]bool {
	byID := map[string]string{}
	for _, j := range d.Jurisdictions {
		byID[j.ID] = j.Code
	}
	out := map[ruleKey]bool{}
	for _, r := range d.Rules {
		out[ruleKey{"rule", byID[r.JurisdictionID], r.RuleDomain, r.RuleCode}] = true
	}
	for _, o := range d.Obligations {
		out[ruleKey{"obligation", byID[o.JurisdictionID], "", o.ObligationCode}] = true
	}
	return out
}

// SupersessionBlockers returns why an older version cannot be marked SUPERSEDED by a newer one; empty means it can. Superseded versions stay
// resolvable for pinned replay but are not eligible for new decisions, so nothing the older version served may be left without a servant.
func SupersessionBlockers(in SupersessionInput) []string {
	var b []string
	if in.OldStatus != "RELEASED" {
		b = append(b, fmt.Sprintf("only a RELEASED version can be superseded, %s@%s is %s", in.Old.Ref, in.Old.Version, in.OldStatus))
	}
	if in.NewStatus != "RELEASED" {
		b = append(b, fmt.Sprintf("newer_version_not_released: %s@%s is %s", in.New.Ref, in.New.Version, in.NewStatus))
	}
	if in.Old.Ref != in.New.Ref {
		b = append(b, "the newer version must belong to the same pack")
	}
	if CompareVersions(in.New.Version, in.Old.Version) <= 0 {
		b = append(b, fmt.Sprintf("not_newer: %s is not later than %s", in.New.Version, in.Old.Version))
	}
	if in.New.From.After(in.Old.From) {
		b = append(b, fmt.Sprintf("period_not_covered: the newer version starts %s, after the older one (%s)", in.New.From.Format(dateLayout), in.Old.From.Format(dateLayout)))
	}
	if in.New.To != nil && (in.Old.To == nil || in.New.To.Before(*in.Old.To)) {
		end := "no end"
		if in.Old.To != nil {
			end = in.Old.To.Format(dateLayout)
		}
		b = append(b, fmt.Sprintf("period_not_covered: the newer version ends %s, before the older one (%s)", in.New.To.Format(dateLayout), end))
	}
	have := map[string]bool{}
	for _, c := range in.NewScope {
		have[c] = true
	}
	for _, c := range in.OldScope {
		if !have[c] {
			b = append(b, "jurisdiction_not_covered: "+c)
		}
	}
	if in.Old.Doc != nil && in.New.Doc != nil {
		newKeys := carried(in.New.Doc)
		var missing []string
		for k := range carried(in.Old.Doc) {
			if !newKeys[k] {
				name := k.code
				if k.domain != "" {
					name = k.domain + "/" + k.code
				}
				missing = append(missing, fmt.Sprintf("not_carried_forward: %s %s for %s", k.kind, name, k.jurisdiction))
			}
		}
		sort.Strings(missing)
		b = append(b, missing...)
	}
	deployed := map[RingRegion]bool{}
	for _, d := range in.NewDeploys {
		deployed[d] = true
	}
	var gaps []string
	for _, d := range in.OldDeploys {
		if !deployed[d] {
			gaps = append(gaps, fmt.Sprintf("newer_not_deployed: %s@%s is not ACTIVE in ring %s, region %s where the older one is", in.New.Ref, in.New.Version, d.Ring, d.Region))
		}
	}
	sort.Strings(gaps)
	return append(b, gaps...)
}
