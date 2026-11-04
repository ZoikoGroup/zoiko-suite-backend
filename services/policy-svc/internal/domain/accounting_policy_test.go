package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	acctNow  = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	acctBook = "55555555-5555-5555-5555-555555555555"
)

func goodDraft() DraftAccountingPolicyParams {
	return DraftAccountingPolicyParams{
		TenantID: "11111111-1111-1111-1111-111111111111", PolicyDomain: "REVENUE_RECOGNITION", Framework: "IFRS",
		SemanticVersion: "2.1.0", Precedence: 5,
		FrameworkSource: "IFRS 15 (2018) paragraph 31", PolicyTextRef: "policy-manual/rev-rec/v2.1", Rationale: "annual review",
		Impact:        ImpactAnalysis{Summary: "contracts signed from 2027", AffectedPeriods: AffectsFutureOnly, ClosedHistoryUnchanged: true},
		EffectiveFrom: acctNow.AddDate(0, 3, 0),
	}
}

func problems(p DraftAccountingPolicyParams) string { return strings.Join(p.Validate(acctNow), " | ") }

func TestDraftAccounting_AWellFormedDraftPasses(t *testing.T) {
	p := goodDraft()
	book := acctBook
	p.BookID, p.JurisdictionID, p.LegalEntityID = &book, &book, &book
	p.RuleMappings = []RuleMapping{
		{RefKind: "JURISDICTION_PACK", Ref: "pack-gb-vat", RefVersion: "2026.3", Rationale: "VAT point of supply", MapsTo: "output tax"},
		{RefKind: MappingKindPolicyVersion, Ref: acctBook, RefVersion: "3", Rationale: "extends the base policy"},
	}
	to := p.EffectiveFrom.AddDate(1, 0, 0)
	p.EffectiveTo = &to
	if errs := p.Validate(acctNow); len(errs) != 0 {
		t.Fatalf("a good draft refused: %v", errs)
	}
}

