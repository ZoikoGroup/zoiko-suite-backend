package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/handler"
	"zoiko.io/inventory-management-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	items map[string]*domain.InventoryItem

	trackingPolicies  map[string]*domain.TrackingPolicy // current version, keyed by item_id
	valuationPolicies map[string]*domain.ValuationPolicy

	createErr error
}

func newStubStore() *stubStore {
	return &stubStore{
		items:             make(map[string]*domain.InventoryItem),
		trackingPolicies:  make(map[string]*domain.TrackingPolicy),
		valuationPolicies: make(map[string]*domain.ValuationPolicy),
	}
}

func (s *stubStore) CreateItem(_ context.Context, it *domain.InventoryItem) error {
	if s.createErr != nil {
		return s.createErr
	}
	for _, existing := range s.items {
		if existing.LegalEntityID == it.LegalEntityID && existing.SKU == it.SKU {
			return domain.ErrDuplicateSKU
		}
	}
	cp := *it
	s.items[it.ItemID] = &cp
	return nil
}

func (s *stubStore) GetItem(_ context.Context, itemID string) (*domain.InventoryItem, error) {
	it, ok := s.items[itemID]
	if !ok {
		return nil, domain.ErrItemNotFound
	}
	cp := *it
	return &cp, nil
}

func (s *stubStore) ListItems(_ context.Context, legalEntityID string) ([]domain.InventoryItem, error) {
	var out []domain.InventoryItem
	for _, it := range s.items {
		if it.LegalEntityID == legalEntityID {
			out = append(out, *it)
		}
	}
	return out, nil
}

func (s *stubStore) ActivateItem(_ context.Context, itemID, principalID string, at time.Time) error {
	it, ok := s.items[itemID]
	if !ok || it.Status != domain.ItemStatusDraft {
		return domain.ErrInvalidItemTransition
	}
	it.Status, it.ActivatedAt, it.ActivatedByPrincipalID = domain.ItemStatusActive, &at, &principalID
	return nil
}

func (s *stubStore) RetireItem(_ context.Context, itemID, principalID, reason string, at time.Time) error {
	it, ok := s.items[itemID]
	if !ok || it.Status != domain.ItemStatusActive {
		return domain.ErrInvalidItemTransition
	}
	it.Status, it.RetiredAt, it.RetiredByPrincipalID, it.RetirementReason = domain.ItemStatusRetired, &at, &principalID, &reason
	return nil
}

func (s *stubStore) AmendProfile(_ context.Context, itemID string, description, itemType, physicalCharacteristics *string) error {
	it, ok := s.items[itemID]
	if !ok {
		return domain.ErrItemNotFound
	}
	if description != nil {
		it.Description = *description
	}
	if itemType != nil {
		it.ItemType = *itemType
	}
	if physicalCharacteristics != nil {
		it.PhysicalCharacteristics = *physicalCharacteristics
	}
	return nil
}

func (s *stubStore) LinkCatalogItem(_ context.Context, itemID, catalogItemID string) error {
	it, ok := s.items[itemID]
	if !ok {
		return domain.ErrItemNotFound
	}
	it.CatalogItemID = &catalogItemID
	return nil
}

func (s *stubStore) SetTrackingPolicy(_ context.Context, p *domain.TrackingPolicy, _ time.Time) error {
	cp := *p
	s.trackingPolicies[p.ItemID] = &cp
	return nil
}

func (s *stubStore) GetCurrentTrackingPolicy(_ context.Context, itemID string) (*domain.TrackingPolicy, error) {
	p, ok := s.trackingPolicies[itemID]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (s *stubStore) SetValuationPolicy(_ context.Context, p *domain.ValuationPolicy) error {
	cp := *p
	s.valuationPolicies[p.ItemID] = &cp
	return nil
}

func (s *stubStore) GetCurrentValuationPolicy(_ context.Context, itemID string) (*domain.ValuationPolicy, error) {
	p, ok := s.valuationPolicies[itemID]
	if !ok {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (s *stubStore) GetProfileAsOf(_ context.Context, itemID string, _ time.Time) (*domain.TrackingPolicy, *domain.ValuationPolicy, error) {
	return s.trackingPolicies[itemID], s.valuationPolicies[itemID], nil
}

var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct{ calls int }

func (p *stubPublisher) PublishInventoryItemCreated(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryItemActivated(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryPolicyChanged(_ context.Context, _, _, _, _, _, _ string) {
	p.calls++
}
func (p *stubPublisher) PublishInventoryItemRetired(_ context.Context, _, _ string, _ domain.InventoryItem) {
	p.calls++
}

var _ handler.Publisher = (*stubPublisher)(nil)

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createDraftItem(t *testing.T, r chi.Router, legalEntityID, sku string) domain.InventoryItem {
	t.Helper()
	req := domain.CreateInventoryItemRequest{LegalEntityID: legalEntityID, SKU: sku, Description: "Widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", rr.Code, rr.Body.String())
	}
	var it domain.InventoryItem
	_ = json.NewDecoder(rr.Body).Decode(&it)
	return it
}

func setValuationPolicy(t *testing.T, r chi.Router, itemID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: domain.ValuationMethodFIFO, EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/items/"+itemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("set valuation policy failed: %d %s", rr.Code, rr.Body.String())
	}
}

func createActiveItem(t *testing.T, s *stubStore, r chi.Router, legalEntityID, sku string) string {
	t.Helper()
	_ = s
	it := createDraftItem(t, r, legalEntityID, sku)
	setValuationPolicy(t, r, it.ItemID)
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/activate", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("activate failed: %d %s", rr.Code, rr.Body.String())
	}
	return it.ItemID
}

// ── CreateInventoryItem ──────────────────────────────────────────────────────

func TestCreateInventoryItem_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/items/", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateInventoryItem_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-1")
	if it.Status != domain.ItemStatusDraft {
		t.Fatalf("expected DRAFT, got %q", it.Status)
	}
}

