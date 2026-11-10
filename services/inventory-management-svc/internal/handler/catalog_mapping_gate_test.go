package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// BIZ-07 failure/degradation: "Missing required mapping blocks use in
// protected transaction." A version without TAX and ACCOUNTING mappings
// must never reach ACTIVE.

func createApprovedOffering(t *testing.T, r chi.Router, sku string) (offeringID, versionID string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/catalog/offerings/", domain.CreateOfferingRequest{
		LegalEntityID: "le-1", SKUCode: sku, Category: "GOODS", OwnerPrincipalID: "owner-1",
		Description: "Widget", Unit: "EACH",
	}, "owner-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create offering: %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Offering domain.Offering        `json:"offering"`
		Version  domain.OfferingVersion `json:"version"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&out)
	base := "/v1/catalog/offerings/" + out.Offering.OfferingID + "/versions/" + out.Version.VersionID
	if rr := doReq(r, http.MethodPost, base+"/approve", nil, "reviewer-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	return out.Offering.OfferingID, out.Version.VersionID
}

func linkMapping(t *testing.T, r chi.Router, offeringID, versionID, mappingType string) {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/catalog/offerings/"+offeringID+"/versions/"+versionID+"/mappings",
		domain.LinkMappingRequest{MappingType: mappingType, MappingRef: mappingType + "-REF"}, "specialist-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("link %s mapping: %d %s", mappingType, rr.Code, rr.Body.String())
	}
}

func activatePath(offeringID, versionID string) string {
	return "/v1/catalog/offerings/" + offeringID + "/versions/" + versionID + "/activate"
}

func TestActivateOffering_NoMappings_Returns422MappingRequired(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-NOMAP")
	rr := doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "CATALOG_MAPPING_REQUIRED") || !strings.Contains(body, "TAX") || !strings.Contains(body, "ACCOUNTING") {
		t.Fatalf("expected CATALOG_MAPPING_REQUIRED naming TAX and ACCOUNTING, got %s", body)
	}
}

func TestActivateOffering_OnlyTaxMapping_StillBlocked(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-TAXONLY")
	linkMapping(t, r, oid, vid, "TAX")
	rr := doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "ACCOUNTING") || strings.Contains(body, "TAX,") {
		t.Fatalf("expected only ACCOUNTING reported missing, got %s", body)
	}
}

func TestActivateOffering_ProductMappingDoesNotSatisfyRequirement(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-PRODONLY")
	linkMapping(t, r, oid, vid, "PRODUCT")
	rr := doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActivateOffering_AllRequiredMappings_Succeeds(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-FULL")
	linkMapping(t, r, oid, vid, "TAX")
	linkMapping(t, r, oid, vid, "ACCOUNTING")
	rr := doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestMissingRequiredCatalogMappings(t *testing.T) {
	if got := domain.MissingRequiredCatalogMappings(nil); len(got) != 2 {
		t.Fatalf("expected both missing, got %v", got)
	}
	full := []domain.CatalogMapping{{MappingType: "TAX"}, {MappingType: "ACCOUNTING"}}
	if got := domain.MissingRequiredCatalogMappings(full); len(got) != 0 {
		t.Fatalf("expected none missing, got %v", got)
	}
}

// ── VersionUsability ─────────────────────────────────────────────────────────

func usabilityPath(offeringID, versionID string) string {
	return "/v1/catalog/offerings/" + offeringID + "/versions/" + versionID + "/usability"
}

type usabilityBody struct {
	Usable          bool     `json:"usable"`
	Status          string   `json:"status"`
	MissingMappings []string `json:"missing_mappings"`
	Reason          string   `json:"reason"`
}

func getUsability(t *testing.T, r chi.Router, offeringID, versionID string) usabilityBody {
	t.Helper()
	rr := doReq(r, http.MethodGet, usabilityPath(offeringID, versionID), nil, "consumer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("usability: %d %s", rr.Code, rr.Body.String())
	}
	var b usabilityBody
	_ = json.NewDecoder(rr.Body).Decode(&b)
	return b
}

func TestVersionUsability_ApprovedNotActive_NotUsable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-U1")
	b := getUsability(t, r, oid, vid)
	if b.Usable || b.Status != domain.CatalogVersionStatusApproved || len(b.MissingMappings) != 2 {
		t.Fatalf("expected not usable, APPROVED, 2 missing; got %+v", b)
	}
}

func TestVersionUsability_ActiveWithMappings_Usable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-U2")
	linkMapping(t, r, oid, vid, "TAX")
	linkMapping(t, r, oid, vid, "ACCOUNTING")
	if rr := doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1"); rr.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rr.Code, rr.Body.String())
	}
	b := getUsability(t, r, oid, vid)
	if !b.Usable || b.Reason != "" || len(b.MissingMappings) != 0 {
		t.Fatalf("expected usable; got %+v", b)
	}
}

func TestVersionUsability_SuspendedAfterActivation_NotUsable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-U3")
	linkMapping(t, r, oid, vid, "TAX")
	linkMapping(t, r, oid, vid, "ACCOUNTING")
	doReq(r, http.MethodPost, activatePath(oid, vid), nil, "reviewer-1")
	rr := doReq(r, http.MethodPost, "/v1/catalog/offerings/"+oid+"/versions/"+vid+"/suspend", map[string]string{"reason": "recall"}, "reviewer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", rr.Code, rr.Body.String())
	}
	b := getUsability(t, r, oid, vid)
	if b.Usable || b.Status != domain.CatalogVersionStatusSuspended {
		t.Fatalf("expected not usable, SUSPENDED; got %+v", b)
	}
}

func TestVersionUsability_WrongOfferingOrUnknownVersion_404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-U4")
	otherOID, _ := createApprovedOffering(t, r, "SKU-U5")
	if rr := doReq(r, http.MethodGet, usabilityPath(otherOID, vid), nil, "consumer-1"); rr.Code != http.StatusNotFound {
		t.Fatalf("version under the wrong offering: expected 404, got %d", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, usabilityPath(oid, "00000000-0000-0000-0000-000000000000"), nil, "consumer-1"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown version: expected 404, got %d", rr.Code)
	}
}

func TestVersionUsability_RequiresPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	oid, vid := createApprovedOffering(t, r, "SKU-U6")
	if rr := doReq(r, http.MethodGet, usabilityPath(oid, vid), nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a principal, got %d", rr.Code)
	}
}
