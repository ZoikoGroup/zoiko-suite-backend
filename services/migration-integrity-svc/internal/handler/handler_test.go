package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/migration-integrity-svc/internal/authz"
	"zoiko.io/migration-integrity-svc/internal/domain"
	"zoiko.io/migration-integrity-svc/internal/handler"
	"zoiko.io/migration-integrity-svc/internal/store"
)

// newMockAuthzServer stands in for authorization-svc during tests, always
// granting the requested action.
func newMockAuthzServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRouter(t *testing.T) http.Handler {
	return newRouterWithThreshold(t, 95)
}

// newRouterWithThreshold wires the handler with an explicit acceptance
// threshold (SPEC_DEVIATIONS A11).
func newRouterWithThreshold(t *testing.T, threshold float64) http.Handler {
	logger := zap.NewNop()
	s := store.NewMemoryStore()
	authzSrv := newMockAuthzServer(t)
	a := authz.NewClient(authzSrv.URL, logger)
	h := handler.NewHandler(s, a, logger, threshold)
	return handler.NewRouter(h)
}

func TestHealthCheck(t *testing.T) {
	router := newRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	withEnvelope(req)
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
	router := newRouter(t)

	// 1. Validate a mixed dataset — some valid, some violating
	validateReq := domain.ValidateMigrationRequest{
		LegalEntityID:    "LE-5005",
		MigrationName:    "Legacy ERP to Ledger Service Migration",
		SourceSystem:     "LEGACY_ERP",
		TargetService:    "ledger-svc",
		ExceptionOwnerID: "owner-1",
		RequiredFields:   []string{"employee_id", "amount", "cost_centre"},
		Records: []domain.MigrationRecord{
			// Duplicate ref (with the third record below — both copies are violations)
			{Ref: "REC-001", Fields: map[string]string{"employee_id": "EMP-1", "amount": "5000.00", "cost_centre": "CC-100"}},
			// Missing required field: cost_centre
			{Ref: "REC-002", Fields: map[string]string{"employee_id": "EMP-2", "amount": "3000.00"}},
			// Duplicate ref
			{Ref: "REC-001", Fields: map[string]string{"employee_id": "EMP-3", "amount": "1500.00", "cost_centre": "CC-101"}},
			// Bad numeric format
			{Ref: "REC-003", Fields: map[string]string{"employee_id": "EMP-4", "amount": "N/A", "cost_centre": "CC-102"}},
			// Valid record
			{Ref: "REC-004", Fields: map[string]string{"employee_id": "EMP-5", "amount": "250.00", "cost_centre": "CC-103"}},
		},
	}

	body, _ := json.Marshal(validateReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	req.Header.Set("X-Principal-Id", "user-1")
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
	if job.TotalRecordsCount != 5 {
		t.Fatalf("expected 5 total records, got %d", job.TotalRecordsCount)
	}
	// Both REC-001 copies, REC-002 (missing field) and REC-003 (bad number) fail; only REC-004 is valid.
	if job.InvalidRecordsCount != 4 || job.ValidRecordsCount != 1 {
		t.Fatalf("expected 4 invalid / 1 valid records, got %d / %d", job.InvalidRecordsCount, job.ValidRecordsCount)
	}
	if job.IntegrityScore != 20 {
		t.Fatalf("expected integrity score 20, got %f", job.IntegrityScore)
	}
	if len(job.IntegrityChecks) != 4 {
		t.Fatalf("expected 4 integrity checks (schema, identity, dup, format), got %d", len(job.IntegrityChecks))
	}
	if len(job.AuditEntries) == 0 {
		t.Fatal("expected audit entries for violations")
	}

	// 2. Get Job by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/migrations/"+job.ID, nil)
	withEnvelope(getReq)
	getReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get, got %d", getRec.Code)
	}

	// 3. List Jobs
	listReq := httptest.NewRequest(http.MethodGet, "/v1/migrations?legal_entity_id=LE-5005", nil)
	withEnvelope(listReq)
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

	// 4. Remediate an Audit Entry — with the required evidence
	if len(job.AuditEntries) > 0 {
		entryID := job.AuditEntries[0].ID
		remBody, _ := json.Marshal(domain.RemediateRequest{
			RemediationReason:      "corrected in source system before reload",
			RemediationEvidenceRef: "extract-run-2026-10-17/rec-002",
		})
		remReq := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+job.ID+"/audit/"+entryID+"/remediate", bytes.NewBuffer(remBody))
		withEnvelope(remReq)
		remReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
		remReq.Header.Set("X-Principal-Id", "user-1")
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
		if remEntry.RemediationReason != "corrected in source system before reload" {
			t.Fatalf("expected remediation_reason echoed, got %q", remEntry.RemediationReason)
		}
		if remEntry.RemediationEvidenceRef != "extract-run-2026-10-17/rec-002" {
			t.Fatalf("expected remediation_evidence_ref echoed, got %q", remEntry.RemediationEvidenceRef)
		}
		if remEntry.RemediatedBy != "user-1" {
			t.Fatalf("expected remediated_by = user-1, got %q", remEntry.RemediatedBy)
		}
		if remEntry.RemediatedAt == nil {
			t.Fatal("expected remediated_at to be set")
		}
	}

	// 5. Archive Job
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/migrations/"+job.ID, nil)
	withEnvelope(delReq)
	delReq.Header.Set("X-Tenant-ID", "tenant-mig-55")
	delReq.Header.Set("X-Principal-Id", "user-1")
	delRec := httptest.NewRecorder()
	router.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on archive, got %d", delRec.Code)
	}
}

