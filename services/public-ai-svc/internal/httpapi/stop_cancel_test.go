package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 05 §12: when the visitor stops (the request is cancelled) the model
// call is cancelled, and the turn is still committed so the next turn is
// accepted rather than refused as busy.
func TestStopCancelsTheModelCall(t *testing.T) {
	cfg := testConfig()
	cfg.PolicyVersion, cfg.ModelTimeout = "p", 5*time.Second
	st := store.NewMemory()
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	ans := answererFunc(func(ctx context.Context, in orchestrator.Input) orchestrator.Result {
		if in.Message == "slow" {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				cancelled <- struct{}{}
			case <-time.After(3 * time.Second):
			}
		}
		return orchestrator.AbstainAll{ReleaseID: "kb"}.Answer(context.Background(), in)
	})
	h := &harness{t: t, h: New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: ans}).Router(), store: st}
	s := h.newSession("key-stopcancel-01")

	reqCtx, stop := context.WithCancel(context.Background())
	id := "3a1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b"
	r := httptest.NewRequest(http.MethodPost, "/public-ai/v1/turns", strings.NewReader(turnBody(s.SessionID, id, "slow"))).WithContext(reqCtx)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(sessionHeader, s.SessionGrant)
	r.Header.Set("Idempotency-Key", id)
	done := make(chan struct{})
	go func() { h.h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()

	<-started
	stop() // the visitor presses Stop
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the model call was not cancelled when the visitor stopped")
	}
	<-done
	if w := h.turn(s, "4a1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", "next question"); w.Code != http.StatusOK {
		t.Fatalf("next turn after Stop: %d %s (slot left locked?)", w.Code, w.Body.String())
	}
}
