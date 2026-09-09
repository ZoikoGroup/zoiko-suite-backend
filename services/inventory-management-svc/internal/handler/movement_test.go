package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// movementFixture creates an ACTIVE item (with a FIFO valuation policy)
// and two ACTIVE locations, returning their IDs.
func movementFixture(t *testing.T, s *stubStore, r chi.Router) (itemID, locA, locB string) {
	t.Helper()
	itemID = createActiveItem(t, s, r, "le-1", "SKU-MV-"+uniqueSuffix())
	la := createDraftLocation(t, r, "le-1", "WH-MV-A-"+uniqueSuffix(), "")
	activateLocation(t, r, la.LocationID)
	lb := createDraftLocation(t, r, "le-1", "WH-MV-B-"+uniqueSuffix(), "")
	activateLocation(t, r, lb.LocationID)
	return itemID, la.LocationID, lb.LocationID
}

var suffixCounter int

func uniqueSuffix() string {
	suffixCounter++
	return string(rune('A' + suffixCounter%26))
}

func createAndCommitReceipt(t *testing.T, r chi.Router, itemID, destLocID, idemKey string, qty float64) domain.InventoryMovement {
	t.Helper()
	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: destLocID, Quantity: qty, UOM: "EACH",
		SourceReference: "PO-1", SourceIdempotencyKey: idemKey, FiscalPeriod: "2026-09",
	}
	rr := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("receive failed: %d %s", rr.Code, rr.Body.String())
	}
	var m domain.InventoryMovement
	_ = json.NewDecoder(rr.Body).Decode(&m)

	v := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}
	c := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/commit", nil, "preparer-1")
	if c.Code != http.StatusOK {
		t.Fatalf("commit failed: %d %s", c.Code, c.Body.String())
	}
	var committed domain.InventoryMovement
	_ = json.NewDecoder(c.Body).Decode(&committed)
	return committed
}

// ── CreateInventoryMovement — field-shape enforcement ────────────────────────

func TestReceiveInventory_WithSourceLocation_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locA, DestinationLocationID: locA, Quantity: 10, UOM: "EACH",
		SourceReference: "PO-1", SourceIdempotencyKey: "k1", FiscalPeriod: "2026-09",
	}
	rr := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestIssueInventory_WithDestinationLocation_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locA, DestinationLocationID: locA, Quantity: 10, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "k2", FiscalPeriod: "2026-09",
	}
	rr := doReq(r, http.MethodPost, "/v1/movements/issue", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #1: duplicate idempotency key returns original result ────

func TestCreateInventoryMovement_DuplicateIdempotencyKey_ReturnsOriginal(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, _, locB := movementFixture(t, s, r)

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locB, Quantity: 5, UOM: "EACH",
		SourceReference: "PO-DUP", SourceIdempotencyKey: "idem-dup", FiscalPeriod: "2026-09",
	}
	first := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first receive failed: %d %s", first.Code, first.Body.String())
	}
	var firstM domain.InventoryMovement
	_ = json.NewDecoder(first.Body).Decode(&firstM)

	second := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	if second.Code != http.StatusCreated {
		t.Fatalf("second receive (duplicate key) failed: %d %s", second.Code, second.Body.String())
	}
	var secondM domain.InventoryMovement
	_ = json.NewDecoder(second.Body).Decode(&secondM)
	if secondM.MovementID != firstM.MovementID {
		t.Fatalf("expected the original movement returned for a duplicate idempotency key, got a new one: %q vs %q", secondM.MovementID, firstM.MovementID)
	}
}

// ── ValidateMovement: INV-01/INV-02's own deferred negative paths land here ─

func TestValidateMovement_LotRequiredButMissing_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, _, locB := movementFixture(t, s, r)
	s.trackingPolicies[itemID] = &domain.TrackingPolicy{ItemID: itemID, RequiresLotTracking: true}

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locB, Quantity: 5, UOM: "EACH",
		SourceReference: "PO-LOT", SourceIdempotencyKey: "idem-lot", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)

	rr := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValidateMovement_RetiredLocation_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, _, locB := movementFixture(t, s, r)
	s.locations[locB].Status = domain.LocationStatusRetired

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locB, Quantity: 5, UOM: "EACH",
		SourceReference: "PO-RET", SourceIdempotencyKey: "idem-ret", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)

	rr := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 movement into a retired location, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #2: serial residency ───────────────────────────────────────

