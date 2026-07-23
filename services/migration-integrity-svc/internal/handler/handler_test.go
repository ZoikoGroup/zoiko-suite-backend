package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/events"
	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/store"
)

func newRouter() http.Handler {
	logger := zap.NewNop()
	s := store.NewMemoryStore()
	p := events.NewPublisher([]string{"localhost:9092"}, "zoiko.migration-integrity.events", logger)
	a := authz.NewClient("http://localhost:8089", logger)
	h := handler.NewHandler(s, p, a, logger)
	return handler.NewRouter(h)
}

func TestHealthCheck(t *testing.T) {
	router := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "ok" || resp["service"] != "migration-integrity-svc" {
		t.Fatalf("unexpected body: %v", resp)
	}
}

func TestValidateMigrationAndLifecycle(t *testing.T) {
	router := newRouter()

	// 1. Validate a mixed dataset — some valid, some violating
	validateReq := domain.ValidateMigrationRequest{
		LegalEntityID: "LE-5005",
		MigrationName: "Legacy ERP to Ledger Service Migration",
		SourceSystem:  "LEGACY_ERP",
		TargetService: "ledger-svc",
		RequiredFields: []string{"employee_id", "amount", "cost_centre"},
		Records: []domain.MigrationRecord{
			// Valid record
			{Ref: "REC-001", Fields: map[string]string{"employee_id": "EMP-1", "amount": "5000.00", "cost_centre": "CC-100"}},
			// Missing required field: cost_centre
			{Ref: "REC-002", Fields: map[string]string{"employee_id": "EMP-2", "amount": "3000.00"}},
			// Duplicate ref
			{Ref: "REC-001", Fields: map[string]string{"employee_id": "EMP-3", "amount": "1500.00", "cost_centre": "CC-101"}},
			// Bad numeric format
			{Ref: "REC-003", Fields: map[string]string{"employee_id": "EMP-4", "amount": "N/A", "cost_centre": "CC-102"}},
		},
	}

	body, _ := json.Marshal(validateReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var job domain.MigrationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal job error: %v", err)
	}

	if job.ID == "" {
		t.Fatal("expected non-empty job ID")
	}
	if job.TotalRecordsCount != 4 {
		t.Fatalf("expected 4 total records, got %d", job.TotalRecordsCount)
	}
	if job.InvalidRecordsCount == 0 {
		t.Fatal("expected some invalid records due to violations")
	}
	if job.IntegrityScore <= 0 || job.IntegrityScore > 100 {
		t.Fatalf("expected integrity score in (0,100], got %f", job.IntegrityScore)
	}
	if len(job.IntegrityChecks) != 3 {
		t.Fatalf("expected 3 integrity checks (schema, dup, format), got %d", len(job.IntegrityChecks))
	}
	if len(job.AuditEntries) == 0 {
		t.Fatal("expected audit entries for violations")
	}

	// 2. Get Job by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+job.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get, got %d", getRec.Code)
	}

	// 3. List Jobs
	listReq := httptest.NewRequest(http.MethodGet, "/v1/migrations?legal_entity_id=LE-5005", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on list, got %d", listRec.Code)
	}
	var listResp map[string]interface{}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listResp)
	if int(listResp["count"].(float64)) < 1 {
		t.Fatal("expected at least 1 job in list")
	}

	// 4. Remediate an Audit Entry
	if len(job.AuditEntries) > 0 {
		entryID := job.AuditEntries[0].ID
		remBody, _ := json.Marshal(domain.RemediateRequest{Notes: "Corrected in source system"})
		remReq := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+job.ID+"/audit/"+entryID+"/remediate", bytes.NewBuffer(remBody))
		remReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
		remReq.Header.Set("Content-Type", "application/json")
		remRec := httptest.NewRecorder()
		router.ServeHTTP(remRec, remReq)
		if remRec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on remediate, got %d: %s", remRec.Code, remRec.Body.String())
		}
		var remEntry domain.AuditEntry
		_ = json.Unmarshal(remRec.Body.Bytes(), &remEntry)
		if !remEntry.IsRemediated {
			t.Fatal("expected is_remediated = true after remediation")
		}
	}

	// 5. Archive Job
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/migrations/"+job.ID, nil)
	delReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
	delRec := httptest.NewRecorder()
	router.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on archive, got %d", delRec.Code)
	}
}

func TestValidateMigrationCleanData(t *testing.T) {
	router := newRouter()

	cleanReq := domain.ValidateMigrationRequest{
		LegalEntityID:  "LE-5006",
		MigrationName:  "Payroll Clean Migration",
		SourceSystem:   "EXTERNAL_PAYROLL",
		TargetService:  "payroll-svc",
		RequiredFields: []string{"employee_id", "salary"},
		Records: []domain.MigrationRecord{
			{Ref: "PAY-001", Fields: map[string]string{"employee_id": "E-1", "salary": "50000.00"}},
			{Ref: "PAY-002", Fields: map[string]string{"employee_id": "E-2", "salary": "62000.00"}},
			{Ref: "PAY-003", Fields: map[string]string{"employee_id": "E-3", "salary": "45000.00"}},
		},
	}

	body, _ := json.Marshal(cleanReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)

	if job.IntegrityScore != 100.0 {
		t.Fatalf("expected integrity score 100.0 for clean data, got %f", job.IntegrityScore)
	}
	if job.InvalidRecordsCount != 0 {
		t.Fatalf("expected 0 invalid records for clean data, got %d", job.InvalidRecordsCount)
	}
	if job.Status != domain.JobStatusCompleted {
		t.Fatalf("expected status COMPLETED, got %s", job.Status)
	}
}

func TestValidationErrors(t *testing.T) {
	router := newRouter()

	// Missing legal_entity_id
	body, _ := json.Marshal(domain.ValidateMigrationRequest{MigrationName: "test"})
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
	}
}
