package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

// MarkValuationRunEmittedToKernel records a run whose valuation went through the ledger accounting kernel: one journal per location and
// item type per posting. journal_id keeps the first so every existing reader still sees a journal; ledger_journal_ids lists them all.
func (s *PgStore) MarkValuationRunEmittedToKernel(ctx context.Context, runID, principalID string, journalIDs []string, at time.Time) error {
	if len(journalIDs) == 0 {
		return domain.ErrInvalidRunTransition
	}
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	ids, _ := json.Marshal(journalIDs)
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_valuation_runs SET status = $1, approved_at = $2, approved_by_principal_id = $3, emitted_at = $2, journal_id = $4, ledger_journal_ids = $5::jsonb
			WHERE run_id = $6 AND status = $7 AND tenant_id = $8
		`, domain.ValuationRunStatusAccountingEventEmitted, at, principalID, journalIDs[0], string(ids), runID, domain.ValuationRunStatusPopulationFrozen, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidRunTransition
		}
		return nil
	})
}

// RunInboundByStock splits a run's receipt-linked inbound value by item and location, which is what the kernel posts the goods-received
// reclass by.
func (s *PgStore) RunInboundByStock(ctx context.Context, runID string) ([]domain.RunInboundLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.RunInboundLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.item_id, e.location_id, COALESCE(SUM(e.value), 0)
			FROM inventory_valuation_entries e
			JOIN inventory_movement_receipt_links l ON l.tenant_id = e.tenant_id AND l.movement_id = e.movement_id
			WHERE e.tenant_id = $1 AND e.run_id = $2 AND e.entry_type = $3
			GROUP BY e.item_id, e.location_id ORDER BY e.item_id, e.location_id
		`, tenantID, runID, domain.ValuationEntryTypeInbound)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.RunInboundLine
			if err := rows.Scan(&l.ItemID, &l.LocationID, &l.ReceiptLinked); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}
