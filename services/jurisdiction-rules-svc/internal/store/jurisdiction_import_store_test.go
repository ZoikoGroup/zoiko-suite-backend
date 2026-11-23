package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// REF-01 ImportJurisdictionUpdate against a real Postgres: proposing changes
// nothing and keeps every row's errors; approval needs a different principal and
// applies every row or none; a source version is imported once; and the history
// and the as-of reads show what the import did.

func at(t time.Time) *time.Time { return &t }

func ref(code, typ string) domain.JurisdictionRef {
	return domain.JurisdictionRef{Code: code, Type: typ}
}

func refp(code, typ string) *domain.JurisdictionRef { r := ref(code, typ); return &r }

func addRow(code, typ, name string, parent *domain.JurisdictionRef) domain.ImportItemInput {
	return domain.ImportItemInput{
		Action: domain.ImportActionAdd, Target: ref(code, typ), JurisdictionName: name, AuthorityType: "STATE",
		EffectiveFrom: at(epoch), Parent: parent,
	}
}

func renameRow(code, typ, name string, from *time.Time) domain.ImportItemInput {
	return domain.ImportItemInput{Action: domain.ImportActionUpdate, Target: ref(code, typ), JurisdictionName: name, EffectiveFrom: from}
}

func moveRow(code, typ string, parent *domain.JurisdictionRef, from *time.Time) domain.ImportItemInput {
	return domain.ImportItemInput{Action: domain.ImportActionUpdate, Target: ref(code, typ), Parent: parent, EffectiveFrom: from}
}

func retireRow(code, typ string, successor *domain.JurisdictionRef) domain.ImportItemInput {
	return domain.ImportItemInput{Action: domain.ImportActionRetire, Target: ref(code, typ), Successor: successor, Note: "withdrawn by the source"}
}

func importParams(version string, items ...domain.ImportItemInput) domain.ProposeImportParams {
	return domain.ProposeImportParams{
		SourceStandard: "ISO 3166-2", SourceVersion: version, EvidenceRef: "https://example.test/iso-" + version + ".csv",
		Summary: "subdivision changes " + version, Items: items, ActorID: "maker-1",
	}
}

func proposeImport(ctx context.Context, s *store.PgStore, p domain.ProposeImportParams) (*domain.ImportBatch, bool, error) {
	var b *domain.ImportBatch
	var created bool
	err := s.WithTransaction(ctx, func(q store.Querier) error {
		var err error
		b, created, err = s.ProposeJurisdictionImportWithQuerier(ctx, q, p)
		return err
	})
	return b, created, err
}

func mustPropose(t *testing.T, ctx context.Context, s *store.PgStore, version string, items ...domain.ImportItemInput) *domain.ImportBatch {
	t.Helper()
	b, created, err := proposeImport(ctx, s, importParams(version, items...))
	if err != nil || !created {
		t.Fatalf("propose %s: created=%v err=%v", version, created, err)
	}
	if b.InvalidCount != 0 {
		t.Fatalf("propose %s: %d invalid rows: %+v", version, b.InvalidCount, b.Items)
	}
	return b
}

// approveImport runs the approval in a transaction, as the handler does, so a
// failure rolls the whole batch back.
func approveImport(ctx context.Context, s *store.PgStore, batchID, actor string) (*domain.ImportBatch, []domain.AppliedChange, error) {
	var b *domain.ImportBatch
	var changes []domain.AppliedChange
	err := s.WithTransaction(ctx, func(q store.Querier) error {
		var err error
		b, changes, err = s.ApproveJurisdictionImportWithQuerier(ctx, q, domain.DecideImportParams{BatchID: batchID, ActorID: actor})
		return err
	})
	return b, changes, err
}

func mustApprove(t *testing.T, ctx context.Context, s *store.PgStore, batchID string) (*domain.ImportBatch, []domain.AppliedChange) {
	t.Helper()
	b, ch, err := approveImport(ctx, s, batchID, "checker-2")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return b, ch
}

func rejectImport(ctx context.Context, s *store.PgStore, batchID, actor, reason string) (*domain.ImportBatch, error) {
	var b *domain.ImportBatch
	err := s.WithTransaction(ctx, func(q store.Querier) error {
		var err error
		b, err = s.RejectJurisdictionImportWithQuerier(ctx, q, domain.DecideImportParams{BatchID: batchID, ActorID: actor, Reason: reason})
		return err
	})
	return b, err
}

