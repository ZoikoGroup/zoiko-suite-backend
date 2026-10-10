package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── stub store methods (INV-04 AllocateLandedCost) ───────────────────────────

func (s *stubStore) AllocateLandedCost(_ context.Context, a *domain.LandedCostAllocation) (bool, error) {
	if s.landedCosts == nil {
		s.landedCosts = map[string]*domain.LandedCostAllocation{}
	}
	for _, ex := range s.landedCosts {
		if ex.IdempotencyKey == a.IdempotencyKey {
			if ex.MovementID != a.MovementID || ex.Amount != a.Amount {
				return false, domain.ErrLandedCostKeyConflict
			}
			*a = *ex
			return false, nil
		}
	}
	var layer *domain.CostLayer
	for _, l := range s.costLayers {
		if l.SourceMovementID == a.MovementID {
			layer = l
		}
	}
	if layer == nil {
		return false, domain.ErrNoCostLayerForMovement
	}
	if eid, ok := s.entriesByMovement[a.MovementID]; ok && s.valuationEntries[eid].ValuationMethod == domain.ValuationMethodStandardCost {
		return false, domain.ErrLandedCostNotApplicable
	}
	var inv, uplift float64
	if layer.RemainingQuantity > 0 && layer.OriginalQuantity > 0 {
		inv = math.Round(a.Amount*layer.RemainingQuantity/layer.OriginalQuantity*100) / 100
		uplift = inv / layer.RemainingQuantity
	}
	layer.UnitCost += uplift
	a.LayerID, a.ItemID, a.LocationID = layer.LayerID, layer.ItemID, layer.LocationID
	a.LegalEntityID = s.movements[a.MovementID].LegalEntityID
	a.InventoryShare, a.COGSShare = inv, math.Round((a.Amount-inv)*100)/100
	a.RemainingQuantityAtAllocation, a.UnitUplift = layer.RemainingQuantity, uplift
	a.Status = domain.LandedCostStatusPendingPosting
	cp := *a
	s.landedCosts[a.AllocationID] = &cp
	return true, nil
}

func (s *stubStore) MarkLandedCostEmitted(_ context.Context, id, journalID string, at time.Time) error {
	a, ok := s.landedCosts[id]
	if !ok || a.Status != domain.LandedCostStatusPendingPosting {
		return domain.ErrInvalidRunTransition
	}
	a.Status, a.JournalID, a.EmittedAt = domain.LandedCostStatusAccountingEventEmitted, &journalID, &at
	return nil
}

func (s *stubStore) GetLandedCostAllocation(_ context.Context, id string) (*domain.LandedCostAllocation, error) {
	a, ok := s.landedCosts[id]
	if !ok {
		return nil, domain.ErrLandedCostNotFound
	}
	cp := *a
	return &cp, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func landedReq(movementID, key string, amount float64) domain.AllocateLandedCostRequest {
	return domain.AllocateLandedCostRequest{
		MovementID: movementID, IdempotencyKey: key, Amount: amount, ValuationEvidenceRef: "FREIGHT-INV-77",
		FiscalPeriod: "2026-10", InventoryAccountCode: "INV-ASSET", COGSAccountCode: "COGS", OffsetAccountCode: "FREIGHT-PAYABLE",
	}
}

// issueQty creates, validates and commits an issue of qty from locID and
// returns the commit status code.
func issueQty(t *testing.T, r chi.Router, itemID, locID, key string, qty float64) int {
	t.Helper()
	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locID, Quantity: qty, UOM: "EACH",
		SourceReference: "SO-LC", SourceIdempotencyKey: key, FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", req, "preparer-1")
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)
	if v := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1"); v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}
	return doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/commit", nil, "preparer-1").Code
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestAllocateLandedCost_SplitsBetweenOnHandAndConsumed(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)

	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-lc-r1", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}
	if code := issueQty(t, r, itemID, locID, "idem-lc-i1", 4); code != http.StatusOK {
		t.Fatalf("issue commit failed: %d", code)
	}
	issued := s.movementsByKey["idem-lc-i1"]
	if rr := valueMovement(t, r, issued, nil); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value issue failed: %d", rr.StatusCode)
	}

	rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", landedReq(receipt.MovementID, "lc-1", 12), "approver-2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var a domain.LandedCostAllocation
	_ = json.NewDecoder(rr.Body).Decode(&a)

	// 6 of 10 units remain -> 60% of 12 = 7.20 to inventory, 4.80 to COGS.
	if a.InventoryShare != 7.2 || a.COGSShare != 4.8 || a.Status != domain.LandedCostStatusAccountingEventEmitted {
		t.Fatalf("expected inventory 7.20 / cogs 4.80 / EMITTED, got %+v", a)
	}
	if len(ledger.lastLines) != 3 {
		t.Fatalf("expected 3 journal lines, got %+v", ledger.lastLines)
	}
	dr1, dr2, cr := ledger.lastLines[0], ledger.lastLines[1], ledger.lastLines[2]
	if dr1.AccountCode != "INV-ASSET" || dr1.DebitAmount != 7.2 ||
		dr2.AccountCode != "COGS" || dr2.DebitAmount != 4.8 ||
		cr.AccountCode != "FREIGHT-PAYABLE" || cr.CreditAmount != 12 {
		t.Fatalf("unexpected journal: %+v", ledger.lastLines)
	}
	// On-hand value = 6 units * (2.00 + 7.20/6) = 19.20 = original 12.00 + 7.20.
	if v, _ := s.GetInventoryValue(context.Background(), itemID, locID); math.Abs(v-19.2) > 0.0001 {
		t.Fatalf("expected on-hand value 19.20, got %v", v)
	}
}

