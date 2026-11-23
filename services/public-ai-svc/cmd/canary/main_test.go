package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/session"
)

const origin = "http://localhost:3000"

func TestCanaryJourneyPasses(t *testing.T) {
	ctx := context.Background()
	env, err := fixtureenv.New(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: time.Second}
	srv := httptest.NewServer(httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour),
		Answerer: env.Pipeline, Config: env.Releases, Routes: env.Releases, Ready: func() bool { return true }}).Router())
	defer srv.Close()

	rep := Run(ctx, srv.Client(), srv.URL, origin)
	for _, s := range rep.Steps {
		if !s.Pass {
			t.Errorf("%s: %s (http %d, status %s)", s.ID, s.Error, s.HTTP, s.Status)
		}
	}
	if !rep.Pass || len(rep.Steps) != 3+len(journeys)+1 {
		t.Fatalf("pass=%v steps=%d", rep.Pass, len(rep.Steps))
	}
}

func TestCanaryFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	rep := Run(context.Background(), srv.Client(), srv.URL, origin)
	if rep.Pass {
		t.Fatal("an unavailable service must fail the canary")
	}
	if last := rep.Steps[len(rep.Steps)-1]; last.ID != "session" || !strings.Contains(last.Error, "503") {
		t.Fatalf("must stop at the failed session create: %+v", last)
	}
}

// The textfile gives the canary alerts a series: success 0/1, the run
// timestamp and per-step latency, with step ids as the only label.
func TestCanaryMetricsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "public_ai_canary.prom")
	rep := Report{Pass: false, Steps: []Step{{ID: "ready", Pass: true, LatencyMS: 12}, {ID: "session", Pass: false, LatencyMS: 1500}}}
	if err := WriteMetrics(path, rep, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, want := range []string{
		"# TYPE public_ai_canary_success gauge\npublic_ai_canary_success 0\n",
		"public_ai_canary_last_run_timestamp_seconds 1700000000\n",
		`public_ai_canary_step_latency_seconds{step="ready"} 0.012`,
		`public_ai_canary_step_latency_seconds{step="session"} 1.5`,
		`public_ai_canary_step_success{step="session"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics file lacks %q:\n%s", want, body)
		}
	}
	rep.Pass = true
	if err := WriteMetrics(path, rep, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "public_ai_canary_success 1\n") {
		t.Fatal("a passing run must write success 1")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}
