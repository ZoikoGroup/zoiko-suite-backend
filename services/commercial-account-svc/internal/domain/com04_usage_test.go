package domain_test

import (
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func sumMeter() domain.MeterDefinition {
	return domain.MeterDefinition{MeterKey: "api.calls", MeterVersion: 1, DisplayName: "API calls", Unit: "call", AggregationMethod: "SUM"}
}

func uniqueMeter(dim string) domain.MeterDefinition {
	return domain.MeterDefinition{MeterKey: "active.users", MeterVersion: 1, DisplayName: "Active users", Unit: "user",
		AggregationMethod: "UNIQUE_COUNT", UniqueDimension: &dim}
}

func TestValidateMeterDefinition(t *testing.T) {
	ok := sumMeter()
	if err := domain.ValidateMeterDefinition(&ok); err != nil {
		t.Fatalf("a well-formed SUM meter was refused: %v", err)
	}
	dim := "user_id"
	u := uniqueMeter(dim)
	if err := domain.ValidateMeterDefinition(&u); err != nil {
		t.Fatalf("a well-formed UNIQUE_COUNT meter was refused: %v", err)
	}

	bad := sumMeter()
	bad.UniqueDimension = &dim
	if err := domain.ValidateMeterDefinition(&bad); err == nil {
		t.Fatal("a SUM meter with a unique_dimension was accepted")
	}
	missing := domain.MeterDefinition{MeterKey: "active.users", DisplayName: "x", Unit: "user", AggregationMethod: "UNIQUE_COUNT"}
	if err := domain.ValidateMeterDefinition(&missing); err == nil {
		t.Fatal("a UNIQUE_COUNT meter with no unique_dimension was accepted")
	}
	badKey := sumMeter()
	badKey.MeterKey = "API.Calls"
	if err := domain.ValidateMeterDefinition(&badKey); err == nil {
		t.Fatal("an uppercase meter key was accepted")
	}
}

// Negative path #16: negative or malformed quantities are refused before
// they ever reach aggregation.
func TestValidateUsageQuantity_RejectsInvalidInputs(t *testing.T) {
	m := sumMeter()
	if r := domain.ValidateUsageQuantity(m, domain.EventInput{Quantity: "5"}); r != "" {
		t.Fatalf("a valid quantity was quarantined: %q", r)
	}
	for _, q := range []string{"-1", "-0.5", "not-a-number", "", "1e3"} {
		if r := domain.ValidateUsageQuantity(m, domain.EventInput{Quantity: q}); r == "" {
			t.Errorf("quantity %q was accepted", q)
		}
	}
}

func TestValidateUsageQuantity_UniqueCountMustBeOneWithItsDimension(t *testing.T) {
	dim := "user_id"
	m := uniqueMeter(dim)
	if r := domain.ValidateUsageQuantity(m, domain.EventInput{Quantity: "1", Dimensions: map[string]string{"user_id": "u1"}}); r != "" {
		t.Fatalf("a valid UNIQUE_COUNT event was quarantined: %q", r)
	}
	if r := domain.ValidateUsageQuantity(m, domain.EventInput{Quantity: "2", Dimensions: map[string]string{"user_id": "u1"}}); r == "" {
		t.Fatal("a UNIQUE_COUNT event with quantity != 1 was accepted")
	}
	if r := domain.ValidateUsageQuantity(m, domain.EventInput{Quantity: "1", Dimensions: map[string]string{"other": "x"}}); r == "" {
		t.Fatal("a UNIQUE_COUNT event missing its declared dimension was accepted")
	}
}

func TestAggregate_SUM(t *testing.T) {
	events := []domain.AcceptedEvent{{Quantity: "3.5", EventID: "a"}, {Quantity: "2.25", EventID: "b"}, {Quantity: "0.001", EventID: "c"}}
	if got := domain.Aggregate(sumMeter(), events); got != "5.7510" {
		t.Fatalf("SUM = %s, want 5.7510", got)
	}
	if got := domain.Aggregate(sumMeter(), nil); got != "0" {
		t.Fatalf("SUM of no events = %s, want 0", got)
	}
}

func TestAggregate_MAX(t *testing.T) {
	m := sumMeter()
	m.AggregationMethod = "MAX"
	events := []domain.AcceptedEvent{{Quantity: "3.5"}, {Quantity: "9.2"}, {Quantity: "1.0"}}
	if got := domain.Aggregate(m, events); got != "9.2" {
		t.Fatalf("MAX = %s, want 9.2", got)
	}
}

// LAST breaks ties on occurred_at by event ID, deterministically, so the
// same input set always aggregates to the same answer.
func TestAggregate_LAST(t *testing.T) {
	m := sumMeter()
	m.AggregationMethod = "LAST"
	t1 := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	events := []domain.AcceptedEvent{
		{Quantity: "1", OccurredAt: t1, EventID: "b"},
		{Quantity: "2", OccurredAt: t2, EventID: "a"},
		{Quantity: "3", OccurredAt: t2, EventID: "z"}, // same instant as the row above; "z" > "a" wins
	}
	if got := domain.Aggregate(m, events); got != "3" {
		t.Fatalf("LAST = %s, want 3 (latest occurred_at, tie broken by event id)", got)
	}
}

func TestAggregate_UniqueCount(t *testing.T) {
	dim := "user_id"
	m := uniqueMeter(dim)
	events := []domain.AcceptedEvent{
		{Dimensions: map[string]string{"user_id": "u1"}}, {Dimensions: map[string]string{"user_id": "u2"}},
		{Dimensions: map[string]string{"user_id": "u1"}}, {Dimensions: map[string]string{"user_id": "u3"}},
	}
	if got := domain.Aggregate(m, events); got != "3" {
		t.Fatalf("UNIQUE_COUNT = %s, want 3 distinct users", got)
	}
}

// Reconciliation (§4.4): the same accepted rows under the same meter
// version reproduce the identical total, whatever order they're given in.
func TestAggregate_IsReproducibleAndOrderIndependent(t *testing.T) {
	m := sumMeter()
	a := []domain.AcceptedEvent{{Quantity: "1.1"}, {Quantity: "2.2"}, {Quantity: "3.3"}}
	b := []domain.AcceptedEvent{a[2], a[0], a[1]}
	if x, y := domain.Aggregate(m, a), domain.Aggregate(m, b); x != y {
		t.Fatalf("aggregation depended on input order: %s vs %s", x, y)
	}
}
