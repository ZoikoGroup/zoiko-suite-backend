package domain

import (
	"testing"
	"time"
)

var (
	dueAt       = time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	dueDayStart = time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	dueSoonAt   = dueAt.Add(-7 * 24 * time.Hour)
)

// OB-S7 / NP-16 / R-INV-26: the state a reader sees is computed from the
// clock's times and now, with no sweep involved.
func TestWFC05_EffectiveState_FrozenClockReadsOverdueWithoutSweep(t *testing.T) {
	cases := []struct {
		name   string
		stored ClockState
		now    time.Time
		want   ClockState
	}{
		{"before warning", ClockStateActive, dueSoonAt.Add(-time.Hour), ClockStateActive},
		{"warning reached", ClockStateActive, dueSoonAt, ClockStateDueSoon},
		{"due day", ClockStateActive, dueDayStart.Add(time.Hour), ClockStateDue},
		{"one second late, sweep never ran", ClockStateActive, dueAt.Add(time.Second), ClockStateOverdue},
		{"a year late", ClockStateDueSoon, dueAt.AddDate(1, 0, 0), ClockStateOverdue},
		{"exactly at due_at is not late", ClockStateDue, dueAt, ClockStateDue},
		{"never backwards", ClockStateDue, dueSoonAt.Add(-time.Hour), ClockStateDue},
		{"fulfilled stays fulfilled", ClockStateFulfilled, dueAt.Add(time.Hour), ClockStateFulfilled},
		{"waived stays waived", ClockStateWaived, dueAt.Add(time.Hour), ClockStateWaived},
		{"planned is not running", ClockStatePlanned, dueAt.Add(time.Hour), ClockStatePlanned},
		{"superseded stays", ClockStateSuperseded, dueAt.Add(time.Hour), ClockStateSuperseded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveState(c.stored, c.now, dueAt, dueDayStart, &dueSoonAt); got != c.want {
				t.Fatalf("EffectiveState(%s, %s) = %s, want %s", c.stored, c.now, got, c.want)
			}
		})
	}
	if got := EffectiveState(ClockStateActive, dueSoonAt.Add(time.Hour), dueAt, dueDayStart, nil); got != ClockStateActive {
		t.Fatalf("no warning instant: want ACTIVE, got %s", got)
	}
}

func TestWFC05_DueDayStart_UsesTheClockZone(t *testing.T) {
	c := Clock{Timezone: "Europe/London", LocalDueDate: "2026-08-31", DueAt: time.Date(2026, 8, 31, 22, 59, 59, 0, time.UTC)}
	if got, want := c.DueDayStart(), time.Date(2026, 8, 30, 23, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("London BST due day starts %s, want %s", got, want)
	}
	bad := Clock{Timezone: "Mars/Olympus", DueAt: dueAt}
	if got := bad.DueDayStart(); !got.Equal(dueAt.Add(-24 * time.Hour)) {
		t.Fatalf("fallback: %s", got)
	}
}

// ST-3: commands are named and gated on the effective state.
func TestWFC05_ClockCommandAllowed(t *testing.T) {
	allowed := map[string][]ClockState{
		ClockCommandActivate:      {ClockStatePlanned},
		ClockCommandSatisfy:       {ClockStateActive, ClockStateDueSoon, ClockStateDue, ClockStateOverdue},
		ClockCommandWaive:         {ClockStatePlanned, ClockStateActive, ClockStateDueSoon, ClockStateDue, ClockStateOverdue},
		ClockCommandNotApplicable: {ClockStatePlanned, ClockStateActive, ClockStateDueSoon, ClockStateDue, ClockStateOverdue},
		ClockCommandRecalculate:   {ClockStatePlanned, ClockStateActive, ClockStateDueSoon, ClockStateDue, ClockStateOverdue},
	}
	for cmd, states := range allowed {
		ok := map[ClockState]bool{}
		for _, s := range states {
			ok[s] = true
		}
		for s := range ClockStatuses {
			if got := ClockCommandAllowed(cmd, s); got != ok[s] {
				t.Errorf("%s on %s: got %v, want %v", cmd, s, got, ok[s])
			}
		}
	}
	if ClockCommandAllowed("set-overdue", ClockStateActive) {
		t.Fatal("an unnamed command must never be allowed")
	}
	if ClockCommandAllowed(ClockCommandSatisfy, ClockStatePlanned) {
		t.Fatal("R-001 s7.2: a PLANNED clock cannot be treated as fulfilled")
	}
}

// R-INV-07: an approval is bound to the clock, the action, the reason and the
// evidence; the order of the evidence does not matter.
func TestWFC05_AuthoritySubjectFingerprint(t *testing.T) {
	a := AuthoritySubjectFingerprint("c-1", "WAIVE", "COURT_ORDER", []string{"DOC:2", "DOC:1"})
	b := AuthoritySubjectFingerprint("c-1", "WAIVE", "COURT_ORDER", []string{"DOC:1", "DOC:2"})
	if a != b {
		t.Fatal("evidence order changed the fingerprint")
	}
	for name, other := range map[string]string{
		"clock":    AuthoritySubjectFingerprint("c-2", "WAIVE", "COURT_ORDER", []string{"DOC:1", "DOC:2"}),
		"action":   AuthoritySubjectFingerprint("c-1", "NOT_APPLICABLE", "COURT_ORDER", []string{"DOC:1", "DOC:2"}),
		"reason":   AuthoritySubjectFingerprint("c-1", "WAIVE", "OTHER", []string{"DOC:1", "DOC:2"}),
		"evidence": AuthoritySubjectFingerprint("c-1", "WAIVE", "COURT_ORDER", []string{"DOC:1"}),
	} {
		if other == a {
			t.Errorf("a different %s gave the same fingerprint", name)
		}
	}
	if len(a) != len("sha256:")+64 || a[:7] != "sha256:" {
		t.Fatalf("fingerprint shape %q (workflow-svc accepts sha256:<64 hex>)", a)
	}
}

func TestWFC05_ValidMilestone(t *testing.T) {
	good := []MilestoneSpec{
		{Kind: MilestoneWarning, OffsetSeconds: -86400, Action: ActionMarkDueSoon},
		{Kind: MilestoneEscalation, OffsetSeconds: 3600, Action: ActionOpenCase},
		{Kind: MilestoneEscalation, OffsetSeconds: 0, Action: ActionRaisePriority, Params: map[string]any{"priority": "CRITICAL"}},
		{Kind: MilestoneEscalation, OffsetSeconds: 60, Action: ActionAddOwner, Params: map[string]any{"owner": "tax-lead"}},
	}
	for _, m := range good {
		if why := ValidMilestone(m); why != "" {
			t.Errorf("%+v refused: %s", m, why)
		}
	}
	bad := []MilestoneSpec{
		{Kind: "PAGE", Action: ActionNotify},
		{Kind: MilestoneEscalation, Action: "CLOSE_OBLIGATION"},
		{Kind: MilestoneEscalation, OffsetSeconds: -1, Action: ActionMarkDueSoon},
		{Kind: MilestoneWarning, OffsetSeconds: 10, Action: ActionMarkDueSoon},
		{Kind: MilestoneEscalation, Action: ActionAddOwner},
		{Kind: MilestoneEscalation, Action: ActionRaisePriority, Params: map[string]any{"priority": "LOW"}},
		{Kind: MilestoneEscalation, OffsetSeconds: 400 * 86400, Action: ActionNotify},
	}
	for _, m := range bad {
		if ValidMilestone(m) == "" {
			t.Errorf("%+v accepted", m)
		}
	}
}