func countJurisdictions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jurisdictions WHERE jurisdiction_code = $1`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func idOfCode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT jurisdiction_id FROM jurisdictions WHERE jurisdiction_code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("no jurisdiction %s: %v", code, err)
	}
	return id
}

func nameOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var n string
	if err := pool.QueryRow(ctx, `SELECT jurisdiction_name FROM jurisdictions WHERE jurisdiction_code = $1`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ── proposing ───────────────────────────────────────────────────────────────

func TestImport_ProposingChangesNothingAndKeepsEveryRow(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)

	b := mustPropose(t, ctx, s, "2026-1",
		addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")),
		// A row may name a parent an earlier row of the same file adds.
		addRow("GB-LND", "STATE_PROVINCE", "London", refp("GB-ENG", "STATE_PROVINCE")),
		renameRow("GB", "COUNTRY", "United Kingdom", at(daysAfter(30))))

	if b.Status != domain.ImportProposed || b.ItemCount != 3 || b.InvalidCount != 0 || len(b.ImportHash) != 64 || b.ProposedBy != "maker-1" {
		t.Fatalf("batch = %+v", b)
	}
	if b.DecidedBy != nil || b.Added != nil || b.Items[0].Outcome != nil {
		t.Errorf("a proposal carries a decision or an outcome: %+v", b)
	}
	if want := domain.ImportHash("ISO 3166-2", "2026-1", importParams("2026-1",
		addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")),
		addRow("GB-LND", "STATE_PROVINCE", "London", refp("GB-ENG", "STATE_PROVINCE")),
		renameRow("GB", "COUNTRY", "United Kingdom", at(daysAfter(30)))).Items); b.ImportHash != want {
		t.Errorf("hash = %s, want the hash of the content, %s", b.ImportHash, want)
	}
	for i, it := range b.Items {
		if it.ItemNo != i+1 || len(it.ValidationErrors) != 0 {
			t.Errorf("row %d = %+v", i+1, it)
		}
	}
	if b.Items[1].Parent == nil || b.Items[1].Parent.Code != "GB-ENG" || b.Items[0].Target.Code != "GB-ENG" {
		t.Errorf("rows were not kept as sent: %+v", b.Items)
	}

	// The registry is exactly as it was.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jurisdictions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d jurisdictions after a proposal (err %v), want the 1 that was there", n, err)
	}
	if got := nameOf(t, ctx, pool, "GB"); got != "Jurisdiction GB" {
		t.Errorf("GB was renamed by a proposal: %q", got)
	}

	back, err := s.GetJurisdictionImport(ctx, b.BatchID)
	if err != nil || len(back.Items) != 3 || back.ImportHash != b.ImportHash {
		t.Errorf("read back = %+v err %v", back, err)
	}
}

func TestImport_EveryRowsErrorsAreKeptAndABadRowBlocksApproval(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	gb := seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	us := seedJurisdiction(t, ctx, pool, "US", "COUNTRY", nil, epoch)
	seedJurisdiction(t, ctx, pool, "AM", "STATE_PROVINCE", &gb, epoch)
	seedJurisdiction(t, ctx, pool, "AM", "STATE_PROVINCE", &us, epoch)
	gone := seedJurisdiction(t, ctx, pool, "GONE", "COUNTRY", nil, epoch)
	if _, _, err := s.DeactivateJurisdiction(ctx, gone, "admin"); err != nil {
		t.Fatal(err)
	}

	b, created, err := proposeImport(ctx, s, importParams("2026-bad",
		addRow("NEW1", "COUNTRY", "New", nil),                                            // 1 fine
		addRow("NEW2", "STATE_PROVINCE", "Orphan", refp("ZZ", "COUNTRY")),                // 2 parent does not exist
		renameRow("NOPE", "COUNTRY", "x", nil),                                           // 3 target does not exist
		renameRow("AM", "STATE_PROVINCE", "x", nil),                                      // 4 two jurisdictions are AM
		addRow("GB", "COUNTRY", "Again", nil),                                            // 5 already exists
		domain.ImportItemInput{Action: "MERGE", Target: ref("A", "B")},                   // 6 not an action
		addRow("NEW1", "COUNTRY", "New again", nil),                                      // 7 same row twice
		retireRow("GB", "COUNTRY", refp("GB", "COUNTRY")),                                // 8 cannot succeed itself
		addRow("NEW3", "STATE_PROVINCE", "Under a retired one", refp("GONE", "COUNTRY")), // 9 retired parent
		renameRow("US", "COUNTRY", "x", at(epoch.AddDate(0, 0, -10))),                    // 10 not after the version in force
	))
	if err != nil || !created {
		t.Fatalf("a batch with bad rows must still be kept: created=%v err=%v", created, err)
	}
	if b.ItemCount != 10 || b.InvalidCount != 9 {
		t.Fatalf("item_count %d invalid_count %d, want 10 and 9", b.ItemCount, b.InvalidCount)
	}
	want := map[int]string{
		2: "no active jurisdiction", 3: "no active jurisdiction", 4: "2 active jurisdictions", 5: "already exists",
		6: "action must be", 7: "already addresses", 8: "cannot succeed itself", 9: "no active jurisdiction", 10: "does not follow",
	}
	for _, it := range b.Items {
		if it.ItemNo == 1 {
			if len(it.ValidationErrors) != 0 {
				t.Errorf("the good row has errors: %v", it.ValidationErrors)
			}
			continue
		}
		if len(it.ValidationErrors) == 0 || !strings.Contains(strings.Join(it.ValidationErrors, "; "), want[it.ItemNo]) {
			t.Errorf("row %d errors = %v, want one mentioning %q", it.ItemNo, it.ValidationErrors, want[it.ItemNo])
		}
	}

	// The errors survive a read, and a different principal still cannot approve it.
	back, _ := s.GetJurisdictionImport(ctx, b.BatchID)
	if back.InvalidCount != 9 || len(back.Items[1].ValidationErrors) == 0 {
		t.Errorf("errors were not kept: %+v", back.Items[1])
	}
	if _, _, err := approveImport(ctx, s, b.BatchID, "checker-2"); !errors.Is(err, domain.ErrImportHasInvalidRows) {
		t.Fatalf("approving a batch with bad rows: %v, want ErrImportHasInvalidRows", err)
	}
	if countJurisdictions(t, ctx, pool, "NEW1") != 0 {
		t.Error("the good row of a bad batch was applied")
	}
}

func TestImport_ASourceVersionIsImportedOnce(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	first := mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))

	// The same content, however it is padded, is the same import.
	padded := addRow("  GB-ENG ", "STATE_PROVINCE", " England", refp("GB", "COUNTRY"))
	again, created, err := proposeImport(ctx, s, importParams("2026-1", padded))
	if err != nil || created || again.BatchID != first.BatchID {
		t.Fatalf("replay: batch %v created=%v err=%v, want the first batch back", again, created, err)
	}

	// Different content for the same version is refused, not merged.
	if _, _, err := proposeImport(ctx, s, importParams("2026-1", addRow("GB-SCT", "STATE_PROVINCE", "Scotland", refp("GB", "COUNTRY")))); !errors.Is(err, domain.ErrImportVersionConflict) {
		t.Errorf("same version, other content: %v, want ErrImportVersionConflict", err)
	}
	// Another version is another import, and another standard is another source.
	if _, created, err := proposeImport(ctx, s, importParams("2026-2", addRow("GB-SCT", "STATE_PROVINCE", "Scotland", refp("GB", "COUNTRY")))); err != nil || !created {
		t.Errorf("another version: created=%v err=%v", created, err)
	}
	other := importParams("2026-1", addRow("GB-WLS", "STATE_PROVINCE", "Wales", refp("GB", "COUNTRY")))
	other.SourceStandard = "ISO 3166-1"
	if _, created, err := proposeImport(ctx, s, other); err != nil || !created {
		t.Errorf("another standard: created=%v err=%v", created, err)
	}

	// An approved import is still that version's import: replays return it.
	mustApprove(t, ctx, s, first.BatchID)
	if b, created, err := proposeImport(ctx, s, importParams("2026-1", padded)); err != nil || created || b.Status != domain.ImportApproved {
		t.Errorf("replay after approval: %+v created=%v err=%v", b, created, err)
	}
	if _, _, err := proposeImport(ctx, s, importParams("2026-1", addRow("GB-SCT", "STATE_PROVINCE", "Scotland", refp("GB", "COUNTRY")))); !errors.Is(err, domain.ErrImportVersionConflict) {
		t.Errorf("approved version, other content: %v", err)
	}
}

func TestImport_ConcurrentProposalsOfOneVersionMakeOneBatch(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	p := importParams("2026-race", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))

	const n = 6
	var wg sync.WaitGroup
	ids, createds, errs := make([]string, n), make([]bool, n), make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			b, created, err := proposeImport(ctx, s, p)
			if b != nil {
				ids[i] = b.BatchID
			}
			createds[i], errs[i] = created, err
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("proposal %d: %v", i, errs[i])
		}
		if createds[i] {
			won++
		}
		if ids[i] != ids[0] {
			t.Errorf("proposal %d returned batch %s, want %s", i, ids[i], ids[0])
		}
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_import_batches`).Scan(&rows)
	if won != 1 || rows != 1 {
		t.Errorf("%d created, %d batches, want exactly one of each", won, rows)
	}
}

