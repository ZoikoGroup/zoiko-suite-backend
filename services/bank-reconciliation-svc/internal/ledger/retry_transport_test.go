package ledger

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryTransport_RetriesGETOn5xxThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newRetryTransport()}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected eventual 200, got %d", resp.StatusCode)
	}
	if attempts != 3 {
		t.Fatalf("expected exactly 3 attempts (2 failures + 1 success), got %d", attempts)
	}
}

func TestRetryTransport_NeverRetriesPOST(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newRetryTransport()}
	resp, err := client.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt for a POST (never retried), got %d", attempts)
	}
}

func TestRetryTransport_CircuitOpensAfterConsecutiveFailures(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	transport := newRetryTransport()
	client := &http.Client{Transport: transport}

	for i := 0; i < breakerFailureThreshold; i++ {
		resp, err := client.Post(srv.URL, "application/json", nil)
		if err != nil {
			t.Fatalf("attempt %d: unexpected error before breaker trips: %v", i, err)
		}
		resp.Body.Close()
	}
	if attempts != breakerFailureThreshold {
		t.Fatalf("expected %d real attempts to reach the server, got %d", breakerFailureThreshold, attempts)
	}

	// One more call: the breaker should now be open and short-circuit
	// before ever reaching the server.
	_, err := client.Post(srv.URL, "application/json", nil)
	if err == nil {
		t.Fatal("expected an error once the circuit is open, got nil")
	}
	if attempts != breakerFailureThreshold {
		t.Fatalf("expected the open circuit to short-circuit the call (server should still show %d attempts), got %d", breakerFailureThreshold, attempts)
	}
}

func TestRetryTransport_HalfOpenProbeRecoversTheCircuit(t *testing.T) {
	attempts := 0
	failing := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transport := newRetryTransport()
	client := &http.Client{Transport: transport}

	for i := 0; i < breakerFailureThreshold; i++ {
		resp, _ := client.Post(srv.URL, "application/json", nil)
		if resp != nil {
			resp.Body.Close()
		}
	}

	// Force the cooldown to have already elapsed rather than sleeping
	// breakerCooldown in a unit test — this is a white-box test in the
	// same package specifically to reach into that private state.
	transport.mu.Lock()
	transport.openUntil = time.Now().Add(-time.Second)
	transport.mu.Unlock()

	// The dependency has recovered: the half-open probe should succeed
	// and close the circuit again.
	failing = false
	resp, err := client.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("expected the half-open probe to succeed, got error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from the probe, got %d", resp.StatusCode)
	}

	transport.mu.Lock()
	fails := transport.consecutiveFail
	transport.mu.Unlock()
	if fails != 0 {
		t.Fatalf("expected the successful probe to reset consecutiveFail to 0, got %d", fails)
	}
}
