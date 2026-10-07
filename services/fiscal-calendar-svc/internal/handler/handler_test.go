package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/handler"
	"zoiko.io/fiscal-calendar-svc/internal/memstore"
	svcmiddleware "zoiko.io/fiscal-calendar-svc/internal/middleware"
	"zoiko.io/fiscal-calendar-svc/internal/service"
)

// ── harness ──────────────────────────────────────────────────────────────────

// stubAuthz denies per action; everything else is granted.
type stubAuthz struct {
	deny  map[string]error
	calls []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	a.calls = append(a.calls, action)
	return a.deny[action]
}

// stubHistory stands in for accounting-period-svc (REF-05).
type stubHistory struct {
	res   *service.PeriodHistory
	err   error
	calls int
}

func (s *stubHistory) CalendarUsage(_ context.Context, _, _, _ string) (*service.PeriodHistory, error) {
	s.calls++
	return s.res, s.err
}

type testEnv struct {
	t       *testing.T
	store   *memstore.Store
	authz   *stubAuthz
	history *stubHistory
	router  http.Handler
	now     time.Time
	keySeq  int
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, store: memstore.New(), authz: &stubAuthz{deny: map[string]error{}},
		history: &stubHistory{res: &service.PeriodHistory{}},
		now:     time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	svc := service.New(e.store).WithClock(func() time.Time { return e.now }).WithPeriodHistory(e.history)
	h := handler.New(svc, e.authz, zap.NewNop(), nil)
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

type opts struct {
	tenant, actor, entity string
	noKey                 bool
	key                   string
}

func (e *testEnv) do(method, path string, body any, o ...opts) *httptest.ResponseRecorder {
	e.t.Helper()
	op := opts{tenant: "tenant-a", actor: "proposer-1", entity: "entity-1"}
	if len(o) > 0 {
		op = o[0]
		if op.tenant == "" {
			op.tenant = "tenant-a"
		}
		if op.actor == "" {
			op.actor = "proposer-1"
		}
		if op.entity == "" {
			op.entity = "entity-1"
		}
	}
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Tenant-Id", op.tenant)
	req.Header.Set("X-Principal-Id", op.actor)
	req.Header.Set("X-Legal-Entity-Id", op.entity)
	req.Header.Set("X-Correlation-ID", "corr-test-1")
	if method == http.MethodPost && !op.noKey {
		key := op.key
		if key == "" {
			e.keySeq++
			key = fmt.Sprintf("key-%d", e.keySeq)
		}
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), "body: %s", rec.Body.String())
	return v
}

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *testEnv) wantErr(rec *httptest.ResponseRecorder, status int, code string) {
	e.t.Helper()
	require.Equal(e.t, status, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(e.t, code, decode[errBody](e.t, rec).Code)
}

type createResp = service.CalendarWithVersion

const monthsPattern = `{"type":"CALENDAR_MONTHS"}`

func createBody(code, scope, effFrom string) map[string]any {
	return map[string]any{
		"legal_entity_id": "entity-1", "code": code, "scope": scope, "reason": "initial calendar for the entity",
		"pattern": json.RawMessage(monthsPattern), "fiscal_year_start_month": 1, "fiscal_year_start_day": 1,
		"effective_from": effFrom,
	}
}

func (e *testEnv) createCal(code, scope, effFrom string, o ...opts) createResp {
	e.t.Helper()
	rec := e.do("POST", "/v1/fiscal-calendars", createBody(code, scope, effFrom), o...)
	require.Equal(e.t, http.StatusCreated, rec.Code, rec.Body.String())
	return decode[createResp](e.t, rec)
}

func (e *testEnv) approve(v domain.FiscalCalendarVersion, actor string) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/fiscal-calendar-versions/"+v.VersionID+":approve",
		map[string]any{"expected_version": v.Version, "reason": "reviewed by controller"}, opts{actor: actor})
}

func (e *testEnv) activate(v domain.FiscalCalendarVersion, actor string) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/fiscal-calendar-versions/"+v.VersionID+":activate",
		map[string]any{"expected_version": v.Version, "reason": "go live"}, opts{actor: actor})
}

// approveAndActivate drives a DRAFT version to ACTIVE and returns it.
func (e *testEnv) approveAndActivate(v domain.FiscalCalendarVersion) domain.FiscalCalendarVersion {
	e.t.Helper()
	rec := e.approve(v, "controller-1")
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	appr := decode[domain.FiscalCalendarVersion](e.t, rec)
	rec = e.activate(appr, "controller-1")
	require.Equal(e.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[domain.FiscalCalendarVersion](e.t, rec)
}

func (e *testEnv) propose(calendarID string, calVersion int64, effFrom string, o ...opts) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/fiscal-calendars/"+calendarID+"/versions:propose-change", map[string]any{
		"expected_version": calVersion, "reason": "move the fiscal year start",
		"pattern": json.RawMessage(`{"type":"CALENDAR_MONTHS"}`), "fiscal_year_start_month": 4, "fiscal_year_start_day": 1,
		"effective_from": effFrom,
	}, o...)
}

func (e *testEnv) getCal(id string, query string, o ...opts) *httptest.ResponseRecorder {
	return e.do("GET", "/v1/fiscal-calendars/"+id+query, nil, o...)
}

func (e *testEnv) calVersion(id string) int64 {
	e.t.Helper()
	rec := e.getCal(id, "")
	require.Equal(e.t, 200, rec.Code)
	return decode[service.CalendarView](e.t, rec).Calendar.Version
}

type outboxEvent struct {
	Type    string
	Key     string
	Tenant  string
	Payload map[string]any
}

func (e *testEnv) events() []outboxEvent {
	var out []outboxEvent
	for _, o := range e.store.Outbox() {
		var env struct {
			EventType string         `json:"event_type"`
			TenantID  string         `json:"tenant_id"`
			Payload   map[string]any `json:"payload"`
		}
		require.NoError(e.t, json.Unmarshal(o.Payload, &env))
		out = append(out, outboxEvent{Type: env.EventType, Key: o.ObjectID, Tenant: env.TenantID, Payload: env.Payload})
	}
	return out
}

