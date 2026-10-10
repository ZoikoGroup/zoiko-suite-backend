package accountingdispatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/accounts-payable-svc/internal/accountingdispatch"
	"zoiko.io/accounts-payable-svc/internal/domain"
)

func post(t *testing.T, status int, body string) accountingdispatch.Outcome {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/postings/events" || r.Header.Get("X-Tenant-Id") != "t1" || r.Header.Get("X-Principal-Id") != "p1" {
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Header)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	o, err := accountingdispatch.NewHTTPClient(srv.URL).Post(context.Background(), "t1", "p1", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Every ACC-04 answer maps to exactly one queue outcome; only transport failures are errors.
func TestHTTPClient_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		jid    string
	}{
		{"committed", 201, `{"execution_id":"e1","status":"COMMITTED","journal_id":"j1"}`, domain.PostingStatusPosted, "j1"},
		{"duplicate returns the prior execution", 200, `{"execution_id":"e1","status":"COMMITTED","journal_id":"j1"}`, domain.PostingStatusPosted, "j1"},
		{"gl recorded a permanent failure", 200, `{"execution_id":"e2","status":"FAILED","failure_reason":"unbalanced"}`, domain.PostingStatusFailed, ""},
		{"quarantined by gl", 200, `{"execution_id":"e3","status":"QUARANTINED"}`, domain.PostingStatusQuarantined, ""},
		{"unknown mapping", 422, `{"error":"no mapping"}`, domain.PostingStatusQuarantined, ""},
		{"invalid request", 400, `{"error":"bad"}`, domain.PostingStatusFailed, ""},
		{"period locked", 412, `{}`, domain.PostingStatusPending, ""},
		{"server error", 503, `{}`, domain.PostingStatusPending, ""},
		{"unrecognised execution status", 200, `{"execution_id":"e4","status":"PROCESSING"}`, domain.PostingStatusPending, ""},
	}
	for _, c := range cases {
		if o := post(t, c.status, c.body); o.Status != c.want || o.JournalID != c.jid {
			t.Fatalf("%s: expected %s/%q, got %+v", c.name, c.want, c.jid, o)
		}
	}
}

func TestHTTPClient_TransportFailureIsAnError_NeverAnOutcome(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := accountingdispatch.NewHTTPClient(url).Post(context.Background(), "t", "p", []byte(`{}`)); err == nil {
		t.Fatal("an unreachable ledger is a retryable error")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv2.Close()
	if _, err := accountingdispatch.NewHTTPClient(srv2.URL).Post(context.Background(), "t", "p", []byte(`{}`)); err == nil {
		t.Fatal("an unreadable success response must not be treated as posted")
	}
}
