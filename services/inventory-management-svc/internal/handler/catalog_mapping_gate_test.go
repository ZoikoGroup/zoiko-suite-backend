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
