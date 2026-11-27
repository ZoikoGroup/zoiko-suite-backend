package store_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-03 / INV-26 / NP-26 (docs/architecture/DQC_GAP_MATRIX.md, step I): a source-declared
// subset reaches PARTIAL, can never be certified as the full population, and the database holds that
// rule as well as the service; declared controls are frozen as evidence.

// subsetPops is cleanPops with the Bank side declared a defined subset by its source.
func subsetPops() []domain.FrozenPopulation {
	pops := frozenAll(cleanPops())
	pops[0].Controls = domain.DeclaredControls{Completeness: &domain.DeclaredCompleteness{Scope: "PARTIAL", Description: "EMEA accounts only"}}
	pops[0].ControlOutcomes = []domain.ControlOutcome{{Control: domain.ControlCompleteness, Status: domain.OutcomePartial, Detail: "EMEA accounts only"}}
	return pops
}

func (f *fixture) startWith(def *domain.ReconciliationDefinition, pops []domain.FrozenPopulation) *domain.ReconciliationRun {
	f.t.Helper()
	r, err := f.s.StartRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
		pops, "test-operator", f.claim("StartRun", def.DefinitionID))
	if err != nil {
		f.t.Fatalf("start run: %v", err)
	}
	return r
}

func (f *fixture) mustRefuse(stmt, want string) {
	f.t.Helper()
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, `SELECT set_config('app.tenant_id', $1, true)`, orgA); err != nil {
		f.t.Fatal(err)
	}
	_, err = tx.Exec(f.ctx, stmt)
	if err == nil {
		f.t.Fatalf("the database accepted: %s", stmt)
	}
	if !strings.Contains(err.Error(), want) {
		f.t.Fatalf("refused with %q, want it to mention %q", err, want)
	}
}

func TestPartial_ADeclaredSubsetMatchesToPartialAndIsNeverCertifiable(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-subset", 0)
	run := f.startWith(def, subsetPops())

	got := f.match(run.RunID)
	if got.ResultState != domain.ResultPartial || got.CertificationState != domain.CertUncertified || got.Status != domain.RunRunning {
		t.Fatalf("a matched subset: status %s, result %s, certification %s; want Running / PARTIAL / UNCERTIFIED",
			got.Status, got.ResultState, got.CertificationState)
	}
	// No exception exists: the subset matched. It still cannot be certified (INV-26).
	if ex := f.exceptions(run.RunID); len(ex) != 0 {
		t.Fatalf("exceptions on a clean subset: %d", len(ex))
	}
	_, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID))
	if err == nil {
		t.Fatal("a PARTIAL run was certified")
	}
	if !errors.Is(err, domain.ErrRunNotEligible) {
		t.Fatalf("certify error = %v, want ErrRunNotEligible", err)
	}
	if f.count("certifications") != 0 {
		t.Fatal("a certification row exists for a PARTIAL run")
	}
}

func TestPartial_FullPopulationAndSubsetAreDifferentFindings(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-vs-full", 0)
	full := f.startRun(orgA, def, cleanPops())
	if got := f.match(full.RunID); got.ResultState != domain.ResultMatch || got.CertificationState != domain.CertEligible {
		t.Fatalf("a full population: result %s, certification %s", got.ResultState, got.CertificationState)
	}
}

func TestPartial_AnExceptionInsideASubsetIsVarianceNotPartial(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-variance", 0)
	pops := frozenAll([]testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL"},
	})
	pops[0].Controls = domain.DeclaredControls{Completeness: &domain.DeclaredCompleteness{Scope: "PARTIAL", Description: "one entity"}}
	run := f.startWith(def, pops)
	got := f.match(run.RunID)
	if got.ResultState != domain.ResultVariance || got.Status != domain.RunExceptionsOpen {
		t.Fatalf("an unmatched item in a subset: status %s, result %s; want ExceptionsOpen / VARIANCE", got.Status, got.ResultState)
	}
}

