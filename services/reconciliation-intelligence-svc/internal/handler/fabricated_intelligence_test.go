package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// TestApplyResolution_EmptyHumanNote_PreservesMachineRationale is the
// end-to-end regression test for a second-order defect found while fixing
// the "fabricated intelligence" issue (analysis produced an opaque numeric
// confidence score with no visible reasoning).
//
// The straightforward fix — record the heuristic's rationale in
// ResolutionNotes when the item is generated — had a bug of its own:
// ApplyResolution unconditionally overwrote ResolutionNotes with the
// human's input. A human approving without typing a note (the common case)
// would silently erase the only record of why the system made the
// recommendation, at exactly the moment the decision was made. Fixed by
// appending rather than overwriting; this test proves that end-to-end
// through the real HTTP handler and MemoryStore, not just the domain
// function in isolation.
func TestApplyResolution_EmptyHumanNote_PreservesMachineRationale(t *testing.T) {
	router := setupTestRouter(t)

	analyzeBody, _ := json.Marshal(domain.AnalyzeReconciliationRequest{
		LegalEntityID: "le-1", JobName: "job", SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{{RefID: "r1", Amount: 100.00}},
		TransactionsB: []domain.TransactionItem{{RefID: "r1", Amount: 132.00}}, // $32 discrepancy, below $50 tolerance
	})
	analyzeReq := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody))
	analyzeReq.Header.Set("Content-Type", "application/json")
	analyzeReq.Header.Set("X-Tenant-ID", "tenant-rec-fab")
	analyzeReq.Header.Set("X-Principal-Id", "principal-01")
	analyzeRec := httptest.NewRecorder()
	router.ServeHTTP(analyzeRec, analyzeReq)
	if analyzeRec.Code != http.StatusOK && analyzeRec.Code != http.StatusCreated {
		t.Fatalf("analyze: expected 200/201, got %d: %s", analyzeRec.Code, analyzeRec.Body.String())
	}

	var job domain.ReconciliationJob
	if err := json.Unmarshal(analyzeRec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if len(job.UnmatchedItems) != 1 {
		t.Fatalf("expected 1 unmatched item, got %d", len(job.UnmatchedItems))
	}
	itemID := job.UnmatchedItems[0].ID
	if !strings.Contains(job.UnmatchedItems[0].ResolutionNotes, "heuristic rule") {
		t.Fatalf("expected machine rationale on generation, got %q", job.UnmatchedItems[0].ResolutionNotes)
	}

	// Approve with NO note — the common case, and the one that used to erase
	// the rationale.
	applyBody, _ := json.Marshal(domain.ApplyResolutionRequest{
		ResolutionStatus: domain.StatusApproved, ResolutionNotes: "",
	})
	applyReq := httptest.NewRequest(http.MethodPost,
		"/v1/reconciliations/"+job.ID+"/resolutions/"+itemID+"/apply", bytes.NewReader(applyBody))
	applyReq.Header.Set("Content-Type", "application/json")
	applyReq.Header.Set("X-Tenant-ID", "tenant-rec-fab")
	applyReq.Header.Set("X-Principal-Id", "principal-02")
	applyRec := httptest.NewRecorder()
	router.ServeHTTP(applyRec, applyReq)
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d: %s", applyRec.Code, applyRec.Body.String())
	}

	var applied domain.UnmatchedItem
	if err := json.Unmarshal(applyRec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode applied item: %v", err)
	}
	if applied.ResolutionStatus != domain.StatusApproved {
		t.Fatalf("expected APPROVED, got %s", applied.ResolutionStatus)
	}
	if !strings.Contains(applied.ResolutionNotes, "heuristic rule") {
		t.Fatalf("REGRESSION: an empty-note approval erased the machine's rationale, got %q", applied.ResolutionNotes)
	}
}

// TestApplyResolution_HumanNote_AppendedNotOverwritten covers the other
// direction: when a human DOES supply a note, both the machine's rationale
// and the human's own reasoning must survive, since a real audit trail
// needs both — why the system proposed it, and why the human agreed.
func TestApplyResolution_HumanNote_AppendedNotOverwritten(t *testing.T) {
	router := setupTestRouter(t)

	analyzeBody, _ := json.Marshal(domain.AnalyzeReconciliationRequest{
		LegalEntityID: "le-1", JobName: "job", SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{{RefID: "orphan", Amount: 5.00}},
	})
	analyzeReq := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody))
	analyzeReq.Header.Set("Content-Type", "application/json")
	analyzeReq.Header.Set("X-Tenant-ID", "tenant-rec-fab2")
	analyzeReq.Header.Set("X-Principal-Id", "principal-01")
	analyzeRec := httptest.NewRecorder()
	router.ServeHTTP(analyzeRec, analyzeReq)

	var job domain.ReconciliationJob
	_ = json.Unmarshal(analyzeRec.Body.Bytes(), &job)
	itemID := job.UnmatchedItems[0].ID

	applyBody, _ := json.Marshal(domain.ApplyResolutionRequest{
		ResolutionStatus: domain.StatusApproved, ResolutionNotes: "confirmed with controller, approved",
	})
	applyReq := httptest.NewRequest(http.MethodPost,
		"/v1/reconciliations/"+job.ID+"/resolutions/"+itemID+"/apply", bytes.NewReader(applyBody))
	applyReq.Header.Set("Content-Type", "application/json")
	applyReq.Header.Set("X-Tenant-ID", "tenant-rec-fab2")
	applyReq.Header.Set("X-Principal-Id", "principal-02")
	applyRec := httptest.NewRecorder()
	router.ServeHTTP(applyRec, applyReq)

	var applied domain.UnmatchedItem
	if err := json.Unmarshal(applyRec.Body.Bytes(), &applied); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(applied.ResolutionNotes, "heuristic rule") {
		t.Fatalf("expected machine rationale preserved alongside human note, got %q", applied.ResolutionNotes)
	}
	if !strings.Contains(applied.ResolutionNotes, "confirmed with controller") {
		t.Fatalf("expected human's own note present, got %q", applied.ResolutionNotes)
	}
}
