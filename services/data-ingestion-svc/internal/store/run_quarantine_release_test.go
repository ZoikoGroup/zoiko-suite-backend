package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-ingestion-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-02 / INV-12 / NP-16 (docs/architecture/DQC_GAP_MATRIX.md, step H): governed release of RUN-LEVEL
// quarantine (INT-04). Real Postgres as the NOSUPERUSER NOBYPASSRLS role.

const (
	quarantiner = "quarantiner-qa"
	rrRequester = "release-requester"
	rrDecider   = "release-decider"
)

var (
	badRecords  = []domain.BatchRecord{rec("evt-2", "dedup-2"), rec("evt-3", "dedup-3")}
	goodRecords = []domain.BatchRecord{rec("evt-2b", "dedup-2b"), rec("evt-3b", "dedup-3b")}
)

func (f *fixture) commitAs(org, runID, actor, schema string, records []domain.BatchRecord, releases string) (*domain.LandingObject, error) {
	return f.s.CommitBatch(f.ctx, org, domain.CommitBatchRequest{
		RunID: runID, Records: records, SchemaVersion: schema, Classification: "internal", ResidencyRegion: "us-east",
		NextPosition: "offset-" + records[0].DedupKey, ReleasesQuarantineID: releases,
	}, actor, f.claim("CommitBatch", runID+records[0].DedupKey+releases))
}

// quarantinedBatch starts a run, establishes schema v1, then has a v2 batch refused whole by `quarantiner`.
func (f *fixture) quarantinedBatch(source string) (run *domain.IngestionRun, item domain.QuarantineItem) {
	f.t.Helper()
	run = f.startRun(orgA, source)
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", []domain.BatchRecord{rec("evt-1-"+source, "dedup-1-"+source)}, ""); err != nil {
		f.t.Fatalf("first commit: %v", err)
	}
	if _, err := f.commitAs(orgA, run.RunID, quarantiner, "v2", badRecords, ""); !errors.Is(err, domain.ErrSchemaIncompatible) {
		f.t.Fatalf("setup: the v2 batch was not quarantined: %v", err)
	}
	items, err := f.s.ListQuarantineItems(f.ctx, orgA, run.RunID)
	if err != nil || len(items) != 1 {
		f.t.Fatalf("setup: items %+v %v", items, err)
	}
	return run, items[0]
}

func (f *fixture) requestRelease(run *domain.IngestionRun, qid, by string) error {
	return f.s.RequestRunRelease(f.ctx, orgA, run.RunID, qid, by, domain.RunReleaseRequest{Reason: "the producer fixed its schema", CorrectionRef: "producer-ticket-4411"},
		f.claim("RequestRunRelease", qid+by))
}

func (f *fixture) decideRelease(run *domain.IngestionRun, qid, by string, release bool) error {
	return f.s.DecideRunRelease(f.ctx, orgA, run.RunID, qid, by, release, domain.RunReleaseDecision{Reason: "verified with the producer"},
		f.claim("DecideRunRelease", qid+by))
}

func (f *fixture) state(run *domain.IngestionRun, qid string) domain.RunReleaseState {
	f.t.Helper()
	_, st, err := f.s.RunQuarantineLedger(f.ctx, orgA, run.RunID, qid)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func (f *fixture) landed(dedup string) int {
	return f.countAsTenant(orgA, `SELECT count(*) FROM landed_records WHERE dedup_key = $1`, dedup)
}

func TestRunRelease_TheQuarantinedPayloadCannotBeCommittedAgain(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.bypass")
	if item.ReleaseState != domain.RunQuarantined {
		t.Fatalf("a new item is %q", item.ReleaseState)
	}
	// The schema is now right, but this is the SAME data that was quarantined: re-sending it is the bypass.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", badRecords, ""); !errors.Is(err, domain.ErrRunReleaseRequired) {
		t.Fatalf("the quarantined payload was committed again with no release: %v", err)
	}
	if f.landed("dedup-2") != 0 || f.landed("dedup-3") != 0 {
		t.Fatal("the refused payload landed")
	}
	// Different data is an ordinary batch, and lands. It does not release anything.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, ""); err != nil {
		t.Fatalf("an unrelated batch was refused: %v", err)
	}
	if got := f.state(run, item.QuarantineID); got != domain.RunQuarantined {
		t.Fatalf("an unrelated landing released the item: %s", got)
	}
}

