package handler

import (
	"net/http/httptest"
	"testing"
)

func TestClientHostStripsThePort(t *testing.T) {
	for _, tc := range []struct{ name, remote, xff, want string }{
		{"peer with port", "192.0.2.1:50123", "", "192.0.2.1"},
		{"peer with different port is the same client", "192.0.2.1:50999", "", "192.0.2.1"},
		{"ipv6 peer with port", "[2001:db8::1]:443", "", "2001:db8::1"},
		{"bare ipv6 from the proxy", "192.0.2.1:1", "2001:db8::7", "2001:db8::7"},
		{"proxy-appended ipv4", "192.0.2.1:1", "6.6.6.6, 203.0.113.7", "203.0.113.7"},
		{"bracketed ipv6 without a port", "192.0.2.1:1", "[2001:db8::9]", "2001:db8::9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/verify", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientHost(r); got != tc.want {
				t.Fatalf("clientHost = %q, want %q", got, tc.want)
			}
		})
	}
}
