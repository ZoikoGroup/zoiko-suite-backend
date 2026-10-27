package idenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrRevoked is an envelope whose session identity-context has ended (logout,
// invalidation, a disabled principal, an ended login session).
var ErrRevoked = errors.New("identity session revoked")

// ErrRevocationUnknown is a revocation check that could not be answered.
var ErrRevocationUnknown = errors.New("identity session status unavailable")

// SessionStatus is identity-context's answer for one session.
type SessionStatus struct {
	Active           bool   `json:"active"`
	SessionContextID string `json:"session_context_id"`
	SID              string `json:"sid,omitempty"`
	// InvalidatedAt is when the session ended (Unix seconds), 0 when active.
	InvalidatedAt int64  `json:"invalidated_at,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// StatusSource answers whether a session is live. Introspector is the
// production implementation; tests use a func.
type StatusSource interface {
	SessionStatus(ctx context.Context, token string, c *Claims) (SessionStatus, error)
}

// StatusFunc adapts a func to StatusSource.
type StatusFunc func(ctx context.Context, token string, c *Claims) (SessionStatus, error)

// SessionStatus implements StatusSource.
func (f StatusFunc) SessionStatus(ctx context.Context, token string, c *Claims) (SessionStatus, error) {
	return f(ctx, token, c)
}

// Introspector calls identity-context-svc's
// POST /v1/context/session/introspect with the envelope itself, so the answer
// is only ever given to a holder of a signed envelope (a session id alone
// reveals nothing).
type Introspector struct {
	url    string
	client *http.Client
}

// NewIntrospector builds the client. client may be nil (3 s timeout).
func NewIntrospector(url string, client *http.Client) *Introspector {
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &Introspector{url: url, client: client}
}

// SessionStatus implements StatusSource.
func (in *Introspector) SessionStatus(ctx context.Context, token string, _ *Claims) (SessionStatus, error) {
	body, _ := json.Marshal(map[string]string{"token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.url, bytes.NewReader(body))
	if err != nil {
		return SessionStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := in.client.Do(req)
	if err != nil {
		return SessionStatus{}, fmt.Errorf("introspect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SessionStatus{}, fmt.Errorf("introspect: unexpected status %d", resp.StatusCode)
	}
	var st SessionStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return SessionStatus{}, fmt.Errorf("introspect: decode: %w", err)
	}
	return st, nil
}

// FailMode is what a revocation check does when identity-context cannot
// answer.
type FailMode string

const (
	// FailClosed refuses the request (503). IAM §19: governed and high-risk
	// operations fail closed when a policy information point is unavailable.
	FailClosed FailMode = "closed"
	// FailOpen admits it and reports. Only for read-only, low-risk services by
	// explicit decision.
	FailOpen FailMode = "open"
)

// Revocation checks envelopes against identity-context's session register.
//
// A live answer is cached for ActiveTTL (the revocation lag ceiling at this
// relying party, IAM §26; default 5 s). An ended session is remembered until
// its envelope expires, so a revoked envelope costs one introspection here,
// not one per request. Revoke lets a service that consumes session.invalidated
// / identity.session.revoked push a revocation in ahead of the cache.
type Revocation struct {
	source    StatusSource
	activeTTL time.Duration
	fail      FailMode
	observer  Observer
	now       func() time.Time

	mu       sync.Mutex
	entries  map[string]revEntry
	inflight map[string]*inflightCall
	sids     map[string]time.Time // ended login sessions, until expiry
}

type revEntry struct {
	active  bool
	until   time.Time
	ended   int64
	reason  string
	learned time.Time
}

type inflightCall struct {
	done chan struct{}
	st   SessionStatus
	err  error
}

// RevocationOptions configures NewRevocation.
type RevocationOptions struct {
	Source    StatusSource
	ActiveTTL time.Duration
	Fail      FailMode
	Observer  Observer
	Now       func() time.Time
}

// NewRevocation builds a checker.
func NewRevocation(o RevocationOptions) *Revocation {
	if o.ActiveTTL <= 0 {
		o.ActiveTTL = 5 * time.Second
	}
	if o.Fail != FailOpen {
		o.Fail = FailClosed
	}
	if o.Observer == nil {
		o.Observer = NopObserver{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Revocation{source: o.Source, activeTTL: o.ActiveTTL, fail: o.Fail, observer: o.Observer, now: o.Now,
		entries: map[string]revEntry{}, inflight: map[string]*inflightCall{}, sids: map[string]time.Time{}}
}

// ActiveTTL is the cache lifetime of a live answer (startup log).
func (r *Revocation) ActiveTTL() time.Duration { return r.activeTTL }

// FailMode is the configured fail mode.
func (r *Revocation) FailMode() FailMode { return r.fail }

// Revoke records that a session ended, ahead of the cache (event consumers).
func (r *Revocation) Revoke(sessionContextID string, at time.Time, until time.Time) {
	if sessionContextID == "" {
		return
	}
	if until.IsZero() {
		until = r.now().Add(time.Hour)
	}
	r.mu.Lock()
	r.entries[sessionContextID] = revEntry{active: false, until: until, ended: at.Unix(), reason: "pushed", learned: r.now()}
	r.mu.Unlock()
}

// RevokeSID records that a login session ended: every envelope carrying it
// is refused until until.
func (r *Revocation) RevokeSID(sid string, until time.Time) {
	if sid == "" {
		return
	}
	if until.IsZero() {
		until = r.now().Add(time.Hour)
	}
	r.mu.Lock()
	r.sids[sid] = until
	r.mu.Unlock()
}

// Check returns nil for a live session, ErrRevoked for an ended one, and
// ErrRevocationUnknown (fail closed) when the answer is unavailable.
func (r *Revocation) Check(ctx context.Context, token string, c *Claims) error {
	if r == nil || r.source == nil {
		return nil
	}
	sc := c.SessionContextID()
	if sc == "" {
		// Every envelope identity-context mints names its session; one that
		// does not cannot be revoked and is refused.
		return fmt.Errorf("%w: envelope names no session", ErrRevoked)
	}
	now := r.now()

	r.mu.Lock()
	r.sweepLocked(now)
	if until, ok := r.sids[c.SID]; ok && c.SID != "" && now.Before(until) {
		r.mu.Unlock()
		return fmt.Errorf("%w: login session ended", ErrRevoked)
	}
	e, ok := r.entries[sc]
	if ok && now.Before(e.until) {
		r.mu.Unlock()
		if e.active {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrRevoked, e.reason)
	}
	wasActive := ok && e.active
	call, running := r.inflight[sc]
	if !running {
		call = &inflightCall{done: make(chan struct{})}
		r.inflight[sc] = call
	}
	r.mu.Unlock()

	if !running {
		call.st, call.err = r.source.SessionStatus(ctx, token, c)
		r.mu.Lock()
		delete(r.inflight, sc)
		if call.err == nil {
			learned := r.now()
			if call.st.Active {
				r.entries[sc] = revEntry{active: true, until: learned.Add(r.activeTTL), learned: learned}
			} else {
				until := c.Expiry().Add(time.Minute)
				if until.Before(learned) {
					until = learned.Add(time.Minute)
				}
				r.entries[sc] = revEntry{active: false, until: until, ended: call.st.InvalidatedAt,
					reason: nonEmpty(call.st.Reason, "session ended"), learned: learned}
				if wasActive && call.st.InvalidatedAt > 0 {
					// This relying party was admitting the session until now:
					// the gap between the revocation and this moment is how
					// long the revocation took to bite here (IAM §26).
					lag := learned.Sub(time.Unix(call.st.InvalidatedAt, 0))
					if lag < 0 {
						lag = 0
					}
					r.observer.RevocationLag(lag)
				}
			}
		}
		r.mu.Unlock()
		close(call.done)
	} else {
		select {
		case <-call.done:
		case <-ctx.Done():
			return r.unknown(ctx.Err())
		}
	}

	if call.err != nil {
		return r.unknown(call.err)
	}
	if !call.st.Active {
		return fmt.Errorf("%w: %s", ErrRevoked, nonEmpty(call.st.Reason, "session ended"))
	}
	return nil
}

func (r *Revocation) unknown(err error) error {
	r.observer.RevocationUnavailable()
	if r.fail == FailOpen {
		return nil
	}
	return errors.Join(ErrRevocationUnknown, err)
}

func (r *Revocation) sweepLocked(now time.Time) {
	if len(r.entries) < 4096 && len(r.sids) < 4096 {
		return
	}
	for k, e := range r.entries {
		if !now.Before(e.until) {
			delete(r.entries, k)
		}
	}
	for k, until := range r.sids {
		if !now.Before(until) {
			delete(r.sids, k)
		}
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
