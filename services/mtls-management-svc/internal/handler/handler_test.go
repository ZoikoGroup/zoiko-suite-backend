package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/mtls-management-svc/internal/domain"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/store"
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

func TestProvisionRotateRevoke(t *testing.T) {
	router := newRouter()
	// Provision
	body, _ := json.Marshal(domain.ProvisionCertRequest{LegalEntityID: "LE-1", ServiceName: "ledger-svc", CommonName: "ledger-svc.zoiko.internal", RotationDays: 90, AutoRotate: true})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body)
	}
	var cert domain.MtlsCertificate
	json.Unmarshal(w.Body.Bytes(), &cert)
	if cert.ID == "" {
		t.Fatal("expected cert ID")
	}
	if cert.Status != domain.CertStatusActive {
		t.Fatalf("expected ACTIVE, got %s", cert.Status)
	}

	// Get
	req2 := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates/"+cert.ID, nil)
	req2.Header.Set("X-Tenant-ID", "t1")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("expected 200 on get, got %d", w2.Code)
	}

	// List
	req3 := httptest.NewRequest(http.MethodGet, "/v1/mtls/certificates?legal_entity_id=LE-1", nil)
	req3.Header.Set("X-Tenant-ID", "t1")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != 200 {
		t.Fatalf("expected 200 on list, got %d", w3.Code)
	}
	var listResp map[string]interface{}
	json.Unmarshal(w3.Body.Bytes(), &listResp)
	if int(listResp["count"].(float64)) < 1 {
		t.Fatal("expected at least 1 cert")
	}

	// Rotate
	req4 := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates/"+cert.ID+"/rotate", nil)
	req4.Header.Set("X-Tenant-ID", "t1")
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != 200 {
		t.Fatalf("expected 200 on rotate, got %d", w4.Code)
	}
	var rotated domain.MtlsCertificate
	json.Unmarshal(w4.Body.Bytes(), &rotated)
	if rotated.Fingerprint == cert.Fingerprint {
		t.Fatal("fingerprint should change after rotation")
	}

	// Policy
	polBody, _ := json.Marshal(domain.CreatePolicyRequest{PolicyName: "ledger-to-treasury", SourceService: "ledger-svc", TargetService: "treasury-svc", Action: domain.PolicyAllow, RequiresMtls: true})
	req5 := httptest.NewRequest(http.MethodPost, "/v1/mtls/policies", bytes.NewBuffer(polBody))
	req5.Header.Set("X-Tenant-ID", "t1")
	w5 := httptest.NewRecorder()
	router.ServeHTTP(w5, req5)
	if w5.Code != 201 {
		t.Fatalf("expected 201 on policy create, got %d", w5.Code)
	}

	// Revoke
	req6 := httptest.NewRequest(http.MethodDelete, "/v1/mtls/certificates/"+cert.ID, nil)
	req6.Header.Set("X-Tenant-ID", "t1")
	w6 := httptest.NewRecorder()
	router.ServeHTTP(w6, req6)
	if w6.Code != 200 {
		t.Fatalf("expected 200 on revoke, got %d", w6.Code)
	}
}

func TestValidationError(t *testing.T) {
	body, _ := json.Marshal(domain.ProvisionCertRequest{ServiceName: "svc"})
	req := httptest.NewRequest(http.MethodPost, "/v1/mtls/certificates", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
