package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// valuationFixture creates an ACTIVE item with a FIFO valuation policy
// (via the existing createActiveItem helper) and one ACTIVE location.
func valuationFixture(t *testing.T, s *stubStore, r chi.Router) (itemID, locID string) {
	t.Helper()
	itemID = createActiveItem(t, s, r, "le-1", "SKU-VAL-"+uniqueSuffix())
	loc := createDraftLocation(t, r, "le-1", "WH-VAL-"+uniqueSuffix(), "")
	activateLocation(t, r, loc.LocationID)
	return itemID, loc.LocationID
}

func valueMovement(t *testing.T, r chi.Router, movementID string, unitCost *float64) *http.Response {
	t.Helper()
	req := domain.ValueMovementRequest{UnitCost: unitCost}
	rr := doReq(r, http.MethodPost, "/v1/valuation/movements/"+movementID+"/value", req, "preparer-1")
	return rr.Result()
}

func f(v float64) *float64 { return &v }

// ── ValueMovement (INBOUND) ───────────────────────────────────────────────────

func TestValueMovement_InboundMissingUnitCost_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)
	committed := createAndCommitReceipt(t, r, itemID, locID, "idem-val-1", 10)

	rr := doReq(r, http.MethodPost, "/v1/valuation/movements/"+committed.MovementID+"/value", domain.ValueMovementRequest{}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValueMovement_Inbound_CreatesCostLayer(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)
	committed := createAndCommitReceipt(t, r, itemID, locID, "idem-val-2", 10)

	rr := valueMovement(t, r, committed.MovementID, f(5.0))
	if rr.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.StatusCode)
	}
	var entry domain.ValuationEntry
	_ = json.NewDecoder(rr.Body).Decode(&entry)
	if entry.Value != 50 || entry.EntryType != domain.ValuationEntryTypeInbound {
		t.Fatalf("expected value=50 INBOUND, got %+v", entry)
	}

	valReq := doReq(r, http.MethodGet, "/v1/valuation/inventory-value?item_id="+itemID+"&location_id="+locID, nil, "preparer-1")
	var resp map[string]float64
	_ = json.NewDecoder(valReq.Body).Decode(&resp)
	if resp["inventory_value"] != 50 {
		t.Fatalf("expected inventory_value=50, got %v", resp["inventory_value"])
	}
}

// ── Negative path #4: same movement valued twice ─────────────────────────────

func TestValueMovement_SameMovementTwice_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)
	committed := createAndCommitReceipt(t, r, itemID, locID, "idem-val-3", 10)

	first := valueMovement(t, r, committed.MovementID, f(5.0))
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first value failed: %d", first.StatusCode)
	}
	second := valueMovement(t, r, committed.MovementID, f(5.0))
	if second.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 valuing the same movement twice, got %d", second.StatusCode)
	}
}

// ── ValueMovement (OUTBOUND, FIFO) ────────────────────────────────────────────

func TestValueMovement_OutboundFIFO_ConsumesOldestLayerFirst(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)

	firstReceipt := createAndCommitReceipt(t, r, itemID, locID, "idem-fifo-1", 10)
	if rr := valueMovement(t, r, firstReceipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value first receipt failed: %d", rr.StatusCode)
	}
	secondReceipt := createAndCommitReceipt(t, r, itemID, locID, "idem-fifo-2", 10)
	if rr := valueMovement(t, r, secondReceipt.MovementID, f(4.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value second receipt failed: %d", rr.StatusCode)
	}

	// Issue 15 units — should consume all 10 from the first ($2) layer and
	// 5 from the second ($4) layer: value = 10*2 + 5*4 = 40.
	issueReq := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locID, Quantity: 15, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-fifo-issue", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", issueReq, "preparer-1")
	var issueM domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&issueM)
	v := doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate issue failed: %d %s", v.Code, v.Body.String())
	}
	c := doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/commit", nil, "preparer-1")
	if c.Code != http.StatusOK {
		t.Fatalf("commit issue failed: %d %s", c.Code, c.Body.String())
	}

	rr := valueMovement(t, r, issueM.MovementID, nil)
	if rr.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.StatusCode)
	}
	var entry domain.ValuationEntry
	_ = json.NewDecoder(rr.Body).Decode(&entry)
	if entry.Value != 40 {
		t.Fatalf("expected FIFO value=40 (10*2 + 5*4), got %v", entry.Value)
	}
}

