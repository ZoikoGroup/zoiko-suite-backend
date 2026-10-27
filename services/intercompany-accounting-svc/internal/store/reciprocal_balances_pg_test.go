package store_test

// SPEC_DEVIATIONS A1/B10 against a real Postgres: GetReciprocalBalances'
// grouped read — exact per-currency totals per direction and match_status,
// the period filter, and nothing leaked across the pair or the tenant.
// Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

// newBalanceEntry is one pair for the reciprocal-balance fixtures.
func newBalanceEntry(tenantID, source, target, currency, amount, period string) *domain.IntercompanyEntry {
	e := newTestEntry(tenantID, uuid.NewString())
	e.SourceLegalEntityID, e.TargetLegalEntityID = source, target
	e.Amount, e.CurrencyCode = domain.Decimal(amount), currency
	e.CounterpartyAmount, e.CounterpartyCurrencyCode = domain.Decimal(amount), currency
	if period != "" {
		e.FiscalPeriod = &period
	}
	return e
}

// inStatus creates e and drives it to status through the service's own
// lifecycle methods, so the fixture rows are the rows the service writes.
func inStatus(t *testing.T, s *store.PgStore, ctx context.Context, e *domain.IntercompanyEntry, status string) {
	t.Helper()
	if _, err := s.CreateEntry(ctx, e, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	targetJ := uuid.NewString()
	switch status {
	case domain.MatchStatusUnmatched:
		return
	case domain.MatchStatusMatched:
		e.TargetJournalID = &targetJ
		e.MatchStatus = domain.MatchStatusMatched
		if err := s.UpdateMatch(ctx, e, testAttempt(e), "matcher", "corr-match"); err != nil {
			t.Fatalf("UpdateMatch(MATCHED): %v", err)
		}
	case domain.MatchStatusMismatch:
		reason := "amount_mismatch: test"
		e.TargetJournalID = &targetJ
		e.MatchStatus = domain.MatchStatusMismatch
		e.MismatchReason = &reason
		if err := s.UpdateMatch(ctx, e, testAttempt(e), "matcher", "corr-mismatch"); err != nil {
			t.Fatalf("UpdateMatch(MISMATCH): %v", err)
		}
	case domain.MatchStatusClosed:
		e.TargetJournalID = &targetJ
		e.MatchStatus = domain.MatchStatusMatched
		if err := s.UpdateMatch(ctx, e, testAttempt(e), "matcher", "corr-match"); err != nil {
			t.Fatalf("UpdateMatch(MATCHED): %v", err)
		}
		if err := s.ClosePair(ctx, e.IntercompanyEntryID, "closer", "period consolidated", "corr-close"); err != nil {
			t.Fatalf("ClosePair: %v", err)
		}
	default:
		t.Fatalf("unhandled fixture status %q", status)
	}
}

type groupKey struct{ source, target, currency, status string }

func groupMap(groups []domain.ReciprocalBalanceGroup) map[groupKey]domain.Decimal {
	m := make(map[groupKey]domain.Decimal, len(groups))
	for _, g := range groups {
		m[groupKey{g.SourceLegalEntityID, g.TargetLegalEntityID, g.CurrencyCode, g.MatchStatus}] = g.Amount
	}
	return m
}

func assertGroups(t *testing.T, groups []domain.ReciprocalBalanceGroup, want map[groupKey]domain.Decimal) {
	t.Helper()
	got := groupMap(groups)
	for k, wantAmount := range want {
		gotAmount, ok := got[k]
		if !ok {
			t.Errorf("missing group %+v (groups: %+v)", k, groups)
			continue
		}
		if gotAmount != wantAmount {
			t.Errorf("group %+v: expected %s, got %s", k, wantAmount, gotAmount)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected group %+v = %s", k, got[k])
		}
	}
}

func TestPgStore_A1_ReciprocalBalances_GroupsExact(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	const (
		a         = "le-us"
		b         = "le-uk"
		c         = "le-de"
		period    = "2026-09"
		oldPeriod = "2026-08"
	)

	// Direction A→B, MATCHED: two USD groups that must sum exactly, a GBP
	// group that must stay its own currency.
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "100.0001", period), domain.MatchStatusMatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "0.3", period), domain.MatchStatusMatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "GBP", "50.5", period), domain.MatchStatusMatched)
	// Direction B→A, MATCHED.
	inStatus(t, s, ctx, newBalanceEntry(tenantID, b, a, "USD", "30", period), domain.MatchStatusMatched)
	// UNMATCHED, MISMATCH and CLOSED: CLOSED still counts — it was a real
	// settled position.
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "7", period), domain.MatchStatusUnmatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "12", period), domain.MatchStatusClosed)
	// A cross-currency pair: its counterparty side (GBP) must not be counted
	// into the source side's USD total, nor co-mingled with the GBP pair.
	cross := newBalanceEntry(tenantID, a, b, "USD", "5000", period)
	cross.CounterpartyAmount, cross.CounterpartyCurrencyCode = "3950.1234", "GBP"
	inStatus(t, s, ctx, cross, domain.MatchStatusMismatch)
	// Out of scope: another period, and a pair with no period at all.
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "999", oldPeriod), domain.MatchStatusMatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "EUR", "1", ""), domain.MatchStatusMatched)
	// Out of scope: a third entity.
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, c, "USD", "999", period), domain.MatchStatusMatched)

	groups, err := s.ReciprocalBalances(ctx, a, b, period)
	if err != nil {
		t.Fatalf("ReciprocalBalances: %v", err)
	}
	assertGroups(t, groups, map[groupKey]domain.Decimal{
		{a, b, "USD", domain.MatchStatusMatched}:   "100.3001",
		{a, b, "GBP", domain.MatchStatusMatched}:   "50.5",
		{b, a, "USD", domain.MatchStatusMatched}:   "30",
		{a, b, "USD", domain.MatchStatusUnmatched}: "7",
		{a, b, "USD", domain.MatchStatusClosed}:    "12",
		{a, b, "USD", domain.MatchStatusMismatch}:  "5000",
	})

	// The assembled answer is what consolidation would read: net per
	// currency, USD and GBP never combined.
	balances, err := domain.BuildReciprocalBalances(a, b, period, groups)
	if err != nil {
		t.Fatalf("BuildReciprocalBalances: %v", err)
	}
	var matched *domain.ReciprocalStatusBalances
	for i := range balances.ByMatchStatus {
		if balances.ByMatchStatus[i].MatchStatus == domain.MatchStatusMatched {
			matched = &balances.ByMatchStatus[i]
		}
	}
	if matched == nil {
		t.Fatalf("no MATCHED slice in %+v", balances.ByMatchStatus)
	}
	wantNet := []domain.ReciprocalCurrencyTotal{
		{CurrencyCode: "GBP", Amount: "50.5"},
		{CurrencyCode: "USD", Amount: "70.3001"},
	}
	if len(matched.Net) != len(wantNet) {
		t.Fatalf("net: expected %+v, got %+v", wantNet, matched.Net)
	}
	for i := range wantNet {
		if matched.Net[i] != wantNet[i] {
			t.Errorf("net[%d]: expected %+v, got %+v", i, wantNet[i], matched.Net[i])
		}
	}

	// A pair with no positions in the period answers an empty list, not nil.
	if empty, err := s.ReciprocalBalances(ctx, b, c, period); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("expected an empty, non-nil result, got %v / %v", empty, err)
	}
}

