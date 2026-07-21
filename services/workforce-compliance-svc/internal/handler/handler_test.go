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

	"zoiko.io/workforce-compliance-svc/internal/domain"
	"zoiko.io/workforce-compliance-svc/internal/employee"
	"zoiko.io/workforce-compliance-svc/internal/handler"
	"zoiko.io/workforce-compliance-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	workAuths map[string]*domain.WorkAuthorization
	visas     map[string]*domain.VisaRecord
	hourLogs  []domain.WorkingHourLog
	alerts    map[string]*domain.ComplianceAlert
}

func newStubStore() *stubStore {
	return &stubStore{
		workAuths: make(map[string]*domain.WorkAuthorization),
		visas:     make(map[string]*domain.VisaRecord),
		alerts:    make(map[string]*domain.ComplianceAlert),
	}
}

func (s *stubStore) CreateWorkAuth(_ context.Context, auth *domain.WorkAuthorization) error {
	auth.AuthID = "auth-1"
	s.workAuths[auth.EmployeeID] = auth
	return nil
}

func (s *stubStore) GetWorkAuth(_ context.Context, employeeID string) (*domain.WorkAuthorization, error) {
	auth, ok := s.workAuths[employeeID]
	if !ok {
		return nil, domain.ErrRecordNotFound
	}
	return auth, nil
}

func (s *stubStore) VerifyWorkAuth(_ context.Context, authID string, verifiedBy string) (*domain.WorkAuthorization, error) {
	for _, auth := range s.workAuths {
		if auth.AuthID == authID {
			auth.Status = domain.VerificationStatusVerified
			auth.VerifiedBy = &verifiedBy
			return auth, nil
		}
	}
	return nil, domain.ErrRecordNotFound
}

func (s *stubStore) CreateVisaRecord(_ context.Context, visa *domain.VisaRecord) error {
	visa.VisaID = "visa-1"
	s.visas[visa.VisaID] = visa
	s.visas[visa.EmployeeID] = visa
	return nil
}

func (s *stubStore) GetVisaRecord(_ context.Context, employeeID string) (*domain.VisaRecord, error) {
	visa, ok := s.visas[employeeID]
	if !ok {
		return nil, domain.ErrRecordNotFound
	}
	return visa, nil
}

func (s *stubStore) FlagVisaExpiration(_ context.Context, visaID string) (*domain.VisaRecord, error) {
	visa, ok := s.visas[visaID]
	if !ok {
		return nil, domain.ErrRecordNotFound
	}
	visa.FlaggedForExpiry = true
	return visa, nil
}

func (s *stubStore) LogWorkingHours(_ context.Context, log *domain.WorkingHourLog) error {
	log.LogID = "log-1"
	s.hourLogs = append(s.hourLogs, *log)
	return nil
}

func (s *stubStore) GetWeeklyHours(_ context.Context, employeeID string, startDate string) (float64, error) {
	var total float64
	for _, l := range s.hourLogs {
		if l.EmployeeID == employeeID {
			total += l.HoursWorked
		}
	}
	return total, nil
}

func (s *stubStore) CreateComplianceAlert(_ context.Context, alert *domain.ComplianceAlert) error {
	alert.AlertID = "alt-1"
	s.alerts[alert.AlertID] = alert
	return nil
}