func eventTypes(evs []outboxEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ── create ───────────────────────────────────────────────────────────────────

func TestCreate_ReturnsDraftAndEmitsEvent(t *testing.T) {
	e := newEnv(t)
	res := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	assert.Equal(t, domain.CalendarDraft, res.Calendar.Status)
	assert.Equal(t, int64(1), res.Calendar.Version)
	assert.Equal(t, "entity-1", res.Calendar.LegalEntityID)
	assert.Equal(t, "STATUTORY", res.Calendar.Scope)
	assert.Equal(t, domain.VersionDraft, res.Version.Status)
	assert.Equal(t, 1, res.Version.VersionNo)
	assert.Equal(t, "proposer-1", res.Version.ProposedBy)
	assert.Equal(t, "CALENDAR_MONTHS", res.Version.Pattern.Type)
	assert.Equal(t, "START_YEAR", res.Version.Pattern.YearLabel, "defaults are canonicalised")

	evs := e.events()
	require.Len(t, evs, 1)
	assert.Equal(t, "FiscalCalendarCreated", evs[0].Type)
	assert.Equal(t, res.Calendar.CalendarID, evs[0].Key)
	assert.Equal(t, "tenant-a", evs[0].Tenant)
	p := evs[0].Payload
	assert.Equal(t, "tenant-a", p["tenant_id"])
	assert.Equal(t, res.Calendar.CalendarID, p["object_id"])
	assert.Equal(t, float64(1), p["object_version"])
	assert.Equal(t, "2026-01-01T00:00:00Z", p["effective_at"])
	assert.Equal(t, "2026-10-07T12:00:00Z", p["recorded_at"])
	assert.Equal(t, "proposer-1", p["actor"])
	assert.Equal(t, "corr-test-1", p["correlation_id"])
	assert.Equal(t, "initial calendar for the entity", p["reason"])
	assert.Equal(t, []string{handler.ActionPropose}, e.authz.calls)
	require.Len(t, e.store.History(), 1)
}

func TestCreate_Validation(t *testing.T) {
	e := newEnv(t)
	bad := func(mut func(m map[string]any)) *httptest.ResponseRecorder {
		b := createBody("MAIN", "STATUTORY", "2026-01-01")
		mut(b)
		return e.do("POST", "/v1/fiscal-calendars", b)
	}
	e.wantErr(bad(func(m map[string]any) { m["pattern"] = json.RawMessage(`{"type":"LUNAR"}`) }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) {
		m["pattern"] = json.RawMessage(`{"type":"WEEK_PATTERN","weeks":[10,10],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST"}`)
	}), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["fiscal_year_start_month"] = 13 }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["fiscal_year_start_day"] = 0 }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { delete(m, "effective_from") }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["effective_to"] = "2025-01-01" }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["code"] = "bad code!" }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["scope"] = "" }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["legal_entity_id"] = "entity-2" }), 422, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { delete(m, "reason") }), 400, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["surprise"] = true }), 400, "CONTEXT_INVALID")
	e.wantErr(bad(func(m map[string]any) { m["effective_from"] = "01/01/2026" }), 400, "CONTEXT_INVALID")
	assert.Empty(t, e.store.Calendars())
	assert.Empty(t, e.store.Outbox())

	e.wantErr(e.do("POST", "/v1/fiscal-calendars", createBody("MAIN", "S", "2026-01-01"), opts{noKey: true}), 400, "CONTEXT_INVALID")
}

func TestCreate_DuplicateCodePerEntity(t *testing.T) {
	e := newEnv(t)
	e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.wantErr(e.do("POST", "/v1/fiscal-calendars", createBody("MAIN", "OTHER", "2026-01-01")), 409, "DUPLICATE_CANDIDATE")
}

func TestCommands_MissingContext(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest("POST", "/v1/fiscal-calendars", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, 401, "CONTEXT_INVALID")

	req = httptest.NewRequest("POST", "/v1/fiscal-calendars", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, 401, "CONTEXT_INVALID")

	req = httptest.NewRequest("POST", "/v1/fiscal-calendars", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "p")
	req.Header.Set("Idempotency-Key", "k")
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, 400, "CONTEXT_INVALID") // no X-Legal-Entity-Id

	req = httptest.NewRequest("GET", "/v1/fiscal-calendars/x", nil)
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	e.wantErr(rec, 401, "CONTEXT_INVALID")
}

// ── lifecycle, SoD, concurrency ──────────────────────────────────────────────

func TestLifecycle_DraftApprovedActive(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")

	// DRAFT cannot be activated.
	e.wantErr(e.activate(*c.Version, "controller-1"), 409, "INVALID_TRANSITION")

	rec := e.approve(*c.Version, "controller-1")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	appr := decode[domain.FiscalCalendarVersion](t, rec)
	assert.Equal(t, domain.VersionApproved, appr.Status)
	assert.Equal(t, "controller-1", appr.ApprovedBy)
	assert.Equal(t, "reviewed by controller", appr.ApprovalReason)
	assert.Equal(t, int64(2), appr.Version)
	assert.Equal(t, `"`+appr.VersionID+`-v2"`, rec.Header().Get("ETag"))

	// APPROVED cannot be approved again.
	e.wantErr(e.approve(appr, "controller-2"), 409, "INVALID_TRANSITION")

	rec = e.activate(appr, "controller-1")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	act := decode[domain.FiscalCalendarVersion](t, rec)
	assert.Equal(t, domain.VersionActive, act.Status)
	assert.Equal(t, "controller-1", act.ActivatedBy)
	assert.Equal(t, 0, e.history.calls, "the first version of a calendar never needs REF-05")

	// ACTIVE cannot be activated or approved again.
	e.wantErr(e.activate(act, "controller-1"), 409, "INVALID_TRANSITION")
	e.wantErr(e.approve(act, "controller-2"), 409, "INVALID_TRANSITION")

	assert.Equal(t, int64(2), e.calVersion(c.Calendar.CalendarID))
	assert.Equal(t, []string{"FiscalCalendarCreated", "FiscalCalendarVersionActivated"}, eventTypes(e.events()))
	act2 := e.events()[1]
	assert.Equal(t, act.VersionID, act2.Key)
	assert.Equal(t, act.VersionID, act2.Payload["object_id"])
	assert.Equal(t, float64(act.Version), act2.Payload["object_version"])
	assert.Equal(t, "2026-01-01T00:00:00Z", act2.Payload["effective_at"])
	assert.Equal(t, "controller-1", act2.Payload["actor"])

	// Status history is complete and ordered.
	var path []string
	for _, h := range e.store.History() {
		path = append(path, string(h.FromStatus)+">"+string(h.ToStatus))
	}
	assert.Equal(t, []string{">DRAFT", "DRAFT>APPROVED", "APPROVED>ACTIVE"}, path)
}

func TestApprove_ProposerCannotApprove_SoDDenied(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	rec := e.approve(*c.Version, "proposer-1")
	e.wantErr(rec, 403, "SOD_DENIED")
	v := e.store.Versions()
	assert.Equal(t, domain.VersionDraft, v[0].Status, "a refused approval changes nothing")
	assert.Len(t, e.events(), 1)
}

