package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// countFixture creates an ACTIVE item, one ACTIVE location, a committed
// receipt of 20 units into that location, and a PLANNED stock count
// scoped to it. Returns the count ID, item ID and location ID.
func countFixture(t *testing.T, s *stubStore, r chi.Router) (countID, itemID, locID string) {
	t.Helper()
	itemID, locID = valuationFixture(t, s, r)
	createAndCommitReceipt(t, r, itemID, locID, "idem-count-"+uniqueSuffix(), 20)

	req := domain.CreateStockCountRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-09", LocationIDs: []string{locID}}
	rr := doReq(r, http.MethodPost, "/v1/stock-counts/", req, "planner-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create stock count failed: %d %s", rr.Code, rr.Body.String())
	}
	var sc domain.StockCount
	_ = json.NewDecoder(rr.Body).Decode(&sc)
	return sc.CountID, itemID, locID
}

func freezeCount(t *testing.T, r chi.Router, countID string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/freeze", nil, "planner-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("freeze count failed: %d %s", rr.Code, rr.Body.String())
	}
}

func onlyLine(t *testing.T, s *stubStore, countID string) *domain.StockCountLine {
	t.Helper()
	for _, l := range s.countLines {
		if l.CountID == countID {
			return l
		}
	}
	t.Fatalf("expected exactly one count line for count %s, found none", countID)
	return nil
}

// ── CreateStockCount / FreezeCountPopulation ─────────────────────────────────

func TestCreateStockCount_MissingLocations_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateStockCountRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-09"}
	rr := doReq(r, http.MethodPost, "/v1/stock-counts/", req, "planner-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestFreezeCountPopulation_CapturesSystemQuantity(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, itemID, locID := countFixture(t, s, r)

	freezeCount(t, r, countID)

	line := onlyLine(t, s, countID)
	if line.ItemID != itemID || line.LocationID != locID || line.SystemQuantity != 20 {
		t.Fatalf("expected frozen line for item=%s loc=%s qty=20, got %+v", itemID, locID, line)
	}
	if line.Status != domain.CountLineStatusPending {
		t.Fatalf("expected PENDING, got %q", line.Status)
	}
}

// ── Negative path #1: blind count never exposes system quantity ─────────────

func TestRecordBlindCount_ResponseNeverExposesSystemQuantity(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	req := domain.RecordBlindCountRequest{ObservedQuantity: 18}
	rr := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", req, "counter-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "system_quantity") {
		t.Fatalf("RecordBlindCount's own response must never contain system_quantity, got: %s", body)
	}
	var resp domain.RecordedBlindCount
	_ = json.NewDecoder(rr.Body).Decode(&resp)
}

// ── Negative path #3: variance approved by the same counter ─────────────────

func TestApproveCountVariance_SameCounter_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 18}, "counter-1")

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "counter-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveCountVariance_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 18}, "counter-1")

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "reviewer-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.countLines[line.LineID].Status != domain.CountLineStatusVarianceApproved {
		t.Fatalf("expected VARIANCE_APPROVED, got %q", s.countLines[line.LineID].Status)
	}
}

// ── Negative path #2: population immutable after freeze (structural) ────────

func TestFreezeCountPopulation_AlreadyFrozen_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/freeze", nil, "planner-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 re-freezing an already-frozen count, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #4: GenerateAdjustmentMovements is the only path to on-hand ─

func TestGenerateAdjustmentMovements_CreatesRealINV03Movement(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, itemID, locID := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 25}, "counter-1")
	approve := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "reviewer-2")
	if approve.Code != http.StatusOK {
		t.Fatalf("approve variance failed: %d %s", approve.Code, approve.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/generate-adjustments", nil, "reviewer-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	updatedLine := s.countLines[line.LineID]
	if updatedLine.Status != domain.CountLineStatusAdjustmentGenerated || updatedLine.AdjustmentMovementID == nil {
		t.Fatalf("expected ADJUSTMENT_GENERATED with a linked movement, got %+v", updatedLine)
	}
	movement, ok := s.movements[*updatedLine.AdjustmentMovementID]
	if !ok {
		t.Fatalf("expected the linked adjustment_movement_id to be a real committed INV-03 movement")
	}
	if movement.MovementType != domain.MovementTypeAdjustment || movement.Status != domain.MovementStatusCommitted {
		t.Fatalf("expected a COMMITTED ADJUSTMENT movement, got %+v", movement)
	}
	if movement.DestinationLocationID == nil || *movement.DestinationLocationID != locID || movement.Quantity != 5 {
		t.Fatalf("expected a +5 increase at %s (observed 25 - system 20), got %+v", locID, movement)
	}
	if movement.ItemID != itemID {
		t.Fatalf("expected movement for item %s, got %s", itemID, movement.ItemID)
	}
}

func TestGenerateAdjustmentMovements_NoVarianceApprovedLines_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/generate-adjustments", nil, "reviewer-2")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── CertifyStockCount / CancelStockCount ──────────────────────────────────────

func TestCertifyStockCount_BeforeAdjustmentsGenerated_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/certify", nil, "certifier-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCancelStockCount_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/cancel", map[string]string{}, "planner-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCancelStockCount_BeforeCertification_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)

	rr := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/cancel", domain.CancelStockCountRequest{Reason: "wrong scope"}, "planner-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.stockCounts[countID].Status != domain.StockCountStatusCancelled {
		t.Fatalf("expected CANCELLED, got %q", s.stockCounts[countID].Status)
	}
}
