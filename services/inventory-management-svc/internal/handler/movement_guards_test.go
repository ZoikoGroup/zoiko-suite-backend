package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// Domain scenario #20: "Inventory movement committed with no source identity".
// Both the business source reference and the idempotency key are mandatory.
func TestCreateMovement_NoSourceIdentity_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r)

	base := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locID, Quantity: 5, UOM: "EACH", FiscalPeriod: "2026-09",
		SourceReference: "PO-1", SourceIdempotencyKey: "idem-src-1",
	}
	for name, mutate := range map[string]func(*domain.CreateInventoryMovementRequest){
		"missing source_reference":       func(q *domain.CreateInventoryMovementRequest) { q.SourceReference = "" },
		"missing source_idempotency_key": func(q *domain.CreateInventoryMovementRequest) { q.SourceIdempotencyKey = "" },
	} {
		req := base
		mutate(&req)
		if rr := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1"); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", name, rr.Code, rr.Body.String())
		}
	}
	if len(s.movements) != 0 {
		t.Fatalf("no movement may be created without source identity, found %d", len(s.movements))
	}
}

// Domain scenario #19: "UOM conversion ambiguity guessed". A movement in a UOM
// other than the item's base UOM is refused — never silently converted.
func TestMovement_UOMDifferentFromBase_NeverCommits(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID := valuationFixture(t, s, r) // base UOM EACH

	req := domain.CreateInventoryMovementRequest{
		ItemID: itemID, DestinationLocationID: locID, Quantity: 5, UOM: "KG",
		SourceReference: "PO-1", SourceIdempotencyKey: "idem-uom-1", FiscalPeriod: "2026-09",
	}
	create := doReq(r, http.MethodPost, "/v1/movements/receive", req, "preparer-1")
	if create.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", create.Code, create.Body.String())
	}
	var m domain.InventoryMovement
	_ = json.NewDecoder(create.Body).Decode(&m)

	// Refused at validate or commit — what matters is it never commits.
	refused := false
	if v := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/validate", nil, "preparer-1"); v.Code == http.StatusUnprocessableEntity {
		refused = true
	}
	if !refused {
		if c := doReq(r, http.MethodPost, "/v1/movements/"+m.MovementID+"/commit", nil, "preparer-1"); c.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected a 422 refusal of the UOM mismatch, got %d: %s", c.Code, c.Body.String())
		}
	}
	if got := s.movements[m.MovementID].Status; got == domain.MovementStatusCommitted {
		t.Fatalf("a movement in a non-base UOM must never reach COMMITTED")
	}
}