func TestExpectedVersion_Required_AndStaleRefused(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/"+c.Version.VersionID+":approve", map[string]any{"reason": "r"}, opts{actor: "c"}), 400, "CONTEXT_INVALID")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/"+c.Version.VersionID+":approve", map[string]any{"expected_version": 0, "reason": "r"}, opts{actor: "c"}), 400, "CONTEXT_INVALID")
	stale := *c.Version
	stale.Version = 7
	e.wantErr(e.approve(stale, "controller-1"), 409, "VERSION_CONFLICT")

	// The header form is accepted too.
	req := httptest.NewRequest("POST", "/v1/fiscal-calendar-versions/"+c.Version.VersionID+":approve", bytes.NewReader([]byte(`{"reason":"r"}`)))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "controller-1")
	req.Header.Set("X-Legal-Entity-Id", "entity-1")
	req.Header.Set("Idempotency-Key", "hdr-1")
	req.Header.Set("X-Expected-Version", "1")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	// A second approval with the now-stale version is a conflict, not a success.
	e.wantErr(e.approve(*c.Version, "controller-2"), 409, "VERSION_CONFLICT")
}

func TestProposeChange_StaleCalendarVersion_Conflict(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.wantErr(e.propose(c.Calendar.CalendarID, 5, "2027-01-01"), 409, "VERSION_CONFLICT")
	rec := e.propose(c.Calendar.CalendarID, 1, "2027-01-01")
	require.Equal(t, 201, rec.Code, rec.Body.String())
	v2 := decode[domain.FiscalCalendarVersion](t, rec)
	assert.Equal(t, 2, v2.VersionNo)
	assert.Equal(t, domain.VersionDraft, v2.Status)
	assert.Equal(t, int64(2), e.calVersion(c.Calendar.CalendarID), "proposing bumps the calendar's version")
	// The same expected_version again is now stale.
	e.wantErr(e.propose(c.Calendar.CalendarID, 1, "2028-01-01"), 409, "VERSION_CONFLICT")
	assert.Equal(t, []string{"FiscalCalendarCreated", "FiscalCalendarChangeProposed"}, eventTypes(e.events()))
	e.wantErr(e.propose("00000000-0000-0000-0000-000000000000", 1, "2027-01-01"), 404, "NOT_FOUND")
}

func TestLegalEntityContextMustMatchObject(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	rec := e.do("POST", "/v1/fiscal-calendar-versions/"+c.Version.VersionID+":approve",
		map[string]any{"expected_version": 1, "reason": "r"}, opts{actor: "controller-1", entity: "entity-2"})
	e.wantErr(rec, 422, "CONTEXT_INVALID")
}

func TestUnknownCommandsAnd404s(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/"+c.Version.VersionID+":destroy", map[string]any{}), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/nocolon", map[string]any{}), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/fiscal-calendars/"+c.Calendar.CalendarID+"/versions:explode", map[string]any{}), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/calendar-transition-plans/x:approve", map[string]any{"expected_version": 1, "reason": "r"}), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/calendar-transition-plans/x:explode", map[string]any{}), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/not-a-uuid:approve", map[string]any{"expected_version": 1, "reason": "r"}), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendars/"+"00000000-0000-0000-0000-000000000000", nil), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendar-versions/00000000-0000-0000-0000-000000000000", nil), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/calendar-transition-plans/00000000-0000-0000-0000-000000000000", nil), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendars/00000000-0000-0000-0000-000000000000/versions", nil), 404, "NOT_FOUND")
}

// ── idempotency ──────────────────────────────────────────────────────────────

func TestIdempotentReplay_ReturnsOriginal_OneEvent(t *testing.T) {
	e := newEnv(t)
	body := createBody("MAIN", "STATUTORY", "2026-01-01")
	first := e.do("POST", "/v1/fiscal-calendars", body, opts{key: "same-key"})
	require.Equal(t, 201, first.Code)
	second := e.do("POST", "/v1/fiscal-calendars", body, opts{key: "same-key"})
	require.Equal(t, 200, second.Code, second.Body.String())
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
	assert.Equal(t, decode[createResp](t, first).Calendar.CalendarID, decode[createResp](t, second).Calendar.CalendarID)
	assert.Len(t, e.store.Calendars(), 1)
	assert.Len(t, e.store.Versions(), 1)
	assert.Len(t, e.events(), 1)

	// Same key, different request: refused, never silently re-run.
	other := createBody("OTHER", "STATUTORY", "2026-01-01")
	e.wantErr(e.do("POST", "/v1/fiscal-calendars", other, opts{key: "same-key"}), 422, "CONTEXT_INVALID")
	assert.Len(t, e.store.Calendars(), 1)
}

