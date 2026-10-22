package store_test

// SPEC_DEVIATIONS A3 against a real Postgres: match attempts are appended,
// never overwritten, mismatch_reason reads the latest one, and migration
// 000007 carries existing reasons over. Skips (not fails) if
// TEST_DATABASE_URL isn't set.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

// rlsVisibleRows counts table's rows for one pair as an ordinary role with
// app.tenant_id set to tenant. The test connection is a superuser, which
// bypasses RLS entirely, and the query has no tenant predicate, so only the
// table's policy can be doing the filtering.
func rlsVisibleRows(t *testing.T, pool *pgxpool.Pool, table, tenant, entryID string) int {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DO $$ BEGIN CREATE ROLE intercompany_rls_probe NOLOGIN;
		EXCEPTION WHEN duplicate_object THEN NULL; END $$`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := pool.Exec(ctx, `GRANT SELECT ON `+table+` TO intercompany_rls_probe`); err != nil {
		t.Fatalf("grant: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE intercompany_rls_probe`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE intercompany_entry_id = $1`, entryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// attemptFor builds the attempt for a mismatch on rule, as the handler would.
func attemptFor(entry *domain.IntercompanyEntry, rule, message string, observed domain.Decimal) *domain.MatchAttempt {
	a := testAttempt(entry)
	a.ExaminedJournalIDs = []string{*entry.TargetJournalID}
	if rule != "" {
		a.FailedRule, a.Message = &rule, &message
	}
	cur, le, st, fp := "USD", "le-uk", "FINALIZED", "2026-09"
	a.Target.ObservedAmount, a.Target.ObservedCurrency = &observed, &cur
	a.Target.ObservedLegalEntityID, a.Target.ObservedStatus, a.Target.ObservedFiscalPeriod = &le, &st, &fp
	return a
}

func TestPgStore_A3_AttemptsAreAppendedAndReasonIsTheLatest(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	period := "2026-09"
	entry := newTestEntry(tenantID, uuid.New().String())
	entry.FiscalPeriod = &period
	if _, err := s.CreateEntry(ctx, entry, "creator", "c0"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	id := entry.IntercompanyEntryID
	reason := func() *string {
		t.Helper()
		e, err := s.GetEntry(ctx, id)
		if err != nil {
			t.Fatalf("GetEntry: %v", err)
		}
		return e.MismatchReason
	}
	if r := reason(); r != nil {
		t.Fatalf("a new pair has no mismatch_reason, got %q", *r)
	}

	// Two failed attempts against different journals, then a match.
	j1, j2, j3 := uuid.New().String(), uuid.New().String(), uuid.New().String()
	entry.TargetJournalID, entry.MatchStatus = &j1, domain.MatchStatusMismatch
	if err := s.UpdateMatch(ctx, entry, attemptFor(entry, domain.MatchRuleAmountMismatch, "amount_mismatch: first", "4000"), "matcher", "c1"); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if r := reason(); r == nil || *r != "amount_mismatch: first" {
		t.Fatalf("after attempt 1: %v", r)
	}
	entry.TargetJournalID = &j2
	if err := s.UpdateMatch(ctx, entry, attemptFor(entry, domain.MatchRuleCurrencyMismatch, "currency_mismatch: second", "5000"), "matcher", "c2"); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if r := reason(); r == nil || *r != "currency_mismatch: second" {
		t.Fatalf("after attempt 2: %v", r)
	}
	entry.TargetJournalID, entry.MatchStatus = &j3, domain.MatchStatusMatched
	if err := s.UpdateMatch(ctx, entry, attemptFor(entry, "", "", "5000"), "matcher", "c3"); err != nil {
		t.Fatalf("attempt 3: %v", err)
	}
	if r := reason(); r != nil {
		t.Fatalf("a matched pair has no mismatch_reason, got %q", *r)
	}

	got, err := s.ListMatchAttempts(ctx, id)
	if err != nil {
		t.Fatalf("ListMatchAttempts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected all 3 attempts kept, got %d", len(got))
	}
	first := got[0]
	if first.Outcome != domain.MatchStatusMismatch || *first.FailedRule != domain.MatchRuleAmountMismatch ||
		*first.Message != "amount_mismatch: first" || *first.Target.JournalID != j1 ||
		first.ExaminedJournalIDs[0] != j1 || *first.Target.ObservedAmount != "4000" {
		t.Fatalf("first attempt not kept as written: %+v", first)
	}
	if first.Target.ExpectedAmount != "5000.0000" || first.Target.ExpectedCurrency != "USD" || first.Tolerance != "0.001" {
		t.Fatalf("expected side / tolerance: %+v", first)
	}
	if first.FiscalPeriod == nil || *first.FiscalPeriod != period || first.AttemptedBy != "test" {
		t.Fatalf("period/actor: %+v", first)
	}
	if first.Source.ObservedAmount != nil || first.Source.JournalID == nil || *first.Source.JournalID != entry.SourceJournalID {
		t.Fatalf("source side: %+v", first.Source)
	}
	if got[2].Outcome != domain.MatchStatusMatched || got[2].FailedRule != nil || got[2].Message != nil {
		t.Fatalf("matched attempt: %+v", got[2])
	}

	// The list endpoint's rows carry the same derived reason.
	list, err := s.ListEntries(ctx, "le-us", "")
	if err != nil || len(list) != 1 || list[0].MismatchReason != nil {
		t.Fatalf("ListEntries: %v %+v", err, list)
	}
}

// The reason stays with a pair after it leaves MISMATCH for DISPUTED, and a
// stale attempt that is refused records nothing.
func TestPgStore_A3_RefusedAttemptIsNotRecorded(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "amount_mismatch: real")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "a", "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "d", "disputed", "c"); err != nil {
		t.Fatal(err)
	}

	entry.MatchStatus, entry.MismatchReason = domain.MatchStatusMatched, nil
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "late", "c"); !errors.Is(err, domain.ErrInvalidPairTransition) {
		t.Fatalf("expected ErrInvalidPairTransition, got %v", err)
	}
	attempts, err := s.ListMatchAttempts(ctx, entry.IntercompanyEntryID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("a refused attempt must not be recorded: %v %d", err, len(attempts))
	}
	e, err := s.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatal(err)
	}
	if e.MatchStatus != domain.MatchStatusDisputed || e.MismatchReason == nil || *e.MismatchReason != "amount_mismatch: real" {
		t.Fatalf("the disputed pair should still explain its mismatch: %s %v", e.MatchStatus, e.MismatchReason)
	}
}

// general-ledger-svc's figures are recorded as reported, even where they
// would not fit the pair's own NUMERIC(18,4).
func TestPgStore_A3_ObservedAmountsAreNotRounded(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "amount_mismatch: huge")
	const observed = "123456789012345678.123456789"
	if err := s.UpdateMatch(ctx, entry, attemptFor(entry, domain.MatchRuleAmountMismatch, "amount_mismatch: huge", observed), "a", "c"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListMatchAttempts(ctx, entry.IntercompanyEntryID)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %d", err, len(got))
	}
	if *got[0].Target.ObservedAmount != observed {
		t.Fatalf("observed amount changed: %q", *got[0].Target.ObservedAmount)
	}
}

func TestPgStore_A3_AttemptsAreImmutableAndTenantIsolated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantA)

	entry := newTestEntry(tenantA, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "amount_mismatch: x")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "a", "c"); err != nil {
		t.Fatal(err)
	}

	for name, sql := range map[string]string{
		"rewritten": `UPDATE intercompany_match_attempts SET message = 'nothing to see' WHERE intercompany_entry_id = $1`,
		"deleted":   `DELETE FROM intercompany_match_attempts WHERE intercompany_entry_id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, entry.IntercompanyEntryID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("attempt %s: expected the immutability trigger to refuse, got %v", name, err)
		}
	}

	if n := rlsVisibleRows(t, pool, "intercompany_match_attempts", tenantA, entry.IntercompanyEntryID); n != 1 {
		t.Fatalf("tenant A should see its own attempt, saw %d", n)
	}
	if n := rlsVisibleRows(t, pool, "intercompany_match_attempts", tenantB, entry.IntercompanyEntryID); n != 0 {
		t.Fatalf("RLS failure: tenant B saw %d of tenant A's attempts", n)
	}
}

