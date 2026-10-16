package domain

import (
	"errors"
	"testing"
	"time"
)

func pop(side string, n int) PopulationInput {
	p := PopulationInput{Side: side, SourceSystem: side}
	for i := 0; i < n; i++ {
		p.Items = append(p.Items, PopulationItemInput{RefID: "REF", AmountMinorUnits: 100, OccurredAt: time.Unix(0, 0)})
	}
	return p
}

// The matcher compares two sides only; a third population would be frozen but
// never reconciled, so it is refused rather than silently ignored.
func TestStartRunRequest_RequiresExactlyTwoPopulations(t *testing.T) {
	for name, pops := range map[string][]PopulationInput{
		"one":   {pop("A", 1)},
		"three": {pop("A", 1), pop("B", 1), pop("C", 1)},
	} {
		if err := (StartRunRequest{DefinitionID: "d", Populations: pops}).Validate(); err == nil {
			t.Fatalf("%s population(s) should be refused", name)
		}
	}
	if err := (StartRunRequest{DefinitionID: "d", Populations: []PopulationInput{pop("A", 1), pop("B", 0)}}).Validate(); err != nil {
		t.Fatalf("two populations (one empty) should be accepted: %v", err)
	}
}

// Scenario 06: a manual match with an amount difference beyond the pinned
// tolerance stays an exception; the reason text cannot force it through.
func TestValidateManualMatch(t *testing.T) {
	cases := []struct {
		name         string
		sideA, sideB string
		a, b, tol    int64
		want         error
	}{
		{"equal amounts", "Bank", "Ledger", 10000, 10000, 0, nil},
		{"within tolerance", "Bank", "Ledger", 10000, 10050, 50, nil},
		{"negative difference within tolerance", "Bank", "Ledger", 10050, 10000, 50, nil},
		{"outside tolerance", "Bank", "Ledger", 10000, 10051, 50, ErrManualMatchOutsideTolerance},
		{"outside zero tolerance", "Bank", "Ledger", 10000, 9999, 0, ErrManualMatchOutsideTolerance},
		{"same side", "Bank", "Bank", 10000, 10000, 0, ErrManualMatchSameSide},
	}
	for _, c := range cases {
		err := ValidateManualMatch(c.sideA, c.sideB, c.a, c.b, c.tol)
		if c.want == nil && err != nil {
			t.Fatalf("%s: unexpected error %v", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