func TestPgStore_A1_ReciprocalBalances_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	const (
		a      = "le-us"
		b      = "le-uk"
		period = "2026-09"
	)
	inStatus(t, s, ctxA, newBalanceEntry(tenantA, a, b, "USD", "100", period), domain.MatchStatusMatched)
	// The same pair names, under another tenant: never visible to tenant A.
	inStatus(t, s, ctxB, newBalanceEntry(tenantB, a, b, "USD", "5", period), domain.MatchStatusMatched)

	groups, err := s.ReciprocalBalances(ctxA, a, b, period)
	if err != nil {
		t.Fatalf("ReciprocalBalances: %v", err)
	}
	assertGroups(t, groups, map[groupKey]domain.Decimal{
		{a, b, "USD", domain.MatchStatusMatched}: "100",
	})
}

// The sums stay exact where float64 cannot tell amounts apart: the largest
// NUMERIC(18,4) value plus the smallest is an integer beyond the column's
// own 14 integer digits, and the smallest value round-trips as itself.
func TestPgStore_A1_ReciprocalBalances_ExactAtNumericBoundary(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	const (
		a      = "le-us"
		b      = "le-uk"
		period = "2026-09"
	)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "99999999999999.9999", period), domain.MatchStatusMatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, a, b, "USD", "0.0001", period), domain.MatchStatusMatched)
	inStatus(t, s, ctx, newBalanceEntry(tenantID, b, a, "USD", "0.0001", period), domain.MatchStatusMatched)

	groups, err := s.ReciprocalBalances(ctx, a, b, period)
	if err != nil {
		t.Fatalf("ReciprocalBalances: %v", err)
	}
	assertGroups(t, groups, map[groupKey]domain.Decimal{
		{a, b, "USD", domain.MatchStatusMatched}: "100000000000000",
		{b, a, "USD", domain.MatchStatusMatched}: "0.0001",
	})
}
