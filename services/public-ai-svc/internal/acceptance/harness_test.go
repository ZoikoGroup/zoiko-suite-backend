// Package acceptance encodes the documents' own acceptance catalogues as
// named tests: Doc 02 G01–G36, Doc 03 KB-001–030, Doc 04 A01–A40 and
// Doc 05 CT-001–052. A case that needs staging, infrastructure or a human is
// still present and says so with t.Skip, so the catalogue is complete and no
// gap is silent.
package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
)

var ctx = context.Background()

const origin = "http://localhost:3000"

type h struct {
	t   *testing.T
	env *fixtureenv.Env
	srv http.Handler
}

func newH(t *testing.T) *h {
	t.Helper()
	env, err := fixtureenv.New(ctx, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AllowedOrigins: []string{origin}, MaxBodyBytes: 16 * 1024, MaxActiveTurnsPerSession: 1,
		PolicyVersion: "pub-policy-1.0", ModelTimeout: time.Second}
	srv := httpapi.New(cfg, nil, httpapi.Deps{Store: env.Store, Sessions: session.New(env.Store, time.Hour), Answerer: env.Pipeline,
		Config: env.Releases, Routes: env.Releases, Ready: func() bool { return env.Releases.Ready(time.Minute) }})
	return &h{t: t, env: env, srv: srv.Router()}
}

// ask runs one turn through the pipeline.
func (x *h) ask(msg string) orchestrator.Result { return x.askIn(msg, "") }

func (x *h) askIn(msg, market string) orchestrator.Result {
	return x.env.Pipeline.Answer(ctx, orchestrator.Input{RequestID: "req_acc", Message: msg, MarketCode: market, Locale: "en-US"})
}

func (x *h) req(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	x.srv.ServeHTTP(w, r)
	return w
}

func (x *h) session(key string) contract.SessionResponse {
	x.t.Helper()
	w := x.req(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0","locale":"en-US"}`, map[string]string{"Idempotency-Key": key})
	var s contract.SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != 201 {
		x.t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	return s
}

func turnBody(sessionID, id, msg string) string {
	b, _ := json.Marshal(map[string]string{"schema_version": "1.0", "session_id": sessionID, "client_turn_id": id, "message": msg})
	return string(b)
}

func (x *h) turn(s contract.SessionResponse, id, msg string) *httptest.ResponseRecorder {
	return x.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, id, msg),
		map[string]string{"X-Public-Chat-Session": s.SessionGrant, "Idempotency-Key": id})
}

func envelope(t *testing.T, w *httptest.ResponseRecorder) contract.Envelope {
	t.Helper()
	var e contract.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("envelope: %v %s", err, w.Body.String())
	}
	return e
}

func status(t *testing.T, r orchestrator.Result, want ...contract.Status) {
	t.Helper()
	for _, s := range want {
		if r.Status == s {
			return
		}
	}
	t.Fatalf("status %s, want one of %v: %q", r.Status, want, r.Text)
}

func lacks(t *testing.T, r orchestrator.Result, words ...string) {
	t.Helper()
	low := strings.ToLower(r.Text)
	for _, w := range words {
		if strings.Contains(low, strings.ToLower(w)) {
			t.Fatalf("text must not contain %q: %q", w, r.Text)
		}
	}
}

func has(t *testing.T, r orchestrator.Result, words ...string) {
	t.Helper()
	low := strings.ToLower(r.Text)
	for _, w := range words {
		if !strings.Contains(low, strings.ToLower(w)) {
			t.Fatalf("text must contain %q: %q", w, r.Text)
		}
	}
}

func cites(r orchestrator.Result, claimID string) bool {
	for _, c := range r.Citations {
		for _, id := range c.ClaimIDs {
			if id == claimID {
				return true
			}
		}
	}
	return false
}

func route(r orchestrator.Result) string {
	if len(r.Actions) == 0 {
		return ""
	}
	return r.Actions[0].RouteKey
}

func fixtures() string { return fixtureenv.FixturesDir() }

// kbDir copies the fixture KB to a temp dir so a test can add or alter sources.
func kbDir(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join(fixtures(), "kb")
	_ = os.MkdirAll(filepath.Join(dst, "sources"), 0o755)
	copyFile(t, filepath.Join(src, "release.json"), filepath.Join(dst, "release.json"))
	entries, _ := os.ReadDir(filepath.Join(src, "sources"))
	for _, e := range entries {
		copyFile(t, filepath.Join(src, "sources", e.Name()), filepath.Join(dst, "sources", e.Name()))
	}
	return dst
}

func copyFile(t *testing.T, from, to string) {
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSource writes a source derived from the pricing fixture with overrides.
func writeSource(t *testing.T, dir, name string, override map[string]any, body string) {
	t.Helper()
	var sf map[string]any
	b, _ := os.ReadFile(filepath.Join(fixtures(), "kb", "sources", "04-pricing.source.json"))
	_ = json.Unmarshal(b, &sf)
	src := sf["source"].(map[string]any)
	src["source_id"], src["source_version_id"], src["url_key"] = "src-"+name, "src-"+name+"-v1", "x-"+name
	src["origin_uri"] = "https://example.invalid/" + name
	for k, v := range override {
		src[k] = v
	}
	sf["body"] = "TEST-ONLY synthetic fixture.\n\n" + body
	out, _ := json.MarshalIndent(sf, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "sources", name+".source.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func manual(t *testing.T, why string) { t.Skip("NOT AUTOMATABLE HERE: " + why) }

func nowUTC() time.Time { return time.Now().UTC() }
