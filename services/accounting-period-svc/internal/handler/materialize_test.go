package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

func TestMaterialize_CreatesOpenPeriodsHistoryAndOneEventEach(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	assert.Equal(t, 12, res.Created)
	assert.Equal(t, 0, res.Existing)
	assert.Empty(t, res.BoundaryDrift)
	require.Len(t, res.Periods, 12)
	for _, p := range res.Periods {
		assert.Equal(t, domain.StateOpen, p.State)
		assert.Equal(t, int64(1), p.Version)
		assert.Equal(t, "tenant-a", p.TenantID)
		assert.Equal(t, "v1", p.CalendarVersionID)
	}
	assert.Equal(t, "2026-03-01", e.periodByKey(res, "FY2026-P03").StartDate)
	assert.Equal(t, "2026-03-31", e.periodByKey(res, "FY2026-P03").EndDate)
	assert.Equal(t, 12, countType(e.outboxTypes(), "PeriodOpened"))
	assert.Len(t, e.store.History(), 12)
	assert.Equal(t, domain.CmdMaterialize, e.store.History()[0].Command)
}

func TestMaterialize_IsIdempotentAndNeverDuplicates(t *testing.T) {
	e := newEnv(t)

	// Same Idempotency-Key and body: the original result, flagged as a replay.
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "year start"},
		opts{key: "fixed"})
	require.Equal(t, http.StatusCreated, rec.Code)
	rec2 := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "year start"},
		opts{key: "fixed"})
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("Idempotent-Replay"))

	// A different key re-materialises the same year: nothing is created.
	again := e.materialize("v1", map[string]any{"reason": "second run"})
	assert.Equal(t, 0, again.Created)
	assert.Equal(t, 12, again.Existing)
	assert.Len(t, e.store.Periods(), 12, "no duplicates")
	assert.Equal(t, 12, countType(e.outboxTypes(), "PeriodOpened"), "no new events")
}

func TestMaterialize_ResolvesVersionThroughCalendarWhenNotPinned(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "reason": "r"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "v1", decode[service.MaterializeResult](t, rec).CalendarVersionID)

	// A calendar that resolves to a different calendar than requested is refused.
	e.cal.resolve[entityA] = service.CalendarRef{CalendarID: "other-cal", VersionID: "v1"}
	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "reason": "r"})
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
}

func TestMaterialize_RejectsNonContiguousOrInconsistentCalendar(t *testing.T) {
	cases := map[string]func(pv *service.CalendarPreview){
		"gap between normal periods": func(pv *service.CalendarPreview) { pv.Periods[4].StartDate = "2026-05-03" },
		"overlap":                    func(pv *service.CalendarPreview) { pv.Periods[4].StartDate = "2026-04-29" },
		"duplicate key":              func(pv *service.CalendarPreview) { pv.Periods[1].PeriodKey = pv.Periods[0].PeriodKey },
		"end before start":           func(pv *service.CalendarPreview) { pv.Periods[0].EndDate = "2025-12-31" },
		"unknown kind":               func(pv *service.CalendarPreview) { pv.Periods[0].Kind = "WEIRD" },
		"wrong calendar":             func(pv *service.CalendarPreview) { pv.CalendarID = "someone-else" },
		"wrong fiscal year":          func(pv *service.CalendarPreview) { pv.FiscalYear = 2027 },
		"wrong legal entity":         func(pv *service.CalendarPreview) { pv.LegalEntityID = "entity-z" },
		"no periods":                 func(pv *service.CalendarPreview) { pv.Periods = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			pv := e.cal.previews["v1"]
			pv.Periods = append([]service.CalendarPeriod(nil), pv.Periods...)
			mutate(&pv)
			e.cal.previews["v1"] = pv
			rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
				map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"})
			e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
			assert.Empty(t, e.store.Periods(), "nothing is stored from an invalid calendar")
			assert.Empty(t, e.store.Outbox())
		})
	}
}

func TestMaterialize_SpecialPeriodMayOverlapNormalOnes(t *testing.T) {
	e := newEnv(t)
	pv := e.cal.previews["v1"]
	pv.Periods = append(append([]service.CalendarPeriod(nil), pv.Periods...),
		service.CalendarPeriod{PeriodKey: "FY2026-ADJ", PeriodNo: 13, StartDate: "2026-12-31", EndDate: "2026-12-31", Kind: "SPECIAL"})
	e.cal.previews["v1"] = pv
	res := e.materialize("v1", nil)
	assert.Equal(t, 13, res.Created)
	assert.Equal(t, domain.KindSpecial, e.periodByKey(res, "FY2026-ADJ").Kind)
}

func TestMaterialize_CalendarUnavailableOrUnknown(t *testing.T) {
	e := newEnv(t)
	e.cal.err = domain.Errf(domain.CodeDependencyUnavailable, "fiscal-calendar-svc unreachable")
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"})
	e.expectErr(rec, http.StatusServiceUnavailable, domain.CodeDependencyUnavailable)

	e.cal.err = nil
	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "nope", "reason": "r"})
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)
	assert.Empty(t, e.store.Periods())
}

// A second calendar version yields SEPARATE periods; the first version's periods
// (and their state) are untouched, and re-running the first version against a
// calendar that has since changed never moves a boundary.
func TestMaterialize_ChangedCalendarNeverMovesExistingBoundaries(t *testing.T) {
	e := newEnv(t)
	v1 := e.materialize("v1", nil)
	before := e.periodByKey(v1, "FY2026-P03")
	e.must(e.cmd(before.PeriodID, "request-soft-close", closer))

	e.cal.previews["v2"] = quarterly("v2")
	v2 := e.materialize("v2", nil)
	assert.Equal(t, 4, v2.Created)
	assert.Len(t, e.store.Periods(), 16, "12 monthly (v1) + 4 quarterly (v2): separate rows")

	after := e.get(before.PeriodID)
	assert.Equal(t, before.StartDate, after.StartDate)
	assert.Equal(t, before.EndDate, after.EndDate)
	assert.Equal(t, domain.StateSoftClosed, after.State, "the v1 period keeps its own state")

	// The v1 preview now returns shifted boundaries (REF-04 edited the version).
	pv := e.cal.previews["v1"]
	pv.Periods = append([]service.CalendarPeriod(nil), pv.Periods...)
	pv.Periods[2].StartDate, pv.Periods[1].EndDate = "2026-03-05", "2026-03-04"
	e.cal.previews["v1"] = pv
	again := e.materialize("v1", map[string]any{"reason": "re-run after calendar edit"})
	assert.Equal(t, 0, again.Created)
	assert.Equal(t, []string{"FY2026-P02", "FY2026-P03"}, again.BoundaryDrift, "drift is reported, never applied")
	still := e.get(before.PeriodID)
	assert.Equal(t, "2026-03-01", still.StartDate)
	assert.Equal(t, "2026-03-31", still.EndDate)
}

func TestMaterialize_ValidationAndAuthz(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"}, opts{noKey: true})
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)

	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1"})
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)

	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 99, "calendar_version_id": "v1", "reason": "r"})
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)

	e.authz.deny[handlerMaterialize] = errDenied()
	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize",
		map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"})
	e.expectErr(rec, http.StatusForbidden, domain.CodeForbidden)
	assert.Empty(t, e.store.Periods())
	assert.Equal(t, 0, e.cal.previewCalls, "authorization comes before any upstream call")
}
