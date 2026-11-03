package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/resolve"
)

// REF-07 accounting policy against a real Postgres, as an ordinary role under
// row-level security: a draft is atomic, one applicability is one family, the
// lifecycle is the GOV-05 machinery, and resolution answers from the families
// that could apply to a request or says it cannot.

const (
	bookGB = "55555555-5555-5555-5555-555555555555"
	bookUS = "66666666-6666-6666-6666-666666666666"
	jurUK  = "77777777-7777-7777-7777-777777777777"
	jurEU  = "88888888-8888-8888-8888-888888888888"
)

func (e *testEnv) acct(tenant string, mutate ...func(*domain.DraftAccountingPolicyParams)) domain.DraftAccountingPolicyParams {
	p := domain.DraftAccountingPolicyParams{
		TenantID: tenant, PolicyDomain: "REVENUE_RECOGNITION", Framework: "IFRS",
		SemanticVersion: "1.0.0", FrameworkSource: "IFRS 15 paragraph 31", PolicyTextRef: "policy-manual/rev-rec/v1",
		Impact:        domain.ImpactAnalysis{Summary: "future contracts", AffectedPeriods: domain.AffectsFutureOnly, ClosedHistoryUnchanged: true},
		EffectiveFrom: e.clock.Add(-72 * time.Hour), Rationale: "annual review",
		CreatedByPrincipalID: "author", Meta: domain.EventMeta{CorrelationID: "corr"},
	}
	for _, m := range mutate {
		m(&p)
	}
	return p
}

func (e *testEnv) acctDraft(t *testing.T, tenant string, mutate ...func(*domain.DraftAccountingPolicyParams)) *domain.AccountingPolicyVersion {
	t.Helper()
	v, created, err := e.s.CreateAccountingDraft(as(tenant), e.acct(tenant, mutate...))
	if err != nil || !created {
		t.Fatalf("draft: created=%v err=%v", created, err)
	}
	return v
}

func book(b string) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.BookID = &b }
}
func juris(j string) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.JurisdictionID = &j }
}
func domainOf(d string) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.PolicyDomain = d }
}
func semver(v string) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.SemanticVersion = v }
}
func precedence(n int) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.Precedence = n }
}
func entityScope(id string) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.LegalEntityID = &id }
}
func period(from time.Time, to *time.Time) func(*domain.DraftAccountingPolicyParams) {
	return func(p *domain.DraftAccountingPolicyParams) { p.EffectiveFrom, p.EffectiveTo = from, to }
}

