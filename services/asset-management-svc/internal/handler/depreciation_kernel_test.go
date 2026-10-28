package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/asset-management-svc/internal/domain"
)

// approvedKernelRun builds a run over two assets of different categories and takes it to APPROVED.
func approvedKernelRun(t *testing.T, ledger *stubLedger) (*stubStore, chi.Router, string) {
	t.Helper()
	s := newStubStore()
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	a1 := createActiveAsset(t, s, r, "le-1")
	a2 := createActiveAsset(t, s, r, "le-1")
	s.assets[a1].AssetCategory, s.assets[a2].AssetCategory = "IT", "Vehicles"
	for _, id := range []string{a1, a2} {
		if rr := doReq(r, http.MethodPost, "/v1/depreciation-schedules/", buildScheduleReq(id), "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("schedule: %d %s", rr.Code, rr.Body.String())
		}
	}
	runReq := domain.CreateDepreciationRunRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-01", DepreciationExpenseAccountCode: "6400", AccumulatedDepreciationAccountCode: "1590"}
	rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/", runReq, "preparer-1")
	var run domain.DepreciationRun
	_ = json.NewDecoder(rr.Body).Decode(&run)
	for _, step := range [][2]string{{"freeze", "preparer-1"}, {"validate", "preparer-1"}, {"approve", "approver-1"}} {
		if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+run.RunID+"/"+step[0], nil, step[1]); rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step[0], rr.Code, rr.Body.String())
		}
	}
	return s, r, run.RunID
}

// Through the kernel a run posts one accounting event per asset category, not one journal of account codes, and records every journal.
func TestEmitDepreciation_Kernel_PostsOneEventPerAssetCategory(t *testing.T) {
	ledger := &stubLedger{kernel: true}
	s, r, runID := approvedKernelRun(t, ledger)

	rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+runID+"/emit", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("emit: %d %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != 0 {
		t.Fatalf("the legacy posting command must not be used: %d calls", ledger.postCalls)
	}
	if len(ledger.kernelPosts) != 2 || ledger.kernelPosts[0] != (kernelPost{"IT", "1000.00", runID}) || ledger.kernelPosts[1] != (kernelPost{"Vehicles", "1000.00", runID}) {
		t.Fatalf("%+v", ledger.kernelPosts)
	}
	got, _ := s.GetDepreciationRun(context.Background(), runID)
	if got.Status != domain.DepreciationRunStatusAccountingEventEmitted || got.JournalID == nil || *got.JournalID != "kj-IT" || len(got.LedgerJournalIDs) != 2 {
		t.Fatalf("%+v", got)
	}

	// superseding the run reverses every journal it posted
	if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+runID+"/supersede", domain.SupersedeDepreciationRunRequest{Reason: "wrong policy"}, "approver-2"); rr.Code != http.StatusOK {
		t.Fatalf("supersede: %d %s", rr.Code, rr.Body.String())
	}
	if len(ledger.reversed) != 2 || ledger.reversed[0] != "kj-IT" || ledger.reversed[1] != "kj-Vehicles" {
		t.Fatalf("both journals are reversed: %v", ledger.reversed)
	}
}

// An asset with no category cannot be reconciled by class, so nothing posts.
func TestEmitDepreciation_Kernel_RefusesAnAssetWithNoCategory(t *testing.T) {
	ledger := &stubLedger{kernel: true}
	s, r, runID := approvedKernelRun(t, ledger)
	for _, a := range s.assets {
		a.AssetCategory = ""
		break
	}
	rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+runID+"/emit", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity || len(ledger.kernelPosts) != 0 {
		t.Fatalf("%d %s %+v", rr.Code, rr.Body.String(), ledger.kernelPosts)
	}
}

// A refusal from the kernel leaves the run APPROVED, so the emit can be retried.
func TestEmitDepreciation_Kernel_RefusalLeavesTheRunApproved(t *testing.T) {
	ledger := &stubLedger{kernel: true, kernelErr: domain.ErrLedgerUnavailable}
	s, r, runID := approvedKernelRun(t, ledger)
	rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+runID+"/emit", nil, "approver-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if got, _ := s.GetDepreciationRun(context.Background(), runID); got.Status != domain.DepreciationRunStatusApproved {
		t.Fatalf("%q", got.Status)
	}
}

// With the kernel off the run posts exactly as before.
func TestEmitDepreciation_KernelOffKeepsTheLegacyPath(t *testing.T) {
	ledger := &stubLedger{postJournalID: "legacy-1"}
	_, r, runID := approvedKernelRun(t, ledger)
	if rr := doReq(r, http.MethodPost, "/v1/depreciation-runs/"+runID+"/emit", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != 1 || len(ledger.kernelPosts) != 0 {
		t.Fatalf("%d %+v", ledger.postCalls, ledger.kernelPosts)
	}
}
