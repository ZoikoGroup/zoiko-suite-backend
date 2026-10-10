package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/handler"
	"zoiko.io/inventory-management-svc/internal/middleware"
)

// ── stub store methods (INV-04 RecalculateValuation / RebuildCostLayers) ─────

// recalcExpected lets a test declare what the evidence implies for a layer
// (layer_id -> expected remaining); the stub has no consumption table.
var recalcExpected = map[string]float64{}
var rebuildCalls int

func (s *stubStore) RecalculateValuation(_ context.Context, itemID, locationID string) (*domain.ValuationRecalculation, error) {
	out := &domain.ValuationRecalculation{ItemID: itemID, LocationID: locationID, Layers: []domain.LayerRecalcRow{}}
	for _, l := range s.costLayers {
		if l.ItemID != itemID || l.LocationID != locationID {
			continue
		}
		exp, ok := recalcExpected[l.LayerID]
		if !ok {
			exp = l.RemainingQuantity
		}
		row := domain.LayerRecalcRow{
			LayerID: l.LayerID, SourceMovementID: l.SourceMovementID, OriginalQuantity: l.OriginalQuantity,
			RemainingQuantity: l.RemainingQuantity, ExpectedRemaining: exp, UnitCost: l.UnitCost, ExpectedUnitCost: l.UnitCost,
			QuantityDrift: exp != l.RemainingQuantity,
		}
		out.DriftDetected = out.DriftDetected || row.QuantityDrift
		out.Layers = append(out.Layers, row)
	}
	return out, nil
}

func (s *stubStore) RebuildCostLayers(_ context.Context, itemID, locationID, reason, principalID string, at time.Time) (*domain.CostLayerRebuildResult, error) {
	rebuildCalls++
	res := &domain.CostLayerRebuildResult{ItemID: itemID, LocationID: locationID, Rebuilds: []domain.LayerRebuild{}, CostDriftLayerIDs: []string{}, GLNotPosted: true}
	for _, l := range s.costLayers {
		if l.ItemID != itemID || l.LocationID != locationID {
			continue
		}
		res.ValueBefore += l.RemainingQuantity * l.UnitCost
		if exp, ok := recalcExpected[l.LayerID]; ok && exp != l.RemainingQuantity {
			res.Rebuilds = append(res.Rebuilds, domain.LayerRebuild{LayerID: l.LayerID, OldRemaining: l.RemainingQuantity, NewRemaining: exp, Reason: reason, RebuiltByPrincipalID: principalID, CreatedAt: at})
			l.RemainingQuantity = exp
		}
		res.ValueAfter += l.RemainingQuantity * l.UnitCost
	}
	res.Rebuilt = len(res.Rebuilds)
	return res, nil
}

// actionAuthZ records the action checked and denies chosen actions.
type actionAuthZ struct {
	seen []string
	deny map[string]bool
}

func (a *actionAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	a.seen = append(a.seen, action)
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func actionRouter(s *stubStore, a *actionAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, a, zap.NewNop()).WithPeriodChecker(&stubPeriodChecker{}))
	return r
}

// recalcFixture builds one valued 10-unit layer and returns its ids.
func recalcFixture(t *testing.T, s *stubStore, r chi.Router) (itemID, locID, layerID string) {
	t.Helper()
	recalcExpected = map[string]float64{}
	rebuildCalls = 0
	itemID, locID = valuationFixture(t, s, r)
	receipt := createAndCommitReceipt(t, r, itemID, locID, "idem-rc-"+uniqueSuffix(), 10)
	if rr := valueMovement(t, r, receipt.MovementID, f(2.0)); rr.StatusCode != http.StatusCreated {
		t.Fatalf("value receipt failed: %d", rr.StatusCode)
	}
	return itemID, locID, s.costLayers[len(s.costLayers)-1].LayerID
}

func TestRecalculateValuation_DriftDetected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID, layerID := recalcFixture(t, s, r)
	recalcExpected[layerID] = 7 // evidence says 3 units were consumed

	rr := doReq(r, http.MethodPost, "/v1/valuation/recalculate", domain.RecalculateValuationRequest{ItemID: itemID, LocationID: locID}, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var res domain.ValuationRecalculation
	_ = json.NewDecoder(rr.Body).Decode(&res)
	if !res.DriftDetected || len(res.Layers) != 1 || !res.Layers[0].QuantityDrift || res.Layers[0].ExpectedRemaining != 7 {
		t.Fatalf("expected quantity drift on the layer, got %+v", res)
	}
	if s.costLayers[0].RemainingQuantity != 10 {
		t.Fatalf("recalculate must not mutate, remaining=%v", s.costLayers[0].RemainingQuantity)
	}
}

