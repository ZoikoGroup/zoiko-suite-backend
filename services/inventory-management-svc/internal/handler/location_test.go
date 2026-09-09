package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftLocation(t *testing.T, r chi.Router, legalEntityID, code string, parentID string) domain.InventoryLocation {
	t.Helper()
	req := domain.CreateInventoryLocationRequest{
		LegalEntityID: legalEntityID, LocationCode: code, LocationType: domain.LocationTypeWarehouse, ParentLocationID: parentID,
	}
	rr := doReq(r, http.MethodPost, "/v1/locations/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create location failed: %d %s", rr.Code, rr.Body.String())
	}
	var l domain.InventoryLocation
	_ = json.NewDecoder(rr.Body).Decode(&l)
	return l
}

func activateLocation(t *testing.T, r chi.Router, locationID string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/locations/"+locationID+"/activate", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("activate location failed: %d %s", rr.Code, rr.Body.String())
	}
}

// ── CreateInventoryLocation ──────────────────────────────────────────────────

func TestCreateInventoryLocation_InvalidType_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateInventoryLocationRequest{LegalEntityID: "le-1", LocationCode: "WH-1", LocationType: "NOT_A_TYPE"}
	rr := doReq(r, http.MethodPost, "/v1/locations/", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateInventoryLocation_DuplicateCode_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createDraftLocation(t, r, "le-1", "WH-DUP", "")

	req := domain.CreateInventoryLocationRequest{LegalEntityID: "le-1", LocationCode: "WH-DUP", LocationType: domain.LocationTypeWarehouse}
	rr := doReq(r, http.MethodPost, "/v1/locations/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #1: reparenting across legal entities ─────────────────────

func TestCreateInventoryLocation_ParentInDifferentEntity_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	parent := createDraftLocation(t, r, "le-2", "WH-PARENT", "")

	req := domain.CreateInventoryLocationRequest{LegalEntityID: "le-1", LocationCode: "WH-CHILD", LocationType: domain.LocationTypeBin, ParentLocationID: parent.LocationID}
	rr := doReq(r, http.MethodPost, "/v1/locations/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 cross-entity parent, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReparentLocationControlled_AcrossLegalEntities_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	a := createDraftLocation(t, r, "le-1", "WH-A", "")
	b := createDraftLocation(t, r, "le-2", "WH-B", "")

	req := domain.ReparentLocationRequest{NewParentLocationID: b.LocationID}
	rr := doReq(r, http.MethodPost, "/v1/locations/"+a.LocationID+"/reparent", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── Negative path #2: circular hierarchy ─────────────────────────────────────

func TestReparentLocationControlled_Circular_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	a := createDraftLocation(t, r, "le-1", "WH-A2", "")
	b := createDraftLocation(t, r, "le-1", "WH-B2", a.LocationID) // b's parent is a

	// Attempt to make a's parent be b — a cycle: a -> b -> a
	req := domain.ReparentLocationRequest{NewParentLocationID: b.LocationID}
	rr := doReq(r, http.MethodPost, "/v1/locations/"+a.LocationID+"/reparent", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 circular hierarchy, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReparentLocationControlled_SelfParent_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	a := createDraftLocation(t, r, "le-1", "WH-SELF", "")

	req := domain.ReparentLocationRequest{NewParentLocationID: a.LocationID}
	rr := doReq(r, http.MethodPost, "/v1/locations/"+a.LocationID+"/reparent", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestReparentLocationControlled_ValidReparent_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	a := createDraftLocation(t, r, "le-1", "WH-VALID-A", "")
	b := createDraftLocation(t, r, "le-1", "WH-VALID-B", "")

	req := domain.ReparentLocationRequest{NewParentLocationID: a.LocationID}
	rr := doReq(r, http.MethodPost, "/v1/locations/"+b.LocationID+"/reparent", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.parents[b.LocationID] == nil || *s.parents[b.LocationID] != a.LocationID {
		t.Fatalf("expected b's parent to be a, got %v", s.parents[b.LocationID])
	}
}

// ── Negative path #4: quarantine release requires a different principal ────

func TestSetQuarantineState_SelfRelease_Returns403(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-Q1", "")
	activateLocation(t, r, l.LocationID)

	setQ := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/quarantine", domain.SetQuarantineStateRequest{Quarantine: true, Reason: "contamination"}, "inspector-1")
	if setQ.Code != http.StatusOK {
		t.Fatalf("set quarantine failed: %d %s", setQ.Code, setQ.Body.String())
	}

	release := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/quarantine", domain.SetQuarantineStateRequest{Quarantine: false, Reason: "cleared"}, "inspector-1")
	if release.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-release, got %d: %s", release.Code, release.Body.String())
	}
}

func TestSetQuarantineState_ReleaseByDifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-Q2", "")
	activateLocation(t, r, l.LocationID)

	setQ := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/quarantine", domain.SetQuarantineStateRequest{Quarantine: true, Reason: "contamination"}, "inspector-1")
	if setQ.Code != http.StatusOK {
		t.Fatalf("set quarantine failed: %d %s", setQ.Code, setQ.Body.String())
	}

	release := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/quarantine", domain.SetQuarantineStateRequest{Quarantine: false, Reason: "cleared"}, "inspector-2")
	if release.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", release.Code, release.Body.String())
	}
	if s.locations[l.LocationID].Status != domain.LocationStatusActive {
		t.Fatalf("expected ACTIVE after release, got %q", s.locations[l.LocationID].Status)
	}
}

func TestSetQuarantineState_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-Q3", "")
	activateLocation(t, r, l.LocationID)

	rr := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/quarantine", map[string]any{"quarantine": true}, "inspector-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── AmendLocationMetadata never touches legal_entity_id ──────────────────────

func TestAmendLocationMetadata_NeverChangesLegalEntity(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-AMEND", "")

	newDesc := "Updated description"
	req := domain.AmendLocationMetadataRequest{Description: &newDesc}
	rr := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/amend", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.locations[l.LocationID].LegalEntityID != "le-1" {
		t.Fatalf("expected legal_entity_id unchanged, got %q", s.locations[l.LocationID].LegalEntityID)
	}
}

// ── RetireLocation ────────────────────────────────────────────────────────────

func TestRetireLocation_FromDraft_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-RETIRE-1", "")

	rr := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/retire", domain.RetireLocationRequest{Reason: "closing site"}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retiring a DRAFT location, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRetireLocation_FromActive_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	l := createDraftLocation(t, r, "le-1", "WH-RETIRE-2", "")
	activateLocation(t, r, l.LocationID)

	rr := doReq(r, http.MethodPost, "/v1/locations/"+l.LocationID+"/retire", domain.RetireLocationRequest{Reason: "closing site"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.locations[l.LocationID].Status != domain.LocationStatusRetired {
		t.Fatalf("expected RETIRED, got %q", s.locations[l.LocationID].Status)
	}
}

// ── ListEligibleLocations ─────────────────────────────────────────────────────

func TestListEligibleLocations_OnlyReturnsActive(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	active := createDraftLocation(t, r, "le-1", "WH-ELIGIBLE", "")
	activateLocation(t, r, active.LocationID)
	createDraftLocation(t, r, "le-1", "WH-STILL-DRAFT", "")

	rr := doReq(r, http.MethodGet, "/v1/locations/?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var list []domain.InventoryLocation
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 1 || list[0].LocationID != active.LocationID {
		t.Fatalf("expected exactly the ACTIVE location, got %+v", list)
	}
}
