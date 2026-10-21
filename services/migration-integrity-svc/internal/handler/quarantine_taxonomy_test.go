package handler_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// Quarantine taxonomy tests (SPEC_DEVIATIONS A6): entries carry a §16
// severity, reason family and owner; an MQ0 blocker fails the job at any
// score and can only be resolved by its owner.

func postValidate(t *testing.T, router http.Handler, req domain.ValidateMigrationRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/migrations/validate", bytes.NewBuffer(body))
	withEnvelope(r)
	r.Header.Set("X-Tenant-ID", "tenant-mig-a6")
	r.Header.Set("X-Principal-Id", "submitter-1")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func remediateAs(t *testing.T, router http.Handler, principal, jobID, entryID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/migrations/"+jobID+"/audit/"+entryID+"/remediate",
		bytes.NewBufferString(`{"remediation_reason":"source corrected","remediation_evidence_ref":"ticket-77"}`))
	withEnvelope(r)
	r.Header.Set("X-Tenant-ID", "tenant-mig-a6")
	r.Header.Set("X-Principal-Id", principal)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

// 99 identified records and one with no source identity: score 99 clears a
// 95 bar, and even a zero bar, yet the MQ0 blocker fails the job.
func blockerRun(owner string) domain.ValidateMigrationRequest {
	records := []domain.MigrationRecord{{Ref: "", Fields: map[string]string{"amount": "1"}}}
	for i := 0; i < 99; i++ {
		records = append(records, domain.MigrationRecord{Ref: fmt.Sprintf("REC-%03d", i), Fields: map[string]string{"amount": "1"}})
	}
	return domain.ValidateMigrationRequest{
		LegalEntityID: "LE-A6", MigrationName: "blocker", SourceSystem: "LEGACY_ERP", TargetService: "general-ledger-svc",
		ExceptionOwnerID: owner, RequiredFields: []string{"amount"}, Records: records,
	}
}

func TestValidate_MQ0BlockerFailsJobAboveThreshold(t *testing.T) {
	for _, threshold := range []float64{95, 0} {
		rec := postValidate(t, newRouterWithThreshold(t, threshold), blockerRun("owner-1"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
		var job domain.MigrationJob
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job.IntegrityScore != 99 {
			t.Fatalf("fixture: expected score 99, got %v", job.IntegrityScore)
		}
		if job.BlockerCount != 1 {
			t.Fatalf("expected blocker_count 1, got %d", job.BlockerCount)
		}
		if job.Status != domain.JobStatusFailed {
			t.Fatalf("bar %v: an MQ0 blocker must fail the job at score 99, got %s", threshold, job.Status)
		}
		if len(job.AuditEntries) != 1 || job.AuditEntries[0].Severity != domain.SeverityMQ0 ||
			job.AuditEntries[0].ReasonFamily != domain.ReasonIdentity || job.AuditEntries[0].OwnerID != "owner-1" {
			t.Fatalf("unexpected audit entries: %+v", job.AuditEntries)
		}
	}
}

func TestValidate_NonBlockingRunStillDecidedByThreshold(t *testing.T) {
	req := blockerRun("owner-1")
	req.Records[0].Ref = "REC-X"
	req.Records[0].Fields["amount"] = "N/A" // MQ1, not a blocker
	rec := postValidate(t, newRouterWithThreshold(t, 95), req)
	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if job.BlockerCount != 0 || job.Status != domain.JobStatusCompleted {
		t.Fatalf("an MQ1 at score 99 is left to the bar: got blockers=%d status=%s", job.BlockerCount, job.Status)
	}
}

func TestValidate_ExceptionOwnerRequired(t *testing.T) {
	rec := postValidate(t, newRouter(t), blockerRun(""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without exception_owner_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRemediate_MQ0OnlyByOwner(t *testing.T) {
	router := newRouter(t)
	rec := postValidate(t, router, blockerRun("owner-1"))
	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	entryID := job.AuditEntries[0].ID

	// The submitter holds AUDIT_ENTRY_REMEDIATE (the mock grants everything)
	// but is not the owner.
	if r := remediateAs(t, router, "submitter-1", job.ID, entryID); r.Code != http.StatusForbidden {
		t.Fatalf("non-owner resolving an MQ0: expected 403, got %d: %s", r.Code, r.Body.String())
	}
	r := remediateAs(t, router, "owner-1", job.ID, entryID)
	if r.Code != http.StatusOK {
		t.Fatalf("owner resolving an MQ0: expected 200, got %d: %s", r.Code, r.Body.String())
	}
	var e domain.AuditEntry
	_ = json.Unmarshal(r.Body.Bytes(), &e)
	if !e.IsRemediated || e.RemediatedBy != "owner-1" {
		t.Fatalf("expected the owner's resolution recorded, got %+v", e)
	}
}

// Below MQ0 the owner rule does not apply: the action check decides.
func TestRemediate_NonBlockerByAnyAuthorizedPrincipal(t *testing.T) {
	router := newRouter(t)
	req := blockerRun("owner-1")
	req.Records[0].Ref = "REC-X"
	req.Records[0].Fields["amount"] = "N/A"
	rec := postValidate(t, router, req)
	var job domain.MigrationJob
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if r := remediateAs(t, router, "submitter-1", job.ID, job.AuditEntries[0].ID); r.Code != http.StatusOK {
		t.Fatalf("authorized non-owner resolving an MQ1: expected 200, got %d: %s", r.Code, r.Body.String())
	}
}
