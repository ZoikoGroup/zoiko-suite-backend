//go:build integration

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// TestPostingPaths_AgainstRealGeneralLedger is the cross-process proof of
// SPEC_DEVIATIONS A13: this service's real handlers and real GL client,
// posting to a running general-ledger-svc (its real envelope middleware,
// handler and Postgres). Before the fix every one of these postings was
// refused by GL and surfaced as 503 journal_posting_failed.
//
// It needs a running general-ledger-svc whose authorization and close-period
// dependencies grant and report OPEN, so it runs only when one is supplied:
//
//	GL_E2E_URL   e.g. http://127.0.0.1:18098
//
// Tenant and legal entity are fresh UUIDs per run: general-ledger-svc stores
// both as uuid columns, as the registry issues them, and the run stays
// repeatable against the same database.
func TestPostingPaths_AgainstRealGeneralLedger(t *testing.T) {
	glURL := os.Getenv("GL_E2E_URL")
	if glURL == "" {
		t.Skip("GL_E2E_URL not set")
	}
	tenant, entity := uuid.NewString(), uuid.NewString()
	newE2ERouter := func(s *stubStore) chi.Router {
		r := chi.NewRouter()
		r.Use(middleware.TenantContext())
		gl := clients.New("", glURL, "", "", "", "", "", "", zap.NewNop())
		h := handler.New(s, &stubAuthZ{}, gl, testSigningKey, zap.NewNop()).SetSubledgerControlGateEnforced(false)
		handler.RegisterRoutes(r, h)
		return r
	}

	// What this service's client sent before the fix, against the real GL:
	// refused at the envelope, and refused again at ACC-03 once past it.
	t.Run("pre-fix request shape is refused by the real GL", func(t *testing.T) {
		oldBody := map[string]any{
			"tenant_id": tenant, "legal_entity_id": entity, "fiscal_period": "2026-01",
			"description": "pre-fix shape", "correlation_id": uuid.NewString(),
			"lines": []map[string]any{{"account_code": "6100", "debit_amount": 10}, {"account_code": "2100", "credit_amount": 10}},
		}
		rec := postJSON(t, glURL+"/v1/journals", oldBody, map[string]string{"X-Tenant-Id": tenant, "X-Principal-Id": "preparer-1"})
		if rec.StatusCode != http.StatusBadRequest || rec.code != "envelope_incomplete" {
			t.Fatalf("tenant+principal only: expected 400 envelope_incomplete, got %d %q", rec.StatusCode, rec.code)
		}
		rec = postJSON(t, glURL+"/v1/journals", oldBody, map[string]string{
			"X-Tenant-Id": tenant, "X-Principal-Id": "preparer-1", "X-Legal-Entity-Id": entity,
			"X-Request-Id": uuid.NewString(), "X-Correlation-ID": uuid.NewString(), "X-Source-Channel": "system",
			"Idempotency-Key": uuid.NewString(),
		})
		if rec.StatusCode != http.StatusBadRequest || rec.code != "missing_field" {
			t.Fatalf("full envelope, old body: expected 400 missing_field, got %d %q", rec.StatusCode, rec.code)
		}
	})

	t.Run("ACC-07 recognition and reversal", func(t *testing.T) {
		s := newStubStore()
		r := newE2ERouter(s)
		rr := doReqAs(r, http.MethodPost, "/v1/accruals/", domain.CreateAccrualRequest{CurrencyCode: testCurrency, LegalEntityID: entity,
			Description: "audit fee", PolicyVersion: "v1", TotalAmount: 900, StartFiscalPeriod: "2026-01", PeriodCount: 3,
			DebitAccountCode: "6100-AuditFee", CreditAccountCode: "2100-Accrued"}, "preparer-1", tenant)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
		}
		var sch domain.AccrualSchedule
		_ = json.NewDecoder(rr.Body).Decode(&sch)
		for _, step := range []struct{ action, principal string }{{"submit", "preparer-1"}, {"approve", "approver-1"}} {
			if rr := doReqAs(r, http.MethodPost, "/v1/accruals/"+sch.ScheduleID+"/"+step.action, nil, step.principal, tenant); rr.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", step.action, rr.Code, rr.Body.String())
			}
		}
		rr = doReqAs(r, http.MethodPost, "/v1/accruals/"+sch.ScheduleID+"/recognize", domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1", tenant)
		if rr.Code != http.StatusCreated {
			t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
		}
		var inst domain.RecognitionInstance
		_ = json.NewDecoder(rr.Body).Decode(&inst)
		assertGLJournal(t, glURL, tenant, inst.JournalID, "FINALIZED", "ACCRUAL", "2026-01-31")

		rr = doReqAs(r, http.MethodPost, "/v1/accruals/"+sch.ScheduleID+"/recognitions/2026-01/reverse", map[string]string{"reason": "estimate corrected"}, "reverser-1", tenant)
		if rr.Code != http.StatusCreated {
			t.Fatalf("reverse: %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("ACC-10 FX revaluation", func(t *testing.T) {
		s := newStubStore()
		r := newE2ERouter(s)
		runID := uuid.NewString()
		s.fxRuns[runID] = &domain.FXRevaluationRun{CurrencyCode: testCurrency, RunID: runID, TenantID: tenant, LegalEntityID: entity, FiscalPeriod: "2026-01",
			Status: domain.FXRevaluationStatusApproved, FXGainLossAccountCode: "7100-FXGainLoss",
			Items: []domain.FXRevaluationItem{{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200}}}
		rr := doReqAs(r, http.MethodPost, "/v1/fx-revaluations/"+runID+"/post", nil, "poster-1", tenant)
		if rr.Code != http.StatusOK {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
		var run domain.FXRevaluationRun
		_ = json.NewDecoder(rr.Body).Decode(&run)
		assertGLJournal(t, glURL, tenant, *run.JournalID, "FINALIZED", "ADJUSTMENT", "2026-01-31")
	})

	// Posts, then reads the journal back from GL and reconciles it against the
	// crosswalk (ACC-17's B6 reconciliation) — RECONCILED only if GL holds
	// exactly what was sent.
	t.Run("ACC-17 opening balances post and reconcile", func(t *testing.T) {
		s := newStubStore()
		r := newE2ERouter(s)
		batchID := uuid.NewString()
		approver := "approver-1"
		s.migrationBatches[batchID] = &domain.MigrationBatch{CurrencyCode: testCurrency, BatchID: batchID, TenantID: tenant,
			LegalEntityID: entity, FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:e2e",
			ExpectedRowCount: 2, ExpectedTotalDebits: 10000, ExpectedTotalCredits: 10000,
			Status: domain.MigrationBatchStatusApproved, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
			ApprovedByPrincipalID: &approver, Entries: balancedMigrationEntries()}
		rr := doReqAs(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1", tenant)
		if rr.Code != http.StatusOK {
			t.Fatalf("commit: %d %s", rr.Code, rr.Body.String())
		}
		var batch domain.MigrationBatch
		_ = json.NewDecoder(rr.Body).Decode(&batch)
		if batch.Status != domain.MigrationBatchStatusReconciled || batch.JournalID == nil {
			t.Fatalf("expected RECONCILED with a journal, got %s", batch.Status)
		}
		assertGLJournal(t, glURL, tenant, *batch.JournalID, "FINALIZED", "OPENING", "2026-01-31")
	})
}

type glResponse struct {
	*http.Response
	code string
}

func postJSON(t *testing.T, url string, body any, headers map[string]string) glResponse {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var e struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	code := e.ErrorCode
	if code == "" {
		code = e.Error
	}
	return glResponse{resp, code}
}

// assertGLJournal reads the journal back from general-ledger-svc and checks
// it is posted with the ACC-03 context this service sent.
func assertGLJournal(t *testing.T, glURL, tenant, journalID, wantStatus, wantType, wantPostingDate string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, glURL+"/v1/journals/"+journalID, nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "auditor-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET journal: %v", err)
	}
	defer resp.Body.Close()
	var j struct {
		Status       string `json:"status"`
		JournalType  string `json:"journal_type"`
		PostingDate  string `json:"posting_date"`
		CurrencyCode string `json:"currency_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET journal %s: %d %v", journalID, resp.StatusCode, err)
	}
	if j.Status != wantStatus || j.JournalType != wantType || j.PostingDate != wantPostingDate || j.CurrencyCode != testCurrency {
		t.Fatalf("GL journal %s = %+v, want %s %s %s %s", journalID, j, wantStatus, wantType, wantPostingDate, testCurrency)
	}
}
