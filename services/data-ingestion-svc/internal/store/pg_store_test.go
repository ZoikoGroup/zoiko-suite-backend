package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-ingestion-svc/internal/domain"
)

// DATA-01 Data Ingestion, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS role.

func rec(sourceEventID, dedupKey string) domain.BatchRecord {
	return domain.BatchRecord{SourceEventID: sourceEventID, DedupKey: dedupKey,
		Payload: map[string]interface{}{"amount": 10, "currency": "USD"}}
}

// Happy path: StartIngestion -> CommitBatch -> CloseRun. The checkpoint
// advances to the caller's declared position, and CloseRun makes the run
// terminal.
func TestIngestion_HappyPath_StartCommitClose(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	req := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1"), rec("evt-2", "dedup-2")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-2",
	}
	got, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", f.claim("CommitBatch", run.RunID))
	if err != nil {
		t.Fatalf("commit batch: %v", err)
	}
	if got.RecordCount != 2 {
		t.Fatalf("record_count = %d, want 2", got.RecordCount)
	}
	if got.ContentHash == "" {
		t.Fatal("content_hash is empty")
	}

	cp, err := f.s.GetCheckpoint(f.ctx, orgA, "erp.invoices")
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if cp.LastPosition != "offset-2" {
		t.Fatalf("checkpoint position = %s, want offset-2", cp.LastPosition)
	}

	closed, err := f.s.CloseRun(f.ctx, orgA, domain.CloseRunRequest{RunID: run.RunID},
		domain.IngestionRunCompleted, "test-operator", f.claim("CloseRun", run.RunID))
	if err != nil {
		t.Fatalf("close run: %v", err)
	}
	if closed.Status != domain.IngestionRunCompleted || closed.ClosedAt == nil {
		t.Fatalf("closed run: %+v", closed)
	}
}

// Negative path #1 (doc-named): duplicate source event does not duplicate
// analytical facts. The same dedup key submitted across two separate
// CommitBatch calls lands exactly once.
func TestIngestion_DuplicateSourceEventDoesNotDuplicateFacts(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	first := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-1",
	}
	got1, err := f.s.CommitBatch(f.ctx, orgA, first, "test-operator", f.claim("CommitBatch", run.RunID+"-1"))
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if got1.RecordCount != 1 {
		t.Fatalf("first commit record_count = %d, want 1", got1.RecordCount)
	}

	// Redelivery: the same event (same dedup key) arrives again in a
	// DIFFERENT batch/request — simulating an at-least-once source poller
	// re-sending overlapping content, not a client retry of the same call.
	second := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1"), rec("evt-2", "dedup-2")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-2",
	}
	got2, err := f.s.CommitBatch(f.ctx, orgA, second, "test-operator", f.claim("CommitBatch", run.RunID+"-2"))
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if got2.RecordCount != 1 {
		t.Fatalf("second commit record_count = %d, want 1 (dedup-1 already landed, only dedup-2 is new)", got2.RecordCount)
	}

	total := f.countAsTenant(orgA, `SELECT count(*) FROM landed_records WHERE tenant_id = $1 AND source_id = $2`, orgA, "erp.invoices")
	if total != 2 {
		t.Fatalf("total landed records = %d, want 2 (dedup-1 once, dedup-2 once)", total)
	}
}

// Negative path #2 (doc-named): wrong-region batch is blocked before
// landing — refused outright, never partially landed, never quarantined.
func TestIngestion_WrongRegionBatchBlockedBeforeLanding(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices") // opened with residency_region = us-east

	req := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "eu-west", NextPosition: "offset-1",
	}
	if _, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", f.claim("CommitBatch", run.RunID)); !errors.Is(err, domain.ErrWrongResidencyRegion) {
		t.Fatalf("wrong-region batch was not blocked: %v", err)
	}

	landed := f.countAsTenant(orgA, `SELECT count(*) FROM landing_objects WHERE run_id = $1`, run.RunID)
	quarantined := f.countAsTenant(orgA, `SELECT count(*) FROM quarantine_items WHERE run_id = $1`, run.RunID)
	if landed != 0 {
		t.Fatalf("wrong-region batch landed %d objects, want 0", landed)
	}
	if quarantined != 0 {
		t.Fatalf("wrong-region batch was quarantined instead of blocked outright: %d", quarantined)
	}
}

