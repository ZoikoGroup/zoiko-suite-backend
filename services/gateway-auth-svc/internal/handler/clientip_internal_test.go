package handler

import (
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/gateway-auth-svc/internal/config"
)

// The leftmost X-Forwarded-For entry is written by the client; only the
// rightmost is appended by the proxy in front of this service. clientIP used
// to take the leftmost, so a caller chose the IP CARTA scored.
func TestClientIP_IgnoresClientWrittenForwardedFor(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, log: zap.NewNop()}
	for _, tc := range []struct {
		name string
		xff  []string
		want string
	}{
		{"spoofed entry prepended", []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
		{"split across header lines", []string{"6.6.6.6", "203.0.113.7"}, "203.0.113.7"},
		{"trailing empty hop", []string{"203.0.113.7, "}, "203.0.113.7"},
		{"single hop", []string{"203.0.113.7"}, "203.0.113.7"},
		{"absent falls back to peer", nil, "192.0.2.1:1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/verify", nil)
			r.RemoteAddr = "192.0.2.1:1234"
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := h.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIP_TrustedProxyCIDRs verifies the trusted proxy configuration.
// The client IP should be the right-most X-Forwarded-For entry NOT in a trusted CIDR.
func TestClientIP_TrustedProxyCIDRs(t *testing.T) {
	cfg := &config.Config{
		TrustedProxyCIDRs: []string{"10.0.0.0/8", "172.16.0.0/12"},
		TrustedProxyDepth: 1,
	}
	h := &Handler{cfg: cfg, log: zap.NewNop()}

	for _, tc := range []struct {
		name       string
		xff        []string
		remoteAddr string
		want       string
	}{
		{
			name:       "untrusted peer, single hop - returns that hop",
			xff:        []string{"203.0.113.7"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "trusted proxy at depth 1, client before it",
			xff:        []string{"203.0.113.7", "10.0.1.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "two trusted proxies, depth 1 - takes first untrusted",
			xff:        []string{"203.0.113.7", "10.0.1.5", "172.16.0.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "all hops trusted - falls back to rightmost",
			xff:        []string{"10.0.1.5", "172.16.0.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "172.16.0.5",
		},
		{
			name:       "IPv6 trusted proxy",
			xff:        []string{"2001:db8::1", "10.0.1.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "2001:db8::1",
		},
		{
			name:       "no XFF, trusted proxy config - falls back to RemoteAddr",
			xff:        nil,
			remoteAddr: "192.0.2.1:1234",
			want:       "192.0.2.1:1234",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/verify", nil)
			r.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := h.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIP_TrustedProxyDepth verifies the trusted proxy depth configuration.
func TestClientIP_TrustedProxyDepth(t *testing.T) {
	// Depth 2 means we skip 2 trusted hops from the right
	cfg := &config.Config{
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		TrustedProxyDepth: 2,
	}
	h := &Handler{cfg: cfg, log: zap.NewNop()}

	for _, tc := range []struct {
		name       string
		xff        []string
		remoteAddr string
		want       string
	}{
		{
			name:       "depth 2: skips two trusted, takes third",
			xff:        []string{"203.0.113.7", "10.0.1.5", "10.0.2.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "depth 2: only one trusted hop, takes first untrusted",
			xff:        []string{"203.0.113.7", "10.0.1.5"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "depth 2: no trusted hops, takes rightmost",
			xff:        []string{"203.0.113.7", "203.0.113.8"},
			remoteAddr: "192.0.2.1:1234",
			want:       "203.0.113.8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/verify", nil)
			r.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := h.clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
