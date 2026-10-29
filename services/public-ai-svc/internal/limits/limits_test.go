package limits

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/store"
)

type fixedFlags struct{ v flags.Values }

func (f fixedFlags) Values() flags.Values { return f.v }

func guard(perIP, perSess int, st store.Store) *Guard {
	cfg := config.Config{RateLimitPerIPPerMinute: perIP, RateLimitPerSessionPerMin: perSess, DailyTokenCeiling: 100, MonthlyTokenCeiling: 1000}
	return New(cfg, st, fixedFlags{flags.Defaults()})
}

func hit(h http.Handler, ip, grant string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", nil)
	r.RemoteAddr = ip + ":1234"
	if grant != "" {
		r.Header.Set("X-Public-Chat-Session", grant)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

// QA-T057 family: per-IP limit gives a graceful 429 with Retry-After.
func TestPerIPLimit(t *testing.T) {
	g := guard(12, 1000, store.NewMemory()) // burst 3
	h := g.Middleware(ok)
	for i := 0; i < 3; i++ {
		if w := hit(h, "203.0.113.7", ""); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	w := hit(h, "203.0.113.7", "")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := hit(h, "203.0.113.8", ""); w.Code != 200 {
		t.Fatal("another client must not be affected")
	}
}

func TestPerSessionLimitAcrossIPs(t *testing.T) {
	g := guard(1000, 12, store.NewMemory())
	h := g.Middleware(ok)
	for i := 0; i < 3; i++ {
		hit(h, "198.51.100."+string(rune('1'+i)), "pgr_same")
	}
	if w := hit(h, "198.51.100.9", "pgr_same"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("session limit must hold across IPs, got %d", w.Code)
	}
}

func TestIPv6Coarsened(t *testing.T) {
	if clientKey(net.ParseIP("2001:db8:1:2:aaaa::1")) != clientKey(net.ParseIP("2001:db8:1:2:bbbb::9")) {
		t.Fatal("IPv6 addresses in one /64 must share a bucket")
	}
}

// Visitors behind the console proxy get their own buckets; a forged
// X-Forwarded-For cannot pick a bucket.
func TestClientIPThroughTrustedProxy(t *testing.T) {
	cfg := config.Config{TrustedProxies: []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8"}}
	g := New(cfg, store.NewMemory(), fixedFlags{flags.Defaults()})
	cases := []struct{ peer, xff, want string }{
		{"127.0.0.1", "203.0.113.7", "203.0.113.7"},           // console proxy
		{"[::1]", "203.0.113.7", "203.0.113.7"},               // IPv6 loopback proxy
		{"127.0.0.1", "6.6.6.6, 203.0.113.7", "203.0.113.7"},  // forged left entry ignored
		{"127.0.0.1", "203.0.113.7, 10.1.2.3", "203.0.113.7"}, // trusted edge hop skipped
		{"127.0.0.1", "", "127.0.0.1"},                        // no header: the peer
		{"127.0.0.1", "not-an-ip", "127.0.0.1"},               // malformed: the peer
		{"198.51.100.4", "203.0.113.7", "198.51.100.4"},       // untrusted peer: header ignored
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", nil)
		r.RemoteAddr = c.peer + ":1234"
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := g.clientIP(r).String(); got != c.want {
			t.Errorf("peer %s xff %q: got %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

func TestVisitorsBehindProxyHaveSeparateBuckets(t *testing.T) {
	cfg := config.Config{RateLimitPerIPPerMinute: 12, RateLimitPerSessionPerMin: 1000, TrustedProxies: []string{"127.0.0.1/32"}}
	h := New(cfg, store.NewMemory(), fixedFlags{flags.Defaults()}).Middleware(ok)
	via := func(visitor string) int {
		r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header.Set("X-Forwarded-For", visitor)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 3; i++ {
		via("203.0.113.7")
	}
	if via("203.0.113.7") != http.StatusTooManyRequests {
		t.Fatal("the busy visitor must be limited")
	}
	if via("203.0.113.8") != 200 {
		t.Fatal("another visitor behind the same proxy must not be limited")
	}
}

func TestGlobalInFlightCap(t *testing.T) {
	v := flags.Defaults()
	v.MaxConcurrentTurns = 1
	g := New(config.Config{}, store.NewMemory(), fixedFlags{v})
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release; w.WriteHeader(200) })
	h := g.Middleware(slow)
	done := make(chan int)
	go func() { done <- hit(h, "192.0.2.1", "").Code }()
	for g.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if w := hit(h, "192.0.2.2", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent turn: %d", w.Code)
	}
	close(release)
	if c := <-done; c != 200 {
		t.Fatalf("first turn: %d", c)
	}
}

type countAnswerer struct{ n int }

func (c *countAnswerer) Answer(context.Context, orchestrator.Input) orchestrator.Result {
	c.n++
	return orchestrator.Result{Status: contract.Answered}
}

// Spend ceiling: once reached, no model path runs (auto throttle).
func TestSpendCeilingStopsAnswering(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	g := guard(1000, 1000, st)
	inner := &countAnswerer{}
	a := g.Wrap(inner)
	if r := a.Answer(ctx, orchestrator.Input{}); r.Status != contract.Answered {
		t.Fatal("under ceiling should answer")
	}
	// Record usage past the daily ceiling through a completed turn.
	day, month := UsageBuckets(time.Now().UTC())
	_ = st.CreateSession(ctx, store.Session{SessionID: "pub_s_000000000001", GrantHash: []byte("g"), ExpiresAt: time.Now().Add(time.Hour)}, "", nil)
	b, _ := st.BeginTurn(ctx, store.Turn{TurnID: "t", SessionID: "pub_s_000000000001", ClientTurnID: "c", RequestHash: []byte("h")}, time.Now().Add(time.Minute))
	_ = st.CompleteTurn(ctx, b.Turn, []string{day, month}, 150, nil)
	if r := a.Answer(ctx, orchestrator.Input{}); r.Status != contract.Unavailable {
		t.Fatalf("over ceiling: %+v", r)
	}
	if inner.n != 1 {
		t.Fatalf("model path called %d times", inner.n)
	}
}