// ── approving ───────────────────────────────────────────────────────────────

func TestImport_TheProposerCannotApproveAndTheDatabaseAgrees(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	b := mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))

	if _, _, err := approveImport(ctx, s, b.BatchID, "maker-1"); !errors.Is(err, domain.ErrNotIndependent) {
		t.Fatalf("the proposer approving: %v, want ErrNotIndependent", err)
	}
	if countJurisdictions(t, ctx, pool, "GB-ENG") != 0 {
		t.Error("a refused approval applied rows")
	}
	if back, _ := s.GetJurisdictionImport(ctx, b.BatchID); back.Status != domain.ImportProposed {
		t.Errorf("status = %s after a refused approval", back.Status)
	}

	// Past the application, the constraint refuses the same thing.
	_, err := pool.Exec(ctx, `UPDATE jurisdiction_import_batches
		   SET status = 'APPROVED', decided_by = proposed_by, decided_at = now(),
		       added_count = 1, updated_count = 0, retired_count = 0, unchanged_count = 0
		 WHERE batch_id = $1`, b.BatchID)
	if err == nil || !strings.Contains(err.Error(), "ck_jib_independent") {
		t.Errorf("self-approval written directly: %v, want ck_jib_independent", err)
	}

	// The proposer can withdraw their own batch.
	if _, err := rejectImport(ctx, s, b.BatchID, "maker-1", "wrong file"); err != nil {
		t.Errorf("the proposer withdrawing: %v", err)
	}
}

