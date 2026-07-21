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

	"zoiko.io/compensation-svc/internal/domain"
	"zoiko.io/compensation-svc/internal/employee"
	"zoiko.io/compensation-svc/internal/handler"
	"zoiko.io/compensation-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	structures map[string]*domain.CompensationStructure
	revisions  map[string]*domain.WageRevision
	bonuses    map[string]*domain.BonusGrant
}

func newStubStore() *stubStore {
	return &stubStore{
		structures: make(map[string]*domain.CompensationStructure),
		revisions:  make(map[string]*domain.WageRevision),
		bonuses:    make(map[string]*domain.BonusGrant),
	}
}

func (s *stubStore) CreateStructure(_ context.Context, str *domain.CompensationStructure) error {
	s.structures[str.StructureID] = str
	return nil
}

func (s *stubStore) ListStructures(_ context.Context, legalEntityID string) ([]domain.CompensationStructure, error) {
	var out []domain.CompensationStructure
	for _, str := range s.structures {
		if legalEntityID != "" && str.LegalEntityID != legalEntityID {
			continue
		}
		out = append(out, *str)
	}
	return out, nil
}

func (s *stubStore) CreateWageRevision(_ context.Context, rev *domain.WageRevision) error {
	for _, r := range s.revisions {
		if r.EmployeeID == rev.EmployeeID && r.Status == "ACTIVE" {
			r.Status = "SUPERSEDED"
			r.EffectiveTo = &rev.EffectiveFrom
		}
	}
	s.revisions[rev.RevisionID] = rev
	return nil
}

func (s *stubStore) GetActiveWageRevision(_ context.Context, employeeID string) (*domain.WageRevision, error) {
	for _, r := range s.revisions {
		if r.EmployeeID == employeeID && r.Status == "ACTIVE" {
			return r, nil
		}
	}
	return nil, domain.ErrWageRevisionNotFound
}

