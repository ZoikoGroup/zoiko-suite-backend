package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// A human decision on a recommendation follows RECOMMENDED -> APPROVED|REJECTED,
// APPROVED -> EXECUTED; anything else is refused and the earlier decision stands.
func TestApplyResolution_EnforcesDecisionPath(t *testing.T) {
	router := setupTestRouter(t)

	body, _ := json.Marshal(domain.AnalyzeReconciliationRequest{
		LegalEntityID: "LE-1", JobName: "decisions", SourceSystemA: domain.SourceGeneralLedger,
		SourceSystemB: domain.SourceBankStatement,
		TransactionsA: []domain.TransactionItem{{RefID: "TX-1", Amount: 75}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewBuffer(body))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("X-Principal-Id", "principal-01")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body.String())
	}
	var job domain.ReconciliationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || len(job.UnmatchedItems) != 1 {
		t.Fatalf("analyze body: %v %s", err, rec.Body.String())
	}

	apply := func(status domain.ResolutionStatus) *httptest.ResponseRecorder {
		b, _ := json.Marshal(domain.ApplyResolutionRequest{ResolutionStatus: status, ResolutionNotes: "decision"})
		r := httptest.NewRequest(http.MethodPost,
			"/v1/reconciliations/"+job.ID+"/resolutions/"+job.UnmatchedItems[0].ID+"/apply", bytes.NewBuffer(b))
		withEnvelope(r)
		r.Header.Set("X-Tenant-ID", "tenant-1")
		r.Header.Set("X-Principal-Id", "principal-01")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	if w := apply("WRITTEN_OFF"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: got %d, want 400", w.Code)
	}
	if w := apply(domain.StatusRecommended); w.Code != http.StatusBadRequest {
		t.Fatalf("a human cannot set RECOMMENDED: got %d, want 400", w.Code)
	}
	if w := apply(domain.StatusExecuted); w.Code != http.StatusConflict {
		t.Fatalf("EXECUTED without APPROVED: got %d, want 409", w.Code)
	}
	if w := apply(domain.StatusApproved); w.Code != http.StatusOK {
		t.Fatalf("approve: got %d %s", w.Code, w.Body.String())
	}
	if w := apply(domain.StatusRejected); w.Code != http.StatusConflict {
		t.Fatalf("overwriting an approval with a rejection: got %d, want 409", w.Code)
	}
	if w := apply(domain.StatusExecuted); w.Code != http.StatusOK {
		t.Fatalf("execute after approval: got %d %s", w.Code, w.Body.String())
	}
	if w := apply(domain.StatusExecuted); w.Code != http.StatusConflict {
		t.Fatalf("EXECUTED is final: got %d, want 409", w.Code)
	}
}
