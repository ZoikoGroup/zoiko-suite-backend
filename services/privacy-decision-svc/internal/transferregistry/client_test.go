package transferregistry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// privacy-transfer-svc's POST /privacy/transfer-decisions is a mutating route:
// it refuses a call without X-Tenant-Id, X-Principal-Id, X-Correlation-ID and
// Idempotency-Key. The client must send all four, derived from the decision
// request that caused the call.

type capture struct {
	method, path string
	header       http.Header
	body         map[string]any
}

type fakeTransfer struct {
	mu     sync.Mutex
	calls  []capture
	status int
	reply  string
}

func (f *fakeTransfer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		f.mu.Lock()
		f.calls = append(f.calls, capture{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: b})
		f.mu.Unlock()
		st := f.status
		if st == 0 {
			st = http.StatusOK
		}
		w.WriteHeader(st)
		_, _ = w.Write([]byte(f.reply))
	}))
	t.Cleanup(s.Close)
	return s
}

func okReply() string {
	return `{"decision_id":"td-1","result":"AUTHORIZED","reason_codes":[],"transfer_mechanism_id":"m1","relationship_id":"r1"}`
}

func TestEvaluateTransfer_SendsTheFullMutationContract(t *testing.T) {
	f := &fakeTransfer{reply: okReply()}
	c := NewClient(f.serve(t).URL)
	ctx := WithRequestContext(context.Background(), "corr-123", "caller-key-9")

	d, err := c.EvaluateTransfer(ctx, "tenant-7", "principal-3", &EvaluateTransferRequest{
		TenantID: "tenant-7", RelationshipID: "r1", TransferMechanismID: "m1", DestinationJurisdiction: "US", AssessmentRequired: true,
	})
	if err != nil || d == nil || d.DecisionID != "td-1" || d.Result != "AUTHORIZED" {
		t.Fatalf("EvaluateTransfer: %+v %v", d, err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(f.calls))
	}
	got := f.calls[0]
	if got.method != http.MethodPost || got.path != "/privacy/transfer-decisions" {
		t.Errorf("request line = %s %s", got.method, got.path)
	}
	for header, want := range map[string]string{
		"X-Tenant-Id":      "tenant-7",
		"X-Principal-Id":   "principal-3",
		"X-Correlation-Id": "corr-123",
		"Idempotency-Key":  "caller-key-9:transfer",
		"Content-Type":     "application/json",
	} {
		if v := got.header.Get(header); v != want {
			t.Errorf("%s = %q, want %q", header, v, want)
		}
	}
	for field, want := range map[string]any{
		"tenant_id": "tenant-7", "relationship_id": "r1", "transfer_mechanism_id": "m1",
		"destination_jurisdiction": "US", "assessment_required": true,
	} {
		if got.body[field] != want {
			t.Errorf("body.%s = %v, want %v", field, got.body[field], want)
		}
	}
}

// The same decision request (same caller key) always yields the same transfer
// key, so a retry is replayed by privacy-transfer-svc rather than recorded
// twice; a different decision request gets its own.
func TestEvaluateTransfer_IdempotencyKeyIsDeterministicPerDecisionRequest(t *testing.T) {
	f := &fakeTransfer{reply: okReply()}
	c := NewClient(f.serve(t).URL)
	req := &EvaluateTransferRequest{RelationshipID: "r1", TransferMechanismID: "m1"}
	call := func(corr, key string) {
		t.Helper()
		if _, err := c.EvaluateTransfer(WithRequestContext(context.Background(), corr, key), "t", "p", req); err != nil {
			t.Fatal(err)
		}
	}
	call("corr-a", "key-1")
	call("corr-b", "key-1") // a retry: fresh correlation id, same decision key
	call("corr-c", "key-2")
	k := func(i int) string { return f.calls[i].header.Get("Idempotency-Key") }
	if k(0) != k(1) {
		t.Errorf("a retry of the same decision request must reuse the transfer key: %q vs %q", k(0), k(1))
	}
	if k(0) == k(2) {
		t.Errorf("a different decision request must not share the transfer key")
	}
	if k(0) != TransferIdempotencyKey("key-1") || k(0) == "key-1" {
		t.Errorf("transfer key %q must be the derived key, not the caller's own", k(0))
	}
}

// Without the §18.1 context nothing is sent: transfer would refuse it with 400
// and the caller would misread that as an outage.
func TestEvaluateTransfer_MissingRequestContext_NoCallIsMade(t *testing.T) {
	f := &fakeTransfer{reply: okReply()}
	c := NewClient(f.serve(t).URL)
	req := &EvaluateTransferRequest{RelationshipID: "r1", TransferMechanismID: "m1"}
	for name, ctx := range map[string]context.Context{
		"no context at all":  context.Background(),
		"no correlation id":  WithRequestContext(context.Background(), "", "key"),
		"no idempotency key": WithRequestContext(context.Background(), "corr", "  "),
	} {
		d, err := c.EvaluateTransfer(ctx, "t", "p", req)
		if !errors.Is(err, ErrMissingRequestContext) || d != nil {
			t.Errorf("%s: got %+v %v, want ErrMissingRequestContext", name, d, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("no request may reach privacy-transfer-svc without its mandatory context (%d calls)", len(f.calls))
	}
}

// A downstream refusal or failure is never reported as a transfer decision.
func TestEvaluateTransfer_DownstreamFailuresAreErrors(t *testing.T) {
	ctx := WithRequestContext(context.Background(), "corr", "key")
	req := &EvaluateTransferRequest{RelationshipID: "r1", TransferMechanismID: "m1"}
	for name, f := range map[string]*fakeTransfer{
		"400 (a contract violation)":  {status: http.StatusBadRequest, reply: `{"error":"X-Correlation-ID header is required"}`},
		"401":                         {status: http.StatusUnauthorized, reply: `{"error":"x"}`},
		"403 (not authorized)":        {status: http.StatusForbidden, reply: `{"error":"x"}`},
		"503":                         {status: http.StatusServiceUnavailable, reply: `{}`},
		"200 with an unreadable body": {status: http.StatusOK, reply: `not json`},
	} {
		c := NewClient(f.serve(t).URL)
		d, err := c.EvaluateTransfer(ctx, "t", "p", req)
		if !errors.Is(err, ErrUnavailable) || d != nil {
			t.Errorf("%s: got %+v %v, want ErrUnavailable and no decision", name, d, err)
		}
	}
	// Unreachable service.
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	if d, err := NewClient(url).EvaluateTransfer(ctx, "t", "p", req); !errors.Is(err, ErrUnavailable) || d != nil {
		t.Errorf("unreachable: got %+v %v", d, err)
	}
}
