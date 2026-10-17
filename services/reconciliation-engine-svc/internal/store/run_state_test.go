package store_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

func (f *fixture) failedRun(org string, def *domain.ReconciliationDefinition, claim domain.IdempotencyClaim) *domain.ReconciliationRun {
	f.t.Helper()
	r, err := f.s.RecordFailedRun(f.ctx, org, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
		domain.RunFailure{Code: domain.FailureSourceUnavailable, Reason: `population "Bank": source population unavailable`},
		"test-operator", claim)
	if err != nil {
		f.t.Fatalf("record failed run: %v", err)
	}
	return r
}

func (f *fixture) transitions(runID string) []string {
	f.t.Helper()
	ts, err := f.s.GetTransitions(f.ctx, orgA, runID)
	if err != nil {
		f.t.Fatalf("get transitions: %v", err)
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Dimension + ":" + t.FromState + ">" + t.ToState
	}
	return out
}

func wantStates(t *testing.T, r *domain.ReconciliationRun, status domain.RunStatus, result domain.ResultState, cert domain.CertificationState) {
	t.Helper()
	if r.Status != status || r.ResultState != result || r.CertificationState != cert {
		t.Fatalf("run %s is %s / %s / %s, want %s / %s / %s", r.RunID, r.Status, r.ResultState, r.CertificationState,
			status, result, cert)
	}
}

func wantHistory(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("transitions:\n got  %v\n want %v", got, want)
	}
}

// A6: the three dimensions move independently, every move is recorded, and
// supersession leaves the earlier result in the history.
func TestRunState_CleanRunToSupersession(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-clean", 0)
	run := f.startRun(orgA, def, cleanPops())
	wantStates(t, run, domain.RunPlanned, domain.ResultNotRun, domain.CertUncertified)

	matched := f.match(run.RunID)
	wantStates(t, matched, domain.RunRunning, domain.ResultMatch, domain.CertEligible)

	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", f.claim("Certify", run.RunID)); err != nil {
		t.Fatalf("certify: %v", err)
	}
	certified, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, certified, domain.RunCertified, domain.ResultMatch, domain.CertCertified)

	replacement := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(replacement.RunID)
	superseded, err := f.s.SupersedeRun(f.ctx, orgA, run.RunID, domain.SupersedeRunRequest{NewRunID: replacement.RunID, Reason: "re-run"},
		"test-operator", f.claim("SupersedeRun", run.RunID))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	wantStates(t, superseded, domain.RunSuperseded, domain.ResultSuperseded, domain.CertSuperseded)

	wantHistory(t, f.transitions(run.RunID),
		"LIFECYCLE:Planned>Running",
		"RESULT:NOT_RUN>MATCH", "CERTIFICATION:UNCERTIFIED>ELIGIBLE",
		"LIFECYCLE:Running>Certified", "CERTIFICATION:ELIGIBLE>CERTIFIED",
		"LIFECYCLE:Certified>Superseded", "RESULT:MATCH>SUPERSEDED", "CERTIFICATION:CERTIFIED>SUPERSEDED")
}

// A VARIANCE is not certifiable; resolving it re-evaluates to MATCH.
func TestRunState_VarianceThenResolved(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-variance", 0)
	run := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-X", 100)}},
	})
	matched := f.match(run.RunID)
	wantStates(t, matched, domain.RunExceptionsOpen, domain.ResultVariance, domain.CertUncertified)
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrOpenExceptionsRemain) {
		t.Fatalf("certify a VARIANCE: %v", err)
	}

	var bank, ledger string
	for _, e := range f.exceptions(run.RunID) {
		if e.Side == "Bank" {
			bank = e.ItemID
		} else if e.Side == "Ledger" {
			ledger = e.ItemID
		}
	}
	resolved, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: bank, ItemBID: ledger, Reason: "same payment, reference typo in the ledger"}}}, "test-operator", f.claim("Match", run.RunID))
	if err != nil {
		t.Fatalf("manual match: %v", err)
	}
	wantStates(t, resolved, domain.RunRunning, domain.ResultMatch, domain.CertEligible)
	wantHistory(t, f.transitions(run.RunID),
		"LIFECYCLE:Planned>Running",
		"LIFECYCLE:Running>ExceptionsOpen", "RESULT:NOT_RUN>VARIANCE",
		"LIFECYCLE:ExceptionsOpen>Running", "RESULT:VARIANCE>MATCH", "CERTIFICATION:UNCERTIFIED>ELIGIBLE")
}

