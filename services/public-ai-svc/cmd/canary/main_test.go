package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
