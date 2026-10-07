package store_test

import (
	"errors"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// DATA-07 Reconciliation Engine, against real Postgres as the
// NOSUPERUSER NOBYPASSRLS app role.

// Happy path: StartRun freezes two matching populations, Match pairs
// every item within tolerance, Certify seals immutable evidence.
func TestReconciliation_HappyPath_MatchAndCertify(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "ledger-vs-bank", 0)
	run := f.startRun(orgA, def.DefinitionID, []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 10000), item("REF-2", 5000)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 10000), item("REF-2", 5000)}},
	})
	if run.Status != domain.RunPlanned {
		t.Fatalf("new run status = %s, want Planned", run.Status)
	}

	matched, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match", run.RunID))
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if matched.Status != domain.RunRunning {
		t.Fatalf("post-match status = %s, want Running (clean)", matched.Status)
	}

	results, err := f.s.GetMatchResults(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get match results: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("match results: %+v", results)
	}

	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if cert.MatchedCount != 2 || cert.ExceptionCount != 0 {
		t.Fatalf("certification: %+v", cert)
	}

	got, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != domain.RunCertified {
		t.Fatalf("run status after certify = %s, want Certified", got.Status)
	}
}

// Doc-named acceptance test #1: equal totals with different
// composition still surface exceptions — matching by ref_id, not by
// aggregate sum, so two populations that sum to the same total but
// share no ref_ids produce zero automatic matches and one exception
// per item.
func TestReconciliation_EqualTotalsDifferentComposition_SurfacesExceptions(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "composition-check", 0)
	run := f.startRun(orgA, def.DefinitionID, []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("X", 10000), item("Y", 5000)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("Z", 9000), item("W", 6000)}},
	})

	matched, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match", run.RunID))
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if matched.Status != domain.RunExceptionsOpen {
		t.Fatalf("status = %s, want ExceptionsOpen despite equal totals (15000 vs 15000)", matched.Status)
	}

	results, err := f.s.GetMatchResults(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get match results: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected zero automatic matches, got: %+v", results)
	}

	exceptions, err := f.s.GetExceptions(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get exceptions: %v", err)
	}
	if len(exceptions) != 4 {
		t.Fatalf("expected 4 exceptions (one per item), got %d: %+v", len(exceptions), exceptions)
	}

	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrOpenExceptionsRemain) {
		t.Fatalf("certify with open exceptions: %v", err)
	}
}

// Doc-named acceptance test #2: tolerance cannot be widened during a
// certified run — enforced as a strict consequence of a Certified run
// being fully immutable except for its one escape hatch to Superseded.
func TestReconciliation_ToleranceCannotBeWidenedOnCertifiedRun(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "tolerance-lock", 0)
	run := f.startRun(orgA, def.DefinitionID, []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 10000)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 10000)}},
	})
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match", run.RunID)); err != nil {
		t.Fatalf("match: %v", err)
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); err != nil {
		t.Fatalf("certify: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin raw tx: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	_, err = tx.Exec(f.ctx, `UPDATE reconciliation_runs SET tolerance_amount_minor_units = 999999 WHERE run_id = $1`, run.RunID)
	if err == nil {
		t.Fatalf("widening tolerance on a certified run should have been rejected")
	}
}

// Doc-named acceptance test #3: manual match requires reason/authority
// and remains auditable.
func TestReconciliation_ManualMatchRequiresReasonAndIsAuditable(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "manual-match", 0)
	run := f.startRun(orgA, def.DefinitionID, []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("BANK-REF", 10000)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("GL-REF", 10000)}},
	})
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match-1", run.RunID)); err != nil {
		t.Fatalf("auto match pass: %v", err)
	}

	exceptions, err := f.s.GetExceptions(f.ctx, orgA, run.RunID)
	if err != nil || len(exceptions) != 2 {
		t.Fatalf("expected 2 exceptions before manual match, got %d (err=%v)", len(exceptions), err)
	}
	var itemA, itemB string
	for _, e := range exceptions {
		if e.Side == "Bank" {
			itemA = e.ItemID
		} else {
			itemB = e.ItemID
		}
	}

	// Missing reason is refused before it ever reaches the database.
	_, err = f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{
		ManualMatches: []domain.ManualMatchInput{{ItemAID: itemA, ItemBID: itemB, Reason: ""}},
	}, "reviewer-alice", f.claim("Match-no-reason", run.RunID))
	if err == nil {
		t.Fatalf("manual match with no reason should have been rejected")
	}

	got, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{
		ManualMatches: []domain.ManualMatchInput{{ItemAID: itemA, ItemBID: itemB, Reason: "confirmed same settlement via bank portal reference lookup"}},
	}, "reviewer-alice", f.claim("Match-2", run.RunID))
	if err != nil {
		t.Fatalf("manual match with reason: %v", err)
	}
	if got.Status != domain.RunRunning {
		t.Fatalf("status after manual match resolves last exception = %s, want Running", got.Status)
	}

	results, err := f.s.GetMatchResults(f.ctx, orgA, run.RunID)
	if err != nil || len(results) != 1 {
		t.Fatalf("match results after manual match: %d (err=%v)", len(results), err)
	}
	mr := results[0]
	if !mr.Manual || mr.Reason == "" || mr.MatchedBy != "reviewer-alice" {
		t.Fatalf("manual match is not auditable: %+v", mr)
	}

	afterExceptions, err := f.s.GetExceptions(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get exceptions after manual match: %v", err)
	}
	for _, e := range afterExceptions {
		if e.Status != domain.ExceptionResolved || e.ResolvedByMatchID == nil || *e.ResolvedByMatchID != mr.MatchResultID {
			t.Fatalf("exception not resolved by the manual match: %+v", e)
		}
	}
}

