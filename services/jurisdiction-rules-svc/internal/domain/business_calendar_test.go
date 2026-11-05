package domain

import (
	"errors"
	"strings"
	"testing"
)

// REF-10 business-day logic, against fixed golden cases: every date below was
// worked out by hand from the 2026 calendar, and the answers must never move.

// gb is a Saturday-Sunday calendar with the 2026 holidays of England, the
// weekend ones observed on the next weekday (the substitute day).
func gb() CalendarVersion {
	hs, err := WithObservedDates([]Holiday{
		{Date: "2026-08-31", Name: "Summer bank holiday"},
		{Date: "2026-12-25", Name: "Christmas Day", ObservedRule: ObservedNextWeekday},
		{Date: "2026-12-26", Name: "Boxing Day", ObservedRule: ObservedNextWeekday}, // a Saturday: observed Monday 28th
		{Date: "2027-01-01", Name: "New Year's Day", ObservedRule: ObservedNextWeekday},
	}, []int{6, 0})
	if err != nil {
		panic(err)
	}
	return CalendarVersion{CalendarCode: "gb-bank", Version: 1, EffectiveFrom: day("2026-01-01"), Timezone: "Europe/London", WeekendDays: []int{6, 0}, Holidays: hs}
}

func TestComputeObservedDate_GoldenCases(t *testing.T) {
	satSun := []int{6, 0}
	friSat := []int{5, 6}
	for _, c := range []struct {
		name    string
		date    string
		rule    string
		weekend []int
		want    string
	}{
		{"a Saturday to the next weekday", "2026-12-26", ObservedNextWeekday, satSun, "2026-12-28"},
		{"a Sunday to the next weekday", "2022-12-25", ObservedNextWeekday, satSun, "2022-12-26"},
		{"a Saturday to the previous weekday", "2026-07-04", ObservedPreviousWeekday, satSun, "2026-07-03"},
		{"a Sunday to the previous weekday", "2027-07-04", ObservedPreviousWeekday, satSun, "2027-07-02"},
		{"a Saturday to the nearest: the Friday", "2026-07-04", ObservedNearestWeekday, satSun, "2026-07-03"},
		{"a Sunday to the nearest: the Monday", "2027-07-04", ObservedNearestWeekday, satSun, "2027-07-05"},
		{"a weekday stays", "2026-12-25", ObservedNextWeekday, satSun, "2026-12-25"},
		{"NONE stays on a weekend", "2026-12-26", ObservedNone, satSun, "2026-12-26"},
		{"no rule stays on a weekend", "2026-12-26", "", satSun, "2026-12-26"},
		// A Friday-Saturday weekend: Friday is the weekend, Thursday is nearer than Sunday.
		{"Friday-Saturday weekend, a Friday to the nearest", "2026-12-25", ObservedNearestWeekday, friSat, "2026-12-24"},
		{"Friday-Saturday weekend, a Saturday to the nearest", "2026-12-26", ObservedNearestWeekday, friSat, "2026-12-27"},
		{"Friday-Saturday weekend, a Friday to the next", "2026-12-25", ObservedNextWeekday, friSat, "2026-12-27"},
		// A single-day weekend: the tie goes to the following day.
		{"a Sunday-only weekend to the nearest", "2026-12-27", ObservedNearestWeekday, []int{0}, "2026-12-28"},
	} {
		got, err := ComputeObservedDate(day(c.date), c.rule, c.weekend)
		if err != nil || got.Format(dateLayout) != c.want {
			t.Errorf("%s: %s %v, want %s", c.name, got.Format(dateLayout), err, c.want)
		}
	}
	if _, err := ComputeObservedDate(day("2026-12-26"), "SOMETIMES", []int{6, 0}); err == nil {
		t.Error("an unknown rule was accepted")
	}
	if _, err := ComputeObservedDate(day("2026-12-26"), ObservedNextWeekday, []int{0, 1, 2, 3, 4, 5, 6}); err == nil {
		t.Error("a calendar with no weekday observed a holiday")
	}
}

