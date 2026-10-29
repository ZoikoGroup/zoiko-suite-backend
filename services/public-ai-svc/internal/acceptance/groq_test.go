package acceptance

// The Groq provider wired into the full pipeline (retrieval, eligibility,
// claims, generation, output validation, citations) on the public KB. A
// local fake Groq server stands in for the API: it answers only from the
// evidence records it was sent, like a grounded model would, and records
// every request so tests can check what reached the model. TestGroqLive
// calls the real API and runs only when GROQ_API_KEY is set.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/orchestrator"
)

type fakeGroq struct {
	srv   *httptest.Server
	mu    sync.Mutex
	users []string // user messages received
	mode  string   // "", "invent", "fail", "slow"
}

var (
	recordRe = regexp.MustCompile(`<record id="([^"]+)"[^>]*>([^<]*)</record>`)
	claimRe  = regexp.MustCompile(`<verified_claim id="([^"]+)">([^<]*)</verified_claim>`)
)

func newFakeGroq(t *testing.T) *fakeGroq {
	f := &fakeGroq{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		user := body.Messages[1].Content
		f.mu.Lock()
		f.users = append(f.users, user)
		mode := f.mode
		f.mu.Unlock()
		switch mode {
		case "fail":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "slow":
			time.Sleep(500 * time.Millisecond)
		}
		ans := map[string]any{"text": "I couldn't confirm that detail from the approved ZoikoSuite information currently available to me.", "claim_refs": []string{}, "evidence_refs": []string{}}
		if c := claimRe.FindStringSubmatch(user); c != nil {
			ans["text"], ans["claim_refs"] = c[2], []string{c[1]}
		} else if m := recordRe.FindStringSubmatch(user); m != nil {
			first := strings.SplitAfter(m[2], ".")[0]
			ans["text"], ans["evidence_refs"] = strings.TrimSpace(first), []string{m[1]}
		}
		if mode == "invent" {
			ans["evidence_refs"] = []string{"kb-made-up-v9#001"}
		}
		content, _ := json.Marshal(ans)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1000, "completion_tokens": 50},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGroq) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

func (f *fakeGroq) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[len(f.users)-1]
}

func groqKB(t *testing.T, timeout time.Duration) (*kb, *fakeGroq) {
	t.Helper()
	env, err := fixtureenv.NewPublicKB(ctx, timeout)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeGroq(t)
	env.Pipeline.Model = model.Gateway{Provider: &model.Groq{APIKey: "gsk_test_not_a_real_key", BaseURL: f.srv.URL}, Timeout: timeout}
	return &kb{t: t, env: env}, f
}

func ask(k *kb, q string) orchestrator.Result { return k.ask(q) }

// Test 1: a greeting is a template; the model is not called.
func TestGroq1_GreetingNeedsNoModel(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	r := ask(k, "Hello")
	if r.Reason != contract.ReasonGreeting || f.calls() != 0 {
		t.Fatalf("reason %s, model calls %d", r.Reason, f.calls())
	}
}

// Test 2 and Test 6: retrieval runs, the eligible records reach the model
// with their metadata, and citations name exactly the record it used.
func TestGroq2_ModulesGroundedWithCitations(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	r := ask(k, "What modules are included in ZoikoSuite?")
	if r.Status != contract.Answered || f.calls() != 1 {
		t.Fatalf("status %s %s, calls %d", r.Status, r.Reason, f.calls())
	}
	sent := f.last()
	if !strings.Contains(sent, `id="kb-core-modules-v1#001"`) || !strings.Contains(sent, `approval="DRAFT_PENDING_REVIEW"`) || !strings.Contains(sent, `authority="P3"`) {
		t.Fatalf("evidence or metadata missing from the model request:\n%s", sent)
	}
	if len(r.Citations) != 1 || r.Citations[0].SourceVersionID != "kb-core-modules-v1" {
		t.Fatalf("citations %+v", r.Citations)
	}
	has(t, r, "core modules")
}

// Test 3: a connector without a status claim: capability explained, status Unverified.
func TestGroq3_IntegrationStatusUnverified(t *testing.T) {
	k, _ := groqKB(t, 2*time.Second)
	r := ask(k, "Do you integrate with banking systems?")
	if r.Status != contract.Answered || r.StatusLabel != "Unverified" {
		t.Fatalf("got %s %s %q", r.Status, r.StatusLabel, r.Text)
	}
}

// Test 4: exact prices without a market: one question, no model, no figures.
func TestGroq4_PricingNeverInvented(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	r := ask(k, "What is the exact price of each ZoikoSuite module?")
	if r.Status != contract.Clarify || f.calls() != 0 || regexp.MustCompile(`\d`).MatchString(r.Text) {
		t.Fatalf("got %s %q, calls %d", r.Status, r.Text, f.calls())
	}
}

// Test 5: nothing approved matches: an evidence gap, no model call.
func TestGroq5_MissingKnowledge(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	r := ask(k, "Does ZoikoSuite run a quantum annealing scheduler for payroll?")
	if r.Status == contract.Answered || f.calls() != 0 {
		t.Fatalf("got %s %q, calls %d", r.Status, r.Text, f.calls())
	}
}

// Test 7 (backend half): the visitor's exact question reaches the model.
func TestGroq7_QuestionReachesModel(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	q := "What does Legal and Contracts do for board resolutions?"
	_ = ask(k, q)
	if f.calls() == 0 || !strings.Contains(f.last(), "<visitor_message>"+q+"</visitor_message>") {
		t.Fatal("the exact question did not reach the model")
	}
}

// Test 8: failures are safe and specific.
func TestGroq8_Failures(t *testing.T) {
	k, f := groqKB(t, 2*time.Second)
	f.mode = "fail"
	if r := ask(k, "What is Delegated Authority?"); r.Status != contract.Unavailable || r.Reason != contract.ReasonModelUnavailable {
		t.Fatalf("provider failure: %s %s", r.Status, r.Reason)
	}

	k2, f2 := groqKB(t, 100*time.Millisecond)
	f2.mode = "slow"
	if r := ask(k2, "What is Delegated Authority?"); r.Status != contract.Unavailable || r.Reason != contract.ReasonModelTimeout {
		t.Fatalf("timeout: %s %s", r.Status, r.Reason)
	}

	k3, f3 := groqKB(t, 2*time.Second)
	f3.mode = "invent"
	r := ask(k3, "What is Delegated Authority?")
	if r.Status != contract.Abstain || r.Reason != contract.ReasonValidationFailed || len(r.Citations) != 0 {
		t.Fatalf("invented citation must be rejected: %s %s %+v", r.Status, r.Reason, r.Citations)
	}
	if f3.calls() != 2 {
		t.Fatalf("want one repair attempt, got %d calls", f3.calls())
	}
}

// TestGroqLive calls the real Groq API. It runs only with GROQ_API_KEY set
// (never commit a key) and checks a grounded, cited answer end to end.
func TestGroqLive(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set: live Groq test not run")
	}
	env, err := fixtureenv.NewPublicKB(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m := os.Getenv("GROQ_MODEL")
	env.Pipeline.Model = model.Gateway{Provider: &model.Groq{APIKey: key, Model: m}, Timeout: 30 * time.Second}
	k := &kb{t: t, env: env}
	r := k.ask("What does Legal and Contracts do?")
	t.Logf("status=%s reason=%s citations=%d text=%q", r.Status, r.Reason, len(r.Citations), r.Text)
	if r.Status != contract.Answered || !cited(r, "kb-legal-contracts") {
		t.Fatalf("live answer not grounded: %s %s", r.Status, r.Reason)
	}
}
