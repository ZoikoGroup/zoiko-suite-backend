package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/handler"
)

// ── stub store ────────────────────────────────────────────────────────────────

type stubStore struct {
	instance  *domain.WorkflowInstance
	stages    []*domain.WorkflowStage
	createErr error

	findInstance *domain.WorkflowInstance
	findErr      error

	currentStage    *domain.WorkflowStage
	currentStageErr error

	submitInstance     *domain.WorkflowInstance
	submitStage        *domain.WorkflowStage
	submitTransitioned bool
	submitErr          error

	escalateInstance     *domain.WorkflowInstance
	escalateTransitioned bool
	escalateErr          error

	cancelInstance     *domain.WorkflowInstance
	cancelTransitioned bool
	cancelErr          error
}

func (s *stubStore) CreateWorkflow(_ context.Context, _ domain.CreateWorkflowParams) (*domain.WorkflowInstance, []*domain.WorkflowStage, error) {
	return s.instance, s.stages, s.createErr
}
func (s *stubStore) FindWorkflowByID(_ context.Context, _ string) (*domain.WorkflowInstance, error) {
	return s.findInstance, s.findErr
}
func (s *stubStore) FindStagesByWorkflowID(_ context.Context, _ string) ([]*domain.WorkflowStage, error) {
	return s.stages, nil
}
func (s *stubStore) FindCurrentStage(_ context.Context, _ string) (*domain.WorkflowStage, error) {
	return s.currentStage, s.currentStageErr
}
func (s *stubStore) SubmitAction(_ context.Context, _ domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error) {
	return s.submitInstance, s.submitStage, s.submitTransitioned, s.submitErr
}
func (s *stubStore) EscalateWorkflow(_ context.Context, _, _ string) (*domain.WorkflowInstance, bool, error) {
	return s.escalateInstance, s.escalateTransitioned, s.escalateErr
}
func (s *stubStore) CancelWorkflow(_ context.Context, _, _ string) (*domain.WorkflowInstance, bool, error) {
	return s.cancelInstance, s.cancelTransitioned, s.cancelErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	startedCalls   int
	grantedCalls   int
	rejectedCalls  int
	escalatedCalls int
	completedCalls int
}

func (p *stubPublisher) PublishWorkflowStarted(_ context.Context, _ domain.WorkflowInstance) error {
	p.startedCalls++
	return nil
}
func (p *stubPublisher) PublishApprovalGranted(_ context.Context, _ domain.WorkflowInstance, _ domain.WorkflowStage) error {
	p.grantedCalls++
	return nil
}
func (p *stubPublisher) PublishApprovalRejected(_ context.Context, _ domain.WorkflowInstance, _ domain.WorkflowStage) error {
	p.rejectedCalls++
	return nil
}
func (p *stubPublisher) PublishWorkflowEscalated(_ context.Context, _ domain.WorkflowInstance) error {
	p.escalatedCalls++
	return nil
}
func (p *stubPublisher) PublishWorkflowCompleted(_ context.Context, _ domain.WorkflowInstance) error {
	p.completedCalls++
	return nil
}

// ── stub authz client ────────────────────────────────────────────────────────

type stubAuthz struct{ err error }

func (a *stubAuthz) CheckApprovalAllowed(_ context.Context, _, _ string) error { return a.err }

func newTestRouter(s *stubStore) chi.Router {
	return newTestRouterFull(s, &stubPublisher{}, &stubAuthz{})
}

func newTestRouterFull(s *stubStore, p *stubPublisher, a *stubAuthz) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, a, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validCreateBody() string {
	return `{"tenant_id":"t-1","legal_entity_id":"le-1","workflow_type":"PURCHASE_APPROVAL","initiated_by":"requester-1","stages":[{"approver_principal_id":"approver-1"},{"approver_principal_id":"approver-2"}]}`
}

// ── CreateWorkflow ───────────────────────────────────────────────────────────

func TestCreateWorkflow_Created(t *testing.T) {
	store := &stubStore{
		instance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING"},
		stages:   []*domain.WorkflowStage{{WorkflowStageID: "s-1", StageOrder: 1}, {WorkflowStageID: "s-2", StageOrder: 2}},
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubAuthz{})

	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.startedCalls != 1 {
		t.Errorf("expected workflow.started published once, got %d", pub.startedCalls)
	}
}

