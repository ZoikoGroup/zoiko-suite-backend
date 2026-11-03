// Package ratelimit throttles the credential endpoints by source (AU-04).
//
// Per-credential lockout bounds guessing against ONE account. It does not
// bound a spray: 25 wrong passwords against 25 accounts from one client all
// answered 401 and none 429 (live probe, 14 Oct 2026). Three limits close it:
//
//   - attempts per client IP per minute on every credential endpoint;
//   - FAILED logins per client IP per window (the spray signature);
//   - failed logins per tenant per window, from all sources (a distributed
//     spray against one customer).
//
// The client IP is the real one: X-Forwarded-For is believed only from a
// configured trusted proxy, walking the list from the right. chi's RealIP,
// which this service used before, believed the header from anyone, so a
// spray could rotate a forged X-Real-IP per request and never be counted.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientIP resolves the originating client of r. trusted are the proxies
// whose X-Forwarded-For entries are believed.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := hostOnly(r.RemoteAddr)
	if !isTrusted(peer, trusted) {
		return peer
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := hostOnly(hops[i])
		if net.ParseIP(ip) == nil {
			return peer
		}
		if !isTrusted(ip, trusted) {
			return ip
		}
	}
	if len(hops) > 0 {
		return hostOnly(hops[0])
	}
	return peer
}

func hostOnly(s string) string {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return strings.Trim(s, "[]")
}

func isTrusted(ip string, trusted []*net.IPNet) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// Middleware sets the resolved client IP as r.RemoteAddr so everything
// downstream (logs, evidence, the limiter) reads the same value. It replaces
// chi's RealIP.
func Middleware(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r, trusted)
			if ip != "" {
				r.RemoteAddr = net.JoinHostPort(ip, "0")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Counter is a fixed-window counter store.
type Counter interface {
	// Incr adds one to key and returns the new count and the window's
	// remaining lifetime.
	Incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error)
	// Get returns key's count and remaining lifetime (0 when absent).
	Get(ctx context.Context, key string) (int64, time.Duration, error)
}

// RedisCounter is the production Counter, shared by every replica.
type RedisCounter struct{ rdb *redis.Client }

// NewRedisCounter builds one.
func NewRedisCounter(rdb *redis.Client) *RedisCounter { return &RedisCounter{rdb: rdb} }

var incrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return {n, redis.call('PTTL', KEYS[1])}`)

// Incr implements Counter.
func (c *RedisCounter) Incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	res, err := incrScript.Run(ctx, c.rdb, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	return res[0], time.Duration(res[1]) * time.Millisecond, nil
}

// Get implements Counter.
func (c *RedisCounter) Get(ctx context.Context, key string) (int64, time.Duration, error) {
	n, err := c.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	ttl, err := c.rdb.PTTL(ctx, key).Result()
	if err != nil {
		return n, 0, nil
	}
	return n, ttl, nil
}

// MemoryCounter is an in-process Counter (tests, and the fallback when
// Redis errors so throttling never silently turns off).
type MemoryCounter struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	n     int64
	until time.Time
}

// NewMemoryCounter builds one.
func NewMemoryCounter() *MemoryCounter { return &MemoryCounter{m: map[string]memEntry{}} }

// Incr implements Counter.
func (c *MemoryCounter) Incr(_ context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	e := c.m[key]
	if now.After(e.until) {
		e = memEntry{until: now.Add(window)}
	}
	e.n++
	c.m[key] = e
	return e.n, e.until.Sub(now), nil
}

// Get implements Counter.
func (c *MemoryCounter) Get(_ context.Context, key string) (int64, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[key]
	if time.Now().After(e.until) {
		return 0, 0, nil
	}
	return e.n, time.Until(e.until), nil
}

// Limits are the thresholds.
type Limits struct {
	AttemptsPerIPPerMinute int
	FailuresPerIP          int
	FailuresPerTenant      int
	FailureWindow          time.Duration
}

// Limiter applies Limits.
type Limiter struct {
	store    Counter
	fallback Counter
	limits   Limits
	onError  func(error)
}

// New builds a limiter over store (falling back to memory on store errors).
func New(store Counter, limits Limits, onError func(error)) *Limiter {
	if onError == nil {
		onError = func(error) {}
	}
	return &Limiter{store: store, fallback: NewMemoryCounter(), limits: limits, onError: onError}
}

// Decision is a throttle outcome.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	// Reason is ip_rate, ip_failures or tenant_failures.
	Reason string
}

func (l *Limiter) incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration) {
	n, ttl, err := l.store.Incr(ctx, key, window)
	if err != nil {
		l.onError(err)
		n, ttl, _ = l.fallback.Incr(ctx, key, window)
	}
	return n, ttl
}

func (l *Limiter) get(ctx context.Context, key string) (int64, time.Duration) {
	n, ttl, err := l.store.Get(ctx, key)
	if err != nil {
		l.onError(err)
		n, ttl, _ = l.fallback.Get(ctx, key)
	}
	return n, ttl
}

// Attempt counts one credential attempt by ip against tenant (tenant may be
// "") on endpoint and decides whether it may proceed. It is called before
// the credential is checked, so a blocked source learns nothing.
func (l *Limiter) Attempt(ctx context.Context, endpoint, ip, tenant string) Decision {
	ipk := hashKey(ip)
	if n, ttl := l.incr(ctx, "rl:attempt:"+endpoint+":"+ipk, time.Minute); n > int64(l.limits.AttemptsPerIPPerMinute) {
		return Decision{RetryAfter: ttl, Reason: "ip_rate"}
	}
	if n, ttl := l.get(ctx, "rl:fail:ip:"+ipk); n >= int64(l.limits.FailuresPerIP) {
		return Decision{RetryAfter: ttl, Reason: "ip_failures"}
	}
	if tenant != "" {
		if n, ttl := l.get(ctx, "rl:fail:tenant:"+hashKey(tenant)); n >= int64(l.limits.FailuresPerTenant) {
			return Decision{RetryAfter: ttl, Reason: "tenant_failures"}
		}
	}
	return Decision{Allowed: true}
}

// Failure records one failed credential check.
func (l *Limiter) Failure(ctx context.Context, ip, tenant string) {
	l.incr(ctx, "rl:fail:ip:"+hashKey(ip), l.limits.FailureWindow)
	if tenant != "" {
		l.incr(ctx, "rl:fail:tenant:"+hashKey(tenant), l.limits.FailureWindow)
	}
}

// Budget counts a generic per-key budget (e.g. reset-link requests per IP
// per hour) and reports whether it is exhausted.
func (l *Limiter) Budget(ctx context.Context, name, key string, max int, window time.Duration) Decision {
	n, ttl := l.incr(ctx, "rl:budget:"+name+":"+hashKey(key), window)
	if n > int64(max) {
		return Decision{RetryAfter: ttl, Reason: name}
	}
	return Decision{Allowed: true}
}

// RetryAfterSeconds renders d.RetryAfter for the Retry-After header (≥1).
func (d Decision) RetryAfterSeconds() string {
	s := int64(d.RetryAfter.Round(time.Second) / time.Second)
	if s < 1 {
		s = 1
	}
	return fmt.Sprintf("%d", s)
}

// HashIP is the pseudonymous IP recorded on evidence rows (no raw IP at rest).
func HashIP(ip string) string { return hashKey(ip) }

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:12])
}