func TestImport_ApprovalAppliesEveryRowAsOneImportAndTheHistoryShowsIt(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	gb := seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	seedJurisdiction(t, ctx, pool, "US", "COUNTRY", nil, epoch)
	old := seedJurisdiction(t, ctx, pool, "OLD", "COUNTRY", nil, epoch)
	r1 := seedJurisdiction(t, ctx, pool, "R1", "STATE_PROVINCE", &gb, epoch)

	b := mustPropose(t, ctx, s, "2026-1",
		addRow("FR", "COUNTRY", "France", nil),                                     // 1 add, with a successor-to-be
		addRow("FR-IDF", "STATE_PROVINCE", "Ile-de-France", refp("FR", "COUNTRY")), // 2 add under a row of this file
		renameRow("GB", "COUNTRY", "United Kingdom", at(daysAfter(30))),            // 3 rename
		moveRow("R1", "STATE_PROVINCE", refp("FR", "COUNTRY"), at(daysAfter(60))),  // 4 move under a row of this file
		retireRow("OLD", "COUNTRY", refp("FR", "COUNTRY")),                         // 5 retire, successor from this file
		renameRow("US", "COUNTRY", "Jurisdiction US", nil),                         // 6 says what is already true
	)

	done, changes := mustApprove(t, ctx, s, b.BatchID)
	if done.Status != domain.ImportApproved || done.DecidedBy == nil || *done.DecidedBy != "checker-2" || done.DecidedAt == nil {
		t.Fatalf("batch = %+v", done)
	}
	// Reconciliation: the counts account for every row.
	if *done.Added != 2 || *done.Updated != 2 || *done.Retired != 1 || *done.Unchanged != 1 ||
		*done.Added+*done.Updated+*done.Retired+*done.Unchanged != done.ItemCount {
		t.Errorf("counts added=%d updated=%d retired=%d unchanged=%d of %d", *done.Added, *done.Updated, *done.Retired, *done.Unchanged, done.ItemCount)
	}
	wantOutcome := []string{"ADDED", "ADDED", "UPDATED", "UPDATED", "RETIRED", "UNCHANGED"}
	for i, it := range done.Items {
		if it.Outcome == nil || *it.Outcome != wantOutcome[i] || it.JurisdictionID == nil {
			t.Errorf("row %d outcome = %v jurisdiction = %v, want %s", i+1, it.Outcome, it.JurisdictionID, wantOutcome[i])
		}
	}
	// A row that changed nothing is not announced.
	if len(changes) != 5 {
		t.Fatalf("%d changes, want 5 (the unchanged row announces nothing)", len(changes))
	}
	var moved *domain.AppliedChange
	for i := range changes {
		if changes[i].ParentChanged {
			moved = &changes[i]
		}
	}
	if moved == nil || moved.Jurisdiction.JurisdictionID != r1 || moved.Before.ParentJurisdictionID == nil || *moved.Before.ParentJurisdictionID != gb {
		t.Errorf("the move was not reported with the parent it left: %+v", moved)
	}

	fr := idOfCode(t, ctx, pool, "FR")
	idf := idOfCode(t, ctx, pool, "FR-IDF")

	// The new jurisdictions are real and in the hierarchy.
	if j, err := s.FindByID(ctx, idf); err != nil || j.ParentJurisdictionID == nil || *j.ParentJurisdictionID != fr || j.CreatedByPrincipalID != "checker-2" {
		t.Errorf("FR-IDF = %+v err %v", j, err)
	}

	// The history records the change against the batch, with its effective date.
	vs := versionsOf(t, ctx, pool, gb)
	if len(vs) != 2 || vs[1].ChangeType != "UPDATED" || !vs[1].From.Equal(daysAfter(30)) || vs[1].Name != "United Kingdom" ||
		vs[0].To == nil || !vs[0].To.Equal(daysAfter(30)) {
		t.Fatalf("GB versions = %+v", vs)
	}
	if vs[1].Reason == nil || !strings.Contains(*vs[1].Reason, b.BatchID) || !strings.Contains(*vs[1].Reason, "ISO 3166-2 2026-1") {
		t.Errorf("reason = %v, want the batch and source version", vs[1].Reason)
	}
	var evidence string
	if err := pool.QueryRow(ctx, `SELECT evidence_ref FROM jurisdiction_versions WHERE jurisdiction_id = $1 AND version_no = 2`, gb).Scan(&evidence); err != nil || evidence != b.EvidenceRef {
		t.Errorf("evidence_ref = %q (err %v), want %q", evidence, err, b.EvidenceRef)
	}
	if rv := versionsOf(t, ctx, pool, r1); len(rv) != 2 || rv[1].ChangeType != "HIERARCHY_CHANGED" || rv[1].Parent == nil || *rv[1].Parent != fr || !rv[1].From.Equal(daysAfter(60)) {
		t.Errorf("R1 versions = %+v", rv)
	}

	// As-of reads: before the move R1 sat under GB, after it under FR; the old
	// name was GB's until the rename took effect.
	if v, err := s.GetJurisdictionAsOf(ctx, r1, daysAfter(10)); err != nil || v.ParentJurisdictionID == nil || *v.ParentJurisdictionID != gb {
		t.Errorf("R1 before the move: %+v err %v", v, err)
	}
	if v, err := s.GetJurisdictionAsOf(ctx, r1, daysAfter(70)); err != nil || v.ParentJurisdictionID == nil || *v.ParentJurisdictionID != fr {
		t.Errorf("R1 after the move: %+v err %v", v, err)
	}
	if v, _ := s.GetJurisdictionAsOf(ctx, gb, daysAfter(10)); v == nil || v.JurisdictionName != "Jurisdiction GB" {
		t.Errorf("GB before the rename: %+v", v)
	}
	if v, _ := s.GetJurisdictionAsOf(ctx, gb, daysAfter(40)); v == nil || v.JurisdictionName != "United Kingdom" {
		t.Errorf("GB after the rename: %+v", v)
	}
	if h, err := s.GetHierarchy(ctx, gb, daysAfter(10), 3); err != nil || h.Count != 2 {
		t.Errorf("GB's tree before the move = %+v err %v, want GB and R1", h, err)
	}
	if h, err := s.GetHierarchy(ctx, gb, daysAfter(70), 3); err != nil || h.Count != 1 {
		t.Errorf("GB's tree after the move = %+v err %v, want GB alone", h, err)
	}
	if h, err := s.GetHierarchy(ctx, fr, daysAfter(70), 3); err != nil || h.Count != 3 {
		t.Errorf("FR's tree after the move = %+v err %v, want FR, FR-IDF and R1", h, err)
	}

	// The retired jurisdiction is refused for new work, readable as of the past,
	// and names its successor, which this very file added.
	if _, err := s.FindByID(ctx, old); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("a retired jurisdiction is still findable for new work: %v", err)
	}
	if v, err := s.GetJurisdictionAsOf(ctx, old, daysAfter(10)); err != nil || !v.Retired || v.SupersededByJurisdictionID == nil || *v.SupersededByJurisdictionID != fr {
		t.Errorf("OLD as of the past = %+v err %v", v, err)
	}
}

