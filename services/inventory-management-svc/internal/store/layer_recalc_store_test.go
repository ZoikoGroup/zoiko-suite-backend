package store_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
	"zoiko.io/inventory-management-svc/internal/store"
)

type recalcEnv struct {
	pool                    *pgxpool.Pool
	s                       *store.PgStore
	ctx                     context.Context
	tenantID, itemID, locID string
	receiptID, layerID      string
}

// newRecalcEnv builds: receipt 10 @ 2.00, issue 4 (so layer remaining = 6 and
// one consumption of 4 exists), then a 12.00 landed cost (unit_cost -> 3.20).
func newRecalcEnv(t *testing.T, sku string) *recalcEnv {
	t.Helper()
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, sku, domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-"+sku)

	now := time.Now().UTC()
	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-rc-r-"+sku, 10)
	cost := 2.0
	if _, _, err := s.ValueMovement(ctx, receipt.MovementID, "preparer-1", &cost, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("value receipt: %v", err)
	}
	issue := createCommittedIssue(t, s, ctx, legalEntityID, itemID, locID, "idem-rc-i-"+sku, 4)
	if _, _, err := s.ValueMovement(ctx, issue.MovementID, "preparer-1", nil, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("value issue: %v", err)
	}
	if _, err := s.AllocateLandedCost(ctx, newLandedCost(receipt.MovementID, "lc-rc-"+sku, 12, now.Add(-time.Hour))); err != nil {
		t.Fatalf("landed cost: %v", err)
	}
	e := &recalcEnv{pool: pool, s: s, ctx: ctx, tenantID: tenantID, itemID: itemID, locID: locID, receiptID: receipt.MovementID}
	if err := pool.QueryRow(context.Background(), `SELECT layer_id FROM inventory_cost_layers WHERE source_movement_id = $1`, receipt.MovementID).Scan(&e.layerID); err != nil {
		t.Fatalf("find layer: %v", err)
	}
	return e
}

func (e *recalcEnv) layer(t *testing.T) (remaining, unitCost float64) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT remaining_quantity, unit_cost FROM inventory_cost_layers WHERE layer_id = $1`, e.layerID).Scan(&remaining, &unitCost); err != nil {
		t.Fatalf("read layer: %v", err)
	}
	return
}

func (e *recalcEnv) auditCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM inventory_layer_rebuilds WHERE layer_id = $1`, e.layerID).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func (e *recalcEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestPgStore_RecalculateValuation_CleanLayerHasNoDrift(t *testing.T) {
	e := newRecalcEnv(t, "SKU-RC-0")
	res, err := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID)
	if err != nil {
		t.Fatal(err)
	}
	if res.DriftDetected || len(res.Layers) != 1 {
		t.Fatalf("a consistent layer (incl. landed-cost uplift) must show no drift, got %+v", res)
	}
	l := res.Layers[0]
	// remaining 6, expected 6; unit cost 2.00 + 7.20/6 = 3.20.
	if l.ExpectedRemaining != 6 || math.Abs(l.ExpectedUnitCost-3.2) > 1e-9 || math.Abs(res.RecordedValue-19.2) > 0.005 || res.RecordedValue != res.ExpectedValue {
		t.Fatalf("unexpected row/totals: %+v", res)
	}
}

func TestPgStore_RecalculateValuation_CentRoundingIsNotDrift(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenantID, legalEntityID := uuid.New().String(), uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	itemID := newValuedTestItem(t, s, ctx, tenantID, legalEntityID, "SKU-RC-R", domain.ValuationMethodFIFO)
	locID := newActiveTestLocation(t, s, ctx, tenantID, legalEntityID, "WH-RC-R")
	receipt := createCommittedReceipt(t, s, ctx, legalEntityID, itemID, locID, "idem-rc-round", 3)
	cost := 1.3333 // value 3.9999 -> 4.00 (rounded to cents)
	if _, _, err := s.ValueMovement(ctx, receipt.MovementID, "p", &cost, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecalculateValuation(ctx, itemID, locID)
	if err != nil || res.DriftDetected {
		t.Fatalf("cent rounding of the entry value must not be flagged as drift: %+v (err %v)", res, err)
	}
}

func TestPgStore_RecalcRebuild_QuantityDrift_FixedAuditedIdempotent(t *testing.T) {
	e := newRecalcEnv(t, "SKU-RC-1")
	e.exec(t, `UPDATE inventory_cost_layers SET remaining_quantity = 9 WHERE layer_id = $1`, e.layerID)

	res, err := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID)
	if err != nil || !res.DriftDetected || !res.Layers[0].QuantityDrift || res.Layers[0].CostDrift || res.Layers[0].ExpectedRemaining != 6 {
		t.Fatalf("expected quantity-only drift (6 expected, 9 recorded), got %+v (err %v)", res, err)
	}
	if rem, _ := e.layer(t); rem != 9 {
		t.Fatalf("recalculate must not mutate, remaining=%v", rem)
	}
	if e.auditCount(t) != 0 {
		t.Fatalf("recalculate must not write audit rows")
	}

	rb, err := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "REC-77 stock drift", "approver-2", time.Now().UTC())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rb.Rebuilt != 1 || rb.Rebuilds[0].OldRemaining != 9 || rb.Rebuilds[0].NewRemaining != 6 || !rb.GLNotPosted {
		t.Fatalf("unexpected rebuild result %+v", rb)
	}
	// value: 9*3.2 = 28.80 before, 6*3.2 = 19.20 after.
	if math.Abs(rb.ValueBefore-28.8) > 0.005 || math.Abs(rb.ValueAfter-19.2) > 0.005 {
		t.Fatalf("expected value 28.80 -> 19.20, got %v -> %v", rb.ValueBefore, rb.ValueAfter)
	}
	rem, uc := e.layer(t)
	if rem != 6 || math.Abs(uc-3.2) > 1e-9 {
		t.Fatalf("remaining must be 6 and unit_cost untouched at 3.20, got %v / %v", rem, uc)
	}
	var reason, by string
	var oldR, newR float64
	if err := e.pool.QueryRow(context.Background(), `SELECT old_remaining, new_remaining, reason, rebuilt_by_principal_id FROM inventory_layer_rebuilds WHERE layer_id = $1`, e.layerID).Scan(&oldR, &newR, &reason, &by); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	if oldR != 9 || newR != 6 || reason != "REC-77 stock drift" || by != "approver-2" {
		t.Fatalf("unexpected audit row %v %v %q %q", oldR, newR, reason, by)
	}

	// Idempotent: a second rebuild finds no drift and writes nothing.
	again, err := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "again", "approver-2", time.Now().UTC())
	if err != nil || again.Rebuilt != 0 || e.auditCount(t) != 1 {
		t.Fatalf("second rebuild must be a no-op, got %+v audit=%d err=%v", again, e.auditCount(t), err)
	}
	if after, _ := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID); after.DriftDetected {
		t.Fatalf("no drift expected after rebuild: %+v", after)
	}
}

