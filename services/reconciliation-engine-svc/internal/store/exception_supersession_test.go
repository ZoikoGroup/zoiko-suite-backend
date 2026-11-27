package store_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-05 "Re-test" / INV-23 (docs/architecture/DQC_GAP_MATRIX.md, step D).
//
// THE DEFECT. Reperform re-tests a run against freshly read populations as a new
// snapshot generation, but nothing could clear an exception raised under an
// EARLIER generation: CLOSED needs a manual match (an item that no longer exists
// cannot be matched, and a balance exception has no item), and REMEDIATED still
// counts as working. After the owning domain corrected its data and the run was
// reperformed over populations that now reconcile, the old exceptions stayed
// OPEN and Certify refused. These tests were first run as probes against the
// unfixed code and showed exactly that.

func (f *fixture) byStatus(runID string) (live, superseded []domain.ReconciliationException) {
	f.t.Helper()
	for _, e := range f.exceptions(runID) {
		switch e.Status {
		case domain.ExceptionSuperseded:
			superseded = append(superseded, e)
		case domain.ExceptionOpen, domain.ExceptionInvestigating, domain.ExceptionRemediated:
			live = append(live, e)
		}
	}
	return live, superseded
}

func dimPops(ledgerR2Entity string, ledgerR2Amount int64) []testPop {
	return []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "entity", "E1"), withDims(item("R2", 100), "entity", "E2")}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{
			withDims(item("R1", 100), "entity", "E1"), withDims(item("R2", ledgerR2Amount), "entity", ledgerR2Entity)}},
	}
}

func (f *fixture) dimDefinition(name string) *domain.ReconciliationDefinition {
	return f.defineWith(name, func(r *domain.DefineReconciliationRequest) { r.BalanceDimensions = []string{"entity"} })
}

// The headline case: the source is corrected, the run is reperformed over
// populations that now balance, and the run certifies.
func TestReperform_CorrectedBalance_SupersedesTheOldVariance_AndTheRunCertifies(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-balance")
	run := f.startRun(orgA, def, dimPops("E1", 100)) // R2 booked to the wrong entity
	f.match(run.RunID)
	if live, _ := f.byStatus(run.RunID); len(live) != 2 {
		t.Fatalf("setup: want 2 live balance exceptions, got %d", len(live))
	}

	got, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(dimPops("E2", 100)), "reperformer", f.claim("Reperform", run.RunID))
	if err != nil {
		t.Fatalf("reperform: %v", err)
	}
	if got.ResultState != domain.ResultMatch {
		t.Fatalf("result after a corrected reperform = %s, want MATCH (the old variances no longer exist)", got.ResultState)
	}

	live, superseded := f.byStatus(run.RunID)
	if len(live) != 0 || len(superseded) != 2 {
		t.Fatalf("after reperform: %d live, %d superseded; want 0 and 2", len(live), len(superseded))
	}
	for _, e := range superseded {
		if e.SupersededBy == nil || *e.SupersededBy != "reperformer" || e.SupersededByGeneration == nil || *e.SupersededByGeneration != 2 ||
			e.SupersededAt == nil || e.SupersededReason == nil || !strings.Contains(*e.SupersededReason, "generation 2") ||
			e.RaisedGeneration == nil || *e.RaisedGeneration != 1 {
			t.Fatalf("supersession not recorded: %+v", e)
		}
	}

	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify after a corrected reperform: %v", err)
	}
	if cert.ExceptionCount != 0 {
		t.Fatalf("certification counts %d exceptions, want 0: superseded findings are history, not what is certified", cert.ExceptionCount)
	}
	// Nothing was deleted: the superseded exceptions are still readable.
	if _, sup := f.byStatus(run.RunID); len(sup) != 2 {
		t.Fatalf("superseded exceptions vanished after certification: %d", len(sup))
	}
}

// Item exceptions had the same trap: an unmatched item that the source removed
// can never be matched, so its exception could never close.
func TestReperform_CorrectedItem_SupersedesTheOldException_AndTheRunCertifies(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "supersede-item", 0)
	bank := testPop{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("R1", 100)}}
	badLedger := testPop{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 100), item("GL-9", 30)}}
	run := f.startRun(orgA, def, []testPop{bank, badLedger})
	f.match(run.RunID)
	if live, _ := f.byStatus(run.RunID); len(live) == 0 {
		t.Fatal("setup: the stray ledger item raised nothing")
	}

	// The owning domain removes GL-9.
	goodLedger := testPop{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 100)}}
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll([]testPop{bank, goodLedger}), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	live, superseded := f.byStatus(run.RunID)
	if len(live) != 0 || len(superseded) == 0 {
		t.Fatalf("after reperform: %d live, %d superseded", len(live), len(superseded))
	}
	if _, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID)); err != nil {
		t.Fatalf("certify after a corrected reperform: %v", err)
	}
}