func TestImport_NothingIsAppliedUnlessEverythingIs(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	y := seedJurisdiction(t, ctx, pool, "Y", "COUNTRY", nil, epoch)
	b := mustPropose(t, ctx, s, "2026-1",
		addRow("X", "COUNTRY", "X", nil),
		renameRow("GB", "COUNTRY", "Renamed", nil),
		retireRow("Y", "COUNTRY", nil))

	// The registry moves on between the proposal and the approval.
	if _, _, err := s.DeactivateJurisdiction(ctx, y, "admin"); err != nil {
		t.Fatal(err)
	}

	_, _, err := approveImport(ctx, s, b.BatchID, "checker-2")
	var rowErr *domain.ImportItemError
	if !errors.As(err, &rowErr) || rowErr.ItemNo != 3 || !errors.Is(err, domain.ErrImportItemFailed) {
		t.Fatalf("approve: %v, want the failing row, 3, named", err)
	}
	if countJurisdictions(t, ctx, pool, "X") != 0 || nameOf(t, ctx, pool, "GB") != "Jurisdiction GB" {
		t.Error("rows 1 and 2 were applied although row 3 failed: a partial import")
	}
	if vs := versionsOf(t, ctx, pool, idOfCode(t, ctx, pool, "GB")); len(vs) != 1 {
		t.Errorf("GB has %d versions, want 1: the rename left history behind", len(vs))
	}
	back, _ := s.GetJurisdictionImport(ctx, b.BatchID)
	if back.Status != domain.ImportProposed || back.DecidedBy != nil || back.Items[0].Outcome != nil {
		t.Errorf("the batch moved on although it did not apply: %+v", back)
	}
}

