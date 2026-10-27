package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

type staticConfig struct{}

func (staticConfig) PublicConfig() contract.ConfigResponse {
	return contract.ConfigResponse{
		AssistantDisplayName: "ZoikoSuite assistant", Capabilities: capabilities(),
		SupportedLocales: []string{"en-US"}, MarketChoices: []string{}, PublicRoutes: []contract.PublicRoute{{RouteKey: "pricing", URL: "https://example.invalid/pricing"}},
		PrivacyNoticeKey: "privacy",
	}
}

// countingAnswerer counts model-path invocations and can block.
type countingAnswerer struct {
	calls   atomic.Int32
	release chan struct{}
}

func (c *countingAnswerer) Answer(ctx context.Context, in orchestrator.Input) orchestrator.Result {
	c.calls.Add(1)
	if c.release != nil {
		<-c.release
	}
	return orchestrator.AbstainAll{ReleaseID: "kb-test"}.Answer(ctx, in)
}

type harness struct {
	t     *testing.T
	h     http.Handler
	store *store.Memory
	ans   *countingAnswerer
}

func newHarness(t *testing.T, ttl time.Duration) *harness {
	cfg := testConfig()
	cfg.PolicyVersion = "pub-policy-1.0"
	cfg.ModelTimeout = 2 * time.Second
	st := store.NewMemory()
	ans := &countingAnswerer{}
	srv := New(cfg, nil, Deps{Store: st, Sessions: session.New(st, ttl), Answerer: ans, Config: staticConfig{}, Ready: func() bool { return true }})
	return &harness{t: t, h: srv.Router(), store: st, ans: ans}
}

func (h *harness) req(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Origin", testOrigin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) newSession(key string) contract.SessionResponse {
	h.t.Helper()
	w := h.req(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0","locale":"en-US"}`, map[string]string{"Idempotency-Key": key})
	if w.Code != http.StatusCreated {
		h.t.Fatalf("create session: %d %s", w.Code, w.Body.String())
	}
	var s contract.SessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

const turnUUID = "6f1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b"

func turnBody(sessionID, clientTurnID, message string) string {
	b, _ := json.Marshal(map[string]string{"schema_version": "1.0", "session_id": sessionID, "client_turn_id": clientTurnID, "message": message})
	return string(b)
}

func (h *harness) turn(s contract.SessionResponse, clientTurnID, msg string) *httptest.ResponseRecorder {
	return h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, clientTurnID, msg),
		map[string]string{sessionHeader: s.SessionGrant, "Idempotency-Key": clientTurnID})
}

// CT-001: POST /sessions issues an anonymous session.
func TestCT001_CreateSession(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000001")
	if !strings.HasPrefix(s.SessionID, "pub_s_") || !strings.HasPrefix(s.SessionGrant, "pgr_") || s.PolicyVersion == "" {
		t.Fatalf("bad session: %+v", s)
	}
	if s.Capabilities.Streaming || s.Capabilities.LeadCapture || s.Capabilities.FileUpload {
		t.Fatal("R0 capabilities must all be false")
	}
	if _, err := time.Parse(time.RFC3339, s.ExpiresAt); err != nil {
		t.Fatalf("expires_at not RFC 3339: %v", err)
	}
}

func TestCreateSessionRequiresIdempotencyKey(t *testing.T) {
	h := newHarness(t, time.Hour)
	w := h.req(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0"}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d", w.Code)
	}
}

// Idempotent replay returns the same session (with a rotated grant).
func TestCT002_SessionReplay(t *testing.T) {
	h := newHarness(t, time.Hour)
	a := h.newSession("key-00000002")
	b := h.newSession("key-00000002")
	if a.SessionID != b.SessionID || a.ExpiresAt != b.ExpiresAt {
		t.Fatalf("replay must return the same session: %+v %+v", a, b)
	}
	if a.SessionGrant == b.SessionGrant {
		t.Fatal("replay must rotate the grant")
	}
	// The old grant no longer works.
	if w := h.turn(a, turnUUID, "hello"); w.Code != http.StatusUnauthorized {
		t.Fatalf("old grant: %d", w.Code)
	}
	// Same key, different body -> 422.
	w := h.req(http.MethodPost, "/public-ai/v1/sessions", `{"schema_version":"1.0","locale":"en-GB"}`, map[string]string{"Idempotency-Key": "key-00000002"})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "idempotency_mismatch") {
		t.Fatalf("mismatch: %d %s", w.Code, w.Body.String())
	}
}

