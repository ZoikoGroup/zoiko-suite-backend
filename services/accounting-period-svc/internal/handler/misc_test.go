package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	id := e.periodByKey(res, "FY2026-P03").PeriodID
	e.drive(id, domain.StateHardClosed)
	b := opts{tenant: "tenant-b", actor: "closer"}

	// Tenant B cannot read, list, resolve or operate on tenant A's periods.
	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods/"+id, nil, b), http.StatusNotFound, domain.CodeNotFound)
	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods/"+id+"/state-history", nil, b), http.StatusNotFound, domain.CodeNotFound)
	list := e.do(http.MethodGet, "/v1/accounting-periods?legal_entity_id="+entityA, nil, b)
	require.Equal(t, http.StatusOK, list.Code)
	assert.Empty(t, decode[struct {
		Items []domain.Period `json:"items"`
	}](t, list).Items)
	rec := e.do(http.MethodGet, "/v1/accounting-periods:resolve?legal_entity_id="+entityA+"&date=2026-03-10", nil, b)
	e.expectErr(rec, http.StatusNotFound, domain.CodePeriodNotFound)
	rec = e.do(http.MethodGet, "/v1/accounting-periods:status-by-key?legal_entity_id="+entityA+"&period_key=FY2026-P03", nil, b)
	e.expectErr(rec, http.StatusNotFound, domain.CodePeriodNotFound)
	usage := e.do(http.MethodGet, "/v1/calendar-usage?calendar_id="+calID, nil, b)
	assert.Nil(t, decode[service.CalendarUsageResult](t, usage).LatestPeriodEnd)
	rec = e.do(http.MethodPost, "/v1/accounting-periods/"+id+":reclose",
		map[string]any{"expected_version": 4, "reason": "x", "acc14_workflow_ref": "w", "control_snapshot_ref": "s"}, b)
	e.expectErr(rec, http.StatusNotFound, domain.CodeNotFound)

	// The same Idempotency-Key in two tenants is two different keys.
	body := map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026, "calendar_version_id": "v1", "reason": "r"}
	ra := e.do(http.MethodPost, "/v1/accounting-periods:materialize", body, opts{tenant: "tenant-a", key: "shared-key"})
	rb := e.do(http.MethodPost, "/v1/accounting-periods:materialize", body, opts{tenant: "tenant-b", key: "shared-key"})
	require.Equal(t, http.StatusOK, ra.Code, "tenant A already materialised: nothing created")
	require.Equal(t, http.StatusCreated, rb.Code, "tenant B gets its own periods")
	assert.NotEqual(t, decode[service.MaterializeResult](t, rb).Periods[0].PeriodID, res.Periods[0].PeriodID)
	assert.Len(t, e.store.Periods(), 24)

	// Tenant A's gate answer is unaffected by tenant B's OPEN periods.
	assert.False(t, resolved(t, e, entityA, "2026-03-10").PostingAllowed)
	// Missing tenant is refused outright.
	rec = e.do(http.MethodGet, "/v1/accounting-periods/"+id, nil, opts{noTenant: true})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestCalendarUsage(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/v1/calendar-usage?calendar_id="+calID+"&calendar_version_id=v1", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"latest_period_end":null,"has_posted_or_closed_periods":false}`, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	res := e.materialize("v1", nil)
	u := decode[service.CalendarUsageResult](t, e.do(http.MethodGet, "/v1/calendar-usage?calendar_id="+calID+"&calendar_version_id=v1", nil))
	require.NotNil(t, u.LatestPeriodEnd)
	assert.Equal(t, "2026-12-31", *u.LatestPeriodEnd)
	assert.False(t, u.HasPostedOrClosedPeriod, "materialised-but-OPEN periods do not count as closed")

	e.drive(e.periodByKey(res, "FY2026-P01").PeriodID, domain.StateSoftClosed)
	u = decode[service.CalendarUsageResult](t, e.do(http.MethodGet, "/v1/calendar-usage?calendar_id="+calID, nil))
	assert.True(t, u.HasPostedOrClosedPeriod, "any state other than OPEN counts as closed")

	// Per-version filter, and an unrelated calendar.
	e.cal.previews["v2"] = quarterly("v2")
	e.materialize("v2", nil)
	u = decode[service.CalendarUsageResult](t, e.do(http.MethodGet, "/v1/calendar-usage?calendar_id="+calID+"&calendar_version_id=v2", nil))
	assert.False(t, u.HasPostedOrClosedPeriod, "v2 has no closed period")
	u = decode[service.CalendarUsageResult](t, e.do(http.MethodGet, "/v1/calendar-usage?calendar_id=nope", nil))
	assert.Nil(t, u.LatestPeriodEnd)

	rec = e.do(http.MethodGet, "/v1/calendar-usage", nil)
	e.expectErr(rec, http.StatusUnprocessableEntity, domain.CodeContextInvalid)
}

func TestReads_GetListHistory(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	id := e.periodByKey(res, "FY2026-P01").PeriodID
	e.drive(id, domain.StateSoftClosed)

	rec := e.do(http.MethodGet, "/v1/accounting-periods/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, `"`+id+`-v2"`, rec.Header().Get("ETag"))

	open := decode[struct {
		Items []domain.Period `json:"items"`
	}](t, e.do(http.MethodGet, "/v1/accounting-periods?legal_entity_id="+entityA+"&state=OPEN", nil))
	assert.Len(t, open.Items, 11, "ListOpenPeriods excludes the soft-closed one")
	for _, p := range open.Items {
		assert.Equal(t, domain.StateOpen, p.State)
	}
	assert.Equal(t, "FY2026-P02", open.Items[0].PeriodKey, "ordered by start date")

	page := decode[struct {
		Items []domain.Period `json:"items"`
	}](t, e.do(http.MethodGet, "/v1/accounting-periods?legal_entity_id="+entityA+"&limit=2&offset=1", nil))
	assert.Len(t, page.Items, 2)

	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods?legal_entity_id="+entityA+"&state=BOGUS", nil), http.StatusUnprocessableEntity, domain.CodeContextInvalid)
	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods", nil), http.StatusUnprocessableEntity, domain.CodeContextInvalid)
	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods?legal_entity_id=x&limit=0", nil), http.StatusBadRequest, domain.CodeContextInvalid)
	e.expectErr(e.do(http.MethodGet, "/v1/accounting-periods/018f0000-0000-7000-8000-000000000000", nil), http.StatusNotFound, domain.CodeNotFound)

	hist := decode[struct {
		Items []domain.HistoryEntry `json:"items"`
	}](t, e.do(http.MethodGet, "/v1/accounting-periods/"+id+"/state-history", nil)).Items
	require.Len(t, hist, 2)
	assert.Equal(t, domain.StateOpen, hist[0].ToState)
	assert.Equal(t, domain.StateSoftClosed, hist[1].ToState)
	assert.Equal(t, closer, hist[1].RequestedBy)
}

func TestContextRequired(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize", map[string]any{"reason": "x"}, opts{noTenant: true})
	e.expectErr(rec, http.StatusUnauthorized, domain.CodeContextInvalid)
	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize", "{not json")
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)
	rec = e.do(http.MethodPost, "/v1/accounting-periods:materialize", map[string]any{"reason": "x", "surprise": 1})
	e.expectErr(rec, http.StatusBadRequest, domain.CodeContextInvalid)
}