func TestIsBusinessDay_UsesTheObservedDateAndSaysWhy(t *testing.T) {
	c := gb()
	for _, tc := range []struct {
		date, reason, name string
		business           bool
	}{
		{"2026-12-24", "", "", true},                                // Thursday
		{"2026-12-25", ReasonHoliday, "Christmas Day", false},       // Friday
		{"2026-12-26", ReasonWeekend, "", false},                    // Saturday: weekend, though also Boxing Day
		{"2026-12-27", ReasonWeekend, "", false},                    // Sunday
		{"2026-12-28", ReasonHoliday, "Boxing Day", false},          // Monday: Boxing Day observed
		{"2026-12-29", "", "", true},                                // Tuesday
		{"2026-08-31", ReasonHoliday, "Summer bank holiday", false}, // Monday
		{"2026-09-01", "", "", true},
		{"2027-01-01", ReasonHoliday, "New Year's Day", false},
	} {
		reason, name, non := c.NonBusinessDay(day(tc.date))
		if non == tc.business || reason != tc.reason || name != tc.name || c.IsBusinessDay(day(tc.date)) != tc.business {
			t.Errorf("%s: reason %q name %q non %v, want business=%v reason %q name %q", tc.date, reason, name, non, tc.business, tc.reason, tc.name)
		}
	}
}

// A calendar with no observed rules reads exactly as before this feature: the
// holiday is non-business on its own date.
func TestIsBusinessDay_AHolidayWithNoRuleIsObservedOnItsOwnDate(t *testing.T) {
	c := CalendarVersion{WeekendDays: []int{6, 0}, Holidays: []Holiday{{Date: "2026-05-01", Name: "Labour Day"}}}
	if c.IsBusinessDay(day("2026-05-01")) || !c.IsBusinessDay(day("2026-05-04")) {
		t.Error("a plain holiday is not observed on its own date, or moved")
	}
}

func TestAdjust_GoldenCases(t *testing.T) {
	c := gb()
	for _, tc := range []struct {
		name, from, convention, want string
		skipped                      []string
	}{
		{"a business day is unchanged", "2026-12-24", ConventionFollowing, "2026-12-24", nil},
		{"NONE leaves a weekend alone", "2026-12-26", ConventionNone, "2026-12-26", nil},
		// Saturday 26th: Sat, Sun, then Monday 28th is Boxing Day: Tuesday 29th.
		{"following over a weekend and a holiday", "2026-12-26", ConventionFollowing, "2026-12-29", []string{"2026-12-26", "2026-12-27", "2026-12-28"}},
		// Sunday 27th back: Sat, then Friday 25th is Christmas: Thursday 24th.
		{"preceding over a weekend and a holiday", "2026-12-27", ConventionPreceding, "2026-12-24", []string{"2026-12-27", "2026-12-26", "2026-12-25"}},
		// Saturday 31 Oct: following would be Monday 2 Nov, another month: the Friday.
		{"modified following stays in the month", "2026-10-31", ConventionModifiedFollowing, "2026-10-30", []string{"2026-10-31"}},
		// Saturday 28 Nov: following is Monday 30 Nov, the same month.
		{"modified following within the month follows", "2026-11-28", ConventionModifiedFollowing, "2026-11-30", []string{"2026-11-28", "2026-11-29"}},
		// Sunday 1 Nov: preceding would be Friday 30 Oct, another month: the Monday.
		{"modified preceding stays in the month", "2026-11-01", ConventionModifiedPreceding, "2026-11-02", []string{"2026-11-01"}},
		{"modified preceding within the month precedes", "2026-11-29", ConventionModifiedPreceding, "2026-11-27", []string{"2026-11-29", "2026-11-28"}},
	} {
		got, skipped, err := c.Adjust(day(tc.from), tc.convention)
		if err != nil || got.Format(dateLayout) != tc.want {
			t.Errorf("%s: %s %v, want %s", tc.name, got.Format(dateLayout), err, tc.want)
			continue
		}
		var dates []string
		for _, s := range skipped {
			dates = append(dates, s.Date)
		}
		if strings.Join(dates, ",") != strings.Join(tc.skipped, ",") {
			t.Errorf("%s: skipped %v, want %v", tc.name, dates, tc.skipped)
		}
	}
	if _, _, err := c.Adjust(day("2026-12-26"), "SOMETIMES"); err == nil {
		t.Error("an unknown convention was accepted")
	}
}