func TestSessionValidation(t *testing.T) {
	h := newHarness(t, time.Hour)
	cases := map[string]struct {
		body string
		code int
	}{
		"unknown field":     {`{"schema_version":"1.0","tenant_id":"x"}`, 400},
		"bad schema":        {`{"schema_version":"2.0"}`, 400},
		"bad market":        {`{"schema_version":"1.0","market_code":"usa"}`, 422},
		"raw url as page":   {`{"schema_version":"1.0","page_context":"https://x/y"}`, 422},
		"not json":          {`{nope`, 400},
		"two objects":       {`{"schema_version":"1.0"}{}`, 400},
		"role elevation":    {`{"schema_version":"1.0","role":"admin"}`, 400},
		"authenticated set": {`{"schema_version":"1.0","authenticated":true}`, 400},
	}
	i := 0
	for name, c := range cases {
		i++
		w := h.req(http.MethodPost, "/public-ai/v1/sessions", c.body, map[string]string{"Idempotency-Key": "key-val-" + strings.Repeat("x", i)})
		if w.Code != c.code {
			t.Errorf("%s: got %d want %d (%s)", name, w.Code, c.code, w.Body.String())
		}
	}
}

func TestDeleteSession(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000003")
	other := h.newSession("key-00000004")
	path := "/public-ai/v1/sessions/" + s.SessionID

	// Another session's grant cannot delete it (no existence leak).
	if w := h.req(http.MethodDelete, path, "", map[string]string{sessionHeader: other.SessionGrant}); w.Code != http.StatusNotFound {
		t.Fatalf("foreign delete: %d", w.Code)
	}
	if w := h.req(http.MethodDelete, path, "", map[string]string{sessionHeader: s.SessionGrant}); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	// Repeat delete of own session is 204 (Doc 05 §07).
	if w := h.req(http.MethodDelete, path, "", map[string]string{sessionHeader: s.SessionGrant}); w.Code != http.StatusNoContent {
		t.Fatalf("repeat delete: %d", w.Code)
	}
	// The deleted session cannot start turns.
	if w := h.turn(s, turnUUID, "hello"); w.Code != http.StatusUnauthorized {
		t.Fatalf("turn after delete: %d", w.Code)
	}
	if w := h.req(http.MethodDelete, path, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("delete without grant: %d", w.Code)
	}
}

func TestExpiredSession(t *testing.T) {
	h := newHarness(t, time.Millisecond)
	s := h.newSession("key-00000005")
	time.Sleep(5 * time.Millisecond)
	w := h.turn(s, turnUUID, "hello")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "session_expired") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestConfigETag(t *testing.T) {
	h := newHarness(t, time.Hour)
	w := h.req(http.MethodGet, "/public-ai/v1/config", "", nil)
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || etag == "" {
		t.Fatalf("config: %d etag=%q", w.Code, etag)
	}
	var c contract.ConfigResponse
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.ConfigRevision == "" || c.Capabilities.LeadCapture {
		t.Fatalf("config body: %+v", c)
	}
	if w := h.req(http.MethodGet, "/public-ai/v1/config", "", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified {
		t.Fatalf("conditional get: %d", w.Code)
	}
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) contract.Envelope {
	t.Helper()
	var e contract.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, w.Body.String())
	}
	return e
}

func TestTurnHappyPathEnvelope(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000006")
	w := h.turn(s, turnUUID, "What does ZoikoSuite do?")
	if w.Code != http.StatusOK {
		t.Fatalf("turn: %d %s", w.Code, w.Body.String())
	}
	e := decodeEnvelope(t, w)
	if e.TurnSequence != 1 || e.Status != contract.Abstain || e.KnowledgeReleaseID != "kb-test" || e.PolicyVersion != "pub-policy-1.0" || !strings.HasPrefix(e.TurnID, "pub_t_") {
		t.Fatalf("envelope: %+v", e)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("turns must be no-store")
	}
	w2 := h.turn(s, "7f1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", "and pricing?")
	if e2 := decodeEnvelope(t, w2); e2.TurnSequence != 2 {
		t.Fatalf("sequence: %d", e2.TurnSequence)
	}
}