// A re-test that finds the SAME bucket with a DIFFERENT variance must record the
// new numbers. Before, ON CONFLICT DO NOTHING dropped it and the old amount stood
// as the run's current evidence.
func TestReperform_ChangedVariance_RecordsTheCurrentNumbers(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-changed")
	// Generation 1: ledger R2 is 100 under E1, so E1 is over by 100.
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	// Generation 2: still booked to E1, but the amount was corrected to 30.
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(dimPops("E1", 30)), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}

	abs := func(n int64) int64 {
		if n < 0 {
			return -n
		}
		return n
	}
	var liveE1, oldE1 *domain.ReconciliationException
	live, superseded := f.byStatus(run.RunID)
	for i := range live {
		if e := live[i]; e.Kind == domain.ExceptionKindBalance && e.DimensionValue != nil && *e.DimensionValue == "E1" {
			liveE1 = &live[i]
		}
	}
	for i := range superseded {
		if e := superseded[i]; e.Kind == domain.ExceptionKindBalance && e.DimensionValue != nil && *e.DimensionValue == "E1" {
			oldE1 = &superseded[i]
		}
	}
	if liveE1 == nil || oldE1 == nil {
		t.Fatalf("want a live and a superseded E1 balance exception; live=%+v superseded=%+v", live, superseded)
	}
	if abs(oldE1.AmountMinorUnits) != 100 {
		t.Fatalf("the superseded exception should keep generation 1's variance (100), has %d", oldE1.AmountMinorUnits)
	}
	if abs(liveE1.AmountMinorUnits) != 30 {
		t.Fatalf("the live exception shows %d, want generation 2's current variance (30): the re-test's numbers must be recorded", liveE1.AmountMinorUnits)
	}
	if liveE1.RaisedGeneration == nil || *liveE1.RaisedGeneration != 2 {
		t.Fatalf("live exception generation = %v, want 2", liveE1.RaisedGeneration)
	}
}

// Re-running matching within a generation is still idempotent: no duplicate
// balance exception for the same bucket and generation.
func TestMatch_Rerun_WithinAGeneration_DoesNotDuplicateBalanceExceptions(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-idempotent")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	before, _ := f.byStatus(run.RunID)
	f.match(run.RunID)
	after, _ := f.byStatus(run.RunID)
	if len(before) != len(after) {
		t.Fatalf("re-running Match changed the live exceptions from %d to %d", len(before), len(after))
	}
}

// Acceptance is final (INV-19) and a closure rests on a real manual match: a
// re-test does not touch either.
func TestReperform_LeavesAcceptedRiskAndClosedExceptionsAlone(t *testing.T) {
	f := newFixture(t)
	def := f.defineGoverned("supersede-accepted", 100000, true)
	bank := testPop{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("R1", 100), item("R2", 50)}}
	ledger := testPop{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("R1", 103), item("R2", 50)}}
	run := f.startRun(orgA, def, []testPop{bank, ledger})
	f.match(run.RunID)
	f.acceptAll(run.RunID)

	var accepted []string
	for _, e := range f.exceptions(run.RunID) {
		if e.Status == domain.ExceptionAcceptedRisk {
			accepted = append(accepted, e.ExceptionID)
		}
	}
	if len(accepted) == 0 {
		t.Fatal("setup: nothing was accepted as risk")
	}
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll([]testPop{bank, ledger}), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	for _, id := range accepted {
		e, err := f.s.GetException(f.ctx, orgA, id)
		if err != nil || e.Status != domain.ExceptionAcceptedRisk {
			t.Fatalf("accepted-risk exception %s is %v after a reperform (err %v), want ACCEPTED_RISK", id, e.Status, err)
		}
	}
}

// Only a re-test writes SUPERSEDED. The transition command cannot request it,
// or "supersede" would become a way to wave a finding away.
func TestTransitionException_CannotRequestSuperseded(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-no-request")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	live, _ := f.byStatus(run.RunID)
	if len(live) == 0 {
		t.Fatal("setup")
	}
	_, err := f.transition(live[0].ExceptionID, "olga", domain.TransitionExceptionRequest{
		ToState: domain.ExceptionSuperseded, Reason: "make it go away"})
	if err == nil {
		t.Fatal("the transition command accepted SUPERSEDED")
	}
	if !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidArgument or ErrInvalidTransition", err)
	}
	if e, _ := f.s.GetException(f.ctx, orgA, live[0].ExceptionID); e.Status != domain.ExceptionOpen {
		t.Fatalf("a refused request changed the exception to %s", e.Status)
	}
}

