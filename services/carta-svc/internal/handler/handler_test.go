package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/carta-svc/internal/domain"
	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/store"
)

func newRouter() http.Handler {
	return handler.NewRouter(handler.New(store.NewMemoryStore(), zap.NewNop()))
}

func TestHealthCheck(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestEvaluateAndListLifecycle(t *testing.T) {
	router := newRouter()

	// High Trust Evaluation -> ALLOW
	evalBody, _ := json.Marshal(domain.EvaluateRequest{
		LegalEntityID: "LE-100",
		Context: domain.AccessContext{
			SubjectID:           "USR-77",
			SubjectType:         "USER",
			DeviceTrustLevel:    95,
			IPAddress:           "10.0.0.15",
			IsKnownLocation:     true,
			ResourceSensitivity: "MEDIUM",
			ActionRequested:     "READ_FINANCIAL_REPORT",
			TimeOfDayHour:       14,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewBuffer(evalBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body)
	}

	var asm domain.CartaAssessment
	json.Unmarshal(w.Body.Bytes(), &asm)
	if asm.ID == "" {
		t.Fatal("expected assessment ID")
	}
	if asm.Decision != domain.DecisionAllow {
		t.Fatalf("expected ALLOW for high trust context, got %s", asm.Decision)
	}

	// Low Trust Evaluation -> ISOLATE/DENY
	untrustedBody, _ := json.Marshal(domain.EvaluateRequest{
		LegalEntityID: "LE-100",
		Context: domain.AccessContext{
			SubjectID:           "USR-99",
			SubjectType:         "USER",
			DeviceTrustLevel:    20,
			IPAddress:           "198.51.100.5",
			IsKnownLocation:     false,
			ResourceSensitivity: "RESTRICTED",
			ActionRequested:     "TRANSFER_TREASURY_FUNDS",
			TimeOfDayHour:       3,
		},
	})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewBuffer(untrustedBody))
	req2.Header.Set("X-Tenant-ID", "t1")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != 201 {
		t.Fatalf("expected 201, got %d", w2.Code)
	}
	var asm2 domain.CartaAssessment
	json.Unmarshal(w2.Body.Bytes(), &asm2)
	if asm2.Decision == domain.DecisionAllow {
		t.Fatal("untrusted context should NOT be ALLOW")
	}

	// List
	req3 := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?subject_id=USR-77", nil)
	req3.Header.Set("X-Tenant-ID", "t1")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("expected 200 on list, got %d", w3.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	evalBody, _ := json.Marshal(domain.EvaluateRequest{LegalEntityID: ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewBuffer(evalBody))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