func (s *stubStore) GetWageRevisionHistory(_ context.Context, employeeID string) ([]domain.WageRevision, error) {
	var out []domain.WageRevision
	for _, r := range s.revisions {
		if r.EmployeeID == employeeID {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) CreateBonusGrant(_ context.Context, b *domain.BonusGrant) error {
	s.bonuses[b.GrantID] = b
	return nil
}

func (s *stubStore) ApproveBonusGrant(_ context.Context, grantID, approvedBy string) error {
	b, ok := s.bonuses[grantID]
	if !ok {
		return domain.ErrBonusNotFound
	}
	if b.Status != "PENDING" {
		return domain.ErrInvalidBonusStatus
	}
	b.Status = "APPROVED"
	b.ApprovedBy = &approvedBy
	return nil
}

func (s *stubStore) ListBonusGrants(_ context.Context, employeeID, status string) ([]domain.BonusGrant, error) {
	var out []domain.BonusGrant
	for _, b := range s.bonuses {
		if employeeID != "" && b.EmployeeID != employeeID {
			continue
		}
		if status != "" && b.Status != status {
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}

type stubPublisher struct {
	updated, bonusApproved, effectiveChanged int
}

func (p *stubPublisher) PublishCompensationUpdated(_ context.Context, _ string, _ domain.WageRevision) {
	p.updated++
}
func (p *stubPublisher) PublishBonusApproved(_ context.Context, _ string, _ domain.BonusGrant) {
	p.bonusApproved++
}
func (p *stubPublisher) PublishEffectiveChanged(_ context.Context, _ string, _ domain.WageRevision) {
	p.effectiveChanged++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeValidator struct{ err error }

func (v *stubEmployeeValidator) ValidateEmployee(_ context.Context, _, _, empID string) (*employee.Employee, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &employee.Employee{EmployeeID: empID, LegalEntityID: "le-us", Status: "ACTIVE"}, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, empValidator *stubEmployeeValidator) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, empValidator, zap.NewNop())
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

// ── CreateStructure Tests ──────────────────────────────────────────────────────

func TestCreateStructure_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/compensation/structures", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Eng Grade 5",
		"pay_type":        "SALARY",
		"min_amount":      80000.0,
		"max_amount":      130000.0,
		"currency":        "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateStructure_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/compensation/structures", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Eng Grade 5",
		"pay_type":        "SALARY",
		"min_amount":      80000.0,
		"max_amount":      130000.0,
		"currency":        "USD",
	}, "hr-admin")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var str domain.CompensationStructure
	_ = json.NewDecoder(rr.Body).Decode(&str)
	if str.Name != "Eng Grade 5" {
		t.Errorf("expected Eng Grade 5 got %q", str.Name)
	}
}

// ── ReviseWage Tests ───────────────────────────────────────────────────────────

func TestReviseWage_EffectiveDatedLineage(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Initial wage revision
	rrRev1 := doReq(r, http.MethodPost, "/v1/compensation/revisions", map[string]any{
		"employee_id":    "emp-101",
		"pay_type":       "SALARY",
		"amount":         90000.0,
		"currency":       "USD",
		"effective_from": "2024-01-01",
		"reason":         "Initial Hire Rate",
	}, "hr-admin")

	if rrRev1.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrRev1.Code, rrRev1.Body.String())
	}

	var rev1 domain.WageRevision
	_ = json.NewDecoder(rrRev1.Body).Decode(&rev1)

	// 2. Second wage revision (Annual Merit Increase)
	rrRev2 := doReq(r, http.MethodPost, "/v1/compensation/revisions", map[string]any{
		"employee_id":    "emp-101",
		"pay_type":       "SALARY",
		"amount":         110000.0,
		"currency":       "USD",
		"effective_from": "2024-07-01",
		"reason":         "Merit Promotion",
	}, "hr-admin")

	if rrRev2.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrRev2.Code, rrRev2.Body.String())
	}

	var rev2 domain.WageRevision
	_ = json.NewDecoder(rrRev2.Body).Decode(&rev2)

	if rev2.Amount != 110000.0 {
		t.Errorf("expected amount 110000 got %f", rev2.Amount)
	}
	if rev2.Status != "ACTIVE" {
		t.Errorf("expected v2 status ACTIVE got %q", rev2.Status)
	}

	// Verify v1 status was set to SUPERSEDED and effective_to updated
	oldRev, _ := s.revisions[rev1.RevisionID]
	if oldRev.Status != "SUPERSEDED" {
		t.Errorf("expected v1 status SUPERSEDED got %q", oldRev.Status)
	}

	if pub.updated != 2 || pub.effectiveChanged != 2 {
		t.Errorf("expected 2 updated & 2 effectiveChanged events got %d & %d", pub.updated, pub.effectiveChanged)
	}
}

// ── Bonus Grant & Approval Tests ───────────────────────────────────────────────

func TestGrantAndApproveBonus_Success(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Grant bonus
	rrGrant := doReq(r, http.MethodPost, "/v1/compensation/bonuses", map[string]any{
		"employee_id": "emp-102",
		"bonus_type":  "PERFORMANCE",
		"amount":      15000.0,
		"currency":    "USD",
		"grant_date":  "2024-12-15",
	}, "manager-1")

	if rrGrant.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrGrant.Code, rrGrant.Body.String())
	}

	var grant domain.BonusGrant
	_ = json.NewDecoder(rrGrant.Body).Decode(&grant)
	if grant.Status != "PENDING" {
		t.Errorf("expected PENDING got %q", grant.Status)
	}

	// 2. Approve bonus
	rrApprove := doReq(r, http.MethodPost, "/v1/compensation/bonuses/"+grant.GrantID+"/approve", map[string]any{
		"confirmation_note": "Approved by VP",
	}, "exec-1")

	if rrApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrApprove.Code, rrApprove.Body.String())
	}

	var approvedGrant domain.BonusGrant
	_ = json.NewDecoder(rrApprove.Body).Decode(&approvedGrant)
	if approvedGrant.Status != "APPROVED" {
		t.Errorf("expected APPROVED got %q", approvedGrant.Status)
	}

	if pub.bonusApproved != 1 {
		t.Errorf("expected 1 bonusApproved event got %d", pub.bonusApproved)
	}
}