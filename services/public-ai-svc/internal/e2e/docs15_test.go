package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/middleware"
	"zoiko.io/public-ai-svc/internal/session"
)

func newFullClient(t *testing.T) *client {
	env, err := fixtureenv.New(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: time.Second}
	srv := httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour), Answerer: env.Pipeline,
		Config: env.Releases, Routes: env.Releases, Ready: func() bool { return true }})
	return &client{t: t, h: srv.Router(), env: env}
}

func (c *client) post(path string, s contract.SessionResponse, body any) (int, string) {
	b, _ := json.Marshal(body)
	w := c.do(http.MethodPost, path, string(b), map[string]string{"X-Public-Chat-Session": s.SessionGrant})
	return w.Code, w.Body.String()
}

// Doc 01 FR-09 / S09: "useful / not useful" on the caller's own answers only.
func TestFeedbackOnOwnTurn(t *testing.T) {
	c := newFullClient(t)
	s := c.session()
	e := c.turn(s, "44444444-4444-4444-8444-444444444444", "What modules does ZoikoSuite include?")
	path := "/public-ai/v1/turns/" + e.TurnID + "/feedback"

	if code, body := c.post(path, s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "rating": "not_useful", "reason": "incomplete"}); code != 204 {
		t.Fatalf("feedback: %d %s", code, body)
	}
	if code, _ := c.post(path, s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "rating": "great"}); code != 422 {
		t.Fatalf("bad rating: %d", code)
	}
	if code, _ := c.post(path, s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "rating": "useful", "comment": "free text"}); code != 400 {
		t.Fatalf("free text must be rejected: %d", code)
	}
	if code, _ := c.post("/public-ai/v1/turns/pub_t_unknown/feedback", s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "rating": "useful"}); code != 404 {
		t.Fatalf("unknown turn: %d", code)
	}
	other := c.do(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0"}`, map[string]string{"Idempotency-Key": "e2e-session-other"})
	var o contract.SessionResponse
	_ = json.Unmarshal(other.Body.Bytes(), &o)
	if code, _ := c.post(path, o, map[string]string{"schema_version": "1.0", "session_id": o.SessionID, "rating": "useful"}); code != 404 {
		t.Fatalf("another session rated this turn: %d", code)
	}
}

// Doc 05 §21: opening an official route emits chat.handoff.clicked.v1 and
// returns only an approved URL; unknown routes are refused.
func TestHandoffClickEmitsEvent(t *testing.T) {
	c := newFullClient(t)
	s := c.session()
	code, body := c.post("/public-ai/v1/handoffs", s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "route_key": "demo"})
	if code != 200 || !strings.Contains(body, `"url":"https://example.invalid/book-demo"`) {
		t.Fatalf("handoff: %d %s", code, body)
	}
	if code, _ := c.post("/public-ai/v1/handoffs", s, map[string]string{"schema_version": "1.0", "session_id": s.SessionID, "route_key": "evil"}); code != 404 {
		t.Fatalf("unapproved route must be refused: %d", code)
	}
	pending, _ := c.env.Store.UnpublishedEvents(context.Background(), 100)
	n := 0
	for _, ev := range pending {
		if ev.EventType == "chat.handoff.clicked.v1" {
			n++
			if strings.Contains(string(ev.Payload), "@") || !strings.Contains(string(ev.Payload), `"route_key":"demo"`) {
				t.Fatalf("handoff payload: %s", ev.Payload)
			}
		}
	}
	if n != 1 {
		t.Fatalf("handoff events = %d", n)
	}
}

// Doc 05 §06: traceparent is used only when well formed.
func TestTraceparentValidation(t *testing.T) {
	cases := map[string]string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01": "4bf92f3577b34da6a3ce929d0e0e4736",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01": "",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01": "",
		"garbage": "",
		"":        "",
	}
	for in, want := range cases {
		if got := middleware.TraceID(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// Doc 03 §13: signed synonym lexicon ("AR" → accounts receivable).
func TestSynonymLexicon(t *testing.T) {
	c := newFullClient(t)
	s := c.session()
	e := c.turn(s, "55555555-5555-4555-8555-555555555555", "Do you have AR and AP modules?")
	if e.Status != contract.Answered || !strings.Contains(strings.ToLower(e.Text), "accounts receivable") {
		t.Fatalf("synonyms not applied: %+v", e)
	}
}