// A6 / scenario 09: a run whose populations could not be proven complete is
// recorded Failed / INDETERMINATE, is closed to every command, replays under
// its key, and can be superseded by a later successful run.
func TestRunState_FailedRunIsIndeterminateAndTerminal(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-failed", 0)
	claim := f.claim("StartRun", def.DefinitionID)
	run := f.failedRun(orgA, def, claim)
	wantStates(t, run, domain.RunFailed, domain.ResultIndeterminate, domain.CertUncertified)
	if run.FailureCode == nil || *run.FailureCode != domain.FailureSourceUnavailable || run.FailureReason == nil {
		t.Fatalf("failure details: %v / %v", run.FailureCode, run.FailureReason)
	}
	if run.DefinitionVersion != def.Version || run.Tolerance == nil {
		t.Fatalf("a failed run is still pinned to what it attempted: %+v", run)
	}
	if pops, _ := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID); len(pops) != 0 {
		t.Fatalf("a failed run froze populations: %+v", pops)
	}
	wantHistory(t, f.transitions(run.RunID), "LIFECYCLE:Planned>Failed", "RESULT:NOT_RUN>INDETERMINATE")

	// The same key replays the failed run; nothing new is created.
	var replay *domain.IdempotentReplayError
	if _, err := f.s.RecordFailedRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
		domain.RunFailure{Code: domain.FailureSourceUnavailable, Reason: "again"}, "test-operator", claim); !errors.As(err, &replay) || replay.ResourceID != run.RunID {
		t.Fatalf("replay of a failed start: %v", err)
	}

	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{}, "test-operator", f.claim("Match", run.RunID)); !errors.Is(err, domain.ErrRunNotOpen) {
		t.Fatalf("match a failed run: %v", err)
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrRunNotOpen) {
		t.Fatalf("certify a failed run: %v", err)
	}

	replacement := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(replacement.RunID)
	superseded, err := f.s.SupersedeRun(f.ctx, orgA, run.RunID, domain.SupersedeRunRequest{NewRunID: replacement.RunID, Reason: "source back"},
		"test-operator", f.claim("SupersedeRun", run.RunID))
	if err != nil {
		t.Fatalf("supersede a failed run: %v", err)
	}
	// Never certified, so its certification stays UNCERTIFIED; the failure stays on record.
	wantStates(t, superseded, domain.RunSuperseded, domain.ResultSuperseded, domain.CertUncertified)
	if superseded.FailureCode == nil {
		t.Fatalf("supersession erased the failure code")
	}
}

// The RaiseException-on-Planned defect: no result exists before the run is
// matched, so there is nothing for an exception to turn into a VARIANCE.
func TestRunState_RaiseExceptionBeforeMatchIsRefused(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-planned", 0)
	run := f.startRun(orgA, def, cleanPops())
	items, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil || len(items) != 2 {
		t.Fatalf("snapshots: %v", err)
	}
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var itemID string
	_, _ = tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
	err = tx.QueryRow(f.ctx, `SELECT item_id FROM population_items WHERE snapshot_id = $1`, items[0].SnapshotID).Scan(&itemID)
	_ = tx.Rollback(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.RaiseException(f.ctx, orgA, run.RunID, domain.RaiseExceptionRequest{ItemID: itemID, Reason: "looks wrong", Category: "MISMATCH", Assertion: "ACCURACY"},
		"test-operator", f.claim("RaiseException", run.RunID))
	if !errors.Is(err, domain.ErrRunNotEvaluated) {
		t.Fatalf("raise exception on a Planned run: %v, want ErrRunNotEvaluated", err)
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "test-operator", f.claim("Certify", run.RunID)); !errors.Is(err, domain.ErrRunNotEvaluated) {
		t.Fatalf("certify a Planned run: %v, want ErrRunNotEvaluated", err)
	}
	got, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunPlanned, domain.ResultNotRun, domain.CertUncertified)

	// After matching, raising an exception turns the MATCH into a VARIANCE and
	// withdraws eligibility.
	f.match(run.RunID)
	if _, err := f.s.RaiseException(f.ctx, orgA, run.RunID, domain.RaiseExceptionRequest{ItemID: itemID, Reason: "looks wrong", Category: "MISMATCH", Assertion: "ACCURACY"},
		"test-operator", f.claim("RaiseException", run.RunID)); err != nil {
		t.Fatalf("raise exception after match: %v", err)
	}
	got, _ = f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunExceptionsOpen, domain.ResultVariance, domain.CertUncertified)
}