func TestPartial_ReperformingOnTheWholePopulationReturnsToMatchAndCertifies(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-then-full", 0)
	run := f.startWith(def, subsetPops())
	if got := f.match(run.RunID); got.ResultState != domain.ResultPartial {
		t.Fatalf("first read: %s", got.ResultState)
	}
	// The source now delivers the whole population: the run may be re-performed onto it.
	got, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(cleanPops()), "reperformer", f.claim("Reperform", run.RunID))
	if err != nil {
		t.Fatalf("reperform: %v", err)
	}
	if got.ResultState != domain.ResultMatch || got.CertificationState != domain.CertEligible {
		t.Fatalf("after the whole population: result %s, certification %s", got.ResultState, got.CertificationState)
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID)); err != nil {
		t.Fatalf("certify the whole population: %v", err)
	}
}

func TestPartial_ReperformingOntoASubsetWithdrawsEligibility(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "full-then-partial", 0)
	run := f.startRun(orgA, def, cleanPops())
	if got := f.match(run.RunID); got.ResultState != domain.ResultMatch || got.CertificationState != domain.CertEligible {
		t.Fatalf("first read: %s / %s", got.ResultState, got.CertificationState)
	}
	got, err := f.s.Reperform(f.ctx, orgA, run.RunID, subsetPops(), "reperformer", f.claim("Reperform", run.RunID))
	if err != nil {
		t.Fatalf("reperform onto a subset: %v", err)
	}
	if got.ResultState != domain.ResultPartial || got.CertificationState != domain.CertUncertified {
		t.Fatalf("a run re-read as a subset kept its full-population finding: %s / %s", got.ResultState, got.CertificationState)
	}
}

func TestPartial_DatabaseHoldsTheRuleToo(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-db", 0)
	subset := f.startWith(def, subsetPops())
	f.match(subset.RunID) // Running / PARTIAL / UNCERTIFIED
	full := f.startRun(orgA, def, cleanPops())
	f.match(full.RunID) // Running / MATCH / ELIGIBLE

	// A run over a subset cannot be written to MATCH, ELIGIBLE or CERTIFIED by any path.
	f.mustRefuse(`UPDATE reconciliation_runs SET result_state = 'MATCH', certification_state = 'UNCERTIFIED' WHERE run_id = '`+subset.RunID+`'`,
		"declared subset")
	f.mustRefuse(`UPDATE reconciliation_runs SET result_state = 'MATCH' WHERE run_id = '`+subset.RunID+`'`, "declared subset")
	f.mustRefuse(`UPDATE reconciliation_runs SET certification_state = 'ELIGIBLE' WHERE run_id = '`+subset.RunID+`'`, "state_dimensions")
	// A run over a whole population cannot be written to PARTIAL: nothing declared a subset.
	f.mustRefuse(`UPDATE reconciliation_runs SET result_state = 'PARTIAL', certification_state = 'UNCERTIFIED' WHERE run_id = '`+full.RunID+`'`,
		"none of its current populations is a declared subset")
}

func TestPartial_SnapshotColumnsCannotContradictTheDeclaration(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "partial-check", 0)
	run := f.startWith(def, subsetPops())
	insert := func(gen int, completeness, controls string) error {
		_, err := f.admin.Exec(f.ctx, `
			INSERT INTO population_snapshots (snapshot_id, tenant_id, run_id, side, generation, source_system, spec_ref, source_watermark,
				item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash, hash_algorithm, hash_serialization,
				completeness, declared_controls, control_outcomes)
			SELECT 'rps_00000000-0000-0000-0000-0000000000'||lpad($1::int::text, 2, '0'), tenant_id, run_id, side, $1::int, source_system, spec_ref, source_watermark,
				item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash, hash_algorithm, hash_serialization,
				$2, $3::jsonb, '[]'::jsonb
			FROM population_snapshots WHERE run_id = $4 AND side = 'Bank' AND generation = 1`, gen, completeness, controls, run.RunID)
		return err
	}
	for name, tc := range map[string]struct{ completeness, controls string }{
		"PARTIAL with no declaration":         {"PARTIAL", `{}`},
		"declared PARTIAL stored as FULL":     {"FULL", `{"completeness":{"scope":"PARTIAL","description":"x"}}`},
		"PARTIAL without saying which":        {"PARTIAL", `{"completeness":{"scope":"PARTIAL"}}`},
		"PARTIAL with a blank description":    {"PARTIAL", `{"completeness":{"scope":"PARTIAL","description":"  "}}`},
		"declared controls are not an object": {"FULL", `[]`},
	} {
		if err := insert(10, tc.completeness, tc.controls); err == nil {
			t.Fatalf("%s: the database accepted a snapshot whose completeness contradicts its declaration", name)
		}
	}
	if err := insert(11, "PARTIAL", `{"completeness":{"scope":"PARTIAL","description":"x"}}`); err != nil {
		t.Fatalf("a consistent PARTIAL snapshot was refused: %v", err)
	}
}