func TestDraftAccounting_MalformedDraftsAreRefusedWithWhy(t *testing.T) {
	cases := map[string]struct {
		mutate func(*DraftAccountingPolicyParams)
		want   string
	}{
		"lower-case domain":    {func(p *DraftAccountingPolicyParams) { p.PolicyDomain = "revenue" }, "policy_domain"},
		"no domain":            {func(p *DraftAccountingPolicyParams) { p.PolicyDomain = "" }, "policy_domain"},
		"domain with a space":  {func(p *DraftAccountingPolicyParams) { p.PolicyDomain = "REV REC" }, "policy_domain"},
		"over-long domain":     {func(p *DraftAccountingPolicyParams) { p.PolicyDomain = "A" + strings.Repeat("B", 64) }, "policy_domain"},
		"no framework":         {func(p *DraftAccountingPolicyParams) { p.Framework = "" }, "framework must"},
		"bad framework":        {func(p *DraftAccountingPolicyParams) { p.Framework = "ifrs" }, "framework must"},
		"book is no uuid":      {func(p *DraftAccountingPolicyParams) { b := "GB"; p.BookID = &b }, "book_id"},
		"jurisdiction no uuid": {func(p *DraftAccountingPolicyParams) { b := "GB"; p.JurisdictionID = &b }, "jurisdiction_id"},
		"entity is no uuid":    {func(p *DraftAccountingPolicyParams) { b := "E1"; p.LegalEntityID = &b }, "legal_entity_id"},
		"no semantic version":  {func(p *DraftAccountingPolicyParams) { p.SemanticVersion = "" }, "semantic_version"},
		"two-part version":     {func(p *DraftAccountingPolicyParams) { p.SemanticVersion = "2.1" }, "semantic_version"},
		"leading zero":         {func(p *DraftAccountingPolicyParams) { p.SemanticVersion = "02.1.0" }, "semantic_version"},
		"over-long version":    {func(p *DraftAccountingPolicyParams) { p.SemanticVersion = "1.0.0-" + strings.Repeat("a", 30) }, "semantic_version"},
		"negative precedence":  {func(p *DraftAccountingPolicyParams) { p.Precedence = -1 }, "precedence"},
		"huge precedence":      {func(p *DraftAccountingPolicyParams) { p.Precedence = MaxPrecedence + 1 }, "precedence"},
		"no framework source":  {func(p *DraftAccountingPolicyParams) { p.FrameworkSource = " " }, "framework_source is required"},
		"no policy text":       {func(p *DraftAccountingPolicyParams) { p.PolicyTextRef = "" }, "policy_text_ref is required"},
		"no rationale":         {func(p *DraftAccountingPolicyParams) { p.Rationale = "" }, "rationale is required"},
		"over-long rationale":  {func(p *DraftAccountingPolicyParams) { p.Rationale = strings.Repeat("x", MaxAccountingText+1) }, "rationale is longer"},
		"no effective_from":    {func(p *DraftAccountingPolicyParams) { p.EffectiveFrom = time.Time{} }, "effective_from is required"},
		"effective_to before":  {func(p *DraftAccountingPolicyParams) { to := p.EffectiveFrom.Add(-time.Hour); p.EffectiveTo = &to }, "effective_to"},
		"no impact summary":    {func(p *DraftAccountingPolicyParams) { p.Impact.Summary = "" }, "impact_analysis.summary"},
		"no impact scope":      {func(p *DraftAccountingPolicyParams) { p.Impact.AffectedPeriods = "" }, "affected_periods"},
		"unknown impact scope": {func(p *DraftAccountingPolicyParams) { p.Impact.AffectedPeriods = "CLOSED_PERIODS" }, "affected_periods"},
		"restates closed":      {func(p *DraftAccountingPolicyParams) { p.Impact.ClosedHistoryUnchanged = false }, "closed_history_unchanged"},
		"too many mappings":    {func(p *DraftAccountingPolicyParams) { p.RuleMappings = make([]RuleMapping, MaxRuleMappings+1) }, "at most"},
	}
	for name, c := range cases {
		p := goodDraft()
		c.mutate(&p)
		if got := problems(p); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want a problem mentioning %q", name, got, c.want)
		}
	}
}

