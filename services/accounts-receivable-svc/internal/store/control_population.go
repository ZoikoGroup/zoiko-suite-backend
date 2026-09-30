package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

// openInvoicesScope is the in-scope set of the open-invoices control population,
// with the OUTSTANDING amount as of the cut-off:
//
//	0 when status = PAID and payment_received_at::date <= cut-off
//	  (no cut-off: 0 when status = PAID), otherwise the invoice amount.
//
// $1 tenant_id, $2 legal_entity_id, $3 cut-off date (NULL = none). Dates are
// taken in UTC so the result does not depend on the session time zone. The
// explicit tenant_id predicate is deliberate: it is not left to RLS alone.
const openInvoicesScope = `
	WITH scope AS (
		SELECT invoice_id, invoice_number, customer_id, currency_code, status,
		       due_date, amount AS invoice_amount,
		       (created_at AT TIME ZONE 'UTC')::date AS created_date,
		       CASE WHEN status = 'PAID'
		                 AND ($3::date IS NULL
		                      OR (payment_received_at AT TIME ZONE 'UTC')::date <= $3::date)
		            THEN 0::numeric(18,2)
		            ELSE amount
		       END AS outstanding
		FROM customer_invoices
		WHERE tenant_id = $1::uuid
		  AND legal_entity_id = $2::uuid
		  AND ($3::date IS NULL OR (created_at AT TIME ZONE 'UTC')::date <= $3::date)
	)`

// ControlPopulation returns one keyset page of the open-invoices population plus
// the whole-set watermark and declared totals.
//
// Everything runs in ONE REPEATABLE READ, read-only transaction, so the count,
// the per-currency totals, the watermark and the page are read from a single
// snapshot: a write committed between them cannot make them disagree.
//
// Watermark = md5 over the ordered `invoice_id|status|outstanding_amount` of the
// WHOLE in-scope set, suffixed with the row count (":<count>").
func (s *PgStore) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	err := s.withRLSOpts(ctx, q.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var digest string
		if err := tx.QueryRow(ctx, openInvoicesScope+`
			SELECT count(*),
			       md5(COALESCE(string_agg(invoice_id::text || '|' || status || '|' || outstanding::text,
			                               ',' ORDER BY invoice_id), ''))
			FROM scope`, q.TenantID, q.LegalEntityID, q.CutOff,
		).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = digest + ":" + strconv.FormatInt(page.DeclaredTotals.RowCount, 10)

		totals, err := tx.Query(ctx, openInvoicesScope+`
			SELECT currency_code, sum(outstanding)::numeric(18,2)::text
			FROM scope GROUP BY currency_code ORDER BY currency_code`,
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
		rows, err := tx.Query(ctx, openInvoicesScope+`
			SELECT invoice_id::text, invoice_number, outstanding::numeric(18,2)::text, currency_code,
			       to_char(created_date, 'YYYY-MM-DD'), status, customer_id,
			       to_char(due_date, 'YYYY-MM-DD'), invoice_amount::numeric(18,2)::text
			FROM scope
			WHERE ($4::uuid IS NULL OR invoice_id > $4::uuid)
			ORDER BY invoice_id
			LIMIT $5`,
			q.TenantID, q.LegalEntityID, q.CutOff, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			var status, customer, due, invAmount string
			if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
				&status, &customer, &due, &invAmount); err != nil {
				return err
			}
			r.Attributes = map[string]string{
				"status": status, "customer_id": customer, "due_date": due, "invoice_amount": invAmount,
			}
			page.Records = append(page.Records, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	// One row beyond the limit was read only to know whether another page exists.
	if len(page.Records) > q.Limit {
		page.Records = page.Records[:q.Limit]
		page.NextCursor = page.Records[q.Limit-1].RecordID
	}
	return page, nil
}
