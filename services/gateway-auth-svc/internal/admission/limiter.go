// Package admission holds the gateway's pre-authorization protections
// (ZS-SVC-M-001 INT-01): quota enforcement, request-shape limits, API version
// admission, overload shedding, and the bounded record of what was decided.
//
// Everything here is in-process and bounded. The gateway is a stateless
// ForwardAuth verifier on the C0 path, so no protection may add a network
// dependency or grow without limit.
package admission

import (
	"hash/fnv"
	"sync"
	"time"
)

// Limiter is a token-bucket quota, keyed. It is safe for concurrent use: each
// key lives in one of a fixed number of shards, and a shard's mutex covers the
// read-refill-spend of its buckets, so two requests can never both spend the
// same token (INT-NP-005).
//
// Scope: counters are per process. With N replicas the effective ceiling is N
// times the configured rate; the edge proxy's own rate limit is the
// cross-replica ceiling. This limiter is what stops one client from starving
// the verification path of the replica it lands on.
//
// Memory is bounded. When a shard is full, idle buckets are evicted; if it is
// still full, new keys share one overflow bucket instead of growing the map,
// so a flood of distinct keys degrades into one shared quota, not an outage.
type Limiter struct {
	rate     float64 // tokens per second; <= 0 disables the limiter
	burst    float64
	idle     time.Duration
	maxPerSh int
	now      func() time.Time
	shards   [shardCount]shard

	ovMu sync.Mutex
	ov   *bucket // shared by every key that does not fit
}

const shardCount = 64

type bucket struct {
	tokens float64
	last   time.Time
}

type shard struct {
	mu sync.Mutex
	m  map[string]*bucket
}

// NewLimiter builds a limiter. rate <= 0 disables it (Allow always succeeds).
// maxKeys bounds the total tracked keys (0 = 65536). now may be nil.
func NewLimiter(rate float64, burst int, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if maxKeys <= 0 {
		maxKeys = 65536
	}
	if burst < 1 {
		burst = 1
	}
	per := maxKeys / shardCount
	if per < 1 {
		per = 1
	}
	l := &Limiter{rate: rate, burst: float64(burst), idle: 10 * time.Minute, maxPerSh: per, now: now}
	for i := range l.shards {
		l.shards[i].m = make(map[string]*bucket)
	}
	return l
}

// Enabled reports whether the limiter enforces anything.
func (l *Limiter) Enabled() bool { return l != nil && l.rate > 0 }

func (l *Limiter) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &l.shards[h.Sum32()%shardCount]
}

// Allow spends one token for key. When refused it reports how long until a
// token will be available, so Retry-After is honest.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	now := l.now()
	sh := l.shardFor(key)
	sh.mu.Lock()
	b, found := sh.m[key]
	if !found {
		if len(sh.m) >= l.maxPerSh {
			l.evictIdle(sh, now)
		}
		if len(sh.m) >= l.maxPerSh {
			sh.mu.Unlock()
			return l.spendOverflow(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		sh.m[key] = b
	}
	ok, retryAfter = l.spend(b, now)
	sh.mu.Unlock()
	return ok, retryAfter
}

// spendOverflow charges the shared bucket used when a shard is full.
func (l *Limiter) spendOverflow(now time.Time) (bool, time.Duration) {
	l.ovMu.Lock()
	defer l.ovMu.Unlock()
	if l.ov == nil {
		l.ov = &bucket{tokens: l.burst, last: now}
	}
	return l.spend(l.ov, now)
}

// spend refills b for the time elapsed and takes one token if there is one.
// The caller holds whatever lock protects b.
func (l *Limiter) spend(b *bucket, now time.Time) (bool, time.Duration) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second // Retry-After is whole seconds; never advise "now"
	}
	return false, wait
}

func (l *Limiter) evictIdle(sh *shard, now time.Time) {
	for k, b := range sh.m {
		if now.Sub(b.last) > l.idle {
			delete(sh.m, k)
		}
	}
}

// State reports a key's remaining tokens without spending one or creating the
// key (GetRateLimitState). A key never seen has a full bucket.
func (l *Limiter) State(key string) (remaining, burst, rate float64) {
	if !l.Enabled() {
		return 0, 0, 0
	}
	now := l.now()
	sh := l.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b, ok := sh.m[key]
	if !ok {
		return l.burst, l.burst, l.rate
	}
	tokens := b.tokens + now.Sub(b.last).Seconds()*l.rate
	if tokens > l.burst {
		tokens = l.burst
	}
	return tokens, l.burst, l.rate
}

// Keys returns the number of tracked keys, for tests and metrics.
func (l *Limiter) Keys() int {
	n := 0
	for i := range l.shards {
		l.shards[i].mu.Lock()
		n += len(l.shards[i].m)
		l.shards[i].mu.Unlock()
	}
	return n
}