func TestPgStore_Rebuild_CostDrift_ReportedNeverFixed(t *testing.T) {
	e := newRecalcEnv(t, "SKU-RC-2")
	// Cost drift AND quantity drift together: only the quantity is fixed.
	e.exec(t, `UPDATE inventory_cost_layers SET unit_cost = 9.5, remaining_quantity = 7 WHERE layer_id = $1`, e.layerID)

	res, _ := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID)
	if !res.Layers[0].CostDrift || !res.Layers[0].QuantityDrift {
		t.Fatalf("expected both drifts, got %+v", res.Layers[0])
	}
	rb, err := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "mixed", "approver-2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	rem, uc := e.layer(t)
	if rem != 6 || uc != 9.5 {
		t.Fatalf("quantity must be fixed (6) but unit_cost left at 9.5, got %v / %v", rem, uc)
	}
	if !rb.CostDriftNotFixed || len(rb.CostDriftLayerIDs) != 1 || rb.CostDriftLayerIDs[0] != e.layerID {
		t.Fatalf("cost drift must be reported as not fixed: %+v", rb)
	}
	// Cost-drift-only layer: rebuild is a no-op and still reports it.
	again, _ := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "again", "approver-2", time.Now().UTC())
	if again.Rebuilt != 0 || !again.CostDriftNotFixed {
		t.Fatalf("expected no-op with cost drift still reported, got %+v", again)
	}
	if after, _ := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID); !after.DriftDetected {
		t.Fatalf("cost drift must remain visible")
	}
}

func TestPgStore_Rebuild_OverConsumedEvidence_Refused(t *testing.T) {
	e := newRecalcEnv(t, "SKU-RC-3")
	// Corrupt the consumption evidence so it exceeds the original quantity (10).
	e.exec(t, `UPDATE inventory_layer_consumptions SET quantity_consumed = 11 WHERE layer_id = $1`, e.layerID)

	res, _ := e.s.RecalculateValuation(e.ctx, e.itemID, e.locID)
	if !res.Layers[0].OverConsumed || res.Layers[0].ExpectedRemaining != -1 {
		t.Fatalf("expected over-consumed row, got %+v", res.Layers[0])
	}
	_, err := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "try", "approver-2", time.Now().UTC())
	if !errors.Is(err, domain.ErrLayerOverConsumed) {
		t.Fatalf("expected ErrLayerOverConsumed, got %v", err)
	}
	if rem, _ := e.layer(t); rem != 6 || e.auditCount(t) != 0 {
		t.Fatalf("a refused rebuild must change nothing (remaining=%v audit=%d)", rem, e.auditCount(t))
	}
}

func TestPgStore_LayerRebuildAudit_IsAppendOnly(t *testing.T) {
	e := newRecalcEnv(t, "SKU-RC-4")
	e.exec(t, `UPDATE inventory_cost_layers SET remaining_quantity = 8 WHERE layer_id = $1`, e.layerID)
	if _, err := e.s.RebuildCostLayers(e.ctx, e.itemID, e.locID, "audit", "approver-2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE inventory_layer_rebuilds SET reason = 'EDITED' WHERE layer_id = $1`, e.layerID); err == nil {
		t.Fatalf("expected UPDATE on the audit table to be rejected")
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM inventory_layer_rebuilds WHERE layer_id = $1`, e.layerID); err == nil {
		t.Fatalf("expected DELETE on the audit table to be rejected")
	}
	if e.auditCount(t) != 1 {
		t.Fatalf("audit row must survive")
	}
}
