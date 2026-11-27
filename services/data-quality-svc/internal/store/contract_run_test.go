package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-02 / DQC-05 with a DQC-01 contract as the rule source (docs/architecture/DQC_GAP_MATRIX.md,
// step F2): a run is judged against an exact PUBLISHED contract version; results carry the rule's severity,
// dimension and certification-profile membership; only a MANDATORY rule's failure blocks certification.

// profiled is a contract with four rules of four severities. Its certification profile is stated, not
// defaulted, and leaves out the WARNING rule: certification requires the BLOCKER and the ERROR, but not it.
func profiled(mode domain.Compatibility) domain.ContractBody {
	b := contractBody(mode)
	b.Rules = []domain.ContractRule{
		{RuleKey: "amount-positive", Dimension: "validity", Severity: domain.SeverityBlocker, Predicate: "positive"},
		{RuleKey: "invoice-unique", Dimension: "uniqueness", Severity: domain.SeverityError},
		{RuleKey: "memo-present", Dimension: "completeness", Severity: domain.SeverityInfo},
		{RuleKey: "format-ok", Dimension: "validity", Severity: domain.SeverityWarning},
	}
	b.CertificationProfile.MandatoryRules = []string{"amount-positive", "invoice-unique"}
	return b
}

func pass(rule string) domain.RuleOutcome {
	return domain.RuleOutcome{RuleKey: rule, PassCount: 100, FailCount: 0, FailureMagnitudeSum: "0"}
}

func fail(rule string) domain.RuleOutcome {
	return domain.RuleOutcome{RuleKey: rule, PassCount: 90, FailCount: 10, FailureMagnitudeSum: "50"}
}

func (f *fixture) contractRun(c *domain.ContractVersion, ref string, outcomes ...domain.RuleOutcome) (*domain.DQRun, error) {
	f.t.Helper()
	return f.contractRunAt(c, ref, nil, outcomes...)
}

func (f *fixture) contractRunAt(c *domain.ContractVersion, ref string, asOf *time.Time, outcomes ...domain.RuleOutcome) (*domain.DQRun, error) {
	f.t.Helper()
	return f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{EvidenceAsOf: asOf,
		ContractAssetID: c.AssetID, ContractVersion: c.VersionNumber, PopulationRef: ref, PopulationRowCount: 100,
		PopulationContentHash: hashLike(ref), RuleOutcomes: outcomes,
	}, "test-operator", f.claim("RunDQ", ref))
}

func (f *fixture) mustContractRun(c *domain.ContractVersion, ref string, outcomes ...domain.RuleOutcome) *domain.DQRun {
	f.t.Helper()
	r, err := f.contractRun(c, ref, outcomes...)
	if err != nil {
		f.t.Fatalf("contract run: %v", err)
	}
	return r
}

func (f *fixture) certify(run *domain.DQRun) (*domain.DQCertification, error) {
	return f.s.CertifyDQ(f.ctx, orgA, run.RunID, "test-operator", domain.CertifyDQRequest{}, f.claim("CertifyDQ", run.RunID))
}

func allPass() []domain.RuleOutcome {
	return []domain.RuleOutcome{pass("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok")}
}

func TestContractRun_ResultsCarryTheContractsSeverityDimensionAndMandatory(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.basic", profiled(domain.CompatBackward))
	run := f.mustContractRun(c, "pop-basic", allPass()...)
	if run.ContractID == nil || *run.ContractID != c.ContractID || run.RuleSetVersionID != "" {
		t.Fatalf("run is not bound to the contract: %+v", run)
	}
	if run.Status != domain.RunRunning {
		t.Fatalf("status = %s", run.Status)
	}
	want := map[string]struct {
		sev  domain.Severity
		dim  string
		mand bool
	}{
		"amount-positive": {domain.SeverityBlocker, "validity", true},
		"invoice-unique":  {domain.SeverityError, "uniqueness", true},
		"memo-present":    {domain.SeverityInfo, "completeness", false},
		"format-ok":       {domain.SeverityWarning, "validity", false},
	}
	for rule, w := range want {
		r := resultFor(t, f, run.RunID, rule)
		if r.Severity == nil || *r.Severity != w.sev || r.Dimension == nil || *r.Dimension != w.dim || r.Mandatory == nil || *r.Mandatory != w.mand {
			t.Fatalf("%s: severity/dimension/mandatory = %v/%v/%v, want %v/%s/%v", rule, deref(r.Severity), r.Dimension, r.Mandatory, w.sev, w.dim, w.mand)
		}
	}
}

