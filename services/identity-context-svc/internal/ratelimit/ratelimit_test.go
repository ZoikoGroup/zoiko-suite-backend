package ratelimit

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func cidrs(t *testing.T, s ...string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range s {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	trusted := cidrs(t, "10.0.0.0/8")
	r := httptest.NewRequest("POST", "/v1/authenticate", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := ClientIP(r, trusted); got != "203.0.113.9" {
		t.Fatalf("untrusted peer's XFF believed: %s", got)
	}
	r.RemoteAddr = "10.0.0.5:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7, 10.0.0.9")
	if got := ClientIP(r, trusted); got != "198.51.100.7" {
		t.Fatalf("got %s, want the first untrusted hop from the right", got)
	}
	if got := ClientIP(r, nil); got != "10.0.0.5" {
		t.Fatalf("no trusted proxies: got %s, want the TCP peer", got)
	}
}

// The 14 Oct probe: 25 failed logins against 25 accounts from one client must
// be throttled, not answered 401 every time.
func TestSprayFromOneSourceIsThrottled(t *testing.T) {
	l := New(NewMemoryCounter(), Limits{AttemptsPerIPPerMinute: 100, FailuresPerIP: 10, FailuresPerTenant: 1000,
		FailureWindow: 10 * time.Minute}, nil)
	ctx := context.Background()
	blocked := 0
	for i := 0; i < 25; i++ {
		d := l.Attempt(ctx, "authenticate", "203.0.113.9", "t1")
		if !d.Allowed {
			blocked++
			if d.Reason != "ip_failures" || d.RetryAfterSeconds() == "0" {
				t.Fatalf("decision = %+v", d)
			}
			continue
		}
		l.Failure(ctx, "203.0.113.9", fmt.Sprintf("t1"))
	}
	if blocked != 15 {
		t.Fatalf("blocked %d of 25, want 15 (after 10 failures)", blocked)
	}
	if d := l.Attempt(ctx, "authenticate", "198.51.100.1", "t1"); !d.Allowed {
		t.Fatal("another source must not be blocked by the first one's failures")
	}
}

func TestTenantWideSprayAndRate(t *testing.T) {
	l := New(NewMemoryCounter(), Limits{AttemptsPerIPPerMinute: 3, FailuresPerIP: 100, FailuresPerTenant: 5,
		FailureWindow: time.Minute}, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		l.Failure(ctx, fmt.Sprintf("10.9.0.%d", i), "t1")
	}
	if d := l.Attempt(ctx, "authenticate", "10.9.9.9", "t1"); d.Allowed || d.Reason != "tenant_failures" {
		t.Fatalf("distributed spray: %+v", d)
	}
	for i := 0; i < 3; i++ {
		if d := l.Attempt(ctx, "step-up", "10.1.1.1", ""); !d.Allowed {
			t.Fatalf("attempt %d refused early", i)
		}
	}
	if d := l.Attempt(ctx, "step-up", "10.1.1.1", ""); d.Allowed || d.Reason != "ip_rate" {
		t.Fatalf("rate: %+v", d)
	}
}
