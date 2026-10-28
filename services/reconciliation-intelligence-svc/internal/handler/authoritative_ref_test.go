package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
)

// A5: a WRITE_OFF is an accounting action, so marking it EXECUTED needs a
// structured reference to the general-ledger-svc journal that posted it. A
// note is not one. A malformed reference, or one on a decision other than
// EXECUTED, is 400, and the earlier decision stands.
func TestApplyResolution_ExecutedWriteOffNeedsJournalRef(t *testing.T) {
	router := setupTestRouter(t)

	// runSmall: one UNMATCHED 5.00 line, below the 10 tolerance, so WRITE_OFF.
	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody("le-1", runSmall)))
	withEnvelope(req)
	req.Header.Set("X-Tenant-ID", "tenant-a5")
	req.Header.Set("X-Principal-Id", "principal-01")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var job domain.ReconciliationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || rec.Code != http.StatusCreated || len(job.UnmatchedItems) != 1 ||
		job.UnmatchedItems[0].Recommendation != domain.RecommendationWriteOff {
		t.Fatalf("analyze: %d %s", rec.Code, rec.Body.String())
	}

	apply := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost,
			"/v1/reconciliations/"+job.ID+"/resolutions/"+job.UnmatchedItems[0].ID+"/apply", bytes.NewBufferString(body))
		withEnvelope(r)
		r.Header.Set("X-Tenant-ID", "tenant-a5")
		r.Header.Set("X-Principal-Id", "principal-01")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	const journal = "5b0f6a3e-2c1d-4e8f-9a7b-6c5d4e3f2a1b"
	ref := `"authoritative_ref":{"kind":"GL_JOURNAL","journal_id":"` + journal + `"}`

	if w := apply(`{"suggestion_status":"APPROVED",` + ref + `}`); w.Code != http.StatusBadRequest {
		t.Fatalf("reference on APPROVED: %d %s, want 400", w.Code, w.Body)
	}
	if w := apply(`{"suggestion_status":"APPROVED"}`); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	for name, body := range map[string]string{
		"no reference":            `{"suggestion_status":"EXECUTED","resolution_notes":"posted JE-1042"}`,
		"journal id is free text": `{"suggestion_status":"EXECUTED","authoritative_ref":{"kind":"GL_JOURNAL","journal_id":"JE-1042"}}`,
		"unknown kind":            `{"suggestion_status":"EXECUTED","authoritative_ref":{"kind":"ADJUSTMENT","journal_id":"` + journal + `"}}`,
	} {
		if w := apply(body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body)
		}
	}

	w := apply(`{"suggestion_status":"EXECUTED",` + ref + `}`)
	var item domain.ReconciliationSuggestion
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil || w.Code != http.StatusOK {
		t.Fatalf("execute with journal: %d %s", w.Code, w.Body)
	}
	if item.SuggestionStatus != domain.StatusExecuted || item.AuthoritativeRef == nil ||
		item.AuthoritativeRef.Kind != domain.AuthoritativeRefGLJournal || item.AuthoritativeRef.JournalID != journal {
		t.Fatalf("executed item: %+v", item)
	}
}
