package periodgate_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/periodgate"
)

// ── helpers ──────────────────────────────────────────────────────────────────

type fakeClose struct {
	err   error
	calls atomic.Int32
	pan   bool
}

func (f *fakeClose) CheckPeriodOpen(context.Context, string, string, string) error {
	f.calls.Add(1)
	return f.err
}
func (f *fakeClose) CheckPeriodOpenAt(context.Context, string, close.PeriodRef) error {
	f.calls.Add(1)
	if f.pan {
		panic("legacy boom")
	}
	return f.err
}

var ref = close.PeriodRef{LegalEntityID: "e1", PeriodName: "2026-07", PostingDate: domain.NewDate(2026, time.July, 15), JournalID: "j1"}

// gateServer answers the resolve call per behaviour and counts requests.
type gateServer struct {
	*httptest.Server
	hits atomic.Int32
	last atomic.Value // *http.Request clone (URL + header)
}

func newGate(t *testing.T, h http.HandlerFunc) *gateServer {
	g := &gateServer{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		g.last.Store(r.Clone(context.Background()))
		h(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func respond(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

const (
	allowedBody = `{"period_id":"p","period_key":"2026-07","state":"OPEN","posting_allowed":true,"posting_mode":"ALLOWED","reason":"","version":1,"state_version":1}`
	blockedBody = `{"period_id":"p","period_key":"2026-07","state":"HARD_CLOSED","posting_allowed":false,"posting_mode":"BLOCKED","reason":"closed","version":1,"state_version":1}`
)

func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matches(m, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func matches(m *dto.Metric, labels map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range labels {
		if got[k] != v {
			return false
		}
	}
	return true
}

func histCount(reg *prometheus.Registry) uint64 {
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "gl_period_gate_shadow_seconds" {
			return mf.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	return 0
}

func totalComparisons(reg *prometheus.Registry) float64 {
	mfs, _ := reg.Gather()
	var n float64
	for _, mf := range mfs {
		if mf.GetName() == "gl_period_gate_comparisons_total" {
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
}

func newShadow(inner close.Client, gate periodgate.Resolver, timeout time.Duration) (*periodgate.Shadow, *prometheus.Registry, *observer.ObservedLogs) {
	reg := prometheus.NewRegistry()
	core, logs := observer.New(zap.WarnLevel)
	return periodgate.NewShadow(inner, gate, periodgate.NewMetrics(reg), zap.New(core), timeout), reg, logs
}

// ── comparator ───────────────────────────────────────────────────────────────

func TestCompare_Table(t *testing.T) {
	L := []periodgate.LegacyOutcome{periodgate.LegacyAllowed, periodgate.LegacyLocked, periodgate.LegacyUnavailable}
	G := []periodgate.GateOutcome{periodgate.GateAllowed, periodgate.GateBlocked, periodgate.GateNotFound, periodgate.GateAmbiguous, periodgate.GateError}
	for _, l := range L {
		for _, g := range G {
			want := periodgate.AgreeNo
			switch {
			case g == periodgate.GateError:
				want = periodgate.AgreeShadowError
			case l == periodgate.LegacyAllowed && g == periodgate.GateAllowed,
				l == periodgate.LegacyLocked && g == periodgate.GateBlocked:
				want = periodgate.AgreeYes
			}
			if got := periodgate.Compare(l, g); got != want {
				t.Errorf("Compare(%s,%s)=%s want %s", l, g, got, want)
			}
		}
	}
}

func TestClassifyLegacy(t *testing.T) {
	if periodgate.ClassifyLegacy(nil) != periodgate.LegacyAllowed ||
		periodgate.ClassifyLegacy(domain.ErrPeriodLocked) != periodgate.LegacyLocked ||
		periodgate.ClassifyLegacy(domain.ErrCloseServiceUnavailable) != periodgate.LegacyUnavailable ||
		periodgate.ClassifyLegacy(errors.New("x")) != periodgate.LegacyUnavailable {
		t.Fatal("legacy classification wrong")
	}
}

// ── gate client ──────────────────────────────────────────────────────────────

func TestClient_ClassifiesResponses_AndSendsContract(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		want periodgate.GateOutcome
		code string
	}{
		{"allowed", respond(200, allowedBody), periodgate.GateAllowed, ""},
		{"blocked", respond(200, blockedBody), periodgate.GateBlocked, ""},
		{"not_found", respond(404, `{"code":"PERIOD_NOT_FOUND","message":"m","posting_allowed":false}`), periodgate.GateNotFound, "PERIOD_NOT_FOUND"},
		{"ambiguous", respond(409, `{"code":"RULE_AMBIGUOUS","message":"m","posting_allowed":false}`), periodgate.GateAmbiguous, "RULE_AMBIGUOUS"},
		{"422", respond(422, `{"code":"CONTEXT_INVALID","posting_allowed":false}`), periodgate.GateError, "CONTEXT_INVALID"},
		{"401", respond(401, `{"code":"UNAUTHENTICATED","posting_allowed":false}`), periodgate.GateError, "UNAUTHENTICATED"},
		{"503", respond(503, `{"code":"DEPENDENCY_UNAVAILABLE","posting_allowed":false}`), periodgate.GateError, "DEPENDENCY_UNAVAILABLE"},
		{"500 html", respond(500, `<html>`), periodgate.GateError, "HTTP_ERROR"},
		{"malformed 200", respond(200, `{"posting_allowed":tru`), periodgate.GateError, "BAD_JSON"},
		{"200 without posting_allowed", respond(200, `{"state":"OPEN"}`), periodgate.GateError, "BAD_JSON"},
		{"200 empty", respond(200, ``), periodgate.GateError, "BAD_JSON"},
		{"200 wrong type", respond(200, `{"posting_allowed":"yes"}`), periodgate.GateError, "BAD_JSON"},
		{"404 garbage body", respond(404, `not json`), periodgate.GateNotFound, "PERIOD_NOT_FOUND"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGate(t, c.h)
			res := periodgate.NewClient(g.URL, time.Second).Resolve(context.Background(), "t1", "e1", "2026-07-15")
			if res.Outcome != c.want {
				t.Fatalf("outcome %s want %s", res.Outcome, c.want)
			}
			if c.code != "" && res.Code != c.code {
				t.Errorf("code %q want %q", res.Code, c.code)
			}
			if g.hits.Load() != 1 {
				t.Errorf("never retry: expected exactly 1 request, got %d", g.hits.Load())
			}
		})
	}

	g := newGate(t, respond(200, allowedBody))
	periodgate.NewClient(g.URL, time.Second).Resolve(context.Background(), "tenant-9", "ent-1", "2026-07-15")
	req := g.last.Load().(*http.Request)
	q := req.URL.Query()
	if req.Method != "GET" || req.URL.Path != "/v1/accounting-periods:resolve" ||
		q.Get("legal_entity_id") != "ent-1" || q.Get("date") != "2026-07-15" || q.Get("purpose") != "post" ||
		req.Header.Get("X-Tenant-Id") != "tenant-9" {
		t.Fatalf("bad request: %s %s hdr=%v", req.Method, req.URL.String(), req.Header)
	}
}

func TestClient_TransportErrorAndTimeout(t *testing.T) {
	g := newGate(t, respond(200, allowedBody))
	url := g.URL
	g.Close()
	if res := periodgate.NewClient(url, time.Second).Resolve(context.Background(), "t", "e", "2026-07-15"); res.Outcome != periodgate.GateError {
		t.Fatalf("connection refused should be error, got %s", res.Outcome)
	}
	slow := newGate(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := periodgate.NewClient(slow.URL, 5*time.Second).Resolve(ctx, "t", "e", "2026-07-15")
	if res.Outcome != periodgate.GateError || res.Code != "TIMEOUT" || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("expected fast TIMEOUT, got %+v after %v", res, time.Since(start))
	}
}

func TestClient_BreakerOnlySkipsShadowCalls(t *testing.T) {
	g := newGate(t, respond(500, `{}`))
	c := periodgate.NewClient(g.URL, time.Second)
	for i := 0; i < 5; i++ {
		c.Resolve(context.Background(), "t", "e", "2026-07-15")
	}
	res := c.Resolve(context.Background(), "t", "e", "2026-07-15")
	if !res.Skipped || g.hits.Load() != 5 {
		t.Fatalf("breaker should suppress the 6th request: skipped=%v hits=%d", res.Skipped, g.hits.Load())
	}
	// 404/409 are healthy answers and must not trip the breaker.
	g2 := newGate(t, respond(404, `{"code":"PERIOD_NOT_FOUND"}`))
	c2 := periodgate.NewClient(g2.URL, time.Second)
	for i := 0; i < 10; i++ {
		if c2.Resolve(context.Background(), "t", "e", "2026-07-15").Skipped {
			t.Fatal("404 must not trip the breaker")
		}
	}
}

// ── shadow wrapper: legacy outcome x gate outcome ────────────────────────────

func TestShadow_AllCombos_LegacyReturnedUnchanged_AndMetricsCorrect(t *testing.T) {
	legacies := []struct {
		name string
		err  error
		out  periodgate.LegacyOutcome
	}{
		{"allowed", nil, periodgate.LegacyAllowed},
		{"locked", domain.ErrPeriodLocked, periodgate.LegacyLocked},
		{"unavailable", domain.ErrCloseServiceUnavailable, periodgate.LegacyUnavailable},
	}
	gates := []struct {
		name string
		h    http.HandlerFunc
		out  periodgate.GateOutcome
	}{
		{"allowed", respond(200, allowedBody), periodgate.GateAllowed},
		{"blocked", respond(200, blockedBody), periodgate.GateBlocked},
		{"not_found", respond(404, `{"code":"PERIOD_NOT_FOUND"}`), periodgate.GateNotFound},
		{"ambiguous", respond(409, `{"code":"RULE_AMBIGUOUS"}`), periodgate.GateAmbiguous},
		{"5xx", respond(503, `{}`), periodgate.GateError},
		{"malformed", respond(200, `{{{`), periodgate.GateError},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }, periodgate.GateError},
	}
	for _, l := range legacies {
		for _, g := range gates {
			t.Run(l.name+"_x_"+g.name, func(t *testing.T) {
				gs := newGate(t, g.h)
				inner := &fakeClose{err: l.err}
				sh, reg, logs := newShadow(inner, periodgate.NewClient(gs.URL, 100*time.Millisecond), 100*time.Millisecond)

				got := sh.CheckPeriodOpenAt(context.Background(), "t1", ref)
				if got != l.err { // identity, not just errors.Is
					t.Fatalf("returned %v, legacy gave %v", got, l.err)
				}
				sh.Drain()

				agree := periodgate.Compare(l.out, g.out)
				if v := counter(t, reg, "gl_period_gate_comparisons_total",
					map[string]string{"legacy": string(l.out), "gate": string(g.out), "agree": agree}); v != 1 {
					t.Fatalf("expected comparisons{%s,%s,%s}=1, got %v", l.out, g.out, agree, v)
				}
				if totalComparisons(reg) != 1 {
					t.Errorf("exactly one series increment expected")
				}
				if histCount(reg) != 1 {
					t.Errorf("histogram should have 1 observation")
				}
				if inner.calls.Load() != 1 {
					t.Errorf("legacy must be called exactly once, got %d", inner.calls.Load())
				}
				if gs.hits.Load() != 1 {
					t.Errorf("gate must be called exactly once (no retry), got %d", gs.hits.Load())
				}
				wantLogs := 0
				if agree == periodgate.AgreeNo {
					wantLogs = 1
				}
				if logs.Len() != wantLogs {
					t.Errorf("disagreement log lines=%d, want %d", logs.Len(), wantLogs)
				}
			})
		}
	}
}

func TestShadow_DisagreementLog_HasRequiredFieldsAndNoMoney(t *testing.T) {
	gs := newGate(t, respond(404, `{"code":"PERIOD_NOT_FOUND"}`))
	sh, _, logs := newShadow(&fakeClose{}, periodgate.NewClient(gs.URL, time.Second), time.Second)
	_ = sh.CheckPeriodOpenAt(context.Background(), "t1", ref)
	sh.Drain()
	if logs.Len() != 1 {
		t.Fatalf("expected 1 log line, got %d", logs.Len())
	}
	f := logs.All()[0].ContextMap()
	want := map[string]any{"journal_id": "j1", "legal_entity_id": "e1", "posting_date": "2026-07-15",
		"legacy": "allowed", "gate": "not_found", "gate_state": "", "gate_code": "PERIOD_NOT_FOUND"}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("field %s=%v want %v", k, f[k], v)
		}
	}
	if len(f) != len(want) {
		t.Errorf("unexpected extra log fields: %v", f)
	}
}

func TestShadow_OffMode_ZeroOutboundGateRequests(t *testing.T) {
	gs := newGate(t, respond(200, allowedBody))
	inner := &fakeClose{err: domain.ErrPeriodLocked}
	c := periodgate.Wrap(periodgate.Settings{Mode: "off", URL: gs.URL, Timeout: time.Second}, inner, prometheus.NewRegistry(), zap.NewNop())
	if c != close.Client(inner) {
		t.Fatal("off mode must return the legacy client untouched")
	}
	for i := 0; i < 20; i++ {
		if err := c.CheckPeriodOpenAt(context.Background(), "t", ref); err != domain.ErrPeriodLocked {
			t.Fatal("legacy result changed")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if gs.hits.Load() != 0 {
		t.Fatalf("off mode sent %d gate requests", gs.hits.Load())
	}
}

func TestShadow_SlowGate_DoesNotSlowTheOperation(t *testing.T) {
	gs := newGate(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) })
	timeout := 150 * time.Millisecond
	sh, reg, _ := newShadow(&fakeClose{}, periodgate.NewClient(gs.URL, timeout), timeout)

	start := time.Now()
	err := sh.CheckPeriodOpenAt(context.Background(), "t", ref)
	opElapsed := time.Since(start)
	if err != nil || opElapsed > 50*time.Millisecond {
		t.Fatalf("operation was slowed by the gate: err=%v elapsed=%v", err, opElapsed)
	}
	sh.Drain()
	if total := time.Since(start); total > timeout+400*time.Millisecond {
		t.Fatalf("shadow outlived its timeout: %v", total)
	}
	if v := counter(t, reg, "gl_period_gate_comparisons_total", map[string]string{"gate": "error", "agree": "shadow_error"}); v != 1 {
		t.Fatalf("timeout should count as shadow_error, got %v", v)
	}
}

func TestShadow_NoDate_SkippedWithoutGateRequest(t *testing.T) {
	gs := newGate(t, respond(200, allowedBody))
	sh, reg, _ := newShadow(&fakeClose{err: domain.ErrPeriodLocked}, periodgate.NewClient(gs.URL, time.Second), time.Second)
	r := ref
	r.PostingDate = domain.Date{}
	if err := sh.CheckPeriodOpenAt(context.Background(), "t", r); err != domain.ErrPeriodLocked {
		t.Fatal("legacy result changed")
	}
	sh.Drain()
	if gs.hits.Load() != 0 || counter(t, reg, "gl_period_gate_shadow_skipped_total", map[string]string{"reason": "no_date"}) != 1 {
		t.Fatalf("expected skip no_date and zero requests; hits=%d", gs.hits.Load())
	}
	if totalComparisons(reg) != 0 {
		t.Fatal("no comparison expected")
	}
}

type panicGate struct{}

func (panicGate) Resolve(context.Context, string, string, string) periodgate.Resolution {
	panic("gate boom")
}

func TestShadow_GatePanic_IsContained(t *testing.T) {
	sh, _, _ := newShadow(&fakeClose{err: domain.ErrPeriodLocked}, panicGate{}, time.Second)
	if err := sh.CheckPeriodOpenAt(context.Background(), "t", ref); err != domain.ErrPeriodLocked {
		t.Fatal("legacy result changed")
	}
	sh.Drain() // must not panic or hang
}

func TestShadow_LegacyPanic_PropagatesAndDoesNotLeakGoroutine(t *testing.T) {
	gs := newGate(t, respond(200, allowedBody))
	sh, reg, _ := newShadow(&fakeClose{pan: true}, periodgate.NewClient(gs.URL, time.Second), time.Second)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("legacy panic must propagate unchanged (same as today)")
			}
		}()
		_ = sh.CheckPeriodOpenAt(context.Background(), "t", ref)
	}()
	sh.Drain()
	if counter(t, reg, "gl_period_gate_shadow_skipped_total", map[string]string{"reason": "legacy_aborted"}) != 1 {
		t.Fatal("expected legacy_aborted skip")
	}
}

