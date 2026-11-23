package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

// recordingAuthz returns err for every check and records what was asked.
type recordingAuthz struct {
	err     error
	actions []string
	scopes  []string
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, scope, action string) error {
	a.actions = append(a.actions, action)
	a.scopes = append(a.scopes, scope)
	return a.err
}

// readTaskStore / readDeadlineStore implement only the reads; any other call
// panics on the nil embedded interface.
type readTaskStore struct{ store.TaskStore }

func (readTaskStore) GetCase(_ context.Context, id string) (*domain.Case, error) {
	return &domain.Case{CaseID: id, LegalEntityID: "le-case"}, nil
}
func (readTaskStore) GetTask(_ context.Context, id string) (*domain.Task, error) {
	return &domain.Task{TaskID: id, LegalEntityID: "le-task"}, nil
}
func (readTaskStore) GetTaskHistory(context.Context, string, string) ([]domain.TaskTransition, error) {
	return nil, nil
}
func (readTaskStore) GetSLAState(context.Context, string, string) (*domain.SLAState, error) {
	return &domain.SLAState{}, nil
}
func (readTaskStore) GetLinkedObjectStatus(context.Context, string, string) (*domain.LinkedObjectStatus, error) {
	return &domain.LinkedObjectStatus{}, nil
}

type readDeadlineStore struct{ store.DeadlineStore }

func (readDeadlineStore) GetDeadline(_ context.Context, id string) (*domain.Deadline, error) {
	return &domain.Deadline{DeadlineID: id, LegalEntityID: "le-deadline"}, nil
}
func (readDeadlineStore) GetSourceDeadline(context.Context, string, string) (*domain.DeadlineSourceInfo, error) {
	return &domain.DeadlineSourceInfo{}, nil
}
func (readDeadlineStore) ExplainCalculation(context.Context, string, string) (*domain.ExplainCalculationResult, error) {
	return &domain.ExplainCalculationResult{}, nil
}

func fullRouter(st *mockStore, az AuthZClient) http.Handler {
	h := New(st, az, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, h)
	RegisterFindingRoutes(r, h, &fakeFindingStore{})
	RegisterTaskRoutes(r, h, readTaskStore{})
	RegisterDeadlineRoutes(r, h, readDeadlineStore{})
	return r
}

func seededExceptions() *mockStore {
	st := newMockStore()
	st.cases["case-1"] = &domain.ExceptionCase{ExceptionCaseID: "case-1", LegalEntityID: "le-exc", CreatedBy: "maker", CaseStatus: domain.CaseOpen}
	return st
}

// The 14 GETs that had no authorization (audit of 23402ba0), with the read
// action and scope each must now be checked against.
var unauthorizedReads = []struct {
	path, action, scope string
}{
	{"/v1/exception-escalation/exceptions/case-1", actionExceptionRead, "le-exc"},
	{"/v1/exception-escalation/exceptions?legal_entity_id=le-q", actionExceptionRead, "le-q"},
	{"/v1/exception-escalation/exceptions", actionExceptionRead, "t1"},
	{"/v1/exception-escalation/escalations", actionExceptionRead, "t1"},
	{"/v1/cases/case-9", actionTaskRead, "le-case"},
	{"/v1/tasks/task-1", actionTaskRead, "le-task"},
	{"/v1/tasks/task-1/history", actionTaskRead, "le-task"},
	{"/v1/tasks/task-1/sla-state", actionTaskRead, "le-task"},
	{"/v1/tasks/task-1/linked-object-status", actionTaskRead, "le-task"},
	{"/v1/deadlines/dl-1", actionDeadlineRead, "le-deadline"},
	{"/v1/deadlines/dl-1/source", actionDeadlineRead, "le-deadline"},
	{"/v1/deadlines/dl-1/explain", actionDeadlineRead, "le-deadline"},
	{"/v1/audit-findings/finding-1", actionAuditFindingRead, "le"},
	{"/v1/audit-findings/finding-1/misstatement-summary", actionAuditFindingRead, "le"},
	{"/v1/audit-findings/finding-1/scope-limitations/open", actionAuditFindingRead, "le"},
}

func get(h http.Handler, path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", "t1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestReads_WithoutPrincipalAre401(t *testing.T) {
	az := &recordingAuthz{}
	h := fullRouter(seededExceptions(), az)
	for _, rd := range unauthorizedReads {
		if w := get(h, rd.path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: expected 401, got %d", rd.path, w.Code)
		}
	}
	if len(az.actions) != 0 {
		t.Fatalf("authz must not be consulted without a principal: %v", az.actions)
	}
}

func TestReads_DenyIs403_UnavailableIs503(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{authz.ErrAuthorizationDenied, http.StatusForbidden}, {authz.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable}} {
		for _, rd := range unauthorizedReads {
			if w := get(fullRouter(seededExceptions(), &recordingAuthz{err: tc.err}), rd.path, "reader"); w.Code != tc.want {
				t.Errorf("GET %s with %v: expected %d, got %d (%s)", rd.path, tc.err, tc.want, w.Code, w.Body.String())
			}
		}
	}
}

