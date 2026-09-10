package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/asset-management-svc/internal/domain"
)

func buildScheduleReq(assetID string) domain.BuildDepreciationScheduleRequest {
	return domain.BuildDepreciationScheduleRequest{
		AssetID: assetID, BookID: "book-1", CostBasis: 12000, ResidualValue: 0, UsefulLifeMonths: 12,
	}
}

// ── BuildDepreciationSchedule ─────────────────────────────────────────────────

func TestBuildDepreciationSchedule_AssetNotActive_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createRegisteredAsset(t, s, r, "le-1") // REGISTERED, not yet ACTIVE

	rr := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestBuildDepreciationSchedule_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")

	rr := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var sch domain.DepreciationSchedule
	_ = json.NewDecoder(rr.Body).Decode(&sch)
	if sch.Status != domain.DepreciationScheduleStatusActive {
		t.Fatalf("expected ACTIVE, got %q", sch.Status)
	}
}

// TestBuildDepreciationSchedule_DuplicateForAssetBook_Returns422 is the
// spec's own negative path, "Same asset depreciated twice in period,"
// enforced at its root: at most one current schedule per (asset, book).
func TestBuildDepreciationSchedule_DuplicateForAssetBook_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")

	first := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first build failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on duplicate schedule for the same asset/book, got %d: %s", second.Code, second.Body.String())
	}
}

// ── RecalculateSchedule ("Useful life changed after approval without invalidation") ──

func TestRecalculateSchedule_CreatesNewVersionSupersedesOld(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")

	create := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	var sch domain.DepreciationSchedule
	_ = json.NewDecoder(create.Body).Decode(&sch)

	newLife := 24
	recalc := doReq(r, http.MethodPost, "/v1/depreciation-schedules/"+sch.ScheduleID+"/recalculate",
		domain.RecalculateScheduleRequest{UsefulLifeMonths: &newLife}, "preparer-1")
	if recalc.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recalc.Code, recalc.Body.String())
	}
	var newVersion domain.DepreciationSchedule
	_ = json.NewDecoder(recalc.Body).Decode(&newVersion)
	if newVersion.Version != 2 || newVersion.UsefulLifeMonths != 24 {
		t.Fatalf("expected version 2 with useful_life_months 24, got %+v", newVersion)
	}

	// GetDepreciationSchedule now resolves to the NEW version's content.
	get := doReq(r, http.MethodGet, "/v1/depreciation-schedules/"+sch.ScheduleID, nil, "preparer-1")
	var current domain.DepreciationSchedule
	_ = json.NewDecoder(get.Body).Decode(&current)
	if current.UsefulLifeMonths != 24 {
		t.Fatalf("expected the current schedule to reflect the new version, got %+v", current)
	}
}

// ── CreateDepreciationRun ("Same asset depreciated twice in period") ────────

func TestCreateDepreciationRun_DuplicateForPeriod_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateDepreciationRunRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
	}
	first := doReq(r, http.MethodPost, "/v1/depreciation-runs/", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first run creation failed: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/depreciation-runs/", req, "preparer-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on duplicate run for the same period, got %d: %s", second.Code, second.Body.String())
	}
}

// ── FreezeDepreciationPopulation ─────────────────────────────────────────────

func TestFreezeDepreciationPopulation_ExcludesNonActiveAssets(t *testing.T) {
	// "Disposed component remains in eligible population" is structurally
	// enforced by filtering to ACTIVE assets only — SUSPENDED stands in
	// for the not-yet-built DISPOSED status (AST-03's own future
	// authority) as the real, already-available exclusion case.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")

	buildRR := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if buildRR.Code != http.StatusCreated {
		t.Fatalf("build schedule failed: %d %s", buildRR.Code, buildRR.Body.String())
	}

	suspendRR := doReq(r, http.MethodPost, "/v1/assets/"+assetID+"/suspend", domain.SuspendAssetRequest{Reason: "under repair"}, "preparer-1")
	if suspendRR.Code != http.StatusOK {
		t.Fatalf("suspend failed: %d %s", suspendRR.Code, suspendRR.Body.String())
	}

	runReq := domain.CreateDepreciationRunRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
	}
	createRunRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/", runReq, "preparer-1")
	var run domain.DepreciationRun
	_ = json.NewDecoder(createRunRR.Body).Decode(&run)

	freezeRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/freeze", nil, "preparer-1")
	if freezeRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (empty population — the only asset is SUSPENDED), got %d: %s", freezeRR.Code, freezeRR.Body.String())
	}
}

// ── Full lifecycle: build -> run -> freeze -> validate -> approve -> emit ────

