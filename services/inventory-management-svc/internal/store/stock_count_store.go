package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

func (s *PgStore) CreateStockCount(ctx context.Context, sc *domain.StockCount, locationIDs []string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_stock_counts (
				count_id, tenant_id, legal_entity_id, fiscal_period, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, sc.CountID, tenantID, sc.LegalEntityID, sc.FiscalPeriod, domain.StockCountStatusPlanned, sc.CreatedAt, sc.CreatedByPrincipalID)
		if err != nil {
			return err
		}
		for _, locID := range locationIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO inventory_stock_count_locations (count_id, location_id) VALUES ($1, $2)`, sc.CountID, locID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PgStore) GetStockCount(ctx context.Context, countID string) (*domain.StockCount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var sc *domain.StockCount
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT count_id, legal_entity_id, fiscal_period, status, cutoff_at, created_at, created_by_principal_id,
				frozen_at, certified_at, certified_by_principal_id, cancelled_at, cancelled_by_principal_id, cancel_reason
			FROM inventory_stock_counts WHERE count_id = $1 AND tenant_id = $2
		`, countID, tenantID)
		var err error
		sc, err = scanStockCount(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrStockCountNotFound
		}
		if err != nil {
			return err
		}

		locRows, err := tx.Query(ctx, `SELECT location_id FROM inventory_stock_count_locations WHERE count_id = $1`, countID)
		if err != nil {
			return err
		}
		defer locRows.Close()
		for locRows.Next() {
			var locID string
			if err := locRows.Scan(&locID); err != nil {
				return err
			}
			sc.LocationIDs = append(sc.LocationIDs, locID)
		}
		if err := locRows.Err(); err != nil {
			return err
		}

		lineRows, err := tx.Query(ctx, `SELECT `+countLineColumns+` FROM inventory_stock_count_lines WHERE tenant_id = $1 AND count_id = $2 ORDER BY created_at`, tenantID, countID)
		if err != nil {
			return err
		}
		defer lineRows.Close()
		for lineRows.Next() {
			l, err := scanCountLine(lineRows)
			if err != nil {
				return err
			}
			sc.Lines = append(sc.Lines, *l)
		}
		return lineRows.Err()
	})
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func scanStockCount(row pgx.Row) (*domain.StockCount, error) {
	var sc domain.StockCount
	if err := row.Scan(
		&sc.CountID, &sc.LegalEntityID, &sc.FiscalPeriod, &sc.Status, &sc.CutoffAt, &sc.CreatedAt, &sc.CreatedByPrincipalID,
		&sc.FrozenAt, &sc.CertifiedAt, &sc.CertifiedByPrincipalID, &sc.CancelledAt, &sc.CancelledByPrincipalID, &sc.CancelReason,
	); err != nil {
		return nil, err
	}
	return &sc, nil
}

const countLineColumns = `
	line_id, count_id, item_id, location_id, system_quantity, assigned_counter_principal_id,
	observed_quantity, observed_at, observed_by_principal_id, status,
	variance_approved_at, variance_approved_by_principal_id, adjustment_movement_id, created_at`

func scanCountLine(row pgx.Row) (*domain.StockCountLine, error) {
	var l domain.StockCountLine
	if err := row.Scan(
		&l.LineID, &l.CountID, &l.ItemID, &l.LocationID, &l.SystemQuantity, &l.AssignedCounterPrincipalID,
		&l.ObservedQuantity, &l.ObservedAt, &l.ObservedByPrincipalID, &l.Status,
		&l.VarianceApprovedAt, &l.VarianceApprovedByPrincipalID, &l.AdjustmentMovementID, &l.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &l, nil
}

// FreezeCountPopulation is the real "count population/snapshot" —
// PLANNED -> POPULATION_FROZEN, capturing one line per (item, location)
// for every item ever moved through each in-scope location, valued at
// its live on-hand quantity AT THIS MOMENT — never re-queried afterward.
// See migration 000005's doc comment on negative path #2.
func (s *PgStore) FreezeCountPopulation(ctx context.Context, countID string, at time.Time) (frozenCount int, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_counts SET status = $1, cutoff_at = $2, frozen_at = $2
			WHERE count_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.StockCountStatusPopulationFrozen, at, countID, domain.StockCountStatusPlanned, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountTransition
		}

		rows, err := tx.Query(ctx, `
			SELECT DISTINCT m.item_id, l.location_id
			FROM inventory_stock_count_locations l
			JOIN inventory_movements m ON (m.source_location_id = l.location_id OR m.destination_location_id = l.location_id)
			WHERE l.count_id = $1 AND m.tenant_id = $2 AND m.status = 'COMMITTED'
		`, countID, tenantID)
		if err != nil {
			return err
		}
		type pair struct{ itemID, locationID string }
		var pairs []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.itemID, &p.locationID); err != nil {
				rows.Close()
				return err
			}
			pairs = append(pairs, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, p := range pairs {
			var inQty, outQty float64
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(quantity), 0) FROM inventory_movements
				WHERE tenant_id = $1 AND item_id = $2 AND destination_location_id = $3 AND status = 'COMMITTED'
			`, tenantID, p.itemID, p.locationID).Scan(&inQty); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(quantity), 0) FROM inventory_movements
				WHERE tenant_id = $1 AND item_id = $2 AND source_location_id = $3 AND status = 'COMMITTED'
			`, tenantID, p.itemID, p.locationID).Scan(&outQty); err != nil {
				return err
			}
			systemQty := inQty - outQty

			if _, err := tx.Exec(ctx, `
				INSERT INTO inventory_stock_count_lines (
					line_id, tenant_id, count_id, item_id, location_id, system_quantity, status, created_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, uuidNewString(), tenantID, countID, p.itemID, p.locationID, systemQty, domain.CountLineStatusPending, at); err != nil {
				return err
			}
			frozenCount++
		}
		return nil
	})
	return frozenCount, err
}

func (s *PgStore) AssignCounter(ctx context.Context, lineID, counterPrincipalID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_count_lines SET assigned_counter_principal_id = $1 WHERE line_id = $2 AND tenant_id = $3
		`, counterPrincipalID, lineID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrCountLineNotFound
		}
		return nil
	})
}

