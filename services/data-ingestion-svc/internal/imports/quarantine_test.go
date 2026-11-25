package imports

import "testing"

func TestDeriveQuarantineState(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		batch                      Status
		quarantined, total, openRq int
		want                       QuarantineState
	}{
		{"nothing quarantined", StatusImporting, 0, 10, 0, QuarantineNone},
		{"some quarantined", StatusCompletedWithExceptions, 2, 10, 0, QuarantinePartial},
		{"every item quarantined", StatusCompletedWithExceptions, 10, 10, 0, QuarantineFull},
		{"an open release request", StatusCompletedWithExceptions, 2, 10, 1, QuarantineReleasePending},
		{"a request on a fully quarantined batch is still pending a decision", StatusCompletedWithExceptions, 10, 10, 1, QuarantineReleasePending},
		{"a batch quarantined whole", StatusQuarantined, 0, 0, 0, QuarantineFull},
		{"a batch quarantined whole stays FULL whatever else is counted", StatusQuarantined, 3, 10, 2, QuarantineFull},
		{"no items counted", StatusImporting, 0, 0, 0, QuarantineNone},
	} {
		if got := DeriveQuarantineState(tc.batch, tc.quarantined, tc.total, tc.openRq); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The key an item is delivered under: its own for life, except that each RELEASE from quarantine is
// a new attempt after a correction and delivers under a key of its own (see DeliveryKey).
func TestDeliveryKey(t *testing.T) {
	it := Item{IdempotencyKey: "imp-1-row-2-abc"}
	if got := DeliveryKey(it); got != "imp-1-row-2-abc" {
		t.Errorf("a never-released item delivers under its own key, got %q", got)
	}
	it.ReleaseGeneration = 1
	k1 := DeliveryKey(it)
	it.ReleaseGeneration = 2
	k2 := DeliveryKey(it)
	if k1 == "imp-1-row-2-abc" || k1 == k2 || k1 != "imp-1-row-2-abc:rel1" || k2 != "imp-1-row-2-abc:rel2" {
		t.Errorf("each release needs a key of its own: %q, %q", k1, k2)
	}
	// Deterministic: the same release redelivered (a timeout after the domain committed) is recognised.
	if DeliveryKey(it) != k2 {
		t.Error("the key for one release must be stable across redeliveries")
	}
}
