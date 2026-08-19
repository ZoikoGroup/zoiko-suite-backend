package authz

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// retryTransport retries idempotent (GET/HEAD) requests with exponential
// backoff on network errors or a 5xx response, and trips a circuit breaker
// after repeated consecutive failures of ANY method so a genuinely down
// dependency stops taking requests it can't serve instead of every caller
// separately paying its own timeout (03-microservices.md §17.7's circuit-
// breaker mandate — previously unimplemented anywhere on this platform;
// this file's own retry logic existed, the breaker half did not).
//
// Mutating requests (POST, PUT, PATCH, DELETE) are never retried here —
// retrying a mutation without an idempotency key could duplicate its
// effect, which is a separate, larger piece of work than a transport-level
// retry policy. The breaker still applies to them: once open, every
// method fails fast rather than blocking behind a dependency that's down.
type retryTransport struct {
	base     http.RoundTripper
	maxTries int
	backoff  time.Duration

	mu              sync.Mutex
	consecutiveFail int
	openUntil       time.Time
}

// breakerFailureThreshold consecutive failures (not one) trips the
// breaker — a single blip is not "down". breakerCooldown is how long it
// stays open before allowing one probe request through (half-open).
const (
	breakerFailureThreshold = 5
	breakerCooldown         = 10 * time.Second
)

var errCircuitOpen = errors.New("circuit breaker open: too many consecutive failures calling this dependency")

func newRetryTransport() *retryTransport {
	return &retryTransport{base: http.DefaultTransport, maxTries: 3, backoff: 100 * time.Millisecond}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if open, err := t.circuitOpen(); open {
		return nil, err
	}

	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		resp, err := t.base.RoundTrip(req)
		t.recordOutcome(err == nil && resp.StatusCode < 500)
		return resp, err
	}

	backoff := t.backoff
	var resp *http.Response
	var err error
	for attempt := 0; attempt < t.maxTries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		resp, err = t.base.RoundTrip(req)
		if err == nil && resp.StatusCode < 500 {
			t.recordOutcome(true)
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
		}
	}
	t.recordOutcome(false)
	return resp, err
}

// circuitOpen reports whether the breaker is currently open. Once it has
// been open for longer than breakerCooldown, the NEXT call is let through
// as a half-open probe — its outcome (via recordOutcome, the same path
// every other call uses) decides whether the breaker closes again or
// reopens for another full cooldown.
func (t *retryTransport) circuitOpen() (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.consecutiveFail < breakerFailureThreshold {
		return false, nil
	}
	if time.Now().After(t.openUntil) {
		return false, nil
	}
	return true, errCircuitOpen
}

func (t *retryTransport) recordOutcome(success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if success {
		t.consecutiveFail = 0
		return
	}
	t.consecutiveFail++
	if t.consecutiveFail >= breakerFailureThreshold {
		t.openUntil = time.Now().Add(breakerCooldown)
	}
}