func TestAddBusinessDays_GoldenCases(t *testing.T) {
	c := gb()
	for _, tc := range []struct {
		name, from string
		n          int
		want       string
	}{
		{"zero is unchanged", "2026-12-18", 0, "2026-12-18"},
		{"one from a Friday is the Monday", "2026-12-18", 1, "2026-12-21"},
		{"one from a Saturday is the Monday", "2026-10-31", 1, "2026-11-02"},
		{"five over Christmas and Boxing Day", "2026-12-18", 5, "2026-12-29"},
		{"minus one back over a weekend", "2026-12-21", -1, "2026-12-18"},
		{"minus one back over Christmas", "2026-12-29", -1, "2026-12-24"},
		{"minus five", "2026-12-29", -5, "2026-12-18"},
		{"two stays in the year", "2026-12-29", 2, "2026-12-31"},
		{"three crosses the New Year holiday", "2026-12-29", 3, "2027-01-04"},
		{"a long way", "2026-01-05", 250, "2026-12-22"},
	} {
		got, skipped, err := c.AddBusinessDays(day(tc.from), tc.n)
		if err != nil || got.Format(dateLayout) != tc.want {
			t.Errorf("%s: %s %v, want %s", tc.name, got.Format(dateLayout), err, tc.want)
		}
		if tc.n == 0 && len(skipped) != 0 {
			t.Errorf("%s: skipped %v", tc.name, skipped)
		}
	}

	// The days passed over are named, with their reasons.
	_, skipped, _ := c.AddBusinessDays(day("2026-12-24"), 2)
	want := []SkippedDay{{"2026-12-25", ReasonHoliday, "Christmas Day"}, {"2026-12-26", ReasonWeekend, ""}, {"2026-12-27", ReasonWeekend, ""}, {"2026-12-28", ReasonHoliday, "Boxing Day"}}
	if len(skipped) != len(want) {
		t.Fatalf("skipped = %v, want %v", skipped, want)
	}
	for i := range want {
		if skipped[i] != want[i] {
			t.Errorf("skipped[%d] = %+v, want %+v", i, skipped[i], want[i])
		}
	}

	for _, n := range []int{MaxBusinessDayShift + 1, -MaxBusinessDayShift - 1} {
		if _, _, err := c.AddBusinessDays(day("2026-01-05"), n); !errors.Is(err, ErrObligationInvalid) {
			t.Errorf("a shift of %d: %v, want it refused", n, err)
		}
	}
}

// Forward and back are inverses on business days.
func TestAddBusinessDays_IsReversible(t *testing.T) {
	c := gb()
	for d := day("2026-01-05"); d.Before(day("2027-01-30")); d = d.AddDate(0, 0, 1) {
		if !c.IsBusinessDay(d) {
			continue
		}
		for _, n := range []int{1, 3, 10, 40} {
			fwd, _, err := c.AddBusinessDays(d, n)
			if err != nil {
				t.Fatal(err)
			}
			back, _, err := c.AddBusinessDays(fwd, -n)
			if err != nil || !back.Equal(d) {
				t.Fatalf("%s +%d = %s, and back = %s: not reversible", d.Format(dateLayout), n, fwd.Format(dateLayout), back.Format(dateLayout))
			}
		}
	}
}

func TestWithObservedDates_WorksOutEachFromItsRuleAndTheWeekend(t *testing.T) {
	got, err := WithObservedDates([]Holiday{
		{Date: "2026-12-26", Name: " Boxing Day ", ObservedRule: ObservedNextWeekday, ObservedDate: "1999-01-01"}, // the caller's date is ignored
		{Date: "2026-12-25", Name: "Christmas"},
	}, []int{6, 0})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ObservedDate != "2026-12-28" || got[0].Name != "Boxing Day" || got[0].ObservedRule != ObservedNextWeekday {
		t.Errorf("got %+v: the observed date must come from the rule, not the caller", got[0])
	}
	if got[1].ObservedDate != "" || got[1].ObservedRule != ObservedNone {
		t.Errorf("got %+v: a holiday with no rule has no observed date", got[1])
	}
	if _, err := WithObservedDates([]Holiday{{Date: "nope", Name: "x"}}, nil); err == nil {
		t.Error("a bad date was accepted")
	}
	if _, err := WithObservedDates([]Holiday{{Date: "2026-12-25", Name: "x", ObservedRule: "SOMETIMES"}}, []int{6, 0}); err == nil {
		t.Error("an unknown rule was accepted")
	}
}

func goodImport() ImportCalendarParams {
	return ImportCalendarParams{
		CalendarCode: "gb-bank", SourceAuthority: "UK Government", SourceReference: "https://www.gov.uk/bank-holidays.json", SourceVersion: "2026-2",
		EffectiveFrom: "2026-01-01", Timezone: "Europe/London", WeekendDays: []int{6, 0},
		Holidays: []Holiday{{Date: "2026-12-25", Name: "Christmas Day", ObservedRule: ObservedNextWeekday}, {Date: "2026-08-31", Name: "Summer bank holiday"}},
		Actor:    "steward-1",
	}
}

