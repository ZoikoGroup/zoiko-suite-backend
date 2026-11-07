package arctl

import (
	"context"
	"crypto/md5" //nolint:gosec // a watermark digest, not a security primitive
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Control populations served on GET /v1/control-populations/{population} (docs/architecture/control-population-contract.md):
// keyset pages ordered by record_id, a whole-set watermark (md5 over ordered `record_id|status|amount`, suffixed ":<count>"),
// whole-set declared totals per currency, one REPEATABLE READ read-only snapshot, the 200000-record cap, decimal strings.
//
//	open-invoices        legacy customer_invoices UNION AR-04 invoices (outstanding = AR-07 residual at the cut-off)
//	open-items           AR-07 open items with outstanding > 0 (ar07.Module.ControlPopulation, registered as-is)
//	credit-debit-notes   AR-06 issued notes, amount signed (credit negative)
//	cash-receipts        AR-08 registered receipts
//
// Underscore spellings (open_items, ...) are registered as aliases by the wiring.

// scopeSpec describes a single-snapshot population over a CTE named `scope` with the columns
// id uuid, ref text, amount numeric, currency text, doc_date date, status text, attrs jsonb.
type scopeSpec struct{ cte string }

// $1 tenant, $2 legal entity, $3 cut-off date (NULL = none).

// newPathInvoiceScope: AR-04 issued/corrected invoices issued on or before the cut-off, at their AR-07 outstanding as of the
// cut-off (original + adjustments recorded before the end of the cut-off day, UTC). An invoice with no open item at all is
// reported with the AR-04 settlement rule and attribute open_item=missing (arctl flags it separately).
var newPathInvoiceScope = scopeSpec{cte: `
	WITH oi AS (
		SELECT o.source_document_id AS doc_id, count(*) AS n,
		       sum(o.original_amount + COALESCE((SELECT sum(a.signed_effect) FROM ar07_adjustments a
		            WHERE a.tenant_id = o.tenant_id AND a.open_item_id = o.open_item_id
		              AND ($3::date IS NULL OR a.recorded_at < (($3::date + 1)::timestamp AT TIME ZONE 'UTC'))), 0)) AS outstanding
		FROM ar07_open_items o WHERE o.tenant_id = $1::uuid AND o.legal_entity_id = $2::uuid
		GROUP BY o.source_document_id
	), scope AS (
		SELECT i.invoice_id AS id, i.invoice_number AS ref, i.currency AS currency, i.issue_date AS doc_date,
		       CASE WHEN oi.doc_id IS NOT NULL THEN oi.outstanding
		            WHEN i.settlement_state IN ('PAID','WRITTEN_OFF','CREDITED')
		                 AND ($3::date IS NULL OR (COALESCE(i.settlement_changed_at, i.issued_at) AT TIME ZONE 'UTC')::date <= $3::date)
		            THEN 0::numeric ELSE i.gross_total END AS amount,
		       CASE WHEN oi.doc_id IS NULL THEN i.settlement_state
		            WHEN oi.outstanding = 0 THEN 'SETTLED'
		            WHEN oi.outstanding = i.gross_total THEN 'OPEN' ELSE 'PARTIALLY_SETTLED' END AS status,
		       jsonb_build_object('status', CASE WHEN oi.doc_id IS NULL THEN i.settlement_state WHEN oi.outstanding = 0 THEN 'SETTLED'
		                                         WHEN oi.outstanding = i.gross_total THEN 'OPEN' ELSE 'PARTIALLY_SETTLED' END,
		                          'lifecycle_state', i.lifecycle_state, 'customer_id', i.customer_party_id::text,
		                          'due_date', to_char(i.due_date, 'YYYY-MM-DD'), 'invoice_amount', i.gross_total::text,
		                          'open_item', CASE WHEN oi.doc_id IS NULL THEN 'missing' WHEN oi.n > 1 THEN 'duplicate' ELSE 'present' END,
		                          'origin', 'AR04') AS attrs
		FROM ar04_invoices i LEFT JOIN oi ON oi.doc_id = i.invoice_id
		WHERE i.tenant_id = $1::uuid AND i.legal_entity_id = $2::uuid AND i.lifecycle_state IN ('ISSUED','CORRECTED')
		  AND ($3::date IS NULL OR i.issue_date <= $3::date)
	)`}

// notesScope: AR-06 issued credit/debit notes (ISSUED, APPLIED, SETTLED) issued on or before the cut-off; credit notes negative.
var notesScope = scopeSpec{cte: `
	WITH scope AS (
		SELECT c.correction_id AS id, COALESCE(c.document_number, '') AS ref, c.currency AS currency, c.issue_date AS doc_date,
		       CASE WHEN c.kind = 'CREDIT_NOTE' THEN -c.gross_total ELSE c.gross_total END AS amount, c.state AS status,
		       jsonb_build_object('kind', c.kind, 'status', c.state, 'customer_id', c.customer_party_id::text,
		                          'source_invoice_id', c.source_invoice_id::text, 'application_state', c.application_state,
		                          'accounting_state', c.accounting_state, 'gross_amount', c.gross_total::text) AS attrs
		FROM ar06_corrections c
		WHERE c.tenant_id = $1::uuid AND c.legal_entity_id = $2::uuid AND c.state IN ('ISSUED','APPLIED','SETTLED')
		  AND ($3::date IS NULL OR c.issue_date <= $3::date)
	)`}

// receiptsScope: AR-08 registered receipts with value date on or before the cut-off; amount is the receipt amount.
var receiptsScope = scopeSpec{cte: `
	WITH scope AS (
		SELECT r.receipt_id AS id, r.settlement_ref AS ref, r.currency::text AS currency, r.value_date AS doc_date, r.amount AS amount,
		       r.state AS status,
		       jsonb_build_object('status', r.state, 'customer_id', COALESCE(r.customer_party_id::text, ''),
		                          'applied_amount', r.applied_amount::text, 'unapplied_amount', r.unapplied_amount::text,
		                          'difference_amount', r.difference_amount::text, 'blocked', r.blocked::text,
		                          'bank_transaction_id', r.bank_transaction_id::text) AS attrs
		FROM ar08_receipts r
		WHERE r.tenant_id = $1::uuid AND r.legal_entity_id = $2::uuid AND ($3::date IS NULL OR r.value_date <= $3::date)
	)`}

// agedUnappliedScope: AR-08 receipts that still hold unapplied cash (unapplied_amount > 0) with a value date strictly before
// the cut-off ($3, the `unapplied_before` parameter). Amount is the UNAPPLIED amount, so declared totals are the aged
// unapplied-cash exposure; age_days is computed from the parameter, never from now(). Blocked and fully-unapplied receipts are
// reported (nothing is pre-filtered) and carry their flags in attributes.
var agedUnappliedScope = scopeSpec{cte: `
	WITH scope AS (
		SELECT r.receipt_id AS id, r.settlement_ref AS ref, r.currency::text AS currency, r.value_date AS doc_date,
		       r.unapplied_amount AS amount, r.state AS status,
		       jsonb_build_object('status', r.state, 'customer_id', COALESCE(r.customer_party_id::text, ''),
		                          'receipt_amount', r.amount::text, 'applied_amount', r.applied_amount::text,
		                          'unapplied_amount', r.unapplied_amount::text, 'blocked', r.blocked::text,
		                          'age_days', ($3::date - r.value_date)::text) AS attrs
		FROM ar08_receipts r
		WHERE r.tenant_id = $1::uuid AND r.legal_entity_id = $2::uuid
		  AND r.unapplied_amount > 0 AND r.value_date < $3::date
	)`}

// servePopulation runs the contract over one scope in a single REPEATABLE READ read-only snapshot.
func servePopulation(ctx context.Context, db *kit.DB, spec scopeSpec, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	page := &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}}}
	err := db.WithTxOpts(ctx, q.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var digest string
		if err := tx.QueryRow(ctx, spec.cte+`
			SELECT count(*), md5(COALESCE(string_agg(id::text || '|' || status || '|' || amount::text, ',' ORDER BY id), '')) FROM scope`,
			q.TenantID, q.LegalEntityID, q.CutOff).Scan(&page.DeclaredTotals.RowCount, &digest); err != nil {
			return err
		}
		if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
			return domain.ErrPopulationTooLarge
		}
		page.Watermark = digest + ":" + strconv.FormatInt(page.DeclaredTotals.RowCount, 10)

		totals, err := tx.Query(ctx, spec.cte+`SELECT currency, sum(amount)::numeric(20,4)::text FROM scope GROUP BY currency ORDER BY currency`,
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
		rows, err := tx.Query(ctx, spec.cte+`
			SELECT id::text, ref, amount::numeric(20,4)::text, currency, to_char(doc_date, 'YYYY-MM-DD'), attrs::text
			FROM scope WHERE ($4::uuid IS NULL OR id > $4::uuid) ORDER BY id LIMIT $5`,
			q.TenantID, q.LegalEntityID, q.CutOff, after, q.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ControlRecord
			var attrs string
			if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &attrs); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(attrs), &r.Attributes); err != nil {
				return err
			}
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

