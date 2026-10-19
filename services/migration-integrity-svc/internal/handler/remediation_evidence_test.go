package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// Remediation evidence tests (SPEC_DEVIATIONS A5): the remediate route used
// to accept a free-text note and discard it, so a remediation was a boolean
// flip with no reason, no evidence and no record of who decided it. A reason
// and an evidence reference are now required, and by/when are recorded.

// createJobWithViolation validates one violating record through the router
// and returns the created job.
func createJobWithViolation(t *testing.T, router http.Handler) domain.MigrationJob {
	t.Helper()
	validateReq := domain.ValidateMigrationRequest{
		LegalEntityID:  "LE-6001",
		MigrationName:  "Remediation evidence test",
		SourceSystem:   "LEGACY_ERP",
		TargetService:  "ledger-svc",
		RequiredFields: []string{"amount"},
		Records: []domain.MigrationRecord{
			{Ref: "REC-100", Fields: map[string]string{"amount": "N/A"}},
		},
	}
	body, _ := json.Marshal(validateReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-66")
	req.Header.Set("X-Principal-Id", "user-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if len(job.AuditEntries) == 0 {
		t.Fatal("setup: expected an audit entry")
	}
	return job
}

func remediate(t *testing.T, router http.Handler, jobID, entryID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+jobID+"/audit/"+entryID+"/remediate", bytes.NewBufferString(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-mig-66")
	req.Header.Set("X-Principal-Id", "user-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRemediateEntry_MissingEvidenceRefused(t *testing.T) {
	router := newRouter(t)
	job := createJobWithViolation(t, router)
	entryID := job.AuditEntries[0].ID

	for _, tc := range []struct {
		name, body string
	}{
		{"no fields at all", `{}`},
		{"reason only", `{"remediation_reason":"fixed"}`},
		{"evidence only", `{"remediation_evidence_ref":"ticket-1"}`},
		{"blank reason", `{"remediation_reason":"  ","remediation_evidence_ref":"ticket-1"}`},
		{"blank evidence", `{"remediation_reason":"fixed","remediation_evidence_ref":" "}`},
		{"legacy notes only", `{"notes":"Corrected in source system"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := remediate(t, router, job.ID, entryID, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 without reason+evidence, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRemediateEntry_RecordsEvidence(t *testing.T) {
	router := newRouter(t)
	job := createJobWithViolation(t, router)
	entryID := job.AuditEntries[0].ID

	rec := remediate(t, router, job.ID, entryID,
		`{"remediation_reason":"source corrected and reloaded","remediation_evidence_ref":"ticket-MIG-42"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var entry domain.AuditEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &entry)
	if !entry.IsRemediated {
		t.Fatal("expected is_remediated true")
	}
	if entry.RemediationReason != "source corrected and reloaded" || entry.RemediationEvidenceRef != "ticket-MIG-42" {
		t.Fatalf("expected evidence recorded on the entry, got %+v", entry)
	}
	if entry.RemediatedBy != "user-1" || entry.RemediatedAt == nil {
		t.Fatalf("expected remediated_by/remediated_at recorded, got %+v", entry)
	}
}