func TestCreateInventoryItem_DuplicateSKU_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createDraftItem(t, r, "le-1", "SKU-DUP")

	req := domain.CreateInventoryItemRequest{LegalEntityID: "le-1", SKU: "SKU-DUP", Description: "Another widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ActivateInventoryItem ("Missing valuation policy blocks ... valuation") ──

func TestActivateInventoryItem_NoValuationPolicy_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-2")

	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/activate", nil, "approver-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestActivateInventoryItem_WithValuationPolicy_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-3")
	_ = id
	if s.items[id].Status != domain.ItemStatusActive {
		t.Fatalf("expected ACTIVE, got %q", s.items[id].Status)
	}
}

// ── SetValuationPolicyFutureEffective ("Retroactive valuation-policy change") ─

func TestSetValuationPolicy_PastEffectiveDate_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-4")

	past := time.Now().UTC().Add(-time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: domain.ValuationMethodFIFO, EffectiveFrom: &past}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retroactive valuation policy, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetValuationPolicy_FutureEffectiveDate_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-5")
	setValuationPolicy(t, r, it.ItemID)
	if s.valuationPolicies[it.ItemID] == nil {
		t.Fatalf("expected a valuation policy to be recorded")
	}
}

func TestSetValuationPolicy_InvalidMethod_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-6")

	future := time.Now().UTC().Add(time.Hour)
	req := domain.SetValuationPolicyRequest{ValuationMethod: "MADE_UP_METHOD", EffectiveFrom: &future}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/valuation-policy", req, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── RetireInventoryItem ──────────────────────────────────────────────────────

func TestRetireInventoryItem_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-7")

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/retire", map[string]string{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRetireInventoryItem_FromDraft_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-8")

	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/retire", domain.RetireInventoryItemRequest{Reason: "x"}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 retiring a DRAFT item, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRetireInventoryItem_FromActive_Succeeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-9")

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/retire", domain.RetireInventoryItemRequest{Reason: "discontinued"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.items[id].Status != domain.ItemStatusRetired {
		t.Fatalf("expected RETIRED, got %q", s.items[id].Status)
	}
}

// ── LinkCommercialCatalogItem (never touches valuation) ──────────────────────

func TestLinkCommercialCatalogItem_NeverChangesValuationPolicy(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	id := createActiveItem(t, s, r, "le-1", "SKU-10")
	before := s.valuationPolicies[id].ValuationMethod

	rr := doReq(r, http.MethodPost, "/v1/items/"+id+"/link-catalog", domain.LinkCommercialCatalogItemRequest{CatalogItemID: "cat-1"}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.valuationPolicies[id].ValuationMethod != before {
		t.Fatalf("expected valuation method unchanged by catalog link, got %q (was %q)", s.valuationPolicies[id].ValuationMethod, before)
	}
	if s.items[id].CatalogItemID == nil || *s.items[id].CatalogItemID != "cat-1" {
		t.Fatalf("expected catalog_item_id linked, got %v", s.items[id].CatalogItemID)
	}
}

// ── AmendInventoryProfile (never touches base_uom/sku) ───────────────────────

func TestAmendInventoryProfile_NeverChangesBaseUOM(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	it := createDraftItem(t, r, "le-1", "SKU-11")

	newDesc := "Updated description"
	req := domain.AmendInventoryProfileRequest{Description: &newDesc}
	rr := doReq(r, http.MethodPost, "/v1/items/"+it.ItemID+"/amend", req, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.items[it.ItemID].BaseUOM != "EACH" {
		t.Fatalf("expected base_uom unchanged (EACH), got %q", s.items[it.ItemID].BaseUOM)
	}
	if s.items[it.ItemID].Description != newDesc {
		t.Fatalf("expected description updated, got %q", s.items[it.ItemID].Description)
	}
}

// ── Authorization ────────────────────────────────────────────────────────────

func TestCreateInventoryItem_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	req := domain.CreateInventoryItemRequest{LegalEntityID: "le-1", SKU: "SKU-12", Description: "Widget", BaseUOM: "EACH"}
	rr := doReq(r, http.MethodPost, "/v1/items/", req, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}