// The database enforces the cross-dimension invariants itself, so no direct
// write can certify a VARIANCE, coerce INDETERMINATE, enter PARTIAL, or
// rewrite history.
func TestRunState_DatabaseRefusesIllegalStates(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-db", 0)
	failed := f.failedRun(orgA, def, f.claim("StartRun", def.DefinitionID))
	open := f.startRun(orgA, def, []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL"},
	})
	f.match(open.RunID) // ExceptionsOpen / VARIANCE
	clean := f.startRun(orgA, def, cleanPops())
	f.match(clean.RunID) // Running / MATCH / ELIGIBLE

	for name, tc := range map[string]struct{ stmt, want string }{
		"INDETERMINATE coerced to MATCH": {`UPDATE reconciliation_runs SET result_state = 'MATCH' WHERE run_id = '` + failed.RunID + `'`, "INDETERMINATE"},
		"VARIANCE certified": {`UPDATE reconciliation_runs SET status = 'Running', result_state = 'VARIANCE',
			certification_state = 'ELIGIBLE' WHERE run_id = '` + open.RunID + `'`, "state_dimensions"},
		"certified without the command": {`UPDATE reconciliation_runs SET certification_state = 'CERTIFIED' WHERE run_id = '` + clean.RunID + `'`, "state_dimensions"},
		"PARTIAL entered":               {`UPDATE reconciliation_runs SET result_state = 'PARTIAL', certification_state = 'UNCERTIFIED' WHERE run_id = '` + clean.RunID + `'`, "PARTIAL"},
		"result reset":                  {`UPDATE reconciliation_runs SET result_state = 'NOT_RUN', certification_state = 'UNCERTIFIED' WHERE run_id = '` + clean.RunID + `'`, "NOT_RUN"},
		"failure rewritten":             {`UPDATE reconciliation_runs SET failure_reason = 'nothing happened' WHERE run_id = '` + failed.RunID + `'`, "failure details"},
		"run created certified": {`INSERT INTO reconciliation_runs (run_id, tenant_id, legal_entity_id, definition_id, definition_version, status,
			result_state, certification_state, amount_scale, match_key_fields, balance_dimensions, tolerance_policy, materiality_amount_minor_units, started_by)
			VALUES ('rcr_00000000-0000-0000-0000-000000000001', '` + orgA + `', '` + entityA + `', '` + def.DefinitionID + `', 1, 'Certified',
			'MATCH', 'CERTIFIED', 2, '{ref_id}', '{}', '{"currencies":{}}', 0, 'x')`, "created Planned"},
		"history rewritten": {`UPDATE reconciliation_run_transitions SET to_state = 'MATCH' WHERE run_id = '` + failed.RunID + `'`, "immutable"},
		"history deleted":   {`DELETE FROM reconciliation_run_transitions WHERE run_id = '` + failed.RunID + `'`, "immutable"},
	} {
		tx, err := f.app.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
		_, err = tx.Exec(f.ctx, tc.stmt)
		_ = tx.Rollback(f.ctx) // before any Fatal: a leaked transaction blocks the pool's Close
		if setErr != nil {
			t.Fatal(setErr)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}
}

// Transition history is tenant-isolated like everything else.
func TestRunState_TransitionsAreTenantIsolated(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "state-rls", 0)
	run := f.failedRun(orgA, def, f.claim("StartRun", def.DefinitionID))
	got, err := f.s.GetTransitions(f.ctx, orgB, run.RunID)
	if err != nil || len(got) != 0 {
		t.Fatalf("tenant B read tenant A's history: %v (err=%v)", got, err)
	}
}
