package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// KB-018: real links must answer 2xx and every redirect must stay on the
// allowlist; placeholders are never fetched.
func TestCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.net/landing", http.StatusFound)
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	host := mustHost(t, ts.URL)
	links := []Link{
		{URL: ts.URL + "/ok", Source: "route:ok"},
		{URL: ts.URL + "/gone", Source: "route:gone"},
		{URL: ts.URL + "/hop", Source: "route:hop"},
		{URL: ts.URL + "/away", Source: "route:away"},
		{URL: "https://example.invalid/pricing", Source: "route:placeholder"},
		{URL: "http://" + host + "/ok", Source: "route:plain-http"},
		{URL: "https://other.example.org/x", Source: "route:unlisted"},
	}
	got := map[string]string{}
	for _, r := range Check(context.Background(), links, []string{host}, ts.Client()) {
		got[r.Source] = r.State
	}
	want := map[string]string{"route:ok": "OK", "route:gone": "BROKEN", "route:hop": "OK", "route:away": "OFF_ALLOWLIST",
		"route:placeholder": "PLACEHOLDER", "route:plain-http": "BROKEN", "route:unlisted": "OFF_ALLOWLIST"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
}

// The public KB release parses; today every link is a placeholder.
func TestCollectPublicKB(t *testing.T) {
	links, _, err := Collect("../../data/public-kb")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) < 18 {
		t.Fatalf("only %d links collected", len(links))
	}
}

func mustHost(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}
