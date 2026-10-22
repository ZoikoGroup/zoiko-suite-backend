package domain_test

import (
	"strings"
	"testing"

	"zoiko.io/consolidation-svc/internal/domain"
)

func balances(statuses ...domain.ReciprocalStatusBalances) *domain.ReciprocalBalances {
	return &domain.ReciprocalBalances{
		EntityA: "le-a", EntityB: "le-b", FiscalPeriod: "2026-07",
		ByMatchStatus: statuses,
	}
}

func status(name string, usd domain.Decimal) domain.ReciprocalStatusBalances {
	return domain.ReciprocalStatusBalances{
		MatchStatus: name,
		EntityAToEntityB: []domain.ReciprocalCurrencyTotal{
			{CurrencyCode: "USD", Amount: usd},
			{CurrencyCode: "GBP", Amount: "17.25"},
		},
		EntityBToEntityA: []domain.ReciprocalCurrencyTotal{},
		Net: []domain.ReciprocalCurrencyTotal{
			{CurrencyCode: "USD", Amount: usd},
		},
	}
}

func TestBuildEliminationExplanation_Matched(t *testing.T) {
	rb := balances(status("MATCHED", "1234.5678"))
	got := domain.BuildEliminationExplanation("group-1", "le-a", "le-b", "2026-07", rb)

	if !got.Eliminable {
		t.Fatalf("MATCHED must be eliminable: %+v", got)
	}
	if got.Rule != domain.EliminationRuleMatchedOrClosed {
		t.Errorf("rule = %q", got.Rule)
	}
	if len(got.MatchedStatuses) != 1 || got.MatchedStatuses[0] != "MATCHED" {
		t.Errorf("matched statuses = %v", got.MatchedStatuses)
	}
	if !strings.Contains(got.Reason, "eliminable") {
		t.Errorf("reason = %q", got.Reason)
	}
	if got.ReciprocalBalance != rb {
		t.Error("the reciprocal balance it was checked against must be returned, not rebuilt")
	}
	// Currencies and exact digits survive untouched.
	if got.ReciprocalBalance.ByMatchStatus[0].EntityAToEntityB[0].Amount != "1234.5678" {
		t.Errorf("USD amount changed: %+v", got.ReciprocalBalance.ByMatchStatus[0])
	}
	if got.ReciprocalBalance.ByMatchStatus[0].EntityAToEntityB[1].Amount != "17.25" {
		t.Errorf("GBP amount changed: %+v", got.ReciprocalBalance.ByMatchStatus[0])
	}
}

func TestBuildEliminationExplanation_Closed(t *testing.T) {
	got := domain.BuildEliminationExplanation("group-1", "le-a", "le-b", "2026-07",
		balances(status("CLOSED", "10")))
	if !got.Eliminable || len(got.MatchedStatuses) != 1 || got.MatchedStatuses[0] != "CLOSED" {
		t.Fatalf("CLOSED must be eliminable: %+v", got)
	}
}

func TestBuildEliminationExplanation_Unmatched(t *testing.T) {
	got := domain.BuildEliminationExplanation("group-1", "le-a", "le-b", "2026-07",
		balances(status("UNMATCHED", "10"), status("DISPUTED", "4")))
	if got.Eliminable {
		t.Fatalf("UNMATCHED/DISPUTED must not be eliminable: %+v", got)
	}
	if len(got.MatchedStatuses) != 0 {
		t.Errorf("matched statuses = %v", got.MatchedStatuses)
	}
	if !strings.Contains(got.Reason, "UNMATCHED") || !strings.Contains(got.Reason, "DISPUTED") {
		t.Errorf("reason must name the statuses present: %q", got.Reason)
	}
}

func TestBuildEliminationExplanation_NoEntries(t *testing.T) {
	got := domain.BuildEliminationExplanation("group-1", "le-a", "le-b", "2026-07",
		balances())
	if got.Eliminable {
		t.Fatal("an empty balance must not be eliminable")
	}
	if got.MatchedStatuses == nil {
		t.Error("matched_statuses must be [] in JSON, not null")
	}
	if !strings.Contains(got.Reason, "no intercompany entries") {
		t.Errorf("reason = %q", got.Reason)
	}
}

func TestBuildEliminationExplanation_NilBalance(t *testing.T) {
	got := domain.BuildEliminationExplanation("group-1", "le-a", "le-b", "2026-07", nil)
	if got.Eliminable || got.ReciprocalBalance != nil {
		t.Fatalf("nil balance: %+v", got)
	}
}
