package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func createRunReadyProject(t *testing.T, r chi.Router, legalEntityID, code string) string {
	t.Helper()
	id := createActiveProject(t, r, legalEntityID, code)

	future := time.Now().UTC().Add(time.Hour)
	estReq := domain.SetApprovedEstimateRequest{ProjectID: id, EstimateToComplete: 100, EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/recognition/estimates", estReq, "estimator-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("set estimate failed: %d %s", rr.Code, rr.Body.String())
	}
	return id
}

func createDraftRun(t *testing.T, r chi.Router, projectID, fiscalPeriod string) domain.RecognitionRun {
	t.Helper()
	req := domain.CreateRecognitionRunRequest{
		ProjectID: projectID, FiscalPeriod: fiscalPeriod, ContractValue: 1000, BilledToDate: 0,
		RevenueAccountCode: "4000", WIPAccountCode: "1300",
	}
	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run failed: %d %s", rr.Code, rr.Body.String())
	}
	var run domain.RecognitionRun
	_ = json.NewDecoder(rr.Body).Decode(&run)
	return run
}

// ── SetApprovedEstimate ("Progress estimate changed after approval without
// invalidation") ─────────────────────────────────────────────────────────────

func TestSetApprovedEstimate_PastEffectiveDate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "REC-1")

	past := time.Now().UTC().Add(-time.Hour)
	req := domain.SetApprovedEstimateRequest{ProjectID: id, EstimateToComplete: 100, EffectiveFrom: &past}
	rr := doReq(r, http.MethodPost, "/v1/recognition/estimates", req, "estimator-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retroactive estimate, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── CreateRecognitionRun ("Invoice amount treated automatically as revenue" —
// structurally impossible: contract_value is always caller-declared, never
// copied from billed_to_date) ────────────────────────────────────────────────

func TestCreateRecognitionRun_MissingContractValue_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "REC-2")

	req := domain.CreateRecognitionRunRequest{ProjectID: id, FiscalPeriod: "2026-09"}
	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 contract_value_required, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateRecognitionRun_DuplicatePeriod_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRunReadyProject(t, r, "le-1", "REC-3")
	createDraftRun(t, r, id, "2026-09")

	req := domain.CreateRecognitionRunRequest{ProjectID: id, FiscalPeriod: "2026-09", ContractValue: 500, RevenueAccountCode: "4000", WIPAccountCode: "1300"}
	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 duplicate live run for period, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── CalculateRecognitionRun (freeze + percentage-of-completion calc) ────────

func TestCalculateRecognitionRun_NoApprovedEstimate_Fails(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveProject(t, r, "le-1", "REC-4")
	req := domain.CreateRecognitionRunRequest{ProjectID: id, FiscalPeriod: "2026-09", ContractValue: 1000, RevenueAccountCode: "4000", WIPAccountCode: "1300"}
	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run failed: %d %s", rr.Code, rr.Body.String())
	}
	var run domain.RecognitionRun
	_ = json.NewDecoder(rr.Body).Decode(&run)

	calc := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/calculate", nil, "preparer-1")
	if calc.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 approved_estimate_required, got %d: %s", calc.Code, calc.Body.String())
	}
}

func TestCalculateRecognitionRun_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRunReadyProject(t, r, "le-1", "REC-5")
	run := createDraftRun(t, r, id, "2026-09")

	calc := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/calculate", nil, "preparer-1")
	if calc.Code != http.StatusOK {
		t.Fatalf("calculate failed: %d %s", calc.Code, calc.Body.String())
	}
	if s.runs[run.RunID].Status != domain.RecognitionRunStatusCalculated {
		t.Fatalf("expected CALCULATED, got %q", s.runs[run.RunID].Status)
	}
}

// ── ApproveRecognitionRun ("Estimator/preparer cannot self-approve material
// estimate or recognition override") ────────────────────────────────────────

func TestApproveRecognitionRun_SameCreator_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRunReadyProject(t, r, "le-1", "REC-6")
	run := createDraftRun(t, r, id, "2026-09")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/calculate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/validate", nil, "preparer-1")

	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/approve", nil, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveRecognitionRun_DifferentApprover_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRunReadyProject(t, r, "le-1", "REC-7")
	run := createDraftRun(t, r, id, "2026-09")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/calculate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/validate", nil, "preparer-1")

	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/approve", nil, "approver-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.runs[run.RunID].Status != domain.RecognitionRunStatusApproved {
		t.Fatalf("expected APPROVED, got %q", s.runs[run.RunID].Status)
	}
}

// ── EmitRecognitionAccountingEvent ("closed period blocks certification") ───

func TestEmitRecognitionAccountingEvent_PeriodLocked_Returns422(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouterWithPeriodChecker(s, pub, &stubAuthZ{}, &stubPeriodChecker{err: domain.ErrPeriodLocked})
	id := createRunReadyProject(t, r, "le-1", "REC-8")
	run := createDraftRun(t, r, id, "2026-09")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/calculate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/approve", nil, "approver-2")

	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/emit", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 period_locked, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── SupersedeRecognitionRun (reversal path) ──────────────────────────────────

func TestSupersedeRecognitionRun_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createRunReadyProject(t, r, "le-1", "REC-9")
	run := createDraftRun(t, r, id, "2026-09")

	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/supersede", map[string]string{}, "manager-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSupersedeRecognitionRun_Succeeds(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	id := createRunReadyProject(t, r, "le-1", "REC-10")
	run := createDraftRun(t, r, id, "2026-09")

	rr := doReq(r, http.MethodPost, "/v1/recognition/runs/"+run.RunID+"/supersede", domain.SupersedeRecognitionRunRequest{Reason: "wrong estimate"}, "manager-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.runs[run.RunID].Status != domain.RecognitionRunStatusSuperseded {
		t.Fatalf("expected SUPERSEDED, got %q", s.runs[run.RunID].Status)
	}
}