func TestValidateMigrationCleanData(t *testing.T) {
	router := newRouter(t)

	cleanReq := domain.ValidateMigrationRequest{
		LegalEntityID:    "LE-5006",
		MigrationName:    "Payroll Clean Migration",
		SourceSystem:     "EXTERNAL_PAYROLL",
		TargetService:    "payroll-svc",
		ExceptionOwnerID: "owner-1",
		RequiredFields:   []string{"employee_id", "salary"},
		Records: []domain.MigrationRecord{
			{Ref: "PAY-001", Fields: map[string]string{"employee_id": "E-1", "salary": "50000.00"}},
			{Ref: "PAY-002", Fields: map[string]string{"employee_id": "E-2", "salary": "62000.00"}},
			{Ref: "PAY-003", Fields: map[string]string{"employee_id": "E-3", "salary": "45000.00"}},
		},
	}

	body, _ := json.Marshal(cleanReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	req.Header.Set("X-Principal-Id", "user-1")
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

func TestValidateMigrationEmptyRefReportedAsMissingIdentity(t *testing.T) {
	router := newRouter(t)

	validateReq := domain.ValidateMigrationRequest{
		LegalEntityID:    "LE-5007",
		MigrationName:    "Unidentified records",
		SourceSystem:     "LEGACY_ERP",
		TargetService:    "ledger-svc",
		ExceptionOwnerID: "owner-1",
		RequiredFields:   []string{"amount"},
		Records: []domain.MigrationRecord{
			{Ref: "", Fields: map[string]string{"amount": "10.00"}},
			{Ref: "  ", Fields: map[string]string{"amount": "20.00"}},
			{Ref: "REC-900", Fields: map[string]string{"amount": "30.00"}},
		},
	}
	body, _ := json.Marshal(validateReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if job.InvalidRecordsCount != 2 || job.ValidRecordsCount != 1 {
		t.Fatalf("expected 2 invalid / 1 valid, got %d / %d", job.InvalidRecordsCount, job.ValidRecordsCount)
	}

	var identity, dup int
	for _, e := range job.AuditEntries {
		switch e.ViolationType {
		case domain.ViolationMissingSourceIdentity:
			identity++
		case domain.ViolationDuplicate:
			dup++
		}
	}
	if identity != 2 {
		t.Fatalf("expected 2 MISSING_SOURCE_IDENTITY audit entries, got %d (%+v)", identity, job.AuditEntries)
	}
	if dup != 0 {
		t.Fatalf("blank refs must not be reported as duplicates, got %d DUPLICATE entries", dup)
	}
}

func TestValidationErrors(t *testing.T) {
	router := newRouter(t)

	// Missing legal_entity_id
	body, _ := json.Marshal(domain.ValidateMigrationRequest{MigrationName: "test"})
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-55")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
	}
}

// A 90%-scoring job is FAILED at a 95 bar and COMPLETED at 80; whichever bar
// was applied is exposed on the job record (SPEC_DEVIATIONS A11).
func TestValidateMigration_ThresholdDecidesStatus(t *testing.T) {
	records := make([]domain.MigrationRecord, 0, 10)
	for i := 0; i < 9; i++ {
		records = append(records, domain.MigrationRecord{
			Ref:    fmt.Sprintf("REC-%02d", i),
			Fields: map[string]string{"amount": "1.00"},
		})
	}
	records = append(records, domain.MigrationRecord{Ref: "REC-BAD", Fields: map[string]string{"amount": "N/A"}})

	for _, tc := range []struct {
		threshold  float64
		wantStatus domain.JobStatus
	}{
		{95, domain.JobStatusFailed},
		{80, domain.JobStatusCompleted},
	} {
		router := newRouterWithThreshold(t, tc.threshold)
		body, _ := json.Marshal(domain.ValidateMigrationRequest{
			LegalEntityID:    "LE-THRESHOLD",
			MigrationName:    "threshold acceptance fixture",
			SourceSystem:     "LEGACY_ERP",
			TargetService:    "general-ledger-svc",
			ExceptionOwnerID: "owner-1",
			RequiredFields:   []string{"amount"},
			Records:          records,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
		withEnvelope(req)
		req.Header.Set("X-Tenant-ID", "tenant-mig-threshold")
		req.Header.Set("X-Principal-Id", "user-1")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
		var job domain.MigrationJob
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatalf("decode job: %v", err)
		}
		if job.IntegrityScoreThreshold != tc.threshold {
			t.Fatalf("expected the applied threshold %v on the job record, got %v", tc.threshold, job.IntegrityScoreThreshold)
		}
		if job.Status != tc.wantStatus {
			t.Fatalf("score %.2f at threshold %v: expected %s, got %s", job.IntegrityScore, tc.threshold, tc.wantStatus, job.Status)
		}
	}
}