// Migration 000007 on rows written before it: a pair's existing
// mismatch_reason reads the same afterwards, now as a LEGACY_UNSTRUCTURED
// attempt; a pair that never mismatched gets no attempt.
func TestMigration000007_CarriesExistingReasonsOver(t *testing.T) {
	pool := openEmptyPool(t)
	applyMigrations(t, pool, migrations[:6]...)

	ctx := context.Background()
	tenantID := uuid.New().String()
	mismatchID, matchedID, targetJ := uuid.New().String(), uuid.New().String(), uuid.New().String()
	now := time.Now().UTC()
	seed := func(id, status string, reason, target any) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO intercompany_entries (intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, target_journal_id, amount, currency_code, counterparty_amount, counterparty_currency_code,
				match_status, mismatch_reason, created_at, updated_at)
			VALUES ($1, $2, 'le-us', 'le-uk', $3, $4, 10, 'USD', 8, 'GBP', $5, $6, $7, $7)`,
			id, tenantID, uuid.New().String(), target, status, reason, now); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}
	seed(mismatchID, "MISMATCH", "amount_mismatch: intercompany amount 10.000000 != journal amount 9.000000", targetJ)
	seed(matchedID, "MATCHED", nil, uuid.New().String())

	applyMigrations(t, pool, migrations[6:]...)

	s := store.New(pool)
	tctx := svcmiddleware.WithTenant(ctx, tenantID)
	e, err := s.GetEntry(tctx, mismatchID)
	if err != nil {
		t.Fatal(err)
	}
	if e.MismatchReason == nil || *e.MismatchReason != "amount_mismatch: intercompany amount 10.000000 != journal amount 9.000000" {
		t.Fatalf("existing reason not carried over: %v", e.MismatchReason)
	}
	attempts, err := s.ListMatchAttempts(tctx, mismatchID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("%v %d", err, len(attempts))
	}
	a := attempts[0]
	if *a.FailedRule != domain.MatchRuleLegacyUnstructured || a.AttemptedBy != "migration:000007" ||
		a.Target.ExpectedAmount != "8.0000" || a.Target.ExpectedCurrency != "GBP" ||
		len(a.ExaminedJournalIDs) != 1 || a.ExaminedJournalIDs[0] != targetJ || a.Tolerance != "0.001" {
		t.Fatalf("legacy attempt: %+v", a)
	}

	m, err := s.GetEntry(tctx, matchedID)
	if err != nil {
		t.Fatal(err)
	}
	if m.MismatchReason != nil {
		t.Fatalf("matched pair gained a reason: %q", *m.MismatchReason)
	}
	if attempts, _ := s.ListMatchAttempts(tctx, matchedID); len(attempts) != 0 {
		t.Fatalf("an attempt was invented for a pair with no recorded mismatch: %+v", attempts)
	}
}

// Rolling 000007 back writes each pair's latest attempt message to
// mismatch_reason, so the pre-000007 binary reads what this one did.
func TestMigration000007_DownWritesTheLatestReasonBack(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "amount_mismatch: older")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "a", "c"); err != nil {
		t.Fatal(err)
	}
	mismatch(entry, "currency_mismatch: latest")
	if err := s.UpdateMatch(ctx, entry, testAttempt(entry), "a", "c"); err != nil {
		t.Fatal(err)
	}

	down, err := os.ReadFile(filepath.Join("..", "..", "deployments", "migrations", "000007_match_attempts.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(bytes.TrimPrefix(down, []byte("\xef\xbb\xbf")))); err != nil {
		t.Fatalf("apply 000007 down: %v", err)
	}
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT mismatch_reason FROM intercompany_entries WHERE intercompany_entry_id = $1`,
		entry.IntercompanyEntryID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == nil || *reason != "currency_mismatch: latest" {
		t.Fatalf("expected the latest reason written back, got %v", reason)
	}
}