func TestDepreciationRun_FullLifecycle_EmitsRealJournal(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "real-journal-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	assetID := createActiveAsset(t, s, r, "le-1")

	buildRR := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if buildRR.Code != http.StatusCreated {
		t.Fatalf("build schedule failed: %d %s", buildRR.Code, buildRR.Body.String())
	}

	runReq := domain.CreateDepreciationRunRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
	}
	createRunRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/", runReq, "preparer-1")
	var run domain.DepreciationRun
	_ = json.NewDecoder(createRunRR.Body).Decode(&run)

	if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/freeze", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("freeze failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/validate", nil, "preparer-1"); rr.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	emitRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/emit", nil, "approver-1")
	if emitRR.Code != http.StatusOK {
		t.Fatalf("emit failed: %d %s", emitRR.Code, emitRR.Body.String())
	}
	if ledger.postCalls != 1 {
		t.Fatalf("expected exactly 1 journal post, got %d", ledger.postCalls)
	}
	if ledger.lastPostedSourceEventID != run.RunID {
		t.Fatalf("expected the run's own ID as the idempotency source_event_id, got %q", ledger.lastPostedSourceEventID)
	}

	got, _ := s.GetDepreciationRun(context.Background(), run.RunID)
	if got.Status != domain.DepreciationRunStatusAccountingEventEmitted || got.JournalID == nil || *got.JournalID != "real-journal-1" {
		t.Fatalf("expected ACCOUNTING_EVENT_EMITTED with the real journal id recorded, got %+v", got)
	}
}

// ── ApproveDepreciationRun (self-approval SoD) ───────────────────────────────

func TestApproveDepreciationRun_SameCreator_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateDepreciationRunRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
	}
	createRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/", req, "preparer-1")
	var run domain.DepreciationRun
	_ = json.NewDecoder(createRR.Body).Decode(&run)
	s.runs[run.RunID].Status = domain.DepreciationRunStatusValidated // skip straight to approvable for this test

	approveRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/approve", nil, "preparer-1")
	if approveRR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", approveRR.Code, approveRR.Body.String())
	}
}

// ── SupersedeDepreciationRun ("Rerun emits duplicate accounting event") ─────

func TestSupersedeDepreciationRun_ReversesJournalAndReleasesPeriod(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "real-journal-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	assetID := createActiveAsset(t, s, r, "le-1")
	_ = doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")

	runReq := domain.CreateDepreciationRunRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		DepreciationExpenseAccountCode: "6400-Depr", AccumulatedDepreciationAccountCode: "1590-AccumDepr",
	}
	createRunRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/", runReq, "preparer-1")
	var run domain.DepreciationRun
	_ = json.NewDecoder(createRunRR.Body).Decode(&run)
	_ = doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/freeze", nil, "preparer-1")
	_ = doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/validate", nil, "preparer-1")
	_ = doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/approve", nil, "approver-1")
	_ = doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/emit", nil, "approver-1")

	supersedeRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/supersede", domain.SupersedeDepreciationRunRequest{Reason: "correcting an error"}, "preparer-1")
	if supersedeRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", supersedeRR.Code, supersedeRR.Body.String())
	}
	if ledger.reverseCalls != 1 {
		t.Fatalf("expected exactly 1 journal reversal, got %d", ledger.reverseCalls)
	}

	// The period slot is released — a fresh run can now be created.
	newRunRR := doReq(r, http.MethodPost, "/v1/depreciation-runs/", runReq, "preparer-1")
	if newRunRR.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating a fresh run for the same period after supersession, got %d: %s", newRunRR.Code, newRunRR.Body.String())
	}
}

// ── GetNetBookValueTotal (ACC-06's own "Assets → GL" source) ────────────────

func TestGetNetBookValueTotal_MissingParams_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/assets/net-book-value?legal_entity_id=le-1", nil, "reader-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing book_id, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetNetBookValueTotal_SumsCostBasisForBookAndLegalEntity(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")

	rr := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("build schedule failed: %d %s", rr.Code, rr.Body.String())
	}

	nbvRR := doReq(r, http.MethodGet, "/v1/assets/net-book-value?legal_entity_id=le-1&book_id=book-1", nil, "reader-1")
	if nbvRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", nbvRR.Code, nbvRR.Body.String())
	}
	var out map[string]float64
	_ = json.NewDecoder(nbvRR.Body).Decode(&out)
	if out["net_book_value_total"] != 12000 {
		t.Fatalf("expected net_book_value_total 12000 (this stub doesn't model accumulated depreciation), got %v", out["net_book_value_total"])
	}
}

func TestGetNetBookValueTotal_DifferentBook_ExcludedFromTotal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	assetID := createActiveAsset(t, s, r, "le-1")
	_ = doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(assetID), "preparer-1")

	nbvRR := doReq(r, http.MethodGet, "/v1/assets/net-book-value?legal_entity_id=le-1&book_id=some-other-book", nil, "reader-1")
	if nbvRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", nbvRR.Code, nbvRR.Body.String())
	}
	var out map[string]float64
	_ = json.NewDecoder(nbvRR.Body).Decode(&out)
	if out["net_book_value_total"] != 0 {
		t.Fatalf("expected 0 for an unrelated book_id, got %v", out["net_book_value_total"])
	}
}
