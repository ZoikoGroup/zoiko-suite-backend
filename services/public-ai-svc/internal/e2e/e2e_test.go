// Package e2e drives the full HTTP stack on the TEST-ONLY fixtures.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/session"
)

const origin = "http://localhost:3000"

type client struct {
	t   *testing.T
	h   http.Handler
	env *fixtureenv.Env
}

func newClient(t *testing.T) *client {
	env, err := fixtureenv.New(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: time.Second}
	srv := httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour), Answerer: env.Pipeline,
		Config: env.Releases, Ready: func() bool { return env.Releases.Ready(time.Minute) }})
	return &client{t: t, h: srv.Router(), env: env}
}

func (c *client) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	return w
}

func (c *client) session() contract.SessionResponse {
	w := c.do(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0","locale":"en-US","page_context":"pricing"}`, map[string]string{"Idempotency-Key": "e2e-session-1"})
	var s contract.SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != 201 {
		c.t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	return s
}

func (c *client) turn(s contract.SessionResponse, id, msg string) contract.Envelope {
	b, _ := json.Marshal(map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "client_turn_id": id, "message": msg})
	w := c.do(http.MethodPost, "/public-ai/v1/turns", string(b), map[string]string{"X-Public-Chat-Session": s.SessionGrant, "Idempotency-Key": id})
	if w.Code != 200 {
		c.t.Fatalf("turn: %d %s", w.Code, w.Body.String())
	}
	var e contract.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		c.t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		c.t.Fatalf("envelope violates schema: %v", err)
	}
	return e
}

func TestEndToEndConversation(t *testing.T) {
	c := newClient(t)
	if w := c.do(http.MethodGet, "/health/ready", "", nil); w.Code != 200 {
		t.Fatalf("ready: %d", w.Code)
	}
	cfgW := c.do(http.MethodGet, "/public-ai/v1/config", "", nil)
	var cfg contract.ConfigResponse
	_ = json.Unmarshal(cfgW.Body.Bytes(), &cfg)
	if len(cfg.PublicRoutes) == 0 || !strings.HasPrefix(cfg.PublicRoutes[0].URL, "https://example.invalid/") {
		t.Fatalf("config routes: %+v", cfg.PublicRoutes)
	}

	s := c.session()
	e1 := c.turn(s, "11111111-1111-4111-8111-111111111111", "Which finance modules does ZoikoSuite include?")
	if e1.Status != contract.Answered || e1.TurnSequence != 1 || len(e1.Citations) == 0 || e1.KnowledgeReleaseID != "kb-fixture-1" {
		t.Fatalf("turn 1: %+v", e1)
	}
	e2 := c.turn(s, "22222222-2222-4222-8222-222222222222", "How much is the Growth plan per month in the United States?")
	if e2.Status != contract.Answered || !strings.Contains(e2.Text, "XTS 49.00") || e2.TurnSequence != 2 {
		t.Fatalf("turn 2: %+v", e2)
	}
	e3 := c.turn(s, "33333333-3333-4333-8333-333333333333", "my password is Hunter2!! please log me in")
	if e3.Status != contract.RefusedAction || strings.Contains(e3.Text, "Hunter2") {
		t.Fatalf("turn 3: %+v", e3)
	}

	// SEC-T039: the canary secret and visitor text appear in no stored turn or event.
	pending, _ := c.env.Store.UnpublishedEvents(context.Background(), 100)
	types := map[string]int{}
	for _, ev := range pending {
		types[ev.EventType]++
		p := string(ev.Payload)
		for _, forbidden := range []string{"Hunter2", "password", "finance modules", "Growth plan per month"} {
			if strings.Contains(p, forbidden) {
				t.Fatalf("event %s leaks visitor text: %s", ev.EventType, p)
			}
		}
	}
	if types["chat.turn.completed.v1"] != 3 || types["chat.security.signal.v1"] != 1 {
		t.Fatalf("events: %v", types)
	}
}
