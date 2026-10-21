package store_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// Audit gap 11 / X4: the envelope forced every write to carry an
// Idempotency-Key and nothing read it. Exercised end to end: the real
// middleware over the real store on the migrated schema.
func TestIdempotencyKey_ReplaysRefusesAndScopes(t *testing.T) {
	s, _, _ := newTestStore(t)

	calls := 0
	fail := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"call":%d}`, calls)
	})
	h := handler.IdempotencyMiddleware(s, zap.NewNop())(inner)

	do := func(path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Principal-Id", "p-1")
		req.Header.Set("Idempotency-Key", key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	read := func(rr *httptest.ResponseRecorder) string { b, _ := io.ReadAll(rr.Body); return string(b) }

	first := do("/v1/admin/jurisdictions", "K", `{"a":1}`)
	if first.Code != http.StatusCreated || read(first) != `{"call":1}` {
		t.Fatalf("first request: %d", first.Code)
	}

	retry := do("/v1/admin/jurisdictions", "K", `{"a":1}`)
	if retry.Code != http.StatusCreated || read(retry) != `{"call":1}` || retry.Header().Get("Idempotent-Replayed") != "true" || calls != 1 {
		t.Fatalf("retry must replay the first response without running: code=%d replayed=%q calls=%d", retry.Code, retry.Header().Get("Idempotent-Replayed"), calls)
	}

	reused := do("/v1/admin/jurisdictions", "K", `{"a":2}`)
	if reused.Code != http.StatusConflict || !strings.Contains(read(reused), "idempotency_key_reused") || calls != 1 {
		t.Fatalf("same key, different body: want 409 idempotency_key_reused without running, got %d calls=%d", reused.Code, calls)
	}

	// One key on a different endpoint is a different request (the Postman
	// collection sends one fixed key on all five writes).
	if other := do("/v1/admin/rules/r-1/transition", "K", `{"a":1}`); other.Code != http.StatusCreated || calls != 2 {
		t.Fatalf("same key on another path must run: %d calls=%d", other.Code, calls)
	}

	// Two keys with one body are two requests.
	if second := do("/v1/admin/jurisdictions", "K2", `{"a":1}`); second.Code != http.StatusCreated || calls != 3 {
		t.Fatalf("a new key must run: %d calls=%d", second.Code, calls)
	}

	// A 5xx is not kept: the same key may be retried.
	fail = true
	if rr := do("/v1/admin/jurisdictions", "K3", `{}`); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want the 503 through, got %d", rr.Code)
	}
	fail = false
	if rr := do("/v1/admin/jurisdictions", "K3", `{}`); rr.Code != http.StatusCreated || calls != 5 {
		t.Fatalf("a retry after a 5xx must run again: %d calls=%d", rr.Code, calls)
	}

	// Reads and non-admin paths are untouched.
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/jurisdictions", nil)
	req.Header.Set("X-Principal-Id", "p-1")
	req.Header.Set("Idempotency-Key", "K")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if calls != 6 {
		t.Fatalf("a GET must pass straight through, calls=%d", calls)
	}
}