func TestDraftAccounting_RuleMappingsArePinnedAndWellFormed(t *testing.T) {
	good := RuleMapping{RefKind: "ACCOUNTING_RULE", Ref: "rule-9", RefVersion: "4", Rationale: "why"}
	cases := map[string]struct {
		mutate func(*RuleMapping)
		want   string
	}{
		"no kind":              {func(m *RuleMapping) { m.RefKind = "" }, "ref_kind"},
		"lower-case kind":      {func(m *RuleMapping) { m.RefKind = "rule" }, "ref_kind"},
		"no ref":               {func(m *RuleMapping) { m.Ref = "" }, "ref is required"},
		"no pinned version":    {func(m *RuleMapping) { m.RefVersion = "" }, "ref_version is required"},
		"no rationale":         {func(m *RuleMapping) { m.Rationale = "" }, "rationale is required"},
		"policy version no id": {func(m *RuleMapping) { m.RefKind = MappingKindPolicyVersion; m.Ref = "not-a-uuid" }, "policy version id"},
		"over-long maps_to":    {func(m *RuleMapping) { m.MapsTo = strings.Repeat("x", 513) }, "maps_to"},
	}
	for name, c := range cases {
		p := goodDraft()
		m := good
		c.mutate(&m)
		p.RuleMappings = []RuleMapping{m}
		if got := problems(p); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	p := goodDraft()
	p.RuleMappings = []RuleMapping{good, good}
	if got := problems(p); !strings.Contains(got, "repeats") {
		t.Errorf("the same dependency twice: %q", got)
	}
}

// A policy that takes effect in the past reaches back into periods that may be
// calculated already, so it must say so and say why.
func TestDraftAccounting_ARetroactiveVersionMustJustifyItself(t *testing.T) {
	p := goodDraft()
	p.EffectiveFrom = acctNow.AddDate(0, -1, 0)
	got := problems(p)
	if !strings.Contains(got, "OPEN_PERIODS") || !strings.Contains(got, "retroactive_justification") {
		t.Fatalf("a retroactive draft: %q, want both the scope and a justification asked for", got)
	}

	p.Impact.AffectedPeriods = AffectsOpenPeriods
	if got := problems(p); !strings.Contains(got, "retroactive_justification") || strings.Contains(got, "OPEN_PERIODS") {
		t.Errorf("scope given, no justification: %q", got)
	}
	p.Impact.RetroactiveJustification = "corrects the treatment since the standard changed"
	if errs := p.Validate(acctNow); len(errs) != 0 {
		t.Errorf("a justified retroactive draft refused: %v", errs)
	}

	// Effective now is not the past.
	p = goodDraft()
	p.EffectiveFrom = acctNow
	if errs := p.Validate(acctNow); len(errs) != 0 {
		t.Errorf("a draft effective now refused: %v", errs)
	}
}

func TestDraftAccounting_EveryProblemIsReportedAtOnce(t *testing.T) {
	errs := DraftAccountingPolicyParams{}.Validate(acctNow)
	if len(errs) < 8 {
		t.Errorf("an empty draft: %d problems %v, want them all", len(errs), errs)
	}
}

func TestFamilyKey_IsTheApplicabilityNotItsSpelling(t *testing.T) {
	book, juris := "AAAAAAAA-0000-0000-0000-000000000001", "BBBBBBBB-0000-0000-0000-000000000002"
	lower := strings.ToLower(book)
	if FamilyKey("LEASES", &book, nil) != FamilyKey("LEASES", &lower, nil) {
		t.Error("the same book in another case is another family")
	}
	keys := map[string]bool{}
	for _, k := range []string{
		FamilyKey("LEASES", nil, nil), FamilyKey("LEASES", &book, nil), FamilyKey("LEASES", nil, &juris),
		FamilyKey("LEASES", &book, &juris), FamilyKey("REVENUE", nil, nil), FamilyKey("LEASES", &juris, &book),
	} {
		if keys[k] {
			t.Errorf("two different applicabilities share the key %q", k)
		}
		keys[k] = true
	}
}

// The stored content is all of the policy, deterministically, so that the
// digest over it pins what the policy is, where it applies and what it rests on.
func TestRulePayload_CoversTheWholePolicyDeterministically(t *testing.T) {
	p := goodDraft()
	book := acctBook
	p.BookID = &book
	p.RuleMappings = []RuleMapping{{RefKind: "JURISDICTION_PACK", Ref: "pack-gb", RefVersion: "2026.3", Rationale: "VAT", MapsTo: "tax"}}
	a, b := p.RulePayload(), p.RulePayload()
	if string(a) != string(b) {
		t.Fatal("the same draft produced different bytes")
	}
	var got map[string]any
	if err := json.Unmarshal(a, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"policy_domain", "framework", "book_id", "jurisdiction_id", "semantic_version", "precedence", "framework_source", "policy_text_ref", "rule_mappings", "impact_analysis"} {
		if _, ok := got[k]; !ok {
			t.Errorf("payload lacks %q: %s", k, a)
		}
	}
	if got["kind"] != PolicyTypeAccounting || got["jurisdiction_id"] != nil || got["book_id"] != acctBook {
		t.Errorf("payload = %s", a)
	}

	// Anything that changes what the policy is changes the bytes.
	for name, mutate := range map[string]func(*DraftAccountingPolicyParams){
		"precedence": func(p *DraftAccountingPolicyParams) { p.Precedence = 6 },
		"text":       func(p *DraftAccountingPolicyParams) { p.PolicyTextRef = "other" },
		"mapping":    func(p *DraftAccountingPolicyParams) { p.RuleMappings[0].RefVersion = "2026.4" },
		"impact":     func(p *DraftAccountingPolicyParams) { p.Impact.Summary = "other" },
		"book":       func(p *DraftAccountingPolicyParams) { p.BookID = nil },
		"semver":     func(p *DraftAccountingPolicyParams) { p.SemanticVersion = "2.1.1" },
		"framework":  func(p *DraftAccountingPolicyParams) { p.Framework = "US_GAAP" },
		"extra mapping": func(p *DraftAccountingPolicyParams) {
			p.RuleMappings = append(p.RuleMappings, RuleMapping{RefKind: "X1", Ref: "r", RefVersion: "1", Rationale: "y"})
		},
	} {
		q := goodDraft()
		q.BookID = &book
		q.RuleMappings = []RuleMapping{{RefKind: "JURISDICTION_PACK", Ref: "pack-gb", RefVersion: "2026.3", Rationale: "VAT", MapsTo: "tax"}}
		mutate(&q)
		if string(q.RulePayload()) == string(a) {
			t.Errorf("changing the %s did not change the payload", name)
		}
	}
}

func TestAccountingCandidate_RankNarrowsThenPrecedence(t *testing.T) {
	book, juris, entity, tenant := "b", "j", "e", "t"
	mk := func(b, j *string, entityScope bool, prec int) *AccountingCandidate {
		c := &AccountingCandidate{Applicability: AccountingApplicability{BookID: b, JurisdictionID: j}, Precedence: prec}
		c.TenantID = &tenant
		if entityScope {
			c.LegalEntityID = &entity
		}
		return c
	}
	cases := []struct {
		name string
		c    *AccountingCandidate
		want []int
	}{
		{"tenant-wide, every book", mk(nil, nil, false, 0), []int{1, 0}},
		{"a book", mk(&book, nil, false, 3), []int{2, 3}},
		{"a jurisdiction", mk(nil, &juris, false, 3), []int{2, 3}},
		{"book and jurisdiction", mk(&book, &juris, false, 0), []int{3, 0}},
		{"an entity", mk(nil, nil, true, 0), []int{2, 0}},
		{"everything", mk(&book, &juris, true, 9), []int{4, 9}},
	}
	for _, c := range cases {
		got := c.c.Rank()
		if len(got) != 2 || got[0] != c.want[0] || got[1] != c.want[1] {
			t.Errorf("%s: rank %v, want %v", c.name, got, c.want)
		}
	}
	// Precedence never outweighs narrowness.
	general, narrow := mk(nil, nil, false, MaxPrecedence), mk(&book, nil, false, 0)
	if g, n := general.Rank(), narrow.Rank(); g[0] >= n[0] {
		t.Errorf("general %v outranks narrow %v on scope", g, n)
	}
}

func TestPinOf_IsWhatAConsumerPersists(t *testing.T) {
	v := &AccountingPolicyVersion{PolicyVersion: PolicyVersion{PolicyID: "p", PolicyVersionID: "v", VersionNumber: 3, ArtifactDigest: "sha256:x", RowVersion: 7},
		Details: AccountingVersionDetails{SemanticVersion: "2.1.0"}}
	if got := PinOf(v); got != (Pin{PolicyID: "p", PolicyVersionID: "v", VersionNumber: 3, SemanticVersion: "2.1.0", ArtifactDigest: "sha256:x", RowVersion: 7}) {
		t.Errorf("pin = %+v", got)
	}
}

func TestApplicability_SpecificityCountsBookAndJurisdiction(t *testing.T) {
	b := "b"
	for want, a := range map[int]AccountingApplicability{
		0: {}, 1: {BookID: &b}, 2: {BookID: &b, JurisdictionID: &b},
	} {
		if got := a.Specificity(); got != want {
			t.Errorf("%+v: %d, want %d", a, got, want)
		}
	}
}
