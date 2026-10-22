package domain_test

// SPEC_DEVIATIONS A1/B10: BuildReciprocalBalances — exact, per-currency
// aggregation of a pair's intercompany positions.

import (
	"strings"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func TestBuildReciprocalBalances_ExactPerCurrencyAndStatus(t *testing.T) {
	got, err := domain.BuildReciprocalBalances("le-a", "le-b", "2026-09", []domain.ReciprocalBalanceGroup{
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "100.0001"},
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "0.0001"},
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "GBP", Amount: "50"},
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-b", TargetLegalEntityID: "le-a", CurrencyCode: "USD", Amount: "30"},
		{MatchStatus: "UNMATCHED", SourceLegalEntityID: "le-b", TargetLegalEntityID: "le-a", CurrencyCode: "GBP", Amount: "9.5"},
		{MatchStatus: "CLOSED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "12"},
		// A third entity's position is not this pair's and must not be added.
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-c", CurrencyCode: "USD", Amount: "999"},
	})
	if err != nil {
		t.Fatalf("BuildReciprocalBalances: %v", err)
	}
	if got.EntityA != "le-a" || got.EntityB != "le-b" || got.FiscalPeriod != "2026-09" {
		t.Fatalf("unexpected echo: %+v", got)
	}
	if len(got.ByMatchStatus) != 3 {
		t.Fatalf("expected UNMATCHED, MATCHED, CLOSED; got %+v", got.ByMatchStatus)
	}
	if got.ByMatchStatus[0].MatchStatus != domain.MatchStatusUnmatched ||
		got.ByMatchStatus[1].MatchStatus != domain.MatchStatusMatched ||
		got.ByMatchStatus[2].MatchStatus != domain.MatchStatusClosed {
		t.Fatalf("statuses not in state-model order: %+v", got.ByMatchStatus)
	}

	matched := got.ByMatchStatus[1]
	// 100.0001 + 0.0001 exactly; nothing co-mingled with GBP.
	assertTotals(t, "MATCHED A→B", matched.EntityAToEntityB,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "50"},
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "100.0002"})
	assertTotals(t, "MATCHED B→A", matched.EntityBToEntityA,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "30"})
	assertTotals(t, "MATCHED net", matched.Net,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "50"},
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "70.0002"})

	unmatched := got.ByMatchStatus[0]
	assertTotals(t, "UNMATCHED A→B", unmatched.EntityAToEntityB)
	assertTotals(t, "UNMATCHED B→A", unmatched.EntityBToEntityA,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "9.5"})
	// A currency in one direction only still gets a signed net.
	assertTotals(t, "UNMATCHED net", unmatched.Net,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "GBP", Amount: "-9.5"})
}

// A total is exact even where float64 cannot tell two amounts apart: the sum
// of the largest NUMERIC(18,4) value and the smallest is an integer beyond
// NUMERIC(18,4)'s own 14 integer digits, and still comes back in full.
func TestBuildReciprocalBalances_ExactAtNumericBoundaries(t *testing.T) {
	got, err := domain.BuildReciprocalBalances("le-a", "le-b", "2026-09", []domain.ReciprocalBalanceGroup{
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "99999999999999.9999"},
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "0.0001"},
	})
	if err != nil {
		t.Fatalf("BuildReciprocalBalances: %v", err)
	}
	assertTotals(t, "MATCHED A→B", got.ByMatchStatus[0].EntityAToEntityB,
		domain.ReciprocalCurrencyTotal{CurrencyCode: "USD", Amount: "100000000000000"})
}

// A total that no amount could render exactly (here a rational big.Rat would
// hold, but no decimal text can) is refused, never rounded to 12 places.
func TestBuildReciprocalBalances_RefusesUnrenderableAmount(t *testing.T) {
	_, err := domain.BuildReciprocalBalances("le-a", "le-b", "2026-09", []domain.ReciprocalBalanceGroup{
		{MatchStatus: "MATCHED", SourceLegalEntityID: "le-a", TargetLegalEntityID: "le-b", CurrencyCode: "USD", Amount: "1/3"},
	})
	if err == nil || !strings.Contains(err.Error(), "exact decimal") {
		t.Fatalf("expected an exactness refusal, got %v", err)
	}
}

func assertTotals(t *testing.T, what string, got []domain.ReciprocalCurrencyTotal, want ...domain.ReciprocalCurrencyTotal) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: expected %+v, got %+v", what, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: expected %+v, got %+v", what, i, want[i], got[i])
		}
	}
}
