package handler

import (
	"net/http/httptest"
	"testing"
)

// The leftmost X-Forwarded-For entry is written by the client; only the
// rightmost is appended by the proxy in front of this service. clientIP used
// to take the leftmost, so a caller chose the IP CARTA scored.
func TestClientIP_IgnoresClientWrittenForwardedFor(t *testing.T) {
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
			if got := clientIP(r); got != tc.want {
				t.Fatalf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
