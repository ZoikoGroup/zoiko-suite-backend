package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "gsk_test_not_a_real_key"

func groqReply(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 60},
	})
}

var req1 = Request{
	PolicyVersion: "pub-policy-1.0", IntentID: "I19", Message: "What does Legal and Contracts do?", MaxOutputTokens: 400,
	Evidence: []Evidence{{Ref: "kb-legal-contracts-v1#001", Title: "Legal and Contracts", Text: "Legal and Contracts governs commercial agreements.",
		SourceID: "kb-legal-contracts", Authority: "P3", Approval: "APPROVED", ValidUntil: "2027-12-31T00:00:00Z", Markets: "ALL"}},
}

func TestGroqRequestShapeAndParse(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("missing bearer key")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		groqReply(t, w, `{"text":"Legal and Contracts governs commercial agreements.","claim_refs":[],"evidence_refs":["kb-legal-contracts-v1#001"]}`)
	}))
	defer srv.Close()

	g := &Groq{APIKey: testKey, BaseURL: srv.URL}
	d, err := g.Generate(context.Background(), req1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Text != "Legal and Contracts governs commercial agreements." || len(d.EvidenceRefs) != 1 || d.InputTokens != 900 || d.OutputTokens != 60 {
		t.Fatalf("draft %+v", d)
	}
	if got["model"] != "openai/gpt-oss-120b" || got["reasoning_effort"] != "low" || got["include_reasoning"] != false || got["stream"] != false {
		t.Fatalf("request %v", got)
	}
	if _, has := got["tools"]; has {
		t.Fatal("tools must never be sent")
	}
	if _, has := got["max_tokens"]; has {
		t.Fatal("max_tokens is deprecated; use max_completion_tokens")
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["strict"] != true {
		t.Fatalf("response_format %v", rf)
	}
	msgs := got["messages"].([]any)
	system := msgs[0].(map[string]any)["content"].(string)
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(system, SystemPrompt) || !strings.Contains(system, PromptVersion) || !strings.Contains(system, "answer the visitor's exact question in the first sentence") {
		t.Fatal("system message must start with the pinned Suivo system prompt")
	}
	for _, want := range []string{`id="kb-legal-contracts-v1#001"`, `source_id="kb-legal-contracts"`, `authority="P3"`, `approval="APPROVED"`, `valid_until="2027-12-31T00:00:00Z"`, "<visitor_message>What does Legal and Contracts do?</visitor_message>"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user message lacks %s", want)
		}
	}
	if strings.Contains(system+user, testKey) {
		t.Fatal("the key must never appear in the prompt")
	}
}

func TestGroqTransientErrorsRetryOnceThroughGateway(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		groqReply(t, w, `{"text":"ok","claim_refs":[],"evidence_refs":["kb-legal-contracts-v1#001"]}`)
	}))
	defer srv.Close()
	gw := Gateway{Provider: &Groq{APIKey: testKey, BaseURL: srv.URL}, Timeout: 2 * time.Second}
	if _, err := gw.Generate(context.Background(), req1); err != nil || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

func TestGroqErrorsNeverLeakKeyOrBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key ` + testKey + `"}}`))
	}))
	defer srv.Close()
	_, err := (&Groq{APIKey: testKey, BaseURL: srv.URL}).Generate(context.Background(), req1)
	if !errors.Is(err, ErrProvider) || errors.Is(err, ErrTransient) || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "Invalid") {
		t.Fatalf("err %v", err)
	}
	if _, err := (&Groq{}).Generate(context.Background(), req1); !errors.Is(err, ErrProvider) {
		t.Fatal("a missing key must fail closed")
	}
}

func TestGroqOffContractReplyIsAnEmptyDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groqReply(t, w, "Sure! Here is some prose instead of JSON.")
	}))
	defer srv.Close()
	d, err := (&Groq{APIKey: testKey, BaseURL: srv.URL}).Generate(context.Background(), req1)
	if err != nil || d.Text != "" {
		t.Fatalf("want empty draft for validation to reject, got %+v %v", d, err)
	}
}

func TestGroqDeadlineIsATimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		groqReply(t, w, `{}`)
	}))
	defer srv.Close()
	gw := Gateway{Provider: &Groq{APIKey: testKey, BaseURL: srv.URL}, Timeout: 50 * time.Millisecond}
	if _, err := gw.Generate(context.Background(), req1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}
