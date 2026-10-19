package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestToMinorUnits_ExactOrRefused(t *testing.T) {
	cases := []struct {
		amount string
		scale  int
		want   int64
		err    error
	}{
		{"12.34", 2, 1234, nil},
		{"12.3", 2, 1230, nil},
		{"-0.05", 2, -5, nil},
		{"100", 0, 100, nil},
		{"1.000", 2, 100, nil}, // trailing zeros are not extra precision
		{"12.345", 2, 0, ErrAmountNotRepresentable},
		{"0.5", 0, 0, ErrAmountNotRepresentable},
		{"92233720368547758.08", 2, 0, ErrAmountOverflow},
		{"1.5e2", 2, 15000, nil},
		{"abc", 2, 0, ErrInvalidArgument},
	}
	for _, c := range cases {
		got, err := ToMinorUnits(c.amount, c.scale)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Fatalf("%s@%d: got %v, want %v", c.amount, c.scale, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%s@%d: got %d, %v; want %d", c.amount, c.scale, got, err, c.want)
		}
	}
}

// A14: a side's total that does not fit an int64 is refused, never wrapped.
func TestControlTotals_RefusesOverflow(t *testing.T) {
	items := []PopulationItemInput{
		{Currency: "USD", AmountMinorUnits: math.MaxInt64 - 10},
		{Currency: "USD", AmountMinorUnits: 11},
	}
	if _, _, err := ControlTotals(items); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("positive overflow: %v", err)
	}
	items = []PopulationItemInput{
		{Currency: "USD", AmountMinorUnits: math.MinInt64 + 1},
		{Currency: "USD", AmountMinorUnits: -2},
	}
	if _, _, err := ControlTotals(items); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("negative overflow: %v", err)
	}
	// Two currencies that each fit but whose grand total does not.
	items = []PopulationItemInput{
		{Currency: "USD", AmountMinorUnits: math.MaxInt64 - 1},
		{Currency: "EUR", AmountMinorUnits: 5},
	}
	if _, _, err := ControlTotals(items); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("grand total overflow: %v", err)
	}
}

func TestControlTotals_PerCurrency(t *testing.T) {
	per, total, err := ControlTotals([]PopulationItemInput{
		{Currency: "USD", AmountMinorUnits: 100}, {Currency: "EUR", AmountMinorUnits: 7}, {Currency: "USD", AmountMinorUnits: -40},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 67 || len(per) != 2 || per[0] != (CurrencyTotal{"EUR", 1, 7}) || per[1] != (CurrencyTotal{"USD", 2, 60}) {
		t.Fatalf("got %+v total %d", per, total)
	}
}

func rec(id, ref, amt string) PopulationRecord {
	return PopulationRecord{RecordID: id, Reference: ref, Amount: amt, Currency: "USD", Date: "2026-09-01"}
}

func TestNewFrozenPopulation(t *testing.T) {
	sp := PopulationSpec{Side: "Bank", System: "banking", Population: "bank-transactions", Params: map[string]string{"bank_account_id": "b1"}}
	fp, err := NewFrozenPopulation(sp, "wm-1", 2, map[string]string{"USD": "3.5"},
		[]PopulationRecord{rec("r1", "REF-1", "1.25"), rec("r2", "REF-2", "2.25")}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fp.SpecRef != "banking/bank-transactions?bank_account_id=b1" || fp.Watermark != "wm-1" || len(fp.Items) != 2 {
		t.Fatalf("population: %+v", fp)
	}
	it := fp.Items[0]
	if it.SourceRecordID != "r1" || it.RefID != "REF-1" || it.AmountMinorUnits != 125 || it.Currency != "USD" ||
		!it.OccurredAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("item: %+v", it)
	}

	// The source's stable identity repeated is a broken population, not two records.
	if _, err := NewFrozenPopulation(sp, "wm", 2, nil, []PopulationRecord{rec("r1", "A", "1"), rec("r1", "B", "1")}, 2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate record_id: %v", err)
	}
	// More precision than the definition's scale is refused, not rounded.
	if _, err := NewFrozenPopulation(sp, "wm", 1, nil, []PopulationRecord{rec("r1", "A", "1.001")}, 2); !errors.Is(err, ErrAmountNotRepresentable) {
		t.Fatalf("excess precision: %v", err)
	}
}

func TestHashPopulation_OrderIndependentAndSensitive(t *testing.T) {
	a := PopulationItemInput{SourceRecordID: "r1", RefID: "X", AmountMinorUnits: 1, Currency: "USD", Dimensions: map[string]string{"k": "v"}}
	b := PopulationItemInput{SourceRecordID: "r2", RefID: "Y", AmountMinorUnits: 2, Currency: "USD"}
	h1 := HashPopulation([]PopulationItemInput{a, b})
	if h1 != HashPopulation([]PopulationItemInput{b, a}) {
		t.Fatal("hash depends on the order the source paged records in")
	}
	changed := a
	changed.Dimensions = map[string]string{"k": "w"}
	if h1 == HashPopulation([]PopulationItemInput{changed, b}) {
		t.Fatal("hash ignores a changed attribute")
	}
	if h1 == HashPopulation([]PopulationItemInput{a}) {
		t.Fatal("hash ignores a removed record")
	}
}

func TestStartRunRequest_Validate(t *testing.T) {
	if err := (StartRunRequest{DefinitionID: "d", PeriodID: "2026-13"}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad period: %v", err)
	}
	if err := (StartRunRequest{DefinitionID: "d", Scope: map[string]string{"cursor": "x"}}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reserved scope key: %v", err)
	}
	if err := (StartRunRequest{DefinitionID: "d", PeriodID: "2026-09", Scope: map[string]string{"bank_account_id": "b1"}}).Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
}
