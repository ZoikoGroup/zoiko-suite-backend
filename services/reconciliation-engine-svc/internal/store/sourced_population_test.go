package store_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// A1: a frozen snapshot pins what it was read with and verified against —
// spec, watermark, declared totals — and its own control totals and hash;
// items keep their source record id and currency.
func TestStartRun_PinsSourceEvidence(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "evidence", 0)
	bank := []domain.PopulationItemInput{item("REF-1", 1250), item("REF-2", -300)}
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: bank},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 1250), item("REF-2", -300)}},
	})
	if run.LegalEntityID != entityA || run.PeriodID != "2026-09" || run.AmountScale != 2 {
		t.Fatalf("run does not pin entity/period/scale: %+v", run)
	}

	snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %d (err=%v)", len(snaps), err)
	}
	b := snaps[0]
	if b.Side != "Bank" || b.SpecRef != "BANK/bank-transactions" || b.SourceWatermark != "wm-Bank" {
		t.Fatalf("snapshot source identity: %+v", b)
	}
	if b.DeclaredRowCount == nil || *b.DeclaredRowCount != 2 || b.DeclaredTotals["USD"] != "9.50" {
		t.Fatalf("declared totals not pinned: %+v", b)
	}
	if len(b.ControlTotals) != 1 || b.ControlTotals[0] != (domain.CurrencyTotal{Currency: "USD", Count: 2, TotalMinorUnits: 950}) {
		t.Fatalf("control totals: %+v", b.ControlTotals)
	}
	if b.AmountScale == nil || *b.AmountScale != 2 || b.ContentHash != domain.HashPopulation(bank) {
		t.Fatalf("scale/hash: %+v", b)
	}

	var withSource, withCurrency int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(source_record_id), count(currency) FROM population_items WHERE snapshot_id = $1`,
		b.SnapshotID).Scan(&withSource, &withCurrency); err != nil {
		t.Fatal(err)
	}
	if withSource != 2 || withCurrency != 2 {
		t.Fatalf("items without source record id / currency: %d, %d", withSource, withCurrency)
	}

	// Signed source amounts reconcile like any other.
	matched, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match", run.RunID))
	if err != nil || matched.Status != domain.RunRunning {
		t.Fatalf("match signed amounts: %+v (err=%v)", matched, err)
	}
}

// The populations were read for one definition version; if the definition
// changed meanwhile, they may not be the populations it now names.
func TestStartRun_RefusesWhenDefinitionChangedDuringRead(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "moving", 0)
	tol := int64(50)
	f.publish(def, func(r *domain.RulesInput) { // v2 approved and in effect
		r.Tolerance.Currencies["USD"] = domain.CurrencyTolerance{AbsoluteMinorUnits: &tol}
	})
	if _, err := f.startRunE(orgA, def, cleanPops()); !errors.Is(err, domain.ErrDefinitionChanged) {
		t.Fatalf("start against a stale definition version: %v", err)
	}
}

func TestStartRun_RefusesPopulationsTheDefinitionDoesNotName(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "named", 0)
	pops := cleanPops()
	pops[1].SourceSystem = "SOMETHING-ELSE"
	if _, err := f.startRunE(orgA, def, pops); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("population from an unnamed system: %v", err)
	}
}

// A snapshot cannot be frozen without its completeness evidence: refused by
// the store, and by the table's own CHECK for any other writer.
func TestStartRun_RefusesPopulationWithoutEvidence(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "no-evidence", 0)
	req := domain.StartRunRequest{DefinitionID: def.DefinitionID}

	noWatermark := frozenAll(cleanPops())
	noWatermark[0].Watermark = ""
	if _, err := f.s.StartRun(f.ctx, orgA, req, def.Version, noWatermark, "test-operator", f.claim("StartRun", "a")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("population without watermark: %v", err)
	}
	short := frozenAll(cleanPops())
	short[1].DeclaredRowCount = 5
	if _, err := f.s.StartRun(f.ctx, orgA, req, def.Version, short, "test-operator", f.claim("StartRun", "b")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("population shorter than declared: %v", err)
	}

	run := f.startRun(orgA, def, cleanPops())
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(f.ctx, `
		INSERT INTO population_snapshots (snapshot_id, tenant_id, run_id, side, source_system, item_count, content_hash)
		VALUES ('rps_00000000-0000-0000-0000-000000000001', $1, $2, 'Third', 'X', 0, repeat('0', 64))`, orgA, run.RunID)
	if err == nil || !strings.Contains(err.Error(), "population_snapshots_sourced_check") {
		t.Fatalf("a snapshot without watermark/declared totals: %v, want population_snapshots_sourced_check", err)
	}
}

// A14: a side whose total does not fit an int64 refuses the run instead of
// wrapping, and nothing is frozen.
func TestStartRun_RefusesOverflowingTotal(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "overflow", 0)
	pops := []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("A", math.MaxInt64-5), item("B", 10)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("A", 1)}},
	}
	if _, err := f.startRunE(orgA, def, pops); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Fatalf("overflowing side total: %v", err)
	}
	var runs int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM reconciliation_runs WHERE definition_id = $1`, def.DefinitionID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("a refused run left %d run rows behind", runs)
	}
}

