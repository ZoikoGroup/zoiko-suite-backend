package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

// LinkMovementReceipt records that a RECEIPT movement came from an AP-03
// goods/service receipt. Idempotent for the same (movement, receipt) pair —
// a retried create returns the original movement and re-links harmlessly. A
// different receipt for an already-linked movement is refused: the link is
// evidence and is never rewritten.
func (s *PgStore) LinkMovementReceipt(ctx context.Context, movementID, apReceiptID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO inventory_movement_receipt_links (tenant_id, movement_id, ap_receipt_id, linked_at, linked_by_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, movement_id) DO NOTHING
		`, tenantID, movementID, apReceiptID, at, principalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		var existing string
		if err := tx.QueryRow(ctx, `
			SELECT ap_receipt_id FROM inventory_movement_receipt_links WHERE tenant_id = $1 AND movement_id = $2
		`, tenantID, movementID).Scan(&existing); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existing != apReceiptID {
			return domain.ErrReceiptLinkConflict
		}
		return nil
	})
}

// GetRunInboundValue splits a valuation run's INBOUND value into the part
// whose movements are linked to an AP-03 receipt (cost already expensed via
// GRNI, so eligible for the Dr Inventory / Cr GRNI reclass) and the rest
// (no known AP source — not posted, reported so the gap stays visible).
func (s *PgStore) GetRunInboundValue(ctx context.Context, runID string) (domain.RunInboundValue, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.RunInboundValue{}, domain.ErrIdentityMissing
	}
	var out domain.RunInboundValue
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(e.value) FILTER (WHERE l.movement_id IS NOT NULL), 0),
				COALESCE(SUM(e.value) FILTER (WHERE l.movement_id IS NULL), 0)
			FROM inventory_valuation_entries e
			LEFT JOIN inventory_movement_receipt_links l
				ON l.tenant_id = e.tenant_id AND l.movement_id = e.movement_id
			WHERE e.tenant_id = $1 AND e.run_id = $2 AND e.entry_type = $3
		`, tenantID, runID, domain.ValuationEntryTypeInbound).Scan(&out.ReceiptLinked, &out.Unlinked)
	})
	return out, err
}
