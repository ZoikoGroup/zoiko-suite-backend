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

// ORG-06 SuspendAssignment, resume, the suspension history and GetAssignmentsAsOf
// at the HTTP layer: permission gates, the always-required suspension reason,
// error mapping, and the as_of parameter. The state machine and the SQL are
// proven on a real database in internal/store.

const suspTenant = "22222222-2222-4222-8222-222222222222"

// suspStore adds the suspension capabilities to the shared stub.
type suspStore struct {
	*stubStore

	suspendErr, resumeErr, listErr, asOfErr error
	calls                                   []string
	gotID, gotActor, gotReason              string
	gotAsOf                                 domain.AssignmentsAsOfQuery
	asOfRows                                []domain.PrincipalRoleAssignment
	suspensions                             []domain.AssignmentSuspension
}

func (s *suspStore) SuspendRoleAssignment(_ context.Context, id, _, actor, reason string) (*domain.PrincipalRoleAssignment, error) {
	s.calls = append(s.calls, "suspend")
	s.gotID, s.gotActor, s.gotReason = id, actor, reason
	if s.suspendErr != nil {
		return nil, s.suspendErr
	}
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, ApprovalStatus: domain.ApprovalSuspended}, nil
}

func (s *suspStore) ResumeRoleAssignment(_ context.Context, id, _, actor, reason string) (*domain.PrincipalRoleAssignment, error) {
	s.calls = append(s.calls, "resume")
	s.gotID, s.gotActor, s.gotReason = id, actor, reason
	if s.resumeErr != nil {
		return nil, s.resumeErr
	}
	return &domain.PrincipalRoleAssignment{PrincipalRoleAssignmentID: id, ApprovalStatus: domain.ApprovalApproved}, nil
}

func (s *suspStore) ListAssignmentSuspensions(_ context.Context, id, _ string) ([]domain.AssignmentSuspension, error) {
	s.gotID = id
	return s.suspensions, s.listErr
}

func (s *suspStore) QueryAssignmentsAsOf(_ context.Context, q domain.AssignmentsAsOfQuery) ([]domain.PrincipalRoleAssignment, error) {
	s.gotAsOf = q
	return s.asOfRows, s.asOfErr
}

func newSuspStore() *suspStore { return &suspStore{stubStore: &stubStore{}} }

func suspDo(t *testing.T, s *suspStore, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", suspTenant)
	w := httptest.NewRecorder()
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "", false, zap.NewNop()))
	r.ServeHTTP(w, req)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if c, _ := b["error"].(string); c != "" {
		return c
	}
	c, _ := b["code"].(string)
	return c
}

func TestSuspend_AppliesTheCommandAsTheVerifiedCaller(t *testing.T) {
	s := newSuspStore()
	w := suspDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"reason_code":"INVESTIGATION","purpose":"fraud review"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s.gotID != "a-1" || s.gotActor != "admin-1" {
		t.Errorf("store got id=%q actor=%q; the actor must be the verified caller", s.gotID, s.gotActor)
	}
	if s.gotReason != "INVESTIGATION: fraud review" {
		t.Errorf("reason = %q", s.gotReason)
	}
	var a domain.PrincipalRoleAssignment
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	if a.ApprovalStatus != domain.ApprovalSuspended {
		t.Errorf("response status = %s", a.ApprovalStatus)
	}
}

// A suspension with no reason is evidence of nothing, so it is refused whatever
// the contract mode; the store is never reached.
func TestSuspend_AReasonIsAlwaysRequired(t *testing.T) {
	for _, body := range []string{``, `{}`, `{"reason_code":"  "}`} {
		s := newSuspStore()
		w := suspDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d, want 400", body, w.Code)
		}
		if len(s.calls) != 0 {
			t.Errorf("body %q: the store was called without a reason", body)
		}
	}
}

func TestResume_ReasonIsOptional(t *testing.T) {
	s := newSuspStore()
	w := suspDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/resume", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s.gotActor != "admin-1" {
		t.Errorf("actor = %q", s.gotActor)
	}
}