func TestCreateWorkflow_NoStages(t *testing.T) {
	r := newTestRouter(&stubStore{})

	body := `{"tenant_id":"t-1","legal_entity_id":"le-1","workflow_type":"PURCHASE_APPROVAL","initiated_by":"requester-1","stages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateWorkflow_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── SubmitAction ─────────────────────────────────────────────────────────────

func TestSubmitAction_Approved_PublishesGrantedOnly_WhenNotFinalStage(t *testing.T) {
	store := &stubStore{
		findInstance:       &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"},
		submitInstance:     &domain.WorkflowInstance{WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING"},
		submitStage:        &domain.WorkflowStage{StageOrder: 1, StageStatus: "APPROVED"},
		submitTransitioned: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubAuthz{})

	body := `{"actor_principal_id":"approver-1","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pub.grantedCalls != 1 {
		t.Errorf("expected approval.granted published once, got %d", pub.grantedCalls)
	}
	if pub.completedCalls != 0 {
		t.Errorf("expected workflow.completed NOT published (not final stage), got %d", pub.completedCalls)
	}
}

func TestSubmitAction_FinalApprove_PublishesCompleted(t *testing.T) {
	store := &stubStore{
		findInstance:       &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"},
		submitInstance:     &domain.WorkflowInstance{WorkflowInstanceID: "w-1", WorkflowStatus: "APPROVED"},
		submitStage:        &domain.WorkflowStage{StageOrder: 2, StageStatus: "APPROVED"},
		submitTransitioned: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubAuthz{})

	body := `{"actor_principal_id":"approver-2","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if pub.grantedCalls != 1 || pub.completedCalls != 1 {
		t.Errorf("expected granted+completed published once each, got granted=%d completed=%d", pub.grantedCalls, pub.completedCalls)
	}
}

func TestSubmitAction_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{
		findInstance:       &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"},
		submitInstance:     &domain.WorkflowInstance{WorkflowInstanceID: "w-1", WorkflowStatus: "PENDING"},
		submitStage:        &domain.WorkflowStage{StageOrder: 1, StageStatus: "APPROVED"},
		submitTransitioned: false,
	}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubAuthz{})

	body := `{"actor_principal_id":"approver-1","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if pub.grantedCalls != 0 {
		t.Errorf("expected no publish on idempotent no-op, got %d", pub.grantedCalls)
	}
}

func TestSubmitAction_AuthorizationDenied_Returns403_NeverTouchesStore(t *testing.T) {
	store := &stubStore{
		findInstance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"},
		submitErr:    domain.ErrWorkflowNotFound, // would only be hit if SubmitAction were called
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubAuthz{err: domain.ErrAuthorizationDenied})

	body := `{"actor_principal_id":"approver-1","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitAction_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	store := &stubStore{findInstance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"}}
	r := newTestRouterFull(store, &stubPublisher{}, &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable})

	body := `{"actor_principal_id":"approver-1","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail-closed), got %d", w.Code)
	}
}

func TestSubmitAction_WrongApprover(t *testing.T) {
	store := &stubStore{
		findInstance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1", WorkflowStatus: "PENDING"},
		submitErr:    domain.ErrWrongApprover,
	}
	r := newTestRouterFull(store, &stubPublisher{}, &stubAuthz{})

	body := `{"actor_principal_id":"someone-else","action":"APPROVE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestSubmitAction_InvalidAction(t *testing.T) {
	store := &stubStore{findInstance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", LegalEntityID: "le-1"}}
	r := newTestRouter(store)

	body := `{"actor_principal_id":"approver-1","action":"MAYBE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/actions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── Escalate / Cancel ────────────────────────────────────────────────────────

func TestEscalateWorkflow_InvalidTransition(t *testing.T) {
	store := &stubStore{escalateErr: domain.ErrInvalidTransition}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/escalate", bytes.NewBufferString(`{"actor_principal_id":"admin-1"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestCancelWorkflow_Success(t *testing.T) {
	store := &stubStore{cancelInstance: &domain.WorkflowInstance{WorkflowInstanceID: "w-1", WorkflowStatus: "CANCELLED"}, cancelTransitioned: true}
	pub := &stubPublisher{}
	r := newTestRouterFull(store, pub, &stubAuthz{})

	req := httptest.NewRequest(http.MethodPost, "/v1/workflows/w-1/cancel", bytes.NewBufferString(`{"actor_principal_id":"admin-1"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["workflow_status"] != "CANCELLED" {
		t.Errorf("expected CANCELLED, got %v", got["workflow_status"])
	}
}

// ── GetNextApprover ──────────────────────────────────────────────────────────

func TestGetNextApprover_NotFound(t *testing.T) {
	store := &stubStore{currentStageErr: domain.ErrWorkflowNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/w-1/next-approver", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetNextApprover_Found(t *testing.T) {
	store := &stubStore{currentStage: &domain.WorkflowStage{StageOrder: 1, ApproverPrincipalID: "approver-1"}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/w-1/next-approver", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}
