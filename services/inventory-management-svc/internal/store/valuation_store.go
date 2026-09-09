package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

// ValueMovement is INV-04's own core command — see migration 000004's
// doc comment for why it lands directly in FINAL (no separate Draft/
// Calculated/Validated command exists). Refuses (ErrMovementAlreadyValued)
// a second attempt against the same movement — negative path #4, "Same
// movement consumes two cost layers twice," enforced by the real
// UNIQUE(tenant_id, movement_id) constraint on inventory_valuation_entries.
func (s *PgStore) ValueMovement(ctx context.Context, movementID, principalID string, unitCost *float64, at time.Time) (*domain.ValuationEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var result *domain.ValuationEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var m struct {
			itemID, legalEntityID, uom, fiscalPeriod, status string
			sourceLocID, destLocID                           *string
			quantity                                         float64
		}
		err := tx.QueryRow(ctx, `
			SELECT item_id, legal_entity_id, source_location_id, destination_location_id, quantity, fiscal_period, status
			FROM inventory_movements WHERE movement_id = $1 AND tenant_id = $2
		`, movementID, tenantID).Scan(&m.itemID, &m.legalEntityID, &m.sourceLocID, &m.destLocID, &m.quantity, &m.fiscalPeriod, &m.status)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrMovementNotFound
		}
		if err != nil {
			return err
		}
		if m.status != "COMMITTED" {
			return domain.ErrMovementNotCommitted
		}

		var valuationMethod string
		err = tx.QueryRow(ctx, `
			SELECT valuation_method FROM inventory_valuation_policies
			WHERE tenant_id = $1 AND item_id = $2 AND effective_to IS NULL
		`, tenantID, m.itemID).Scan(&valuationMethod)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrValuationPolicyRequiredForActivation
			}
			return err
		}

		entryID := uuidNewString()
		var entry *domain.ValuationEntry
		if m.destLocID != nil {
			entry, err = s.valueInbound(ctx, tx, tenantID, entryID, m.legalEntityID, m.itemID, *m.destLocID, movementID, m.quantity, valuationMethod, m.fiscalPeriod, unitCost, principalID, at)
		} else if m.sourceLocID != nil {
			entry, err = s.valueOutbound(ctx, tx, tenantID, entryID, m.legalEntityID, m.itemID, *m.sourceLocID, movementID, m.quantity, valuationMethod, m.fiscalPeriod, unitCost, principalID, at)
		} else {
			return domain.ErrMovementNotFound
		}
		if err != nil {
			return err
		}
		result = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PgStore) valueInbound(ctx context.Context, tx pgx.Tx, tenantID, entryID, legalEntityID, itemID, locationID, movementID string, quantity float64, valuationMethod, fiscalPeriod string, unitCost *float64, principalID string, at time.Time) (*domain.ValuationEntry, error) {
	if unitCost == nil {
		return nil, domain.ErrUnitCostRequired
	}
	value := roundCents(quantity * *unitCost)

	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_cost_layers (
			layer_id, tenant_id, legal_entity_id, item_id, location_id, source_movement_id,
			original_quantity, remaining_quantity, unit_cost, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uuidNewString(), tenantID, legalEntityID, itemID, locationID, movementID, quantity, quantity, *unitCost, at)
	if err != nil {
		if isUniqueViolation(err) {
			// UNIQUE(tenant_id, source_movement_id) — this movement
			// already has a cost layer, i.e. it was already valued. Same
			// negative path (#4) as the entries-table UNIQUE violation
			// insertValuationEntry maps below, just caught one insert
			// earlier for an INBOUND movement.
			return nil, domain.ErrMovementAlreadyValued
		}
		return nil, err
	}

	entry := &domain.ValuationEntry{
		EntryID: entryID, LegalEntityID: legalEntityID, ItemID: itemID, LocationID: locationID, MovementID: movementID,
		EntryType: domain.ValuationEntryTypeInbound, Quantity: quantity, Value: value, ValuationMethod: valuationMethod,
		FiscalPeriod: fiscalPeriod, CreatedAt: at, CreatedByPrincipalID: principalID,
	}
	if err := s.insertValuationEntry(ctx, tx, tenantID, entry); err != nil {
		return nil, err
	}
	return entry, nil
}

type consumableLayer struct {
	layerID  string
	remain   float64
	unitCost float64
}

