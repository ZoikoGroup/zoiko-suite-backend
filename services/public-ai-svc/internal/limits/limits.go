// Package limits enforces abuse and cost controls (Doc 06 §13, Doc 08 §25):
// per-IP and per-session token buckets, a global in-flight turn cap, and
// daily/monthly token ceilings that stop model use before they are exceeded.
// Numeric values are dev defaults pending CISO/SRE approval (D06-05).
package limits

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	buckets map[string]*bucket
}

func newLimiter(perMin int) *limiter {
	b := float64(perMin) / 4
	if b < 3 {
		b = 3
	}
	return &limiter{perMin: float64(perMin), burst: b, buckets: map[string]*bucket{}}
}

// allow takes one token for key, or returns the wait until one is available.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l.perMin <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 100_000 { // bound memory: drop idle buckets
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
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
	ip, sess *limiter
	trusted  []*net.IPNet
	inFlight atomic.Int64
	store    store.Store
	flags    interface{ Values() flags.Values }
	daily    int64
	monthly  int64
	now      func() time.Time
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
		store: st, flags: fl, daily: cfg.DailyTokenCeiling, monthly: cfg.MonthlyTokenCeiling,
		now: time.Now,
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

// clientKey coarsens the client address: IPv4 as is, IPv6 to /64.
func clientKey(ip net.IP) string {
	if ip == nil {
		return "ip:unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return "ip:" + v4.String()
	}
	return "ip6:" + ip.Mask(net.CIDRMask(64, 128)).String()
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
		if ok, wait := g.ip.allow(clientKey(g.clientIP(r)), now); !ok {
			tooMany(w, r, wait)
			return
		}
		if grant := r.Header.Get("X-Public-Chat-Session"); grant != "" {
			sum := sha256.Sum256([]byte(grant)) // never keep the grant itself
			if ok, wait := g.sess.allow("s:"+string(sum[:12]), now); !ok {
				tooMany(w, r, wait)
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/public-ai/v1/turns" {
			max := int64(g.flags.Values().MaxConcurrentTurns)
			if n := g.inFlight.Add(1); max > 0 && n > max {
				g.inFlight.Add(-1)
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

// overCeiling reports whether either spend ceiling is reached. Errors fail
// closed: if usage cannot be read, the model is not called.
func (g *Guard) overCeiling(ctx context.Context) bool {
	day, month := UsageBuckets(g.now().UTC())
	d, err := g.store.Usage(ctx, day)
	if err != nil {
		return true
	}
	m, err := g.store.Usage(ctx, month)
	if err != nil {
		return true
	}
	return (g.daily > 0 && d >= g.daily) || (g.monthly > 0 && m >= g.monthly)
}

type guarded struct {
	g    *Guard
	next orchestrator.Answerer
}

// Wrap stops answering (UNAVAILABLE + official route) once a spend ceiling
// is reached: the automatic throttle of Doc 06 §13.
func (g *Guard) Wrap(next orchestrator.Answerer) orchestrator.Answerer { return guarded{g, next} }

func (a guarded) Answer(ctx context.Context, in orchestrator.Input) orchestrator.Result {
	if a.g.overCeiling(ctx) {
		t := policy.TplUnavailable
		return orchestrator.Result{Status: t.Status, IntentID: "UNKNOWN", Text: t.Text, Citations: []contract.Citation{},
			Actions: []contract.Action{}, KnowledgeReleaseID: "none"}
	}
	return a.next.Answer(ctx, in)
}
