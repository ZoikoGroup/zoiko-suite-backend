package store_test

// SPEC_DEVIATIONS A9 against a real Postgres: CLOSED, the resolution outcome
// and the append-only transition history (migration 000006). Skips (not
// fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

type wantTransition struct{ from, to string }

func assertTransitions(t *testing.T, s *store.PgStore, ctx context.Context, id string, want []wantTransition) []domain.PairTransition {
	t.Helper()
	got, err := s.ListTransitions(ctx, id)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d transitions, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		from := ""
		if got[i].FromState != nil {
			from = *got[i].FromState
		}
		if from != w.from || got[i].ToState != w.to {
			t.Fatalf("transition %d: expected %q -> %q, got %q -> %q", i, w.from, w.to, from, got[i].ToState)
		}
	}
	return got
}

func mismatch(entry *domain.IntercompanyEntry, reason string) {
	targetJ := uuid.New().String()
	entry.TargetJournalID = &targetJ
	entry.MatchStatus = domain.MatchStatusMismatch
	entry.MismatchReason = &reason
}

func TestPgStore_A9_EveryStateChangeIsRecorded(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	period := "2026-09"
	entry := newTestEntry(tenantID, uuid.New().String())
	entry.FiscalPeriod = &period
	if _, err := s.CreateEntry(ctx, entry, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	id := entry.IntercompanyEntryID
	if err := s.AcknowledgeCounterparty(ctx, id, "acker", "corr-ack"); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	mismatch(entry, "amount_mismatch: first")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "matcher", "corr-m1"); err != nil {
		t.Fatalf("UpdateMatch 1: %v", err)
	}
	// A second mismatch is a new attempt, not a state change: no new row.
	mismatch(entry, "amount_mismatch: second")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "matcher", "corr-m2"); err != nil {
		t.Fatalf("UpdateMatch 2: %v", err)
	}
	if err := s.DisputeIntercompany(ctx, id, "disputer", "wrong rate", "corr-dispute"); err != nil {
		t.Fatalf("Dispute: %v", err)
	}
	if err := s.ResolveMismatch(ctx, id, "resolver", "corrected on the UK side", domain.ResolutionCorrected, "corr-resolve"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := assertTransitions(t, s, ctx, id, []wantTransition{
		{"", domain.MatchStatusUnmatched},
		{domain.MatchStatusUnmatched, domain.MatchStatusAwaitingCounterparty},
		{domain.MatchStatusAwaitingCounterparty, domain.MatchStatusMismatch},
		{domain.MatchStatusMismatch, domain.MatchStatusDisputed},
		{domain.MatchStatusDisputed, domain.MatchStatusResolved},
	})
	if got[0].Actor != "creator" || got[0].CorrelationID == nil || *got[0].CorrelationID != "corr-create" {
		t.Errorf("creation row: %+v", got[0])
	}
	if got[2].Reason == nil || *got[2].Reason != "amount_mismatch: first" {
		t.Errorf("mismatch row reason: %v", got[2].Reason)
	}
	if got[3].Reason == nil || *got[3].Reason != "wrong rate" || got[3].Actor != "disputer" {
		t.Errorf("dispute row: %+v", got[3])
	}
	if got[4].ResolutionOutcome == nil || *got[4].ResolutionOutcome != domain.ResolutionCorrected {
		t.Errorf("resolve row outcome: %v", got[4].ResolutionOutcome)
	}
	for _, tr := range got {
		if tr.FiscalPeriod == nil || *tr.FiscalPeriod != period {
			t.Errorf("transition %d does not carry the pair's period: %v", tr.TransitionID, tr.FiscalPeriod)
		}
	}

	e, err := s.GetEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e.ResolutionOutcome == nil || *e.ResolutionOutcome != domain.ResolutionCorrected {
		t.Fatalf("entry outcome: %v", e.ResolutionOutcome)
	}
}

func TestPgStore_A9_CloseOnlyFromMatched(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "creator", "c1"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	id := entry.IntercompanyEntryID

	// Not yet matched: an open pair cannot be closed out of view.
	if err := s.ClosePair(ctx, id, "closer", "too early", "c2"); !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("close from UNMATCHED: expected ErrInvalidPairTransition, got %v", err)
	}

	targetJ := uuid.New().String()
	entry.TargetJournalID, entry.MatchStatus, entry.MismatchReason = &targetJ, domain.MatchStatusMatched, nil
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "matcher", "c3"); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}
	if err := s.ClosePair(ctx, id, "closer", "period 2026-09 consolidated", "c4"); err != nil {
		t.Fatalf("ClosePair: %v", err)
	}

	e, err := s.GetEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e.MatchStatus != domain.MatchStatusClosed || e.ClosedAt == nil || e.ClosedByPrincipalID == nil ||
		*e.ClosedByPrincipalID != "closer" || e.CloseReason == nil {
		t.Fatalf("close not recorded: %+v", e)
	}

	// Terminal: no second close, no re-match.
	if err := s.ClosePair(ctx, id, "closer", "again", "c5"); !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("second close: expected ErrInvalidPairTransition, got %v", err)
	}
	entry.MatchStatus = domain.MatchStatusMatched
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "matcher", "c6"); !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("match after close: expected ErrInvalidPairTransition, got %v", err)
	}

	assertTransitions(t, s, ctx, id, []wantTransition{
		{"", domain.MatchStatusUnmatched},
		{domain.MatchStatusUnmatched, domain.MatchStatusMatched},
		{domain.MatchStatusMatched, domain.MatchStatusClosed},
	})

	if err := s.ClosePair(ctx, uuid.New().String(), "closer", "r", "c7"); !errors.Is(err, domain.ErrEntryNotFound) {
		t.Fatalf("unknown pair: expected ErrEntryNotFound, got %v", err)
	}
}

