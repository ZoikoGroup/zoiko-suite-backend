package admission

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ---- limiter -----------------------------------------------------------------

func TestLimiterBurstThenRefuse(t *testing.T) {
	c := newClock()
	l := NewLimiter(10, 5, 0, c.now)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	ok, wait := l.Allow("k")
	if ok {
		t.Fatal("request beyond the burst was admitted")
	}
	if wait < time.Second {
		t.Fatalf("Retry-After must be at least a whole second, got %v", wait)
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	c := newClock()
	l := NewLimiter(2, 2, 0, c.now) // 2 tokens/s
	l.Allow("k")
	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("empty bucket admitted a request")
	}
	c.advance(500 * time.Millisecond) // +1 token
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("a refilled token was not usable")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("only one token had refilled")
	}
	c.advance(time.Hour) // refill is capped at burst
	admitted := 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("k"); ok {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("after a long idle the bucket must hold exactly the burst (2), admitted %d", admitted)
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := NewLimiter(1, 1, 0, c.now)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a second request from a must be refused")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("one client's exhaustion must not refuse another")
	}
}

// INT-NP-005: concurrent requests cannot bypass the quota. With the clock
// frozen no token refills, so exactly `burst` requests may ever be admitted.
func TestLimiterConcurrentRequestsCannotBypassQuota_INT_NP_005(t *testing.T) {
	c := newClock()
	const burst = 50
	l := NewLimiter(1000, burst, 0, c.now)
	var admitted, refused int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 400; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := l.Allow("hot"); ok {
				atomic.AddInt64(&admitted, 1)
			} else {
				atomic.AddInt64(&refused, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted != burst || refused != 400-burst {
		t.Fatalf("admitted %d refused %d, want exactly %d admitted", admitted, refused, burst)
	}
}

func TestLimiterDisabled(t *testing.T) {
	for _, l := range []*Limiter{NewLimiter(0, 10, 0, nil), nil} {
		for i := 0; i < 1000; i++ {
			if ok, _ := l.Allow("k"); !ok {
				t.Fatal("a disabled limiter must admit everything")
			}
		}
		if l.Enabled() {
			t.Fatal("Enabled must be false")
		}
	}
}

func TestLimiterMemoryIsBoundedAndOverflowSharesAQuota(t *testing.T) {
	c := newClock()
	l := NewLimiter(1, 1, 64, c.now) // one key per shard
	var admitted int
	for i := 0; i < 5000; i++ {
		if ok, _ := l.Allow(fmt.Sprintf("client-%d", i)); ok {
			admitted++
		}
	}
	if l.Keys() > 64 {
		t.Fatalf("tracked %d keys, the bound is 64", l.Keys())
	}
	// 64 tracked keys admit once each; every other key shares one overflow
	// bucket (burst 1): a flood of distinct keys must not mean unlimited quota.
	if admitted > 64+1 {
		t.Fatalf("a flood of 5000 distinct keys admitted %d requests", admitted)
	}
}

func TestLimiterEvictsIdleKeys(t *testing.T) {
	c := newClock()
	l := NewLimiter(1, 1, 64, c.now)
	for i := 0; i < 64; i++ {
		l.Allow(fmt.Sprintf("old-%d", i))
	}
	c.advance(time.Hour)
	// The table is full but everything in it is idle: a new client must get its
	// own bucket (evicting an idle one), not the shared overflow bucket. Pick
	// one new key per shard, so no two of them legitimately compete for a slot.
	seen := map[*shard]bool{}
	for i := 0; len(seen) < 32 && i < 100000; i++ {
		key := fmt.Sprintf("new-%d", i)
		sh := l.shardFor(key)
		if seen[sh] {
			continue
		}
		seen[sh] = true
		if ok, _ := l.Allow(key); !ok {
			t.Fatalf("new client %q was refused although its shard only held idle keys", key)
		}
	}
	if len(seen) < 32 {
		t.Fatal("could not find keys in 32 distinct shards")
	}
}

func TestLimiterStateDoesNotSpendOrCreate(t *testing.T) {
	c := newClock()
	l := NewLimiter(1, 3, 0, c.now)
	rem, burst, rate := l.State("never-seen")
	if rem != 3 || burst != 3 || rate != 1 || l.Keys() != 0 {
		t.Fatalf("state of an unseen key: %v %v %v keys=%d", rem, burst, rate, l.Keys())
	}
	l.Allow("k")
	if rem, _, _ := l.State("k"); rem != 2 {
		t.Fatalf("remaining = %v, want 2", rem)
	}
	if rem, _, _ := l.State("k"); rem != 2 {
		t.Fatal("reading state must not spend tokens")
	}
}

// ---- policy ------------------------------------------------------------------

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestPolicyRequestSize_INT_NP_004(t *testing.T) {
	p := Policy{MaxBodyBytes: 1000}
	if v, _ := p.Check(hdr("Content-Length", "1000"), "/v1/x"); v != nil {
		t.Fatalf("at the limit: %+v", v)
	}
	v, _ := p.Check(hdr("Content-Length", "1001"), "/v1/x")
	if v == nil || v.Kind != KindTooLarge || v.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit: %+v", v)
	}
	for _, bad := range []string{"abc", "-5", "1e3", "9999999999999999999999"} {
		v, _ := p.Check(hdr("Content-Length", bad), "/v1/x")
		if v == nil || v.Kind != KindMalformed || v.Status != http.StatusBadRequest {
			t.Errorf("Content-Length %q: %+v", bad, v)
		}
	}
	if v, _ := p.Check(hdr(), "/v1/x"); v != nil {
		t.Fatalf("no declared length is admitted (the edge bounds chunked bodies): %+v", v)
	}
	if v, _ := (Policy{}).Check(hdr("Content-Length", "99999999999"), "/v1/x"); v != nil {
		t.Fatalf("a zero cap disables the check: %+v", v)
	}
}

func TestPolicyRefusesCompressedBodiesByDefault_DecompressionBomb(t *testing.T) {
	p := Policy{MaxBodyBytes: 1 << 20}
	for _, enc := range []string{"gzip", "deflate", "br", "GZIP", "gzip, br"} {
		v, _ := p.Check(hdr("Content-Length", "100", "Content-Encoding", enc), "/v1/x")
		if v == nil || v.Kind != KindEncodingRefused || v.Status != http.StatusUnsupportedMediaType {
			t.Errorf("%q: %+v", enc, v)
		}
	}
	// refused even when no length is declared: a compressed chunked body is the bomb.
	if v, _ := p.Check(hdr("Content-Encoding", "gzip"), "/v1/x"); v == nil || v.Kind != KindEncodingRefused {
		t.Errorf("undeclared length with gzip: %+v", v)
	}
	if v, _ := p.Check(hdr("Content-Length", "10", "Content-Encoding", "identity"), "/v1/x"); v != nil {
		t.Errorf("identity is not compression: %+v", v)
	}
}

func TestPolicyAllowedEncodingStillCapped(t *testing.T) {
	p := Policy{MaxBodyBytes: 10_000_000, AllowedEncodings: []string{"gzip"}, MaxCompressedBytes: 1000}
	if v, _ := p.Check(hdr("Content-Length", "900", "Content-Encoding", "gzip"), "/v1/x"); v != nil {
		t.Fatalf("%+v", v)
	}
	if v, _ := p.Check(hdr("Content-Length", "1001", "Content-Encoding", "gzip"), "/v1/x"); v == nil || v.Kind != KindTooLarge {
		t.Fatalf("compressed body over its own cap: %+v", v)
	}
	if v, _ := p.Check(hdr("Content-Length", "10", "Content-Encoding", "br"), "/v1/x"); v == nil || v.Kind != KindEncodingRefused {
		t.Fatalf("an encoding not on the allow-list: %+v", v)
	}
	if v, _ := p.Check(hdr("Content-Length", "10", "Content-Encoding", "gzip, br"), "/v1/x"); v == nil {
		t.Fatal("every layered encoding must be allowed, not just the first")
	}
}

func TestPolicyVersionAdmission_INT_NP_003(t *testing.T) {
	p := Policy{SupportedVersions: []string{"v1", "v2"}}
	v, ver := p.Check(hdr(), "/v1/payments?x=1")
	if v != nil || ver != "v1" {
		t.Fatalf("supported path version: %+v %q", v, ver)
	}
	v, ver = p.Check(hdr(), "/v3/payments")
	if v == nil || v.Kind != KindVersionUnsupported || v.Status != http.StatusBadRequest || ver != "v3" {
		t.Fatalf("unsupported path version: %+v %q", v, ver)
	}
	// case: V1 is v1
	if v, _ := p.Check(hdr(), "/V1/x"); v != nil {
		t.Fatalf("version segments are case-insensitive: %+v", v)
	}
	// unversioned routes are not the version gate's business
	if v, ver := p.Check(hdr(), "/ap09/proposals"); v != nil || ver != "" {
		t.Fatalf("unversioned route: %+v %q", v, ver)
	}
	if v, _ := p.Check(hdr(), "/vendor/x"); v != nil {
		t.Fatalf("'vendor' is not a version: %+v", v)
	}
}

func TestPolicyVersionHeaderNeverFallsBack(t *testing.T) {
	p := Policy{SupportedVersions: []string{"v1"}}
	if v, ver := p.Check(hdr("X-API-Version", "v1"), "/things"); v != nil || ver != "v1" {
		t.Fatalf("header version on an unversioned path: %+v %q", v, ver)
	}
	if v, _ := p.Check(hdr("X-API-Version", "v2"), "/things"); v == nil || v.Kind != KindVersionUnsupported {
		t.Fatalf("unsupported header version: %+v", v)
	}
	if v, _ := p.Check(hdr("Accept-Version", "v2"), "/v1/things"); v == nil {
		t.Fatal("a header naming a different version than the path must be refused, not reconciled")
	}
	if v, _ := p.Check(hdr("X-API-Version", "latest"), "/v1/things"); v == nil {
		t.Fatal("a non-version header value must be refused")
	}
	if v, _ := (Policy{}).Check(hdr("X-API-Version", "v9"), "/v9/x"); v != nil {
		t.Fatalf("empty SupportedVersions disables the gate: %+v", v)
	}
}

func TestPolicyMalformedURIRefused(t *testing.T) {
	p := Policy{SupportedVersions: []string{"v1"}}
	for _, uri := range []string{"://bad", "/v1/\x7fx", "http://[::1"} {
		if v, _ := p.Check(hdr(), uri); v == nil || v.Kind != KindMalformed {
			t.Errorf("%q: %+v", uri, v)
		}
	}
}

func TestViolationDetailNeverEchoesInput(t *testing.T) {
	p := Policy{MaxBodyBytes: 10, SupportedVersions: []string{"v1"}}
	for _, c := range []struct {
		h   http.Header
		uri string
	}{
		{hdr("Content-Length", "SECRETVALUE"), "/v1/x"},
		{hdr("X-API-Version", "v7-secret"), "/v1/x"},
		{hdr("Content-Length", "999"), "/v1/x"},
	} {
		v, _ := p.Check(c.h, c.uri)
		if v == nil {
			t.Fatal("expected a violation")
		}
		if strings.Contains(strings.ToLower(v.Detail), "secret") || strings.Contains(v.Detail, "999") {
			t.Errorf("detail echoes input: %q", v.Detail)
		}
	}
}

func TestRoutePathStripsQueryAndBounds(t *testing.T) {
	if got := RoutePath("/v1/x?token=abc#frag"); got != "/v1/x" {
		t.Fatalf("%q", got)
	}
	if got := RoutePath("/" + strings.Repeat("a", 1000)); len(got) != 256 {
		t.Fatalf("len %d", len(got))
	}
}

// ---- overload gate -----------------------------------------------------------

func TestGateShedsAboveCeiling(t *testing.T) {
	g := NewGate(3)
	for i := 0; i < 3; i++ {
		if !g.Acquire() {
			t.Fatalf("slot %d refused", i+1)
		}
	}
	if g.Acquire() {
		t.Fatal("a slot above the ceiling was granted")
	}
	if g.InFlight() != 3 {
		t.Fatalf("in flight = %d (a refused acquire must not leak a slot)", g.InFlight())
	}
	g.Release()
	if !g.Acquire() {
		t.Fatal("a released slot must be reusable")
	}
}

func TestGateConcurrentNeverExceedsCeiling(t *testing.T) {
	g := NewGate(10)
	var peak, cur int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !g.Acquire() {
				return
			}
			n := atomic.AddInt64(&cur, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&cur, -1)
			g.Release()
		}()
	}
	wg.Wait()
	if peak > 10 {
		t.Fatalf("peak concurrency %d exceeded the ceiling 10", peak)
	}
	if g.InFlight() != 0 {
		t.Fatalf("slots leaked: %d", g.InFlight())
	}
}

