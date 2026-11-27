package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// 000016's down refuses while evidence it would destroy exists (a declared subset, declared
// controls, a PARTIAL run), and otherwise round-trips: after the down the old rule (no run may be
// PARTIAL) is back, and after the up again PARTIAL works.
func TestMigration000016_DownRefusesWhileDeclaredEvidenceExists(t *testing.T) {
	f := newFixture(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000016_declared_controls_partial.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000016_declared_controls_partial.up.sql"))
	if err != nil {
		t.Fatal(err)
	}

	// Only sources that declared nothing: the outcomes are derivable, so down is allowed.
	def := f.defineReconciliation(orgA, "migration-16", 0)
	plain := f.startRun(orgA, def, cleanPops())
	f.match(plain.RunID)
	if _, err := f.admin.Exec(f.ctx, string(down)); err != nil {
		t.Fatalf("down with nothing declared: %v", err)
	}
	// The old rule is back: nothing may enter PARTIAL, whatever its populations say.
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_runs SET result_state = 'PARTIAL', certification_state = 'UNCERTIFIED' WHERE run_id = '`+plain.RunID+`'`); err == nil ||
		!strings.Contains(err.Error(), "no defined subset semantics") {
		t.Fatalf("after the down a run entered PARTIAL: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}

	// A declared subset exists: down refuses and changes nothing.
	subset := f.startWith(def, subsetPops())
	f.match(subset.RunID)
	if _, err := f.admin.Exec(f.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot revert 000016") {
		t.Fatalf("down with a declared subset: %v", err)
	}
	got, _ := f.s.GetRun(f.ctx, orgA, subset.RunID)
	if got.ResultState != domain.ResultPartial {
		t.Fatalf("a refused down changed the run: %s", got.ResultState)
	}
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM population_snapshots WHERE completeness = 'PARTIAL'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a refused down changed the snapshots: %d, %v", n, err)
	}

	// Declared controls on a FULL population also block it: that evidence would be lost.
	f2 := newFixture(t) // resets the schema
	def2 := f2.defineReconciliation(orgA, "migration-16b", 0)
	pops := frozenAll(cleanPops())
	pops[0].Controls = domain.DeclaredControls{Sequence: &domain.DeclaredSequence{Attribute: "seq", First: 1, Last: 1}}
	f2.startWith(def2, pops)
	if _, err := f2.admin.Exec(f2.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot revert 000016") {
		t.Fatalf("down with declared controls: %v", err)
	}
}
