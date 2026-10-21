package handler_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/workflow-svc/internal/domain"
)

// Governance audit 6/7, gap #1: "Any principal holding a tenant header can
// cancel or escalate any workflow in that tenant." Cancel and escalate were
// later gated on WORKFLOW_APPROVE, which still let every approver in the
// legal entity act on every workflow in it, and invalidate had no gate at all.
// These tests pin the permission each route now asks authorization-svc for.

func pendingInstance() *domain.WorkflowInstance {
	return &domain.WorkflowInstance{
		WorkflowInstanceID: testWorkflowID,
		TenantID:           "11111111-1111-4111-8111-111111111111",
		LegalEntityID:      "22222222-2222-4222-8222-222222222222",
		WorkflowStatus:     "PENDING",
		CurrentStage:       1,
		InitiatedBy:        "requester-1",
	}
}

func lifecycleRequest(route, principal string) *http.Request {
	var body *bytes.Buffer
	if route == "invalidate" {
		body = bytes.NewBufferString(`{"reason_code":"CONTROL_FAILURE"}`)
	} else {
		body = bytes.NewBufferString("")
	}
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflows/"+testWorkflowID+"/"+route, body), principal)
	if route == "invalidate" {
		req.Header.Set("X-Expected-Version", "1")
	}
	return req
}

func TestLifecycleAuthz_PermissionAskedFor(t *testing.T) {
	cases := []struct {
		name      string
		route     string
		principal string
		onStage   bool
		want      string
	}{
		{"initiator cancels own workflow", "cancel", "requester-1", false, "WORKFLOW_CANCEL"},
		{"current approver cannot cancel as a participant", "cancel", "approver-1", true, "WORKFLOW_ADMINISTER"},
		{"stranger cancels someone else's workflow", "cancel", "stranger-1", false, "WORKFLOW_ADMINISTER"},
		{"initiator escalates", "escalate", "requester-1", false, "WORKFLOW_ESCALATE"},
		{"current-stage approver escalates", "escalate", "approver-1", true, "WORKFLOW_ESCALATE"},
		{"stranger escalates someone else's workflow", "escalate", "stranger-1", false, "WORKFLOW_ADMINISTER"},
		{"invalidate has its own permission", "invalidate", "svc-ap-1", false, "WORKFLOW_INVALIDATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{
				findInstance:           pendingInstance(),
				currentStageApprover:   tc.onStage,
				cancelInstance:         &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: "CANCELLED"},
				escalateInstance:       &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: "ESCALATED"},
				invalidateInstance:     &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: domain.WorkflowStatusInvalidated},
				cancelTransitioned:     true,
				escalateTransitioned:   true,
				invalidateTransitioned: true,
			}
			authz := &stubAuthz{}
			r := newTestRouterFull(store, &stubPublisher{}, authz)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, lifecycleRequest(tc.route, tc.principal))

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 when authorization-svc grants, got %d: %s", w.Code, w.Body.String())
			}
			if len(authz.asked) != 1 || authz.asked[0] != tc.want {
				t.Fatalf("expected one check for %s, got %v", tc.want, authz.asked)
			}
			if authz.owners[0] != "requester-1" {
				t.Errorf("expected the initiator as resource owner, got %q", authz.owners[0])
			}
		})
	}
}

// The audit scenario itself: a tenant principal who is not on the workflow
// (and whose WORKFLOW_APPROVE grant used to be enough) is refused, and the
// store transition is never reached.
func TestLifecycleAuthz_NonParticipantDenied(t *testing.T) {
	for _, route := range []string{"cancel", "escalate", "invalidate"} {
		t.Run(route, func(t *testing.T) {
			store := &stubStore{
				findInstance: pendingInstance(),
				// Any of these surfacing means the transition ran.
				cancelErr:     errors.New("cancel reached"),
				escalateErr:   errors.New("escalate reached"),
				invalidateErr: errors.New("invalidate reached"),
			}
			authz := &stubAuthz{err: domain.ErrAuthorizationDenied}
			r := newTestRouterFull(store, &stubPublisher{}, authz)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, lifecycleRequest(route, "stranger-1"))

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
			var got map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got["error"] != "authorization_denied" {
				t.Errorf("expected error authorization_denied, got %v", got["error"])
			}
			want := "WORKFLOW_ADMINISTER"
			if route == "invalidate" {
				want = "WORKFLOW_INVALIDATE"
			}
			if got["action_type"] != want {
				t.Errorf("expected action_type %s in the denial, got %v", want, got["action_type"])
			}
		})
	}
}

