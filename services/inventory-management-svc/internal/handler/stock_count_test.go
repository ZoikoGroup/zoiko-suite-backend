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
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	callsBefore := pub.calls
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

	// System quantity is 20 (the committed receipt), observed is 18 — a real
	// variance, so StockCountVarianceDetected (INV-05) must fire internally
	// even though the HTTP response above never carries system_quantity.
	if pub.calls != callsBefore+1 {
		t.Fatalf("expected exactly 1 new publish call (StockCountVarianceDetected) for the observed/system mismatch, got %d new", pub.calls-callsBefore)
	}
}

// ── StockCountVarianceDetected only fires on an actual mismatch ─────────────

func TestRecordBlindCount_NoVariance_DoesNotPublishVarianceEvent(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	callsBefore := pub.calls
	req := domain.RecordBlindCountRequest{ObservedQuantity: 20}
	rr := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", req, "counter-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if pub.calls != callsBefore {
		t.Fatalf("expected no new publish calls when observed quantity matches system quantity, got %d new", pub.calls-callsBefore)
	}
}

// ── GetVarianceReport / GetCountSnapshot / GetAdjustmentStatus / GetCountEvidence ─

func TestGetVarianceReport_OnlyIncludesLinesWithAVariance(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 25}, "counter-1")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/"+countID+"/variance-report", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Variances []struct {
			domain.StockCountLine
			Variance float64 `json:"variance"`
		} `json:"variances"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if len(resp.Variances) != 1 {
		t.Fatalf("expected 1 line with a variance, got %+v", resp.Variances)
	}
	if resp.Variances[0].Variance != 5 { // observed 25 - system 20
		t.Fatalf("expected variance=5, got %v", resp.Variances[0].Variance)
	}
}

func TestGetCountSnapshot_ReturnsFrozenPopulationOnly(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, itemID, locID := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 25}, "counter-1")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/"+countID+"/snapshot", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "observed_quantity") {
		t.Fatalf("snapshot must never include the observation, only the frozen population, got: %s", body)
	}
	var resp struct {
		Lines []struct {
			ItemID         string  `json:"item_id"`
			LocationID     string  `json:"location_id"`
			SystemQuantity float64 `json:"system_quantity"`
		} `json:"lines"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if len(resp.Lines) != 1 || resp.Lines[0].ItemID != itemID || resp.Lines[0].LocationID != locID || resp.Lines[0].SystemQuantity != 20 {
		t.Fatalf("expected 1 frozen line item=%s loc=%s qty=20, got %+v", itemID, locID, resp.Lines)
	}
}

func TestGetAdjustmentStatus_TracksApprovedVsAdjusted(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 25}, "counter-1")

	before := doReq(r, http.MethodGet, "/v1/stock-counts/"+countID+"/adjustment-status", nil, "preparer-1")
	var beforeResp map[string]float64
	_ = json.NewDecoder(before.Body).Decode(&beforeResp)
	if beforeResp["variance_lines"] != 1 || beforeResp["approved_lines"] != 0 || beforeResp["pending_adjustment_lines"] != 0 {
		t.Fatalf("expected 1 variance, 0 approved before approval, got %+v", beforeResp)
	}

	approve := doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "reviewer-2")
	if approve.Code != http.StatusOK {
		t.Fatalf("approve variance failed: %d %s", approve.Code, approve.Body.String())
	}
	afterApprove := doReq(r, http.MethodGet, "/v1/stock-counts/"+countID+"/adjustment-status", nil, "preparer-1")
	var afterApproveResp map[string]float64
	_ = json.NewDecoder(afterApprove.Body).Decode(&afterApproveResp)
	if afterApproveResp["approved_lines"] != 1 || afterApproveResp["pending_adjustment_lines"] != 1 || afterApproveResp["adjusted_lines"] != 0 {
		t.Fatalf("expected 1 approved, 1 pending, 0 adjusted after approval, got %+v", afterApproveResp)
	}

	gen := doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/generate-adjustments", nil, "reviewer-2")
	if gen.Code != http.StatusOK {
		t.Fatalf("generate adjustments failed: %d %s", gen.Code, gen.Body.String())
	}
	afterGen := doReq(r, http.MethodGet, "/v1/stock-counts/"+countID+"/adjustment-status", nil, "preparer-1")
	var afterGenResp map[string]float64
	_ = json.NewDecoder(afterGen.Body).Decode(&afterGenResp)
	if afterGenResp["adjusted_lines"] != 1 || afterGenResp["pending_adjustment_lines"] != 0 {
		t.Fatalf("expected 1 adjusted, 0 pending after generation, got %+v", afterGenResp)
	}
}