func TestIdempotentReplay_Activate_EmitsOnce_EvenWhenRefDependencyIsDown(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	rec := e.approve(*c.Version, "controller-1")
	appr := decode[domain.FiscalCalendarVersion](t, rec)
	body := map[string]any{"expected_version": appr.Version, "reason": "go live"}
	path := "/v1/fiscal-calendar-versions/" + appr.VersionID + ":activate"
	first := e.do("POST", path, body, opts{actor: "controller-1", key: "act-1"})
	require.Equal(t, 200, first.Code)
	e.history.err = errors.New("down")
	second := e.do("POST", path, body, opts{actor: "controller-1", key: "act-1"})
	require.Equal(t, 200, second.Code, "a replay returns the original result without re-checking dependencies")
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
	count := 0
	for _, ev := range e.events() {
		if ev.Type == "FiscalCalendarVersionActivated" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

// ── as-of, resolve, two versions ─────────────────────────────────────────────

// twoVersions builds a calendar with v1 effective 2026-01-01 and v2 effective
// 2027-04-01, both activated, and returns (calendar id, v1, v2).
func twoVersions(t *testing.T, e *testEnv) (string, domain.FiscalCalendarVersion, domain.FiscalCalendarVersion) {
	t.Helper()
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1 := e.approveAndActivate(*c.Version)
	rec := e.propose(c.Calendar.CalendarID, e.calVersion(c.Calendar.CalendarID), "2027-04-01")
	require.Equal(t, 201, rec.Code, rec.Body.String())
	d2 := decode[domain.FiscalCalendarVersion](t, rec)
	v2 := e.approveAndActivate(d2)
	return c.Calendar.CalendarID, v1, v2
}

func TestAsOf_ResolvesAcrossTwoVersions(t *testing.T) {
	e := newEnv(t)
	calID, v1, v2 := twoVersions(t, e)
	assert.Equal(t, 1, e.history.calls, "activating the second version asked REF-05 once")

	type view = service.CalendarView
	at := func(d string) *httptest.ResponseRecorder { return e.getCal(calID, "?as_of="+d) }

	rec := at("2026-06-15")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, v1.VersionID, decode[view](t, rec).Version.VersionID)
	rec = at("2027-03-31")
	assert.Equal(t, v1.VersionID, decode[view](t, rec).Version.VersionID, "the last day of v1")
	rec = at("2027-04-01")
	assert.Equal(t, v2.VersionID, decode[view](t, rec).Version.VersionID, "the first day of v2")
	rec = at("2031-01-01")
	assert.Equal(t, v2.VersionID, decode[view](t, rec).Version.VersionID, "v2 is open-ended")
	e.wantErr(at("2025-12-31"), 404, "NOT_FOUND") // before any version: never guessed
	e.wantErr(at("not-a-date"), 400, "CONTEXT_INVALID")

	// Without as_of: the version in force today (2026-10-07) is v1.
	rec = e.getCal(calID, "")
	cur := decode[view](t, rec)
	assert.Equal(t, v1.VersionID, cur.Version.VersionID)
	assert.Equal(t, "2026-10-07", cur.AsOf.String())
	assert.Equal(t, domain.CalendarActive, cur.Calendar.Status)

	// v1 was ended where v2 begins, and superseded - not deleted or rewritten.
	rec = e.do("GET", "/v1/fiscal-calendars/"+calID+"/versions", nil)
	items := decode[struct {
		Items []domain.FiscalCalendarVersion `json:"items"`
	}](t, rec).Items
	require.Len(t, items, 2)
	assert.Equal(t, domain.VersionSuperseded, items[0].Status)
	require.NotNil(t, items[0].EffectiveTo)
	assert.Equal(t, "2027-04-01", items[0].EffectiveTo.String())
	assert.Equal(t, v2.VersionID, items[0].SupersededByVersion)
	assert.Equal(t, "CALENDAR_MONTHS", items[0].Pattern.Type)
	assert.Equal(t, 1, items[0].FiscalYearStartMonth, "v1's definition is untouched by the change")
	assert.Equal(t, 4, items[1].FiscalYearStartMonth)
	assert.Equal(t, domain.VersionActive, items[1].Status)
	assert.Nil(t, items[1].EffectiveTo)

	// Events: v1 superseded, with a stable object identity, then v2 activated.
	evs := e.events()
	assert.Equal(t, []string{"FiscalCalendarCreated", "FiscalCalendarVersionActivated", "FiscalCalendarChangeProposed", "FiscalCalendarSuperseded", "FiscalCalendarVersionActivated"}, eventTypes(evs))
	assert.Equal(t, v1.VersionID, evs[3].Key)
	assert.Equal(t, v2.VersionID, evs[3].Payload["superseded_by_version_id"])
	assert.Equal(t, v1.VersionID, evs[4].Payload["previous_version_id"])
	assert.Equal(t, "2027-04-01T00:00:00Z", evs[4].Payload["effective_at"])
}

func TestResolve_ByEntityScopeDate(t *testing.T) {
	e := newEnv(t)
	calID, v1, v2 := twoVersions(t, e)
	resolve := func(q string) *httptest.ResponseRecorder { return e.do("GET", "/v1/fiscal-calendars:resolve?"+q, nil) }

	rec := resolve("legal_entity_id=entity-1&scope=STATUTORY&date=2026-05-05")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got := decode[service.Resolution](t, rec)
	assert.Equal(t, calID, got.CalendarID)
	assert.Equal(t, v1.VersionID, got.VersionID)
	assert.Equal(t, 1, got.VersionNo)
	assert.Equal(t, "2027-04-01", got.EffectiveTo.String())

	rec = resolve("legal_entity_id=entity-1&scope=STATUTORY&date=2027-04-01")
	assert.Equal(t, v2.VersionID, decode[service.Resolution](t, rec).VersionID)

	// No match is a typed 404 - never the nearest, default or latest calendar.
	e.wantErr(resolve("legal_entity_id=entity-1&scope=STATUTORY&date=2025-01-01"), 404, "NOT_FOUND")
	e.wantErr(resolve("legal_entity_id=entity-1&scope=MANAGEMENT&date=2026-05-05"), 404, "NOT_FOUND")
	e.wantErr(resolve("legal_entity_id=entity-2&scope=STATUTORY&date=2026-05-05"), 404, "NOT_FOUND")
	e.wantErr(resolve("legal_entity_id=entity-1&scope=STATUTORY"), 400, "CONTEXT_INVALID")
	e.wantErr(resolve("legal_entity_id=entity-1&date=2026-05-05"), 400, "CONTEXT_INVALID")
	e.wantErr(resolve("scope=STATUTORY&date=2026-05-05"), 400, "CONTEXT_INVALID")
	e.wantErr(resolve("legal_entity_id=entity-1&scope=STATUTORY&date=yesterday"), 400, "CONTEXT_INVALID")
}

func TestResolve_DraftAndApprovedVersionsDoNotResolve(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.wantErr(e.do("GET", "/v1/fiscal-calendars:resolve?legal_entity_id=entity-1&scope=STATUTORY&date=2026-05-05", nil), 404, "NOT_FOUND")
	rec := e.approve(*c.Version, "controller-1")
	require.Equal(t, 200, rec.Code)
	e.wantErr(e.do("GET", "/v1/fiscal-calendars:resolve?legal_entity_id=entity-1&scope=STATUTORY&date=2026-05-05", nil), 404, "NOT_FOUND")
}

// ── overlap: one in-force version per entity/scope/date ──────────────────────

func TestActivation_OverlapWithInForceVersion_Rejected(t *testing.T) {
	e := newEnv(t)
	c1 := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c1.Version) // open-ended from 2026-01-01

	// A second calendar for the same entity and scope, overlapping.
	c2 := e.createCal("SECOND", "STATUTORY", "2026-06-01")
	rec := e.approve(*c2.Version, "controller-1")
	require.Equal(t, 200, rec.Code)
	appr := decode[domain.FiscalCalendarVersion](t, rec)
	e.wantErr(e.activate(appr, "controller-1"), 409, "INVALID_TRANSITION")
	got := e.store.Versions()
	for _, v := range got {
		if v.VersionID == appr.VersionID {
			assert.Equal(t, domain.VersionApproved, v.Status, "a rejected activation changes nothing")
		}
	}

	// A bounded second calendar that ends before the first begins does not overlap.
	b := createBody("EARLIER", "STATUTORY", "2024-01-01")
	b["effective_to"] = "2026-01-01"
	rec = e.do("POST", "/v1/fiscal-calendars", b)
	require.Equal(t, 201, rec.Code, rec.Body.String())
	earlier := e.approveAndActivate(*decode[createResp](t, rec).Version)
	assert.Equal(t, domain.VersionActive, earlier.Status)

	// Parallel basis: another scope, or another entity, may run alongside.
	c3 := e.createCal("MGMT", "MANAGEMENT", "2026-01-01")
	assert.Equal(t, domain.VersionActive, e.approveAndActivate(*c3.Version).Status)
	b = createBody("OTHERENT", "STATUTORY", "2026-01-01")
	b["legal_entity_id"] = "entity-2"
	rec = e.do("POST", "/v1/fiscal-calendars", b, opts{entity: "entity-2"})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	v := decode[createResp](t, rec).Version
	rec = e.do("POST", "/v1/fiscal-calendar-versions/"+v.VersionID+":approve", map[string]any{"expected_version": 1, "reason": "r"}, opts{actor: "controller-1", entity: "entity-2"})
	require.Equal(t, 200, rec.Code)
	rec = e.do("POST", "/v1/fiscal-calendar-versions/"+v.VersionID+":activate", map[string]any{"expected_version": 2, "reason": "r"}, opts{actor: "controller-1", entity: "entity-2"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func TestActivation_NewVersionStartingBeforeActivePredecessor_Rejected(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	rec := e.propose(c.Calendar.CalendarID, e.calVersion(c.Calendar.CalendarID), "2025-01-01") // overlaps v1 entirely
	d2 := decode[domain.FiscalCalendarVersion](t, rec)
	appr := decode[domain.FiscalCalendarVersion](t, e.approve(d2, "controller-1"))
	e.wantErr(e.activate(appr, "controller-1"), 409, "INVALID_TRANSITION")
}

func TestActivation_SameStartAsPredecessor_Rejected(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	rec := e.propose(c.Calendar.CalendarID, e.calVersion(c.Calendar.CalendarID), "2026-01-01")
	d2 := decode[domain.FiscalCalendarVersion](t, rec)
	appr := decode[domain.FiscalCalendarVersion](t, e.approve(d2, "controller-1"))
	e.wantErr(e.activate(appr, "controller-1"), 409, "INVALID_TRANSITION")
}

// ── immutability ─────────────────────────────────────────────────────────────

func TestApprovedVersion_HasNoEditSurface_AndCorrectionIsANewVersion(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	appr := decode[domain.FiscalCalendarVersion](t, e.approve(*c.Version, "controller-1"))
	before := e.store.Versions()[0]

	for _, m := range []string{"PATCH", "PUT", "DELETE"} {
		rec := e.do(m, "/v1/fiscal-calendar-versions/"+appr.VersionID, map[string]any{"pattern": json.RawMessage(`{"type":"CALENDAR_MONTHS"}`)})
		assert.Contains(t, []int{404, 405}, rec.Code, m)
		rec = e.do(m, "/v1/fiscal-calendars/"+c.Calendar.CalendarID, map[string]any{"code": "X"})
		assert.Contains(t, []int{404, 405}, rec.Code, m)
	}
	// The protected fields cannot be smuggled into a lifecycle command either.
	rec := e.do("POST", "/v1/fiscal-calendar-versions/"+appr.VersionID+":activate", map[string]any{
		"expected_version": appr.Version, "reason": "r", "pattern": json.RawMessage(`{"type":"CALENDAR_MONTHS"}`), "effective_from": "2020-01-01"}, opts{actor: "controller-1"})
	e.wantErr(rec, 400, "CONTEXT_INVALID")

	after := e.store.Versions()[0]
	assert.Equal(t, before.Pattern, after.Pattern)
	assert.Equal(t, before.EffectiveFrom, after.EffectiveFrom)
	assert.Equal(t, before.FiscalYearStartMonth, after.FiscalYearStartMonth)

	// A correction is a new version; the approved one keeps its definition.
	rec = e.propose(c.Calendar.CalendarID, 1, "2026-07-01")
	require.Equal(t, 201, rec.Code)
	assert.Len(t, e.store.Versions(), 2)
	assert.Equal(t, 1, e.store.Versions()[0].FiscalYearStartMonth)
}

// ── transition plan: never rewrite history ───────────────────────────────────

// history reports posted/closed periods through the given date.
func (e *testEnv) postedThrough(date string) {
	d, err := domain.ParseDate(date)
	require.NoError(e.t, err)
	e.history.res = &service.PeriodHistory{HasPostedOrClosedPeriods: true, LatestPeriodEnd: &d}
}

func (e *testEnv) proposeApproved(calID, effFrom string) domain.FiscalCalendarVersion {
	e.t.Helper()
	rec := e.propose(calID, e.calVersion(calID), effFrom)
	require.Equal(e.t, 201, rec.Code, rec.Body.String())
	return decode[domain.FiscalCalendarVersion](e.t, e.approve(decode[domain.FiscalCalendarVersion](e.t, rec), "controller-1"))
}

func (e *testEnv) createPlan(to, from domain.FiscalCalendarVersion, affects bool, actor string) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/fiscal-calendar-versions/"+to.VersionID+"/transition-plan", map[string]any{
		"from_version_id": from.VersionID, "expected_version": to.Version, "reason": "carry balances over the boundary",
		"impact_assessment":      map[string]any{"summary": "year end moves from December to March", "periods_affected": 3},
		"mapping":                map[string]any{"FY2026-P12": []string{"FY2026-P12", "FY2027-P01"}},
		"affects_posted_periods": affects,
	}, opts{actor: actor})
}

