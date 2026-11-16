package policy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
)

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	entity = "33333333-3333-3333-3333-333333333333"
)

func replyStatus(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
}

// P-13: a refused caller is an authorization answer, not an outage.
func TestEvaluate_RefusedCallerIsNotAnOutage(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := replyStatus(status)
		_, _, err := NewHTTPClient(srv.URL, zap.NewNop()).EvaluateApprovalThreshold(context.Background(), Caller{PrincipalID: "u"}, tenant, entity, 1)
		srv.Close()
		if !errors.Is(err, domain.ErrPolicyAccessDenied) {
			t.Errorf("HTTP %d: want ErrPolicyAccessDenied, got %v", status, err)
		}
	}
	srv := replyStatus(http.StatusInternalServerError)
	defer srv.Close()
	if _, _, err := NewHTTPClient(srv.URL, zap.NewNop()).EvaluateApprovalThreshold(context.Background(), Caller{PrincipalID: "u"}, tenant, entity, 1); !errors.Is(err, domain.ErrPolicyServiceUnavailable) {
		t.Errorf("HTTP 500 stays unavailable, got %v", err)
	}
}

// A stable decision id makes a retry one decision in policy-svc's evidence.
func TestEvaluate_SendsTheCallersDecisionID(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"result":"WITHIN_THRESHOLD","policy_version_id":"v1"}`))
	}))
	defer srv.Close()
	if _, _, err := NewHTTPClient(srv.URL, zap.NewNop()).EvaluateApprovalThreshold(context.Background(), Caller{PrincipalID: "u", DecisionID: "biz-42"}, tenant, entity, 1); err != nil {
		t.Fatal(err)
	}
	if body["decision_id"] != "biz-42" {
		t.Fatalf("decision_id = %v", body["decision_id"])
	}
}

func TestEvaluate_NoGoverningThreshold(t *testing.T) {
	srv := replyStatus(http.StatusNotFound)
	defer srv.Close()
	c := NewHTTPClient(srv.URL, zap.NewNop())
	res, _, err := c.EvaluateApprovalThreshold(context.Background(), Caller{PrincipalID: "u"}, tenant, entity, 1)
	if err != nil || res != "APPROVAL_REQUIRED" {
		t.Fatalf("no governing threshold must fail closed to APPROVAL_REQUIRED, got %q %v", res, err)
	}
}
