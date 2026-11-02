package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/store"
)

func doJSON(r http.Handler, method, path, principal string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func exceptionBody(createdBy string) domain.CreateExceptionRequest {
	return domain.CreateExceptionRequest{LegalEntityID: "le", JurisdictionID: "GB", ExceptionType: "OVERDUE_FILING",
		SeverityLevel: domain.SeverityHigh, LinkedObjectType: "FILING", LinkedObjectID: "f1", CreatedBy: createdBy}
}

// created_by used to be taken from the body, so omitting it let the creator
// pass the resolve-time self-approval check on their own case.
func TestCreateException_CreatorIsThePrincipal(t *testing.T) {
	r, _ := setupTestRouter()
	if w := doJSON(r, "POST", "/v1/exception-escalation/exceptions", "p1", exceptionBody("someone-else")); w.Code != http.StatusForbidden {
		t.Fatalf("mismatched created_by: expected 403, got %d", w.Code)
	}
	w := doJSON(r, "POST", "/v1/exception-escalation/exceptions", "p1", exceptionBody(""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", w.Code)
	}
	var c domain.ExceptionCase
	_ = json.NewDecoder(w.Body).Decode(&c)
	if c.CreatedBy != "p1" {
		t.Fatalf("created_by = %q, want p1", c.CreatedBy)
	}
	if w := doJSON(r, "POST", "/v1/exception-escalation/exceptions/"+c.ExceptionCaseID+"/resolve", "p1",
		domain.ResolveCaseRequest{ClosedBy: "p1"}); w.Code != http.StatusForbidden {
		t.Fatalf("self-resolve: expected 403, got %d", w.Code)
	}
}

// An unknown priority used to reach the tasks_priority_check constraint and
// come back as a 500.
func TestCreateTask_InvalidPriorityIs400(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	RegisterTaskRoutes(r, New(newMockStore(), &mockPublisher{}, &mockAuthzClient{}, logger), nil)
	w := doJSON(r, "POST", "/v1/tasks", "p1", map[string]string{"legal_entity_id": "le", "task_type": "X",
		"linked_object_type": "Y", "linked_object_id": "z", "priority": "URGENT"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d - %s", w.Code, w.Body.String())
	}
}

// fakeFindingStore implements only what these tests touch; any other call
// panics on the nil embedded interface.
type fakeFindingStore struct {
	store.FindingStore
	communicated bool
	resolved     bool
}

func (f *fakeFindingStore) GetFinding(_ context.Context, id string) (*domain.AuditFinding, error) {
	if id != "finding-1" {
		return nil, domain.ErrFindingNotFound
	}
	return &domain.AuditFinding{FindingID: id, LegalEntityID: "le", Status: domain.FindingIdentified}, nil
}

func (f *fakeFindingStore) CommunicateFinding(_ context.Context, p domain.CommunicateFindingParams) (*domain.AuditFinding, error) {
	f.communicated = true
	return &domain.AuditFinding{FindingID: p.FindingID, Status: domain.FindingCommunicated}, nil
}

func (f *fakeFindingStore) GetScopeLimitation(_ context.Context, id string) (*domain.ScopeLimitation, error) {
	return &domain.ScopeLimitation{LimitationID: id, FindingID: "finding-1"}, nil
}

func (f *fakeFindingStore) ResolveScopeLimitation(_ context.Context, p domain.ResolveScopeLimitationParams) (*domain.ScopeLimitation, error) {
	f.resolved = true
	return &domain.ScopeLimitation{LimitationID: p.LimitationID}, nil
}

func (f *fakeFindingStore) ListOpenScopeLimitations(_ context.Context, _, _ string) ([]domain.ScopeLimitation, error) {
	return nil, nil
}

func (f *fakeFindingStore) GetMisstatementSummary(_ context.Context, _ string) ([]domain.MisstatementRecord, error) {
	return nil, nil
}

func findingRouter(fs *fakeFindingStore, authzErr error) http.Handler {
	logger, _ := zap.NewDevelopment()
	r := chi.NewRouter()
	RegisterFindingRoutes(r, New(newMockStore(), &mockPublisher{}, &mockAuthzClient{err: authzErr}, logger), fs)
	return r
}

// communicate and scope-limitation resolve used to run with no principal and
// no authorization check at all.
func TestFindingCommands_RequirePrincipalAndAuthorization(t *testing.T) {
	cases := []struct{ name, path string }{
		{"communicate", "/v1/audit-findings/finding-1/communicate"},
		{"resolve scope limitation", "/v1/audit-findings/scope-limitations/scopelimit-1/resolve"},
	}
	for _, c := range cases {
		fs := &fakeFindingStore{}
		if w := doJSON(findingRouter(fs, nil), "POST", c.path, "", map[string]any{}); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", c.name, w.Code)
		}
		if w := doJSON(findingRouter(fs, authz.ErrAuthorizationDenied), "POST", c.path, "p1", map[string]any{}); w.Code != http.StatusForbidden {
			t.Errorf("%s denied: expected 403, got %d", c.name, w.Code)
		}
		if fs.communicated || fs.resolved {
			t.Errorf("%s: mutation ran without authorization", c.name)
		}
		if w := doJSON(findingRouter(fs, nil), "POST", c.path, "p1", map[string]any{}); w.Code != http.StatusOK {
			t.Errorf("%s granted: expected 200, got %d", c.name, w.Code)
		}
	}
}

// Empty collections used to serialize as JSON null.
func TestFindingLists_EmptyIsArrayNotNull(t *testing.T) {
	r := findingRouter(&fakeFindingStore{}, nil)
	for _, p := range []string{"/v1/audit-findings/finding-1/scope-limitations/open", "/v1/audit-findings/finding-1/misstatement-summary"} {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if got := bytes.TrimSpace(w.Body.Bytes()); string(got) != "[]" {
			t.Errorf("%s: expected [], got %s", p, got)
		}
	}
}