func TestGetCountEvidence_AfterAdjustment_IncludesMovement(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 25}, "counter-1")
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "reviewer-2")
	doReq(r, http.MethodPost, "/v1/stock-counts/"+countID+"/generate-adjustments", nil, "reviewer-2")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/lines/"+line.LineID+"/evidence", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]json.RawMessage
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if _, ok := resp["line"]; !ok {
		t.Fatal("expected evidence to include the count line")
	}
	var movement domain.InventoryMovement
	if err := json.Unmarshal(resp["adjustment_movement"], &movement); err != nil {
		t.Fatalf("expected evidence to include the adjustment_movement: %v", err)
	}
	if movement.MovementType != domain.MovementTypeAdjustment {
		t.Fatalf("expected an ADJUSTMENT movement, got %+v", movement)
	}
}

func TestGetCountEvidence_BeforeAdjustment_OmitsMovement(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/lines/"+line.LineID+"/evidence", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]json.RawMessage
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if _, ok := resp["adjustment_movement"]; ok {
		t.Fatal("expected no adjustment_movement before GenerateAdjustmentMovements has run")
	}
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

// ── GetUnapprovedVarianceCount (§9 "Stock count") ────────────────────────────

func TestGetUnapprovedVarianceCount_MissingParams_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodGet, "/v1/stock-counts/unapproved-variance-count?legal_entity_id=le-1", nil, "reader-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing fiscal_period, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetUnapprovedVarianceCount_VarianceRecordedButNeverApproved_CountsOne(t *testing.T) {
	// Proves the real gap this assertion surfaces: RecordBlindCount alone
	// (no ApproveCountVariance) leaves a real variance sitting unapproved
	// — nothing in this service's own commands blocks that today.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 18}, "counter-1")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/unapproved-variance-count?legal_entity_id=le-1&fiscal_period=2026-09", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]int
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["unapproved_variance_count"] != 1 {
		t.Fatalf("expected 1 (observed 18 vs system 20, never approved), got %+v", out)
	}
}

func TestGetUnapprovedVarianceCount_ApprovedVariance_ExcludedFromCount(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 18}, "counter-1")
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/approve-variance", nil, "reviewer-2")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/unapproved-variance-count?legal_entity_id=le-1&fiscal_period=2026-09", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]int
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["unapproved_variance_count"] != 0 {
		t.Fatalf("expected 0 once the variance is approved, got %+v", out)
	}
}

func TestGetUnapprovedVarianceCount_NoVariance_CountsZero(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	countID, _, _ := countFixture(t, s, r)
	freezeCount(t, r, countID)
	line := onlyLine(t, s, countID)

	// Observed exactly matches system quantity (20) — no real variance.
	doReq(r, http.MethodPost, "/v1/stock-counts/lines/"+line.LineID+"/record-count", domain.RecordBlindCountRequest{ObservedQuantity: 20}, "counter-1")

	rr := doReq(r, http.MethodGet, "/v1/stock-counts/unapproved-variance-count?legal_entity_id=le-1&fiscal_period=2026-09", nil, "reader-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]int
	_ = json.NewDecoder(rr.Body).Decode(&out)
	if out["unapproved_variance_count"] != 0 {
		t.Fatalf("expected 0 (observed equals system, no variance), got %+v", out)
	}
}