// ── ValueMovement (OUTBOUND, insufficient layers) ─────────────────────────────

// TestValueMovement_OutboundInsufficientLayers_Returns422 exercises a
// real gap this session flags deliberately: INV-03's own negative-stock
// check only knows about PHYSICAL movements, not valuation. A receipt
// that is never valued (no cost layer created) still lets a same-sized
// issue commit at the physical layer — but valuing that issue afterward
// has no cost layers to draw from.
func TestValueMovement_OutboundInsufficientLayers_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)

	createAndCommitReceipt(t, r, itemID, locID, "idem-insuf-1", 5) // never valued — no cost layer created

	issueReq := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locID, Quantity: 5, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-insuf-issue", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", issueReq, "preparer-1")
	var issueM domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&issueM)
	v := doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate issue failed: %d %s", v.Code, v.Body.String())
	}
	c := doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/commit", nil, "preparer-1")
	if c.Code != http.StatusOK {
		t.Fatalf("commit issue failed: %d %s", c.Code, c.Body.String())
	}

	rr := valueMovement(t, r, issueM.MovementID, nil)
	if rr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 insufficient cost layers, got %d", rr.StatusCode)
	}
}

// ── CreateValuationRun / EmitInventoryAccountingEvent ────────────────────────

func TestEmitInventoryAccountingEvent_SelfApproval_Returns403(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-run-1", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}

	issueReq := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locID, Quantity: 4, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-run-issue", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", issueReq, "preparer-1")
	var issueM domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&issueM)
	doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/commit", nil, "preparer-1")
	if rr := valueMovement(t, r, issueM.MovementID, nil); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value issue failed: %d", rr.StatusCode)
	}

	runReq := domain.CreateValuationRunRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-09", InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS"}
	rr := doReq(r, http.MethodPost, "/v1/valuation/runs/", runReq, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run failed: %d %s", rr.Code, rr.Body.String())
	}
	var runResp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&runResp)
	runID := runResp["run_id"].(string)

	emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "preparer-1")
	if emit.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", emit.Code, emit.Body.String())
	}
}

func TestEmitInventoryAccountingEvent_DifferentApprover_PostsJournal(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-run-2", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(3.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}
	issueReq := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locID, Quantity: 4, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-run2-issue", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", issueReq, "preparer-1")
	var issueM domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&issueM)
	doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/movements/"+issueM.MovementID+"/commit", nil, "preparer-1")
	if rr := valueMovement(t, r, issueM.MovementID, nil); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value issue failed: %d", rr.StatusCode)
	}

	runReq := domain.CreateValuationRunRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-09", InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS"}
	rr := doReq(r, http.MethodPost, "/v1/valuation/runs/", runReq, "preparer-1")
	var runResp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&runResp)
	runID := runResp["run_id"].(string)

	emit := doReq(r, http.MethodPost, "/v1/valuation/runs/"+runID+"/emit", nil, "approver-2")
	if emit.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", emit.Code, emit.Body.String())
	}
	if ledger.postCalls != 1 {
		t.Fatalf("expected exactly 1 journal post, got %d", ledger.postCalls)
	}
}

// ── RecordInventoryWriteDown / ReverseWriteDown ──────────────────────────────