func TestImportCalendarParams_Validate(t *testing.T) {
	if err := goodImport().Validate(); err != nil {
		t.Fatalf("a good import refused: %v", err)
	}
	for name, mutate := range map[string]func(*ImportCalendarParams){
		"bad code":           func(p *ImportCalendarParams) { p.CalendarCode = "GB Bank" },
		"no authority":       func(p *ImportCalendarParams) { p.SourceAuthority = " " },
		"no reference":       func(p *ImportCalendarParams) { p.SourceReference = "" },
		"no source version":  func(p *ImportCalendarParams) { p.SourceVersion = "" },
		"over-long version":  func(p *ImportCalendarParams) { p.SourceVersion = strings.Repeat("x", 65) },
		"over-long ref":      func(p *ImportCalendarParams) { p.SourceReference = strings.Repeat("x", 513) },
		"no actor":           func(p *ImportCalendarParams) { p.Actor = "" },
		"no holidays":        func(p *ImportCalendarParams) { p.Holidays = nil },
		"too many":           func(p *ImportCalendarParams) { p.Holidays = make([]Holiday, MaxImportHolidays+1) },
		"bad effective date": func(p *ImportCalendarParams) { p.EffectiveFrom = "01/01/2026" },
		"bad time zone":      func(p *ImportCalendarParams) { p.Timezone = "Mars/Olympus" },
		"a holiday twice":    func(p *ImportCalendarParams) { p.Holidays = append(p.Holidays, p.Holidays[0]) },
		"an unknown rule":    func(p *ImportCalendarParams) { p.Holidays[0].ObservedRule = "SOMETIMES" },
		"a nameless holiday": func(p *ImportCalendarParams) { p.Holidays[0].Name = " " },
		"a bad weekend":      func(p *ImportCalendarParams) { p.WeekendDays = []int{7} },
	} {
		p := goodImport()
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, ErrObligationInvalid) {
			t.Errorf("%s: %v, want it refused", name, err)
		}
	}
}

func TestCalendarImportHash_IsTheContentOfTheEdition(t *testing.T) {
	base := CalendarImportHash(goodImport())
	if len(base) != 64 {
		t.Fatalf("hash = %q", base)
	}
	// Order, padding and where it was fetched from are not content.
	same := goodImport()
	same.Holidays = []Holiday{same.Holidays[1], same.Holidays[0]}
	same.Holidays[1].Name = "  Christmas Day "
	same.SourceReference = "https://mirror.example/bank-holidays.json"
	same.SourceAuthority = " UK Government "
	same.WeekendDays = []int{0, 6}
	same.SourceIDs = []string{"s1"}
	same.Actor = "someone-else"
	if got := CalendarImportHash(same); got != base {
		t.Error("the same edition hashed differently because of ordering, padding, mirror or actor")
	}
	for name, mutate := range map[string]func(*ImportCalendarParams){
		"another edition":   func(p *ImportCalendarParams) { p.SourceVersion = "2026-3" },
		"another authority": func(p *ImportCalendarParams) { p.SourceAuthority = "Other" },
		"another date":      func(p *ImportCalendarParams) { p.Holidays[0].Date = "2026-12-24" },
		"another name":      func(p *ImportCalendarParams) { p.Holidays[0].Name = "Xmas" },
		"another rule":      func(p *ImportCalendarParams) { p.Holidays[0].ObservedRule = ObservedNearestWeekday },
		"another weekend":   func(p *ImportCalendarParams) { p.WeekendDays = []int{5, 6} },
		"another zone":      func(p *ImportCalendarParams) { p.Timezone = "Europe/Paris" },
		"another cut-off":   func(p *ImportCalendarParams) { c := "15:00"; p.CutoffTime = &c },
		"another start":     func(p *ImportCalendarParams) { p.EffectiveFrom = "2026-02-01" },
		"an extra holiday": func(p *ImportCalendarParams) {
			p.Holidays = append(p.Holidays, Holiday{Date: "2026-05-04", Name: "May Day"})
		},
	} {
		p := goodImport()
		mutate(&p)
		if CalendarImportHash(p) == base {
			t.Errorf("%s hashed the same as the original", name)
		}
	}
}