// NewPathInvoices serves AR-04 invoices at their AR-07 outstanding (the new-path half of `open-invoices`).
func NewPathInvoices(db *kit.DB) PopulationFunc {
	return func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return servePopulation(ctx, db, newPathInvoiceScope, q)
	}
}

// CreditDebitNotes serves the `credit-debit-notes` population.
func CreditDebitNotes(db *kit.DB) PopulationFunc {
	return func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return servePopulation(ctx, db, notesScope, q)
	}
}

// CashReceipts serves the `cash-receipts` population.
func CashReceipts(db *kit.DB) PopulationFunc {
	return func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return servePopulation(ctx, db, receiptsScope, q)
	}
}

// ParamUnappliedBefore is the required parameter of the `aged-unapplied-receipts` population: a YYYY-MM-DD date; receipts
// with a value date strictly before it are in scope.
const ParamUnappliedBefore = "unapplied_before"

// AgedUnappliedReceipts serves the `aged-unapplied-receipts` population (FIN-CTRL-029). It is a violation-set population:
// the policy age limit is a controlled decision, so the caller chooses the cut-off (run date minus the approved limit).
func AgedUnappliedReceipts(db *kit.DB) PopulationFunc {
	return func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		d, err := time.Parse("2006-01-02", q.Params[ParamUnappliedBefore])
		if err != nil {
			return nil, fmt.Errorf("%w: %s must be YYYY-MM-DD", domain.ErrInvalidPopulationParam, ParamUnappliedBefore)
		}
		q.CutOff = &d
		return servePopulation(ctx, db, agedUnappliedScope, q)
	}
}

