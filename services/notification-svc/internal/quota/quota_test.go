package quota

import (
	"errors"
	"testing"
	"time"
)

func find(bs []Budget, dim string) *Budget {
	for i := range bs {
		if bs[i].Dimension == dim {
			return &bs[i]
		}
	}
	return nil
}

func TestBudgetsApplyToTenantRecipientAndIntent(t *testing.T) {
	bs := Budgets(DefaultLimits, Request{Class: "T0", Channel: "email", RecipientID: "p1", IntentID: "int-1"})
	if len(bs) != 3 {
		t.Fatalf("want three budgets, got %+v", bs)
	}
	if b := find(bs, DimTenant); b.Limit != 600 || b.Window != time.Minute || b.Bucket != "tenant:EMAIL:general" {
		t.Errorf("tenant budget: %+v", b)
	}
	if b := find(bs, DimRecipient); b.Limit != 20 || b.Window != time.Hour {
		t.Errorf("recipient budget: %+v", b)
	}
	if b := find(bs, DimIntent); b.Limit != 300 || b.Bucket != "intent:general:int-1" {
		t.Errorf("intent budget: %+v", b)
	}
}

// Security messages draw on their own pool, so nothing else can use up their capacity, yet
// they stay bounded.
func TestSecurityMessagesHaveProtectedButBoundedCapacity(t *testing.T) {
	general := Budgets(DefaultLimits, Request{Class: "A1", Channel: "EMAIL", RecipientID: "p1"})
	security := Budgets(DefaultLimits, Request{Class: "S0", Channel: "EMAIL", RecipientID: "p1"})

	g, s := find(general, DimTenant), find(security, DimTenant)
	if g.Bucket == s.Bucket {
		t.Fatal("S0 must not share a counter with other classes, or a flood could starve it")
	}
	if s.Limit != DefaultLimits.TenantS0PerMinute || s.Limit == 0 {
		t.Errorf("the protected pool is still bounded: %+v", s)
	}
	if find(security, DimRecipient).Limit <= find(general, DimRecipient).Limit {
		t.Error("a security notice gets a higher per-person allowance than a routine one")
	}
	if find(security, DimRecipient).Bucket == find(general, DimRecipient).Bucket {
		t.Error("a person's security allowance is separate from their routine one")
	}
	for _, class := range []string{"T0", "A1", ""} {
		if Protected(class) {
			t.Errorf("%q must not use the protected pool", class)
		}
	}
}

func TestZeroLimitsAndMissingScopesOmitBudgets(t *testing.T) {
	if bs := Budgets(Limits{}, Request{Class: "T0", Channel: "EMAIL", RecipientID: "p1", IntentID: "i"}); len(bs) != 0 {
		t.Errorf("no limits, no budgets: %+v", bs)
	}
	bs := Budgets(DefaultLimits, Request{Class: "T0", Channel: "EMAIL"})
	if len(bs) != 1 || bs[0].Dimension != DimTenant {
		t.Errorf("no recipient and no intent leaves only the tenant budget: %+v", bs)
	}
	if find(Budgets(DefaultLimits, Request{Class: "T0", Channel: "EMAIL", RecipientID: "a"}), DimRecipient).Bucket ==
		find(Budgets(DefaultLimits, Request{Class: "T0", Channel: "EMAIL", RecipientID: "b"}), DimRecipient).Bucket {
		t.Error("each recipient has their own counter")
	}
}

func TestWindows(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 30, 45, 0, time.UTC)
	if got := WindowStart(at, time.Minute); !got.Equal(time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("minute window start = %v", got)
	}
	if got := WindowStart(at, time.Hour); !got.Equal(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("hour window start = %v", got)
	}
	if got := RetryAfter(at, time.Minute); got != 15*time.Second {
		t.Errorf("retry after = %v, want 15s", got)
	}
	// Right at the end of a window the caller is still told to wait at least a second.
	if got := RetryAfter(time.Date(2026, 10, 6, 9, 30, 59, 999_000_000, time.UTC), time.Minute); got != time.Second {
		t.Errorf("retry after the very end of a window = %v, want 1s", got)
	}
}

func TestExceededErrorSaysWhichBudgetAndWhen(t *testing.T) {
	var err error = &ExceededError{Dimension: DimRecipient, Limit: 20, Window: time.Hour, RetryAfter: 90 * time.Second}
	var ex *ExceededError
	if !errors.As(err, &ex) || ex.Dimension != DimRecipient {
		t.Fatal("must be recoverable with errors.As")
	}
	msg := err.Error()
	for _, want := range []string{"recipient", "20", "1h0m0s", "90 seconds"} {
		if !contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