func TestShadow_CallerCancelDoesNotChangeLegacyOrCrash(t *testing.T) {
	gs := newGate(t, respond(200, allowedBody))
	sh, _, _ := newShadow(&fakeClose{}, periodgate.NewClient(gs.URL, time.Second), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	if err := sh.CheckPeriodOpenAt(ctx, "t", ref); err != nil {
		t.Fatal(err)
	}
	cancel()
	sh.Drain()
}

type blockingGate struct{ release chan struct{} }

func (b blockingGate) Resolve(context.Context, string, string, string) periodgate.Resolution {
	<-b.release
	return periodgate.Resolution{Outcome: periodgate.GateAllowed}
}

func TestShadow_Overload_SkipsInsteadOfQueueing(t *testing.T) {
	bg := blockingGate{release: make(chan struct{})}
	sh, reg, _ := newShadow(&fakeClose{}, bg, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = sh.CheckPeriodOpenAt(context.Background(), "t", ref) }()
	}
	wg.Wait()
	if v := counter(t, reg, "gl_period_gate_shadow_skipped_total", map[string]string{"reason": "overload"}); v != 16 {
		t.Errorf("expected 16 overload skips (80 calls, 64 slots), got %v", v)
	}
	bg.stop()
	sh.Drain()
}

func (b blockingGate) stop() { releaseAll(b.release) }
