package close

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

// retryTransport retries idempotent (GET/HEAD) requests with exponential
// backoff on network errors or a 5xx response, and trips a circuit breaker
// after repeated consecutive failures of ANY method — same shape as
// internal/ledger's own retryTransport in this service, copied rather than
// shared across packages per this codebase's existing per-package
// convention (see internal/authz, internal/ledger).
type retryTransport struct {
	base     http.RoundTripper
	maxTries int
	backoff  time.Duration

	mu              sync.Mutex
	consecutiveFail int
	openUntil       time.Time
}

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