func TestRunRelease_GovernedFlowLandsTheCorrectedBatchAndConsumesTheApproval(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.flow")
	qid := item.QuarantineID

	if err := f.requestRelease(run, qid, rrRequester); err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := f.state(run, qid); got != domain.RunReleasePending {
		t.Fatalf("after a request: %s", got)
	}
	// Nothing lands under a request that has not been decided.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, qid); !errors.Is(err, domain.ErrRunReleaseNotApproved) {
		t.Fatalf("a batch landed under an undecided request: %v", err)
	}
	if err := f.decideRelease(run, qid, rrDecider, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := f.state(run, qid); got != domain.RunReleaseApprovedS {
		t.Fatalf("after approval: %s", got)
	}
	landing, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, qid)
	if err != nil {
		t.Fatalf("the corrected batch under the approval: %v", err)
	}
	if f.landed("dedup-2b") != 1 {
		t.Fatalf("the corrected batch did not land")
	}
	ledger, st, err := f.s.RunQuarantineLedger(f.ctx, orgA, run.RunID, qid)
	if err != nil || st != domain.RunReleased || len(ledger) != 3 {
		t.Fatalf("ledger %+v state %s %v", ledger, st, err)
	}
	if ledger[0].Kind != domain.RunReleaseRequested || ledger[0].CorrectionRef != "producer-ticket-4411" || ledger[0].Actor != rrRequester ||
		ledger[1].Kind != domain.RunReleaseApproved || ledger[1].Actor != rrDecider ||
		ledger[2].Kind != domain.RunReleaseConsumed || ledger[2].LandingID != landing.LandingID {
		t.Fatalf("ledger content: %+v", ledger)
	}
	items, _ := f.s.ListQuarantineItems(f.ctx, orgA, run.RunID)
	if items[0].ReleaseState != domain.RunReleased {
		t.Fatalf("the item lists as %s", items[0].ReleaseState)
	}
	// The approval is used once. A second batch under it is refused, and so is a second request.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", []domain.BatchRecord{rec("evt-9", "dedup-9")}, qid); !errors.Is(err, domain.ErrRunReleaseNotApproved) {
		t.Fatalf("a second batch under a consumed approval: %v", err)
	}
	if err := f.requestRelease(run, qid, rrRequester); !errors.Is(err, domain.ErrRunReleaseAlreadyReleased) {
		t.Fatalf("a request for a released item: %v", err)
	}
}

// What is re-submitted must be a correction.
func TestRunRelease_TheSameDataIsNotACorrection_AndTheApprovalSurvivesTheRefusal(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.same")
	qid := item.QuarantineID
	if err := f.requestRelease(run, qid, rrRequester); err != nil {
		t.Fatal(err)
	}
	if err := f.decideRelease(run, qid, rrDecider, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", badRecords, qid); !errors.Is(err, domain.ErrRunReleaseNotACorrection) {
		t.Fatalf("the identical payload under an approval: %v", err)
	}
	if f.landed("dedup-2") != 0 {
		t.Fatal("the identical payload landed")
	}
	if got := f.state(run, qid); got != domain.RunReleaseApprovedS {
		t.Fatalf("a refused re-submission used up the approval: %s", got)
	}
	// A real correction still works afterwards: it was the sameness that was refused.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, qid); err != nil {
		t.Fatalf("the corrected batch: %v", err)
	}
}

func TestRunRelease_ABatchThatIsItselfQuarantinedDoesNotConsumeTheApproval(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.again")
	qid := item.QuarantineID
	_ = f.requestRelease(run, qid, rrRequester)
	_ = f.decideRelease(run, qid, rrDecider, true)
	// The "correction" still has the wrong schema, so it is quarantined too: nothing landed, nothing consumed.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v2", goodRecords, qid); !errors.Is(err, domain.ErrSchemaIncompatible) {
		t.Fatalf("a still-incompatible correction: %v", err)
	}
	if got := f.state(run, qid); got != domain.RunReleaseApprovedS {
		t.Fatalf("a quarantined replacement consumed the approval: %s", got)
	}
}

