package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

// kernelFixture is the valuation fixture with the item type the kernel reconciles stock by.
func kernelFixture(t *testing.T, s *stubStore, r chi.Router) (itemID, locID string) {
	t.Helper()
	itemID, locID = valuationFixture(t, s, r)
	s.items[itemID].ItemType = "FINISHED"
	return itemID, locID
}

func eventByType(evs []clients.KernelEvent, typ string) *clients.KernelEvent {
	for i := range evs {
		if evs[i].EventType == typ {
			return &evs[i]
		}
	}
	return nil
}

func valuedIssue(t *testing.T, s *stubStore, r chi.Router, itemID, locID, key string, qty float64) {
	t.Helper()
	if code := issueQty(t, r, itemID, locID, key, qty); code != http.StatusOK {
		t.Fatalf("issue failed: %d", code)
	}
	if rr := valueMovement(t, r, s.movementsByKey[key], nil); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value issue failed: %d", rr.StatusCode)
	}
}

// Through the kernel a run posts cost of sales and the goods-received reclass as events by location and item type, never a journal of
// account codes, and records every journal it produced.
func TestKernelEmit_ValuationRun_PostsCostOfSalesAndTheReclassByLocation(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{kernel: true}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := kernelFixture(t, s, r)

	linked := receiveWithAPReceipt(t, r, itemID, locID, "idem-k-1", "rcpt-7", 10)
	if rr := valueMovement(t, r, linked.MovementID, f(3.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}
	valuedIssue(t, s, r, itemID, locID, "idem-k-i1", 4)
	runID := newRun(t, r)

	emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "approver-2")
	if emit.Code != http.StatusOK {
		t.Fatalf("emit: %d %s", emit.Code, emit.Body.String())
	}
	if ledger.postCalls != 0 {
		t.Fatalf("the legacy posting command must not be used: %d", ledger.postCalls)
	}
	if len(ledger.kernelEvents) != 2 {
		t.Fatalf("%+v", ledger.kernelEvents)
	}
	cogs := eventByType(ledger.kernelEvents, "INVENTORY.COGS_RECOGNIZED")
	if cogs == nil || cogs.Facts["amount"] != "12.00" || cogs.Facts["run_ref"] != runID || cogs.Dimensions["item_class"] != "FINISHED" || cogs.Dimensions["location"] == "" ||
		cogs.SourceKey != "inv-run:"+runID+":cogs:"+cogs.Dimensions["location"]+":FINISHED" || cogs.FiscalPeriod != "2026-09" {
		t.Fatalf("%+v", cogs)
	}
	reclass := eventByType(ledger.kernelEvents, "INVENTORY.GRNI_EXPENSE_RECLASSIFIED")
	if reclass == nil || reclass.Facts["amount"] != "30.00" || reclass.Facts["receipt_basis"] != "ap-linked" {
		t.Fatalf("%+v", reclass)
	}
	got, _ := s.GetValuationRun(context.Background(), runID)
	if got.Status != domain.ValuationRunStatusAccountingEventEmitted || len(got.LedgerJournalIDs) != 2 || got.JournalID == nil {
		t.Fatalf("%+v", got)
	}
}

// An item with no type cannot be reconciled by class, so nothing posts and the run stays open.
func TestKernelEmit_ValuationRun_RefusesAnItemWithNoType(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{kernel: true}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)
	s.items[itemID].ItemType = ""
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-k-2", 10)
	valueMovement(t, r, receipt.MovementID, f(3.0))
	valuedIssue(t, s, r, itemID, locID, "idem-k-i2", 4)
	runID := newRun(t, r)
	emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "approver-2")
	if emit.Code != http.StatusUnprocessableEntity || len(ledger.kernelEvents) != 0 {
		t.Fatalf("%d %s %+v", emit.Code, emit.Body.String(), ledger.kernelEvents)
	}
}

// A kernel refusal leaves the run frozen so the emit can be retried.
func TestKernelEmit_ValuationRun_RefusalLeavesTheRunOpen(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{kernel: true, kernelErr: domain.ErrLedgerUnavailable}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := kernelFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-k-3", 10)
	valueMovement(t, r, receipt.MovementID, f(3.0))
	valuedIssue(t, s, r, itemID, locID, "idem-k-i3", 4)
	runID := newRun(t, r)
	if emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "approver-2"); emit.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", emit.Code, emit.Body.String())
	}
	if got, _ := s.GetValuationRun(context.Background(), runID); got.Status != domain.ValuationRunStatusPopulationFrozen {
		t.Fatalf("%q", got.Status)
	}
}

// A write-down is an INVENTORY.NRV_WRITE_DOWN event carrying its evidence.
func TestKernelWriteDown_PostsAnNRVEventWithItsEvidence(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{kernel: true}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := kernelFixture(t, s, r)
	req := domain.RecordWriteDownRequest{ItemID: itemID, LocationID: locID, Amount: 100, ValuationEvidenceRef: "NRV-1", ExpenseAccountCode: "X", InventoryAccountCode: "Y", FiscalPeriod: "2026-09"}
	rr := doReq(r, http.MethodPost, "/v1/valuation/write-downs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var wd domain.WriteDown
	_ = json.NewDecoder(rr.Body).Decode(&wd)
	e := eventByType(ledger.kernelEvents, "INVENTORY.NRV_WRITE_DOWN")
	if ledger.postCalls != 0 || e == nil || e.Facts["amount"] != "100.00" || e.Facts["valuation_evidence_ref"] != "NRV-1" || e.Facts["write_down_ref"] != wd.WriteDownID || e.SourceKey != "inv-wd:"+wd.WriteDownID {
		t.Fatalf("%d %+v", ledger.postCalls, e)
	}
}

// Landed cost is an INVENTORY.LANDED_COST_ALLOCATED event: the part on stock still held, the part on goods already sold, the evidence.
func TestKernelLandedCost_PostsTheSplitAsOneEvent(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{kernel: true}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := kernelFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-k-lc", 10)
	valueMovement(t, r, receipt.MovementID, f(2.0))
	valuedIssue(t, s, r, itemID, locID, "idem-k-lci", 4)
	rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", landedReq(receipt.MovementID, "lc-k", 12), "approver-2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := eventByType(ledger.kernelEvents, "INVENTORY.LANDED_COST_ALLOCATED")
	if ledger.postCalls != 0 || e == nil || e.Facts["amount"] != "12.00" || e.Facts["inventory_share"] != "7.20" || e.Facts["cogs_share"] != "4.80" || e.Dimensions["item_class"] != "FINISHED" {
		t.Fatalf("%d %+v", ledger.postCalls, e)
	}
}

// With the kernel off the legacy journal is posted exactly as before.
func TestKernelOff_ValuationRunKeepsTheLegacyPath(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "legacy-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-k-off", 10)
	valueMovement(t, r, receipt.MovementID, f(3.0))
	valuedIssue(t, s, r, itemID, locID, "idem-k-offi", 4)
	runID := newRun(t, r)
	if emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "approver-2"); emit.Code != http.StatusOK {
		t.Fatalf("%d %s", emit.Code, emit.Body.String())
	}
	if ledger.postCalls != 1 || len(ledger.kernelEvents) != 0 {
		t.Fatalf("%d %+v", ledger.postCalls, ledger.kernelEvents)
	}
}
