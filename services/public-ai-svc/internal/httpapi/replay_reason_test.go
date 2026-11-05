package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

// fixedAnswerer returns res for every turn.
type fixedAnswerer struct{ res orchestrator.Result }

func (f fixedAnswerer) Answer(context.Context, orchestrator.Input) orchestrator.Result { return f.res }

// revocableConfig is a ConfigSource that also checks citations, like the
// release runtime.
type revocableConfig struct {
	staticConfig
	revoked *bool
}

func (c revocableConfig) CitationsRevoked(context.Context, []string, []string) (bool, error) {
	return *c.revoked, nil
}

func harnessWith(t *testing.T, res orchestrator.Result, cfgSrc ConfigSource) *harness {
	cfg := testConfig()
	cfg.PolicyVersion = "pub-policy-1.0"
	cfg.ModelTimeout = 2 * time.Second
	st := store.NewMemory()
	srv := New(cfg, nil, Deps{Store: st, Sessions: session.New(st, time.Hour), Answerer: fixedAnswerer{res}, Config: cfgSrc, Ready: func() bool { return true }})
	return &harness{t: t, h: srv.Router(), store: st}
}

func decodeEnv(t *testing.T, b []byte) contract.Envelope {
	t.Helper()
	var e contract.Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Doc 03 §19, Doc 05 §12: an idempotent replay re-checks revocation; a
// stored answer whose source or claim was revoked since becomes a safe
// ABSTAIN for the same turn, with no citations.
func TestReplayRechecksRevocation(t *testing.T) {
	res := orchestrator.Result{Status: contract.Answered, IntentID: "I05", Text: "Plans support 3 entities.",
		Citations:          []contract.Citation{{SourceVersionID: "src-v1", ClaimIDs: []string{"c-1"}, URLKey: "pricing"}},
		KnowledgeReleaseID: "kb-1", Reason: contract.ReasonAnswered}
	revoked := false
	h := harnessWith(t, res, revocableConfig{revoked: &revoked})
	s := h.newSession("replay-key-0001")
	first := h.turn(s, turnUUID, "plans?")
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	orig := decodeEnv(t, first.Body.Bytes())

	if again := h.turn(s, turnUUID, "plans?"); again.Body.String() != first.Body.String() {
		t.Fatalf("unrevoked replay must be byte-identical:\n%s\n%s", first.Body.String(), again.Body.String())
	}
	revoked = true
	w := h.turn(s, turnUUID, "plans?")
	e := decodeEnv(t, w.Body.Bytes())
	if w.Code != http.StatusOK || e.Status != contract.Abstain || e.Reason != contract.ReasonSourceRevoked || len(e.Citations) != 0 ||
		e.TurnID != orig.TurnID || e.TurnSequence != orig.TurnSequence || e.Text == orig.Text {
		t.Fatalf("revoked replay: %d %+v", w.Code, e)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("safe replay must be schema-valid: %v", err)
	}

	// No revocation checker wired: fail closed.
	h2 := harnessWith(t, res, staticConfig{})
	s2 := h2.newSession("replay-key-0002")
	h2.turn(s2, turnUUID, "plans?")
	if e := decodeEnv(t, h2.turn(s2, turnUUID, "plans?").Body.Bytes()); e.Status != contract.Abstain {
		t.Fatalf("unchecked replay must abstain, got %s", e.Status)
	}
}

// Doc 05 §10: security and spend reasons never reach the browser.
func TestSecurityReasonsNotRendered(t *testing.T) {
	cases := []struct {
		status   contract.Status
		internal string
		public   string
	}{
		{contract.RefusedAction, contract.ReasonPromptInjection, ""},
		{contract.RefusedAction, contract.ReasonSecurityReport, ""},
		{contract.Abstain, contract.ReasonToolInstruction, contract.ReasonValidationFailed},
		{contract.Unavailable, contract.ReasonSpendCeiling, ""},
		{contract.RefusedAction, contract.ReasonSensitiveInput, contract.ReasonSensitiveInput},
		{contract.Abstain, contract.ReasonNoEvidence, contract.ReasonNoEvidence},
	}
	for i, c := range cases {
		res := orchestrator.Result{Status: c.status, IntentID: "I05", Text: "I can't help with that here.",
			KnowledgeReleaseID: "kb-1", Reason: c.internal}
		h := harnessWith(t, res, staticConfig{})
		s := h.newSession("reason-key-000" + string(rune('a'+i)))
		w := h.turn(s, turnUUID, "x")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.internal, w.Code, w.Body.String())
		}
		if e := decodeEnv(t, w.Body.Bytes()); e.Reason != c.public {
			t.Errorf("%s: browser saw reason %q, want %q", c.internal, e.Reason, c.public)
		}
	}
}
