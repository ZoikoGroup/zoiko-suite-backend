package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// QA-T042, Doc 06 §08: a reply carrying fields outside the draft contract
// (a model-invented "actions" or "status") is rejected whole; the empty
// draft fails validation and the turn abstains.
func TestGroqUnknownFieldsAreAnEmptyDraft(t *testing.T) {
	for _, content := range []string{
		`{"text":"Book now.","claim_refs":[],"evidence_refs":["kb-legal-contracts-v1#001"],"actions":[{"type":"OPEN_URL","url":"https://evil.example"}]}`,
		`{"text":"Approved.","claim_refs":[],"evidence_refs":["kb-legal-contracts-v1#001"],"status":"ANSWERED"}`,
		`{"text":"ok","claim_refs":[],"evidence_refs":[]} {"status":"ANSWERED"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { groqReply(t, w, content) }))
		d, err := (&Groq{APIKey: testKey, BaseURL: srv.URL}).Generate(context.Background(), req1)
		srv.Close()
		if err != nil || d.Text != "" || len(d.ClaimRefs) != 0 || len(d.EvidenceRefs) != 0 {
			t.Fatalf("content %s: want empty draft, got %+v %v", content, d, err)
		}
		if d.InputTokens != 900 {
			t.Fatal("token usage must still be recorded for spend accounting")
		}
	}
}

// Doc 06 §12 egress: a redirect is never followed, even to the same host,
// and is a provider error.
func TestGroqRedirectIsNotFollowed(t *testing.T) {
	var followed int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followed, 1)
		groqReply(t, w, `{"text":"ok","claim_refs":[],"evidence_refs":[]}`)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	client := &http.Client{} // a caller-supplied client is not trusted to refuse redirects
	_, err := (&Groq{APIKey: testKey, BaseURL: srv.URL, Client: client}).Generate(context.Background(), req1)
	if !errors.Is(err, ErrProvider) || errors.Is(err, ErrTransient) {
		t.Fatalf("err %v, want a provider error", err)
	}
	if atomic.LoadInt32(&followed) != 0 {
		t.Fatal("the redirect was followed")
	}
	if client.CheckRedirect != nil {
		t.Fatal("the caller's client must not be mutated")
	}
}