func TestReads_AllowedCheckTheReadActionAndScope(t *testing.T) {
	for _, rd := range unauthorizedReads {
		az := &recordingAuthz{}
		if w := get(fullRouter(seededExceptions(), az), rd.path, "reader"); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: expected 200, got %d (%s)", rd.path, w.Code, w.Body.String())
			continue
		}
		if len(az.actions) != 1 || az.actions[0] != rd.action || az.scopes[0] != rd.scope {
			t.Errorf("GET %s: checks %v scopes %v, want one %s on %s", rd.path, az.actions, az.scopes, rd.action, rd.scope)
		}
	}
}

func post(h http.Handler, path, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", principal)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func codeOf(w *httptest.ResponseRecorder) string {
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body["code"]
}

func TestEscalate_ActorRule(t *testing.T) {
	st := seededExceptions()
	h := fullRouter(st, &recordingAuthz{})
	w := post(h, "/v1/exception-escalation/exceptions/case-1/escalate", "escalator",
		`{"escalated_to_role":"HEAD","escalation_reason":"r","escalated_by":"forged"}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeActorMismatch {
		t.Fatalf("differing escalated_by: expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if len(st.escalations) != 0 {
		t.Fatal("refused escalation must not be stored")
	}
	w = post(h, "/v1/exception-escalation/exceptions/case-1/escalate", "escalator",
		`{"escalated_to_role":"HEAD","escalation_reason":"r"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("absent escalated_by: expected 200, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Rec domain.EscalationRecord `json:"escalation_record"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Rec.EscalatedBy != "escalator" {
		t.Fatalf("escalated_by must be the principal, got %q", resp.Rec.EscalatedBy)
	}
}

func TestResolve_ActorRuleAndClosedByOptional(t *testing.T) {
	st := seededExceptions()
	h := fullRouter(st, &recordingAuthz{})
	w := post(h, "/v1/exception-escalation/exceptions/case-1/resolve", "checker", `{"closed_by":"forged","closure_reason":"x"}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeActorMismatch {
		t.Fatalf("differing closed_by: expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if st.cases["case-1"].CaseStatus != domain.CaseOpen {
		t.Fatal("refused resolve must not change state")
	}
	// closed_by used to be required (400); it is now filled from the principal.
	w = post(h, "/v1/exception-escalation/exceptions/case-1/resolve", "checker", `{"closure_reason":"x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("absent closed_by: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := st.cases["case-1"].ClosedBy; got != "checker" {
		t.Fatalf("closed_by must be the principal, got %q", got)
	}
}

func TestResolve_SegregationOfDutiesCode(t *testing.T) {
	h := fullRouter(seededExceptions(), &recordingAuthz{})
	w := post(h, "/v1/exception-escalation/exceptions/case-1/resolve", "maker", `{"closure_reason":"x"}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeSegregationOfDuties {
		t.Fatalf("creator resolving: expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
}

type sodStore struct{ *mockStore }

func (sodStore) ResolveException(context.Context, string, *domain.ResolveCaseRequest) (*domain.ExceptionCase, error) {
	return nil, domain.ErrSelfApprovalNotAllowed
}

// The store's lock-time SoD refusal maps to the same 403.
func TestResolve_StoreSoDIs403(t *testing.T) {
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, New(sodStore{seededExceptions()}, &recordingAuthz{}, zap.NewNop()))
	w := post(r, "/v1/exception-escalation/exceptions/case-1/resolve", "checker", `{}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeSegregationOfDuties {
		t.Fatalf("expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
}

func TestCreate_ActorMismatchCode(t *testing.T) {
	h := fullRouter(newMockStore(), &recordingAuthz{})
	w := post(h, "/v1/exception-escalation/exceptions", "maker",
		`{"legal_entity_id":"le","jurisdiction_id":"GB","exception_type":"X","linked_object_type":"Y","linked_object_id":"z","created_by":"forged"}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeActorMismatch {
		t.Fatalf("expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
}

// withEventMeta puts the request identity where the store reads it for the
// outbox events.
func TestWithEventMeta(t *testing.T) {
	var got store.EventMeta
	h := withEventMeta(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = store.EventMetaFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Principal-Id", "p1")
	req.Header.Set("X-Correlation-ID", "c1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.ActorID != "p1" || got.CorrelationID != "c1" {
		t.Fatalf("event meta = %+v", got)
	}
}
