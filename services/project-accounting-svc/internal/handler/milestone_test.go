package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// stub milestone storage lives outside stubStore (keyed by pointer) so this
// file does not need to edit handler_test.go.
var stubMilestoneData = map[*stubStore]map[string]*domain.Milestone{}

func (s *stubStore) ms() map[string]*domain.Milestone {
	m, ok := stubMilestoneData[s]
	if !ok {
		m = map[string]*domain.Milestone{}
		stubMilestoneData[s] = m
	}
	return m
}

func (s *stubStore) DefineMilestone(_ context.Context, m *domain.Milestone) error {
	for _, e := range s.ms() {
		if e.ProjectID == m.ProjectID && e.Name == m.Name {
			return domain.ErrDuplicateMilestoneName
		}
	}
	cp := *m
	s.ms()[m.MilestoneID] = &cp
	return nil
}

func (s *stubStore) GetMilestone(_ context.Context, id string) (*domain.Milestone, error) {
	m, ok := s.ms()[id]
	if !ok {
		return nil, domain.ErrMilestoneNotFound
	}
	cp := *m
	return &cp, nil
}

func (s *stubStore) ListMilestones(_ context.Context, projectID string) ([]domain.Milestone, error) {
	var out []domain.Milestone
	for _, m := range s.ms() {
		if m.ProjectID == projectID {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *stubStore) MarkMilestoneAchieved(_ context.Context, id, principalID, evidence string, at time.Time) error {
	m, ok := s.ms()[id]
	if !ok {
		return domain.ErrMilestoneNotFound
	}
	if evidence == "" {
		return domain.ErrMilestoneEvidenceRequired
	}
	if m.Status != domain.MilestoneStatusPlanned {
		return domain.ErrInvalidMilestoneTransition
	}
	m.Status, m.AchievementEvidenceRef, m.AchievedAt, m.AchievedByPrincipalID = domain.MilestoneStatusAchieved, evidence, &at, &principalID
	return nil
}

func (s *stubStore) ApproveMilestoneAchievement(_ context.Context, id, principalID string, at time.Time) error {
	m, ok := s.ms()[id]
	if !ok {
		return domain.ErrMilestoneNotFound
	}
	if m.Status != domain.MilestoneStatusAchieved || m.ApprovedAt != nil {
		return domain.ErrInvalidMilestoneTransition
	}
	if m.AchievedByPrincipalID != nil && *m.AchievedByPrincipalID == principalID {
		return domain.ErrSelfApprovalNotPermittedMilestone
	}
	m.ApprovedAt, m.ApprovedByPrincipalID = &at, &principalID
	return nil
}

func (s *stubStore) ListRunMilestones(_ context.Context, _ string) ([]domain.RunMilestone, error) {
	return nil, nil
}

func defineMilestone(t *testing.T, r chi.Router, projectID, name string, amount float64) domain.Milestone {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/milestones", domain.DefineMilestoneRequest{Name: name, Amount: amount}, "pm-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("define milestone failed: %d %s", rr.Code, rr.Body.String())
	}
	var m domain.Milestone
	_ = json.NewDecoder(rr.Body).Decode(&m)
	return m
}

func achieve(r chi.Router, projectID, milestoneID, evidence, principal string) int {
	rr := doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/milestones/"+milestoneID+"/achieve",
		domain.MarkMilestoneAchievedRequest{AchievementEvidenceRef: evidence}, principal)
	return rr.Code
}

func approveMs(r chi.Router, projectID, milestoneID, principal string) int {
	return doReq(r, http.MethodPost, "/v1/projects/"+projectID+"/milestones/"+milestoneID+"/approve", nil, principal).Code
}

func TestFinancialProfile_MilestoneMethod_Accepted(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "MS-PROF")
	future := time.Now().UTC().Add(2 * time.Hour)
	req := domain.AmendFinancialProfileRequest{
		RecognitionMethod: domain.RecognitionMethodMilestone, BillingType: domain.BillingTypeFixedPrice, Currency: "USD", EffectiveFrom: &future,
	}
	rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/financial-profile", req, "manager-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 for MILESTONE method, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDefineMilestone_Validation(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "MS-1")

	if rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/milestones", domain.DefineMilestoneRequest{Amount: 10}, "pm-1"); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing name: expected 400, got %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/milestones", domain.DefineMilestoneRequest{Name: "a", Amount: 0}, "pm-1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zero amount: expected 422, got %d", rr.Code)
	}
	defineMilestone(t, r, id, "Design", 100)
	if rr := doReq(r, http.MethodPost, "/v1/projects/"+id+"/milestones", domain.DefineMilestoneRequest{Name: "Design", Amount: 5}, "pm-1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate name: expected 422, got %d", rr.Code)
	}
}

func TestMilestone_AchievedWithoutEvidence_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "MS-2")
	m := defineMilestone(t, r, id, "Design", 100)
	if code := achieve(r, id, m.MilestoneID, "  ", "pm-1"); code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 evidence_required, got %d", code)
	}
}

func TestMilestone_SelfApproval_Refused_DifferentPrincipalAllowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "MS-3")
	m := defineMilestone(t, r, id, "Design", 100)

	if code := approveMs(r, id, m.MilestoneID, "approver-1"); code != http.StatusUnprocessableEntity {
		t.Fatalf("approve before achieved: expected 422, got %d", code)
	}
	if code := achieve(r, id, m.MilestoneID, "signed-acceptance-1", "pm-1"); code != http.StatusOK {
		t.Fatalf("achieve: expected 200, got %d", code)
	}
	if code := approveMs(r, id, m.MilestoneID, "pm-1"); code != http.StatusForbidden {
		t.Fatalf("self-approval: expected 403, got %d", code)
	}
	if code := approveMs(r, id, m.MilestoneID, "approver-1"); code != http.StatusOK {
		t.Fatalf("approval by a different principal: expected 200, got %d", code)
	}
	if code := approveMs(r, id, m.MilestoneID, "approver-2"); code != http.StatusUnprocessableEntity {
		t.Fatalf("double approval: expected 422, got %d", code)
	}
}

func TestMilestone_CrossProjectAction_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	a := createActiveProject(t, r, "le-1", "MS-A")
	b := createActiveProject(t, r, "le-1", "MS-B")
	m := defineMilestone(t, r, a, "Design", 100)
	if code := achieve(r, b, m.MilestoneID, "ev", "pm-1"); code != http.StatusNotFound {
		t.Fatalf("expected 404 for a milestone addressed under another project, got %d", code)
	}
}

func TestListMilestones_And_Authz(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "MS-4")
	defineMilestone(t, r, id, "Design", 100)
	defineMilestone(t, r, id, "Build", 200)
	rr := doReq(r, http.MethodGet, "/v1/projects/"+id+"/milestones", nil, "viewer-1")
	var list []domain.Milestone
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if rr.Code != http.StatusOK || len(list) != 2 {
		t.Fatalf("expected 2 milestones, got %d / %d", rr.Code, len(list))
	}

	denied := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	if rr := doReq(denied, http.MethodPost, "/v1/projects/"+id+"/milestones", domain.DefineMilestoneRequest{Name: "X", Amount: 1}, "pm-1"); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authz denies, got %d", rr.Code)
	}
}
