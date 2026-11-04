//go:build integration

// REF-10 Business Calendar end to end: real Postgres (embedded), the real store
// and the real HTTP handlers. An authority's holiday list is imported as a draft
// with its evidence, published by someone else, and then answers IsBusinessDay
// and AddBusinessDays deterministically, naming the version that answered, so a
// past answer can be reproduced after the calendar is amended.
package registryit_test

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(migrations(t, name)[0])
	require.NoError(t, err)
	return string(b)
}

type calFx struct {
	e    *env
	f    *fixture
	code string
}

func newCalFx(t *testing.T) *calFx {
	t.Helper()
	e := newEnv()
	f := newFixture(t, e)
	c := &calFx{e: e, f: f, code: "bc-" + strings.ToLower(uuid.NewString()[:6])}
	code, r := e.do("POST", "/v1/admin/calendars", "steward-1", map[string]any{"calendar_code": c.code, "jurisdiction_id": f.jur.JurisdictionID, "authority": "UK Government"})
	mustStatus(t, 201, code, r)
	return c
}

func hol(date, name, rule string) map[string]any {
	h := map[string]any{"date": date, "name": name}
	if rule != "" {
		h["observed_rule"] = rule
	}
	return h
}

// gbHolidays are the end-of-2026 holidays of England, the weekend ones observed on
// the next weekday: Boxing Day is a Saturday and is observed on Monday 28 December.
func gbHolidays() []map[string]any {
	return []map[string]any{
		hol("2026-08-31", "Summer bank holiday", ""),
		hol("2026-12-25", "Christmas Day", "WEEKEND_TO_NEXT_WEEKDAY"),
		hol("2026-12-26", "Boxing Day", "WEEKEND_TO_NEXT_WEEKDAY"),
	}
}

func (c *calFx) importBody(sourceVersion string, holidays []map[string]any) map[string]any {
	return map[string]any{
		"source_authority": "UK Government", "source_reference": "https://www.gov.uk/bank-holidays.json", "source_version": sourceVersion,
		"effective_from": "2026-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": holidays,
		"source_ids": []string{c.f.sourceID},
	}
}

func (c *calFx) imp(t *testing.T, sourceVersion string, holidays []map[string]any, want int) map[string]any {
	t.Helper()
	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", c.importBody(sourceVersion, holidays))
	mustStatus(t, want, code, r)
	return r
}

func (c *calFx) publish(t *testing.T, version int) map[string]any {
	t.Helper()
	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/versions/"+strconv.Itoa(version)+"/publish", "publisher-1", nil)
	mustStatus(t, 200, code, r)
	require.Equal(t, "PUBLISHED", r["status"])
	return r
}

func (c *calFx) get(t *testing.T, path string, want int) map[string]any {
	t.Helper()
	code, r := c.e.do("GET", "/v1/calendars/"+c.code+path, "", nil)
	mustStatus(t, want, code, r)
	return r
}

func calendarOf(r map[string]any) map[string]any { c, _ := r["calendar"].(map[string]any); return c }

// ── import ──────────────────────────────────────────────────────────────────

func TestBusinessCalendar_ImportDraftsWithItsEvidenceAndObservedDates(t *testing.T) {
	c := newCalFx(t)
	body := c.importBody("2026-2", []map[string]any{
		hol("2026-12-25", "Christmas Day", "WEEKEND_TO_NEXT_WEEKDAY"),
		// The caller's own observed date is ignored: it is worked out from the rule.
		{"date": "2026-12-26", "name": " Boxing Day ", "observed_rule": "WEEKEND_TO_NEXT_WEEKDAY", "observed_date": "1999-01-01"},
		hol("2026-08-31", "Summer bank holiday", ""),
	})
	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", body)
	mustStatus(t, 201, code, r)

	assert.EqualValues(t, 1, r["version"])
	assert.Equal(t, "DRAFT", r["status"])
	assert.Equal(t, "UK Government", r["source_authority"])
	assert.Equal(t, "https://www.gov.uk/bank-holidays.json", r["source_reference"])
	assert.Equal(t, "2026-2", r["source_version"])
	assert.Len(t, r["import_hash"], 64)
	assert.True(t, strings.HasPrefix(r["content_digest"].(string), "sha256:"))
	assert.Equal(t, []any{c.f.sourceID}, r["source_ids"], "the cited sources are linked at import")

	byDate := map[string]map[string]any{}
	for _, h := range r["holidays"].([]any) {
		m := h.(map[string]any)
		byDate[m["date"].(string)] = m
	}
	assert.Equal(t, "2026-12-28", byDate["2026-12-26"]["observed_date"], "Boxing Day on a Saturday is observed on the Monday")
	assert.Equal(t, "Boxing Day", byDate["2026-12-26"]["name"])
	assert.Equal(t, "WEEKEND_TO_NEXT_WEEKDAY", byDate["2026-12-26"]["observed_rule"])
	assert.Equal(t, "2026-12-25", byDate["2026-12-25"]["observed_date"], "a weekday holiday is observed on itself")
	_, hasRule := byDate["2026-08-31"]["observed_rule"]
	assert.False(t, hasRule, "a plain holiday reads exactly as it always did")

	// The permission asked is the calendar version's, as for any draft.
	assert.True(t, c.e.auth.pairs["regulatory_calendar_version.create"])
}