func TestImport_ACycleIsRefusedAtTheRowThatMakesIt(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	a := seedJurisdiction(t, ctx, pool, "A", "COUNTRY", nil, epoch)
	seedJurisdiction(t, ctx, pool, "C", "STATE_PROVINCE", &a, epoch)
	seedJurisdiction(t, ctx, pool, "P", "COUNTRY", nil, epoch)
	seedJurisdiction(t, ctx, pool, "Q", "COUNTRY", nil, epoch)

	// A under its own descendant: visible when proposing.
	b, _, err := proposeImport(ctx, s, importParams("2026-cyc", moveRow("A", "COUNTRY", refp("C", "STATE_PROVINCE"), nil), moveRow("A", "COUNTRY", refp("A", "COUNTRY"), nil)))
	if err != nil {
		t.Fatal(err)
	}
	if b.InvalidCount == 0 || !strings.Contains(strings.Join(b.Items[0].ValidationErrors, ";"), "cycle") {
		t.Errorf("row 1 errors = %v, want a cycle named", b.Items[0].ValidationErrors)
	}

	// P under Q and Q under P: each is fine alone, together they are a loop.
	// Only applying them in order can see that.
	loop := mustPropose(t, ctx, s, "2026-loop",
		moveRow("P", "COUNTRY", refp("Q", "COUNTRY"), nil), moveRow("Q", "COUNTRY", refp("P", "COUNTRY"), nil))
	_, _, err = approveImport(ctx, s, loop.BatchID, "checker-2")
	var rowErr *domain.ImportItemError
	if !errors.As(err, &rowErr) || rowErr.ItemNo != 2 || !strings.Contains(rowErr.Message, "cycle") {
		t.Fatalf("approve: %v, want row 2 refused for a cycle", err)
	}
	var parents int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jurisdictions WHERE jurisdiction_code IN ('P','Q') AND parent_jurisdiction_id IS NOT NULL`).Scan(&parents)
	if parents != 0 {
		t.Errorf("%d of P and Q were moved by an import that failed", parents)
	}
}

// Two imports approved at the same moment, each fine alone, must not leave a
// loop that neither saw: the hierarchy is changed under one lock.
func TestImport_TwoApprovalsCannotRaceIntoACycle(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	for round := 0; round < 5; round++ {
		a, b := fmt.Sprintf("A%d", round), fmt.Sprintf("B%d", round)
		seedJurisdiction(t, ctx, pool, a, "COUNTRY", nil, epoch)
		seedJurisdiction(t, ctx, pool, b, "COUNTRY", nil, epoch)
		b1 := mustPropose(t, ctx, s, fmt.Sprintf("r%d-a", round), moveRow(a, "COUNTRY", refp(b, "COUNTRY"), nil))
		b2 := mustPropose(t, ctx, s, fmt.Sprintf("r%d-b", round), moveRow(b, "COUNTRY", refp(a, "COUNTRY"), nil))

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, c := range []struct{ batch, who string }{{b1.BatchID, "checker-1"}, {b2.BatchID, "checker-2"}} {
			wg.Add(1)
			go func(i int, batch, who string) {
				defer wg.Done()
				<-start
				_, _, errs[i] = approveImport(ctx, s, batch, who)
			}(i, c.batch, c.who)
		}
		close(start)
		wg.Wait()

		ok, refused := 0, 0
		for _, err := range errs {
			var rowErr *domain.ImportItemError
			switch {
			case err == nil:
				ok++
			case errors.As(err, &rowErr) && strings.Contains(rowErr.Message, "cycle"):
				refused++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("round %d: %d approved, %d refused for a cycle, want exactly one of each (%v)", round, ok, refused, errs)
		}
		var moved int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM jurisdictions WHERE jurisdiction_code IN ($1, $2) AND parent_jurisdiction_id IS NOT NULL`, a, b).Scan(&moved)
		if moved != 1 {
			t.Fatalf("round %d: %d of the pair have a parent, want 1: a loop was committed", round, moved)
		}
	}
}

