package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// inScope is the single definition of the population, shared by the aggregate
// and the page so they can never describe different sets. $1 tenant, $2 legal
// entity, $3 inclusive created_at cut-off date (NULL = none). The tenant_id
// predicate is explicit: RLS alone does not bind a superuser connection.
const inScope = `
	FROM vendor_invoices
	WHERE tenant_id = $1
	  AND legal_entity_id = $2
	  AND ($3::date IS NULL OR (created_at AT TIME ZONE 'UTC')::date <= $3::date)`

// ControlPopulation serves one page of the open-invoices control population.
//
// One REPEATABLE READ, READ ONLY transaction: the whole-set count, per-currency
// totals and watermark, and the page itself, all come from a single snapshot, so
// a commit between them cannot make declared_totals disagree with the records.
//
// The invoice total is vendor_invoices.amount (the gross header total; lines
// roll up to it — net + tax = amount is enforced at intake).
func (s *PgStore) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	var cutoff any // untyped nil -> SQL NULL
	if q.PeriodEnd != "" {
		cutoff = q.PeriodEnd
	}

	err := s.withRLSOpts(ctx, q.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// Whole-set count and content digest, computed by the database.
		var digest string
		if err := tx.QueryRow(ctx,
			`SELECT count(*), COALESCE(md5(string_agg(invoice_id::text || '|' || status || '|' || amount::text, ',' ORDER BY invoice_id)), '')`+inScope,
			q.TenantID, q.LegalEntityID, cutoff).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return mapPgError(err)
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = fmt.Sprintf("%d:%s", page.DeclaredTotals.RowCount, digest)

		rows, err := tx.Query(ctx,
			`SELECT currency_code, sum(amount)::text`+inScope+` GROUP BY currency_code`,
			q.TenantID, q.LegalEntityID, cutoff)
		if err != nil {
			return mapPgError(err)
		}
		for rows.Next() {
			var cur, total string
			if err := rows.Scan(&cur, &total); err != nil {
				rows.Close()
				return err
			}
			page.DeclaredTotals.Totals[cur] = total
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapPgError(err)
		}

		var after any
		if q.AfterRecordID != "" {
			after = q.AfterRecordID
		}
		// limit+1 to learn whether another page exists without a second query.
		prows, err := tx.Query(ctx,
			`SELECT invoice_id::text, invoice_number, amount::text, currency_code,
			        ((created_at AT TIME ZONE 'UTC')::date)::text, status, vendor_id, due_date::text`+inScope+`
			   AND ($4::uuid IS NULL OR invoice_id > $4::uuid)
			 ORDER BY invoice_id ASC
			 LIMIT $5`,
			q.TenantID, q.LegalEntityID, cutoff, after, q.Limit+1)
		if err != nil {
			return mapPgError(err)
		}
		defer prows.Close()
		for prows.Next() {
			var r domain.ControlRecord
			var status, vendor, due string
			if err := prows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &status, &vendor, &due); err != nil {
				return err
			}
			r.Attributes = map[string]string{"status": status, "vendor_id": vendor, "due_date": due}
			page.Records = append(page.Records, r)
		}
		return mapPgError(prows.Err())
	})
	if err != nil {
		return nil, err
	}
	if len(page.Records) > q.Limit {
		page.Records = page.Records[:q.Limit]
		page.NextRecordID = page.Records[q.Limit-1].RecordID
	}
	return page, nil
}