func (s *stubStore) ListComplianceAlerts(_ context.Context, legalEntityID string) ([]domain.ComplianceAlert, error) {
	var out []domain.ComplianceAlert
	for _, a := range s.alerts {
		if legalEntityID != "" && a.LegalEntityID != legalEntityID {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

func (s *stubStore) ResolveComplianceAlert(_ context.Context, alertID string, resolvedBy string) error {
	a, ok := s.alerts[alertID]
	if ok {
		a.IsResolved = true
		a.ResolvedBy = &resolvedBy
	}
	return nil
}

type stubPublisher struct {
	authVerified, visaFlagged, hoursBreached, alertRaised int
}

func (p *stubPublisher) PublishWorkAuthVerified(_ context.Context, _ string, _ domain.WorkAuthorization) {
	p.authVerified++
}
func (p *stubPublisher) PublishVisaExpirationFlagged(_ context.Context, _ string, _ domain.VisaRecord) {
	p.visaFlagged++
}
func (p *stubPublisher) PublishWorkingHoursBreach(_ context.Context, _ string, _ domain.WorkingHourLog) {
	p.hoursBreached++
}
func (p *stubPublisher) PublishComplianceAlertRaised(_ context.Context, _ string, _ domain.ComplianceAlert) {
	p.alertRaised++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeValidator struct{ err error }

func (v *stubEmployeeValidator) ValidateEmployee(_ context.Context, _, legalEntityID, empID string) (*employee.Employee, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &employee.Employee{EmployeeID: empID, LegalEntityID: legalEntityID, Status: "ACTIVE"}, nil
}

type stubJurisdiction struct{}

func (j *stubJurisdiction) GetWorkingHourLimit(_ context.Context, _ string) (float64, error) {
	return 40.0, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, empValidator *stubEmployeeValidator, jRules *stubJurisdiction) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, empValidator, jRules, zap.NewNop())
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

// ── Tests ──────────────────────────────────────────────────────────────────────

func TestWorkAuth_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{}, &stubJurisdiction{})
	rr := doReq(r, http.MethodPost, "/v1/compliance/work-auth", map[string]any{
		"legal_entity_id": "le-us",
		"employee_id":     "emp-101",
		"document_type":   "I-9",
		"document_number": "DOC-123",
		"issue_date":      "2024-01-01",
		"effective_from":  "2024-01-01",
	}, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestWorkforceComplianceLifecycle(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{}, &stubJurisdiction{})

	// 1. Create Work Auth
	rrAuth := doReq(r, http.MethodPost, "/v1/compliance/work-auth", map[string]any{
		"legal_entity_id": "le-us",
		"employee_id":     "emp-101",
		"document_type":   "I-9",
		"document_number": "DOC-999",
		"issue_date":      "2024-01-01",
		"effective_from":  "2024-01-01",
	}, "hr-admin")

	if rrAuth.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrAuth.Code, rrAuth.Body.String())
	}
	var auth domain.WorkAuthorization
	_ = json.NewDecoder(rrAuth.Body).Decode(&auth)

	// 2. Verify Work Auth
	rrVerify := doReq(r, http.MethodPost, "/v1/compliance/work-auth/"+auth.AuthID+"/verify", nil, "hr-admin")
	if rrVerify.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrVerify.Code, rrVerify.Body.String())
	}
	if pub.authVerified != 1 {
		t.Errorf("expected 1 authVerified event got %d", pub.authVerified)
	}

	// 3. Create Visa Record & Flag Expiry
	rrVisa := doReq(r, http.MethodPost, "/v1/compliance/visas", map[string]any{
		"legal_entity_id": "le-us",
		"employee_id":     "emp-101",
		"visa_type":       "H1B",
		"issuing_country": "USA",
		"expiration_date": "2024-12-31",
	}, "hr-admin")
	if rrVisa.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrVisa.Code, rrVisa.Body.String())
	}
	var visa domain.VisaRecord
	_ = json.NewDecoder(rrVisa.Body).Decode(&visa)

	rrFlag := doReq(r, http.MethodPost, "/v1/compliance/visas/"+visa.VisaID+"/flag-expiry", nil, "hr-admin")
	if rrFlag.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrFlag.Code, rrFlag.Body.String())
	}
	if pub.visaFlagged != 1 {
		t.Errorf("expected 1 visaFlagged event got %d", pub.visaFlagged)
	}

	// 4. Log Hours (45 hours > 40 hour limit -> breach)
	rrHours := doReq(r, http.MethodPost, "/v1/compliance/hours", map[string]any{
		"legal_entity_id": "le-us",
		"employee_id":     "emp-101",
		"work_date":       "2024-06-01",
		"hours_worked":    45.0,
		"overtime_hours":  5.0,
	}, "hr-admin")
	if rrHours.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrHours.Code, rrHours.Body.String())
	}
	var hourLog domain.WorkingHourLog
	_ = json.NewDecoder(rrHours.Body).Decode(&hourLog)
	if !hourLog.IsBreached {
		t.Errorf("expected is_breached to be true for 45 hours vs 40 max")
	}
	if pub.hoursBreached != 1 {
		t.Errorf("expected 1 hoursBreached event got %d", pub.hoursBreached)
	}
}