func TestImport_ConcurrentApprovalsApplyOnce(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	b := mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = approveImport(ctx, s, b.BatchID, fmt.Sprintf("checker-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	ok, decided := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrImportDecided):
			decided++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || decided != n-1 || countJurisdictions(t, ctx, pool, "GB-ENG") != 1 {
		t.Errorf("%d approved, %d refused as decided, %d GB-ENG rows: want one winner and one row", ok, decided, countJurisdictions(t, ctx, pool, "GB-ENG"))
	}
}

// ── rejecting, reading ──────────────────────────────────────────────────────

func TestImport_RejectNeedsAReasonFreesTheVersionAndIsFinal(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	b := mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))

	if _, err := rejectImport(ctx, s, b.BatchID, "checker-2", "  "); !errors.Is(err, domain.ErrInvalidImport) {
		t.Errorf("reject with no reason: %v", err)
	}
	if _, err := rejectImport(ctx, s, b.BatchID, "", "x"); !errors.Is(err, domain.ErrInvalidImport) {
		t.Errorf("reject with no actor: %v", err)
	}
	r, err := rejectImport(ctx, s, b.BatchID, "checker-2", "the file is the wrong edition")
	if err != nil || r.Status != domain.ImportRejected || r.DecisionReason == nil || *r.DecisionReason != "the file is the wrong edition" || r.DecidedBy == nil {
		t.Fatalf("reject: %+v err %v", r, err)
	}
	if _, err := rejectImport(ctx, s, b.BatchID, "checker-3", "again"); !errors.Is(err, domain.ErrImportDecided) {
		t.Errorf("rejecting twice: %v", err)
	}
	if _, _, err := approveImport(ctx, s, b.BatchID, "checker-2"); !errors.Is(err, domain.ErrImportDecided) {
		t.Errorf("approving a rejected batch: %v", err)
	}
	if countJurisdictions(t, ctx, pool, "GB-ENG") != 0 {
		t.Error("a rejected batch changed the registry")
	}

	// A corrected file for the same version can be proposed again.
	fixed, created, err := proposeImport(ctx, s, importParams("2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England (corrected)", refp("GB", "COUNTRY"))))
	if err != nil || !created || fixed.BatchID == b.BatchID {
		t.Errorf("re-proposing a rejected version: %+v created=%v err=%v", fixed, created, err)
	}
}

func TestImport_ReadAndList(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	b1 := mustPropose(t, ctx, s, "v1", addRow("N1", "COUNTRY", "N1", nil))
	b2 := mustPropose(t, ctx, s, "v2", addRow("N2", "COUNTRY", "N2", nil))
	b3 := mustPropose(t, ctx, s, "v3", addRow("N3", "COUNTRY", "N3", nil))
	if _, err := rejectImport(ctx, s, b2.BatchID, "checker-2", "no"); err != nil {
		t.Fatal(err)
	}
	mustApprove(t, ctx, s, b3.BatchID)

	all, err := s.ListJurisdictionImports(ctx, "", 0)
	if err != nil || len(all) != 3 || all[0].BatchID != b3.BatchID || all[2].BatchID != b1.BatchID {
		t.Fatalf("list = %v err %v, want the three newest first", all, err)
	}
	if all[0].Items != nil {
		t.Error("a listing carries rows; read one batch for them")
	}
	for status, want := range map[string]string{"PROPOSED": b1.BatchID, "REJECTED": b2.BatchID, "APPROVED": b3.BatchID} {
		got, err := s.ListJurisdictionImports(ctx, status, 10)
		if err != nil || len(got) != 1 || got[0].BatchID != want {
			t.Errorf("list %s = %v err %v, want only %s", status, got, err, want)
		}
	}
	if two, _ := s.ListJurisdictionImports(ctx, "", 2); len(two) != 2 {
		t.Errorf("limit 2 gave %d", len(two))
	}

	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if _, err := s.GetJurisdictionImport(ctx, id); !errors.Is(err, domain.ErrImportNotFound) {
			t.Errorf("get %q: %v, want ErrImportNotFound", id, err)
		}
		if _, _, err := approveImport(ctx, s, id, "checker-2"); !errors.Is(err, domain.ErrImportNotFound) {
			t.Errorf("approve %q: %v, want ErrImportNotFound", id, err)
		}
	}
}

func TestImport_ABatchAsAWholeMustBeAcceptable(t *testing.T) {
	s, _, ctx := newTestStore(t)
	p := importParams("2026-1")
	if _, _, err := proposeImport(ctx, s, p); !errors.Is(err, domain.ErrInvalidImport) {
		t.Errorf("no rows: %v", err)
	}
	p = importParams("2026-1", addRow("X", "COUNTRY", "X", nil))
	p.EvidenceRef = ""
	if _, _, err := proposeImport(ctx, s, p); !errors.Is(err, domain.ErrInvalidImport) {
		t.Errorf("no evidence: %v", err)
	}
}

// ── the database holds the line too ─────────────────────────────────────────

