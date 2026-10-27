package ar04

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// newPathScope is the in-scope set of the AR-04 open-invoices control population, mirroring
// store.ControlPopulation (docs/architecture/control-population-contract.md) for new-path invoices:
// every ISSUED or CORRECTED invoice of the legal entity issued on or before the cut-off, with the
// OUTSTANDING amount as of the cut-off:
//
//	0 when the settlement dimension is PAID / WRITTEN_OFF / CREDITED and that settlement was
//	  recorded on or before the cut-off (no cut-off: 0 when so settled), otherwise the gross.
//
// A PARTIALLY_PAID invoice is reported at its full gross here: AR-04 does not own the residual
// (AR-07 does); the cross-domain population must take partial residuals from the open items.
// $1 tenant_id, $2 legal_entity_id, $3 cut-off date (NULL = none), dates in UTC, explicit tenant predicate.
const newPathScope = `
	WITH scope AS (
		SELECT invoice_id, invoice_number, customer_party_id, currency, settlement_state, lifecycle_state, due_date,
		       gross_total AS invoice_amount, issue_date AS doc_date,
		       CASE WHEN settlement_state IN ('PAID','WRITTEN_OFF','CREDITED')
		                 AND ($3::date IS NULL
		                      OR (COALESCE(settlement_changed_at, issued_at) AT TIME ZONE 'UTC')::date <= $3::date)
		            THEN 0::numeric(18,2)
		            ELSE gross_total::numeric(18,2)
		       END AS outstanding
		FROM ar04_invoices
		WHERE tenant_id = $1::uuid
		  AND legal_entity_id = $2::uuid
		  AND lifecycle_state IN ('ISSUED','CORRECTED')
		  AND ($3::date IS NULL OR issue_date <= $3::date)
	)`

// ControlPopulation returns one keyset page of the AR-04 new-path open-invoices population plus the
// whole-set watermark and declared totals, all read from ONE REPEATABLE READ read-only snapshot.
// Same contract and cap as the legacy store.ControlPopulation, so main.go can union the two:
//
//	Watermark = md5 over the ordered `invoice_id|settlement_state|outstanding` of the WHOLE set, suffixed ":<count>".
//
// Each record carries attributes status (the settlement dimension), lifecycle_state, customer_id,
// due_date and invoice_amount.
func (m *Module) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}},
	}
	err := m.deps.DB.WithTxOpts(ctx, q.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var digest string
		if err := tx.QueryRow(ctx, newPathScope+`
			SELECT count(*),
			       md5(COALESCE(string_agg(invoice_id::text || '|' || settlement_state || '|' || outstanding::text, ',' ORDER BY invoice_id), ''))
			FROM scope`, q.TenantID, q.LegalEntityID, q.CutOff).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = digest + ":" + strconv.FormatInt(page.DeclaredTotals.RowCount, 10)

		totals, err := tx.Query(ctx, newPathScope+`
			SELECT currency, sum(outstanding)::numeric(18,2)::text FROM scope GROUP BY currency ORDER BY currency`, q.TenantID, q.LegalEntityID, q.CutOff)
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
		rows, err := tx.Query(ctx, newPathScope+`
			SELECT invoice_id::text, invoice_number, outstanding::numeric(18,2)::text, currency, to_char(doc_date, 'YYYY-MM-DD'),
			       settlement_state, lifecycle_state, customer_party_id::text, to_char(due_date, 'YYYY-MM-DD'), invoice_amount::numeric(18,2)::text
			FROM scope
			WHERE ($4::uuid IS NULL OR invoice_id > $4::uuid)
			ORDER BY invoice_id
			LIMIT $5`, q.TenantID, q.LegalEntityID, q.CutOff, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			var status, lifecycle, customer, due, invAmount string
			if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &status, &lifecycle, &customer, &due, &invAmount); err != nil {
				return err
			}
			r.Attributes = map[string]string{"status": status, "lifecycle_state": lifecycle, "customer_id": customer, "due_date": due, "invoice_amount": invAmount}
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

// NewPathInvoiceExists reports whether an invoice with this id, or this number (optionally within one
// legal entity), exists in the AR-04 tables. The legacy pay endpoint calls it to refuse (409
// CASH_APPLICATION_REQUIRED) any new-path invoice: those settle only through AR-08 cash application.
func (m *Module) NewPathInvoiceExists(ctx context.Context, tenantID, legalEntityID, invoiceID, invoiceNumber string) (bool, error) {
	var exists bool
	err := m.deps.DB.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ar04_invoices WHERE tenant_id = $1 AND (
				($2 <> '' AND invoice_id::text = $2)
			 OR ($3 <> '' AND invoice_number = $3 AND ($4 = '' OR legal_entity_id::text = $4))))`,
			tenantID, invoiceID, invoiceNumber, legalEntityID).Scan(&exists)
	})
	return exists, err
}