func TestActivation_ChangeOverPostedHistory_RequiresApprovedTransitionPlan(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1 := e.approveAndActivate(*c.Version)
	e.postedThrough("2026-09-30")

	// v2 takes effect 2026-07-01: inside the posted range.
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")
	rec := e.activate(v2, "controller-1")
	e.wantErr(rec, 409, "TRANSITION_PLAN_REQUIRED")
	assert.Contains(t, decode[errBody](t, rec).Message, "2026-09-30")
	assert.Equal(t, domain.VersionActive, e.store.Versions()[0].Status, "v1 untouched by the refused activation")
	assert.Nil(t, e.store.Versions()[0].EffectiveTo, "v1's boundary is not moved")

	// A plan that does not declare posted-period impact does not satisfy the rule.
	planRec := e.createPlan(v2, v1, false, "proposer-1")
	require.Equal(t, 201, planRec.Code, planRec.Body.String())
	plan := decode[domain.CalendarTransitionPlan](t, planRec)
	approve := func(p domain.CalendarTransitionPlan, actor string) *httptest.ResponseRecorder {
		return e.do("POST", "/v1/calendar-transition-plans/"+p.PlanID+":approve", map[string]any{"expected_version": p.Version, "reason": "reviewed"}, opts{actor: actor})
	}
	e.wantErr(approve(plan, "proposer-1"), 403, "SOD_DENIED") // the plan's own proposer
	rec = approve(plan, "controller-1")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, domain.PlanApproved, decode[domain.CalendarTransitionPlan](t, rec).Status)
	e.wantErr(e.activate(v2, "controller-1"), 409, "TRANSITION_PLAN_REQUIRED")
	assert.Equal(t, domain.PlanApproved, e.store.Plans()[0].Status)
	e.wantErr(approve(decode[domain.CalendarTransitionPlan](t, rec), "controller-2"), 409, "INVALID_TRANSITION")
}