func TestCommitMovement_ReceiveDuplicateSerial_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, _, locB := movementFixture(t, s, r)

	receive := func(idemKey string) *http.Response {
		req := domain.CreateInventoryMovementRequest{
			ItemID: itemID, DestinationLocationID: locB, Quantity: 1, UOM: "EACH",
			SourceReference: "PO-SN", SourceIdempotencyKey: idemKey, FiscalPeriod: "2026-09", SerialNumber: "SN-001",
		}
		rr := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
		return rr.Result()
	}

	first := receive("sn-1")
	var firstM domain.InventoryMovement
	_ = json.NewDecoder(first.Body).Decode(&firstM)
	v1 := doReq(r, http.MethodPost, "/v1/movements/"+firstM.MovementID+"/validate", nil, "preparer-1")
	if v1.Code != http.StatusOK {
		t.Fatalf("validate 1 failed: %d %s", v1.Code, v1.Body.String())
	}
	c1 := doReq(r, http.MethodPost, "/v1/movements/"+firstM.MovementID+"/commit", nil, "preparer-1")
	if c1.Code != http.StatusOK {
		t.Fatalf("commit 1 failed: %d %s", c1.Code, c1.Body.String())
	}

	second := receive("sn-2")
	var secondM domain.InventoryMovement
	_ = json.NewDecoder(second.Body).Decode(&secondM)
	v2 := doReq(r, http.MethodPost, "/v1/movements/"+secondM.MovementID+"/validate", nil, "preparer-1")
	if v2.Code != http.StatusOK {
		t.Fatalf("validate 2 failed: %d %s", v2.Code, v2.Body.String())
	}
	c2 := doReq(r, http.MethodPost, "/v1/movements/"+secondM.MovementID+"/commit", nil, "preparer-1")
	if c2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 duplicate serial receipt, got %d: %s", c2.Code, c2.Body.String())
	}
}

// ── Negative path #3: negative stock ─────────────────────────────────────────

func TestCommitMovement_IssueMoreThanOnHand_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)
	createAndCommitReceipt(t, r, itemID, locA, "idem-recv-1", 5)

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, SourceLocationID: locA, Quantity: 10, UOM: "EACH",
		SourceReference: "SO-1", SourceIdempotencyKey: "idem-issue-1", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/issue", req, "preparer-1")
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)
	v := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 negative stock, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #4: hard-closed period ─────────────────────────────────────

func TestCommitMovement_PeriodLocked_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouterWithPeriodChecker(s, &stubPublisher{}, &stubAuthZ{}, &stubPeriodChecker{err: domain.ErrPeriodLocked})
	itemID, _, locB := movementFixture(t, s, r)

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locB, Quantity: 5, UOM: "EACH",
		SourceReference: "PO-LOCK", SourceIdempotencyKey: "idem-lock", FiscalPeriod: "2026-01",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)
	v := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1")
	if v.Code != http.StatusOK {
		t.Fatalf("validate failed: %d %s", v.Code, v.Body.String())
	}

	rr := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 period locked, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Full receive lifecycle + on-hand derivation ──────────────────────────────

func TestFullReceiveLifecycle_UpdatesOnHand(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)
	createAndCommitReceipt(t, r, itemID, locA, "idem-lifecycle", 25)

	rr := doReq(r, http.MethodGet, "/v1/on-hand?item_id="+itemID+"&location_id="+locA, nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["on_hand"] != "25" {
		t.Fatalf("expected on_hand=25, got %q", resp["on_hand"])
	}
}

// ── ReverseMovement reverts the on-hand effect ───────────────────────────────

func TestReverseMovement_RevertsOnHand(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)
	committed := createAndCommitReceipt(t, r, itemID, locA, "idem-reverse-1", 15)

	rr := doReq(r, http.MethodPost, "/v1/movements/"+committed.MovementID+"/reverse", domain.ReverseMovementRequest{Reason: "wrong quantity"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("reverse failed: %d %s", rr.Code, rr.Body.String())
	}

	onHandReq := doReq(r, http.MethodGet, "/v1/on-hand?item_id="+itemID+"&location_id="+locA, nil, "preparer-1")
	var resp map[string]string
	_ = json.NewDecoder(onHandReq.Body).Decode(&resp)
	if resp["on_hand"] != "0" {
		t.Fatalf("expected on_hand=0 after reversal, got %q", resp["on_hand"])
	}
}

func TestReverseMovement_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locA, _ := movementFixture(t, s, r)
	committed := createAndCommitReceipt(t, r, itemID, locA, "idem-reverse-2", 5)

	rr := doReq(r, http.MethodPost, "/v1/movements/"+committed.MovementID+"/reverse", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── AdjustInventoryFromApprovedCount uses a distinct authz action ───────────

func TestAdjustInventoryFromApprovedCount_UsesDistinctAction(t *testing.T) {
	s := newStubStore()
	setupRouter := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, _, locB := movementFixture(t, s, setupRouter)

	denyingRouter := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locB, Quantity: 3, UOM: "EACH",
		SourceReference: "COUNT-1", SourceIdempotencyKey: "idem-adj-1", FiscalPeriod: "2026-09",
	}
	rr := doReq(denyingRouter, http.MethodPost, "/v1/movements/adjust", req, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (authz denied for adjust action), got %d: %s", rr.Code, rr.Body.String())
	}
}
