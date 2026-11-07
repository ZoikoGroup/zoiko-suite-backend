package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

// Doc 05 §08: a session grant works only from the origin that created it.
func TestGrantBoundToOrigin(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-origin-01")
	w := h.req(http.MethodPost, "/public-ai/v1/turns", turnBody(s.SessionID, turnUUID, "hello"),
		map[string]string{"X-Public-Chat-Session": s.SessionGrant, "Idempotency-Key": turnUUID, "Origin": "https://evil.example"})
	if w.Code == http.StatusOK {
		t.Fatalf("grant accepted from another origin: %d", w.Code)
	}
	if w := h.turn(s, turnUUID, "hello"); w.Code != http.StatusOK {
		t.Fatalf("own origin refused: %d %s", w.Code, w.Body.String())
	}
}

// Doc 06 §13: at 80 % of the session token cap answers are reduced, at the
// cap only static content is served.
func TestSessionTokenCapStages(t *testing.T) {
	cfg := testConfig()
	cfg.PolicyVersion, cfg.ModelTimeout, cfg.SessionTokenCap = "p", time.Second, 1000
	st := store.NewMemory()
	var got []string
	ans := answererFunc(func(ctx context.Context, in orchestrator.Input) orchestrator.Result {
		got = append(got, in.Budget)
		return orchestrator.AbstainAll{ReleaseID: "kb"}.Answer(ctx, in)
	})
	h := &harness{t: t, h: New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: ans}).Router(), store: st}
	s := h.newSession("key-tokencap-01")
	ids := []string{"0a1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", "1a1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b", "2a1c2a52-3c1e-4b5e-9a7f-0c1d2e3f4a5b"}
	var capped []bool
	ans2 := ans
	ans = answererFunc(func(ctx context.Context, in orchestrator.Input) orchestrator.Result {
		capped = append(capped, in.SessionCapped)
		return ans2.Answer(ctx, in)
	})
	h = &harness{t: t, h: New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: ans}).Router(), store: st}
	for i, used := range []int64{0, 850, 1000} {
		sess, _ := st.SessionByGrant(context.Background(), session.HashGrant(s.SessionGrant))
		st.SetTokensUsedForTest(sess.SessionID, used)
		h.turn(s, ids[i], "hello")
	}
	want := []string{"", orchestrator.BudgetReduced, orchestrator.BudgetStatic}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stages %v, want %v", got, want)
		}
	}
	// Only the session's own cap is reported as a session limit.
	if len(capped) != 3 || capped[0] || capped[1] || !capped[2] {
		t.Fatalf("session capped %v, want [false false true]", capped)
	}
}