func TestActivation_WithApprovedPlan_Succeeds_AndRecordsPlanInEvent(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1 := e.approveAndActivate(*c.Version)
	e.postedThrough("2026-09-30")
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")

	// Not yet planned, and a PROPOSED plan is not enough.
	e.wantErr(e.activate(v2, "controller-1"), 409, "TRANSITION_PLAN_REQUIRED")
	planRec := e.createPlan(v2, v1, true, "proposer-1")
	require.Equal(t, 201, planRec.Code, planRec.Body.String())
	plan := decode[domain.CalendarTransitionPlan](t, planRec)
	assert.Equal(t, domain.PlanProposed, plan.Status)
	assert.True(t, plan.AffectsPostedPeriods)
	e.wantErr(e.activate(v2, "controller-1"), 409, "TRANSITION_PLAN_REQUIRED")

	rec := e.do("POST", "/v1/calendar-transition-plans/"+plan.PlanID+":approve", map[string]any{"expected_version": plan.Version, "reason": "reviewed"}, opts{actor: "controller-1"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = e.activate(v2, "controller-1")
	require.Equal(t, 200, rec.Code, rec.Body.String())

	evs := e.events()
	last := evs[len(evs)-1]
	assert.Equal(t, "FiscalCalendarVersionActivated", last.Type)
	assert.Equal(t, plan.PlanID, last.Payload["transition_plan_id"])
	// The boundary was ended at the new version's start; the old periods are not rewritten.
	got := e.store.Versions()[0]
	assert.Equal(t, domain.VersionSuperseded, got.Status)
	assert.Equal(t, "2026-07-01", got.EffectiveTo.String())
}

func TestPlanSoD_VersionProposerCannotApprovePlan(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1 := e.approveAndActivate(*c.Version)
	e.postedThrough("2026-09-30")
	// proposer-1 proposed v2 (propose uses the default actor); the plan is written by someone else.
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")
	plan := decode[domain.CalendarTransitionPlan](t, e.createPlan(v2, v1, true, "planner-9"))
	rec := e.do("POST", "/v1/calendar-transition-plans/"+plan.PlanID+":approve", map[string]any{"expected_version": plan.Version, "reason": "ok"}, opts{actor: "proposer-1"})
	e.wantErr(rec, 403, "SOD_DENIED")
	rec = e.do("POST", "/v1/calendar-transition-plans/"+plan.PlanID+":approve", map[string]any{"expected_version": 5, "reason": "ok"}, opts{actor: "controller-1"})
	e.wantErr(rec, 409, "VERSION_CONFLICT")
}

func TestPlan_RejectThenReplan(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1 := e.approveAndActivate(*c.Version)
	e.postedThrough("2026-09-30")
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")
	p1 := decode[domain.CalendarTransitionPlan](t, e.createPlan(v2, v1, true, "proposer-1"))
	e.wantErr(e.createPlan(v2, v1, true, "proposer-1"), 409, "DUPLICATE_CANDIDATE")
	rec := e.do("POST", "/v1/calendar-transition-plans/"+p1.PlanID+":reject", map[string]any{"expected_version": p1.Version, "reason": "mapping is wrong"}, opts{actor: "controller-1"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, domain.PlanRejected, decode[domain.CalendarTransitionPlan](t, rec).Status)
	e.wantErr(e.activate(v2, "controller-1"), 409, "TRANSITION_PLAN_REQUIRED")
	assert.Equal(t, 201, e.createPlan(v2, v1, true, "proposer-1").Code, "a rejected plan can be replaced")

	rec = e.do("GET", "/v1/calendar-transition-plans/"+p1.PlanID, nil)
	require.Equal(t, 200, rec.Code)
	assert.Equal(t, domain.PlanRejected, decode[domain.CalendarTransitionPlan](t, rec).Status)
}

func TestPlanCreate_Validation(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	v1draft := *c.Version
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")

	// From must have been in force: v1 is still only a DRAFT.
	e.wantErr(e.createPlan(v2, v1draft, true, "proposer-1"), 409, "INVALID_TRANSITION")
	v1 := e.approveAndActivate(v1draft)
	_ = v1
	// Stale to-version.
	stale := v2
	stale.Version = 99
	e.wantErr(e.createPlan(stale, v1, true, "proposer-1"), 409, "VERSION_CONFLICT")
	// From and to the same version / later version.
	e.wantErr(e.createPlan(v2, v2, true, "proposer-1"), 422, "CONTEXT_INVALID")
	// Missing impact assessment.
	rec := e.do("POST", "/v1/fiscal-calendar-versions/"+v2.VersionID+"/transition-plan", map[string]any{
		"from_version_id": v1.VersionID, "expected_version": v2.Version, "reason": "r", "impact_assessment": map[string]any{},
		"mapping": map[string]any{"a": []string{"b"}}, "affects_posted_periods": true}, opts{actor: "proposer-1"})
	e.wantErr(rec, 422, "CONTEXT_INVALID")
	// Posted-period impact without a mapping.
	rec = e.do("POST", "/v1/fiscal-calendar-versions/"+v2.VersionID+"/transition-plan", map[string]any{
		"from_version_id": v1.VersionID, "expected_version": v2.Version, "reason": "r", "impact_assessment": map[string]any{"s": "x"},
		"affects_posted_periods": true}, opts{actor: "proposer-1"})
	e.wantErr(rec, 422, "CONTEXT_INVALID")
	// Unknown to-version.
	rec = e.do("POST", "/v1/fiscal-calendar-versions/00000000-0000-0000-0000-000000000000/transition-plan", map[string]any{
		"from_version_id": v1.VersionID, "expected_version": 1, "reason": "r", "impact_assessment": map[string]any{"s": "x"}}, opts{actor: "proposer-1"})
	e.wantErr(rec, 404, "NOT_FOUND")
	assert.Empty(t, e.store.Plans())
}

func TestActivation_ChangeAfterPostedHistory_NeedsNoPlan(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	e.postedThrough("2026-09-30")
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-10-01") // the day after the last posted period
	rec := e.activate(v2, "controller-1")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, 1, e.history.calls)
}

func TestActivation_NothingPosted_NeedsNoPlan(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	e.history.res = &service.PeriodHistory{HasPostedOrClosedPeriods: false}
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2026-07-01")
	require.Equal(t, 200, e.activate(v2, "controller-1").Code)
}

func TestActivation_PostedButUnknownEnd_IsTreatedAsTouchingHistory(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	e.history.res = &service.PeriodHistory{HasPostedOrClosedPeriods: true}
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2030-01-01")
	e.wantErr(e.activate(v2, "controller-1"), 409, "TRANSITION_PLAN_REQUIRED")
}

func TestActivation_FailsClosed_WhenPeriodHistoryUnavailable(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2027-01-01")

	e.history.err = errors.New("connection refused")
	rec := e.activate(v2, "controller-1")
	e.wantErr(rec, 503, "DEPENDENCY_UNAVAILABLE")
	assert.Contains(t, decode[errBody](t, rec).Message, "failing closed")
	assert.NotContains(t, rec.Body.String(), "connection refused", "internal error text is not leaked")
	for _, v := range e.store.Versions() {
		if v.VersionID == v2.VersionID {
			assert.Equal(t, domain.VersionApproved, v.Status)
		}
	}

	// A nil answer is as bad as an error.
	e.history.err, e.history.res = nil, nil
	e.wantErr(e.activate(v2, "controller-1"), 503, "DEPENDENCY_UNAVAILABLE")

	// And with the dependency back, it proceeds.
	e.history.res = &service.PeriodHistory{}
	require.Equal(t, 200, e.activate(v2, "controller-1").Code)
}

func TestActivation_FailsClosed_WhenNoPeriodHistoryClientConfigured(t *testing.T) {
	e := newEnv(t)
	svc := service.New(e.store).WithClock(func() time.Time { return e.now }) // no client
	h := handler.New(svc, e.authz, zap.NewNop(), nil)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	first := e.approveAndActivate(*c.Version) // the first version needs no REF-05 answer
	assert.Equal(t, domain.VersionActive, first.Status)
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2027-01-01")
	e.wantErr(e.activate(v2, "controller-1"), 503, "DEPENDENCY_UNAVAILABLE")
}

// ── events, atomicity, tenants, authz ────────────────────────────────────────

func TestEvents_AtomicWithStateChange(t *testing.T) {
	e := newEnv(t)
	e.store.FailEnqueue = true
	rec := e.do("POST", "/v1/fiscal-calendars", createBody("MAIN", "STATUTORY", "2026-01-01"), opts{key: "k-atomic"})
	e.wantErr(rec, 503, "DEPENDENCY_UNAVAILABLE")
	assert.Empty(t, e.store.Calendars(), "no state without its event")
	assert.Empty(t, e.store.Versions())
	assert.Empty(t, e.store.History())

	// The failed attempt left no idempotency record: the retry runs for real.
	e.store.FailEnqueue = false
	rec = e.do("POST", "/v1/fiscal-calendars", createBody("MAIN", "STATUTORY", "2026-01-01"), opts{key: "k-atomic"})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	assert.Len(t, e.events(), 1)
}

func TestEvents_ActivationFailureRollsBackSupersession(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.approveAndActivate(*c.Version)
	v2 := e.proposeApproved(c.Calendar.CalendarID, "2027-01-01")
	n := len(e.events())
	e.store.FailEnqueue = true
	e.wantErr(e.activate(v2, "controller-1"), 503, "DEPENDENCY_UNAVAILABLE")
	e.store.FailEnqueue = false
	assert.Len(t, e.events(), n)
	vs := e.store.Versions()
	assert.Equal(t, domain.VersionActive, vs[0].Status, "the predecessor was not superseded by a failed activation")
	assert.Nil(t, vs[0].EffectiveTo)
	assert.Equal(t, domain.VersionApproved, vs[1].Status)
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	calID, v1, _ := twoVersions(t, e)
	b := opts{tenant: "tenant-b"}

	e.wantErr(e.getCal(calID, "", b), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendars/"+calID+"/versions", nil, b), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendar-versions/"+v1.VersionID, nil, b), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendar-versions/"+v1.VersionID+"/periods-preview?fiscal_year=2026", nil, b), 404, "NOT_FOUND")
	e.wantErr(e.do("GET", "/v1/fiscal-calendars:resolve?legal_entity_id=entity-1&scope=STATUTORY&date=2026-05-05", nil, b), 404, "NOT_FOUND")
	e.wantErr(e.do("POST", "/v1/fiscal-calendar-versions/"+v1.VersionID+":approve", map[string]any{"expected_version": 1, "reason": "r"}, opts{tenant: "tenant-b", actor: "controller-9"}), 404, "NOT_FOUND")
	e.wantErr(e.propose(calID, 1, "2030-01-01", b), 404, "NOT_FOUND")

	// Tenant B can use the same code, scope and idempotency key independently.
	body := createBody("MAIN", "STATUTORY", "2026-01-01")
	rec := e.do("POST", "/v1/fiscal-calendars", body, opts{tenant: "tenant-b", key: "shared-key"})
	require.Equal(t, 201, rec.Code, rec.Body.String())
	other := decode[createResp](t, rec)
	assert.NotEqual(t, calID, other.Calendar.CalendarID)
	assert.Equal(t, "tenant-b", other.Calendar.TenantID)
	// ...and does not see tenant A's calendar in a resolve.
	rec = e.do("GET", "/v1/fiscal-calendars:resolve?legal_entity_id=entity-1&scope=STATUTORY&date=2026-05-05", nil, b)
	e.wantErr(rec, 404, "NOT_FOUND") // B's own is still DRAFT
	for _, ev := range e.events() {
		if ev.Payload["object_id"] == other.Calendar.CalendarID {
			assert.Equal(t, "tenant-b", ev.Tenant)
		}
	}
}

func TestAuthorization_ActionsPerCommand_Denied403_Unavailable503(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	e.authz.calls = nil

	rec := e.approve(*c.Version, "controller-1")
	require.Equal(t, 200, rec.Code)
	appr := decode[domain.FiscalCalendarVersion](t, rec)
	e.activate(appr, "controller-1")
	e.propose(c.Calendar.CalendarID, e.calVersion(c.Calendar.CalendarID), "2027-01-01")
	assert.Equal(t, []string{handler.ActionApprove, handler.ActionActivate, handler.ActionPropose}, e.authz.calls)

	e2 := newEnv(t)
	e2.authz.deny[handler.ActionPropose] = domain.ErrAuthorizationDenied
	e2.wantErr(e2.do("POST", "/v1/fiscal-calendars", createBody("MAIN", "S", "2026-01-01")), 403, "FORBIDDEN")
	assert.Empty(t, e2.store.Calendars())

	e3 := newEnv(t)
	c3 := e3.createCal("MAIN", "STATUTORY", "2026-01-01")
	e3.authz.deny[handler.ActionApprove] = domain.ErrAuthorizationDenied
	e3.wantErr(e3.approve(*c3.Version, "controller-1"), 403, "FORBIDDEN")
	e3.authz.deny[handler.ActionApprove] = nil
	appr3 := decode[domain.FiscalCalendarVersion](t, e3.approve(*c3.Version, "controller-1"))
	e3.authz.deny[handler.ActionActivate] = domain.ErrAuthzServiceUnavailable
	e3.wantErr(e3.activate(appr3, "controller-1"), 503, "DEPENDENCY_UNAVAILABLE")
	assert.Equal(t, domain.VersionApproved, e3.store.Versions()[0].Status)
}

// ── periods-preview: the REF-05 contract ─────────────────────────────────────

func TestPeriodsPreview_ContractShapeAndStatusGate(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	path := func(vid string, q string) string {
		return "/v1/fiscal-calendar-versions/" + vid + "/periods-preview" + q
	}
	// DRAFT: not previewable.
	e.wantErr(e.do("GET", path(c.Version.VersionID, "?fiscal_year=2026"), nil), 409, "INVALID_TRANSITION")

	appr := decode[domain.FiscalCalendarVersion](t, e.approve(*c.Version, "controller-1"))
	rec := e.do("GET", path(appr.VersionID, "?fiscal_year=2026"), nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())

	// Exact top-level and per-period keys: this is what REF-05 codes against.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"calendar_id", "version_id", "version_no", "legal_entity_id", "fiscal_year", "periods"}, keys)
	var periods []map[string]any
	require.NoError(t, json.Unmarshal(raw["periods"], &periods))
	require.Len(t, periods, 12)
	pk := make([]string, 0)
	for k := range periods[0] {
		pk = append(pk, k)
	}
	assert.ElementsMatch(t, []string{"period_key", "period_no", "start_date", "end_date", "kind"}, pk)

	pv := decode[service.PeriodsPreview](t, rec)
	assert.Equal(t, c.Calendar.CalendarID, pv.CalendarID)
	assert.Equal(t, appr.VersionID, pv.VersionID)
	assert.Equal(t, 1, pv.VersionNo)
	assert.Equal(t, "entity-1", pv.LegalEntityID)
	assert.Equal(t, 2026, pv.FiscalYear)
	assert.Equal(t, "FY2026-P01", pv.Periods[0].PeriodKey)
	assert.Equal(t, "2026-01-01", pv.Periods[0].StartDate.String())
	assert.Equal(t, "2026-01-31", pv.Periods[0].EndDate.String())
	assert.Equal(t, domain.PeriodNormal, pv.Periods[0].Kind)
	assert.Equal(t, "2026-02-28", pv.Periods[1].EndDate.String())

	// Deterministic: byte-identical on repeat, and still available once ACTIVE / SUPERSEDED.
	again := e.do("GET", path(appr.VersionID, "?fiscal_year=2026"), nil)
	assert.Equal(t, rec.Body.String(), again.Body.String())
	act := decode[domain.FiscalCalendarVersion](t, e.activate(appr, "controller-1"))
	assert.Equal(t, rec.Body.String(), e.do("GET", path(act.VersionID, "?fiscal_year=2026"), nil).Body.String())

	e.wantErr(e.do("GET", path(appr.VersionID, ""), nil), 400, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", path(appr.VersionID, "?fiscal_year=abc"), nil), 400, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", path(appr.VersionID, "?fiscal_year=1500"), nil), 422, "CONTEXT_INVALID")
	e.wantErr(e.do("GET", path("00000000-0000-0000-0000-000000000000", "?fiscal_year=2026"), nil), 404, "NOT_FOUND")
}

func TestPeriodsPreview_WeekPatternAndAdjustmentPeriod(t *testing.T) {
	e := newEnv(t)
	b := createBody("RETAIL", "STATUTORY", "2026-01-01")
	b["pattern"] = json.RawMessage(`{"type":"WEEK_PATTERN","weeks":[4,4,5,4,4,5,4,4,5,4,4,5],"week_start":"MONDAY","extra_week_rule":"ADD_TO_LAST","special_periods":[{"key":"ADJ","position":"AFTER_LAST","zero_length":true}]}`)
	rec := e.do("POST", "/v1/fiscal-calendars", b)
	require.Equal(t, 201, rec.Code, rec.Body.String())
	v := decode[createResp](t, rec).Version
	appr := decode[domain.FiscalCalendarVersion](t, e.approve(*v, "controller-1"))
	rec = e.do("GET", "/v1/fiscal-calendar-versions/"+appr.VersionID+"/periods-preview?fiscal_year=2029", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	pv := decode[service.PeriodsPreview](t, rec)
	require.Len(t, pv.Periods, 13)
	assert.Equal(t, "FY2029-P12", pv.Periods[11].PeriodKey)
	assert.Equal(t, "2030-01-06", pv.Periods[11].EndDate.String(), "53-week year: P12 absorbs the extra week")
	assert.Equal(t, "FY2029-ADJ", pv.Periods[12].PeriodKey)
	assert.Equal(t, domain.PeriodSpecial, pv.Periods[12].Kind)
	assert.Equal(t, 13, pv.Periods[12].PeriodNo)
}

func TestGetCalendar_ETag(t *testing.T) {
	e := newEnv(t)
	c := e.createCal("MAIN", "STATUTORY", "2026-01-01")
	rec := e.getCal(c.Calendar.CalendarID, "")
	assert.Equal(t, `"`+c.Calendar.CalendarID+`-v1"`, rec.Header().Get("ETag"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	cur := decode[service.CalendarView](t, rec)
	assert.Nil(t, cur.Version, "a DRAFT calendar has no version in force")
}