// A10: definitions are scoped to a legal entity, a run inherits its
// definition's entity, and neither can be moved to another entity.
func TestLegalEntity_ScopesDefinitionsAndRuns(t *testing.T) {
	f := newFixture(t)
	a := f.defineReconciliation(orgA, "shared-name", 0)
	reqB := defineRequest("shared-name", 0)
	reqB.LegalEntityID = entityB
	b := f.defineApproved(orgA, reqB)
	if a.DefinitionID == b.DefinitionID || a.LegalEntityID != entityA || b.LegalEntityID != entityB {
		t.Fatalf("one name in two entities must be two definitions: %+v / %+v", a, b)
	}
	got, err := f.s.GetDefinition(f.ctx, orgA, b.DefinitionID)
	if err != nil || got.LegalEntityID != entityB || len(got.Populations) != 2 || got.AmountScale != 2 {
		t.Fatalf("get definition: %+v (err=%v)", got, err)
	}
	if _, err := f.s.GetDefinition(f.ctx, orgB, b.DefinitionID); !errors.Is(err, domain.ErrDefinitionNotFound) {
		t.Fatalf("cross-tenant get definition: %v", err)
	}

	run := f.startRun(orgA, b, cleanPops())
	if run.LegalEntityID != entityB {
		t.Fatalf("run entity %q, want the definition's %q", run.LegalEntityID, entityB)
	}

	for name, stmt := range map[string]string{
		"run":        `UPDATE reconciliation_runs SET legal_entity_id = '` + entityA + `' WHERE run_id = '` + run.RunID + `'`,
		"definition": `UPDATE reconciliation_definitions SET legal_entity_id = '` + entityA + `' WHERE definition_id = '` + b.DefinitionID + `'`,
	} {
		tx, err := f.app.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
		_, err = tx.Exec(f.ctx, stmt)
		_ = tx.Rollback(f.ctx) // before any Fatal: a leaked transaction blocks the pool's Close
		if setErr != nil {
			t.Fatal(setErr)
		}
		if err == nil || !strings.Contains(err.Error(), "legal_entity_id") && !strings.Contains(err.Error(), "pinned scope") {
			t.Fatalf("%s legal_entity_id change: %v, want the immutability trigger to refuse it", name, err)
		}
	}
}

// A11: every rule change is a new, immutable version; earlier versions are
// kept, each with its own digest, and a run pins the version it started under.
func TestRuleVersions_HistoryIsKept(t *testing.T) {
	f := newFixture(t)
	first := f.defineReconciliation(orgA, "rules", 0)
	run := f.startRun(orgA, first, cleanPops())

	second := f.publish(first, func(r *domain.RulesInput) {
		r.Populations[1].Params = map[string]string{"account_codes": "1100,1200"}
	})
	scale := 3
	third := f.publish(second, func(r *domain.RulesInput) { r.AmountScale = &scale })
	if second.Version != 2 || third.Version != 3 || third.AmountScale != 3 || second.Populations[1].Params["account_codes"] != "1100,1200" {
		t.Fatalf("versions: %d / %d %+v", second.Version, third.Version, third)
	}
	versions, err := f.s.ListRuleVersions(f.ctx, orgA, first.DefinitionID)
	if err != nil || len(versions) != 3 {
		t.Fatalf("history: %d versions (err=%v)", len(versions), err)
	}
	digests := map[string]bool{}
	for _, v := range versions {
		digests[v.RulesSHA256] = true
		if v.Status != "APPROVED" || v.ApprovedBy == nil || *v.ApprovedBy == v.CreatedBy {
			t.Fatalf("version %d approval: %+v", v.RuleVersion, v)
		}
	}
	if len(digests) != 3 || versions[0].Rules.AmountScale != 2 {
		t.Fatalf("earlier versions must be kept as they were: %+v", versions)
	}
	pinned, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	if pinned.DefinitionVersion != 1 || pinned.RulesSHA256 != versions[0].RulesSHA256 || pinned.AmountScale != 2 {
		t.Fatalf("the run must keep the version it started under: %+v", pinned)
	}
}

// A13: the replacement must be a terminal run of the same definition, it can
// replace only one run, and the supersession records who did it.
func TestSupersede_Constraints(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "supersede-rules", 0)
	other := f.defineReconciliation(orgA, "other-definition", 0)

	old := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(old.RunID)

	foreign := f.startRun(orgA, other, cleanPops())
	f.matchAndCertify(foreign.RunID)
	if _, err := f.s.SupersedeRun(f.ctx, orgA, old.RunID, domain.SupersedeRunRequest{NewRunID: foreign.RunID, Reason: "r"},
		"test-operator", f.claim("Supersede", "1")); !errors.Is(err, domain.ErrSupersedeDifferentDefinition) {
		t.Fatalf("replacement of another definition: %v", err)
	}

	pending := f.startRun(orgA, def, cleanPops())
	if _, err := f.s.SupersedeRun(f.ctx, orgA, old.RunID, domain.SupersedeRunRequest{NewRunID: pending.RunID, Reason: "r"},
		"test-operator", f.claim("Supersede", "2")); !errors.Is(err, domain.ErrReplacementNotTerminal) {
		t.Fatalf("non-terminal replacement: %v", err)
	}

	replacement := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(replacement.RunID)
	if _, err := f.s.SupersedeRun(f.ctx, orgA, old.RunID, domain.SupersedeRunRequest{NewRunID: replacement.RunID, Reason: "r"},
		"controller-carol", f.claim("Supersede", "3")); err != nil {
		t.Fatalf("valid supersession: %v", err)
	}

	another := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(another.RunID)
	if _, err := f.s.SupersedeRun(f.ctx, orgA, another.RunID, domain.SupersedeRunRequest{NewRunID: replacement.RunID, Reason: "r"},
		"test-operator", f.claim("Supersede", "4")); !errors.Is(err, domain.ErrReplacementAlreadyUsed) {
		t.Fatalf("a replacement reused for a second run: %v", err)
	}
}