// released drafts a version and takes it live.
func (e *testEnv) released(t *testing.T, tenant string, mutate ...func(*domain.DraftAccountingPolicyParams)) *domain.AccountingPolicyVersion {
	t.Helper()
	v := e.acctDraft(t, tenant, mutate...)
	e.releaseOver(t, tenant, v, e.activeOf(t, v))
	got, err := e.s.GetAccountingPolicyVersion(as(tenant), v.PolicyVersionID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// resolved ranks the candidates exactly as the handler does.
func (e *testEnv) resolved(t *testing.T, tenant string, q domain.AccountingCandidateQuery, asOf, knownAt time.Time) (resolve.Result, map[string]*domain.AccountingCandidate) {
	t.Helper()
	q.TenantID = tenant
	cands, err := e.s.AccountingCandidates(as(tenant), q)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	byVersion := map[*domain.ApplicablePolicyVersion]*domain.AccountingCandidate{}
	byID := map[string]*domain.AccountingCandidate{}
	list := make([]*domain.ApplicablePolicyVersion, 0, len(cands))
	for _, c := range cands {
		if q.PolicyDomain != "" && c.Applicability.PolicyDomain != q.PolicyDomain {
			continue
		}
		byVersion[&c.ApplicablePolicyVersion] = c
		byID[c.PolicyVersionID] = c
		list = append(list, &c.ApplicablePolicyVersion)
	}
	res := resolve.ResolveRanked(list, asOf, knownAt, func(v *domain.ApplicablePolicyVersion) []int { return byVersion[v].Rank() })
	return res, byID
}

func (e *testEnv) governs(t *testing.T, tenant string, q domain.AccountingCandidateQuery, asOf, knownAt time.Time) string {
	t.Helper()
	res, _ := e.resolved(t, tenant, q, asOf, knownAt)
	if res.Status != resolve.Resolved {
		return string(res.Status)
	}
	return res.Selected.PolicyVersionID
}

func sp2(s string) *string { return &s }

// ── drafting ────────────────────────────────────────────────────────────────

func TestAccountingDraft_IsOneAtomicRecordOfEverythingThePolicyRestsOn(t *testing.T) {
	e := newEnv(t)
	pv := e.acctDraft(t, tenantA)
	// A second version that depends on the first: a real, pinned dependency.
	v := e.acctDraft(t, tenantA, book(bookGB), juris(jurUK), entityScope(entityA), precedence(7), semver("2.1.0"), func(p *domain.DraftAccountingPolicyParams) {
		p.RuleMappings = []domain.RuleMapping{
			{RefKind: "JURISDICTION_PACK", Ref: "pack-gb-vat", RefVersion: "2026.3", Rationale: "VAT point of supply", MapsTo: "output tax"},
			{RefKind: domain.MappingKindPolicyVersion, Ref: pv.PolicyVersionID, RefVersion: "1.0.0", Rationale: "extends the base policy"},
		}
	})

	if v.VersionStatus != domain.StatusDraft || v.VersionNumber != 1 || v.TenantID == nil || *v.TenantID != tenantA ||
		v.LegalEntityID == nil || *v.LegalEntityID != entityA || v.ScopeType != domain.ScopeTypeLegalEntity {
		t.Errorf("version = %+v", v.PolicyVersion)
	}
	a := v.Applicability
	if a.PolicyDomain != "REVENUE_RECOGNITION" || a.Framework != "IFRS" || a.BookID == nil || *a.BookID != bookGB || a.JurisdictionID == nil || *a.JurisdictionID != jurUK {
		t.Errorf("applicability = %+v", a)
	}
	if v.Details.SemanticVersion != "2.1.0" || v.Details.Precedence != 7 || v.Details.FrameworkSource == "" || v.Details.PolicyTextRef == "" ||
		v.Details.Impact.AffectedPeriods != domain.AffectsFutureOnly || !v.Details.Impact.ClosedHistoryUnchanged {
		t.Errorf("details = %+v", v.Details)
	}
	if len(v.RuleMappings) != 2 || v.RuleMappings[0].MappingNo != 1 || v.RuleMappings[0].RefKind != "JURISDICTION_PACK" || v.RuleMappings[0].MapsTo != "output tax" ||
		v.RuleMappings[1].DependsOnVersionID == nil || *v.RuleMappings[1].DependsOnVersionID != pv.PolicyVersionID {
		t.Errorf("mappings = %+v", v.RuleMappings)
	}

	// The digest pins the whole policy: the payload the trigger hashed carries
	// the applicability, the details and the mappings.
	sum := sha256.Sum256(v.RulePayload)
	if v.ArtifactDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("digest %s does not match the stored payload", v.ArtifactDigest)
	}
	for _, want := range []string{bookGB, jurUK, "2.1.0", "pack-gb-vat", "2026.3", "IFRS 15 paragraph 31"} {
		if !strings.Contains(string(v.RulePayload), want) {
			t.Errorf("payload lacks %q, so the digest would not pin it: %s", want, v.RulePayload)
		}
	}

	// The family is an ordinary tenant-owned policy of the accounting type.
	fam, err := e.s.FindPolicyByID(as(tenantA), v.PolicyID)
	if err != nil || fam.PolicyType != domain.PolicyTypeAccounting || fam.TenantID == nil || *fam.TenantID != tenantA || !strings.HasPrefix(fam.PolicyCode, "ACCT:REVENUE_RECOGNITION:") {
		t.Errorf("family = %+v err %v", fam, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.created'`); n != 2 {
		t.Errorf("%d policy.created events, want one per family (2)", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.updated'`); n != 2 {
		t.Errorf("%d policy.updated events, want one per draft (2)", n)
	}
}

func TestAccountingDraft_OneApplicabilityIsOneFamilyWhateverTheVersion(t *testing.T) {
	e := newEnv(t)
	a1 := e.acctDraft(t, tenantA, book(bookGB))
	a2 := e.acctDraft(t, tenantA, book(bookGB), semver("1.1.0"))
	a3 := e.acctDraft(t, tenantA, book(bookGB), semver("1.0.0"), entityScope(entityA)) // same version number, another scope: allowed
	if a1.PolicyID != a2.PolicyID || a1.PolicyID != a3.PolicyID {
		t.Fatal("versions of one applicability were put in different families")
	}
	if a1.VersionNumber != 1 || a2.VersionNumber != 2 {
		t.Errorf("version numbers %d, %d: want them numbered in the family", a1.VersionNumber, a2.VersionNumber)
	}

	other := map[string]*domain.AccountingPolicyVersion{
		"another book":          e.acctDraft(t, tenantA, book(bookUS)),
		"every book":            e.acctDraft(t, tenantA),
		"a jurisdiction":        e.acctDraft(t, tenantA, juris(jurUK)),
		"book and jurisdiction": e.acctDraft(t, tenantA, book(bookGB), juris(jurUK)),
		"another domain":        e.acctDraft(t, tenantA, book(bookGB), domainOf("LEASES")),
	}
	seen := map[string]string{a1.PolicyID: "the first"}
	for name, v := range other {
		if prev, dup := seen[v.PolicyID]; dup {
			t.Errorf("%s shares a family with %s", name, prev)
		}
		seen[v.PolicyID] = name
	}
	if n := e.count(t, `SELECT COUNT(*) FROM accounting_policy_applicability`); n != 6 {
		t.Errorf("%d families, want 6", n)
	}

	// A book has one framework.
	_, _, err := e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), semver("9.0.0"), func(p *domain.DraftAccountingPolicyParams) { p.Framework = "US_GAAP" }))
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a different framework for the same applicability: %v, want ErrConflict", err)
	}
}

