package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Decision is one APIRequestDecision (ZS-SVC-M-001 §9): why a request was not
// admitted. It carries identifiers and a hashed client key, never a token, a
// query string, a body or a raw address (INT-CTRL-035).
type Decision struct {
	ID            string    `json:"decision_id"`
	At            time.Time `json:"at"`
	TenantID      string    `json:"tenant_id,omitempty"`
	PrincipalID   string    `json:"principal_id,omitempty"`
	ClientKey     string    `json:"client_key"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	APIVersion    string    `json:"api_version,omitempty"`
	Outcome       string    `json:"outcome"`
	Code          string    `json:"code"`
	Status        int       `json:"status"`
	Detail        string    `json:"detail,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
}

// Usage counts what a tenant's traffic has been through since Since.
type Usage struct {
	Allowed     uint64    `json:"allowed"`
	Denied      uint64    `json:"denied"`
	RateLimited uint64    `json:"rate_limited"`
	Shed        uint64    `json:"shed"`
	Since       time.Time `json:"since"`
}

// Outcome labels the recorder classifies counters by. They are the same strings
// the handler and telemetry use.
const (
	OutcomeRateLimited = "rate_limited"
	OutcomeOverloaded  = "overloaded"
)

const (
	maxTrackedTenants = 10000
	otherTenants      = "~other"
	maxTrackedClients = 10000
)

// ClientKey derives the identifier used for per-client limits and evidence from
// a client address. Hashed so evidence and in-memory state never retain the
// address itself.
func ClientKey(addr string) string {
	sum := sha256.Sum256([]byte(addr))
	return hex.EncodeToString(sum[:])[:16]
}

// Recorder keeps a bounded recent window of refused requests plus per-tenant
// counters and an abuse detector. It is in-memory by decision: the gateway has
// no database, durable evidence is the SIEM event stream and the metrics, and
// this window answers "what happened just now" for operators.
type Recorder struct {
	now       func() time.Time
	threshold int

	mu       sync.Mutex
	ring     []Decision
	next     int
	full     bool
	usage    map[string]*Usage
	abuse    map[string]*abuseWindow
	throttle map[string]time.Time
	started  time.Time
}

type abuseWindow struct {
	start   time.Time
	count   int
	alerted bool
}

// NewRecorder builds a recorder holding the last capacity refusals. threshold
// is how many refusals one client may collect in a minute before it counts as
// abusive (0 disables abuse detection).
func NewRecorder(capacity, threshold int, now func() time.Time) *Recorder {
	if capacity < 1 {
		capacity = 1024
	}
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		now: now, threshold: threshold,
		ring:     make([]Decision, capacity),
		usage:    make(map[string]*Usage),
		abuse:    make(map[string]*abuseWindow),
		throttle: make(map[string]time.Time),
		started:  now(),
	}
}

// Record stores a refusal, assigns its id and time, updates counters and
// reports whether this refusal pushed its client over the abuse threshold (true
// exactly once per client per minute).
func (r *Recorder) Record(d Decision) (stored Decision, abusive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	d.At = r.now().UTC()
	r.ring[r.next] = d
	r.next = (r.next + 1) % len(r.ring)
	if r.next == 0 {
		r.full = true
	}

	u := r.usageFor(d.TenantID)
	switch d.Outcome {
	case OutcomeRateLimited:
		u.RateLimited++
	case OutcomeOverloaded:
		u.Shed++
	default:
		u.Denied++
	}

	if r.threshold > 0 && d.ClientKey != "" {
		w, ok := r.abuse[d.ClientKey]
		if !ok || d.At.Sub(w.start) >= time.Minute {
			if !ok && len(r.abuse) >= maxTrackedClients {
				r.sweepAbuse(d.At)
			}
			if len(r.abuse) < maxTrackedClients || ok {
				w = &abuseWindow{start: d.At}
				r.abuse[d.ClientKey] = w
			}
		}
		if w != nil {
			w.count++
			if w.count >= r.threshold && !w.alerted {
				w.alerted = true
				abusive = true
			}
		}
	}
	return d, abusive
}

func (r *Recorder) sweepAbuse(now time.Time) {
	for k, w := range r.abuse {
		if now.Sub(w.start) >= time.Minute {
			delete(r.abuse, k)
		}
	}
}

func (r *Recorder) usageFor(tenant string) *Usage {
	if tenant == "" {
		tenant = "~unauthenticated"
	}
	u, ok := r.usage[tenant]
	if !ok {
		if len(r.usage) >= maxTrackedTenants {
			tenant = otherTenants
			if u, ok = r.usage[tenant]; ok {
				return u
			}
		}
		u = &Usage{Since: r.started}
		r.usage[tenant] = u
	}
	return u
}

// Allowed counts an admitted request for tenant.
func (r *Recorder) Allowed(tenant string) {
	r.mu.Lock()
	r.usageFor(tenant).Allowed++
	r.mu.Unlock()
}

// Get returns a recorded refusal by id (GetGatewayDecision).
func (r *Recorder) Get(id string) (Decision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.ring {
		if d.ID == id && id != "" {
			return d, true
		}
	}
	return Decision{}, false
}

// ListDenied returns recent refusals, newest first, optionally for one tenant
// (ListDeniedRequests). limit is clamped to the window size.
func (r *Recorder) ListDenied(tenant string, limit int) []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit < 1 || limit > len(r.ring) {
		limit = len(r.ring)
	}
	n := len(r.ring)
	if !r.full {
		n = r.next
	}
	var out []Decision
	for i := 1; i <= n && len(out) < limit; i++ {
		d := r.ring[(r.next-i+len(r.ring))%len(r.ring)]
		if tenant != "" && d.TenantID != tenant {
			continue
		}
		out = append(out, d)
	}
	return out
}

// UsageOf returns a tenant's counters (GetClientUsage).
func (r *Recorder) UsageOf(tenant string) (Usage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.usage[tenant]
	if !ok {
		return Usage{}, false
	}
	return *u, true
}

// MayEmit reports whether an event for key may be emitted now, and if so marks
// it. It keeps a flood of identical refusals from becoming a flood of identical
// events: the metrics still count every refusal.
func (r *Recorder) MayEmit(key string, every time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if last, ok := r.throttle[key]; ok && now.Sub(last) < every {
		return false
	}
	if len(r.throttle) >= maxTrackedClients {
		for k, t := range r.throttle {
			if now.Sub(t) >= every {
				delete(r.throttle, k)
			}
		}
		if len(r.throttle) >= maxTrackedClients {
			return false // bounded: when saturated, suppress rather than grow
		}
	}
	r.throttle[key] = now
	return true
}