// CT-009/CT-010: replay of the same key + payload returns the identical
// response without a second model call.
func TestCT009_TurnReplayNoSecondCall(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000007")
	a := h.turn(s, turnUUID, "hello")
	b := h.turn(s, turnUUID, "hello")
	if a.Body.String() != b.Body.String() {
		t.Fatalf("replay differs:\n%s\n%s", a.Body.String(), b.Body.String())
	}
	if n := h.ans.calls.Load(); n != 1 {
		t.Fatalf("model path called %d times, want 1", n)
	}
}

// Same key, different payload -> 422 idempotency_mismatch.
func TestIdempotencyMismatch(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000008")
	h.turn(s, turnUUID, "hello")
	w := h.turn(s, turnUUID, "something else")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "publicai.idempotency_mismatch") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestIdempotencyHeaderMismatch(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000009")
	w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, turnUUID, "hi"),
		map[string]string{sessionHeader: s.SessionGrant, "Idempotency-Key": "8f1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "idempotency_header_mismatch") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// CT-011: a second turn while one is running gets 409 turn_conflict.
func TestCT011_TurnConflict(t *testing.T) {
	h := newHarness(t, time.Hour)
	h.ans.release = make(chan struct{})
	s := h.newSession("key-00000010")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); h.turn(s, turnUUID, "first") }()
	for h.ans.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	w := h.turn(s, "9f1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", "second")
	close(h.ans.release)
	wg.Wait()
	if w.Code != http.StatusConflict || w.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d (Retry-After=%q)", w.Code, w.Header().Get("Retry-After"))
	}
}

// The body's session_id must equal the grant subject.
func TestTurnSessionMustMatchGrant(t *testing.T) {
	h := newHarness(t, time.Hour)
	a := h.newSession("key-00000011")
	b := h.newSession("key-00000012")
	w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(b.SessionID, turnUUID, "hi"),
		map[string]string{sessionHeader: a.SessionGrant, "Idempotency-Key": turnUUID})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "scope_denied") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestTurnRejectsExtraFieldsAndBadValues(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000013")
	extra := `{"schema_version":"1.0","session_id":"` + s.SessionID + `","client_turn_id":"` + turnUUID + `","message":"hi","tenant_id":"t1"}`
	w := h.req(http.MethodPost, "/public-ai/v1/turns", extra, map[string]string{sessionHeader: s.SessionGrant, "Idempotency-Key": turnUUID})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("extra field: %d", w.Code)
	}
	long := turnBody(s.SessionID, turnUUID, strings.Repeat("a", 2001))
	w = h.req(http.MethodPost, "/public-ai/v1/turns", long, map[string]string{sessionHeader: s.SessionGrant, "Idempotency-Key": turnUUID})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("long message: %d", w.Code)
	}
	if h.ans.calls.Load() != 0 {
		t.Fatal("invalid requests must not reach the orchestrator")
	}
}

// A misbehaving orchestrator result is never sent: it becomes UNAVAILABLE.
func TestInvalidOrchestratorOutputIsContained(t *testing.T) {
	cfg := testConfig()
	cfg.PolicyVersion, cfg.ModelTimeout = "p", time.Second
	st := store.NewMemory()
	bad := answererFunc(func(context.Context, orchestrator.Input) orchestrator.Result {
		return orchestrator.Result{Status: contract.Answered, IntentID: "I05", Text: "Price is 5", KnowledgeReleaseID: "kb"} // no provenance
	})
	h := &harness{t: t, h: New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: bad}).Router(), store: st}
	s := h.newSession("key-00000014")
	e := decodeEnvelope(t, h.turn(s, turnUUID, "price?"))
	if e.Status != contract.Unavailable || strings.Contains(e.Text, "5") {
		t.Fatalf("unsafe output escaped: %+v", e)
	}
}

type answererFunc func(context.Context, orchestrator.Input) orchestrator.Result

func (f answererFunc) Answer(ctx context.Context, in orchestrator.Input) orchestrator.Result {
	return f(ctx, in)
}
