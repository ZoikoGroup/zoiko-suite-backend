package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/handler"
	"zoiko.io/accounting-period-svc/internal/memstore"
	svcmiddleware "zoiko.io/accounting-period-svc/internal/middleware"
	"zoiko.io/accounting-period-svc/internal/service"
)

// ── stubs ────────────────────────────────────────────────────────────────────

// stubAuthz denies per action; everything else is granted.
type stubAuthz struct {
	deny       map[string]error
	calls      []string
	calledEnts []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, entity, action string) error {
	a.calls = append(a.calls, action)
	a.calledEnts = append(a.calledEnts, entity)
	return a.deny[action]
}

// stubProv is the ProvenanceVerifier stub: err is returned for every call.
type stubProv struct {
	err   error
	calls []service.ProvenanceRequest
}

func (p *stubProv) Verify(_ context.Context, r service.ProvenanceRequest) error {
	p.calls = append(p.calls, r)
	return p.err
}

// stubCal is the CalendarClient stub.
type stubCal struct {
	previews     map[string]service.CalendarPreview // by version id
	resolve      map[string]service.CalendarRef     // by legal entity id
	err          error
	previewCalls int
}

func (c *stubCal) Resolve(_ context.Context, _, entity, _, _ string) (*service.CalendarRef, error) {
	if c.err != nil {
		return nil, c.err
	}
	r, ok := c.resolve[entity]
	if !ok {
		return nil, domain.Errf(domain.CodeNotFound, "no calendar")
	}
	return &r, nil
}

func (c *stubCal) PeriodsPreview(_ context.Context, _, vid string, _ int) (*service.CalendarPreview, error) {
	c.previewCalls++
	if c.err != nil {
		return nil, c.err
	}
	pv, ok := c.previews[vid]
	if !ok {
		return nil, domain.Errf(domain.CodeNotFound, "no version")
	}
	return &pv, nil
}

const (
	entityA = "entity-a"
	calID   = "cal-1"
)

// monthly is a 12-period NORMAL preview for 2026.
func monthly(version string) service.CalendarPreview {
	pv := service.CalendarPreview{CalendarID: calID, VersionID: version, VersionNo: 1, LegalEntityID: entityA, FiscalYear: 2026}
	for m := 1; m <= 12; m++ {
		start := time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, -1)
		pv.Periods = append(pv.Periods, service.CalendarPeriod{
			PeriodKey: fmt.Sprintf("FY2026-P%02d", m), PeriodNo: m,
			StartDate: start.Format("2006-01-02"), EndDate: end.Format("2006-01-02"), Kind: "NORMAL"})
	}
	return pv
}

// quarterly covers the same year with 4 NORMAL periods (a "changed calendar").
func quarterly(version string) service.CalendarPreview {
	pv := service.CalendarPreview{CalendarID: calID, VersionID: version, VersionNo: 2, LegalEntityID: entityA, FiscalYear: 2026}
	for q := 0; q < 4; q++ {
		start := time.Date(2026, time.Month(q*3+1), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 3, -1)
		pv.Periods = append(pv.Periods, service.CalendarPeriod{
			PeriodKey: fmt.Sprintf("FY2026-Q%d", q+1), PeriodNo: q + 1,
			StartDate: start.Format("2006-01-02"), EndDate: end.Format("2006-01-02"), Kind: "NORMAL"})
	}
	return pv
}

// ── harness ──────────────────────────────────────────────────────────────────

type testEnv struct {
	t      *testing.T
	store  *memstore.Store
	authz  *stubAuthz
	prov   *stubProv
	cal    *stubCal
	router http.Handler
	now    time.Time
	keySeq int
	refSeq int
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, store: memstore.New(), authz: &stubAuthz{deny: map[string]error{}}, prov: &stubProv{},
		cal: &stubCal{previews: map[string]service.CalendarPreview{"v1": monthly("v1")},
			resolve: map[string]service.CalendarRef{entityA: {CalendarID: calID, VersionID: "v1"}}},
		now: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)}
	svc := service.New(e.store, e.cal, e.prov).WithClock(func() time.Time { return e.now })
	h := handler.New(svc, e.authz, zap.NewNop(), nil)
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

type opts struct {
	tenant, actor string
	noKey         bool
	key           string
	noTenant      bool
}

