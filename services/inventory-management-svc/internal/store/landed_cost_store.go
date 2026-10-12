package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

const landedCostColumns = `
	allocation_id, legal_entity_id, item_id, location_id, movement_id, layer_id, idempotency_key,
	amount, inventory_share, cogs_share, remaining_quantity_at_allocation, unit_uplift,
	valuation_evidence_ref, fiscal_period, inventory_account_code, cogs_account_code, offset_account_code,
	status, journal_id, created_at, created_by_principal_id, emitted_at`

func scanLandedCost(row pgx.Row) (*domain.LandedCostAllocation, error) {
	var a domain.LandedCostAllocation
	if err := row.Scan(
		&a.AllocationID, &a.LegalEntityID, &a.ItemID, &a.LocationID, &a.MovementID, &a.LayerID, &a.IdempotencyKey,
		&a.Amount, &a.InventoryShare, &a.COGSShare, &a.RemainingQuantityAtAllocation, &a.UnitUplift,
		&a.ValuationEvidenceRef, &a.FiscalPeriod, &a.InventoryAccountCode, &a.COGSAccountCode, &a.OffsetAccountCode,
		&a.Status, &a.JournalID, &a.CreatedAt, &a.CreatedByPrincipalID, &a.EmittedAt,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

// AllocateLandedCost applies a late landed cost to the cost layer of one
// valued RECEIPT — see migration 000009's doc comment for the model.
//
// Idempotent on (tenant, idempotency_key): a retry returns the original
// allocation (created=false) so a posting that failed after this transaction
// committed can be resumed without applying the cost twice. Reusing a key for
// a different movement or amount is refused.
//
// The layer row is locked FOR UPDATE, so the inventory/COGS split is computed
// against a remaining quantity that cannot move underneath it (a concurrent
// issue consuming this layer waits, or runs first and is reflected).
func (s *PgStore) AllocateLandedCost(ctx context.Context, a *domain.LandedCostAllocation) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		existing, err := scanLandedCost(tx.QueryRow(ctx,
			`SELECT `+landedCostColumns+` FROM inventory_landed_cost_allocations WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, a.IdempotencyKey))
		if err == nil {
			if existing.MovementID != a.MovementID || existing.Amount != a.Amount {
				return domain.ErrLandedCostKeyConflict
			}
			*a = *existing
			created = false
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var layerID, itemID, locationID, legalEntityID string
		var original, remaining float64
		err = tx.QueryRow(ctx, `
			SELECT layer_id, item_id, location_id, legal_entity_id, original_quantity, remaining_quantity
			FROM inventory_cost_layers WHERE tenant_id = $1 AND source_movement_id = $2
			FOR UPDATE
		`, tenantID, a.MovementID).Scan(&layerID, &itemID, &locationID, &legalEntityID, &original, &remaining)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNoCostLayerForMovement
		}
		if err != nil {
			return err
		}

		var method string
		if err := tx.QueryRow(ctx, `
			SELECT valuation_method FROM inventory_valuation_entries WHERE tenant_id = $1 AND movement_id = $2
		`, tenantID, a.MovementID).Scan(&method); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if method == domain.ValuationMethodStandardCost {
			return domain.ErrLandedCostNotApplicable
		}

		var invShare, uplift float64
		if remaining > 0 && original > 0 {
			invShare = roundCents(a.Amount * remaining / original)
			uplift = invShare / remaining
		}
		cogsShare := roundCents(a.Amount - invShare)

		if uplift > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE inventory_cost_layers SET unit_cost = unit_cost + $1
				WHERE layer_id = $2 AND tenant_id = $3
			`, uplift, layerID, tenantID); err != nil {
				return err
			}
		}

		a.LayerID, a.ItemID, a.LocationID, a.LegalEntityID = layerID, itemID, locationID, legalEntityID
		a.InventoryShare, a.COGSShare = invShare, cogsShare
		a.RemainingQuantityAtAllocation, a.UnitUplift = remaining, uplift
		a.Status = domain.LandedCostStatusPendingPosting
		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_landed_cost_allocations (
				allocation_id, tenant_id, legal_entity_id, item_id, location_id, movement_id, layer_id, idempotency_key,
				amount, inventory_share, cogs_share, remaining_quantity_at_allocation, unit_uplift,
				valuation_evidence_ref, fiscal_period, inventory_account_code, cogs_account_code, offset_account_code,
				status, created_at, created_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		`, a.AllocationID, tenantID, legalEntityID, itemID, locationID, a.MovementID, layerID, a.IdempotencyKey,
			a.Amount, invShare, cogsShare, remaining, uplift,
			a.ValuationEvidenceRef, a.FiscalPeriod, a.InventoryAccountCode, a.COGSAccountCode, a.OffsetAccountCode,
			a.Status, a.CreatedAt, a.CreatedByPrincipalID)
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

// MarkLandedCostEmitted links the posted journal. Idempotent per allocation:
// only a PENDING_POSTING row advances.
func (s *PgStore) MarkLandedCostEmitted(ctx context.Context, allocationID, journalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_landed_cost_allocations SET status = $1, journal_id = $2, emitted_at = $3
			WHERE allocation_id = $4 AND tenant_id = $5 AND status = $6
		`, domain.LandedCostStatusAccountingEventEmitted, journalID, at, allocationID, tenantID, domain.LandedCostStatusPendingPosting)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}
		return nil
	})
}

func (s *PgStore) GetLandedCostAllocation(ctx context.Context, allocationID string) (*domain.LandedCostAllocation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var a *domain.LandedCostAllocation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		a, err = scanLandedCost(tx.QueryRow(ctx,
			`SELECT `+landedCostColumns+` FROM inventory_landed_cost_allocations WHERE allocation_id = $1 AND tenant_id = $2`,
			allocationID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLandedCostNotFound
		}
		return err
	})
	return a, err
}
