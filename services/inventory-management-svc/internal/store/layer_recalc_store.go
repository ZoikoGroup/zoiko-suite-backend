package store

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

// layerRecalcSQL recomputes every layer of (item, location) from immutable
// evidence only:
//
//	expected_remaining = original_quantity - SUM(consumptions.quantity_consumed)
//	expected_unit_cost = INBOUND entry value / original_quantity
//	                     + SUM(landed-cost allocations' unit_uplift)
//
// The quantity comparison is done in SQL (NUMERIC, exact) so float rounding
// can never fabricate or hide quantity drift.
const layerRecalcSQL = `
	SELECT layer_id, source_movement_id, original_quantity, remaining_quantity, unit_cost,
	       expected_remaining, entry_value, uplift, remaining_quantity <> expected_remaining
	FROM (
		SELECT l.layer_id, l.source_movement_id, l.original_quantity, l.remaining_quantity, l.unit_cost, l.created_at,
		       l.original_quantity - COALESCE((
		           SELECT SUM(c.quantity_consumed) FROM inventory_layer_consumptions c
		           WHERE c.tenant_id = l.tenant_id AND c.layer_id = l.layer_id), 0) AS expected_remaining,
		       e.value AS entry_value,
		       COALESCE((
		           SELECT SUM(a.unit_uplift) FROM inventory_landed_cost_allocations a
		           WHERE a.tenant_id = l.tenant_id AND a.layer_id = l.layer_id), 0) AS uplift
		FROM inventory_cost_layers l
		LEFT JOIN inventory_valuation_entries e
		       ON e.tenant_id = l.tenant_id AND e.movement_id = l.source_movement_id AND e.entry_type = 'INBOUND'
		WHERE l.tenant_id = $1 AND l.item_id = $2 AND l.location_id = $3
	) x
	ORDER BY created_at, layer_id`

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// recalcLayers runs layerRecalcSQL inside tx and classifies each layer.
func recalcLayers(ctx context.Context, tx pgx.Tx, tenantID, itemID, locationID string) (*domain.ValuationRecalculation, error) {
	rows, err := tx.Query(ctx, layerRecalcSQL, tenantID, itemID, locationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &domain.ValuationRecalculation{ItemID: itemID, LocationID: locationID, Layers: []domain.LayerRecalcRow{}}
	var recorded, expected float64
	for rows.Next() {
		var r domain.LayerRecalcRow
		var entryValue *float64
		var uplift float64
		if err := rows.Scan(&r.LayerID, &r.SourceMovementID, &r.OriginalQuantity, &r.RemainingQuantity, &r.UnitCost,
			&r.ExpectedRemaining, &entryValue, &uplift, &r.QuantityDrift); err != nil {
			return nil, err
		}
		r.OverConsumed = r.ExpectedRemaining < 0
		if entryValue == nil || r.OriginalQuantity <= 0 {
			// No INBOUND valuation entry to verify against: the cost cannot
			// be confirmed, so it is reported as drift (never auto-fixed).
			r.ExpectedUnitCost = 0
			r.CostDrift = true
		} else {
			r.ExpectedUnitCost = *entryValue/r.OriginalQuantity + uplift
			// The entry value is rounded to cents, so allow half a cent
			// spread over the original quantity.
			tol := 0.005/r.OriginalQuantity + 0.000001
			r.CostDrift = math.Abs(r.UnitCost-r.ExpectedUnitCost) > tol
		}
		recorded += r.RemainingQuantity * r.UnitCost
		expected += r.ExpectedRemaining * r.ExpectedUnitCost
		out.Layers = append(out.Layers, r)
		if r.QuantityDrift || r.CostDrift {
			out.DriftDetected = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.RecordedValue, out.ExpectedValue = round2(recorded), round2(expected)
	return out, nil
}

// RecalculateValuation is the read-only INV-04 verification: it recomputes
// each cost layer from immutable evidence and reports drift. It mutates
// nothing.
func (s *PgStore) RecalculateValuation(ctx context.Context, itemID, locationID string) (*domain.ValuationRecalculation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var res *domain.ValuationRecalculation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		res, err = recalcLayers(ctx, tx, tenantID, itemID, locationID)
		return err
	})
	return res, err
}

// RebuildCostLayers is INV-04's RebuildCostLayersControlled. It restores
// remaining_quantity to the evidence-implied value for layers with QUANTITY
// drift only. unit_cost, original_quantity, consumptions and valuation
// entries are never written (historic evidence is not rewritten); cost drift
// is reported and refused for auto-fix. Each change is audited in
// inventory_layer_rebuilds in the same transaction. Layers are locked FOR
// UPDATE so a concurrent issue cannot consume between the check and the fix.
// A layer whose evidence is over-consumed (expected < 0) is unfixable and
// aborts the whole rebuild with ErrLayerOverConsumed.
//
// No GL posting and no period check: this changes no ledger fact, only a
// sub-ledger quantity back to its own evidence. The value effect is returned
// (value_before/value_after) for the reconciliation owner.
func (s *PgStore) RebuildCostLayers(ctx context.Context, itemID, locationID, reason, principalID string, at time.Time) (*domain.CostLayerRebuildResult, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var result *domain.CostLayerRebuildResult
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			SELECT 1 FROM inventory_cost_layers WHERE tenant_id = $1 AND item_id = $2 AND location_id = $3
			ORDER BY layer_id FOR UPDATE
		`, tenantID, itemID, locationID); err != nil {
			return err
		}
		before, err := recalcLayers(ctx, tx, tenantID, itemID, locationID)
		if err != nil {
			return err
		}
		res := &domain.CostLayerRebuildResult{
			ItemID: itemID, LocationID: locationID, Rebuilds: []domain.LayerRebuild{},
			CostDriftLayerIDs: []string{}, ValueBefore: before.RecordedValue, GLNotPosted: true,
		}
		valueAfter := 0.0
		for _, l := range before.Layers {
			if l.CostDrift {
				res.CostDriftLayerIDs = append(res.CostDriftLayerIDs, l.LayerID)
			}
			newRemaining := l.RemainingQuantity
			if l.QuantityDrift {
				if l.OverConsumed {
					return domain.ErrLayerOverConsumed
				}
				newRemaining = l.ExpectedRemaining
				if _, err := tx.Exec(ctx, `
					UPDATE inventory_cost_layers SET remaining_quantity = $1 WHERE layer_id = $2 AND tenant_id = $3
				`, newRemaining, l.LayerID, tenantID); err != nil {
					return err
				}
				rb := domain.LayerRebuild{
					RebuildID: uuidNewString(), LayerID: l.LayerID, OldRemaining: l.RemainingQuantity, NewRemaining: newRemaining,
					Reason: reason, RebuiltByPrincipalID: principalID, CreatedAt: at,
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO inventory_layer_rebuilds (
						rebuild_id, tenant_id, layer_id, item_id, location_id, old_remaining, new_remaining,
						reason, rebuilt_by_principal_id, created_at
					) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				`, rb.RebuildID, tenantID, l.LayerID, itemID, locationID, rb.OldRemaining, rb.NewRemaining,
					reason, principalID, at); err != nil {
					return err
				}
				res.Rebuilds = append(res.Rebuilds, rb)
			}
			valueAfter += newRemaining * l.UnitCost
		}
		res.Rebuilt = len(res.Rebuilds)
		res.ValueAfter = round2(valueAfter)
		res.CostDriftNotFixed = len(res.CostDriftLayerIDs) > 0
		result = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