func TestRecordInventoryWriteDown_MissingEvidence_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	itemID, locID := valuationFixture(t, s, r)

	req := domain.RecordWriteDownRequest{
		ItemID: itemID, LocationID: locID, Amount: 100, ExpenseAccountCode: "WD-EXP", InventoryAccountCode: "INV-ASSET", FiscalPeriod: "2026-09",
	}
	rr := doReq(r, http.MethodPost, "/v1/valuation/write-downs/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRecordInventoryWriteDown_WithEvidence_Succeeds(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-wd-1"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	req := domain.RecordWriteDownRequest{
		ItemID: itemID, LocationID: locID, Amount: 100, ValuationEvidenceRef: "NRV-APPRAISAL-1",
		ExpenseAccountCode: "WD-EXP", InventoryAccountCode: "INV-ASSET", FiscalPeriod: "2026-09",
	}
	rr := doReq(r, http.MethodPost, "/v1/valuation/write-downs/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != 1 {
		t.Fatalf("expected exactly 1 journal post, got %d", ledger.postCalls)
	}
}

func TestReverseWriteDown_SelfReversal_Returns403(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-wd-2"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	req := domain.RecordWriteDownRequest{
		ItemID: itemID, LocationID: locID, Amount: 50, ValuationEvidenceRef: "NRV-2",
		ExpenseAccountCode: "WD-EXP", InventoryAccountCode: "INV-ASSET", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/valuation/write-downs/", req, "preparer-1")
	var wd domain.WriteDown
	_ = json.NewDecoder(create.Body).Decode(&wd)

	rr := doReq(r, http.MethodPost, "/v1/valuation/write-downs/"+wd.WriteDownID+"/reverse", domain.ReverseWriteDownRequest{Reason: "recovered value"}, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-reversal, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReverseWriteDown_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postJournalID: "jrnl-wd-3"}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	req := domain.RecordWriteDownRequest{
		ItemID: itemID, LocationID: locID, Amount: 50, ValuationEvidenceRef: "NRV-3",
		ExpenseAccountCode: "WD-EXP", InventoryAccountCode: "INV-ASSET", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/valuation/write-downs/", req, "preparer-1")
	var wd domain.WriteDown
	_ = json.NewDecoder(create.Body).Decode(&wd)

	rr := doReq(r, http.MethodPost, "/v1/valuation/write-downs/"+wd.WriteDownID+"/reverse", domain.ReverseWriteDownRequest{Reason: "recovered value"}, "reviewer-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ledger.reverseCalls != 1 {
		t.Fatalf("expected exactly 1 journal reversal, got %d", ledger.reverseCalls)
	}
}

// ── GetInventoryValueTotal (§9 "Inventory value → GL") ───────────────────────

func TestGetInventoryValueTotal_MissingLegalEntity_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/valuation/inventory-value-total", nil, "reader-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetInventoryValueTotal_SumsAcrossItemsAndLocations(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemA, locA := valuationFixture(t, s, r)
	committedA := createAndCommitReceipt(t, r, itemA, locA, "idem-val-total-1", 10)
	valueMovement(t, r, committedA.MovementID, f(5.0)) // 10 * 5 = 50

	itemB := createActiveItem(t, s, r, "le-1", "SKU-VAL-"+uniqueSuffix())
	committedB := createAndCommitReceipt(t, r, itemB, locA, "idem-val-total-2", 4)
	valueMovement(t, r, committedB.MovementID, f(25.0)) // 4 * 25 = 100

	rr := doReq(r, http.MethodGet, "/v1/valuation/inventory-value-total?legal_entity_id=le-1", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]float64
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["inventory_value_total"] != 150 {
		t.Fatalf("expected 150 (50 + 100 across two items), got %+v", out)
	}
}

func TestGetInventoryValueTotal_DifferentLegalEntity_ExcludedFromTotal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemA, locA := valuationFixture(t, s, r)
	committedA := createAndCommitReceipt(t, r, itemA, locA, "idem-val-total-3", 10)
	valueMovement(t, r, committedA.MovementID, f(5.0))

	rr := doReq(r, http.MethodGet, "/v1/valuation/inventory-value-total?legal_entity_id=le-unrelated", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]float64
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["inventory_value_total"] != 0 {
		t.Fatalf("expected 0 for an unrelated legal_entity_id, got %+v", out)
	}
}
