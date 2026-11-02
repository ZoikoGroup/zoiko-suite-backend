package idempotency

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

var newMemoryStore = NewMemoryStore

// countingHandler creates a "record" every time it runs and answers with status.
type countingHandler struct {
	runs   int
	status int
}

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.runs++
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.status)
	_, _ = w.Write([]byte(`{"id":"rec-` + string(rune('0'+c.runs)) + `"}`))
}

func send(handler http.Handler, method, key, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/things", strings.NewReader(body))
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", principal)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// Regression: a retried create with the same key used to create a second record.
func TestReplaySameKeyReturnsFirstAnswerWithoutRerunning(t *testing.T) {
	next := &countingHandler{status: http.StatusCreated}
	handler := Middleware(newMemoryStore(), zap.NewNop())(next)

	first := send(handler, http.MethodPost, "key-1", "p1", `{"name":"x"}`)
	second := send(handler, http.MethodPost, "key-1", "p1", `{"name":"x"}`)

	if next.runs != 1 {
		t.Fatalf("handler ran %d times, want 1", next.runs)
	}
	if second.Code != http.StatusCreated || second.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %q, want %d %q", second.Code, second.Body.String(), first.Code, first.Body.String())
	}
	if second.Header().Get("X-Idempotent-Replay") != "true" {
		t.Error("replayed answer must carry X-Idempotent-Replay: true")
	}
}

func TestSameKeyDifferentBodyIsConflict(t *testing.T) {
	next := &countingHandler{status: http.StatusCreated}
	handler := Middleware(newMemoryStore(), zap.NewNop())(next)
	send(handler, http.MethodPost, "key-1", "p1", `{"name":"x"}`)
	w := send(handler, http.MethodPost, "key-1", "p1", `{"name":"DIFFERENT"}`)
	if w.Code != http.StatusConflict || next.runs != 1 {
		t.Fatalf("expected 409 and no second run, got %d and %d runs", w.Code, next.runs)
	}
}

func TestSameKeyOtherPrincipalIsConflict(t *testing.T) {
	next := &countingHandler{status: http.StatusCreated}
	handler := Middleware(newMemoryStore(), zap.NewNop())(next)
	send(handler, http.MethodPost, "key-1", "p1", `{"name":"x"}`)
	w := send(handler, http.MethodPost, "key-1", "p2", `{"name":"x"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("another principal must not receive p1's stored answer; got %d", w.Code)
	}
}

func TestTransientFailureIsNotReplayed(t *testing.T) {
	next := &countingHandler{status: http.StatusServiceUnavailable}
	handler := Middleware(newMemoryStore(), zap.NewNop())(next)
	send(handler, http.MethodPost, "key-1", "p1", `{}`)
	next.status = http.StatusCreated
	w := send(handler, http.MethodPost, "key-1", "p1", `{}`)
	if w.Code != http.StatusCreated || next.runs != 2 {
		t.Fatalf("a retry after a 503 must run again; got %d after %d runs", w.Code, next.runs)
	}
}

func TestReadsAndKeylessWritesPassThrough(t *testing.T) {
	next := &countingHandler{status: http.StatusOK}
	handler := Middleware(newMemoryStore(), zap.NewNop())(next)
	send(handler, http.MethodGet, "key-1", "p1", "")
	send(handler, http.MethodGet, "key-1", "p1", "")
	send(handler, http.MethodPost, "", "p1", `{}`)
	if next.runs != 3 {
		t.Fatalf("reads and keyless writes must not be deduplicated; runs=%d", next.runs)
	}
}