func (s *PgStore) valueOutbound(ctx context.Context, tx pgx.Tx, tenantID, entryID, legalEntityID, itemID, locationID, movementID string, quantity float64, valuationMethod, fiscalPeriod string, unitCost *float64, principalID string, at time.Time) (*domain.ValuationEntry, error) {
	if valuationMethod == domain.ValuationMethodStandardCost {
		if unitCost == nil {
			return nil, domain.ErrUnitCostRequired
		}
		value := roundCents(quantity * *unitCost)
		entry := &domain.ValuationEntry{
			EntryID: entryID, LegalEntityID: legalEntityID, ItemID: itemID, LocationID: locationID, MovementID: movementID,
			EntryType: domain.ValuationEntryTypeOutbound, Quantity: quantity, Value: value, ValuationMethod: valuationMethod,
			FiscalPeriod: fiscalPeriod, CreatedAt: at, CreatedByPrincipalID: principalID,
		}
		return entry, s.insertValuationEntry(ctx, tx, tenantID, entry)
	}

	rows, err := tx.Query(ctx, `
		SELECT layer_id, remaining_quantity, unit_cost FROM inventory_cost_layers
		WHERE tenant_id = $1 AND item_id = $2 AND location_id = $3 AND remaining_quantity > 0
		ORDER BY created_at ASC
		FOR UPDATE
	`, tenantID, itemID, locationID)
	if err != nil {
		return nil, err
	}
	var layers []consumableLayer
	for rows.Next() {
		var l consumableLayer
		if err := rows.Scan(&l.layerID, &l.remain, &l.unitCost); err != nil {
			rows.Close()
			return nil, err
		}
		layers = append(layers, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var totalRemaining float64
	for _, l := range layers {
		totalRemaining += l.remain
	}
	if totalRemaining < quantity {
		return nil, domain.ErrInsufficientCostLayers
	}

	// FIFO consumes oldest layers first (query order is insertion order,
	// which is chronological — see the INSERT-only, never-reordered
	// nature of this table). WEIGHTED_AVERAGE consumes the same
	// layers/quantities for bookkeeping, but VALUES the consumption at
	// the single blended rate computed across every remaining layer
	// before this consumption — see migration 000004's own scope note.
	var averageRate float64
	if valuationMethod == domain.ValuationMethodWeightedAverage {
		var totalValue float64
		for _, l := range layers {
			totalValue += l.remain * l.unitCost
		}
		averageRate = totalValue / totalRemaining
	}

	// layers is already in FIFO (created_at ASC) order from the query
	// above — consumed in that order regardless of method.
	remainingToConsume := quantity
	var totalValue float64
	var consumptions []domain.LayerConsumption
	for i := range layers {
		if remainingToConsume <= 0 {
			break
		}
		l := &layers[i]
		take := l.remain
		if take > remainingToConsume {
			take = remainingToConsume
		}
		rate := l.unitCost
		if valuationMethod == domain.ValuationMethodWeightedAverage {
			rate = averageRate
		}
		totalValue += take * rate
		remainingToConsume -= take

		if _, err := tx.Exec(ctx, `UPDATE inventory_cost_layers SET remaining_quantity = remaining_quantity - $1 WHERE tenant_id = $2 AND layer_id = $3`, take, tenantID, l.layerID); err != nil {
			return nil, err
		}
		consumptions = append(consumptions, domain.LayerConsumption{
			ConsumptionID: uuidNewString(), LayerID: l.layerID, QuantityConsumed: take, UnitCostAtConsumption: rate, CreatedAt: at,
		})
	}

	entry := &domain.ValuationEntry{
		EntryID: entryID, LegalEntityID: legalEntityID, ItemID: itemID, LocationID: locationID, MovementID: movementID,
		EntryType: domain.ValuationEntryTypeOutbound, Quantity: quantity, Value: roundCents(totalValue), ValuationMethod: valuationMethod,
		FiscalPeriod: fiscalPeriod, CreatedAt: at, CreatedByPrincipalID: principalID,
	}
	if err := s.insertValuationEntry(ctx, tx, tenantID, entry); err != nil {
		return nil, err
	}
	for _, c := range consumptions {
		c.ValuationEntryID = entryID
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_layer_consumptions (consumption_id, tenant_id, valuation_entry_id, layer_id, quantity_consumed, unit_cost_at_consumption, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, c.ConsumptionID, tenantID, entryID, c.LayerID, c.QuantityConsumed, c.UnitCostAtConsumption, c.CreatedAt); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

func (s *PgStore) insertValuationEntry(ctx context.Context, tx pgx.Tx, tenantID string, e *domain.ValuationEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_valuation_entries (
			entry_id, tenant_id, legal_entity_id, item_id, location_id, movement_id, entry_type,
			quantity, value, valuation_method, fiscal_period, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, e.EntryID, tenantID, e.LegalEntityID, e.ItemID, e.LocationID, e.MovementID, e.EntryType,
		e.Quantity, e.Value, e.ValuationMethod, e.FiscalPeriod, e.CreatedAt, e.CreatedByPrincipalID)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.ErrMovementAlreadyValued
		}
		return err
	}
	return nil
}

func (s *PgStore) GetValuationEntry(ctx context.Context, entryID string) (*domain.ValuationEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var e *domain.ValuationEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT entry_id, legal_entity_id, item_id, location_id, movement_id, entry_type, quantity, value,
				valuation_method, fiscal_period, run_id, created_at, created_by_principal_id
			FROM inventory_valuation_entries WHERE entry_id = $1 AND tenant_id = $2
		`, entryID, tenantID)
		var err error
		e, err = scanValuationEntry(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrValuationEntryNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

func scanValuationEntry(row pgx.Row) (*domain.ValuationEntry, error) {
	var e domain.ValuationEntry
	if err := row.Scan(
		&e.EntryID, &e.LegalEntityID, &e.ItemID, &e.LocationID, &e.MovementID, &e.EntryType, &e.Quantity, &e.Value,
		&e.ValuationMethod, &e.FiscalPeriod, &e.RunID, &e.CreatedAt, &e.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &e, nil
}

// GetInventoryValue sums the value of unconsumed INBOUND layers (i.e.
// current inventory value) for (item, location) — remaining_quantity *
// unit_cost across every open layer.
func (s *PgStore) GetInventoryValue(ctx context.Context, itemID, locationID string) (float64, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	var value float64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(remaining_quantity * unit_cost), 0) FROM inventory_cost_layers
			WHERE tenant_id = $1 AND item_id = $2 AND location_id = $3
		`, tenantID, itemID, locationID).Scan(&value)
	})
	return value, err
}

