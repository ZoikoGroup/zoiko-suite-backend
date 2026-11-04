// Package limits enforces abuse and cost controls (Doc 06 §13, Doc 08 §25):
// per-IP and per-session token buckets, a global in-flight turn cap, and
// daily/monthly token ceilings that stop model use before they are exceeded.
// Numeric values are dev defaults pending CISO/SRE approval (D06-05).
package limits

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"zoiko.io/public-ai-svc/internal/metrics"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/store"
)

// bucket is a token bucket refilled continuously.
type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	maxKeys int
	buckets map[string]*bucket
}

// idleTTL: a bucket untouched this long has refilled completely, so
// dropping it changes no decision (SEC-T046: keys are not kept longer than
// the limit needs them).
const idleTTL = 10 * time.Minute

// DefaultMaxKeys is the hard cap on buckets per limiter, so a flood of
// distinct addresses cannot grow memory without bound.
const DefaultMaxKeys = 100_000

// SessionCreatePerIPPerMinute is the default tighter per-address limit on
// POST /sessions (Doc 06 §13, SEC-T055): creating sessions is cheap for an
// attacker and each one carries a grant, so it is limited separately from
// the general per-IP limit.
const SessionCreatePerIPPerMinute = 10

// sessionCreateLimit is the configured limit, or the default when unset
// (config refuses 0 outside dev/test).
func sessionCreateLimit(configured int) int {
	if configured > 0 {
		return configured
	}
	return SessionCreatePerIPPerMinute
}

func newLimiter(perMin int) *limiter {
	b := float64(perMin) / 4
	if b < 3 {
		b = 3
	}
	return &limiter{perMin: float64(perMin), burst: b, maxKeys: DefaultMaxKeys, buckets: map[string]*bucket{}}
}

// sweep drops buckets idle for idleTTL; it returns how many remain.
func (l *limiter) sweep(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	return len(l.buckets)
}

func (l *limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > idleTTL {
			delete(l.buckets, k)
		}
	}
}

// makeRoomLocked keeps the map under maxKeys before a new key is added:
// idle buckets go first; if the map is still full the least recently used
// tenth is evicted in one pass, so the cost is amortised under a flood.
// Evicting the oldest (not refusing new keys) keeps a flood of fresh
// addresses from locking out legitimate new visitors.
func (l *limiter) makeRoomLocked(now time.Time) {
	if l.maxKeys <= 0 || len(l.buckets) < l.maxKeys {
		return
	}
	l.sweepLocked(now)
	if len(l.buckets) < l.maxKeys {
		return
	}
	type entry struct {
		k    string
		last time.Time
	}
	all := make([]entry, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	n := len(all) - l.maxKeys + 1
	if tenth := l.maxKeys / 10; n < tenth {
		n = tenth
	}
	for _, e := range all[:n] {
		delete(l.buckets, e.k)
	}
}

// allow takes one token for key, or returns the wait until one is available.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l.perMin <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		l.makeRoomLocked(now)
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	rate := l.perMin / 60
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Guard holds the limiters.
type Guard struct {
	ip, sess   *limiter
	sessCreate *limiter // POST /sessions per address (SEC-T055)
	trusted    []*net.IPNet
	inFlight   atomic.Int64
	store      store.Store
	flags      interface{ Values() flags.Values }
	daily      int64
	monthly    int64
	now        func() time.Time
}

// New builds a Guard from configuration.
func New(cfg config.Config, st store.Store, fl interface{ Values() flags.Values }) *Guard {
	var trusted []*net.IPNet
	for _, p := range cfg.TrustedProxies {
		if _, n, err := net.ParseCIDR(p); err == nil { // Validate rejects bad entries
			trusted = append(trusted, n)
		}
	}
	return &Guard{
		ip: newLimiter(cfg.RateLimitPerIPPerMinute), sess: newLimiter(cfg.RateLimitPerSessionPerMin), trusted: trusted,
		sessCreate: newLimiter(sessionCreateLimit(cfg.RateLimitSessionCreatePerMin)), store: st, flags: fl, daily: cfg.DailyTokenCeiling, monthly: cfg.MonthlyTokenCeiling,
		now: time.Now,
	}
}

// RunSweeper drops idle buckets from every limiter each interval until ctx
// is done (SEC-T046): without it, keys of visitors who left would stay in
// memory until the hard cap forced an eviction.
func (g *Guard) RunSweeper(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Sweep()
		}
	}
}

// Sweep drops buckets idle for longer than idleTTL from every limiter.
func (g *Guard) Sweep() {
	now := g.now()
	for _, l := range []*limiter{g.ip, g.sess, g.sessCreate} {
		l.sweep(now)
	}
}

func (g *Guard) isTrusted(ip net.IP) bool {
	for _, n := range g.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the visitor's address. Browser traffic arrives through the
// console's same-origin proxy, so the TCP peer is the proxy, not the
// visitor. X-Forwarded-For is believed only when the peer is a trusted
// proxy (PUBLIC_AI_TRUSTED_PROXIES); it is read right to left, skipping
// trusted hops, and the first untrusted address is the visitor. Entries
// left of that one are client-supplied and ignored, so a visitor cannot
// pick their own bucket.
func (g *Guard) clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !g.isTrusted(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			return peer // malformed chain: fall back to the proxy itself
		}
		if !g.isTrusted(ip) {
			return ip
		}
	}
	return peer
}

