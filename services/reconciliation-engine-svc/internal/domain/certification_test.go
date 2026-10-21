package domain

import (
	"testing"
	"time"
)

// A12: a manual match's evidence carries the figures Compare checked — the
// percentage of the declared base side and the calendar-day difference — for
// a pair ValidateManualMatch accepted.
func TestNewManualMatchEvidence_RecordsTheComparison(t *testing.T) {
	bps, days := int64(100), 2
	p := TolerancePolicy{Currencies: map[string]CurrencyTolerance{"USD": {PercentageBps: &bps}}, PercentageBaseSide: "Ledger", DateDays: &days}
	day := time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC)
	a := MatchItem{ItemID: "rpi_a", Side: "Bank", SourceRecordID: "b-1", RefID: "X", Amount: 2009, Currency: "USD", OccurredAt: day}
	b := MatchItem{ItemID: "rpi_b", Side: "Ledger", SourceRecordID: "g-1", RefID: "Y", Amount: 2000, Currency: "USD",
		OccurredAt: day.Add(25 * time.Hour)}
	if err := ValidateManualMatch(a, b, p); err != nil {
		t.Fatalf("pair should be accepted: %v", err)
	}
	ev := NewManualMatchEvidence(a, b, 3, p)
	if ev.Generation != 3 || !ev.KeyOverridden || ev.DifferenceMinorUnits != 9 || ev.AllowedMinorUnits != 20 ||
		ev.DateDifferenceDays == nil || *ev.DateDifferenceDays != 2 || ev.AllowedDays == nil || *ev.AllowedDays != 2 ||
		ev.ItemA.SourceRecordID != "b-1" || ev.ItemB.ItemID != "rpi_b" || ev.Currency != "USD" {
		t.Fatalf("evidence: %+v", ev)
	}

	// Without a date tolerance, dates are not part of the comparison, and the
	// evidence does not claim they were.
	p.DateDays = nil
	if ev := NewManualMatchEvidence(a, b, 1, p); ev.DateDifferenceDays != nil || ev.AllowedDays != nil {
		t.Fatalf("date figures without a date tolerance: %+v", ev)
	}
}

func TestAuthoritySnapshot_Traceable(t *testing.T) {
	ok := &AuthoritySnapshot{PrincipalID: "carol", AccessDecisionID: "adl_1", DecisionOutcome: "GRANTED"}
	if !ok.Traceable("carol") || ok.Traceable("dave") {
		t.Fatal("traceable only for its own principal")
	}
	var none *AuthoritySnapshot
	noID := *ok
	noID.AccessDecisionID = ""
	denied := *ok
	denied.DecisionOutcome = "DENIED"
	for _, a := range []*AuthoritySnapshot{none, &noID, &denied} {
		if a.Traceable("carol") {
			t.Fatalf("traceable: %+v", a)
		}
	}
}
