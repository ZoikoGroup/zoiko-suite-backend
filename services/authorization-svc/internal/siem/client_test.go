package siem_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/siem"
)

const testTenant = "00000000-0000-0000-0000-0000000000a1"

// TestStream_DoesNotBlockTheCaller is the regression test for the measured
// defect: Stream did its HTTP inline, so an unreachable or slow
// siem-integration-svc added its own latency to every DENIED authorization
// decision — 850ms median against a service that was not running, on the
// platform's hottest endpoint.
//
// The upstream here sleeps far longer than any acceptable request budget. If
// Stream is synchronous this test fails on the elapsed assertion; if it is
// fire-and-forget it returns immediately regardless of what upstream does.
func TestStream_DoesNotBlockTheCaller(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int64

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // hold the request open
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	defer close(release)

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())
	defer c.Close()

	start := time.Now()
	c.Stream(context.Background(), testTenant, "authorization.denied", siem.SeverityHigh, "denied")
	elapsed := time.Since(start)

	// Generous bound: the point is orders of magnitude, not microseconds.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Stream blocked the caller for %v — streaming is a side-channel and must never "+
			"add its own latency to an authorization decision", elapsed)
	}
}

// TestStream_DoesNotUseTheRequestContext. The delivery must not be bound to
// the caller's request context: that context is cancelled the moment the
// response is written, so honouring it would cancel every event the function
// exists to send. Passing an ALREADY-cancelled context here is the sharpest
// version of that check.
func TestStream_DoesNotUseTheRequestContext(t *testing.T) {
	delivered := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/siem/exporters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"exp-1","status":"ACTIVE"}]}`))
			return
		}
		delivered <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request is already over

	c.Stream(ctx, testTenant, "authorization.denied", siem.SeverityHigh, "denied")

	select {
	case path := <-delivered:
		if path != "/v1/siem/stream" {
			t.Fatalf("delivered to %q, want /v1/siem/stream", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was delivered — the event was bound to the caller's cancelled request context, " +
			"which would drop every event in production since the context is cancelled as the response is written")
	}
}

// TestStream_DeliversToEveryActiveExporter pins that making this async did not
// change what actually gets sent, and that an inactive exporter is skipped.
func TestStream_DeliversToEveryActiveExporter(t *testing.T) {
	var streams atomic.Int64
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/siem/exporters" {
			if got := r.Header.Get("X-Tenant-ID"); got != testTenant {
				t.Errorf("exporter lookup sent tenant %q, want %q", got, testTenant)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"exp-1","status":"ACTIVE"},
				{"id":"exp-2","status":"ACTIVE"},
				{"id":"exp-3","status":"SUSPENDED"}
			]}`))
			return
		}
		if streams.Add(1) == 2 {
			close(done)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())
	defer c.Close()

	c.Stream(context.Background(), testTenant, "authorization.denied", siem.SeverityHigh, "denied")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("delivered %d streams, want 2 (the ACTIVE exporters only)", streams.Load())
	}
}

// TestClose_DrainsAcceptedEvents — Stream returns before delivery, so a
// SIGTERM immediately afterwards must not discard an event already accepted.
func TestClose_DrainsAcceptedEvents(t *testing.T) {
	var streams atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/siem/exporters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"exp-1","status":"ACTIVE"}]}`))
			return
		}
		streams.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())
	for i := 0; i < 5; i++ {
		c.Stream(context.Background(), testTenant, "authorization.denied", siem.SeverityHigh, "denied")
	}
	c.Close() // must block until the queue is drained

	if got := streams.Load(); got != 5 {
		t.Fatalf("delivered %d of 5 accepted events after Close — Close is not draining", got)
	}
}

// TestStream_DisabledStartsNothing. SIEM_SERVICE_URL empty is the documented
// off switch and the default; it must allocate no queue and start no
// goroutines, and Close on it must be safe.
func TestStream_DisabledStartsNothing(t *testing.T) {
	c := siem.New("", "authorization-svc", zap.NewNop())

	start := time.Now()
	for i := 0; i < 1000; i++ {
		c.Stream(context.Background(), testTenant, "authorization.denied", siem.SeverityHigh, "denied")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("1000 no-op Streams took %v", elapsed)
	}
	if c.Dropped() != 0 {
		t.Errorf("disabled client counted %d drops — nothing was ever queued", c.Dropped())
	}
	c.Close() // must not panic or hang
}

// TestStream_NoTenantIsANoOp — the exporter lookup is per-tenant, so an event
// with no tenant has nowhere to go. It must not occupy a queue slot.
func TestStream_NoTenantIsANoOp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream called for a tenantless event: %s", r.URL.Path)
	}))
	defer upstream.Close()

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())
	c.Stream(context.Background(), "", "authorization.denied", siem.SeverityHigh, "denied")
	c.Close()
}

// TestStream_DropsRatherThanBlocksWhenSaturated. A full queue must discard,
// not block: blocking is the behaviour being removed, and an unbounded queue
// would turn a SIEM outage into this service's memory problem. The drop is
// counted so the gap is measurable rather than silent.
func TestStream_DropsRatherThanBlocksWhenSaturated(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := siem.New(upstream.URL, "authorization-svc", zap.NewNop())

	// Far more than the queue depth, with every worker wedged on the upstream.
	start := time.Now()
	for i := 0; i < 5000; i++ {
		c.Stream(context.Background(), testTenant, "authorization.denied", siem.SeverityHigh, "denied")
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("5000 Streams against a wedged upstream took %v — the queue is blocking, not dropping", elapsed)
	}
	if c.Dropped() == 0 {
		t.Fatal("no drops recorded after saturating the queue — a full queue must drop loudly, not grow")
	}

	close(release)
	c.Close()
}