func (e *testEnv) do(method, path string, body any, o ...opts) *httptest.ResponseRecorder {
	e.t.Helper()
	op := opts{tenant: "tenant-a", actor: "closer"}
	if len(o) > 0 {
		op = o[0]
		if op.tenant == "" && !op.noTenant {
			op.tenant = "tenant-a"
		}
		if op.actor == "" {
			op.actor = "closer"
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
	if op.tenant != "" {
		req.Header.Set("X-Tenant-Id", op.tenant)
	}
	req.Header.Set("X-Principal-Id", op.actor)
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
	Code           string `json:"code"`
	Message        string `json:"message"`
	PostingAllowed *bool  `json:"posting_allowed"`
}

func (e *testEnv) expectErr(rec *httptest.ResponseRecorder, status int, code domain.Code) errBody {
	e.t.Helper()
	require.Equal(e.t, status, rec.Code, "body: %s", rec.Body.String())
	b := decode[errBody](e.t, rec)
	require.Equal(e.t, string(code), b.Code, "message: %s", b.Message)
	return b
}

// ── domain helpers ───────────────────────────────────────────────────────────

func (e *testEnv) materialize(version string, extra map[string]any, o ...opts) service.MaterializeResult {
	e.t.Helper()
	body := map[string]any{"legal_entity_id": entityA, "calendar_id": calID, "fiscal_year": 2026,
		"calendar_version_id": version, "reason": "year start"}
	for k, v := range extra {
		body[k] = v
	}
	rec := e.do(http.MethodPost, "/v1/accounting-periods:materialize", body, o...)
	require.Contains(e.t, []int{http.StatusOK, http.StatusCreated}, rec.Code, "body: %s", rec.Body.String())
	return decode[service.MaterializeResult](e.t, rec)
}

func (e *testEnv) get(id string, o ...opts) domain.Period {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/v1/accounting-periods/"+id, nil, o...)
	require.Equal(e.t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	return decode[domain.Period](e.t, rec)
}

func (e *testEnv) periodByKey(res service.MaterializeResult, key string) domain.Period {
	e.t.Helper()
	for _, p := range res.Periods {
		if p.PeriodKey == key {
			return p
		}
	}
	e.t.Fatalf("period %s not materialised", key)
	return domain.Period{}
}

// cmd sends a state command with fresh ACC-14 refs and the CURRENT version.
func (e *testEnv) cmd(id, name, actor string, mod ...func(map[string]any)) *httptest.ResponseRecorder {
	e.t.Helper()
	p := e.get(id)
	e.refSeq++
	body := map[string]any{
		"expected_version": p.Version, "reason": "close run " + name,
		"acc14_workflow_ref": fmt.Sprintf("wf-%d", e.refSeq), "control_snapshot_ref": fmt.Sprintf("snap-%d", e.refSeq),
	}
	if name == "authorize-reopen" {
		body["reopen_scope"] = map[string]any{"book_scope": "", "module_scope": ""}
		body["expires_at"] = e.now.Add(time.Hour).Format(time.RFC3339)
	}
	for _, m := range mod {
		m(body)
	}
	return e.do(http.MethodPost, "/v1/accounting-periods/"+id+":"+name, body, opts{actor: actor})
}

func (e *testEnv) must(rec *httptest.ResponseRecorder) domain.Period {
	e.t.Helper()
	require.Equal(e.t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	return decode[domain.Period](e.t, rec)
}

const (
	closer   = "closer-1"
	approver = "approver-1"
)

// drive moves a period to target through legal commands (soft by closer, the rest by approver).
func (e *testEnv) drive(id string, target domain.State) {
	e.t.Helper()
	steps := map[domain.State][][2]string{
		domain.StateOpen:             {},
		domain.StateSoftClosed:       {{"request-soft-close", closer}},
		domain.StateHardClosed:       {{"request-soft-close", closer}, {"hard-close", approver}},
		domain.StateReopenAuthorized: {{"request-soft-close", closer}, {"hard-close", approver}, {"authorize-reopen", approver}},
		domain.StateReclosed:         {{"request-soft-close", closer}, {"hard-close", approver}, {"authorize-reopen", approver}, {"reclose", approver}},
	}
	for _, s := range steps[target] {
		e.must(e.cmd(id, s[0], s[1]))
	}
	require.Equal(e.t, target, e.get(id).State)
}

func (e *testEnv) resolve(entity, date string, kv ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	path := "/v1/accounting-periods:resolve?legal_entity_id=" + entity + "&date=" + date + "&purpose=post"
	for i := 0; i+1 < len(kv); i += 2 {
		path += "&" + kv[i] + "=" + kv[i+1]
	}
	return e.do(http.MethodGet, path, nil)
}

func (e *testEnv) outboxTypes() []string {
	var out []string
	for _, o := range e.store.Outbox() {
		out = append(out, o.EventType)
	}
	return out
}

func countType(types []string, t string) int {
	n := 0
	for _, x := range types {
		if x == t {
			n++
		}
	}
	return n
}

const handlerMaterialize = handler.ActionMaterialize

func errDenied() error { return domain.ErrAuthorizationDenied }