// keySalt is random per process: bucket keys cannot be reversed to an
// address or linked across restarts (Doc 06 §15, SEC-T046).
var keySalt = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("limits: no randomness for the key salt: " + err.Error())
	}
	return b
}()

// clientKey coarsens the client address (IPv4 as is, IPv6 to /64) and
// returns a salted hash of it, never the address itself.
func clientKey(ip net.IP) string {
	if ip == nil {
		return "ip:unknown"
	}
	addr := ""
	if v4 := ip.To4(); v4 != nil {
		addr = v4.String()
	} else {
		addr = ip.Mask(net.CIDRMask(64, 128)).String()
	}
	h := sha256.New()
	h.Write(keySalt)
	h.Write([]byte(addr))
	return "ip:" + hex.EncodeToString(h.Sum(nil)[:12])
}

func tooMany(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := int(wait.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	problem.Write(w, r, problem.RateLimited, "")
}

// Middleware applies per-IP and per-session limits and the global in-flight
// cap on POST /turns. Excess load gets 429 + Retry-After (X10).
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := g.now()
		ipKey := clientKey(g.clientIP(r))
		if ok, wait := g.ip.allow(ipKey, now); !ok {
			metrics.RateLimited("ip")
			tooMany(w, r, wait)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/public-ai/v1/sessions" {
			if ok, wait := g.sessCreate.allow(ipKey, now); !ok {
				metrics.RateLimited("session_create")
				tooMany(w, r, wait)
				return
			}
		}
		if grant := r.Header.Get("X-Public-Chat-Session"); grant != "" {
			sum := sha256.Sum256([]byte(grant)) // never keep the grant itself
			if ok, wait := g.sess.allow("s:"+string(sum[:12]), now); !ok {
				metrics.RateLimited("session")
				tooMany(w, r, wait)
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/public-ai/v1/turns" {
			max := int64(g.flags.Values().MaxConcurrentTurns)
			if n := g.inFlight.Add(1); max > 0 && n > max {
				g.inFlight.Add(-1)
				metrics.RateLimited("inflight")
				tooMany(w, r, time.Second)
				return
			}
			defer g.inFlight.Add(-1)
		}
		next.ServeHTTP(w, r)
	})
}

// UsageBuckets are the spend counters (must match httpapi's usageBuckets).
func UsageBuckets(now time.Time) (day, month string) {
	return "tokens:day:" + now.Format("2006-01-02"), "tokens:month:" + now.Format("2006-01")
}

type guarded struct {
	g    *Guard
	next orchestrator.Answerer
}

// Wrap stops answering (UNAVAILABLE + official route) once a spend ceiling
// is reached: the automatic throttle of Doc 06 §13.
func (g *Guard) Wrap(next orchestrator.Answerer) orchestrator.Answerer { return guarded{g, next} }

func (a guarded) Answer(ctx context.Context, in orchestrator.Input) orchestrator.Result {
	stage, failed := a.g.spendStage(ctx)
	if failed || stage == orchestrator.BudgetStatic {
		// Ceiling reached or usage unreadable: fail closed, nothing runs
		// (Doc 06 §13 automatic throttle).
		t := policy.TplUnavailable
		return orchestrator.Result{Status: t.Status, IntentID: "UNKNOWN", Text: t.Text, Citations: []contract.Citation{},
			Actions: []contract.Action{}, KnowledgeReleaseID: "none", Reason: contract.ReasonKillSwitch}
	}
	in.Budget = orchestrator.TighterBudget(in.Budget, stage)
	return a.next.Answer(ctx, in)
}

// spendStage is the staged throttle on the daily and monthly ceilings
// (Doc 06 §13): at 80 % answers are shorter with no repair, at 100 % only
// static approved content is served. failed: usage could not be read.
func (g *Guard) spendStage(ctx context.Context) (stage string, failed bool) {
	day, month := UsageBuckets(g.now().UTC())
	d, err := g.store.Usage(ctx, day)
	if err != nil {
		return "", true
	}
	m, err := g.store.Usage(ctx, month)
	if err != nil {
		return "", true
	}
	ratio := func(window string, used, limit int64) {
		if limit > 0 {
			metrics.SetSpendRatio(window, float64(used)/float64(limit))
		}
	}
	ratio("day", d, g.daily) // drives the 50/80/100 % spend alerts (SEC-T054)
	ratio("month", m, g.monthly)
	at := func(used, limit int64, num, den int64) bool { return limit > 0 && used*den >= limit*num }
	switch {
	case at(d, g.daily, 1, 1) || at(m, g.monthly, 1, 1):
		return orchestrator.BudgetStatic, false
	case at(d, g.daily, 4, 5) || at(m, g.monthly, 4, 5):
		return orchestrator.BudgetReduced, false
	}
	return "", false
}