func TestLifecycleAuthz_AuthorizationUnavailable_FailsClosed(t *testing.T) {
	for _, route := range []string{"cancel", "escalate", "invalidate"} {
		t.Run(route, func(t *testing.T) {
			store := &stubStore{
				findInstance:  pendingInstance(),
				cancelErr:     errors.New("cancel reached"),
				escalateErr:   errors.New("escalate reached"),
				invalidateErr: errors.New("invalidate reached"),
			}
			authz := &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}
			r := newTestRouterFull(store, &stubPublisher{}, authz)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, lifecycleRequest(route, "requester-1"))

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
			}
			var got map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			if got["error"] != "authorization_service_unavailable" {
				t.Errorf("expected error authorization_service_unavailable, got %v", got["error"])
			}
		})
	}
}

// Escalate cannot decide which permission applies without knowing whether the
// caller is on the current stage, so a failed lookup refuses rather than
// guessing either way.
func TestEscalateWorkflow_StageLookupFails_NoAuthzNoTransition(t *testing.T) {
	store := &stubStore{
		findInstance:            pendingInstance(),
		currentStageApproverErr: domain.ErrStoreUnavailable,
		escalateErr:             errors.New("escalate reached"),
	}
	authz := &stubAuthz{}
	r := newTestRouterFull(store, &stubPublisher{}, authz)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, lifecycleRequest("escalate", "approver-1"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if len(authz.asked) != 0 {
		t.Errorf("expected no authorization call, got %v", authz.asked)
	}
}

// A workflow in another tenant is invisible to FindWorkflowByID, so invalidate
// answers 404 before asking authorization-svc anything. Before, a lookup error
// was ignored and the store transition ran anyway.
func TestInvalidateWorkflow_NotFound_NoAuthzNoTransition(t *testing.T) {
	store := &stubStore{
		findErr:       domain.ErrWorkflowNotFound,
		invalidateErr: errors.New("invalidate reached"),
	}
	authz := &stubAuthz{}
	r := newTestRouterFull(store, &stubPublisher{}, authz)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, lifecycleRequest("invalidate", "svc-ap-1"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if len(authz.asked) != 0 {
		t.Errorf("expected no authorization call, got %v", authz.asked)
	}
}

// A workflow definition belongs to no single legal entity, and
// authorization-svc refuses an empty legal_entity_id with a 400 — a reply
// this service can only read as the authorization plane being down. The
// PLATFORM sentinel is what authorization-svc accepts for a platform-scope
// act and resolves to its configured platform entity; sending "" made every
// definition route fail closed with a 503 against a live authorization-svc.
func TestLifecycleAuthz_DefinitionOpsAskPlatformScope(t *testing.T) {
	store := &stubStore{
		workflowDefinition: &domain.WorkflowDefinition{TenantID: "11111111-1111-4111-8111-111111111111", WorkflowType: "PURCHASE_APPROVAL", Version: 1},
	}
	authz := &stubAuthz{}
	r := newTestRouterFull(store, &stubPublisher{}, authz)

	body := `{"tenant_id":"11111111-1111-4111-8111-111111111111","workflow_type":"PURCHASE_APPROVAL","version":1,"stages_json":"W10="}`
	req := scopedAs(httptest.NewRequest(http.MethodPost, "/v1/workflow-definitions/", bytes.NewBufferString(body)), "creator-1")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when authorization-svc grants, got %d: %s", w.Code, w.Body.String())
	}
	if len(authz.asked) != 1 || authz.asked[0] != "WORKFLOW_DEFINE" {
		t.Fatalf("expected one check for WORKFLOW_DEFINE, got %v", authz.asked)
	}
	if len(authz.entities) != 1 || authz.entities[0] != "PLATFORM" {
		t.Fatalf("expected the PLATFORM sentinel as legal_entity_id, got %v", authz.entities)
	}

	// Contrast: an instance-scoped op must still ask with the instance's
	// own legal entity, not the sentinel.
	instanceStore := &stubStore{
		findInstance:       pendingInstance(),
		cancelInstance:     &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, WorkflowStatus: "CANCELLED"},
		cancelTransitioned: true,
	}
	instanceAuthz := &stubAuthz{}
	instanceRouter := newTestRouterFull(instanceStore, &stubPublisher{}, instanceAuthz)

	iw := httptest.NewRecorder()
	instanceRouter.ServeHTTP(iw, lifecycleRequest("cancel", "requester-1"))

	if iw.Code != http.StatusOK {
		t.Fatalf("expected 200 for the instance-scoped cancel, got %d: %s", iw.Code, iw.Body.String())
	}
	if len(instanceAuthz.entities) != 1 || instanceAuthz.entities[0] != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("expected the instance's legal entity le-1, got %v", instanceAuthz.entities)
	}
}
