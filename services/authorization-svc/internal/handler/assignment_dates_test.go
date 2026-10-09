package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Effective dates on assignments (Authorization Standard §9: an access
// assignment carries "effective dates"; revocation is "immediate or
// effective-dated removal") and the paged list access reviews read.
//
// Before: create had no effective_to, so an end date could not be set at grant
// time; revoke could only end an assignment now; and the list stopped at 500
// rows without saying so, so a review over a larger role reviewed only part of
// it.

const datesTenant = "11111111-1111-4111-8111-111111111111"

// datedStore adds the optional capabilities to the shared stub.
type datedStore struct {
	*stubStore
	gotCreate     domain.CreateRoleAssignmentParams
	gotScheduleID string
	gotScheduleAt time.Time
	scheduleErr   error
	gotQuery      domain.AssignmentQuery
	page          []domain.PrincipalRoleAssignment
}

func (s *datedStore) CreateRoleAssignment(_ context.Context, p domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	s.gotCreate = p
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: "a-1", EffectiveTo: p.EffectiveTo}, nil
}

func (s *datedStore) ScheduleRoleAssignmentEnd(_ context.Context, id, _ string, at time.Time) (*domain.PrincipalRoleAssignment, error) {
	s.gotScheduleID, s.gotScheduleAt = id, at
	if s.scheduleErr != nil {
		return nil, s.scheduleErr
	}
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, EffectiveTo: &at}, nil
}

func (s *datedStore) QueryRoleAssignments(_ context.Context, q domain.AssignmentQuery) ([]domain.PrincipalRoleAssignment, error) {
	s.gotQuery = q
	return s.page, nil
}

func newDatedStore() *datedStore {
	return &datedStore{stubStore: &stubStore{
		role: &domain.Role{RoleID: "r-1", TenantID: datesTenant, RoleScopeType: "LEGAL_ENTITY"},
	}}
}

func datesDo(t *testing.T, s *datedStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Buffer
	if body != "" {
		rdr = bytes.NewBufferString(body)
	} else {
		rdr = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", datesTenant)
	w := httptest.NewRecorder()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	r.ServeHTTP(w, req)
	return w
}

func TestCreateRoleAssignment_EffectiveToIsStored(t *testing.T) {
	s := newDatedStore()
	end := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	body := `{"principal_id":"p-1","role_id":"r-1","legal_entity_id":"11111111-1111-4111-8111-bbbbbbbbbbb1","effective_from":"2026-01-01T00:00:00Z","effective_to":"` + end.Format(time.RFC3339) + `"}`
	w := datesDo(t, s, http.MethodPost, "/v1/admin/role-assignments", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotCreate.EffectiveTo == nil || !s.gotCreate.EffectiveTo.Equal(end) {
		t.Fatalf("effective_to not passed to the store: %v", s.gotCreate.EffectiveTo)
	}
}

func TestCreateRoleAssignment_EffectiveToMustFollowFrom(t *testing.T) {
	for name, end := range map[string]string{
		"before from": "2025-12-31T00:00:00Z",
		"in the past": "2026-01-02T00:00:00Z",
	} {
		t.Run(name, func(t *testing.T) {
			s := newDatedStore()
			body := `{"principal_id":"p-1","role_id":"r-1","legal_entity_id":"11111111-1111-4111-8111-bbbbbbbbbbb1","effective_from":"2026-01-01T00:00:00Z","effective_to":"` + end + `"}`
			w := datesDo(t, s, http.MethodPost, "/v1/admin/role-assignments", body)
			if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("invalid_effective_to")) {
				t.Fatalf("expected 400 invalid_effective_to, got %d: %s", w.Code, w.Body.String())
			}
			if s.gotCreate.PrincipalID != "" {
				t.Fatal("store reached despite an invalid end date")
			}
		})
	}
}

func TestRevokeRoleAssignment_EffectiveDatedSchedulesInsteadOfRevoking(t *testing.T) {
	s := newDatedStore()
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	w := datesDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-9/revoke", `{"effective_to":"`+at.Format(time.RFC3339)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotScheduleID != "a-9" || !s.gotScheduleAt.Equal(at) {
		t.Fatalf("schedule not called with the id and instant: %q %v", s.gotScheduleID, s.gotScheduleAt)
	}
}

func TestRevokeRoleAssignment_PastEffectiveToRefused(t *testing.T) {
	s := newDatedStore()
	w := datesDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-9/revoke", `{"effective_to":"2020-01-01T00:00:00Z"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotScheduleID != "" {
		t.Fatal("a past end date reached the store")
	}
}

func TestRevokeRoleAssignment_ScheduleOfEndedAssignmentIs404(t *testing.T) {
	s := newDatedStore()
	s.scheduleErr = domain.ErrRoleAssignmentNotFound
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	w := datesDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-9/revoke", `{"effective_to":"`+at+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListRoleAssignments_PagedFormPassesPagingAndUsage(t *testing.T) {
	s := newDatedStore()
	last := time.Now().Add(-time.Hour).UTC()
	s.page = []domain.PrincipalRoleAssignment{{PrincipalRoleAssignmentID: "a-1", LastGrantedAt: &last, PrincipalStatus: "SUSPENDED"}}
	w := datesDo(t, s, http.MethodGet, "/v1/admin/role-assignments?role_id=r-1&limit=200&offset=400&include_usage=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	q := s.gotQuery
	if q.TenantID != datesTenant || q.RoleID != "r-1" || q.Limit != 200 || q.Offset != 400 || !q.IncludeUsage || !q.ActiveOnly {
		t.Fatalf("query not forwarded: %+v", q)
	}
	var out []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out) != 1 || out[0]["principal_status"] != "SUSPENDED" || out[0]["last_granted_at"] == nil {
		t.Fatalf("usage fields missing from the response: %s", w.Body.String())
	}
}

func TestListRoleAssignments_BadPagingRefused(t *testing.T) {
	s := newDatedStore()
	w := datesDo(t, s, http.MethodGet, "/v1/admin/role-assignments?offset=-1", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