// Union serves the concatenation of disjoint populations (record ids never collide: legacy and AR-04 invoice ids are
// distinct UUIDs) as ONE population: records merged in record_id order (UUID text order equals Postgres uuid order), the
// watermark is md5 over the part watermarks suffixed with the total count, totals are summed exactly per currency.
//
// Backward compatibility: when every part after the first is empty the FIRST part's page is returned verbatim, so a tenant
// with only legacy invoices sees exactly the legacy watermark, totals and formatting. Parts are read in separate
// snapshots; a write between two page requests changes a part watermark and therefore the combined watermark, which the
// consumer (financial-control-svc) detects as a mid-extraction change.
func Union(parts ...PopulationFunc) PopulationFunc {
	return func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		pages := make([]*domain.ControlPopulationPage, len(parts))
		var total int64
		nonEmptyAfterFirst := false
		for i, p := range parts {
			pg, err := p(ctx, q)
			if err != nil {
				return nil, err
			}
			pages[i] = pg
			total += pg.DeclaredTotals.RowCount
			if i > 0 && pg.DeclaredTotals.RowCount > 0 {
				nonEmptyAfterFirst = true
			}
		}
		if !nonEmptyAfterFirst {
			return pages[0], nil
		}
		if total > domain.MaxControlPopulationRecords {
			return nil, domain.ErrPopulationTooLarge
		}
		out := &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, DeclaredTotals: domain.DeclaredTotals{RowCount: total, Totals: map[string]string{}}}
		sum := map[string]kit.Amount{}
		h := md5.New() //nolint:gosec
		more := false
		for _, pg := range pages {
			h.Write([]byte(pg.Watermark + "|"))
			out.Records = append(out.Records, pg.Records...)
			more = more || pg.NextCursor != ""
			for cur, s := range pg.DeclaredTotals.Totals {
				a, err := kit.ParseAmount(s)
				if err != nil {
					return nil, err
				}
				sum[cur] = sum[cur].Add(a)
			}
		}
		for cur, a := range sum {
			out.DeclaredTotals.Totals[cur] = a.String()
		}
		out.Watermark = hex.EncodeToString(h.Sum(nil)) + ":" + strconv.FormatInt(total, 10)
		sort.Slice(out.Records, func(i, j int) bool { return out.Records[i].RecordID < out.Records[j].RecordID })
		if len(out.Records) > q.Limit {
			more = true
			out.Records = out.Records[:q.Limit]
		}
		if more && len(out.Records) > 0 {
			out.NextCursor = out.Records[len(out.Records)-1].RecordID
		}
		return out, nil
	}
}

// PopulationSubledger adapts a population provider (open-invoices) to a SubledgerSource: the whole-set declared totals.
type PopulationSubledger struct{ Provider PopulationFunc }

func (p PopulationSubledger) OpenReceivable(ctx context.Context, tenantID, legalEntityID string, cutoff time.Time) (*Subledger, error) {
	c := cutoff
	pg, err := p.Provider(ctx, domain.ControlPopulationQuery{TenantID: tenantID, LegalEntityID: legalEntityID, CutOff: &c, Limit: 1})
	if err != nil {
		return nil, err
	}
	out := &Subledger{Available: true, Totals: map[string]kit.Amount{}, Count: pg.DeclaredTotals.RowCount, Watermark: pg.Watermark}
	for cur, s := range pg.DeclaredTotals.Totals {
		a, err := kit.ParseAmount(s)
		if err != nil {
			return nil, err
		}
		out.Totals[cur] = a
	}
	return out, nil
}