func deref(s *domain.Severity) any {
	if s == nil {
		return nil
	}
	return *s
}

// A rule-set run is unchanged: no severity is invented for it.
func TestContractRun_RuleSetRunsCarryNoSeverity(t *testing.T) {
	f := newContractFixture(t)
	v := f.publishRuleSet(orgA, "legacy-no-severity", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	run, err := f.run(v, "pop-legacy", passingOutcome("r-a"))
	if err != nil {
		t.Fatal(err)
	}
	if run.ContractID != nil || run.RuleSetVersionID != v.VersionID {
		t.Fatalf("legacy run: %+v", run)
	}
	if r := resultFor(t, f, run.RunID, "r-a"); r.Severity != nil || r.Dimension != nil || r.Mandatory != nil {
		t.Fatalf("a rule-set result carries contract fields: %+v", r)
	}
}

func TestContractRun_OnlyAPublishedContractCanBeJudgedAgainst(t *testing.T) {
	f := newContractFixture(t)
	draft := f.mustCreate(orgA, "run.draft", profiled(domain.CompatBackward))
	if _, err := f.contractRun(draft, "pop-d", allPass()...); !errors.Is(err, domain.ErrContractNotPublished) {
		t.Fatalf("a DRAFT contract: %v", err)
	}
	if _, err := f.submit(draft, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.contractRun(draft, "pop-d", allPass()...); !errors.Is(err, domain.ErrContractNotPublished) {
		t.Fatalf("a REVIEW contract: %v", err)
	}
	pub, err := f.publish(draft, publisher, false)
	if err != nil {
		t.Fatal(err)
	}
	// The same request now succeeds: it was publication that made the difference.
	if _, err := f.contractRun(pub, "pop-d", allPass()...); err != nil {
		t.Fatalf("a PUBLISHED contract was refused: %v", err)
	}
	// An unknown version fails safe (NP-01), as does an unknown asset.
	bad := *pub
	bad.VersionNumber = 9
	if _, err := f.contractRun(&bad, "pop-u", allPass()...); !errors.Is(err, domain.ErrContractVersionUnknown) {
		t.Fatalf("an unknown version: %v", err)
	}
	bad = *pub
	bad.AssetID = "dca_00000000-0000-0000-0000-000000000000"
	if _, err := f.contractRun(&bad, "pop-u2", allPass()...); !errors.Is(err, domain.ErrContractNotFound) {
		t.Fatalf("an unknown asset: %v", err)
	}
	// A deprecated contract is not used for NEW evidence.
	if _, err := f.s.DeprecateContract(f.ctx, orgA, pub.AssetID, 1, publisher, "replaced", f.claim("D", "rd")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.contractRun(pub, "pop-after", allPass()...); !errors.Is(err, domain.ErrContractNotPublished) {
		t.Fatalf("a DEPRECATED contract: %v", err)
	}
}

func TestContractRun_RequestMustNameExactlyOneRuleSource(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.source", profiled(domain.CompatBackward))
	v := f.publishRuleSet(orgA, "legacy-source", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	base := domain.RunDQRequest{PopulationRef: "p", PopulationRowCount: 1, PopulationContentHash: hashLike("p"), RuleOutcomes: []domain.RuleOutcome{pass("r-a")}}
	for name, mutate := range map[string]func(*domain.RunDQRequest){
		"neither": func(*domain.RunDQRequest) {},
		"both": func(r *domain.RunDQRequest) {
			r.RuleSetVersionID, r.ContractAssetID, r.ContractVersion = v.VersionID, c.AssetID, 1
			r.RuleOutcomes = allPass() // valid for the contract, so only the double source can be what is refused
		},
		"contract with no asset":                      func(r *domain.RunDQRequest) { r.ContractVersion = 1 },
		"contract with no version (never the latest)": func(r *domain.RunDQRequest) { r.ContractAssetID = c.AssetID },
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if _, err := f.s.RunDQ(f.ctx, orgA, r, "test-operator", f.claim("RunDQ", name)); err == nil {
				t.Fatal("the request was accepted")
			}
		})
	}
}

// The heart of F2: severity decides what blocks certification.
func TestContractRun_OnlyAMandatoryFailureBlocksCertification(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.mand", profiled(domain.CompatBackward))

	// A WARNING and an INFO rule fail; the profile does not require them: the run is clean and certifies.
	ok := f.mustContractRun(c, "pop-optional-fails", pass("amount-positive"), pass("invoice-unique"), fail("memo-present"), fail("format-ok"))
	if ok.Status != domain.RunRunning {
		t.Fatalf("a non-mandatory failure opened exceptions: %s", ok.Status)
	}
	cert, err := f.certify(ok)
	if err != nil {
		t.Fatalf("a run whose only failures are non-mandatory could not certify: %v", err)
	}
	// ...and the certification does not hide them: they are in the sealed summary, with their severity.
	failed := 0
	for _, r := range cert.Summary.Results {
		if r.Status == domain.ResultFail {
			failed++
			if r.Severity == nil || r.Mandatory == nil || *r.Mandatory {
				t.Fatalf("a sealed failing result lost its severity/mandatory: %+v", r)
			}
		}
	}
	if failed != 2 {
		t.Fatalf("the sealed summary has %d failing results, want the 2 that failed", failed)
	}
	if cert.Summary.ContractID == "" || len(cert.Summary.ContractSHA256) != 64 {
		t.Fatalf("the certification does not name the contract it was judged against: %+v", cert.Summary)
	}

	// A mandatory ERROR rule fails: exceptions open and the run cannot certify.
	for _, rule := range []string{"invoice-unique", "amount-positive"} {
		outs := map[string]domain.RuleOutcome{"amount-positive": pass("amount-positive"), "invoice-unique": pass("invoice-unique")}
		outs[rule] = fail(rule)
		bad := f.mustContractRun(c, "pop-bad-"+rule, outs["amount-positive"], outs["invoice-unique"], pass("memo-present"), pass("format-ok"))
		if bad.Status != domain.RunExceptionsOpen {
			t.Fatalf("a failing mandatory %s left the run %s", rule, bad.Status)
		}
		if _, err := f.certify(bad); err == nil {
			t.Fatalf("certified a run with a failing mandatory rule %s", rule)
		}
	}
}

// A mandatory rule with no evidence is Indeterminate and blocks; an optional one with no evidence does not.
func TestContractRun_MissingEvidenceFollowsMandatory(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.missing", profiled(domain.CompatBackward))

	optionalMissing := f.mustContractRun(c, "pop-opt-missing", pass("amount-positive"), pass("invoice-unique"))
	if optionalMissing.Status != domain.RunRunning {
		t.Fatalf("missing optional evidence opened exceptions: %s", optionalMissing.Status)
	}
	if r := resultFor(t, f, optionalMissing.RunID, "memo-present"); r.Status != domain.ResultIndeterminate || r.Severity == nil {
		t.Fatalf("missing optional rule: %+v", r)
	}
	if _, err := f.certify(optionalMissing); err != nil {
		t.Fatalf("certification was blocked by missing OPTIONAL evidence: %v", err)
	}

	mandatoryMissing := f.mustContractRun(c, "pop-mand-missing", pass("amount-positive"), pass("memo-present"), pass("format-ok"))
	if mandatoryMissing.Status != domain.RunExceptionsOpen {
		t.Fatalf("missing mandatory evidence: %s", mandatoryMissing.Status)
	}
	if r := resultFor(t, f, mandatoryMissing.RunID, "invoice-unique"); r.Status != domain.ResultIndeterminate {
		t.Fatalf("missing mandatory rule: %+v", r)
	}
}

// Certification re-derives what is mandatory from the contract itself; it does not trust the run's status.
// A run forced to look clean while a mandatory rule is not Pass is still refused.
func TestContractRun_CertificationRechecksMandatoryEvidence(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.recheck", profiled(domain.CompatBackward))
	bad := f.mustContractRun(c, "pop-recheck", fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
	if _, err := f.admin.Exec(f.ctx, `ALTER TABLE dq_runs DISABLE TRIGGER trigger_dq_run_lifecycle`); err != nil {
		t.Skipf("cannot relax the lifecycle trigger for this check: %v", err)
	}
	defer f.admin.Exec(f.ctx, `ALTER TABLE dq_runs ENABLE TRIGGER trigger_dq_run_lifecycle`) //nolint:errcheck
	if _, err := f.admin.Exec(f.ctx, `UPDATE dq_runs SET status='Running' WHERE run_id=$1`, bad.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.certify(bad); !errors.Is(err, domain.ErrEvidenceIncomplete) {
		t.Fatalf("certified on a forged clean status: %v", err)
	}
}

func TestContractRun_ToleranceDecidesPassAndFail(t *testing.T) {
	f := newContractFixture(t)
	b := profiled(domain.CompatBackward)
	b.Rules[1].Tolerance = &domain.Tolerance{Kind: "PERCENT", Value: "10", Scale: 2, Rounding: "HALF_UP", Population: "all invoices"}
	b.Rules[0].Tolerance = &domain.Tolerance{Kind: "ABSOLUTE", Value: "50", Scale: 2, Rounding: "HALF_UP", Population: "all invoices"}
	c := f.published(orgA, "run.tol", b)
	outcomes := func(amount, unique domain.RuleOutcome) []domain.RuleOutcome {
		return []domain.RuleOutcome{amount, unique, pass("memo-present"), pass("format-ok")}
	}
	cases := []struct {
		name   string
		amount domain.RuleOutcome
		unique domain.RuleOutcome
		want   domain.DQResultStatus // of invoice-unique (PERCENT 10) then amount-positive (ABSOLUTE 50)
		wantA  domain.DQResultStatus
	}{
		{"exactly at both limits passes", domain.RuleOutcome{RuleKey: "amount-positive", PassCount: 99, FailCount: 1, FailureMagnitudeSum: "50"},
			domain.RuleOutcome{RuleKey: "invoice-unique", PassCount: 90, FailCount: 10, FailureMagnitudeSum: "0"}, domain.ResultPass, domain.ResultPass},
		{"just over both limits fails", domain.RuleOutcome{RuleKey: "amount-positive", PassCount: 99, FailCount: 1, FailureMagnitudeSum: "50.01"},
			domain.RuleOutcome{RuleKey: "invoice-unique", PassCount: 89, FailCount: 11, FailureMagnitudeSum: "0"}, domain.ResultFail, domain.ResultFail},
	}
	for i, tc := range cases {
		run := f.mustContractRun(c, "pop-tol-"+string(rune('a'+i)), outcomes(tc.amount, tc.unique)...)
		if got := resultFor(t, f, run.RunID, "invoice-unique").Status; got != tc.want {
			t.Fatalf("%s: PERCENT rule = %s, want %s", tc.name, got, tc.want)
		}
		if got := resultFor(t, f, run.RunID, "amount-positive").Status; got != tc.wantA {
			t.Fatalf("%s: ABSOLUTE rule = %s, want %s", tc.name, got, tc.wantA)
		}
	}
	// No tolerance means strict: one failure fails.
	strict := f.mustContractRun(c, "pop-tol-strict", pass("amount-positive"), pass("invoice-unique"),
		domain.RuleOutcome{RuleKey: "memo-present", PassCount: 99, FailCount: 1, FailureMagnitudeSum: "0"}, pass("format-ok"))
	if got := resultFor(t, f, strict.RunID, "memo-present").Status; got != domain.ResultFail {
		t.Fatalf("a rule with no tolerance and one failure = %s, want Fail", got)
	}
}

// A run is evidence about one contract version. A reperform cannot quietly move to another.
func TestContractRun_ReperformKeepsTheSameContractVersion(t *testing.T) {
	f := newContractFixture(t)
	c1 := f.published(orgA, "run.rep", profiled(domain.CompatBackward))
	bad := f.mustContractRun(c1, "pop-rep", fail("amount-positive"), pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
	if bad.Status != domain.RunExceptionsOpen {
		t.Fatal(bad.Status)
	}
	reperform := func(req domain.RunDQRequest) (*domain.DQRun, error) {
		return f.s.Reperform(f.ctx, orgA, bad.RunID, req, "test-operator", f.claim("Reperform", bad.RunID))
	}
	base := func(c *domain.ContractVersion) domain.RunDQRequest {
		return domain.RunDQRequest{ContractAssetID: c.AssetID, ContractVersion: c.VersionNumber, PopulationRef: "pop-rep-2", PopulationRowCount: 100,
			PopulationContentHash: hashLike("pop-rep-2"), RuleOutcomes: allPass()}
	}
	// v2 of the contract, published.
	b2 := profiled(domain.CompatBackward)
	b2.Schema = append(b2.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
	v2 := f.mustCreate(orgA, "run.rep", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	v2p, err := f.publish(v2, publisher, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reperform(base(v2p)); err == nil {
		t.Fatal("a reperform moved to a different contract version")
	}
	rs := f.publishRuleSet(orgA, "rep-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	if _, err := reperform(domain.RunDQRequest{RuleSetVersionID: rs.VersionID, PopulationRef: "p", PopulationRowCount: 1, PopulationContentHash: hashLike("p"), RuleOutcomes: []domain.RuleOutcome{pass("r-a")}}); err == nil {
		t.Fatal("a reperform moved from a contract to a rule set")
	}
	fixed, err := reperform(base(c1))
	if err != nil {
		t.Fatalf("a reperform against the same contract version was refused: %v", err)
	}
	if fixed.Status != domain.RunRunning || fixed.SupersedesRunID == nil || *fixed.SupersedesRunID != bad.RunID {
		t.Fatalf("reperformed run: %+v", fixed)
	}
	if _, err := f.certify(fixed); err != nil {
		t.Fatalf("certify after a clean reperform: %v", err)
	}
}

// A run's evidence is bound to the exact version it was judged against, so a later deprecation of the
// contract does not strand a run that already exists.
func TestContractRun_CertifiesAfterTheContractIsDeprecated(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.dep", profiled(domain.CompatBackward))
	run := f.mustContractRun(c, "pop-dep", allPass()...)
	if _, err := f.s.DeprecateContract(f.ctx, orgA, c.AssetID, 1, publisher, "replaced", f.claim("D", "dep")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.certify(run); err != nil {
		t.Fatalf("a run judged against a since-deprecated contract could not certify: %v", err)
	}
}

// The same evidence against the same contract has one certification; the basis names the contract, so the
// same population judged against a different version is a different basis.
func TestContractRun_BasisNamesTheContract(t *testing.T) {
	f := newContractFixture(t)
	c1 := f.published(orgA, "run.basis", profiled(domain.CompatBackward))
	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	run, err := f.contractRunAt(c1, "pop-basis", &observed, allPass()...)
	if err != nil {
		t.Fatal(err)
	}
	cert1, err := f.certify(run)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.contractRunAt(c1, "pop-basis", &observed, allPass()...)
	if err != nil {
		t.Fatal(err)
	}
	// The same evidence against the same contract is ONE run (the evidence fingerprint), so there is no second
	// run to certify: the request returns the existing, already certified run, which cannot be certified again.
	if !again.Deduplicated || again.RunID != run.RunID {
		t.Fatalf("the same evidence made a second run: %+v", again)
	}
	if _, err := f.certify(again); err == nil {
		t.Fatal("the same evidence against the same contract certified twice")
	}
	b2 := profiled(domain.CompatBackward)
	b2.Schema = append(b2.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
	v2 := f.mustCreate(orgA, "run.basis", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	v2p, err := f.publish(v2, publisher, false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.contractRunAt(v2p, "pop-basis", &observed, allPass()...)
	if err != nil {
		t.Fatal(err)
	}
	cert2, err := f.certify(other)
	if err != nil {
		t.Fatalf("the same population against a different contract version was refused: %v", err)
	}
	if cert1.BasisSHA256 == cert2.BasisSHA256 {
		t.Fatal("two contract versions share a certification basis")
	}
}

func TestContractRun_ValidationEventCarriesSeverity(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.evt", profiled(domain.CompatBackward))
	run := f.mustContractRun(c, "pop-evt", fail("amount-positive"), pass("invoice-unique"), fail("memo-present"), pass("format-ok"))
	var n int
	var blockers int
	var contractID string
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*), COALESCE(max((payload->>'blocker_not_passed')::int), -1), COALESCE(max(payload->>'contract_id'), '')
		FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.validation.completed'`, run.RunID).Scan(&n, &blockers, &contractID); err != nil {
		t.Fatal(err)
	}
	if n != 1 || blockers != 1 || contractID != c.ContractID {
		t.Fatalf("events=%d blockers_not_passed=%d contract=%q", n, blockers, contractID)
	}
	// A rule-set run emits no dqc event.
	v := f.publishRuleSet(orgA, "legacy-evt", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	lr, err := f.run(v, "pop-evt-legacy", passingOutcome("r-a"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type LIKE 'dqc.%'`, lr.RunID).Scan(&n)
	if n != 0 {
		t.Fatalf("a rule-set run emitted %d dqc events", n)
	}
}

// The same rules, held by the database.
func TestContractRun_DatabaseConstraints(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "run.db", profiled(domain.CompatBackward))
	v := f.publishRuleSet(orgA, "db-legacy", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	runSQL := `INSERT INTO dq_runs (run_id, tenant_id, ruleset_version_id, contract_id, population_ref, population_row_count, population_content_hash, status, created_by)
		VALUES ($1, $2, $3, $4, 'p', 1, repeat('a',64), 'Running', 'x')`
	id := func() string { return newTestRunID() }
	if _, err := f.admin.Exec(f.ctx, runSQL, id(), orgA, v.VersionID, c.ContractID); err == nil {
		t.Fatal("a run with both rule sources was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, runSQL, id(), orgA, nil, nil); err == nil {
		t.Fatal("a run with no rule source was accepted")
	}
	// Proof that those two refusals were about the rule source: each single source is accepted.
	if _, err := f.admin.Exec(f.ctx, runSQL, id(), orgA, v.VersionID, nil); err != nil {
		t.Fatalf("a rule-set run was refused: %v", err)
	}
	runID := id()
	if _, err := f.admin.Exec(f.ctx, runSQL, runID, orgA, nil, c.ContractID); err != nil {
		t.Fatalf("a contract run was refused: %v", err)
	}
	resSQL := `INSERT INTO dq_results (result_id, tenant_id, run_id, rule_key, status, severity, dimension, mandatory)
		VALUES ($1, $2, $3, $4, 'Pass', $5, $6, $7)`
	res := func() string { return "dqr_" + newTestRunID()[4:] }
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "a", "FATAL", "validity", true); err == nil {
		t.Fatal("an unknown severity was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "b", "ERROR", nil, true); err == nil {
		t.Fatal("a severity without a dimension was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "c", "BLOCKER", "validity", false); err == nil {
		t.Fatal("a non-mandatory BLOCKER was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "d", "BLOCKER", "validity", true); err != nil {
		t.Fatalf("a mandatory BLOCKER was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "e", "WARNING", "validity", false); err != nil {
		t.Fatalf("a non-mandatory WARNING was refused: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, resSQL, res(), orgA, runID, "f", nil, nil, nil); err != nil {
		t.Fatalf("a result with no contract fields was refused: %v", err)
	}
}

func newTestRunID() string { return "dqn_" + uuid.NewString() }

// The tolerance's rounding mode and scale are applied: the same failure magnitude passes under DOWN and fails
// under HALF_UP, because 50.9 is 50 and 51 respectively at scale 0, against a limit of 50.
func TestContractRun_ToleranceRoundingDecidesTheResult(t *testing.T) {
	f := newContractFixture(t)
	outcome := domain.RuleOutcome{RuleKey: "amount-positive", PassCount: 99, FailCount: 1, FailureMagnitudeSum: "50.9"}
	for mode, want := range map[string]domain.DQResultStatus{"DOWN": domain.ResultPass, "HALF_UP": domain.ResultFail} {
		b := profiled(domain.CompatBackward)
		b.Rules[0].Tolerance = &domain.Tolerance{Kind: "ABSOLUTE", Value: "50", Scale: 0, Rounding: mode, Population: "all invoices"}
		c := f.published(orgA, "run.round."+strings.ToLower(mode), b)
		run := f.mustContractRun(c, "pop-round-"+mode, outcome, pass("invoice-unique"), pass("memo-present"), pass("format-ok"))
		if got := resultFor(t, f, run.RunID, "amount-positive").Status; got != want {
			t.Fatalf("%s: %s, want %s", mode, got, want)
		}
	}
}
