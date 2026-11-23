package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 06 negative tests (SEC-T003/T004/T012/T013/T015/T024/T057).

// SEC-T003/T004: a grant with one byte changed is rejected, never treated as
// another session.
func TestByteFlippedGrantRejected(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-flip-0001")
	g := []byte(s.SessionGrant)
	for _, i := range []int{0, len(g) / 2, len(g) - 1} {
		bad := append([]byte(nil), g...)
		if bad[i] == 'A' {
			bad[i] = 'B'
		} else {
			bad[i] = 'A'
		}
		w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, turnUUID, "hi"),
			map[string]string{sessionHeader: string(bad), "Idempotency-Key": turnUUID})
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("flipped byte %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if h.ans.calls.Load() != 0 {
		t.Fatal("a forged grant reached the answerer")
	}
}

// SEC-T012: a request for a Host outside PUBLIC_AI_ALLOWED_HOSTS is refused.
func TestForgedHostRejected(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedHosts = []string{"assistant.zoikosuite.com"}
	st := store.NewMemory()
	srv := New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: &countingAnswerer{}, Config: staticConfig{}, Ready: func() bool { return true }})
	for host, want := range map[string]int{"assistant.zoikosuite.com": http.StatusCreated, "evil.example": http.StatusForbidden, "assistant.zoikosuite.com.evil.example": http.StatusForbidden} {
		r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/sessions", strings.NewReader(`{"schema_version":"1.0"}`))
		r.Host = host
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", "key-host-"+strings.ReplaceAll(host, ".", "-"))
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
}

// SEC-T013: an Authorization header grants nothing: no grant, no session.
func TestAuthorizationHeaderGrantsNothing(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-authz-001")
	w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, turnUUID, "hi"),
		map[string]string{"Authorization": "Bearer staff-token", "Idempotency-Key": turnUUID})
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Fatalf("Authorization without a grant: %d", w.Code)
	}
	if h.ans.calls.Load() != 0 {
		t.Fatal("an Authorization header reached the answerer")
	}
}

// SEC-T015: deeply nested JSON inside the body limit is a 400, never a panic.
func TestDeeplyNestedBodyRejected(t *testing.T) {
	h := newHarness(t, time.Hour)
	nested := `{"schema_version":"1.0","locale":` + strings.Repeat("[", 7000) + strings.Repeat("]", 7000) + `}`
	w := h.req(http.MethodPost, "/public-ai/v1/sessions", nested, map[string]string{"Idempotency-Key": "key-nest-0001"})
	if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("nested body: %d", w.Code)
	}
	huge := `{"schema_version":"1.0","locale":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}`
	if w := h.req(http.MethodPost, "/public-ai/v1/sessions", huge, map[string]string{"Idempotency-Key": "key-nest-0002"}); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the body limit: %d", w.Code)
	}
}

// SEC-T057 over HTTP: a reply that finishes after the session expired is not
// delivered or stored as an answer.
func TestLateReplyAfterExpiryNotDelivered(t *testing.T) {
	h := newHarness(t, 300*time.Millisecond)
	h.ans.release = make(chan struct{})
	s := h.newSession("key-late-0001")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.turn(s, turnUUID, "What is ZoikoSuite?") }()
	time.Sleep(500 * time.Millisecond) // the session expires while the answer runs
	close(h.ans.release)
	w := <-done
	if h.ans.calls.Load() != 1 {
		t.Fatal("the turn never started, so expiry was not exercised")
	}
	if w.Code == http.StatusOK {
		t.Fatalf("a reply after expiry was delivered: %s", w.Body.String())
	}
}
