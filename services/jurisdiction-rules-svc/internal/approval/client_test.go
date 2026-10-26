package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	idenv "zoiko.io/identity-envelope"

	"zoiko.io/jurisdiction-rules-svc/internal/approval"
)

const (
	ref    = "33333333-3333-4333-8333-333333333333"
	ruleID = "rule-1"
)

func wf(status, subjectType, subjectID string, actedBy ...string) map[string]any {
	var stages []map[string]any
	for _, a := range actedBy {
		stages = append(stages, map[string]any{"stage_status": "APPROVED", "acted_by": a})
	}
	return map[string]any{"workflow_instance_id": ref, "workflow_status": status, "subject_type": subjectType,
		"subject_id": subjectID, "initiated_by": "author-1", "stages": stages}
}

func serve(t *testing.T, status int, body any, seen *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workflows/"+ref {
			t.Errorf("path = %s", r.URL.Path)
		}
		if seen != nil {
			*seen = r.Header.Clone()
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func TestVerify_AnApprovalOfThisRuleByAnotherPrincipalPasses(t *testing.T) {
	var seen http.Header
	srv := serve(t, http.StatusOK, wf("APPROVED", approval.SubjectType, ruleID, "reviewer-1"), &seen)
	defer srv.Close()
	ctx := idenv.WithIdentity(context.Background(), &idenv.Identity{Basis: idenv.BasisEnvelope, PrincipalID: "activator-1", Token: "env.jwt"})
	if err := approval.New(srv.URL).Verify(ctx, ref, ruleID, "author-1"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if seen.Get("Authorization") != "Bearer env.jwt" {
		t.Errorf("the caller's envelope was not forwarded: %q", seen.Get("Authorization"))
	}
}

func TestVerify_RefusesWhatIsNotAnApprovalOfThisRule(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   any
	}{
		"pending":         {http.StatusOK, wf("PENDING", approval.SubjectType, ruleID, "reviewer-1")},
		"another rule":    {http.StatusOK, wf("APPROVED", approval.SubjectType, "rule-2", "reviewer-1")},
		"another subject": {http.StatusOK, wf("APPROVED", "invoice", ruleID, "reviewer-1")},
		"author decided":  {http.StatusOK, wf("APPROVED", approval.SubjectType, ruleID, "reviewer-1", "author-1")},
		"no decision":     {http.StatusOK, wf("APPROVED", approval.SubjectType, ruleID)},
		"unknown":         {http.StatusNotFound, map[string]string{"error": "workflow_not_found"}},
		"not readable":    {http.StatusForbidden, map[string]string{"error": "authorization_denied"}},
	} {
		srv := serve(t, tc.status, tc.body, nil)
		err := approval.New(srv.URL).Verify(context.Background(), ref, ruleID, "author-1")
		srv.Close()
		if !errors.Is(err, approval.ErrNotValid) {
			t.Errorf("%s: err = %v, want ErrNotValid", name, err)
		}
	}
	if err := approval.New("http://127.0.0.1:1").Verify(context.Background(), "APPROVED-BY-ME", ruleID, "author-1"); !errors.Is(err, approval.ErrNotValid) {
		t.Errorf("a made-up reference: %v", err)
	}
}

func TestVerify_FailsClosedWhenWorkflowCannotAnswer(t *testing.T) {
	srv := serve(t, http.StatusInternalServerError, map[string]string{"error": "x"}, nil)
	defer srv.Close()
	if err := approval.New(srv.URL).Verify(context.Background(), ref, ruleID, "author-1"); !errors.Is(err, approval.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if err := approval.New("http://127.0.0.1:1").Verify(context.Background(), ref, ruleID, "author-1"); !errors.Is(err, approval.ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}
