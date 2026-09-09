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

	"zoiko.io/project-accounting-svc/internal/domain"
	"zoiko.io/project-accounting-svc/internal/handler"
	"zoiko.io/project-accounting-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	projects map[string]*domain.Project

	workPackages map[string][]domain.WorkPackage // project_id -> work packages

	financialProfiles map[string]*domain.FinancialProfile // project_id -> current version
}

func newStubStore() *stubStore {
	return &stubStore{
		projects:          make(map[string]*domain.Project),
		workPackages:      make(map[string][]domain.WorkPackage),
		financialProfiles: make(map[string]*domain.FinancialProfile),
	}
}

func (s *stubStore) CreateProject(_ context.Context, p *domain.Project) error {
	for _, existing := range s.projects {
		if existing.LegalEntityID == p.LegalEntityID && existing.ProjectCode == p.ProjectCode {
			return domain.ErrDuplicateProjectCode
		}
	}
	cp := *p
	s.projects[p.ProjectID] = &cp
	return nil
}

func (s *stubStore) GetProject(_ context.Context, projectID string) (*domain.Project, error) {
	p, ok := s.projects[projectID]
	if !ok {
		return nil, domain.ErrProjectNotFound
	}
	cp := *p
	cp.WorkPackages = s.workPackages[projectID]
	return &cp, nil
}

