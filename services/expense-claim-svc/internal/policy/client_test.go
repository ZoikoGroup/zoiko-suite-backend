package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// policy-svc takes the evaluating actor from the verified X-Principal-Id and
// authorizes it; a body actor is ignored there and was the forged-actor gap of
// the Governance audit. The signed envelope travels with the call so
// policy-svc can check the headers against it.
func TestEvaluate_ForwardsVerifiedIdentityNotABodyActor(t *testing.T) {
	var got *http.Request
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"result":"WITHIN_THRESHOLD","policy_version_id":"v1"}`))
	}))
	defer srv.Close()

	res, _, err := NewHTTPClient(srv.URL, zap.NewNop()).EvaluateApprovalThreshold(context.Background(),
		Caller{PrincipalID: "user-7", Authorization: "Bearer signed", RequestID: "req-1", CorrelationID: "corr-1"},
		"11111111-1111-1111-1111-111111111111", "33333333-3333-3333-3333-333333333333", 120.5)
	if err != nil || res != "WITHIN_THRESHOLD" {
		t.Fatalf("unexpected result %q err=%v", res, err)
	}
	for h, want := range map[string]string{
		"X-Principal-Id": "user-7", "Authorization": "Bearer signed", "X-Request-Id": "req-1",
		"X-Correlation-ID": "corr-1", "X-Tenant-Id": "11111111-1111-1111-1111-111111111111", "X-Source-Channel": "system",
	} {
		if got.Header.Get(h) != want {
			t.Errorf("%s: want %q, got %q", h, want, got.Header.Get(h))
		}
	}
	if _, ok := body["evaluated_by_principal_id"]; ok {
		t.Error("the actor must not travel in the body")
	}
	if body["decision_id"] == "" || body["policy_type"] != "APPROVAL_THRESHOLD" {
		t.Errorf("unexpected body: %v", body)
	}
}