func TestRecalculateValuation_NoDrift(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID, _ := recalcFixture(t, s, r)
	rr := doReq(r, http.MethodPost, "/v1/valuation/recalculate", domain.RecalculateValuationRequest{ItemID: itemID, LocationID: locID}, "approver-1")
	var res domain.ValuationRecalculation
	_ = json.NewDecoder(rr.Body).Decode(&res)
	if rr.Code != http.StatusOK || res.DriftDetected {
		t.Fatalf("expected 200 with no drift, got %d %+v", rr.Code, res)
	}
}

func TestRecalculateValuation_MissingFields_400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/valuation/recalculate", domain.RecalculateValuationRequest{ItemID: "x"}, "p"); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestRebuildCostLayers_ReasonRequired_400_NoStoreCall(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID, layerID := recalcFixture(t, s, r)
	recalcExpected[layerID] = 7
	for _, reason := range []string{"", "   "} {
		rr := doReq(r, http.MethodPost, "/v1/valuation/cost-layers/rebuild", domain.RebuildCostLayersRequest{ItemID: itemID, LocationID: locID, Reason: reason}, "approver-2")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("reason %q: expected 400, got %d: %s", reason, rr.Code, rr.Body.String())
		}
	}
	if rebuildCalls != 0 {
		t.Fatalf("store must not be called without a reason")
	}
}

func TestRebuildCostLayers_FixesQuantity_ReportsValue(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	itemID, locID, layerID := recalcFixture(t, s, r)
	recalcExpected[layerID] = 7
	rr := doReq(r, http.MethodPost, "/v1/valuation/cost-layers/rebuild", domain.RebuildCostLayersRequest{ItemID: itemID, LocationID: locID, Reason: "REC-2026-10 drift"}, "approver-2")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var res domain.CostLayerRebuildResult
	_ = json.NewDecoder(rr.Body).Decode(&res)
	if res.Rebuilt != 1 || res.ValueBefore != 20 || res.ValueAfter != 14 || !res.GLNotPosted {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestRecalcAndRebuild_Authorization(t *testing.T) {
	s := newStubStore()
	a := &actionAuthZ{deny: map[string]bool{}}
	r := actionRouter(s, a)
	itemID, locID, layerID := recalcFixture(t, s, newRouter(s, &stubPublisher{}, &stubAuthZ{}))
	recalcExpected[layerID] = 7

	// A principal with RUN but not APPROVE may verify but not rebuild.
	a.deny["INVENTORY_VALUATION_APPROVE"] = true
	if rr := doReq(r, http.MethodPost, "/v1/valuation/recalculate", domain.RecalculateValuationRequest{ItemID: itemID, LocationID: locID}, "p"); rr.Code != http.StatusOK {
		t.Fatalf("recalculate should need only RUN, got %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/valuation/cost-layers/rebuild", domain.RebuildCostLayersRequest{ItemID: itemID, LocationID: locID, Reason: "x"}, "p"); rr.Code != http.StatusForbidden {
		t.Fatalf("rebuild without APPROVE must be 403, got %d", rr.Code)
	}
	if rebuildCalls != 0 || s.costLayers[0].RemainingQuantity != 10 {
		t.Fatalf("a forbidden rebuild must change nothing")
	}
	if got := a.seen; len(got) != 2 || got[0] != "INVENTORY_VALUATION_RUN" || got[1] != "INVENTORY_VALUATION_APPROVE" {
		t.Fatalf("unexpected actions checked: %v", got)
	}

	// And RUN denied blocks recalculate.
	a.deny["INVENTORY_VALUATION_RUN"] = true
	if rr := doReq(r, http.MethodPost, "/v1/valuation/recalculate", domain.RecalculateValuationRequest{ItemID: itemID, LocationID: locID}, "p"); rr.Code != http.StatusForbidden {
		t.Fatalf("recalculate without RUN must be 403, got %d", rr.Code)
	}
}
