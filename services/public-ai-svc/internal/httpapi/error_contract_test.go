package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/problem"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

type failBeginStore struct {
	*store.Memory
	err error
}

func (s failBeginStore) BeginTurn(context.Context, store.Turn, time.Time) (store.BeginResult, error) {
	return store.BeginResult{}, s.err
}

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) problem.Problem {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type = %q", ct)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("problem responses must be no-store")
	}
	if rid := w.Header().Get("X-Request-ID"); rid == "" || !strings.HasPrefix(rid, "req_") {
		t.Fatalf("missing server request id: %q", rid)
	}
	var p problem.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, w.Body.String())
	}
	if p.Status != w.Code {
		t.Fatalf("status field = %d, HTTP status = %d", p.Status, w.Code)
	}
	if p.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("request_id = %q, header = %q", p.RequestID, w.Header().Get("X-Request-ID"))
	}
	if p.Instance != "/public-ai/errors/"+p.RequestID {
		t.Fatalf("instance = %q", p.Instance)
	}
	return p
}

func TestBadRequestUsesRFC9457(t *testing.T) {
	h := newHarness(t, time.Hour)
	w := h.req(http.MethodPost, "/public-ai/v1/sessions", `{nope`, map[string]string{"Idempotency-Key": "key-error-400"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	p := decodeProblem(t, w)
	if p.Code != "publicai.invalid_request" || p.Retryable {
		t.Fatalf("problem = %+v", p)
	}
	if p.Detail != "request body is not valid for this schema" || strings.Contains(p.Detail, "unexpected") {
		t.Fatalf("detail leaks decoder internals: %q", p.Detail)
	}
}

func TestServiceUnavailableDoesNotLeakStoreError(t *testing.T) {
	const secret = "dial tcp 10.0.0.5:5432: password authentication failed"
	st := failBeginStore{Memory: store.NewMemory(), err: errors.New(secret)}
	h := &harness{t: t, h: New(testConfig(), nil, Deps{
		Store: st, Sessions: session.New(st, time.Hour), Answerer: &countingAnswerer{}, Config: staticConfig{},
		Ready: func() bool { return true },
	}).Router(), store: st.Memory}
	s := h.newSession("key-error-503")
	w := h.turn(s, turnUUID, "hello")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	p := decodeProblem(t, w)
	if p.Code != "publicai.service_unavailable" || !p.Retryable || p.Detail != "" {
		t.Fatalf("problem = %+v", p)
	}
	if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "10.0.0.5") {
		t.Fatal("internal store detail leaked into the response")
	}
}