func TestDeclaredControls_AreFrozenAsEvidenceAndSealedIntoTheCertification(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "declared-evidence", 0)
	pops := frozenAll(cleanPops())
	hash := "ab" + strings.Repeat("0", 62)
	pops[0].Controls = domain.DeclaredControls{
		Hash:         &domain.DeclaredHash{Algorithm: "sha256", Serialization: "cpc-v1", Value: hash},
		Sequence:     &domain.DeclaredSequence{Attribute: "seq", First: 1, Last: 1},
		RollForward:  &domain.DeclaredRollForward{Opening: map[string]string{"USD": "0"}, Closing: map[string]string{"USD": "1.00"}},
		Completeness: &domain.DeclaredCompleteness{Scope: "FULL"},
	}
	pops[0].ControlOutcomes = []domain.ControlOutcome{{Control: domain.ControlHash, Status: domain.OutcomeVerified},
		{Control: domain.ControlSequence, Status: domain.OutcomeVerified}}
	run := f.startWith(def, pops)

	snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %d, %v", len(snaps), err)
	}
	var bank, ledger domain.PopulationSnapshot
	for _, s := range snaps {
		if s.Side == "Bank" {
			bank = s
		} else {
			ledger = s
		}
	}
	if bank.Completeness != "FULL" || bank.DeclaredControls.Hash == nil || bank.DeclaredControls.Hash.Value != hash ||
		bank.DeclaredControls.Sequence.Last != 1 || bank.DeclaredControls.RollForward.Closing["USD"] != "1.00" || len(bank.ControlOutcomes) != 2 {
		t.Fatalf("the declared controls were not frozen with the snapshot: %+v", bank)
	}
	if !ledger.DeclaredControls.IsZero() || len(ledger.ControlOutcomes) != 0 || ledger.Completeness != "FULL" {
		t.Fatalf("a source that declared nothing must freeze nothing: %+v", ledger)
	}

	f.match(run.RunID)
	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify a full population with declared controls: %v", err)
	}
	var sealed *domain.CertifiedPopulation
	for i := range cert.Populations {
		if cert.Populations[i].Side == "Bank" {
			sealed = &cert.Populations[i]
		}
	}
	if sealed == nil || sealed.DeclaredControls == nil || sealed.DeclaredControls.Hash == nil || len(sealed.ControlOutcomes) != 2 {
		t.Fatalf("the sealed certification does not carry the declared controls: %+v", sealed)
	}
}

func TestDeclaredControls_ChangeTheCertificationBasisOnlyWhenDeclared(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "basis-controls", 0)
	// Identical evidence, certified twice with nothing declared: the second is a duplicate basis (existing rule).
	same := cleanPops() // the same records, so the same content hash
	a := f.startRun(orgA, def, same)
	f.matchAndCertify(a.RunID)
	b := f.startRun(orgA, def, same)
	f.match(b.RunID)
	if _, err := f.s.Certify(f.ctx, orgA, b.RunID, "certifier", grant("certifier"), f.claim("Certify", b.RunID)); !errors.Is(err, domain.ErrCertificationBasisExists) {
		t.Fatalf("identical evidence with no declared controls must still be a duplicate basis: %v", err)
	}
	// The same records but a source that now also declares a closing balance is different evidence.
	pops := frozenAll(same)
	pops[0].Controls = domain.DeclaredControls{RollForward: &domain.DeclaredRollForward{Opening: map[string]string{"USD": "0"}, Closing: map[string]string{"USD": "1.00"}}}
	c := f.startWith(def, pops)
	f.match(c.RunID)
	if _, err := f.s.Certify(f.ctx, orgA, c.RunID, "certifier", grant("certifier"), f.claim("Certify", c.RunID)); err != nil {
		t.Fatalf("evidence with declared controls is a different basis: %v", err)
	}
}

