package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Migration 000014 backfills raised_generation on BALANCE exceptions that exist
// when it runs: the generation that was current when each was raised. That
// statement touches production data once and cannot be re-run, so it is tested
// here the only way that is honest: the SQL is taken from the shipped migration
// file itself (not copied), and replayed against data with several generations.
func TestMigration000014_BackfillsTheGenerationEachBalanceExceptionWasRaisedUnder(t *testing.T) {
	f := newFixture(t)

	// Generation 1 raises two balance exceptions; a still-broken reperform
	// supersedes them and generation 2 raises its own.
	def := f.dimDefinition("migration-backfill")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(dimPops("E1", 30)), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	type row struct {
		id  string
		gen int
	}
	var want []row
	for _, e := range f.exceptions(run.RunID) {
		if e.Kind == "BALANCE" {
			if e.RaisedGeneration == nil {
				t.Fatalf("setup: a balance exception has no generation: %+v", e)
			}
			want = append(want, row{e.ExceptionID, *e.RaisedGeneration})
		}
	}
	seen := map[int]int{}
	for _, w := range want {
		seen[w.gen]++
	}
	if seen[1] < 2 || seen[2] < 1 {
		t.Fatalf("setup needs balance exceptions on both generations, got %v", seen)
	}

	// Put the table back into the shape it has when 000014 runs over legacy rows:
	// no generation on any balance exception.
	f.pauseTrigger("reconciliation_exceptions", "trigger_exception_resolution")
	for _, stmt := range []string{
		`ALTER TABLE reconciliation_exceptions DROP CONSTRAINT reconciliation_exceptions_balance_has_generation`,
		`UPDATE reconciliation_exceptions SET raised_generation = NULL WHERE kind = 'BALANCE'`,
	} {
		if _, err := f.admin.Exec(f.ctx, stmt); err != nil {
			t.Fatalf("recreate the legacy shape (%s): %v", stmt, err)
		}
	}

	// The backfill, exactly as shipped.
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations/000014_exception_supersession.up.sql"))
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	sql := string(raw)
	start := strings.Index(sql, "UPDATE reconciliation_exceptions e")
	if start < 0 {
		t.Fatal("the migration no longer contains the backfill statement this test replays")
	}
	end := strings.Index(sql[start:], ";")
	if _, err := f.admin.Exec(f.ctx, sql[start:start+end+1]); err != nil {
		t.Fatalf("run the shipped backfill: %v", err)
	}

	for _, w := range want {
		var got *int
		if err := f.admin.QueryRow(f.ctx, `SELECT raised_generation FROM reconciliation_exceptions WHERE exception_id = $1`, w.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != w.gen {
			t.Fatalf("exception %s backfilled to %v, was raised under generation %d", w.id, got, w.gen)
		}
	}
}
