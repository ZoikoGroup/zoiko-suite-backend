package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JR-DST-1 (R-001 §7.3, R-INV-18): a cutoff that falls in a DST gap or fold
// resolves by the documented rule, never by Go's unspecified choice.

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestResolveLocalInstant_Rules(t *testing.T) {
	london := mustLoc(t, "Europe/London")
	ny := mustLoc(t, "America/New_York")
	cases := []struct {
		name string
		loc  *time.Location
		y    int
		mo   time.Month
		d    int
		h    int
		mi   int
		want time.Time
		rule string
	}{
		// London 2026-03-29: 01:00 GMT -> 02:00 BST at 01:00 UTC. 01:30 does not exist.
		{"london gap", london, 2026, time.March, 29, 1, 30, time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), LocalTimeNonexistent},
		// London 2026-10-25: 02:00 BST -> 01:00 GMT at 01:00 UTC. 01:30 is at 00:30 UTC (BST) and 01:30 UTC (GMT).
		{"london fold", london, 2026, time.October, 25, 1, 30, time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), LocalTimeAmbiguous},
		{"london normal", london, 2026, time.October, 26, 1, 30, time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC), LocalTimeExact},
		// New York 2026-03-08: 02:00 EST -> 03:00 EDT at 07:00 UTC. 02:30 does not exist.
		{"new york gap", ny, 2026, time.March, 8, 2, 30, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), LocalTimeNonexistent},
		// New York 2026-11-01: 02:00 EDT -> 01:00 EST at 06:00 UTC. 01:30 is at 05:30 UTC (EDT) and 06:30 UTC (EST).
		{"new york fold", ny, 2026, time.November, 1, 1, 30, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), LocalTimeAmbiguous},
		{"utc", time.UTC, 2026, time.March, 29, 1, 30, time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC), LocalTimeExact},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rule := ResolveLocalInstant(c.y, c.mo, c.d, c.h, c.mi, 0, 0, c.loc)
			assert.Equal(t, c.want, got.UTC())
			assert.Equal(t, c.rule, rule)
		})
	}
}

func TestDueDate_CutoffInDSTGapAndFold_UsesTheDocumentedRule(t *testing.T) {
	r := vatRule()
	r.BusinessDayAdjustment = AdjustNone
	r.OffsetMonths, r.OffsetDays, r.OffsetToMonthEnd = 0, 0, false
	r.Anchor = AnchorEventDate
	r.PeriodBasis = BasisEventDriven
	r.CutoffApplies = true
	cut := "01:30"

	london := gbCal(1, d(2026, 1, 1))
	london.CutoffTime = &cut
	gap := CalculateDueDate(r, []CalendarVersion{london}, DueFacts{EventDate: "2026-03-29"})
	require.Equal(t, DueCalculated, gap.Outcome, gap.Message)
	require.NotNil(t, gap.DueAt)
	assert.Equal(t, time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC), *gap.DueAt)
	assert.True(t, hasStep(gap.Explanation, LocalTimeNonexistent), gap.Explanation)

	fold := CalculateDueDate(r, []CalendarVersion{london}, DueFacts{EventDate: "2026-10-25"})
	require.Equal(t, DueCalculated, fold.Outcome, fold.Message)
	assert.Equal(t, time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), *fold.DueAt)
	assert.True(t, hasStep(fold.Explanation, LocalTimeAmbiguous), fold.Explanation)

	plain := CalculateDueDate(r, []CalendarVersion{london}, DueFacts{EventDate: "2026-10-26"})
	assert.Equal(t, time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC), *plain.DueAt)
	assert.False(t, hasStep(plain.Explanation, LocalTimeNonexistent) || hasStep(plain.Explanation, LocalTimeAmbiguous))

	ny := london
	ny.Timezone = "America/New_York"
	cutNY := "02:30"
	ny.CutoffTime = &cutNY
	nyGap := CalculateDueDate(r, []CalendarVersion{ny}, DueFacts{EventDate: "2026-03-08"})
	assert.Equal(t, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), *nyGap.DueAt)
	assert.True(t, hasStep(nyGap.Explanation, LocalTimeNonexistent))
	cutNY = "01:30"
	nyFold := CalculateDueDate(r, []CalendarVersion{ny}, DueFacts{EventDate: "2026-11-01"})
	assert.Equal(t, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), *nyFold.DueAt)
	assert.True(t, hasStep(nyFold.Explanation, LocalTimeAmbiguous))
}

func hasStep(steps []string, sub string) bool {
	for _, s := range steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