func TestAllocateLandedCost_MissingEvidence_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := landedReq("m-1", "lc-e", 5)
	req.ValuationEvidenceRef = ""
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAllocateLandedCost_UnvaluedMovement_Returns422(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-lc-r2", 10) // committed but never valued -> no layer

	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", landedReq(receipt.MovementID, "lc-2", 5), "approver-2"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (no cost layer), got %d: %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != 0 {
		t.Fatalf("nothing may post when there is no layer, got %d posts", ledger.postCalls)
	}
}

// Negative path: late cost must not silently rewrite a closed period.
func TestAllocateLandedCost_LockedPeriod_Refused_NothingApplied(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	pc := &stubPeriodChecker{err: domain.ErrPeriodLocked}
	r := newRouterFull(s, &stubPublisher{}, &stubAuthZ{}, pc, ledger)
	itemID, locID := valuationFixture(t, s, newRouter(s, &stubPublisher{}, &stubAuthZ{}))
	// Build the receipt through an open-period router, then try under a locked one.
	open := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	receipt := createAndCommitReceipt(t, open, itemID, locID, "idem-lc-r3", 10)
	if rr := valueMovement(t, open, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}

	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", landedReq(receipt.MovementID, "lc-3", 5), "approver-2"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 period_locked, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.landedCosts) != 0 || ledger.postCalls != 0 {
		t.Fatalf("a locked period must apply and post nothing (allocations=%d posts=%d)", len(s.landedCosts), ledger.postCalls)
	}
	if v, _ := s.GetInventoryValue(context.Background(), itemID, locID); v != 20 {
		t.Fatalf("layer value must be untouched at 20, got %v", v)
	}
}

func TestAllocateLandedCost_ReplayIsIdempotent_NeverAppliedTwice(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-lc-r4", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}

	req := landedReq(receipt.MovementID, "lc-4", 10)
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2"); rr.Code != http.StatusCreated {
		t.Fatalf("first: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2"); rr.Code != http.StatusOK {
		t.Fatalf("replay: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ledger.postCalls != 1 {
		t.Fatalf("expected exactly 1 journal post across the replay, got %d", ledger.postCalls)
	}
	if v, _ := s.GetInventoryValue(context.Background(), itemID, locID); math.Abs(v-30) > 0.0001 { // 20 + 10, not 20 + 20
		t.Fatalf("expected value 30 (cost applied once), got %v", v)
	}

	// Same key, different amount: refused.
	req.Amount = 99
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2"); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for key reuse with a different amount, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAllocateLandedCost_PostingFailure_ThenRetryResumes(t *testing.T) {
	s := newStubStore()
	ledger := &stubLedger{postErr: errors.New("ledger down")}
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, ledger)
	itemID, locID := valuationFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-lc-r5", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}

	req := landedReq(receipt.MovementID, "lc-5", 10)
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when posting fails, got %d: %s", rr.Code, rr.Body.String())
	}
	ledger.postErr = nil
	rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", req, "approver-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on resume, got %d: %s", rr.Code, rr.Body.String())
	}
	var a domain.LandedCostAllocation
	_ = json.NewDecoder(rr.Body).Decode(&a)
	if a.Status != domain.LandedCostStatusAccountingEventEmitted || a.JournalID == nil {
		t.Fatalf("expected EMITTED with a journal after resume, got %+v", a)
	}
	if v, _ := s.GetInventoryValue(context.Background(), itemID, locID); math.Abs(v-30) > 0.0001 {
		t.Fatalf("cost must be applied exactly once across fail+retry, got value %v", v)
	}
}

func TestAllocateLandedCost_StandardCostItem_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouterWithLedger(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	itemID, locID := valuationFixture(t, s, r)
	s.valuationPolicies[itemID].ValuationMethod = domain.ValuationMethodStandardCost
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-lc-r6", 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}
	if rr := doReq(r, http.MethodPost, "/v1/valuation/landed-costs/", landedReq(receipt.MovementID, "lc-6", 5), "approver-2"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for STANDARD_COST, got %d: %s", rr.Code, rr.Body.String())
	}
}
