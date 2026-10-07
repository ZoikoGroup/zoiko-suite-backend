package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sendBody(correlation, purpose, recipient string) map[string]any {
	b := map[string]any{
		"recipient_principal_id": recipient,
		"legal_entity_id":        "le-us",
		"channel":                "IN_APP",
		"subject":                "Payroll",
		"body":                   "Your document is ready",
		"correlation_id":         correlation,
	}
	if purpose != "" {
		b["purpose_context"] = purpose
	}
	return b
}

// §3.4: two DIFFERENT communications raised by one business event share its
// correlation id. Before purpose scoping the second was silently answered with
// the first. With a purpose each is its own communication.
func TestSend_SameCorrelationDifferentPurpose_AreTwoCommunications(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	first := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "payslip", "emp-1"), "p-1")
	second := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "tax_form", "emp-1"), "p-1")
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("want 201, 201; got %d, %d: %s", first.Code, second.Code, second.Body.String())
	}
	if len(s.byID) != 2 {
		t.Fatalf("notifications = %d, want 2", len(s.byID))
	}
}

// The same purpose for a different recipient is also a distinct communication.
func TestSend_SamePurposeDifferentRecipient_AreTwoCommunications(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "payslip", "emp-1"), "p-1")
	rr := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "payslip", "emp-2"), "p-1")
	if rr.Code != http.StatusCreated || len(s.byID) != 2 {
		t.Fatalf("want a second 201 notification, got %d and %d rows", rr.Code, len(s.byID))
	}
}

// A retry of the same purpose-scoped request replays rather than re-sending.
func TestSend_SamePurposeRetried_Replays(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "payslip", "emp-1"), "p-1")
	rr := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-7", "payslip", "emp-1"), "p-1")
	if rr.Code != http.StatusOK || len(s.byID) != 1 || pub.sent != 1 {
		t.Fatalf("replay: want 200, 1 row, 1 event; got %d, %d rows, %d events", rr.Code, len(s.byID), pub.sent)
	}
}

// No purpose: exactly the pre-000012 behaviour — correlation id alone decides.
func TestSend_NoPurpose_DedupesOnCorrelationAsBefore(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	doReq(r, http.MethodPost, "/v1/notifications/", sendBody("corr-legacy", "", "emp-1"), "p-1")
	rr := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("corr-legacy", "", "emp-2"), "p-1")
	if rr.Code != http.StatusOK || len(s.byID) != 1 {
		t.Fatalf("legacy dedup: want 200 replay and 1 row, got %d and %d rows", rr.Code, len(s.byID))
	}
}

// The X-Purpose-Context envelope header scopes the key when the body does not.
func TestSend_PurposeFromEnvelopeHeader(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	for _, purpose := range []string{"payslip", "tax_form"} {
		req := newJSONRequest(t, http.MethodPost, "/v1/notifications/", sendBody("payroll-run-9", "", "emp-1"))
		req.Header.Set("X-Principal-Id", "p-1")
		req.Header.Set("X-Purpose-Context", purpose)
		if rr := serve(r, req); rr.Code != http.StatusCreated {
			t.Fatalf("%s: want 201, got %d: %s", purpose, rr.Code, rr.Body.String())
		}
	}
	if len(s.byID) != 2 {
		t.Fatalf("notifications = %d, want 2", len(s.byID))
	}
}

func newJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}
