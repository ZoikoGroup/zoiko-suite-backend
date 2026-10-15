package handler_test

import (
	"net/http"
	"testing"

	"zoiko.io/asset-management-svc/internal/domain"
)

// book_id is REQUIRED at ValidateAssetEvent for the three book-specific
// event types (spec invariant 2) and not for the others.
func TestValidateAssetEvent_BookSpecificTypes_RequireBookID(t *testing.T) {
	amount := 100.0
	for _, typ := range []string{domain.AssetEventTypeImpairment, domain.AssetEventTypeRevaluation, domain.AssetEventTypeAddition} {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
		id := createActiveAsset(t, s, r, "le-1")
		e := createDraftAssetEvent(t, r, id, typ, domain.CreateAssetEventRequest{Amount: &amount, ValuationEvidenceRef: "EV-1"})
		rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s without book_id: expected 422, got %d: %s", typ, rr.Code, rr.Body.String())
		}
	}
}

func TestValidateAssetEvent_BookSpecificTypes_WithBookID_Succeeds(t *testing.T) {
	amount := 100.0
	for _, typ := range []string{domain.AssetEventTypeImpairment, domain.AssetEventTypeRevaluation, domain.AssetEventTypeAddition} {
		s := newStubStore()
		r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
		id := createActiveAsset(t, s, r, "le-1")
		e := createDraftAssetEvent(t, r, id, typ, domain.CreateAssetEventRequest{Amount: &amount, ValuationEvidenceRef: "EV-1", BookID: "book-1"})
		if e.BookID == nil || *e.BookID != "book-1" {
			t.Fatalf("%s: book_id not persisted on create: %+v", typ, e.BookID)
		}
		rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s with book_id: expected 200, got %d: %s", typ, rr.Code, rr.Body.String())
		}
	}
}

func TestValidateAssetEvent_TransferWithoutBookID_Allowed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveAsset(t, s, r, "le-1")
	e := createDraftAssetEvent(t, r, id, domain.AssetEventTypeTransfer, domain.CreateAssetEventRequest{})
	rr := doReq(r, http.MethodPost, "/v1/asset-events/"+e.EventID+"/validate", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