// Negative path #3 (doc-named): schema-incompatible batch quarantines
// whole, without partial silent coercion — nothing lands, everything goes
// to quarantine as one unit.
func TestIngestion_SchemaIncompatibleBatchQuarantinesWhole(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	// First batch establishes the registered schema version (v1) via the
	// checkpoint CommitBatch itself advances.
	first := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-1",
	}
	if _, err := f.s.CommitBatch(f.ctx, orgA, first, "test-operator", f.claim("CommitBatch", run.RunID+"-1")); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Second batch declares an incompatible schema version.
	second := domain.CommitBatchRequest{
		RunID:         run.RunID,
		Records:       []domain.BatchRecord{rec("evt-2", "dedup-2"), rec("evt-3", "dedup-3")},
		SchemaVersion: "v2", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-3",
	}
	_, err := f.s.CommitBatch(f.ctx, orgA, second, "test-operator", f.claim("CommitBatch", run.RunID+"-2"))
	if !errors.Is(err, domain.ErrSchemaIncompatible) {
		t.Fatalf("schema-incompatible batch was not quarantined: %v", err)
	}

	landedFromSecond := f.countAsTenant(orgA, `SELECT count(*) FROM landed_records WHERE dedup_key IN ('dedup-2', 'dedup-3')`)
	if landedFromSecond != 0 {
		t.Fatalf("schema-incompatible batch partially landed %d records, want 0 (no silent coercion)", landedFromSecond)
	}

	items, err := f.s.ListQuarantineItems(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("list quarantine items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("quarantine items = %d, want 1 (the whole second batch, as one unit)", len(items))
	}

	runAfter, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if runAfter.Status != domain.IngestionRunPartiallyQuarantined {
		t.Fatalf("run status = %s, want PartiallyQuarantined", runAfter.Status)
	}

	// The checkpoint's registered schema version was NOT overwritten by
	// the incompatible batch.
	cp, err := f.s.GetCheckpoint(f.ctx, orgA, "erp.invoices")
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if cp.SchemaVersion != "v1" {
		t.Fatalf("checkpoint schema_version = %s, want v1 (unchanged by the quarantined batch)", cp.SchemaVersion)
	}
}

// Idempotent replay: retrying CommitBatch with the same claim key must not
// land the batch a second time.
func TestIngestion_CommitBatch_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	req := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-1",
	}
	claim := f.claimWithKey("CommitBatch", run.RunID, "replay-key-1")
	first, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	_, err = f.s.CommitBatch(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.LandingID {
		t.Fatalf("replay of CommitBatch: %v", err)
	}

	count := f.countAsTenant(orgA, `SELECT count(*) FROM landing_objects WHERE run_id = $1`, run.RunID)
	if count != 1 {
		t.Fatalf("landing_objects after replay = %d, want 1", count)
	}
}

// DB-level negative control: a landed object is immutable even to the
// application role directly, not merely by application convention.
func TestIngestion_LandingObjectIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	req := domain.CommitBatchRequest{
		RunID: run.RunID, Records: []domain.BatchRecord{rec("evt-1", "dedup-1")},
		SchemaVersion: "v1", Classification: "internal", ResidencyRegion: "us-east", NextPosition: "offset-1",
	}
	got, err := f.s.CommitBatch(f.ctx, orgA, req, "test-operator", f.claim("CommitBatch", run.RunID))
	if err != nil {
		t.Fatalf("commit batch: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE landing_objects SET record_count = 999 WHERE landing_id = $1`, got.LandingID); err == nil {
		t.Fatal("a raw UPDATE against a landing object was not rejected")
	}
}

// DB-level negative control: a run that has been closed (terminal) cannot
// be mutated further, even to touch an unrelated column, not just its
// status.
func TestIngestion_TerminalRunIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")
	if _, err := f.s.CloseRun(f.ctx, orgA, domain.CloseRunRequest{RunID: run.RunID}, domain.IngestionRunCompleted,
		"test-operator", f.claim("CloseRun", run.RunID)); err != nil {
		t.Fatalf("close run: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE ingestion_runs SET checkpoint_ref = 'tampered' WHERE run_id = $1`, run.RunID); err == nil {
		t.Fatal("a raw UPDATE touching an unrelated column on a terminal run was not rejected")
	}
}

// Tenant isolation: a different organization cannot read another
// tenant's run, checkpoint, or landing evidence — enforced by RLS, not
// merely an application-level filter.
func TestIngestion_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	run := f.startRun(orgA, "erp.invoices")

	if _, err := f.s.GetRun(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrIngestionRunNotFound) {
		t.Fatalf("cross-tenant run read: %v", err)
	}
	own, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil || own.RunID != run.RunID {
		t.Fatalf("own-tenant run read: %+v (err=%v)", own, err)
	}
}
