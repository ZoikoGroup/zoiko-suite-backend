package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/session"
)

const origin = "http://localhost:3000"

func server(t *testing.T, modelTimeout time.Duration) (*httptest.Server, *fixtureenv.Env) {
	t.Helper()
	env, err := fixtureenv.New(context.Background(), modelTimeout)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: modelTimeout, Environment: "test"}
	srv := httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour), Answerer: env.Pipeline,
		Config: env.Releases, Routes: env.Releases, Ready: func() bool { return true }})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, env
}

// QA-T060: a steady and a burst profile complete with no transport errors,
// no 5xx and governed outcomes only.
func TestLoadProfiles(t *testing.T) {
	ts, _ := server(t, 2*time.Second)
	for _, profile := range []string{"1x", "burst"} {
		rep := Run(context.Background(), Options{URL: ts.URL + "/public-ai/v1", Origin: origin, Rate: 20, Profile: profile,
			Duration: 1500 * time.Millisecond, Concurrency: 4})
		if v := Verdict(rep, 2*time.Second, 0); v != "" {
			t.Fatalf("%s: %s (%+v)", profile, v, rep)
		}
		if rep.Statuses["ANSWERED"] == 0 {
			t.Fatalf("%s: no answers: %+v", profile, rep.Statuses)
		}
	}
}

// QA-T063 fault injection: when the model fails or hangs under load, every
// visitor still gets a governed 200 reply (UNAVAILABLE or a static answer),
// never a 5xx, and a hung model costs at most the model timeout.
func TestModelFaultsUnderLoad(t *testing.T) {
	for _, fault := range []model.Fault{model.FaultError, model.FaultTimeout} {
		ts, env := server(t, 300*time.Millisecond)
		env.Fake.Fault = fault
		rep := Run(context.Background(), Options{URL: ts.URL + "/public-ai/v1", Origin: origin, Rate: 20, Profile: "1x",
			Duration: 1500 * time.Millisecond, Concurrency: 4})
		if v := Verdict(rep, 2*time.Second, 0); v != "" {
			t.Fatalf("%s: %s (%+v)", fault, v, rep)
		}
		for s := range rep.Statuses {
			if s == "ANSWERED" && fault == model.FaultTimeout {
				// Static claim statements need no model; anything else is a leak.
				continue
			}
			if len(s) > 5 && s[:5] == "http_" {
				t.Fatalf("%s: non-200 reply %s (%+v)", fault, s, rep.Statuses)
			}
		}
		if rep.Statuses["UNAVAILABLE"] == 0 && rep.Statuses["ABSTAIN"] == 0 {
			t.Fatalf("%s: the fault never surfaced: %+v", fault, rep.Statuses)
		}
	}
}