// RecordBlindCount accepts PENDING or NEEDS_RECOUNT (a real recount
// simply overwrites the prior observation with a fresh one) and moves
// the line to COUNTED. The store method's own signature has no way to
// receive or return system_quantity to the caller here — see
// domain.RecordedBlindCount's own doc comment for the handler-level half
// of negative path #1.
func (s *PgStore) RecordBlindCount(ctx context.Context, lineID, principalID string, observedQuantity float64, at time.Time) (*domain.StockCountLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var line *domain.StockCountLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_count_lines SET
				observed_quantity = $1, observed_at = $2, observed_by_principal_id = $3, status = $4
			WHERE line_id = $5 AND status IN ($6, $7) AND tenant_id = $8
		`, observedQuantity, at, principalID, domain.CountLineStatusCounted,
			lineID, domain.CountLineStatusPending, domain.CountLineStatusNeedsRecount, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountLineTransition
		}
		row := tx.QueryRow(ctx, `SELECT `+countLineColumns+` FROM inventory_stock_count_lines WHERE line_id = $1 AND tenant_id = $2`, lineID, tenantID)
		line, err = scanCountLine(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return line, nil
}

func (s *PgStore) RequestRecount(ctx context.Context, lineID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_count_lines SET status = $1
			WHERE line_id = $2 AND status = $3 AND tenant_id = $4
		`, domain.CountLineStatusNeedsRecount, lineID, domain.CountLineStatusCounted, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountLineTransition
		}
		return nil
	})
}

// ApproveCountVariance refuses self-approval — the spec's own SoD,
// "Counter cannot approve own material variance" — enforced at the LINE
// level inside the same transaction as the guarded status UPDATE, the
// same pattern INV-02's own SetQuarantine release check uses.
func (s *PgStore) ApproveCountVariance(ctx context.Context, lineID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var observedBy *string
		err := tx.QueryRow(ctx, `
			SELECT observed_by_principal_id FROM inventory_stock_count_lines
			WHERE line_id = $1 AND status = $2 AND tenant_id = $3
		`, lineID, domain.CountLineStatusCounted, tenantID).Scan(&observedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidCountLineTransition
		}
		if err != nil {
			return err
		}
		if observedBy != nil && *observedBy == principalID {
			return domain.ErrSelfVarianceApprovalNotPermitted
		}
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_count_lines SET status = $1, variance_approved_at = $2, variance_approved_by_principal_id = $3
			WHERE line_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.CountLineStatusVarianceApproved, at, principalID, lineID, domain.CountLineStatusCounted, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountLineTransition
		}
		return nil
	})
}

func (s *PgStore) GetCountLine(ctx context.Context, lineID string) (*domain.StockCountLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var l *domain.StockCountLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+countLineColumns+` FROM inventory_stock_count_lines WHERE line_id = $1 AND tenant_id = $2`, lineID, tenantID)
		var err error
		l, err = scanCountLine(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCountLineNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// LinkCountLineAdjustment is GenerateAdjustmentMovements' own final step
// per line — negative path #4, "Count directly overwrites on-hand
// quantity," is why this column exists at all: the line never gets an
// on-hand value written to it directly, only a link to a REAL row in
// inventory_movements that INV-03's own CreateMovement/CommitMovement
// already created and committed.
func (s *PgStore) LinkCountLineAdjustment(ctx context.Context, lineID, movementID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_count_lines SET status = $1, adjustment_movement_id = $2
			WHERE line_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.CountLineStatusAdjustmentGenerated, movementID, lineID, domain.CountLineStatusVarianceApproved, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountLineTransition
		}
		return nil
	})
}

func (s *PgStore) MarkCountAdjustmentsGenerated(ctx context.Context, countID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_counts SET status = $1
			WHERE count_id = $2 AND status IN ($3, $4) AND tenant_id = $5
		`, domain.StockCountStatusAdjustmentsGenerated, countID, domain.StockCountStatusPopulationFrozen, domain.StockCountStatusAdjustmentsGenerated, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountTransition
		}
		return nil
	})
}

func (s *PgStore) CertifyStockCount(ctx context.Context, countID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_counts SET status = $1, certified_at = $2, certified_by_principal_id = $3
			WHERE count_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.StockCountStatusCertified, at, principalID, countID, domain.StockCountStatusAdjustmentsGenerated, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountTransition
		}
		return nil
	})
}

func (s *PgStore) CancelStockCount(ctx context.Context, countID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_stock_counts SET status = $1, cancelled_at = $2, cancelled_by_principal_id = $3, cancel_reason = $4
			WHERE count_id = $5 AND status != $6 AND status != $7 AND tenant_id = $8
		`, domain.StockCountStatusCancelled, at, principalID, reason, countID, domain.StockCountStatusCertified, domain.StockCountStatusCancelled, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCountTransition
		}
		return nil
	})
}
