package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func pcDay(s string) time.Time   { t, _ := time.Parse("2006-01-02", s); return t }
func pcDayp(s string) *time.Time { t := pcDay(s); return &t }
func pcInt(n int) *int           { return &n }
func pcStr(s string) *string     { return &s }

func pcScope(entries ...ArtifactScopeEntry) []ArtifactScopeEntry { return entries }

var (
	pcGB  = ArtifactScopeEntry{ID: "j-pcGB", Code: "GB"}
	pcENG = ArtifactScopeEntry{ID: "j-eng", Code: "GB-ENG", ParentID: pcStr("j-pcGB")}
	pcFR  = ArtifactScopeEntry{ID: "j-pcFR", Code: "FR"}
)

func pcRule(id, jur, dom, code, from string, to *time.Time) ArtifactRule {
	return ArtifactRule{RuleID: id, JurisdictionID: jur, RuleDomain: dom, RuleCode: code, EffectiveFrom: pcDay(from), EffectiveTo: to}
}

func pcPack(ref, ver, from string, to *time.Time, jur []ArtifactScopeEntry, rules []ArtifactRule, obl []ObligationRule) PackWithDoc {
	return PackWithDoc{Ref: ref, Version: ver, From: pcDay(from), To: to, Doc: &ArtifactDoc{PackRef: ref, PackVersion: ver, Jurisdictions: jur, Rules: rules, Obligations: obl}}
}

func TestCrossPackConflicts_FlagsUndeclaredOverlapAcrossPacks(t *testing.T) {
	a := pcPack("pack.a", "1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r1", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)}, nil)
	b := pcPack("pack.b", "1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2026-03-01", nil)}, nil)
	got := CrossPackConflicts(a, []PackWithDoc{b})
	if assert.Len(t, got, 1) {
		assert.Contains(t, got[0], "TAX/VAT_RATE")
		assert.Contains(t, got[0], "pack.b@1")
		assert.Contains(t, got[0], "2026-03-01 onward", "the overlapping period is stated")
	}
	// It is symmetric: checking the other pack against this one finds the same conflict.
	assert.Len(t, CrossPackConflicts(b, []PackWithDoc{a}), 1)
}

func TestCrossPackConflicts_IgnoresWhatCannotCollide(t *testing.T) {
	base := pcPack("pack.a", "1", "2026-01-01", nil, pcScope(pcGB, pcENG, pcFR), []ArtifactRule{pcRule("r1", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)}, nil)
	cases := map[string]PackWithDoc{
		"different code":       pcPack("pack.b", "1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "OTHER", "2026-01-01", nil)}, nil),
		"different domain":     pcPack("pack.b", "1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "PAYROLL", "VAT_RATE", "2026-01-01", nil)}, nil),
		"unrelated country":    pcPack("pack.b", "1", "2026-01-01", nil, pcScope(pcFR), []ArtifactRule{pcRule("r2", "j-pcFR", "TAX", "VAT_RATE", "2026-01-01", nil)}, nil),
		"rule ends first":      pcPack("pack.b", "1", "2025-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2025-01-01", pcDayp("2026-01-01"))}, nil),
		"pack window ends":     pcPack("pack.b", "1", "2025-01-01", pcDayp("2026-01-01"), pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2025-01-01", nil)}, nil),
		"same pack, other ver": pcPack("pack.a", "2", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)}, nil),
	}
	for name, other := range cases {
		assert.Empty(t, CrossPackConflicts(base, []PackWithDoc{other}), name)
	}
	// The boundary: one rule ending the day the other starts does not overlap (end exclusive).
	touching := pcPack("pack.b", "1", "2025-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2025-01-01", pcDayp("2026-01-01"))}, nil)
	assert.Empty(t, CrossPackConflicts(base, []PackWithDoc{touching}))
	overlap := pcPack("pack.b", "1", "2025-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r2", "j-pcGB", "TAX", "VAT_RATE", "2025-01-01", pcDayp("2026-01-02"))}, nil)
	assert.Len(t, CrossPackConflicts(base, []PackWithDoc{overlap}), 1, "one day of overlap is a conflict")
}

func TestCrossPackConflicts_ObligationRulesHaveNoPrecedence(t *testing.T) {
	ob := func(id, jur string, from string) ObligationRule {
		return ObligationRule{ObligationRuleID: id, JurisdictionID: jur, ObligationCode: "VAT_RETURN", EffectiveFrom: pcDay(from)}
	}
	a := pcPack("pack.a", "1", "2026-01-01", nil, pcScope(pcGB), nil, []ObligationRule{ob("o1", "j-pcGB", "2026-01-01")})
	b := pcPack("pack.b", "1", "2026-01-01", nil, pcScope(pcGB), nil, []ObligationRule{ob("o2", "j-pcGB", "2026-06-01")})
	got := CrossPackConflicts(a, []PackWithDoc{b})
	if assert.Len(t, got, 1) {
		assert.Contains(t, got[0], "obligation VAT_RETURN")
		assert.Contains(t, got[0], "no precedence")
	}
}

