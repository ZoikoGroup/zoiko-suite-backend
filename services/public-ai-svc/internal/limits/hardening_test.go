package limits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/metrics"
	"zoiko.io/public-ai-svc/internal/store"
)

func scrape(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return w.Body.String()
}

// SEC-T054, Doc 06 §13: the spend check publishes the day and month ratios
// that the 50/80/100 % alerts read.
func TestSpendStagePublishesSpendRatio(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	g := New(config.Config{DailyTokenCeiling: 1000, MonthlyTokenCeiling: 10000}, st, nil)
	day, month := UsageBuckets(g.now().UTC())
	st.SetUsageForTest(day, 800)
	st.SetUsageForTest(month, 2500)
	if _, failed := g.spendStage(ctx); failed {
		t.Fatal("usage unreadable")
	}
	body := scrape(t)
	for _, want := range []string{`public_ai_spend_ratio{window="day"} 0.8`, `public_ai_spend_ratio{window="month"} 0.25`} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %s", want)
		}
	}
}

// SEC-T046: idle buckets are swept on a timer, not only under memory pressure.
func TestSweepDropsIdleBuckets(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	g := New(config.Config{RateLimitPerIPPerMinute: 60, RateLimitPerSessionPerMin: 60}, store.NewMemory(), fixedFlags{flags.Defaults()})
	g.now = func() time.Time { return now }
	g.ip.allow("old", now)
	g.sess.allow("old", now)
	g.sessCreate.allow("old", now)
	now = now.Add(idleTTL - time.Minute)
	g.ip.allow("fresh", now)
	now = now.Add(2 * time.Minute) // "old" is now idle > idleTTL, "fresh" is not
	g.Sweep()
	if _, ok := g.ip.buckets["old"]; ok {
		t.Fatal("idle ip bucket kept")
	}
	if _, ok := g.ip.buckets["fresh"]; !ok {
		t.Fatal("active bucket dropped")
	}
	if len(g.sess.buckets) != 0 || len(g.sessCreate.buckets) != 0 {
		t.Fatal("idle session buckets kept")
	}
}

func TestRunSweeperStopsWithContext(t *testing.T) {
	now := time.Now()
	g := New(config.Config{RateLimitPerIPPerMinute: 60}, store.NewMemory(), fixedFlags{flags.Defaults()})
	g.ip.allow("old", now.Add(-time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.RunSweeper(ctx, 5*time.Millisecond); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		g.ip.mu.Lock()
		n := len(g.ip.buckets)
		g.ip.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("sweeper did not run")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop")
	}
}

// SEC-T046: a flood of distinct keys never grows a limiter past its cap;
// the least recently used buckets are evicted, recent ones kept.
func TestBucketMapHardCap(t *testing.T) {
	l := newLimiter(60)
	l.maxKeys = 100
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		l.allow("k"+strconv.Itoa(i), now.Add(time.Duration(i)*time.Millisecond))
		if len(l.buckets) > l.maxKeys {
			t.Fatalf("after %d keys the map holds %d > cap %d", i+1, len(l.buckets), l.maxKeys)
		}
	}
	if _, ok := l.buckets["k999"]; !ok {
		t.Fatal("the newest key must be kept")
	}
	if _, ok := l.buckets["k0"]; ok {
		t.Fatal("the oldest key must be evicted")
	}
}

// SEC-T055, Doc 06 §13: session creation has its own, tighter per-IP limit
// with its own metric scope; other routes are unaffected by it.
func TestSessionCreateLimit(t *testing.T) {
	g := guard(1000, 1000, store.NewMemory())
	h := g.Middleware(ok)
	create := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/sessions", nil)
		r.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 3; i++ { // burst of SessionCreatePerIPPerMinute=10 is 3
		if w := create("203.0.113.50"); w.Code != 200 {
			t.Fatalf("create %d: %d", i, w.Code)
		}
	}
	w := create("203.0.113.50")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("4th create: %d", w.Code)
	}
	if !strings.Contains(scrape(t), `public_ai_rate_limited_total{scope="session_create"}`) {
		t.Fatal("session_create scope not counted")
	}
	if w := hit(h, "203.0.113.50", ""); w.Code != 200 {
		t.Fatalf("turns from the same address must not share the create bucket: %d", w.Code)
	}
	if w := create("203.0.113.51"); w.Code != 200 {
		t.Fatal("another address must not be affected")
	}
}