func TestAccountingDraft_ReplayIsTheSameDraftAndDifferentContentIsAConflict(t *testing.T) {
	e := newEnv(t)
	first := e.acctDraft(t, tenantA, book(bookGB), semver("2.0.0"))

	again, created, err := e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), semver("2.0.0")))
	if err != nil || created || again.PolicyVersionID != first.PolicyVersionID {
		t.Fatalf("replay: %v created=%v err=%v, want the first draft back", again, created, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_versions`); n != 1 {
		t.Errorf("%d versions after a replay, want 1", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM accounting_policy_rule_mappings`); n != 0 {
		t.Errorf("%d mappings", n)
	}

	for name, mutate := range map[string]func(*domain.DraftAccountingPolicyParams){
		"another precedence": precedence(3),
		"another text":       func(p *domain.DraftAccountingPolicyParams) { p.PolicyTextRef = "policy-manual/other" },
		"another period":     period(e.clock.Add(-48*time.Hour), nil),
		"another impact":     func(p *domain.DraftAccountingPolicyParams) { p.Impact.Summary = "something else" },
		"another rationale":  func(p *domain.DraftAccountingPolicyParams) { p.Rationale = "because" },
		"a mapping": func(p *domain.DraftAccountingPolicyParams) {
			p.RuleMappings = []domain.RuleMapping{{RefKind: "ACCOUNTING_RULE", Ref: "r1", RefVersion: "1", Rationale: "x"}}
		},
	} {
		if _, _, err := e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), semver("2.0.0"), mutate)); !errors.Is(err, domain.ErrIdempotencyMismatch) {
			t.Errorf("same semantic version with %s: %v, want ErrIdempotencyMismatch", name, err)
		}
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_versions`); n != 1 {
		t.Errorf("%d versions after the refusals, want 1", n)
	}
}

func TestAccountingDraft_ConcurrentDraftsOfOneVersionMakeOne(t *testing.T) {
	e := newEnv(t)
	const n = 6
	var wg sync.WaitGroup
	ids, created, errs := make([]string, n), make([]bool, n), make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, c, err := e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), semver("3.0.0")))
			if v != nil {
				ids[i] = v.PolicyVersionID
			}
			created[i], errs[i] = c, err
		}(i)
	}
	close(start)
	wg.Wait()
	won := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("draft %d: %v", i, errs[i])
		}
		if created[i] {
			won++
		}
		if ids[i] != ids[0] {
			t.Errorf("draft %d returned %s, want %s", i, ids[i], ids[0])
		}
	}
	if won != 1 || e.count(t, `SELECT COUNT(*) FROM policy_versions`) != 1 || e.count(t, `SELECT COUNT(*) FROM accounting_policy_applicability`) != 1 {
		t.Errorf("%d created; versions/families not exactly one each", won)
	}
}

func TestAccountingDraft_ConcurrentDraftsOfDifferentVersionsAreNumberedOnce(t *testing.T) {
	e := newEnv(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), semver(fmt.Sprintf("1.%d.0", i))))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("draft %d: %v", i, err)
		}
	}
	var distinct, families int
	if err := e.admin.QueryRow(context.Background(), `SELECT COUNT(DISTINCT version_number), COUNT(DISTINCT policy_id) FROM policy_versions`).Scan(&distinct, &families); err != nil {
		t.Fatal(err)
	}
	if distinct != n || families != 1 {
		t.Errorf("%d distinct version numbers in %d families, want %d in 1", distinct, families, n)
	}
}

func TestAccountingDraft_ABadDependencyWritesNothing(t *testing.T) {
	e := newEnv(t)
	_, _, err := e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), func(p *domain.DraftAccountingPolicyParams) {
		p.RuleMappings = []domain.RuleMapping{
			{RefKind: "JURISDICTION_PACK", Ref: "pack", RefVersion: "1", Rationale: "ok"},
			{RefKind: domain.MappingKindPolicyVersion, Ref: "99999999-9999-9999-9999-999999999999", RefVersion: "1", Rationale: "to nothing"},
		}
	}))
	if !errors.Is(err, domain.ErrAccountingDependencyNotFound) {
		t.Fatalf("a dependency on nothing: %v, want ErrAccountingDependencyNotFound", err)
	}
	for _, table := range []string{"policies", "policy_versions", "accounting_policy_applicability", "accounting_policy_version_details", "accounting_policy_rule_mappings", "outbox"} {
		if n := e.count(t, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Errorf("%s has %d rows after a refused draft: it was not atomic", table, n)
		}
	}

	// Another tenant's version is nothing to this tenant.
	theirs := e.acctDraft(t, tenantB, book(bookGB))
	_, _, err = e.s.CreateAccountingDraft(as(tenantA), e.acct(tenantA, book(bookGB), func(p *domain.DraftAccountingPolicyParams) {
		p.RuleMappings = []domain.RuleMapping{{RefKind: domain.MappingKindPolicyVersion, Ref: theirs.PolicyVersionID, RefVersion: "1.0.0", Rationale: "theirs"}}
	}))
	if !errors.Is(err, domain.ErrAccountingDependencyNotFound) {
		t.Errorf("a dependency on another tenant's version: %v, want it not found", err)
	}
}

// ── tenant isolation ────────────────────────────────────────────────────────

func TestAccounting_TenantsDoNotSeeEachOther(t *testing.T) {
	e := newEnv(t)
	a := e.released(t, tenantA, book(bookGB))
	b := e.released(t, tenantB, book(bookGB))
	if a.PolicyID == b.PolicyID {
		t.Fatal("two tenants share a family")
	}
	if _, err := e.s.GetAccountingPolicyVersion(as(tenantB), a.PolicyVersionID); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("tenant B read tenant A's version: %v", err)
	}
	if is, err := e.s.IsAccountingPolicy(as(tenantB), a.PolicyID); err != nil || is {
		t.Errorf("tenant B saw tenant A's family as accounting: %v %v", is, err)
	}
	cands, err := e.s.AccountingCandidates(as(tenantB), domain.AccountingCandidateQuery{TenantID: tenantB, BookID: sp2(bookGB)})
	if err != nil || len(cands) != 1 || cands[0].PolicyVersionID != b.PolicyVersionID {
		t.Errorf("tenant B's candidates = %v err %v, want only its own", cands, err)
	}
	// Asking as one tenant for another's data finds none of it.
	if cands, err := e.s.AccountingCandidates(as(tenantB), domain.AccountingCandidateQuery{TenantID: tenantA, BookID: sp2(bookGB)}); !errors.Is(err, domain.ErrInvalidScope) || len(cands) != 0 {
		t.Errorf("candidates for another tenant's id: %d err %v, want it refused", len(cands), err)
	}
	if _, _, err := e.s.CreateAccountingDraft(as(tenantB), e.acct(tenantA, semver("8.0.0"))); !errors.Is(err, domain.ErrInvalidScope) {
		t.Errorf("a draft in another tenant's name: %v, want it refused", err)
	}
	if _, err := e.s.AccountingCandidates(context.Background(), domain.AccountingCandidateQuery{TenantID: tenantA}); !errors.Is(err, domain.ErrInvalidScope) {
		t.Errorf("candidates with no verified tenant: %v, want it refused", err)
	}
}

func TestAccounting_IsAccountingPolicyTellsFamiliesApart(t *testing.T) {
	e := newEnv(t)
	acct := e.acctDraft(t, tenantA)
	generic := e.family(t, tenantA, sp(tenantA), "AP-5K")
	if is, err := e.s.IsAccountingPolicy(as(tenantA), acct.PolicyID); err != nil || !is {
		t.Errorf("accounting family: %v %v", is, err)
	}
	if is, err := e.s.IsAccountingPolicy(as(tenantA), generic.PolicyID); err != nil || is {
		t.Errorf("generic family: %v %v", is, err)
	}
	if is, err := e.s.IsAccountingPolicy(as(tenantA), "not-a-uuid"); err != nil || is {
		t.Errorf("not a uuid: %v %v", is, err)
	}
	if _, err := e.s.GetAccountingPolicyVersion(as(tenantA), "not-a-uuid"); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("get with a bad id: %v", err)
	}
	// A generic version is not an accounting version.
	gv := e.draft(t, tenantA, generic, sp(tenantA), nil, e.clock, `{"threshold_amount": 5}`)
	if _, err := e.s.GetAccountingPolicyVersion(as(tenantA), gv.PolicyVersionID); !errors.Is(err, domain.ErrPolicyVersionNotFound) {
		t.Errorf("a generic version read as accounting: %v", err)
	}
}

// ── the database holds the line too ─────────────────────────────────────────

func TestAccounting_TheDatabaseKeepsItsRulesAndItsEvidence(t *testing.T) {
	e := newEnv(t)
	v := e.acctDraft(t, tenantA, book(bookGB), func(p *domain.DraftAccountingPolicyParams) {
		p.RuleMappings = []domain.RuleMapping{{RefKind: "ACCOUNTING_RULE", Ref: "r1", RefVersion: "1", Rationale: "x"}}
	})
	ctx := context.Background()
	exec := func(sql string, args ...any) error { _, err := e.admin.Exec(ctx, sql, args...); return err }

	for name, sql := range map[string]string{
		"rewriting applicability":  `UPDATE accounting_policy_applicability SET framework = 'US_GAAP'`,
		"deleting applicability":   `DELETE FROM accounting_policy_applicability`,
		"truncating applicability": `TRUNCATE accounting_policy_applicability CASCADE`,
		"rewriting details":        `UPDATE accounting_policy_version_details SET precedence = 9`,
		"deleting details":         `DELETE FROM accounting_policy_version_details`,
		"truncating details":       `TRUNCATE accounting_policy_version_details`,
		"rewriting a mapping":      `UPDATE accounting_policy_rule_mappings SET ref_version = '2'`,
		"deleting a mapping":       `DELETE FROM accounting_policy_rule_mappings`,
		"truncating mappings":      `TRUNCATE accounting_policy_rule_mappings`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}

	insertDetails := func(id, semver string, precedence int, impact string) error {
		return exec(`INSERT INTO accounting_policy_version_details
			(policy_version_id, policy_id, tenant_id, semantic_version, precedence, framework_source, policy_text_ref, impact_analysis)
			VALUES ($1, $2, $3, $4, $5, 'src', 'ref', $6::jsonb)`, id, v.PolicyID, tenantA, semver, precedence, impact)
	}
	okImpact := `{"summary":"s","affected_periods":"FUTURE_ONLY","closed_history_unchanged":true}`
	other := e.draft(t, tenantA, e.family(t, tenantA, sp(tenantA), "AP-X"), sp(tenantA), nil, e.clock, `{"threshold_amount": 1}`)
	for name, c := range map[string]struct {
		semver     string
		precedence int
		impact     string
	}{
		"a bad semantic version":      {"1.0", 0, okImpact},
		"a leading zero":              {"01.0.0", 0, okImpact},
		"precedence past the maximum": {"9.0.0", 1001, okImpact},
		"negative precedence":         {"9.0.1", -1, okImpact},
		"an impact that restates":     {"9.0.2", 0, `{"summary":"s","affected_periods":"FUTURE_ONLY","closed_history_unchanged":false}`},
		"an impact with no summary":   {"9.0.3", 0, `{"affected_periods":"FUTURE_ONLY","closed_history_unchanged":true}`},
		"an impact reaching closed":   {"9.0.4", 0, `{"summary":"s","affected_periods":"CLOSED_PERIODS","closed_history_unchanged":true}`},
		"an impact that is no object": {"9.0.5", 0, `[]`},
	} {
		if err := insertDetails(other.PolicyVersionID, c.semver, c.precedence, c.impact); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if err := insertDetails(other.PolicyVersionID, "9.1.0", 0, okImpact); err != nil {
		t.Fatalf("a good details row refused: %v", err)
	}

	for name, sql := range map[string]string{
		"a mapping of a policy version with no version id": `INSERT INTO accounting_policy_rule_mappings (policy_version_id, tenant_id, mapping_no, ref_kind, ref, ref_version, mapping_rationale)
			VALUES ('` + v.PolicyVersionID + `', '` + tenantA + `', 9, 'POLICY_VERSION', 'x', '1', 'y')`,
		"a mapping with a version id but another kind": `INSERT INTO accounting_policy_rule_mappings (policy_version_id, tenant_id, mapping_no, ref_kind, ref, ref_version, mapping_rationale, depends_on_version_id)
			VALUES ('` + v.PolicyVersionID + `', '` + tenantA + `', 9, 'ACCOUNTING_RULE', 'x', '1', 'y', '` + other.PolicyVersionID + `')`,
		"a mapping with no pinned version": `INSERT INTO accounting_policy_rule_mappings (policy_version_id, tenant_id, mapping_no, ref_kind, ref, ref_version, mapping_rationale)
			VALUES ('` + v.PolicyVersionID + `', '` + tenantA + `', 9, 'ACCOUNTING_RULE', 'x', ' ', 'y')`,
		"a mapping numbered twice": `INSERT INTO accounting_policy_rule_mappings (policy_version_id, tenant_id, mapping_no, ref_kind, ref, ref_version, mapping_rationale)
			VALUES ('` + v.PolicyVersionID + `', '` + tenantA + `', 1, 'ACCOUNTING_RULE', 'y', '1', 'y')`,
		"a lower-case domain": `INSERT INTO accounting_policy_applicability (policy_id, tenant_id, policy_domain, framework, created_by_principal_id)
			VALUES ('` + other.PolicyID + `', '` + tenantA + `', 'leases', 'IFRS', 'x')`,
		"a second family for one applicability": `INSERT INTO accounting_policy_applicability (policy_id, tenant_id, policy_domain, framework, book_id, created_by_principal_id)
			VALUES ('` + other.PolicyID + `', '` + tenantA + `', 'REVENUE_RECOGNITION', 'IFRS', '` + bookGB + `', 'x')`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
}

// ── resolution ──────────────────────────────────────────────────────────────

// Two domains of one tenant are both active at once. The generic rule that two
// policies of a type cannot share a scope would have refused the second.
func TestAccountingResolve_DifferentDomainsAreActiveTogether(t *testing.T) {
	e := newEnv(t)
	rev := e.released(t, tenantA)
	leases := e.released(t, tenantA, domainOf("LEASES"))
	fx := e.released(t, tenantA, domainOf("FX_TRANSLATION"))

	for d, want := range map[string]string{"REVENUE_RECOGNITION": rev.PolicyVersionID, "LEASES": leases.PolicyVersionID, "FX_TRANSLATION": fx.PolicyVersionID} {
		if got := e.governs(t, tenantA, domain.AccountingCandidateQuery{PolicyDomain: d}, e.clock, e.clock); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
	}
	if got := e.governs(t, tenantA, domain.AccountingCandidateQuery{PolicyDomain: "TAX"}, e.clock, e.clock); got != string(resolve.NotApplicable) {
		t.Errorf("a domain with no policy: %s, want NOT_APPLICABLE", got)
	}
}

func TestAccountingResolve_AFamilyNarrowedToABookAppliesOnlyToThatBook(t *testing.T) {
	e := newEnv(t)
	general := e.released(t, tenantA)
	gbBook := e.released(t, tenantA, book(bookGB))
	ukJur := e.released(t, tenantA, juris(jurUK), precedence(1))

	at := e.clock
	q := func(b, j *string) domain.AccountingCandidateQuery {
		return domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION", BookID: b, JurisdictionID: j}
	}
	cases := []struct {
		name string
		q    domain.AccountingCandidateQuery
		want string
	}{
		{"no book, no jurisdiction: only the general one", q(nil, nil), general.PolicyVersionID},
		{"the GB book: its own policy is narrower", q(sp2(bookGB), nil), gbBook.PolicyVersionID},
		{"another book: the general one", q(sp2(bookUS), nil), general.PolicyVersionID},
		{"the UK jurisdiction", q(nil, sp2(jurUK)), ukJur.PolicyVersionID},
		{"another jurisdiction", q(nil, sp2(jurEU)), general.PolicyVersionID},
		// The book policy and the jurisdiction policy are equally specific; the
		// jurisdiction one was given the higher precedence, so it governs.
		{"the GB book in the UK: precedence decides", q(sp2(bookGB), sp2(jurUK)), ukJur.PolicyVersionID},
	}
	for _, c := range cases {
		if got := e.governs(t, tenantA, c.q, at, at); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}

	// A request that names a book never sees another book's policy as a candidate.
	cands, err := e.s.AccountingCandidates(as(tenantA), domain.AccountingCandidateQuery{TenantID: tenantA, PolicyDomain: "REVENUE_RECOGNITION", BookID: sp2(bookUS)})
	if err != nil || len(cands) != 1 || cands[0].PolicyVersionID != general.PolicyVersionID {
		t.Errorf("candidates for another book = %v err %v", cands, err)
	}
}

func TestAccountingResolve_EntityScopeNarrowsToo(t *testing.T) {
	e := newEnv(t)
	tenantWide := e.released(t, tenantA)
	entityOnly := e.released(t, tenantA, entityScope(entityA), semver("1.0.1"))
	if got := e.governs(t, tenantA, domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION", LegalEntityID: sp2(entityA)}, e.clock, e.clock); got != entityOnly.PolicyVersionID {
		t.Errorf("for the entity: %s, want its own policy %s", got, entityOnly.PolicyVersionID)
	}
	if got := e.governs(t, tenantA, domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION"}, e.clock, e.clock); got != tenantWide.PolicyVersionID {
		t.Errorf("for no entity: %s, want the tenant-wide policy", got)
	}
	if got := e.governs(t, tenantA, domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION", LegalEntityID: sp2("44444444-4444-4444-4444-444444444444")}, e.clock, e.clock); got != tenantWide.PolicyVersionID {
		t.Errorf("for another entity: %s, want the tenant-wide policy", got)
	}
}

// An activation that would make resolution ambiguous is refused when it is
// made, not discovered when a posting is attempted.
func TestAccountingActivation_RefusesAnAmbiguousPairAndAllowsTheRest(t *testing.T) {
	e := newEnv(t)
	e.released(t, tenantA, book(bookGB)) // narrowed by book, precedence 0

	activate := func(muts ...func(*domain.DraftAccountingPolicyParams)) error {
		v := e.acctDraft(t, tenantA, muts...)
		for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}, {domain.CommandActivate, "activator"}} {
			if _, err := e.cmd(t, tenantA, &v.PolicyVersion, step.cmd, step.actor); err != nil {
				return err
			}
		}
		return nil
	}

	// Equally specific (one narrows by jurisdiction), equal precedence, could
	// both apply to one request, same period: ambiguous.
	if err := activate(juris(jurUK)); !errors.Is(err, domain.ErrPolicyConflicted) {
		t.Fatalf("an ambiguous pair: %v, want ErrPolicyConflicted", err)
	}
	// An explicit precedence makes it unambiguous.
	if err := activate(juris(jurUK), precedence(1), semver("1.0.1")); err != nil {
		t.Errorf("an ambiguous pair given explicit precedence: %v", err)
	}
	// Policies that cannot meet in one request are not ambiguous.
	if err := activate(book(bookUS), semver("1.0.2")); err != nil {
		t.Errorf("a different book: %v", err)
	}
	// Different specificity is not contradictory: the narrower overrides.
	if err := activate(book(bookGB), juris(jurEU), semver("1.0.3")); err != nil {
		t.Errorf("a narrower policy: %v", err)
	}
	if err := activate(semver("1.0.4")); err != nil {
		t.Errorf("a broader policy: %v", err)
	}
	// Another domain is another subject.
	if err := activate(domainOf("LEASES"), juris(jurUK), semver("1.0.5")); err != nil {
		t.Errorf("another domain: %v", err)
	}
}

func TestAccountingActivation_OnlyPoliciesOverlappingInTimeAreAmbiguous(t *testing.T) {
	e := newEnv(t)
	end := e.clock.Add(-24 * time.Hour)
	e.released(t, tenantA, book(bookGB), period(e.clock.Add(-96*time.Hour), &end))

	later := e.acctDraft(t, tenantA, juris(jurUK), period(e.clock.Add(-12*time.Hour), nil))
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}, {domain.CommandActivate, "activator"}} {
		if _, err := e.cmd(t, tenantA, &later.PolicyVersion, step.cmd, step.actor); err != nil {
			t.Fatalf("a policy that starts after the other ended is not ambiguous with it: %s: %v", step.cmd, err)
		}
	}
}

func TestAccountingActivation_ConcurrentAmbiguousActivationsLetOneThrough(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 8; round++ {
		d := domainOf(fmt.Sprintf("ROUND_%d", round))
		a := e.acctDraft(t, tenantA, d, book(bookGB))
		b := e.acctDraft(t, tenantA, d, juris(jurUK))
		for _, v := range []*domain.AccountingPolicyVersion{a, b} {
			for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
				if _, err := e.cmd(t, tenantA, &v.PolicyVersion, step.cmd, step.actor); err != nil {
					t.Fatal(err)
				}
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, v := range []*domain.AccountingPolicyVersion{a, b} {
			wg.Add(1)
			go func(i int, v *domain.AccountingPolicyVersion) {
				defer wg.Done()
				<-start
				_, errs[i] = e.cmd(t, tenantA, &v.PolicyVersion, domain.CommandActivate, "activator")
			}(i, v)
		}
		close(start)
		wg.Wait()
		ok, conflicted := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrPolicyConflicted):
				conflicted++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || conflicted != 1 {
			t.Fatalf("round %d: %d activated, %d refused as ambiguous, want exactly one of each", round, ok, conflicted)
		}
	}
}

// Supersession keeps history: the version in force on a date is the one that
// governed then, however many have come since, and what was known then.
func TestAccountingResolve_AsOfAndKnownAtFollowTheHistory(t *testing.T) {
	e := newEnv(t)
	t0 := e.clock
	v1 := e.released(t, tenantA, semver("1.0.0"), period(t0.Add(-240*time.Hour), nil))
	e.advance(48 * time.Hour)
	// v2 takes over 24 hours before it is activated: retroactive.
	v2 := e.released(t, tenantA, semver("2.0.0"), period(e.clock.Add(-24*time.Hour), nil))
	q := domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION"}
	takeover := v2.EffectiveFrom

	if got := e.governs(t, tenantA, q, takeover.Add(-time.Hour), e.clock); got != v1.PolicyVersionID {
		t.Errorf("just before the takeover: %s, want v1", got)
	}
	if got := e.governs(t, tenantA, q, takeover.Add(time.Hour), e.clock); got != v2.PolicyVersionID {
		t.Errorf("just after the takeover: %s, want v2", got)
	}
	// What the platform knew before v2 was recorded: v1 still governs, even for
	// a date v2 now covers. Historical replay is not rewritten.
	if got := e.governs(t, tenantA, q, takeover.Add(time.Hour), t0.Add(time.Hour)); got != v1.PolicyVersionID {
		t.Errorf("as known before v2 was activated: %s, want v1", got)
	}
	if got := e.governs(t, tenantA, q, t0.Add(-1000*time.Hour), e.clock); got != string(resolve.NotApplicable) {
		t.Errorf("before any version: %s, want NOT_APPLICABLE", got)
	}

	old, err := e.s.GetAccountingPolicyVersion(as(tenantA), v1.PolicyVersionID)
	if err != nil || old.VersionStatus != domain.StatusSuperseded || old.SupersededByVersionID == nil || *old.SupersededByVersionID != v2.PolicyVersionID {
		t.Errorf("v1 = %+v err %v, want it kept, superseded by v2", old, err)
	}
	// The superseded version's content is still exactly what it was.
	if old.Details.SemanticVersion != "1.0.0" || old.ArtifactDigest != v1.ArtifactDigest {
		t.Error("a superseded version's content changed")
	}
}

// An emergency suspension withdraws the policy: nothing governs, and the
// predecessor is not resurrected in its place.
func TestAccountingResolve_ASuspendedPolicyBlocksRatherThanFallingBack(t *testing.T) {
	e := newEnv(t)
	e.released(t, tenantA, semver("1.0.0"), period(e.clock.Add(-240*time.Hour), nil))
	e.advance(time.Hour)
	v2 := e.released(t, tenantA, semver("2.0.0"), period(e.clock.Add(-time.Hour), nil))
	q := domain.AccountingCandidateQuery{PolicyDomain: "REVENUE_RECOGNITION"}
	if got := e.governs(t, tenantA, q, e.clock, e.clock); got != v2.PolicyVersionID {
		t.Fatalf("before the suspension: %s", got)
	}
	e.advance(time.Minute)
	if _, err := e.cmd(t, tenantA, &v2.PolicyVersion, domain.CommandSuspend, "ops"); err != nil {
		t.Fatal(err)
	}
	if got := e.governs(t, tenantA, q, e.clock, e.clock); got != string(resolve.NotApplicable) {
		t.Errorf("after the suspension: %s, want NOT_APPLICABLE: an emergency fallback to v1 needs a governed exception", got)
	}
}

// ── the migration ───────────────────────────────────────────────────────────

func TestMigration000009_DownIsRefusedWhileAccountingPoliciesExist(t *testing.T) {
	e := newEnv(t)
	e.acctDraft(t, tenantA, book(bookGB))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "deployments", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	down, up := read("000009_accounting_policy.down.sql"), read("000009_accounting_policy.up.sql")
	ctx := context.Background()
	if _, err := e.admin.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "000009 down refused") {
		t.Fatalf("down with policies present: %v, want it refused", err)
	}
	for _, tbl := range []string{"accounting_policy_rule_mappings", "accounting_policy_version_details", "accounting_policy_applicability"} {
		if _, err := e.admin.Exec(ctx, `ALTER TABLE `+tbl+` DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := e.admin.Exec(ctx, `DELETE FROM `+tbl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.admin.Exec(ctx, down); err != nil {
		t.Fatalf("down with nothing to lose: %v", err)
	}
	if _, err := e.admin.Exec(ctx, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// activeOf is the id of the active version of v's policy and scope, or "".
func (e *testEnv) activeOf(t *testing.T, v *domain.AccountingPolicyVersion) string {
	t.Helper()
	var id string
	err := e.admin.QueryRow(context.Background(), `
		SELECT COALESCE(MAX(policy_version_id::text), '') FROM policy_versions
		WHERE policy_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND legal_entity_id IS NOT DISTINCT FROM $3::uuid
		  AND version_status = 'ACTIVE' AND policy_version_id <> $4`, v.PolicyID, v.TenantID, v.LegalEntityID, v.PolicyVersionID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// releaseOver takes a draft through review, approval and activation, naming the
// version it replaces, if any.
func (e *testEnv) releaseOver(t *testing.T, tenant string, v *domain.AccountingPolicyVersion, supersedes string) {
	t.Helper()
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		if _, err := e.cmd(t, tenant, &v.PolicyVersion, step.cmd, step.actor); err != nil {
			t.Fatalf("%s: %v", step.cmd, err)
		}
	}
	if _, err := e.activate(tenant, v, "activator", supersedes); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

func (e *testEnv) activate(tenant string, v *domain.AccountingPolicyVersion, actor, supersedes string) (*domain.TransitionResult, error) {
	return e.s.Transition(as(tenant), domain.TransitionCommand{
		PolicyVersionID: v.PolicyVersionID, Command: domain.CommandActivate, ActorID: actor, Reason: "test",
		SupersedesVersionID: supersedes, Meta: domain.EventMeta{CorrelationID: "corr", IdempotencyKey: uuid.NewString()},
	})
}