func pcSuperInput() SupersessionInput {
	old := pcPack("pack.a", "2026.08.1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r1", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)},
		[]ObligationRule{{ObligationRuleID: "o1", JurisdictionID: "j-pcGB", ObligationCode: "VAT_RETURN", EffectiveFrom: pcDay("2026-01-01")}})
	nw := pcPack("pack.a", "2026.08.2", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{pcRule("r9", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)},
		[]ObligationRule{{ObligationRuleID: "o9", JurisdictionID: "j-pcGB", ObligationCode: "VAT_RETURN", EffectiveFrom: pcDay("2026-01-01")}})
	return SupersessionInput{Old: old, New: nw, OldStatus: "RELEASED", NewStatus: "RELEASED", OldScope: []string{"GB"}, NewScope: []string{"GB"},
		OldDeploys: []RingRegion{{"ring0", "eu"}}, NewDeploys: []RingRegion{{"ring0", "eu"}, {"ring1", "eu"}}}
}

func TestSupersessionBlockers_AllowsASafeSupersession(t *testing.T) {
	assert.Empty(t, SupersessionBlockers(pcSuperInput()))
}

func TestSupersessionBlockers_EachGuaranteeIsEnforced(t *testing.T) {
	cases := map[string]struct {
		mod  func(*SupersessionInput)
		want string
	}{
		"old not released":   {func(i *SupersessionInput) { i.OldStatus = "CERTIFIED" }, "only a RELEASED version"},
		"new not released":   {func(i *SupersessionInput) { i.NewStatus = "CERTIFIED" }, "newer_version_not_released"},
		"not newer":          {func(i *SupersessionInput) { i.New.Version = "2026.08.1" }, "not_newer"},
		"numeric not text":   {func(i *SupersessionInput) { i.New.Version = "2026.08.0" }, "not_newer"},
		"other pack":         {func(i *SupersessionInput) { i.New.Ref = "pack.other" }, "same pack"},
		"starts later":       {func(i *SupersessionInput) { i.New.From = pcDay("2026-02-01") }, "period_not_covered: the newer version starts"},
		"ends earlier":       {func(i *SupersessionInput) { i.New.To = pcDayp("2027-01-01") }, "period_not_covered: the newer version ends"},
		"scope narrower":     {func(i *SupersessionInput) { i.OldScope = []string{"GB", "FR"} }, "jurisdiction_not_covered: FR"},
		"rule dropped":       {func(i *SupersessionInput) { i.New.Doc.Rules = nil }, "not_carried_forward: rule TAX/VAT_RATE for GB"},
		"obligation dropped": {func(i *SupersessionInput) { i.New.Doc.Obligations = nil }, "not_carried_forward: obligation VAT_RETURN for GB"},
		"deployment gap":     {func(i *SupersessionInput) { i.NewDeploys = nil }, "newer_not_deployed: pack.a@2026.08.2 is not ACTIVE in ring ring0, region eu"},
	}
	for name, c := range cases {
		in := pcSuperInput()
		c.mod(&in)
		got := strings.Join(SupersessionBlockers(in), " | ")
		assert.Contains(t, got, c.want, name)
	}

	// A newer version that ends later, or has no end, still covers an older one with an end.
	in := pcSuperInput()
	in.Old.To = pcDayp("2027-01-01")
	in.New.To = pcDayp("2027-06-01")
	assert.Empty(t, SupersessionBlockers(in))
	in.New.To = nil
	assert.Empty(t, SupersessionBlockers(in))
	// Reports every blocker at once, not just the first.
	in = pcSuperInput()
	in.NewStatus, in.NewDeploys, in.New.Doc.Rules = "CERTIFIED", nil, nil
	assert.GreaterOrEqual(t, len(SupersessionBlockers(in)), 3)
}

func TestCrossPackConflicts_ParentAndSubdivisionCollide_AndDeclaredPrecedenceDoesNotHelpAcrossPacks(t *testing.T) {
	national := func(prec *int) PackWithDoc {
		r := pcRule("r1", "j-pcGB", "TAX", "VAT_RATE", "2026-01-01", nil)
		r.Precedence = prec
		return pcPack("pack.nat", "1", "2026-01-01", nil, pcScope(pcGB), []ArtifactRule{r}, nil)
	}
	local := func(prec *int, supersedes *string) PackWithDoc {
		l := pcRule("r2", "j-eng", "TAX", "VAT_RATE", "2026-01-01", nil)
		l.Precedence, l.SupersedesRuleID = prec, supersedes
		return pcPack("pack.eng", "1", "2026-01-01", nil, pcScope(pcGB, pcENG), []ArtifactRule{l}, nil)
	}
	got := CrossPackConflicts(local(nil, nil), []PackWithDoc{national(nil)})
	if assert.Len(t, got, 1, "a subdivision rule and its parent's rule collide") {
		assert.Contains(t, got[0], "(GB-ENG)")
		assert.Contains(t, got[0], "(GB)")
	}
	// The resolver honours precedence only among the rules of ONE pack and refuses any overlap between packs, so declaring it across packs
	// resolves nothing at runtime. The release check must not pretend otherwise.
	assert.Len(t, CrossPackConflicts(local(pcInt(10), nil), []PackWithDoc{national(pcInt(5))}), 1, "distinct precedence across packs")
	assert.Len(t, CrossPackConflicts(local(nil, pcStr("r1")), []PackWithDoc{national(nil)}), 1, "a supersedes link across packs")
	assert.Contains(t, CrossPackConflicts(local(pcInt(10), nil), []PackWithDoc{national(pcInt(5))})[0], "belong in the same pack")
}
