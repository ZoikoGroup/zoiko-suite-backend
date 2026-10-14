package ar07

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// openItemsScope is the in-scope set of the `open_items` control population (docs/architecture/control-population-contract.md):
// every open item of the legal entity issued on or before the cut-off whose OUTSTANDING amount, reconstructed from the
// adjustment ledger through the end of the cut-off day (UTC), is positive. No cut-off means the current ledger balance.
// $1 tenant_id, $2 legal_entity_id, $3 cut-off date (NULL = none); explicit tenant predicate.
const openItemsScope = `
	WITH scope AS (
		SELECT o.open_item_id, o.document_number, o.customer_party_id, o.currency, o.due_date, o.issue_date, o.original_amount,
		       o.source_document_id, o.disputed, o.collection_hold,
		       (o.original_amount + COALESCE((SELECT sum(a.signed_effect) FROM ar07_adjustments a
		            WHERE a.tenant_id = o.tenant_id AND a.open_item_id = o.open_item_id
		              AND ($3::date IS NULL OR a.recorded_at < (($3::date + 1)::timestamp AT TIME ZONE 'UTC'))), 0)) AS outstanding
		FROM ar07_open_items o
		WHERE o.tenant_id = $1::uuid AND o.legal_entity_id = $2::uuid AND ($3::date IS NULL OR o.issue_date <= $3::date)
	), pop AS (
		SELECT *, CASE WHEN outstanding = original_amount THEN 'OPEN' ELSE 'PARTIALLY_SETTLED' END AS status FROM scope WHERE outstanding > 0
	)`

// ControlPopulation returns one keyset page of the `open_items` population plus the whole-set watermark and declared totals,
// all read from ONE REPEATABLE READ read-only snapshot. Amount is the outstanding balance. Watermark = md5 over the ordered
// `open_item_id|status|outstanding` of the WHOLE set, suffixed ":<count>". Attributes: status, customer_id, due_date,
// invoice_amount (the original amount), source_document_id, disputed, collection_hold.
func (m *Module) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	err := m.deps.DB.WithTxOpts(ctx, q.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var digest string
		if err := tx.QueryRow(ctx, openItemsScope+`
			SELECT count(*), md5(COALESCE(string_agg(open_item_id::text || '|' || status || '|' || outstanding::text, ',' ORDER BY open_item_id), ''))
			FROM pop`, q.TenantID, q.LegalEntityID, q.CutOff).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = digest + ":" + strconv.FormatInt(page.DeclaredTotals.RowCount, 10)

		totals, err := tx.Query(ctx, openItemsScope+`SELECT currency, sum(outstanding)::numeric(20,4)::text FROM pop GROUP BY currency ORDER BY currency`,
			q.TenantID, q.LegalEntityID, q.CutOff)
		if err != nil {
			return err
		}
		for totals.Next() {
			var cur, sum string
			if err := totals.Scan(&cur, &sum); err != nil {
				totals.Close()
				return err
			}
			page.DeclaredTotals.Totals[cur] = sum
		}
		totals.Close()
		if err := totals.Err(); err != nil {
			return err
		}

		var after *string
		if q.AfterRecordID != "" {
			after = &q.AfterRecordID
		}
		rows, err := tx.Query(ctx, openItemsScope+`
			SELECT open_item_id::text, document_number, outstanding::numeric(20,4)::text, currency, to_char(issue_date, 'YYYY-MM-DD'), status,
			       customer_party_id::text, to_char(due_date, 'YYYY-MM-DD'), original_amount::numeric(20,4)::text, source_document_id::text, disputed, collection_hold
			FROM pop WHERE ($4::uuid IS NULL OR open_item_id > $4::uuid) ORDER BY open_item_id LIMIT $5`,
			q.TenantID, q.LegalEntityID, q.CutOff, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			var status, customer, due, orig, src string
			var disputed, held bool
			if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &status, &customer, &due, &orig, &src, &disputed, &held); err != nil {
				return err
			}
			r.Attributes = map[string]string{"status": status, "customer_id": customer, "due_date": due, "invoice_amount": orig,
				"source_document_id": src, "disputed": strconv.FormatBool(disputed), "collection_hold": strconv.FormatBool(held)}
			page.Records = append(page.Records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	if len(page.Records) > q.Limit {
		page.Records = page.Records[:q.Limit]
		page.NextCursor = page.Records[q.Limit-1].RecordID
	}
	return page, nil
}
