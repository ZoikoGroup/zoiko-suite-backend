package authz

import (
	"net/http"
	"time"
)

// retryTransport retries idempotent (GET/HEAD) requests with exponential
// backoff on network errors or a 5xx response. Mutating requests (POST,
// PUT, PATCH, DELETE) are never retried here — retrying a mutation without
// an idempotency key could duplicate its effect, which is a separate,
// larger piece of work than a transport-level retry policy. This exists
// because every cross-service call in this platform previously had a flat
// timeout and nothing else: one slow hop was a hard failure, which matters
// more on higher-latency international links than on a local dev network.
type retryTransport struct {
	base     http.RoundTripper
	maxTries int
	backoff  time.Duration
}

func newRetryTransport() *retryTransport {
	return &retryTransport{base: http.DefaultTransport, maxTries: 3, backoff: 100 * time.Millisecond}
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return t.base.RoundTrip(req)
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
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
		}
	}
	return resp, err
}