func TestPgStore_A9_ResolveRequiresOutcome(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "x")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "a", "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "a", "r", "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveMismatch(ctx, entry.IntercompanyEntryID, "a", "n", "", "c"); !errors.Is(err, domain.ErrResolutionOutcomeInvalid) {
		t.Fatalf("expected ErrResolutionOutcomeInvalid, got %v", err)
	}
	// And the database refuses a new RESOLVED row without one, whatever wrote it.
	if _, err := pool.Exec(ctx, `UPDATE intercompany_entries SET match_status = 'RESOLVED' WHERE intercompany_entry_id = $1`,
		entry.IntercompanyEntryID); err == nil || !strings.Contains(err.Error(), "chk_resolved_has_outcome") {
		t.Fatalf("expected chk_resolved_has_outcome to refuse, got %v", err)
	}
}

func TestPgStore_A9_HistoryIsImmutable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"rewritten": `UPDATE intercompany_pair_transitions SET to_state = 'MATCHED' WHERE intercompany_entry_id = $1`,
		"deleted":   `DELETE FROM intercompany_pair_transitions WHERE intercompany_entry_id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, entry.IntercompanyEntryID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("history %s: expected the immutability trigger to refuse, got %v", name, err)
		}
	}
}

// Probed under an ordinary role (rlsVisibleRows): the test connection is a
// superuser and would bypass RLS.
func TestPgStore_A9_HistoryIsTenantIsolatedByRLS(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctx := context.Background()

	entry := newTestEntry(tenantA, uuid.New().String())
	if _, err := s.CreateEntry(svcmiddleware.WithTenant(ctx, tenantA), entry, "a", "c"); err != nil {
		t.Fatal(err)
	}

	if n := rlsVisibleRows(t, pool, "intercompany_pair_transitions", tenantA, entry.IntercompanyEntryID); n != 1 {
		t.Fatalf("tenant A should see its own history, saw %d rows", n)
	}
	if n := rlsVisibleRows(t, pool, "intercompany_pair_transitions", tenantB, entry.IntercompanyEntryID); n != 0 {
		t.Fatalf("RLS failure: tenant B saw %d of tenant A's transition rows", n)
	}
}

// Migration 000006 on rows written before it: each pair gets one history row
// for the state it is in, nothing earlier is invented, and a pair resolved
// before outcomes existed stays readable with no outcome.
func TestMigration000006_BackfillsCurrentStateOnly(t *testing.T) {
	pool := openEmptyPool(t)
	applyMigrations(t, pool, migrations[:5]...)

	ctx := context.Background()
	tenantID := uuid.New().String()
	resolvedID, matchedID := uuid.New().String(), uuid.New().String()
	now := time.Now().UTC()
	for id, status := range map[string]string{resolvedID: "RESOLVED", matchedID: "MATCHED"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO intercompany_entries (intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, amount, currency_code, counterparty_amount, counterparty_currency_code,
				match_status, created_at, updated_at)
			VALUES ($1, $2, 'le-us', 'le-uk', $3, 10, 'USD', 10, 'USD', $4, $5, $5)`,
			id, tenantID, uuid.New().String(), status, now); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}

	applyMigrations(t, pool, migrations[5:]...)

	s := store.New(pool)
	tctx := svcmiddleware.WithTenant(ctx, tenantID)
	for id, status := range map[string]string{resolvedID: "RESOLVED", matchedID: "MATCHED"} {
		got := assertTransitions(t, s, tctx, id, []wantTransition{{"", status}})
		if got[0].Actor != "migration:000006" {
			t.Errorf("%s: backfill actor %q", status, got[0].Actor)
		}
	}
	e, err := s.GetEntry(tctx, resolvedID)
	if err != nil {
		t.Fatalf("legacy RESOLVED pair no longer readable: %v", err)
	}
	if e.ResolutionOutcome != nil {
		t.Fatalf("an outcome was invented for a legacy resolution: %q", *e.ResolutionOutcome)
	}
}