func TestPriorClosing_IsTheLatestCertifiedNotRevokedPeriodOfTheSameEntityAndPopulation(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "prior-closing", 0)
	period := func(start, end, open, closeBal string) []domain.FrozenPopulation {
		p := frozenAll(cleanPops())
		p[0].Controls = domain.DeclaredControls{RollForward: &domain.DeclaredRollForward{Opening: map[string]string{"USD": open},
			Closing: map[string]string{"USD": closeBal}, PeriodStart: start, PeriodEnd: end}}
		p[0].Watermark = "wm-" + start
		return p
	}
	certify := func(pops []domain.FrozenPopulation) *domain.ReconciliationRun {
		r := f.startWith(def, pops)
		f.match(r.RunID)
		if _, err := f.s.Certify(f.ctx, orgA, r.RunID, "certifier", grant("certifier"), f.claim("Certify", r.RunID)); err != nil {
			t.Fatalf("certify: %v", err)
		}
		return r
	}
	spec := "BANK/bank-transactions"

	if got, err := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Bank", "2026-09-01"); err != nil || got != nil {
		t.Fatalf("nothing certified yet: %+v, %v", got, err)
	}
	aug := certify(period("2026-08-01", "2026-08-31", "0", "100.00"))
	got, err := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Bank", "2026-09-01")
	if err != nil || got == nil || got.RunID != aug.RunID || got.PeriodEnd != "2026-08-31" || got.Closing["USD"] != "100.00" {
		t.Fatalf("prior closing = %+v, %v", got, err)
	}
	// Not for a period that is not after it, another side, another entity or another population.
	if got, _ := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Bank", "2026-08-31"); got != nil {
		t.Fatalf("a period ending on the start date is not before it: %+v", got)
	}
	if got, _ := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Ledger", "2026-09-01"); got != nil {
		t.Fatalf("another side: %+v", got)
	}
	if got, _ := f.s.PriorClosing(f.ctx, orgA, entityB, spec, "Bank", "2026-09-01"); got != nil {
		t.Fatalf("another entity: %+v", got)
	}
	if got, _ := f.s.PriorClosing(f.ctx, orgA, entityA, "BANK/other", "Bank", "2026-09-01"); got != nil {
		t.Fatalf("another population: %+v", got)
	}
	if got, _ := f.s.PriorClosing(f.ctx, orgB, entityA, spec, "Bank", "2026-09-01"); got != nil {
		t.Fatalf("another tenant must see nothing: %+v", got)
	}
}

// A revoked certification is no longer authoritative, so its closing balance cannot be what the
// next period's opening is tied to.
func TestPriorClosing_IgnoresARevokedCertification(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "prior-revoked", 0)
	pops := frozenAll(cleanPops())
	pops[0].Controls = domain.DeclaredControls{RollForward: &domain.DeclaredRollForward{Opening: map[string]string{"USD": "0"},
		Closing: map[string]string{"USD": "100"}, PeriodStart: "2026-08-01", PeriodEnd: "2026-08-31"}}
	run := f.startWith(def, pops)
	f.match(run.RunID)
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID)); err != nil {
		t.Fatal(err)
	}
	spec := "BANK/bank-transactions"
	if got, _ := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Bank", "2026-09-01"); got == nil {
		t.Fatal("a certified period must supply its closing")
	}
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", run.RunID)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got, err := f.s.PriorClosing(f.ctx, orgA, entityA, spec, "Bank", "2026-09-01"); err != nil || got != nil {
		t.Fatalf("a revoked certification supplied a closing balance: %+v, %v", got, err)
	}
}