func TestGateDisabled(t *testing.T) {
	for _, g := range []*Gate{NewGate(0), nil} {
		for i := 0; i < 100; i++ {
			if !g.Acquire() {
				t.Fatal("a disabled gate must admit everything")
			}
		}
		g.Release()
	}
}

// ---- decision recorder -------------------------------------------------------

func TestRecorderStoresAndQueries(t *testing.T) {
	c := newClock()
	r := NewRecorder(10, 0, c.now)
	a, _ := r.Record(Decision{TenantID: "t1", Outcome: "invalid_token", Code: "INT-AUTH-003", Status: 401})
	r.Record(Decision{TenantID: "t2", Outcome: "no_token", Code: "INT-AUTH-003", Status: 401})
	r.Record(Decision{TenantID: "t1", Outcome: OutcomeRateLimited, Code: "INT-RATE-007", Status: 429})
	r.Record(Decision{TenantID: "t1", Outcome: OutcomeOverloaded, Code: "INT-BP-016", Status: 503})

	if a.ID == "" || a.At.IsZero() {
		t.Fatalf("stored decision lacks id/time: %+v", a)
	}
	if got, ok := r.Get(a.ID); !ok || got.TenantID != "t1" {
		t.Fatalf("Get: %+v %v", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unknown id found")
	}
	if _, ok := r.Get(""); ok {
		t.Fatal("empty id must never match an empty slot")
	}
	all := r.ListDenied("", 0)
	if len(all) != 4 || all[0].Outcome != OutcomeOverloaded {
		t.Fatalf("newest first: %+v", all)
	}
	if t1 := r.ListDenied("t1", 0); len(t1) != 3 {
		t.Fatalf("tenant filter: %d", len(t1))
	}
	if limited := r.ListDenied("", 2); len(limited) != 2 {
		t.Fatalf("limit: %d", len(limited))
	}
	u, ok := r.UsageOf("t1")
	if !ok || u.Denied != 1 || u.RateLimited != 1 || u.Shed != 1 {
		t.Fatalf("usage: %+v", u)
	}
	r.Allowed("t1")
	r.Allowed("t1")
	if u, _ := r.UsageOf("t1"); u.Allowed != 2 {
		t.Fatalf("allowed: %+v", u)
	}
	if _, ok := r.UsageOf("unknown"); ok {
		t.Fatal("usage of an unseen tenant")
	}
}

func TestRecorderRingOverwritesOldest(t *testing.T) {
	r := NewRecorder(3, 0, newClock().now)
	var ids []string
	for i := 0; i < 5; i++ {
		d, _ := r.Record(Decision{TenantID: "t", Outcome: "no_token", Detail: fmt.Sprint(i)})
		ids = append(ids, d.ID)
	}
	list := r.ListDenied("", 0)
	if len(list) != 3 || list[0].Detail != "4" || list[2].Detail != "2" {
		t.Fatalf("ring contents: %+v", list)
	}
	if _, ok := r.Get(ids[0]); ok {
		t.Fatal("an overwritten decision must not be retrievable")
	}
	if _, ok := r.Get(ids[4]); !ok {
		t.Fatal("the newest decision must be retrievable")
	}
}

func TestRecorderNeverStoresSecretsItWasNotGiven(t *testing.T) {
	if k := ClientKey("203.0.113.9"); strings.Contains(k, "203") || len(k) != 16 {
		t.Fatalf("client key must be a hash, got %q", k)
	}
	if ClientKey("a") == ClientKey("b") || ClientKey("a") != ClientKey("a") {
		t.Fatal("client key must be stable and distinct")
	}
}

func TestRecorderAbuseDetectionFiresOncePerMinute(t *testing.T) {
	c := newClock()
	r := NewRecorder(100, 5, c.now)
	var fired int
	for i := 0; i < 20; i++ {
		if _, abusive := r.Record(Decision{ClientKey: "c1", Outcome: "no_token"}); abusive {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("abuse alert fired %d times in one window, want 1", fired)
	}
	if _, abusive := r.Record(Decision{ClientKey: "c2", Outcome: "no_token"}); abusive {
		t.Fatal("a different client must have its own count")
	}
	c.advance(61 * time.Second)
	fired = 0
	for i := 0; i < 6; i++ {
		if _, abusive := r.Record(Decision{ClientKey: "c1", Outcome: "no_token"}); abusive {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("a new window must be able to alert again, fired %d", fired)
	}
	r2 := NewRecorder(10, 0, c.now)
	for i := 0; i < 50; i++ {
		if _, abusive := r2.Record(Decision{ClientKey: "c", Outcome: "x"}); abusive {
			t.Fatal("threshold 0 disables abuse detection")
		}
	}
}

func TestRecorderMayEmitThrottles(t *testing.T) {
	c := newClock()
	r := NewRecorder(10, 0, c.now)
	if !r.MayEmit("k", 30*time.Second) {
		t.Fatal("first emit refused")
	}
	if r.MayEmit("k", 30*time.Second) {
		t.Fatal("second emit inside the interval allowed")
	}
	if !r.MayEmit("other", 30*time.Second) {
		t.Fatal("throttle is per key")
	}
	c.advance(31 * time.Second)
	if !r.MayEmit("k", 30*time.Second) {
		t.Fatal("emit after the interval refused")
	}
}

func TestRecorderStateIsBounded(t *testing.T) {
	r := NewRecorder(10, 5, newClock().now)
	for i := 0; i < maxTrackedTenants+500; i++ {
		r.Record(Decision{TenantID: fmt.Sprintf("t%d", i), ClientKey: fmt.Sprintf("c%d", i), Outcome: "no_token"})
		r.MayEmit(fmt.Sprintf("k%d", i), time.Hour)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.usage) > maxTrackedTenants+1 || len(r.abuse) > maxTrackedClients || len(r.throttle) > maxTrackedClients {
		t.Fatalf("unbounded state: usage=%d abuse=%d throttle=%d", len(r.usage), len(r.abuse), len(r.throttle))
	}
}

func TestRecorderConcurrentUse(t *testing.T) {
	r := NewRecorder(64, 10, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				d, _ := r.Record(Decision{TenantID: "t", ClientKey: fmt.Sprint(i), Outcome: "no_token"})
				r.Get(d.ID)
				r.ListDenied("t", 10)
				r.Allowed("t")
				r.UsageOf("t")
				r.MayEmit(fmt.Sprint(j%5), time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	if u, _ := r.UsageOf("t"); u.Denied != 5000 || u.Allowed != 5000 {
		t.Fatalf("lost updates under concurrency: %+v", u)
	}
}