// Idempotent replay: retrying StartRun with the same claim key must not
// create a second run.
func TestReconciliation_StartRun_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "replay-check", 0)
	req := domain.StartRunRequest{DefinitionID: def.DefinitionID, Populations: []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
	}}
	claim := f.claim("StartRun", "replay-check")
	first, err := f.s.StartRun(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first start run: %v", err)
	}
	_, err = f.s.StartRun(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.RunID {
		t.Fatalf("replay of StartRun: %v", err)
	}
}

// SupersedeRun marks a certified run as superseded by a replacement,
// without touching its own sealed certification.
func TestReconciliation_SupersedeRun(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "supersede-check", 0)
	pops := []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
	}
	oldRun := f.startRun(orgA, def.DefinitionID, pops)
	if _, err := f.s.Match(f.ctx, orgA, oldRun.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match-old", oldRun.RunID)); err != nil {
		t.Fatalf("match old run: %v", err)
	}
	if _, err := f.s.Certify(f.ctx, orgA, oldRun.RunID, "test-operator", f.claim("Certify-old", oldRun.RunID)); err != nil {
		t.Fatalf("certify old run: %v", err)
	}

	newRun := f.startRun(orgA, def.DefinitionID, pops)

	superseded, err := f.s.SupersedeRun(f.ctx, orgA, oldRun.RunID, domain.SupersedeRunRequest{
		NewRunID: newRun.RunID, Reason: "corrected population re-submitted",
	}, "test-operator", f.claim("Supersede", oldRun.RunID))
	if err != nil {
		t.Fatalf("supersede run: %v", err)
	}
	if superseded.Status != domain.RunSuperseded || superseded.SupersededByRunID == nil || *superseded.SupersededByRunID != newRun.RunID {
		t.Fatalf("superseded run: %+v", superseded)
	}

	cert, err := f.s.GetCertification(f.ctx, orgA, oldRun.RunID)
	if err != nil {
		t.Fatalf("get certification after supersede: %v", err)
	}
	if cert.MatchedCount != 1 {
		t.Fatalf("certification evidence changed after supersede: %+v", cert)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS, not an
// application-level filter.
func TestReconciliation_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "isolation-check", 0)
	run := f.startRun(orgA, def.DefinitionID, []domain.PopulationInput{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
	})

	if _, err := f.s.GetRun(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("cross-tenant get run: %v", err)
	}
	snapshots, err := f.s.GetPopulationSnapshots(f.ctx, orgB, run.RunID)
	if err != nil {
		t.Fatalf("cross-tenant get snapshots: %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("cross-tenant read leaked population snapshots: %+v", snapshots)
	}
}
