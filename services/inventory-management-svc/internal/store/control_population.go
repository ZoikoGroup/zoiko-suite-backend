package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// withReadSnapshot runs fn in ONE REPEATABLE READ, read-only transaction with
// app.tenant_id set (the RLS helper), so everything fn reads comes from a
// single snapshot.
func (s *PgStore) withReadSnapshot(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// stockCountLinesScope is the in-scope set of the stock-count-lines control
// population for one side.
//
// $1 tenant_id, $2 legal_entity_id, $3 count_id (uuid), $4 side ('book'|'physical').
//
// side=book: amount is the BOOK quantity as of the count cut-off, computed here
// from COMMITTED movements for (item, location) with business_date on or before
// the cut-off date (UTC) - the same rule as liveOnHand/GetOnHandAsOf (receipts
// into the location minus issues out of it). It is deliberately NOT the frozen
// system_quantity, so tampering with the frozen value is detectable; the frozen
// value is exposed as an attribute. Every line is included.
//
// side=physical: amount is observed_quantity; a line with no observation is
// omitted so it surfaces as a missing counted line.
//
// The explicit tenant_id predicates are deliberate: they are not left to RLS.
// The amount is cast to numeric(18,4) so its text form is stable for the digest.
const stockCountLinesScope = `
	WITH scope AS (
		SELECT l.line_id, l.count_id, l.item_id, l.location_id,
		       l.system_quantity AS frozen_quantity,
		       l.status AS line_status,
		       (l.variance_approved_at IS NOT NULL) AS variance_approved,
		       COALESCE(l.adjustment_movement_id::text, '') AS adjustment_movement_id,
		       c.status AS count_status,
		       (c.cutoff_at AT TIME ZONE 'UTC')::date AS cutoff_date,
		       i.base_uom AS uom,
		       (CASE WHEN $4::text = 'book' THEN
		            COALESCE((SELECT SUM(m.quantity) FROM inventory_movements m
		                      WHERE m.tenant_id = $1 AND m.item_id = l.item_id
		                        AND m.destination_location_id = l.location_id
		                        AND m.status = 'COMMITTED'
		                        AND m.business_date <= (c.cutoff_at AT TIME ZONE 'UTC')::date), 0)
		          - COALESCE((SELECT SUM(m.quantity) FROM inventory_movements m
		                      WHERE m.tenant_id = $1 AND m.item_id = l.item_id
		                        AND m.source_location_id = l.location_id
		                        AND m.status = 'COMMITTED'
		                        AND m.business_date <= (c.cutoff_at AT TIME ZONE 'UTC')::date), 0)
		        ELSE l.observed_quantity
		       END)::numeric(18,4) AS amount
		FROM inventory_stock_count_lines l
		JOIN inventory_stock_counts c ON c.count_id = l.count_id AND c.tenant_id = l.tenant_id
		JOIN inventory_items i ON i.item_id = l.item_id AND i.tenant_id = l.tenant_id
		WHERE l.tenant_id = $1
		  AND c.tenant_id = $1
		  AND c.legal_entity_id = $2
		  AND l.count_id = $3::uuid
		  AND ($4::text = 'book' OR l.observed_quantity IS NOT NULL)
	)`

// ControlPopulationStockCountLines returns one keyset page of the
// stock-count-lines population plus the whole-set watermark and declared totals.
//
// Everything (count existence, count, digest, totals and the page) runs in ONE
// REPEATABLE READ, read-only transaction, so the book quantities, the watermark
// and the page are all computed from a single snapshot.
//
// Watermark = "<row count>:<md5 over the ordered line_id|side|amount|line_status
// of the WHOLE in-scope set>". Paging is keyset on line_id, compared as uuid on
// both the ORDER BY and the cursor.
func (s *PgStore) ControlPopulationStockCountLines(ctx context.Context, q domain.StockCountLinesQuery) (*domain.ControlPopulationPage, error) {
	if q.TenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	err := s.withReadSnapshot(ctx, q.TenantID, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `
			SELECT 1 FROM inventory_stock_counts
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND count_id = $3::uuid`,
			q.TenantID, q.LegalEntityID, q.CountID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrStockCountNotFound
		}
		if err != nil {
			return err
		}

		var digest string
		if err := tx.QueryRow(ctx, stockCountLinesScope+`
			SELECT count(*),
			       md5(COALESCE(string_agg(line_id::text || '|' || $4::text || '|' || amount::text || '|' || line_status,
			                               ',' ORDER BY line_id), ''))
			FROM scope`, q.TenantID, q.LegalEntityID, q.CountID, q.Side,
		).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = strconv.FormatInt(page.DeclaredTotals.RowCount, 10) + ":" + digest

		if page.DeclaredTotals.RowCount > 0 {
			var sum string
			if err := tx.QueryRow(ctx, stockCountLinesScope+`
				SELECT COALESCE(sum(amount), 0)::numeric(18,4)::text FROM scope`,
				q.TenantID, q.LegalEntityID, q.CountID, q.Side).Scan(&sum); err != nil {
				return err
			}
			page.DeclaredTotals.Totals["XXX"] = sum
		}

		var after *string
		if q.AfterRecordID != "" {
			after = &q.AfterRecordID
		}
		rows, err := tx.Query(ctx, stockCountLinesScope+`
			SELECT line_id::text, count_id::text, item_id::text, location_id::text,
			       amount::text, to_char(cutoff_date, 'YYYY-MM-DD'),
			       uom, count_status, line_status, variance_approved, adjustment_movement_id,
			       frozen_quantity::numeric(18,4)::text
			FROM scope
			WHERE ($5::uuid IS NULL OR line_id > $5::uuid)
			ORDER BY line_id
			LIMIT $6`,
			q.TenantID, q.LegalEntityID, q.CountID, q.Side, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			var countID, itemID, locationID, uom, countStatus, lineStatus, adjID, frozen string
			var approved bool
			if err := rows.Scan(&r.RecordID, &countID, &itemID, &locationID, &r.Amount, &r.Date,
				&uom, &countStatus, &lineStatus, &approved, &adjID, &frozen); err != nil {
				return err
			}
			r.Reference = countID + "|" + itemID + "|" + locationID
			r.Currency = "XXX"
			r.Attributes = map[string]string{
				"uom": uom, "item_id": itemID, "location_id": locationID,
				"count_status": countStatus, "line_status": lineStatus,
				"variance_approved":      strconv.FormatBool(approved),
				"adjustment_movement_id": adjID,
				"frozen_system_quantity": frozen,
			}
			page.Records = append(page.Records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// One row beyond the limit was read only to know whether another page exists.
	if len(page.Records) > q.Limit {
		page.Records = page.Records[:q.Limit]
		page.NextCursor = page.Records[q.Limit-1].RecordID
	}
	return page, nil
}
