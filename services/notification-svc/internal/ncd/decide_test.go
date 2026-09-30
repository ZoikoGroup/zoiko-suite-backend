package ncd

import (
	"testing"
	"time"
)

// TC-08: quiet hours are civil time. Across the UK spring-forward (29 Mar 2026,
// 01:00 GMT -> 02:00 BST) a 22:00–07:00 window ends at 07:00 LOCAL, which is
// 06:00 UTC after the change and would be 07:00 UTC under a fixed offset.
func TestQuietWindowEnd_FollowsDST(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	now := time.Date(2026, 3, 28, 23, 30, 0, 0, time.UTC) // 23:30 GMT, inside the window
	end := QuietWindowEnd(now, london, "22:00", "07:00")
	if end == nil || !end.Equal(time.Date(2026, 3, 29, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("window end = %v, want 2026-03-29T06:00Z (07:00 BST)", end)
	}
	if QuietWindowEnd(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC), london, "22:00", "07:00") != nil {
		t.Fatal("midday is outside the window")
	}
}

func TestStateMachineMatchesSpec(t *testing.T) {
	if CanTransition(AttemptUnknown, AttemptSubmitting) || CanTransition(AttemptDelivered, AttemptAccepted) || CanTransition(AttemptFailed, AttemptAccepted) {
		t.Fatal("§6.2: no backwards or post-terminal transitions")
	}
	if !CanTransition(AttemptSubmitting, AttemptDelivered) || !CanTransition(AttemptUnknown, AttemptFailed) {
		t.Fatal("§6.2: callback-first delivery and UNKNOWN resolution must be legal")
	}
	for _, s := range []string{AttemptCreated, AttemptSubmitting, AttemptAccepted, AttemptPending, AttemptUnknown, AttemptDelivered, AttemptFailed, AttemptBounced} {
		if DeliveryState(ChannelEmail, s) == "SENT" {
			t.Fatal("§3.3: no state is ever exposed as SENT")
		}
	}
}