func TestCalendarContentDigest_PinsWhatTheVersionSays(t *testing.T) {
	a, b := gb(), gb()
	if CalendarContentDigest(a) != CalendarContentDigest(b) || !strings.HasPrefix(CalendarContentDigest(a), "sha256:") {
		t.Fatal("the same version has two digests")
	}
	b.Holidays = append([]Holiday(nil), b.Holidays...)
	b.Holidays[0], b.Holidays[1] = b.Holidays[1], b.Holidays[0]
	if CalendarContentDigest(a) != CalendarContentDigest(b) {
		t.Error("the order of the holidays changed the digest")
	}
	for name, mutate := range map[string]func(*CalendarVersion){
		"a version number": func(c *CalendarVersion) { c.Version = 2 },
		"a holiday":        func(c *CalendarVersion) { c.Holidays = c.Holidays[1:] },
		"an observed date": func(c *CalendarVersion) {
			c.Holidays = append([]Holiday(nil), c.Holidays...)
			c.Holidays[1].ObservedDate = "2026-12-29"
		},
		"the weekend":   func(c *CalendarVersion) { c.WeekendDays = []int{5, 6} },
		"the time zone": func(c *CalendarVersion) { c.Timezone = "UTC" },
	} {
		c := gb()
		mutate(&c)
		if CalendarContentDigest(c) == CalendarContentDigest(a) {
			t.Errorf("changing %s did not change the digest", name)
		}
	}
}

func TestDiffHolidays(t *testing.T) {
	prev := []Holiday{
		{Date: "2026-01-01", Name: "New Year"},
		{Date: "2026-05-04", Name: "May Day"},
		{Date: "2026-12-26", Name: "Boxing Day", ObservedRule: ObservedNextWeekday, ObservedDate: "2026-12-28"},
		{Date: "2026-08-31", Name: "Summer"},
	}
	cur := []Holiday{
		{Date: "2026-01-01", Name: "New Year"},                                                                      // unchanged
		{Date: "2026-05-04", Name: "Early May bank holiday"},                                                        // renamed
		{Date: "2026-12-26", Name: "Boxing Day", ObservedRule: ObservedPreviousWeekday, ObservedDate: "2026-12-25"}, // observed elsewhere
		{Date: "2026-06-01", Name: "Jubilee"},                                                                       // added; Summer removed
	}
	got := DiffHolidays(prev, cur)
	want := map[string]string{"2026-05-04": "CHANGED", "2026-06-01": "ADDED", "2026-08-31": "REMOVED", "2026-12-26": "CHANGED"}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %v", got, want)
	}
	for i, ch := range got {
		if want[ch.Date] != ch.Change {
			t.Errorf("%s: %s, want %s", ch.Date, ch.Change, want[ch.Date])
		}
		if i > 0 && got[i-1].Date >= ch.Date {
			t.Error("changes are not in date order")
		}
	}
	for _, ch := range got {
		if ch.Date == "2026-12-26" && (ch.PreviousObservedDate != "2026-12-28" || ch.ObservedDate != "2026-12-25") {
			t.Errorf("observed move = %+v", ch)
		}
		if ch.Date == "2026-05-04" && (ch.PreviousName != "May Day" || ch.Name != "Early May bank holiday" || ch.PreviousObservedDate != "") {
			t.Errorf("rename = %+v", ch)
		}
	}
	if got := DiffHolidays(nil, cur); len(got) != 4 || got[0].Change != "ADDED" {
		t.Errorf("a first version adds everything: %+v", got)
	}
	if got := DiffHolidays(cur, cur); len(got) != 0 {
		t.Errorf("an identical version changes nothing: %+v", got)
	}
}

func TestValidateCalendarVersion_CarriesObservedFields(t *testing.T) {
	ok := CalendarVersion{Timezone: "UTC", WeekendDays: []int{6, 0}, Holidays: []Holiday{{Date: "2026-12-26", Name: "x", ObservedRule: ObservedNextWeekday, ObservedDate: "2026-12-28"}}}
	if err := ValidateCalendarVersion(ok); err != nil {
		t.Errorf("a calendar with an observed holiday refused: %v", err)
	}
	bad := ok
	bad.Holidays = []Holiday{{Date: "2026-12-26", Name: "x", ObservedRule: "SOMETIMES"}}
	if err := ValidateCalendarVersion(bad); !errors.Is(err, ErrObligationInvalid) {
		t.Errorf("an unknown rule: %v", err)
	}
	bad.Holidays = []Holiday{{Date: "2026-12-26", Name: "x", ObservedRule: ObservedNextWeekday, ObservedDate: "tomorrow"}}
	if err := ValidateCalendarVersion(bad); !errors.Is(err, ErrObligationInvalid) {
		t.Errorf("a bad observed date: %v", err)
	}
}