func (s *PgStore) GetCostLayers(ctx context.Context, itemID, locationID string) ([]domain.CostLayer, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.CostLayer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT layer_id, item_id, location_id, source_movement_id, original_quantity, remaining_quantity, unit_cost, created_at
			FROM inventory_cost_layers WHERE tenant_id = $1 AND item_id = $2 AND location_id = $3 ORDER BY created_at
		`, tenantID, itemID, locationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.CostLayer
			if err := rows.Scan(&l.LayerID, &l.ItemID, &l.LocationID, &l.SourceMovementID, &l.OriginalQuantity, &l.RemainingQuantity, &l.UnitCost, &l.CreatedAt); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── Valuation runs ───────────────────────────────────────────────────────────

// CreateValuationRun inserts a new run and, in the same transaction,
// claims every unclaimed FINAL valuation entry for this (legal_entity,
// fiscal_period) — folding "PopulationFrozen" into creation itself, see
// migration 000004's doc comment.
func (s *PgStore) CreateValuationRun(ctx context.Context, r *domain.ValuationRun) (frozenCount int, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return 0, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_valuation_runs (
				run_id, tenant_id, legal_entity_id, fiscal_period, inventory_account_code, cogs_account_code, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, r.RunID, tenantID, r.LegalEntityID, r.FiscalPeriod, r.InventoryAccountCode, r.COGSAccountCode, domain.ValuationRunStatusPopulationFrozen, r.CreatedAt, r.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrValuationRunAlreadyExistsForPeriod
			}
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_valuation_entries SET run_id = $1
			WHERE tenant_id = $2 AND legal_entity_id = $3 AND fiscal_period = $4 AND run_id IS NULL
		`, r.RunID, tenantID, r.LegalEntityID, r.FiscalPeriod)
		if err != nil {
			return err
		}
		frozenCount = int(tag.RowsAffected())
		return nil
	})
	return frozenCount, err
}