func TestImport_TheDatabaseKeepsImportsAsEvidence(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	b := mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))
	bad, _, err := proposeImport(ctx, s, importParams("2026-bad", renameRow("NOPE", "COUNTRY", "x", nil)))
	if err != nil || bad.InvalidCount != 1 {
		t.Fatalf("bad batch: %+v err %v", bad, err)
	}
	gb := idOfCode(t, ctx, pool, "GB")

	for name, sql := range map[string]string{
		"rewriting a batch's summary": `UPDATE jurisdiction_import_batches SET summary = 'x' WHERE batch_id = '` + b.BatchID + `'`,
		"rewriting a batch's hash":    `UPDATE jurisdiction_import_batches SET import_hash = repeat('0', 64) WHERE batch_id = '` + b.BatchID + `'`,
		"rewriting who proposed it":   `UPDATE jurisdiction_import_batches SET proposed_by = 'checker-2' WHERE batch_id = '` + b.BatchID + `'`,
		"deleting a batch":            `DELETE FROM jurisdiction_import_batches WHERE batch_id = '` + b.BatchID + `'`,
		"truncating batches":          `TRUNCATE jurisdiction_import_batches CASCADE`,
		"rewriting a row's action":    `UPDATE jurisdiction_import_items SET action = 'RETIRE' WHERE batch_id = '` + b.BatchID + `'`,
		"erasing a row's errors":      `UPDATE jurisdiction_import_items SET validation_errors = '[]' WHERE batch_id = '` + bad.BatchID + `'`,
		"deleting a row":              `DELETE FROM jurisdiction_import_items WHERE batch_id = '` + b.BatchID + `'`,
		"truncating rows":             `TRUNCATE jurisdiction_import_items`,
		"approving a batch that has a bad row": `UPDATE jurisdiction_import_batches SET status = 'APPROVED', decided_by = 'checker-2', decided_at = now(),
			added_count = 0, updated_count = 1, retired_count = 0, unchanged_count = 0 WHERE batch_id = '` + bad.BatchID + `'`,
		"rejecting with no reason": `UPDATE jurisdiction_import_batches SET status = 'REJECTED', decided_by = 'checker-2', decided_at = now() WHERE batch_id = '` + b.BatchID + `'`,
		"deciding with no decider": `UPDATE jurisdiction_import_batches SET status = 'REJECTED', decision_reason = 'x' WHERE batch_id = '` + b.BatchID + `'`,
		"approving with counts that do not add up": `UPDATE jurisdiction_import_batches SET status = 'APPROVED', decided_by = 'checker-2', decided_at = now(),
			added_count = 5, updated_count = 0, retired_count = 0, unchanged_count = 0 WHERE batch_id = '` + b.BatchID + `'`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}

	// A row's outcome is recorded once.
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_import_items SET outcome = 'ADDED', jurisdiction_id = $2 WHERE batch_id = $1`, b.BatchID, gb); err != nil {
		t.Fatalf("recording an outcome: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_import_items SET outcome = 'UPDATED' WHERE batch_id = $1`, b.BatchID); err == nil {
		t.Error("an outcome was changed after it was recorded")
	}
	// An outcome names the jurisdiction it concerned.
	if _, err := pool.Exec(ctx, `UPDATE jurisdiction_import_items SET outcome = 'ADDED' WHERE batch_id = $1`, bad.BatchID); err == nil {
		t.Error("an outcome with no jurisdiction was recorded")
	}

	// One live import per source version, whatever the application does.
	if _, err := pool.Exec(ctx, `
		INSERT INTO jurisdiction_import_batches (source_standard, source_version, import_hash, evidence_ref, summary, item_count, proposed_by)
		VALUES ('ISO 3166-2', '2026-1', repeat('1', 64), 'e', 's', 1, 'someone')`); err == nil {
		t.Error("a second live import of one source version was inserted")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jurisdiction_import_batches (source_standard, source_version, import_hash, evidence_ref, summary, item_count, proposed_by)
		VALUES ('ISO 3166-2', '2026-9', 'not-a-hash', 'e', 's', 1, 'someone')`); err == nil {
		t.Error("a batch with no real hash was inserted")
	}
}

func TestMigration000029_DownIsRefusedWhileImportsExist(t *testing.T) {
	s, pool, ctx := newTestStore(t)
	seedJurisdiction(t, ctx, pool, "GB", "COUNTRY", nil, epoch)
	mustPropose(t, ctx, s, "2026-1", addRow("GB-ENG", "STATE_PROVINCE", "England", refp("GB", "COUNTRY")))
	down, up := migration(t, "000029_jurisdiction_imports.down.sql"), migration(t, "000029_jurisdiction_imports.up.sql")

	if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "000029 down refused") {
		t.Fatalf("down with imports present: %v, want it refused", err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("re-running up: %v", err)
	}
	for _, tbl := range []string{"jurisdiction_import_items", "jurisdiction_import_batches"} {
		if _, err := pool.Exec(ctx, `ALTER TABLE `+tbl+` DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM `+tbl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down with nothing to lose: %v", err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