func (s *stubStore) ListProjects(_ context.Context, legalEntityID string) ([]domain.Project, error) {
	var out []domain.Project
	for _, p := range s.projects {
		if p.LegalEntityID == legalEntityID {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubStore) ApproveProject(_ context.Context, projectID, principalID string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusDraft {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ApprovedAt, p.ApprovedByPrincipalID = domain.ProjectStatusApproved, &at, &principalID
	return nil
}

func (s *stubStore) ActivateProject(_ context.Context, projectID string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusApproved {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ActivatedAt = domain.ProjectStatusActive, &at
	return nil
}

func (s *stubStore) SuspendProject(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusActive {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.SuspendedAt, p.SuspendedByPrincipalID, p.SuspensionReason = domain.ProjectStatusSuspended, &at, &principalID, &reason
	return nil
}

func (s *stubStore) CloseProject(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok {
		return domain.ErrInvalidProjectTransition
	}
	if p.Status != domain.ProjectStatusActive && p.Status != domain.ProjectStatusSuspended {
		return domain.ErrInvalidProjectTransition
	}
	p.Status, p.ClosedAt, p.ClosedByPrincipalID, p.CloseReason = domain.ProjectStatusClosed, &at, &principalID, &reason
	return nil
}

func (s *stubStore) ReopenProjectControlled(_ context.Context, projectID, principalID, reason string, at time.Time) error {
	p, ok := s.projects[projectID]
	if !ok || p.Status != domain.ProjectStatusClosed {
		return domain.ErrInvalidProjectTransition
	}
	if p.ClosedByPrincipalID != nil && *p.ClosedByPrincipalID == principalID {
		return domain.ErrSelfReopenNotPermitted
	}
	p.Status, p.ReopenedAt, p.ReopenedByPrincipalID, p.ReopenReason = domain.ProjectStatusActive, &at, &principalID, &reason
	return nil
}

func (s *stubStore) LinkContract(_ context.Context, projectID, contractRef string) error {
	p, ok := s.projects[projectID]
	if !ok {
		return domain.ErrProjectNotFound
	}
	p.ContractRef = &contractRef
	return nil
}

func (s *stubStore) AddWorkPackage(_ context.Context, w *domain.WorkPackage) error {
	for _, existing := range s.workPackages[w.ProjectID] {
		if existing.WBSCode == w.WBSCode {
			return domain.ErrDuplicateWBSCode
		}
	}
	s.workPackages[w.ProjectID] = append(s.workPackages[w.ProjectID], *w)
	return nil
}

func (s *stubStore) AmendFinancialProfile(_ context.Context, projectID string, newVersion *domain.FinancialProfile) error {
	current, exists := s.financialProfiles[projectID]
	if exists {
		newVersion.ProfileID = current.ProfileID
		newVersion.Version = current.Version + 1
	} else {
		newVersion.ProfileID = "profile-" + projectID
		newVersion.Version = 1
	}
	cp := *newVersion
	s.financialProfiles[projectID] = &cp
	return nil
}

func (s *stubStore) GetCurrentFinancialProfile(_ context.Context, projectID string) (*domain.FinancialProfile, error) {
	f, ok := s.financialProfiles[projectID]
	if !ok {
		return nil, nil
	}
	cp := *f
	return &cp, nil
}

func (s *stubStore) GetFinancialProfileAsOf(_ context.Context, projectID string, _ time.Time) (*domain.FinancialProfile, error) {
	return s.GetCurrentFinancialProfile(context.Background(), projectID)
}

var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishProjectCreated(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectApproved(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectActivated(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectFinancialProfileChanged(_ context.Context, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishProjectClosed(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}
func (p *stubPublisher) PublishProjectReopened(_ context.Context, _, _ string, _ domain.Project) {
	p.calls++
}

var _ handler.Publisher = (*stubPublisher)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftProject(t *testing.T, r chi.Router, legalEntityID, code string) domain.Project {
	t.Helper()
	req := domain.CreateProjectRequest{LegalEntityID: legalEntityID, ProjectCode: code, Name: "Test Project"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var p domain.Project
	_ = json.NewDecoder(rr.Body).Decode(&p)
	return p
}

func approveProject(t *testing.T, r chi.Router, projectID string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/approve", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
}

func setFinancialProfile(t *testing.T, r chi.Router, projectID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	req := domain.AmendFinancialProfileRequest{
		RecognitionMethod: domain.RecognitionMethodPercentageOfCompletion, BillingType: domain.BillingTypeFixedPrice,
		Currency: "USD", EffectiveFrom: &future,
	}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("set financial profile failed: %d %s", rr.Code, rr.Body.String())
	}
}

func createActiveProject(t *testing.T, r chi.Router, legalEntityID, code string) string {
	t.Helper()
	p := createDraftProject(t, r, legalEntityID, code)
	approveProject(t, r, p.ProjectID)
	setFinancialProfile(t, r, p.ProjectID)
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/activate", nil, "manager-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("activate failed: %d %s", rr.Code, rr.Body.String())
	}
	return p.ProjectID
}

// ── CreateProject ─────────────────────────────────────────────────────────────

func TestCreateProject_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/projects/", map[string]string{}, "creator-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateProject_DuplicateCode_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createDraftProject(t, r, "le-1", "PRJ-DUP")

	req := domain.CreateProjectRequest{LegalEntityID: "le-1", ProjectCode: "PRJ-DUP", Name: "Another"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ApproveProject (self-approval SoD) ───────────────────────────────────────

func TestApproveProject_SameCreator_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-1")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/approve", nil, "creator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveProject_DifferentApprover_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-2")
	approveProject(t, r, p.ProjectID)
	if s.projects[p.ProjectID].Status != domain.ProjectStatusApproved {
		t.Fatalf("expected APPROVED, got %q", s.projects[p.ProjectID].Status)
	}
}

// ── ActivateProject ("Missing policy/currency/book blocks activation") ──────

func TestActivateProject_NoFinancialProfile_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-3")
	approveProject(t, r, p.ProjectID)

	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/activate", nil, "manager-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActivateProject_WithFinancialProfile_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-4")
	if s.projects[id].Status != domain.ProjectStatusActive {
		t.Fatalf("expected ACTIVE, got %q", s.projects[id].Status)
	}
}

// ── AmendFinancialProfile ("Recognition policy changed after run") ──────────

func TestAmendFinancialProfile_PastEffectiveDate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-5")

	past := time.Now().UTC().Add(-time.Hour)
	req := domain.AmendFinancialProfileRequest{
		RecognitionMethod: domain.RecognitionMethodTimeAndMaterials, BillingType: domain.BillingTypeTimeAndMaterials,
		Currency: "USD", EffectiveFrom: &past,
	}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retroactive profile change, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAmendFinancialProfile_InvalidRecognitionMethod_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	p := createDraftProject(t, r, "le-1", "PRJ-6")

	future := time.Now().UTC().Add(time.Hour)
	req := domain.AmendFinancialProfileRequest{RecognitionMethod: "MADE_UP", BillingType: domain.BillingTypeFixedPrice, Currency: "USD", EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+p.ProjectID+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── CloseProject / ReopenProjectControlled ───────────────────────────────────

func TestCloseProject_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-7")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", map[string]string{}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReopenProjectControlled_SameCloser_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-8")
	closeResp := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", domain.CloseProjectRequest{Reason: "done"}, "closer-1")
	if closeResp.Code != http.StatusOK {
		t.Fatalf("close failed: %d %s", closeResp.Code, closeResp.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/reopen", domain.ReopenProjectRequest{Reason: "need more work"}, "closer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-reopen, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReopenProjectControlled_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-9")
	closeResp := doReq(r, http.MethodPost, "/v1/projects/"+id+"/close", domain.CloseProjectRequest{Reason: "done"}, "closer-1")
	if closeResp.Code != http.StatusOK {
		t.Fatalf("close failed: %d %s", closeResp.Code, closeResp.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/reopen", domain.ReopenProjectRequest{Reason: "need more work"}, "reviewer-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.projects[id].Status != domain.ProjectStatusActive {
		t.Fatalf("expected ACTIVE after reopen, got %q", s.projects[id].Status)
	}
}

// ── AddWorkPackage (WBS deletion orphans historical cost — no delete path) ───

func TestAddWorkPackage_DuplicateCode_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-10")

	req := domain.AddWorkPackageRequest{WBSCode: "WBS-1"}
	first := doReq(r, http.MethodPost, "/v1/projects/"+id+"/work-packages", req, "manager-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first add failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/projects/"+id+"/work-packages", req, "manager-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 duplicate wbs_code, got %d: %s", second.Code, second.Body.String())
	}
}

// ── LinkContract ──────────────────────────────────────────────────────────────

func TestLinkContract_MissingRef_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "PRJ-11")

	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/link-contract", map[string]string{}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Authorization ────────────────────────────────────────────────────────────

func TestCreateProject_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateProjectRequest{LegalEntityID: "le-1", ProjectCode: "PRJ-12", Name: "Test"}
	rr := doReq(r, http.MethodPost, "/v1/projects/", req, "creator-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
