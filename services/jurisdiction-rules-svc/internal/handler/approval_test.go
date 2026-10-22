package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/approval"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

type stubApprovals struct {
	err               error
	ref, rule, author string
	calls             int
}

func (s *stubApprovals) Verify(_ context.Context, reference, ruleID, ruleAuthor string) error {
	s.calls++
	s.ref, s.rule, s.author = reference, ruleID, ruleAuthor
	return s.err
}

func routerWithApprovals(st *stubStore, v *stubApprovals) http.Handler {
	r := chi.NewRouter()
	h := handler.New(st, permitAll(), &spyPublisher{}, testAuthzScopeID, nopLogger()).WithApprovalVerifier(v)
	handler.RegisterRoutes(r, h)
	return r
}

func activationStore() *stubStore {
	return &stubStore{
		ruleByID:         &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "DRAFT", CreatedByPrincipalID: "author-1"},
		transitionedRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "ACTIVE"},
		transitionDidRun: true,
	}
}

// J-02: the activation's approval reference is confirmed with workflow-svc,
// for this rule and its author.
func TestActivation_ConfirmsTheApproval(t *testing.T) {
	v := &stubApprovals{}
	rr := postJSONWithApproval(t, routerWithApprovals(activationStore(), v), "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"}, "wf-123")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if v.ref != "wf-123" || v.rule != "r-1" || v.author != "author-1" {
		t.Fatalf("verified %+v", v)
	}
}

func TestActivation_RefusesAnInvalidApproval(t *testing.T) {
	st := activationStore()
	v := &stubApprovals{err: fmt.Errorf("%w: pending", approval.ErrNotValid)}
	rr := postJSONWithApproval(t, routerWithApprovals(st, v), "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"}, "wf-123")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if st.transitionArgs.RuleID != "" {
		t.Fatal("the rule was transitioned on an invalid approval")
	}
}

func TestActivation_FailsClosedWhenWorkflowIsDown(t *testing.T) {
	v := &stubApprovals{err: approval.ErrUnavailable}
	rr := postJSONWithApproval(t, routerWithApprovals(activationStore(), v), "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "ACTIVE"}, "wf-123")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}

// Other transitions cite no approval and ask nothing.
func TestRetirement_AsksNoApproval(t *testing.T) {
	st := activationStore()
	st.transitionedRule = &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RuleStatus: "RETIRED"}
	v := &stubApprovals{}
	rr := postJSON(t, routerWithApprovals(st, v), "/v1/admin/rules/r-1/transition", map[string]any{"new_status": "RETIRED"})
	if rr.Code != http.StatusOK || v.calls != 0 {
		t.Fatalf("code %d, verifier calls %d", rr.Code, v.calls)
	}
}