func (s *PgStore) GetValuationRun(ctx context.Context, runID string) (*domain.ValuationRun, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var r *domain.ValuationRun
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT run_id, legal_entity_id, fiscal_period, inventory_account_code, cogs_account_code, status, journal_id,
				created_at, created_by_principal_id, approved_at, approved_by_principal_id, emitted_at
			FROM inventory_valuation_runs WHERE run_id = $1 AND tenant_id = $2
		`, runID, tenantID)
		var err error
		r, err = scanValuationRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrValuationRunNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT entry_id, legal_entity_id, item_id, location_id, movement_id, entry_type, quantity, value,
				valuation_method, fiscal_period, run_id, created_at, created_by_principal_id
			FROM inventory_valuation_entries WHERE tenant_id = $1 AND run_id = $2
		`, tenantID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanValuationEntry(rows)
			if err != nil {
				return err
			}
			r.Entries = append(r.Entries, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func scanValuationRun(row pgx.Row) (*domain.ValuationRun, error) {
	var r domain.ValuationRun
	if err := row.Scan(
		&r.RunID, &r.LegalEntityID, &r.FiscalPeriod, &r.InventoryAccountCode, &r.COGSAccountCode, &r.Status, &r.JournalID,
		&r.CreatedAt, &r.CreatedByPrincipalID, &r.ApprovedAt, &r.ApprovedByPrincipalID, &r.EmittedAt,
	); err != nil {
		return nil, err
	}
	return &r, nil
}

// MarkValuationRunEmitted moves POPULATION_FROZEN -> ACCOUNTING_EVENT_EMITTED
// — collapsing Calculated/Approved into this one command, see migration
// 000004's doc comment.
func (s *PgStore) MarkValuationRunEmitted(ctx context.Context, runID, principalID, journalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_valuation_runs SET status = $1, approved_at = $2, approved_by_principal_id = $3, emitted_at = $2, journal_id = $4
			WHERE run_id = $5 AND status = $6 AND tenant_id = $7
		`, domain.ValuationRunStatusAccountingEventEmitted, at, principalID, journalID, runID, domain.ValuationRunStatusPopulationFrozen, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}
		return nil
	})
}

// ── Write-downs ──────────────────────────────────────────────────────────────

func (s *PgStore) CreateWriteDown(ctx context.Context, w *domain.WriteDown, journalID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_write_downs (
				write_down_id, tenant_id, legal_entity_id, item_id, location_id, amount, valuation_evidence_ref,
				expense_account_code, inventory_account_code, journal_id, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, w.WriteDownID, tenantID, w.LegalEntityID, w.ItemID, w.LocationID, w.Amount, w.ValuationEvidenceRef,
			w.ExpenseAccountCode, w.InventoryAccountCode, journalID, domain.WriteDownStatusAccountingEventEmitted, w.CreatedAt, w.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetWriteDown(ctx context.Context, writeDownID string) (*domain.WriteDown, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var w *domain.WriteDown
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT write_down_id, legal_entity_id, item_id, location_id, amount, valuation_evidence_ref,
				expense_account_code, inventory_account_code, journal_id, status, created_at, created_by_principal_id,
				reversed_at, reversed_by_principal_id, reversal_reason
			FROM inventory_write_downs WHERE write_down_id = $1 AND tenant_id = $2
		`, writeDownID, tenantID)
		var err error
		w, err = scanWriteDown(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrWriteDownNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

func scanWriteDown(row pgx.Row) (*domain.WriteDown, error) {
	var w domain.WriteDown
	if err := row.Scan(
		&w.WriteDownID, &w.LegalEntityID, &w.ItemID, &w.LocationID, &w.Amount, &w.ValuationEvidenceRef,
		&w.ExpenseAccountCode, &w.InventoryAccountCode, &w.JournalID, &w.Status, &w.CreatedAt, &w.CreatedByPrincipalID,
		&w.ReversedAt, &w.ReversedByPrincipalID, &w.ReversalReason,
	); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *PgStore) ReverseWriteDown(ctx context.Context, writeDownID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_write_downs SET status = $1, reversed_at = $2, reversed_by_principal_id = $3, reversal_reason = $4
			WHERE write_down_id = $5 AND status = $6 AND tenant_id = $7
		`, domain.WriteDownStatusReversed, at, principalID, reason, writeDownID, domain.WriteDownStatusAccountingEventEmitted, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrWriteDownAlreadyReversed
		}
		return nil
	})
}
