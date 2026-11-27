package handler_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/reconciliation-intelligence-svc/internal/envelope"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

// Analyze: the body legal_entity_id may only repeat X-Legal-Entity-Id. A
// caller scoped to one entity must not analyze (and persist a job for)
// another — refused 403 before authorization-svc or any source is asked.
func TestAnalyze_BodyLegalEntityMustMatchHeader(t *testing.T) {
	az := &recordingAuthz{}
	st := store.NewMemoryStore()
	router := newTestRouter(t, deps{authz: az, store: st})

	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody("LE-3003", runLE3003)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Legal-Entity-Id", "LE-OTHER")
	withEnvelope(req)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "legal_entity_mismatch") {
		t.Fatalf("expected 403 legal_entity_mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := az.asked("RECONCILIATION_ANALYZE"); len(got) != 0 {
		t.Fatalf("a scope mismatch must be refused before authorization: %v", got)
	}

	// Matching header: accepted (201) and authorized against that entity.
	if rec := postAnalyze(router, "LE-3003", runLE3003); rec.Code != http.StatusCreated {
		t.Fatalf("matching header: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := az.asked("RECONCILIATION_ANALYZE"); len(got) != 1 || got[0].LegalEntityID != "LE-3003" {
		t.Fatalf("authz: %v", got)
	}
}

// The header is optional on this route (a caller may omit it); then the body
// legal_entity_id is used as before, and the tenant_id mismatch rule still
// applies.
func TestAnalyze_NoLegalEntityHeader_BodyUsedAndTenantRuleKept(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, "observe") // let the request reach the handler without the header
	router := setupTestRouter(t)

	send := func(body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		withEnvelope(req)
		req.Header.Del("X-Legal-Entity-Id")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(analyzeBody("LE-3003", runLE3003)); rec.Code != http.StatusCreated {
		t.Fatalf("no header: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	body := []byte(`{"tenant_id":"someone-else","legal_entity_id":"LE-3003","job_name":"j","bank_reconciliation_run_id":"` + runLE3003 + `"}`)
	if rec := send(body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenant_mismatch") {
		t.Fatalf("tenant rule: expected 403 tenant_mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

// With envelope enforcement in observe mode, a key-less write still never
// reaches the handler.
func TestAnalyze_WithoutIdempotencyKey_RefusedInObserveMode(t *testing.T) {
	t.Setenv(envelope.EnvVarMode, "observe")
	az := &recordingAuthz{}
	router := newTestRouter(t, deps{authz: az})

	req := httptest.NewRequest(http.MethodPost, "/v1/reconciliations/analyze", bytes.NewReader(analyzeBody("LE-3003", runLE3003)))
	req.Header.Set("Content-Type", "application/json")
	withEnvelope(req)
	req.Header.Del("Idempotency-Key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "idempotency_key_required") {
		t.Fatalf("expected 400 idempotency_key_required, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(az.asked("RECONCILIATION_ANALYZE")) != 0 {
		t.Fatalf("a key-less write must not reach the handler")
	}
}
