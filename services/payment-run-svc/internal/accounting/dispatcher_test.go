package accounting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/store"
)

// These are FAKE-CLIENT tests: an httptest server stands in for
// general-ledger-svc and replays the response shapes read from its handler
// (PostAccountingEvent: 201 new execution, 200 prior execution for a repeated
// source_event_id, 4xx refusals). They prove how the dispatcher interprets the
// documented contract; they are not an integration test against the real ledger.

func fakeLedger(t *testing.T, status int, body string, seen *http.Request, seenBody *[]byte) *Dispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		if seenBody != nil {
			*seenBody, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(nil, srv.URL, "svc-principal", time.Second, 1, zap.NewNop())
}

func req() store.PostingRequest {
	return store.PostingRequest{
		RequestID: "r1", TenantID: "tenant-1", LegalEntityID: "le-1", SourceEventID: "ap11:settle:i1",
		Payload: json.RawMessage(`{"source_event_id":"ap11:settle:i1"}`),
	}
}

func TestPost_SendsTenantPrincipalAndPayload_To_PostingsEvents(t *testing.T) {
	var seen http.Request
	var body []byte
	d := fakeLedger(t, 201, `{"execution_id":"e1","status":"COMMITTED"}`, &seen, &body)
	res := d.post(context.Background(), req())

	if seen.Method != http.MethodPost || seen.URL.Path != "/v1/postings/events" {
		t.Fatalf("expected POST /v1/postings/events, got %s %s", seen.Method, seen.URL.Path)
	}
	if seen.Header.Get("X-Tenant-Id") != "tenant-1" || seen.Header.Get("X-Principal-Id") != "svc-principal" {
		t.Fatalf("tenant and service principal must be forwarded, got %v", seen.Header)
	}
	if string(body) != `{"source_event_id":"ap11:settle:i1"}` {
		t.Fatalf("payload must be sent unchanged, got %s", body)
	}
	if !res.Final || !res.Posted || res.ExecutionID != "e1" {
		t.Fatalf("201 COMMITTED must be POSTED with the execution id, got %+v", res)
	}
}

func TestPost_200ReplayOfCommittedExecution_IsPosted(t *testing.T) {
	d := fakeLedger(t, 200, `{"execution_id":"e1","status":"COMMITTED"}`, nil, nil)
	if res := d.post(context.Background(), req()); !res.Posted || res.ExecutionID != "e1" {
		t.Fatalf("a repeated source_event_id returns the prior COMMITTED execution: %+v", res)
	}
}

// A 200 can be the replay of an execution that failed earlier. That is final
// for the dispatcher (never POSTED, never silently retried forever); an
// operator reprocesses it in the ledger and then requeues here.
func TestPost_200ReplayOfFailedExecution_IsNotPosted(t *testing.T) {
	for _, status := range []string{"FAILED", "QUARANTINED"} {
		d := fakeLedger(t, 200, `{"execution_id":"e1","status":"`+status+`","failure_reason":"store down"}`, nil, nil)
		res := d.post(context.Background(), req())
		if !res.Final || res.Posted || res.Err == "" {
			t.Fatalf("%s replay must be final, not posted, with the reason: %+v", status, res)
		}
	}
}

func TestPost_LedgerRefusals_AreFinalQuarantine(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 412, 422} {
		d := fakeLedger(t, code, `{"error":"posting_rule_ambiguous"}`, nil, nil)
		res := d.post(context.Background(), req())
		if !res.Final || res.Posted || res.Err == "" {
			t.Fatalf("%d is a definitive refusal (a retry cannot change it): %+v", code, res)
		}
	}
}

func TestPost_TransientAnswers_AreRetried(t *testing.T) {
	for _, code := range []int{408, 429, 500, 502, 503} {
		d := fakeLedger(t, code, ``, nil, nil)
		if res := d.post(context.Background(), req()); res.Final || res.Posted {
			t.Fatalf("%d is transient and must be retried, got %+v", code, res)
		}
	}
}

func TestPost_LedgerUnreachable_IsRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening any more
	d := New(nil, url, "svc-principal", time.Second, 1, zap.NewNop())
	if res := d.post(context.Background(), req()); res.Final || res.Posted || res.Err == "" {
		t.Fatalf("an unreachable ledger is transient: %+v", res)
	}
}