func TestSuspension_PermissionIsRequired(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{"purpose":"x"}`},
		{http.MethodPost, "/v1/admin/role-assignments/a-1/resume", `{}`},
		{http.MethodGet, "/v1/admin/role-assignments/a-1/suspensions", ``},
		{http.MethodGet, "/v1/admin/role-assignments/as-of", ``},
	} {
		s := newSuspStore()
		s.denyAdmin = true
		w := suspDo(t, s, route.method, route.path, route.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", route.method, route.path, w.Code)
		}
		if len(s.calls) != 0 {
			t.Errorf("%s %s: reached the store without the permission", route.method, route.path)
		}
	}
}

func TestSuspension_StoreRefusalsAreMapped(t *testing.T) {
	cases := []struct {
		name       string
		suspend    bool
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unknown", true, domain.ErrRoleAssignmentNotFound, http.StatusNotFound, "role_assignment_not_found"},
		{"not suspendable", true, domain.ErrAssignmentNotSuspendable, http.StatusConflict, "assignment_not_suspendable"},
		{"not suspended", false, domain.ErrAssignmentNotSuspended, http.StatusConflict, "assignment_not_suspended"},
		{"ended", false, domain.ErrAssignmentEnded, http.StatusConflict, "assignment_ended"},
		{"self resume", false, domain.ErrSelfResume, http.StatusForbidden, "self_resume_not_allowed"},
		{"store down (suspend)", true, domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
		{"store down (resume)", false, domain.ErrStoreUnavailable, http.StatusServiceUnavailable, "store_unavailable"},
	}
	for _, c := range cases {
		s := newSuspStore()
		path, body := "/v1/admin/role-assignments/a-1/resume", `{}`
		if c.suspend {
			s.suspendErr = c.err
			path, body = "/v1/admin/role-assignments/a-1/suspend", `{"purpose":"x"}`
		} else {
			s.resumeErr = c.err
		}
		w := suspDo(t, s, http.MethodPost, path, body)
		if w.Code != c.wantStatus || errorCode(t, w) != c.wantCode {
			t.Errorf("%s: %d %q, want %d %q (%s)", c.name, w.Code, errorCode(t, w), c.wantStatus, c.wantCode, w.Body.String())
		}
	}
}

func TestSuspension_MalformedBodyIs400(t *testing.T) {
	s := newSuspStore()
	w := suspDo(t, s, http.MethodPost, "/v1/admin/role-assignments/a-1/suspend", `{not json`)
	if w.Code != http.StatusBadRequest || len(s.calls) != 0 {
		t.Errorf("status %d, calls %v", w.Code, s.calls)
	}
}

func TestListAssignmentSuspensions(t *testing.T) {
	s := newSuspStore()
	by := "admin-2"
	s.suspensions = []domain.AssignmentSuspension{{SuspensionID: "s-1", PrincipalRoleAssignmentID: "a-1", SuspendedBy: "admin-1", ResumedBy: &by}}
	w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/a-1/suspensions", ``)
	if w.Code != http.StatusOK || s.gotID != "a-1" {
		t.Fatalf("status %d id %q: %s", w.Code, s.gotID, w.Body.String())
	}
	var out []domain.AssignmentSuspension
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 1 || out[0].SuspendedBy != "admin-1" {
		t.Errorf("body %s (%v)", w.Body.String(), err)
	}

	s.listErr = domain.ErrRoleAssignmentNotFound
	if w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/a-1/suspensions", ``); w.Code != http.StatusNotFound {
		t.Errorf("unknown assignment: %d", w.Code)
	}
}

func TestAssignmentsAsOf_ParsesTheInstantAndScopesTheQuery(t *testing.T) {
	for raw, want := range map[string]time.Time{
		"2026-03-31":           time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
		"2026-03-31T10:30:00Z": time.Date(2026, 3, 31, 10, 30, 0, 0, time.UTC),
	} {
		s := newSuspStore()
		w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/as-of?principal_id=alice&as_of="+raw, ``)
		if w.Code != http.StatusOK {
			t.Fatalf("as_of=%s: status %d: %s", raw, w.Code, w.Body.String())
		}
		if !s.gotAsOf.AsOf.Equal(want) || s.gotAsOf.PrincipalID != "alice" || s.gotAsOf.TenantID != suspTenant {
			t.Errorf("as_of=%s: query = %+v", raw, s.gotAsOf)
		}
	}
}

func TestAssignmentsAsOf_DefaultsToNowAndNeverReturnsNull(t *testing.T) {
	s := newSuspStore()
	before := time.Now()
	w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/as-of", ``)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if s.gotAsOf.AsOf.Before(before.Add(-time.Second)) || s.gotAsOf.AsOf.After(time.Now().Add(time.Second)) {
		t.Errorf("default as_of = %v, want about now", s.gotAsOf.AsOf)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if arr, ok := body["assignments"].([]any); !ok || len(arr) != 0 {
		t.Errorf("assignments = %#v, want an empty list, not null", body["assignments"])
	}
}

// Answering today's structure to a question about a date the caller mistyped
// would be a wrong answer that looks right.
func TestAssignmentsAsOf_MalformedInstantIs400(t *testing.T) {
	s := newSuspStore()
	w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/as-of?as_of=last-tuesday", ``)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d", w.Code)
	}
	if !s.gotAsOf.AsOf.IsZero() {
		t.Error("the store was queried for an unparseable date")
	}
}

func TestAssignmentsAsOf_StoreOutageIs503(t *testing.T) {
	s := newSuspStore()
	s.asOfErr = domain.ErrStoreUnavailable
	if w := suspDo(t, s, http.MethodGet, "/v1/admin/role-assignments/as-of", ``); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d", w.Code)
	}
}