func TestBusinessCalendar_AnEditionIsImportedOnce(t *testing.T) {
	c := newCalFx(t)
	first := c.imp(t, "2026-2", gbHolidays(), 201)

	// The same content, reordered and from another mirror, is a replay.
	reordered := []map[string]any{gbHolidays()[2], gbHolidays()[0], gbHolidays()[1]}
	body := c.importBody("2026-2", reordered)
	body["source_reference"] = "https://mirror.example/bank-holidays.json"
	code, again := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-2", body)
	mustStatus(t, 200, code, again)
	assert.Equal(t, first["calendar_version_id"], again["calendar_version_id"])
	assert.Equal(t, "https://www.gov.uk/bank-holidays.json", again["source_reference"], "the first import's evidence stands")

	// Different content for the same edition is refused, not merged.
	changed := append(gbHolidays(), hol("2026-05-04", "Early May bank holiday", ""))
	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", c.importBody("2026-2", changed))
	mustStatus(t, 409, code, r)
	assert.Equal(t, "calendar_import_conflict", r["code"])

	// A new edition is a new version.
	second := c.imp(t, "2026-3", changed, 201)
	assert.EqualValues(t, 2, second["version"])
	assert.NotEqual(t, first["content_digest"], second["content_digest"])

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE c.calendar_code=$1`, c.code).Scan(&n))
	assert.Equal(t, 2, n)
}

func TestBusinessCalendar_ConcurrentImportsOfOneEditionMakeOneVersion(t *testing.T) {
	c := newCalFx(t)
	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	ids := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", c.importBody("2026-9", gbHolidays()))
			codes[i] = code
			ids[i], _ = r["calendar_version_id"].(string)
		}(i)
	}
	close(start)
	wg.Wait()
	created := 0
	for i := 0; i < n; i++ {
		require.Contains(t, []int{200, 201}, codes[i], "import %d", i)
		if codes[i] == 201 {
			created++
		}
		assert.Equal(t, ids[0], ids[i])
	}
	assert.Equal(t, 1, created, "exactly one import creates the version")
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE c.calendar_code=$1`, c.code).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestBusinessCalendar_ARefusedImportWritesNothing(t *testing.T) {
	c := newCalFx(t)
	count := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE c.calendar_code=$1`, c.code).Scan(&n))
		return n
	}
	post := func(mutate func(map[string]any)) (int, map[string]any) {
		b := c.importBody("2026-4", gbHolidays())
		mutate(b)
		return c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", b)
	}
	for name, mutate := range map[string]func(map[string]any){
		"no authority":      func(b map[string]any) { b["source_authority"] = "" },
		"no reference":      func(b map[string]any) { delete(b, "source_reference") },
		"no source version": func(b map[string]any) { b["source_version"] = " " },
		"no holidays":       func(b map[string]any) { b["holidays"] = []any{} },
		"an unknown rule":   func(b map[string]any) { b["holidays"] = []map[string]any{hol("2026-12-25", "x", "SOMETIMES")} },
		"a holiday twice": func(b map[string]any) {
			b["holidays"] = []map[string]any{hol("2026-12-25", "x", ""), hol("2026-12-25", "y", "")}
		},
		"a bad time zone":      func(b map[string]any) { b["timezone"] = "Mars/Olympus" },
		"a bad date":           func(b map[string]any) { b["effective_from"] = "01/01/2026" },
		"a nameless holiday":   func(b map[string]any) { b["holidays"] = []map[string]any{hol("2026-12-25", " ", "")} },
		"a source that is not": func(b map[string]any) { b["source_ids"] = []string{uuid.NewString()} },
	} {
		code, r := post(mutate)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, r := post(func(b map[string]any) { delete(b, "weekend_days") })
	assert.Equal(t, 400, code, "%v", r)
	assert.Equal(t, 0, count(), "a refused import left a version behind")

	code, r = c.e.do("POST", "/v1/admin/calendars/no-such-calendar/imports", "steward-1", c.importBody("2026-4", gbHolidays()))
	assert.Equal(t, 404, code, "%v", r)
	code, r = c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "", c.importBody("2026-4", gbHolidays()))
	assert.Equal(t, 401, code, "%v", r)
	c.e.auth.deny = "regulatory_calendar_version.create"
	code, r = c.e.do("POST", "/v1/admin/calendars/"+c.code+"/imports", "steward-1", c.importBody("2026-4", gbHolidays()))
	assert.Equal(t, 403, code, "%v", r)
	assert.Equal(t, 0, count())
}

// An imported version is published like any other: by someone else, citing a
// reviewed source. Until then it governs nothing.
func TestBusinessCalendar_ADraftNeverAnswersAndPublishingIsIndependent(t *testing.T) {
	c := newCalFx(t)
	c.imp(t, "2026-2", gbHolidays(), 201)

	r := c.get(t, "/is-business-day?date=2026-12-24", 404)
	assert.Equal(t, "no_calendar_version", r["code"], "a draft must not answer")

	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/versions/1/publish", "steward-1", nil)
	mustStatus(t, 403, code, r)
	assert.Equal(t, "segregation_of_duties", r["code"], "the importer cannot publish their own import")
	c.get(t, "/is-business-day?date=2026-12-24", 404)

	c.publish(t, 1)
	assert.Equal(t, true, c.get(t, "/is-business-day?date=2026-12-24", 200)["is_business_day"])
}

// ── events ──────────────────────────────────────────────────────────────────

func TestBusinessCalendar_PublishingAnnouncesTheCalendarAndEachChangedHoliday(t *testing.T) {
	c := newCalFx(t)
	pub := c.e.pub

	c.imp(t, "2026-2", gbHolidays(), 201)
	c.publish(t, 1)
	assert.Equal(t, 1, pub.count("business-calendar.updated"))
	assert.Equal(t, 3, pub.count("holiday-occurrence.changed"), "a first version adds every holiday")
	assert.Equal(t, 1, pub.count("jurisdiction.calendar.changed"), "the original event is still published")
	up := pub.payloadsOf("business-calendar.updated")[0]
	assert.Equal(t, c.code, up["calendar_code"])
	assert.EqualValues(t, 1, up["calendar_version"])
	assert.EqualValues(t, 3, up["holidays_changed"])
	assert.Equal(t, "2026-2", *(up["source_version"].(*string)))
	assert.True(t, strings.HasPrefix(up["content_digest"].(string), "sha256:"))

	// The next edition: Summer is removed, May Day is added, Boxing Day is now observed on the Friday.
	next := []map[string]any{
		hol("2026-12-25", "Christmas Day", "WEEKEND_TO_NEXT_WEEKDAY"),
		hol("2026-12-26", "Boxing Day", "WEEKEND_TO_PREVIOUS_WEEKDAY"),
		hol("2026-05-04", "Early May bank holiday", ""),
	}
	c.imp(t, "2026-3", next, 201)
	c.publish(t, 2)
	assert.Equal(t, 2, pub.count("business-calendar.updated"))
	changes := pub.payloadsOf("holiday-occurrence.changed")[3:]
	require.Len(t, changes, 3, "only what differs from the previous version")
	got := map[string]map[string]any{}
	for _, ch := range changes {
		got[ch["holiday_date"].(string)] = ch
	}
	assert.Equal(t, "REMOVED", got["2026-08-31"]["change"])
	assert.Equal(t, "ADDED", got["2026-05-04"]["change"])
	assert.Equal(t, "CHANGED", got["2026-12-26"]["change"])
	assert.Equal(t, "2026-12-28", got["2026-12-26"]["previous_observed_date"])
	assert.Equal(t, "2026-12-25", got["2026-12-26"]["observed_date"])
	assert.EqualValues(t, 2, got["2026-12-26"]["calendar_version"])

	// A repeated publish changes nothing and announces nothing more.
	c.publish(t, 2)
	assert.Equal(t, 2, pub.count("business-calendar.updated"))
}

// ── the reads ───────────────────────────────────────────────────────────────

func TestBusinessCalendar_IsBusinessDayAnswersWithItsProvenance(t *testing.T) {
	c := newCalFx(t)
	c.imp(t, "2026-2", gbHolidays(), 201)
	v1 := c.publish(t, 1)

	for _, tc := range []struct {
		date     string
		business bool
		reason   string
		name     string
	}{
		{"2026-12-24", true, "", ""},
		{"2026-12-25", false, "HOLIDAY", "Christmas Day"},
		{"2026-12-26", false, "WEEKEND", ""},
		{"2026-12-27", false, "WEEKEND", ""},
		{"2026-12-28", false, "HOLIDAY", "Boxing Day"},
		{"2026-12-29", true, "", ""},
		{"2026-08-31", false, "HOLIDAY", "Summer bank holiday"},
	} {
		r := c.get(t, "/is-business-day?date="+tc.date, 200)
		assert.Equal(t, tc.date, r["date"])
		assert.Equal(t, tc.business, r["is_business_day"], tc.date)
		if tc.reason == "" {
			_, has := r["reason"]
			assert.False(t, has, tc.date)
		} else {
			assert.Equal(t, tc.reason, r["reason"], tc.date)
			assert.Equal(t, tc.name != "", r["holiday_name"] != nil, tc.date)
			if tc.name != "" {
				assert.Equal(t, tc.name, r["holiday_name"], tc.date)
			}
		}
		cal := calendarOf(r)
		assert.Equal(t, c.code, cal["calendar_code"])
		assert.EqualValues(t, 1, cal["calendar_version"])
		assert.Equal(t, v1["calendar_version_id"], cal["calendar_version_id"])
		assert.Equal(t, v1["content_digest"], cal["content_digest"], "the answer names the content it was given by")
		assert.Equal(t, "UK Government", cal["source_authority"])
		assert.Equal(t, "2026-2", cal["source_version"])
		assert.Equal(t, v1["import_hash"], cal["import_hash"])
		assert.Equal(t, false, cal["pinned"])
		assert.Equal(t, "Europe/London", cal["timezone"])
	}
}

func TestBusinessCalendar_AddBusinessDaysIsDeterministicAndNamesWhatItSkipped(t *testing.T) {
	c := newCalFx(t)
	c.imp(t, "2026-2", gbHolidays(), 201)
	c.publish(t, 1)

	r := c.get(t, "/add-business-days?date=2026-12-24&days=1", 200)
	assert.Equal(t, "2026-12-29", r["result_date"], "over Christmas, the weekend and Boxing Day")
	assert.EqualValues(t, 1, r["days"])
	assert.Equal(t, "2026-12-24", r["start_date"])
	var skipped []string
	for _, s := range r["skipped"].([]any) {
		m := s.(map[string]any)
		skipped = append(skipped, m["date"].(string)+":"+m["reason"].(string))
	}
	assert.Equal(t, []string{"2026-12-25:HOLIDAY", "2026-12-26:WEEKEND", "2026-12-27:WEEKEND", "2026-12-28:HOLIDAY"}, skipped)

	assert.Equal(t, "2026-12-24", c.get(t, "/add-business-days?date=2026-12-29&days=-1", 200)["result_date"])
	assert.Equal(t, "2026-12-18", c.get(t, "/add-business-days?date=2026-12-29&days=-5", 200)["result_date"])
	assert.Equal(t, "2026-12-24", c.get(t, "/add-business-days?date=2026-12-24&days=0", 200)["result_date"])
	assert.Equal(t, "2026-11-02", c.get(t, "/add-business-days?date=2026-10-31&days=1", 200)["result_date"], "from a Saturday")
	assert.NotNil(t, c.get(t, "/add-business-days?date=2026-12-24&days=0", 200)["skipped"], "an empty list, not null")

	// The same question always has the same answer.
	a, b := c.get(t, "/add-business-days?date=2026-01-05&days=250", 200), c.get(t, "/add-business-days?date=2026-01-05&days=250", 200)
	assert.Equal(t, a, b)

	for name, q := range map[string]string{
		"no date":      "/add-business-days?days=1",
		"no days":      "/add-business-days?date=2026-12-24",
		"a bad date":   "/add-business-days?date=tomorrow&days=1",
		"bad days":     "/add-business-days?date=2026-12-24&days=x",
		"too many":     "/add-business-days?date=2026-12-24&days=1001",
		"too few":      "/add-business-days?date=2026-12-24&days=-1001",
		"a bad known":  "/add-business-days?date=2026-12-24&days=1&known_at=yesterday",
		"a bad pin":    "/add-business-days?date=2026-12-24&days=1&calendar_version=0",
		"no date (is)": "/is-business-day",
	} {
		code, r := c.e.do("GET", "/v1/calendars/"+c.code+q, "", nil)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, r := c.e.do("GET", "/v1/calendars/no-such/is-business-day?date=2026-12-24", "", nil)
	assert.Equal(t, 404, code, "%v", r)
	assert.Equal(t, "calendar_not_found", r["code"])
	// Before the version took effect nothing governs.
	assert.Equal(t, "no_calendar_version", c.get(t, "/is-business-day?date=2025-12-24", 404)["code"])
}

// A due date worked out today is never silently changed by a calendar amended
// tomorrow: ask as it was known then, or name the version.
func TestBusinessCalendar_AnAmendmentDoesNotRewriteHistory(t *testing.T) {
	c := newCalFx(t)
	c.imp(t, "2026-2", gbHolidays(), 201)
	v1 := c.publish(t, 1)
	assert.Equal(t, false, c.get(t, "/is-business-day?date=2026-08-31", 200)["is_business_day"])
	v1At := v1["published_at"].(string)

	// The authority corrects the whole year: Summer is dropped. Version 2 takes effect from the
	// same date, so for every date it now supersedes version 1.
	c.imp(t, "2026-3", []map[string]any{hol("2026-12-25", "Christmas Day", "WEEKEND_TO_NEXT_WEEKDAY")}, 201)
	v2 := c.publish(t, 2)
	assert.Equal(t, true, c.get(t, "/is-business-day?date=2026-08-31", 200)["is_business_day"], "asked now, the corrected calendar answers")
	assert.EqualValues(t, 2, calendarOf(c.get(t, "/is-business-day?date=2026-08-31", 200))["calendar_version"])

	// Asked as it was known when version 1 was the latest, the answer is what it was.
	q := url.QueryEscape(v1At)
	past := c.get(t, "/is-business-day?date=2026-08-31&known_at="+q, 200)
	assert.Equal(t, false, past["is_business_day"], "the answer given then is reproducible by knowledge time")
	assert.EqualValues(t, 1, calendarOf(past)["calendar_version"])
	assert.Equal(t, "Summer bank holiday", past["holiday_name"])

	// Or by naming the version it was given by.
	pinned := c.get(t, "/is-business-day?date=2026-08-31&calendar_version=1", 200)
	assert.Equal(t, false, pinned["is_business_day"])
	assert.Equal(t, true, calendarOf(pinned)["pinned"])
	assert.Equal(t, v1["content_digest"], calendarOf(pinned)["content_digest"])
	add := c.get(t, "/add-business-days?date=2026-08-28&days=1&calendar_version=1", 200)
	assert.Equal(t, "2026-09-01", add["result_date"], "pinned to version 1 the Summer holiday is still skipped")
	assert.Equal(t, "2026-08-31", c.get(t, "/add-business-days?date=2026-08-28&days=1&calendar_version=2", 200)["result_date"])

	// A pin cannot reach back before the version was published, or to a draft.
	early := url.QueryEscape(time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339))
	assert.Equal(t, "no_calendar_version", c.get(t, "/is-business-day?date=2026-08-31&calendar_version=1&known_at="+early, 404)["code"])
	c.imp(t, "2026-4", gbHolidays(), 201) // version 3, a draft
	assert.Equal(t, "no_calendar_version", c.get(t, "/is-business-day?date=2026-08-31&calendar_version=3", 404)["code"])
	assert.Equal(t, v2["calendar_version_id"], calendarOf(c.get(t, "/is-business-day?date=2026-08-31", 200))["calendar_version_id"], "a draft never takes over")
}

// ── the database holds the line too ─────────────────────────────────────────

func TestBusinessCalendar_TheDatabaseKeepsTheEvidenceAndTheObservedShape(t *testing.T) {
	c := newCalFx(t)
	draft := c.imp(t, "2026-2", gbHolidays(), 201)
	id := draft["calendar_version_id"].(string)

	for name, q := range map[string]string{
		"rewriting the authority of a draft":  `UPDATE regulatory_calendar_versions SET source_authority = 'Someone else' WHERE calendar_version_id = $1`,
		"rewriting the hash of a draft":       `UPDATE regulatory_calendar_versions SET import_hash = repeat('0', 64) WHERE calendar_version_id = $1`,
		"rewriting the source version of one": `UPDATE regulatory_calendar_versions SET source_version = 'other' WHERE calendar_version_id = $1`,
		"an observed rule with no date":       `INSERT INTO regulatory_holidays (calendar_version_id, holiday_date, name, observed_rule) VALUES ($1, '2026-02-02', 'x', 'WEEKEND_TO_NEXT_WEEKDAY')`,
		"an observed date with no rule":       `INSERT INTO regulatory_holidays (calendar_version_id, holiday_date, name, observed_date) VALUES ($1, '2026-02-02', 'x', '2026-02-03')`,
		"an unknown observed rule":            `INSERT INTO regulatory_holidays (calendar_version_id, holiday_date, name, observed_rule, observed_date) VALUES ($1, '2026-02-02', 'x', 'SOMETIMES', '2026-02-03')`,
		"half an import's evidence":           `INSERT INTO regulatory_calendar_versions (calendar_id, version, effective_from, timezone, weekend_days, created_by_principal_id, source_authority) SELECT calendar_id, 99, '2026-01-01', 'UTC', '{0,6}', 'x', 'only the authority' FROM regulatory_calendar_versions WHERE calendar_version_id = $1`,
		"an import hash that is no hash":      `INSERT INTO regulatory_calendar_versions (calendar_id, version, effective_from, timezone, weekend_days, created_by_principal_id, source_authority, source_reference, source_version, import_hash) SELECT calendar_id, 99, '2026-01-01', 'UTC', '{0,6}', 'x', 'a', 'b', 'c', 'not-a-hash' FROM regulatory_calendar_versions WHERE calendar_version_id = $1`,
		"a second import of the same edition": `INSERT INTO regulatory_calendar_versions (calendar_id, version, effective_from, timezone, weekend_days, created_by_principal_id, source_authority, source_reference, source_version, import_hash) SELECT calendar_id, 99, '2026-01-01', 'UTC', '{0,6}', 'x', 'a', 'b', '2026-2', repeat('1', 64) FROM regulatory_calendar_versions WHERE calendar_version_id = $1`,
	} {
		_, err := pool.Exec(ctx, q, id)
		assert.Error(t, err, name)
	}

	// Once published the observed dates are frozen with the holidays.
	c.publish(t, 1)
	_, err := pool.Exec(ctx, `UPDATE regulatory_holidays SET observed_date = '2026-12-29' WHERE calendar_version_id = $1 AND holiday_date = '2026-12-26'`, id)
	assert.Error(t, err, "a published observed date was rewritten")
	_, err = pool.Exec(ctx, `UPDATE regulatory_calendar_versions SET source_authority = 'x' WHERE calendar_version_id = $1`, id)
	assert.Error(t, err, "the evidence of a published import was rewritten")
}

// Hand-authored versions keep working: no evidence, no observed rules, and a
// holiday observed on its own date.
func TestBusinessCalendar_AHandAuthoredVersionIsUnchanged(t *testing.T) {
	c := newCalFx(t)
	code, r := c.e.do("POST", "/v1/admin/calendars/"+c.code+"/versions", "steward-1", map[string]any{
		"effective_from": "2026-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6},
		"holidays": []map[string]any{hol("2026-12-26", "Boxing Day", "WEEKEND_TO_NEXT_WEEKDAY"), hol("2026-05-04", "May Day", "")}})
	mustStatus(t, 201, code, r)
	_, hasEvidence := r["source_authority"]
	assert.False(t, hasEvidence, "a hand-authored version has no import evidence")
	assert.NotEmpty(t, r["content_digest"])
	hs := r["holidays"].([]any)
	assert.Equal(t, "2026-12-28", hs[1].(map[string]any)["observed_date"], "the observed rule works on a hand-authored version too")
	_, hasObserved := hs[0].(map[string]any)["observed_rule"]
	assert.False(t, hasObserved)
}

func TestMigration000030_DownIsRefusedWhileImportsExist(t *testing.T) {
	c := newCalFx(t)
	c.imp(t, "2026-2", gbHolidays(), 201)
	_, err := pool.Exec(ctx, readMigration(t, "000030_business_calendar.down.sql"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "000030 down refused")
	// The up migration is re-runnable.
	_, err = pool.Exec(ctx, readMigration(t, "000030_business_calendar.up.sql"))
	require.NoError(t, err)
}
