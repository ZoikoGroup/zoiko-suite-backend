package handler_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/handler"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// CreateWorkflow combines two rules that met in the merge of WFC-03 quorum
// stages: every named approver must be authorized for the legal entity, and
// no principal may approve in more than one stage. Both must see the members
// of a QUORUM pool, which has no single approver_principal_id.

// denyOneAuthz refuses approval authority to exactly one principal.
type denyOneAuthz struct {
	stubAuthz
	denied  string
	checked []string
}

func (a *denyOneAuthz) CheckApprovalAllowed(_ context.Context, principalID, _, _ string) error {
	a.checked = append(a.checked, principalID)
	if principalID == a.denied {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func createWith(t *testing.T, stages string, az *denyOneAuthz) *httptest.ResponseRecorder {
	t.Helper()
	if az == nil {
		az = &denyOneAuthz{}
	}
	store := &stubStore{
		instance: &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: "PENDING"},
		stages:   []*domain.WorkflowStage{{WorkflowStageID: "s-1", StageOrder: 1}, {WorkflowStageID: "s-2", StageOrder: 2}},
	}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(store, &stubPublisher{}, az, &stubDocuments{}, zap.NewNop()))

	body := `{"tenant_id":"t-1","legal_entity_id":"le-1","workflow_type":"PURCHASE_APPROVAL","stages":` + stages + `}`
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(body)), "requester-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateWorkflow_TwoQuorumStagesWithDisjointPools_Accepted(t *testing.T) {
	w := createWith(t, `[
		{"stage_type":"QUORUM","required_approvals":2,"quorum_approvers":["a-1","a-2","a-3"]},
		{"stage_type":"QUORUM","required_approvals":1,"quorum_approvers":["b-1","b-2"]}]`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("two quorum stages with disjoint pools must not be refused as duplicates; got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateWorkflow_PrincipalInTwoStagePools_Refused(t *testing.T) {
	w := createWith(t, `[
		{"approver_principal_id":"a-1"},
		{"stage_type":"QUORUM","required_approvals":1,"quorum_approvers":["b-1","a-1"]}]`, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "duplicate_approver") {
		t.Fatalf("a principal approving in two stages must be refused duplicate_approver; got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateWorkflow_UnauthorizedQuorumMember_Refused(t *testing.T) {
	az := &denyOneAuthz{denied: "b-2"}
	w := createWith(t, `[
		{"approver_principal_id":"a-1"},
		{"stage_type":"QUORUM","required_approvals":1,"quorum_approvers":["b-1","b-2"]}]`, az)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "approver_not_authorized") {
		t.Fatalf("an unauthorized pool member must be refused; got %d: %s", w.Code, w.Body.String())
	}
	if len(az.checked) != 3 {
		t.Fatalf("every named approver (1 single + 2 pool members) must be checked; checked %v", az.checked)
	}
}