func TestRunRelease_IndependenceAndDecisions(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.indep")
	qid := item.QuarantineID

	if err := f.decideRelease(run, qid, rrDecider, true); !errors.Is(err, domain.ErrRunNoOpenRelease) {
		t.Fatalf("a decision with no request: %v", err)
	}
	if err := f.requestRelease(run, qid, rrRequester); err != nil {
		t.Fatal(err)
	}
	if err := f.requestRelease(run, qid, "someone-else"); !errors.Is(err, domain.ErrRunReleaseOpen) {
		t.Fatalf("a second request while one is open: %v", err)
	}
	if err := f.decideRelease(run, qid, rrRequester, true); !errors.Is(err, domain.ErrRunReleaseNotIndependent) {
		t.Fatalf("the requester approved their own request: %v", err)
	}
	if err := f.decideRelease(run, qid, rrRequester, false); !errors.Is(err, domain.ErrRunReleaseNotIndependent) {
		t.Fatalf("the requester refused their own request: %v", err)
	}
	// Whoever quarantined the batch cannot APPROVE its release (NP-16)...
	claim := f.claim("DecideRunRelease", "quarantiner")
	if err := f.s.DecideRunRelease(f.ctx, orgA, run.RunID, qid, quarantiner, true, domain.RunReleaseDecision{Reason: "x"}, claim); !errors.Is(err, domain.ErrRunReleaseNotIndependent) {
		t.Fatalf("the quarantiner approved: %v", err)
	}
	if n := f.countAsTenant(orgA, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key = $1`, claim.Key); n != 0 {
		t.Fatalf("a refused decision consumed its key: %d", n)
	}
	// ...but may refuse it.
	if err := f.decideRelease(run, qid, quarantiner, false); err != nil {
		t.Fatalf("the quarantiner refusing: %v", err)
	}
	if got := f.state(run, qid); got != domain.RunQuarantined {
		t.Fatalf("after a refusal: %s", got)
	}
	// A refusal closes the request, so a better-evidenced one can be made, and an independent person can approve it.
	if err := f.requestRelease(run, qid, rrRequester); err != nil {
		t.Fatalf("a new request after a refusal: %v", err)
	}
	if err := f.requestRelease(run, qid, rrRequester); !errors.Is(err, domain.ErrRunReleaseOpen) {
		t.Fatal(err)
	}
	if err := f.decideRelease(run, qid, rrDecider, true); err != nil {
		t.Fatalf("an independent approval: %v", err)
	}
	// An unused approval also blocks a new request.
	if err := f.requestRelease(run, qid, "someone-else"); !errors.Is(err, domain.ErrRunReleaseOpen) {
		t.Fatalf("a request over an unused approval: %v", err)
	}
}

func TestRunRelease_ValidationScopeAndTenancy(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.scope")
	qid := item.QuarantineID
	bad := []domain.RunReleaseRequest{{Reason: "x"}, {CorrectionRef: "y"}, {Reason: "  ", CorrectionRef: "y"}}
	for _, r := range bad {
		if err := f.s.RequestRunRelease(f.ctx, orgA, run.RunID, qid, rrRequester, r, f.claim("RequestRunRelease", "v")); !errors.Is(err, domain.ErrRunReleaseInvalid) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	if err := f.s.DecideRunRelease(f.ctx, orgA, run.RunID, qid, rrDecider, true, domain.RunReleaseDecision{Reason: " "}, f.claim("DecideRunRelease", "v")); !errors.Is(err, domain.ErrRunReleaseInvalid) {
		t.Fatalf("a decision with no reason: %v", err)
	}
	if err := f.requestRelease(run, "dqi_00000000-0000-0000-0000-000000000000", rrRequester); !errors.Is(err, domain.ErrQuarantineItemNotFound) {
		t.Fatalf("an unknown item: %v", err)
	}
	// An item is reached through its own run.
	other := f.startRun(orgA, "rr.scope.other")
	if err := f.requestRelease(other, qid, rrRequester); !errors.Is(err, domain.ErrQuarantineItemNotFound) {
		t.Fatalf("an item reached through another run: %v", err)
	}
	// Another tenant sees neither the item nor its ledger.
	if err := f.s.RequestRunRelease(f.ctx, orgB, run.RunID, qid, rrRequester, domain.RunReleaseRequest{Reason: "x", CorrectionRef: "y"}, f.claim("RequestRunRelease", "b")); !errors.Is(err, domain.ErrQuarantineItemNotFound) {
		t.Fatalf("tenant B requested a release of tenant A's item: %v", err)
	}
	if _, _, err := f.s.RunQuarantineLedger(f.ctx, orgB, run.RunID, qid); !errors.Is(err, domain.ErrQuarantineItemNotFound) {
		t.Fatalf("tenant B read tenant A's ledger: %v", err)
	}
	// A release id that names nothing, or another source's item, lands nothing.
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, "dqi_00000000-0000-0000-0000-000000000000"); !errors.Is(err, domain.ErrQuarantineItemNotFound) {
		t.Fatalf("an unknown release id: %v", err)
	}
	run2, _ := f.quarantinedBatch("rr.scope.src2")
	_ = run2
	// (rr.scope.src2's item belongs to a different source than rr.scope's run.)
	items2, _ := f.s.ListQuarantineItems(f.ctx, orgA, run2.RunID)
	_ = f.requestRelease(run2, items2[0].QuarantineID, rrRequester)
	_ = f.decideRelease(run2, items2[0].QuarantineID, rrDecider, true)
	if _, err := f.commitAs(orgA, run.RunID, "test-operator", "v1", goodRecords, items2[0].QuarantineID); !errors.Is(err, domain.ErrRunReleaseNotApproved) {
		t.Fatalf("an approval of another source's item: %v", err)
	}
}

// A replay of the landing that consumed a release is a replay, not a refusal that the release is used up.
func TestRunRelease_ReplayOfTheConsumingCommitIsAReplay(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.replay")
	qid := item.QuarantineID
	_ = f.requestRelease(run, qid, rrRequester)
	_ = f.decideRelease(run, qid, rrDecider, true)
	req := domain.CommitBatchRequest{RunID: run.RunID, Records: goodRecords, SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east",
		NextPosition: "offset-r", ReleasesQuarantineID: qid}
	claim := f.claimWithKey("CommitBatch", run.RunID+"-release", "replay-key-1")
	if _, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", claim); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("the replay: %v", err)
	}
}

// The same rules held by the database, for a statement that skips the service.
func TestRunRelease_DatabaseHoldsTheRules(t *testing.T) {
	f := newFixture(t)
	run, item := f.quarantinedBatch("rr.db")
	qid := item.QuarantineID
	_ = run
	insert := func(kind, actor string, correction, answers, landing any) error {
		_, err := f.admin.Exec(f.ctx, `INSERT INTO quarantine_item_release_ledger (tenant_id, quarantine_id, kind, actor, reason, correction_ref, answers_ledger_id, landing_id)
			VALUES ($1, $2, $3, $4, 'r', $5, $6::uuid, $7)`, orgA, qid, kind, actor, correction, answers, landing)
		return err
	}
	// A request needs the correction named.
	if insert("REQUESTED", rrRequester, nil, nil, nil) == nil {
		t.Fatal("a request naming no correction was accepted")
	}
	if err := insert("REQUESTED", rrRequester, "ticket-1", nil, nil); err != nil {
		t.Fatalf("a valid request was refused: %v", err)
	}
	// One open request at a time.
	if insert("REQUESTED", "someone", "ticket-2", nil, nil) == nil {
		t.Fatal("a second open request was accepted")
	}
	var reqID string
	if err := f.admin.QueryRow(f.ctx, `SELECT ledger_id::text FROM quarantine_item_release_ledger WHERE quarantine_id = $1 AND kind = 'REQUESTED'`, qid).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	// Decisions: never the requester; an approval never the quarantiner; only a request is decided.
	if insert("APPROVED", rrRequester, nil, reqID, nil) == nil {
		t.Fatal("the requester approved their own request")
	}
	if insert("APPROVED", quarantiner, nil, reqID, nil) == nil {
		t.Fatal("the quarantiner approved")
	}
	if insert("CONSUMED", rrDecider, nil, reqID, "lo_x") == nil {
		t.Fatal("a request was consumed")
	}
	if err := insert("APPROVED", rrDecider, nil, reqID, nil); err != nil {
		t.Fatalf("an independent approval was refused: %v", err)
	}
	// An entry is answered once.
	if insert("REFUSED", "another", nil, reqID, nil) == nil {
		t.Fatal("a request was answered twice")
	}
	var apprID string
	if err := f.admin.QueryRow(f.ctx, `SELECT ledger_id::text FROM quarantine_item_release_ledger WHERE quarantine_id = $1 AND kind = 'APPROVED'`, qid).Scan(&apprID); err != nil {
		t.Fatal(err)
	}
	// Append-only.
	for name, stmt := range map[string]string{
		"change an entry": `UPDATE quarantine_item_release_ledger SET reason = 'x' WHERE quarantine_id = $1`,
		"delete an entry": `DELETE FROM quarantine_item_release_ledger WHERE quarantine_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, stmt, qid); err == nil {
			t.Fatalf("the database allowed: %s", name)
		}
	}
	// A consumption names the landing; and once consumed, the item cannot be requested again.
	if insert("CONSUMED", rrDecider, nil, apprID, nil) == nil {
		t.Fatal("a consumption naming no landing was accepted")
	}
	if err := insert("CONSUMED", rrDecider, nil, apprID, "lo_x"); err != nil {
		t.Fatalf("a valid consumption was refused: %v", err)
	}
	if insert("REQUESTED", "someone", "ticket-3", nil, nil) == nil {
		t.Fatal("a request for a released item was accepted")
	}
}