// At the database: SUPERSEDED must say who/when/which generation/why, and once
// reached it is as immutable as CLOSED.
func TestDatabase_SupersededIsCompleteAndFinal(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-db")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	live, _ := f.byStatus(run.RunID)
	if len(live) < 2 {
		t.Fatal("setup")
	}
	a, b := live[0].ExceptionID, live[1].ExceptionID

	// A bare status change is refused by the CHECK: nothing says who or why.
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_exceptions SET status = 'SUPERSEDED' WHERE exception_id = $1`, a); err == nil {
		t.Fatal("database accepted SUPERSEDED with no who/generation/why")
	}
	// The same move with the evidence is allowed: proof it was the CHECK, and that
	// the lifecycle permits OPEN -> SUPERSEDED.
	const full = `UPDATE reconciliation_exceptions SET status = 'SUPERSEDED', superseded_by = 'x', superseded_by_generation = 2,
		superseded_reason = 'replaced by a later run' WHERE exception_id = $1`
	if _, err := f.admin.Exec(f.ctx, full, a); err != nil {
		t.Fatalf("database refused a complete supersession: %v", err)
	}
	// ...and once superseded it cannot move again.
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_exceptions SET owner_principal = 'someone' WHERE exception_id = $1`, a); err == nil {
		t.Fatal("database allowed editing a superseded exception")
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_exceptions SET status = 'OPEN', superseded_by = NULL,
		superseded_by_generation = NULL, superseded_reason = NULL, superseded_at = NULL WHERE exception_id = $1`, a); err == nil {
		t.Fatal("database allowed un-superseding an exception")
	}
	// Supersession fields cannot be written on an exception that is not superseded.
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_exceptions SET superseded_by = 'x' WHERE exception_id = $1`, b); err == nil {
		t.Fatal("database allowed supersession fields on a non-superseded exception")
	}
	// The generation an exception was raised under is fixed.
	if _, err := f.admin.Exec(f.ctx, `UPDATE reconciliation_exceptions SET raised_generation = 9 WHERE exception_id = $1`, b); err == nil {
		t.Fatal("database allowed changing the generation an exception was raised under")
	}
}

// A superseded exception is not a working one: the run's assessment and its
// state ignore it, and the transition history records the move.
func TestReperform_RecordsTheSupersessionInTheExceptionsOwnHistory(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-history")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	live, _ := f.byStatus(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(dimPops("E2", 100)), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	var to, actor, reason string
	if err := f.admin.QueryRow(f.ctx, `SELECT to_state, actor, reason FROM reconciliation_exception_transitions
		WHERE exception_id = $1 ORDER BY transition_id DESC LIMIT 1`, live[0].ExceptionID).Scan(&to, &actor, &reason); err != nil {
		t.Fatalf("read transition: %v", err)
	}
	if to != "SUPERSEDED" || actor != "reperformer" || !strings.Contains(reason, "generation 2") {
		t.Fatalf("transition = %s by %s (%q), want SUPERSEDED by reperformer naming generation 2", to, actor, reason)
	}
}

// A closure rests on a manual match that really happened; a re-test does not
// reopen it or overwrite it.
func TestReperform_LeavesManuallyClosedExceptionsAlone(t *testing.T) {
	f := newFixture(t)
	run, bank, ledger := manualMatchRun(f)
	if _, err := f.s.Match(f.ctx, orgA, run.RunID, domain.MatchRequest{ManualMatches: []domain.ManualMatchInput{
		{ItemAID: bank, ItemBID: ledger, Reason: "same settlement; reference truncated"}}}, "reviewer", f.claim("Match", "manual")); err != nil {
		t.Fatalf("manual match: %v", err)
	}
	var closed []string
	for _, e := range f.exceptions(run.RunID) {
		if e.Status == domain.ExceptionClosed {
			closed = append(closed, e.ExceptionID)
		}
	}
	if len(closed) != 2 {
		t.Fatalf("setup: want 2 closed exceptions, got %d", len(closed))
	}
	// Reperform over the same shape of data.
	pops := []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100), item("BANK-X", 1003)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 100), item("GL-Y", 1000)}},
	}
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(pops), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	for _, id := range closed {
		e, err := f.s.GetException(f.ctx, orgA, id)
		if err != nil || e.Status != domain.ExceptionClosed || e.ResolvedByMatchID == nil {
			t.Fatalf("closed exception %s is %v after a reperform (err %v), want CLOSED by its manual match", id, e.Status, err)
		}
	}
}

// A superseded finding is history, not work: it does not nag (no UNASSIGNED /
// OVERDUE attention signal) and cannot be reassigned.
func TestSupersededException_IsNotWork(t *testing.T) {
	f := newFixture(t)
	def := f.dimDefinition("supersede-not-work")
	run := f.startRun(orgA, def, dimPops("E1", 100))
	f.match(run.RunID)
	live, _ := f.byStatus(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(dimPops("E2", 100)), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	e, err := f.s.GetException(f.ctx, orgA, live[0].ExceptionID)
	if err != nil || e.Status != domain.ExceptionSuperseded {
		t.Fatalf("setup: %v / %v", e, err)
	}
	if len(e.Attention) != 0 {
		t.Fatalf("a superseded exception carries attention signals %v; it is history, not work", e.Attention)
	}
	_, err = f.s.AssignException(f.ctx, orgA, e.ExceptionID, domain.AssignExceptionRequest{OwnerPrincipal: "olga", Reason: "take it"},
		"lead", f.claim("AssignException", e.ExceptionID))
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("reassigning a superseded exception: err = %v, want ErrInvalidTransition", err)
	}
}
