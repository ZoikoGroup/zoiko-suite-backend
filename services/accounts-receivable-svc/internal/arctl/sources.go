package arctl

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// ---- narrow read interfaces the run consumes (fakes in tests) ----

// DocumentSource lists the issued documents the receivable subledger must carry exactly once: AR-04 issued invoices
// (ISSUED / CORRECTED) and AR-06 issued debit notes. Credit notes are not open items (they adjust their source item).
type DocumentSource interface {
	ExpectedOpenItemDocuments(ctx context.Context, tenantID, legalEntityID string) ([]ExpectedDoc, error)
}

// OpenItemCompleteness is ar07.Module.Completeness.
type OpenItemCompleteness interface {
	Completeness(ctx context.Context, tenantID, legalEntityID string, docs []ar07.ExpectedDocument) (*ar07.CompletenessReport, error)
}

// PostingSource lists the business facts that must have an accounting event and the observed queue rows for the entity.
type PostingSource interface {
	PostingFacts(ctx context.Context, tenantID, legalEntityID string) ([]PostingFact, []QueueRow, error)
}

// SubledgerSource is the AR open receivable per currency at a cut-off (the served `open-invoices` population totals:
// legacy invoices + AR-04 invoices at their AR-07 residual).
type SubledgerSource interface {
	OpenReceivable(ctx context.Context, tenantID, legalEntityID string, cutoff time.Time) (*Subledger, error)
}

// ExceptionSource is one owning module's visible exceptions (ar10.Module.ControlExceptions is adapted by ExceptionLister).
type ExceptionSource interface {
	Name() string
	Exceptions(ctx context.Context, tenantID, legalEntityID string) ([]ExceptionItem, error)
}

// GLBalanceSource reads the GL balance of the AR control account(s) resolved from mapping keys.
type GLBalanceSource interface {
	ControlAccountBalance(ctx context.Context, c svcclient.Caller, legalEntityID, periodID string, mappingKeys []string) (*GLRead, error)
}

// ---- adapters over exported module methods ----

// ListerFunc adapts any function returning (kind, aggregate, detail) triples (e.g. ar10.Module.ControlExceptions) to an
// ExceptionSource.
type ListerFunc func(ctx context.Context, tenantID, legalEntityID string) ([]ExceptionItem, error)

type namedLister struct {
	name string
	fn   ListerFunc
}

func (n namedLister) Name() string { return n.name }
func (n namedLister) Exceptions(ctx context.Context, t, e string) ([]ExceptionItem, error) {
	return n.fn(ctx, t, e)
}

// NewExceptionSource names a lister as a source, e.g. NewExceptionSource("AR10", func(...) { ex, err := ar10m.ControlExceptions(...); map })
func NewExceptionSource(name string, fn ListerFunc) ExceptionSource { return namedLister{name, fn} }

// ---- PgSources: read-only control reads over the owner modules' tables ----
//
// A control is only independent if it does not ask the module it checks to grade itself, and the owner modules export no
// list/enumeration method, so this adapter reads their tables with read-only, tenant-scoped SELECTs. It never writes them.
// (Suggested follow-up: each owner exports a ListXxxForControl method and this adapter becomes a thin wrapper.)

// PgSources implements DocumentSource, PostingSource and the roll-up ExceptionSources over the shared database.
type PgSources struct{ DB *kit.DB }

func (s PgSources) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return s.DB.WithTxOpts(ctx, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// ExpectedOpenItemDocuments: AR-04 invoices ISSUED/CORRECTED and AR-06 DEBIT_NOTEs ISSUED/APPLIED/SETTLED.
func (s PgSources) ExpectedOpenItemDocuments(ctx context.Context, tenantID, legalEntityID string) ([]ExpectedDoc, error) {
	var out []ExpectedDoc
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT invoice_id::text, 'INVOICE', COALESCE(invoice_number,''), currency, gross_total, 'AR04'
			  FROM ar04_invoices WHERE tenant_id = $1 AND legal_entity_id = $2 AND lifecycle_state IN ('ISSUED','CORRECTED')
			UNION ALL
			SELECT correction_id::text, 'DEBIT_NOTE', COALESCE(document_number,''), currency, gross_total, 'AR06'
			  FROM ar06_corrections WHERE tenant_id = $1 AND legal_entity_id = $2 AND kind = 'DEBIT_NOTE' AND state IN ('ISSUED','APPLIED','SETTLED')
			ORDER BY 1`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d ExpectedDoc
			if err := rows.Scan(&d.DocumentID, &d.DocumentKind, &d.Number, &d.Currency, &d.Amount, &d.Origin); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, kit.MapPgErr(err)
}

// PostingFacts: issued invoices (AR-04 and legacy), issued credit/debit notes, applied and unapplied cash applications,
// settled refunds and committed write-offs, with the queue rows that exist for them.
func (s PgSources) PostingFacts(ctx context.Context, tenantID, legalEntityID string) ([]PostingFact, []QueueRow, error) {
	var facts []PostingFact
	var queue []QueueRow
	err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'INVOICE', invoice_id::text, invoice_id::text, currency, gross_total, 'AR04', false, '' FROM ar04_invoices
			  WHERE tenant_id = $1 AND legal_entity_id = $2 AND lifecycle_state IN ('ISSUED','CORRECTED')
			UNION ALL
			SELECT kind, correction_id::text, correction_id::text, currency, gross_total, 'AR06', false, '' FROM ar06_corrections
			  WHERE tenant_id = $1 AND legal_entity_id = $2 AND state IN ('ISSUED','APPLIED','SETTLED')
			UNION ALL
			SELECT 'CASH_APPLICATION', a.application_id::text, a.application_id::text, r.currency, a.applied_amount, 'AR08', false, ''
			  FROM ar08_applications a JOIN ar08_receipts r ON r.receipt_id = a.receipt_id AND r.tenant_id = a.tenant_id
			  WHERE a.tenant_id = $1 AND a.legal_entity_id = $2
			UNION ALL
			SELECT 'CASH_UNAPPLICATION', a.application_id::text, a.application_id::text || ':unapply', r.currency, a.applied_amount, 'AR08', false, ''
			  FROM ar08_applications a JOIN ar08_receipts r ON r.receipt_id = a.receipt_id AND r.tenant_id = a.tenant_id
			  WHERE a.tenant_id = $1 AND a.legal_entity_id = $2 AND a.status = 'UNAPPLIED'
			UNION ALL
			SELECT 'REFUND', refund_id::text, refund_id::text, currency::text, amount, 'AR10', false, '' FROM ar10_refunds
			  WHERE tenant_id = $1 AND legal_entity_id = $2 AND state = 'SETTLED'
			UNION ALL
			SELECT 'WRITE_OFF', writeoff_id::text, writeoff_id::text, currency::text, amount, 'AR10', false, '' FROM ar10_writeoffs
			  WHERE tenant_id = $1 AND legal_entity_id = $2 AND state = 'COMMITTED'
			UNION ALL
			SELECT 'LEGACY_INVOICE', invoice_id::text, invoice_id::text, currency_code, (amount * 10000)::bigint, 'LEGACY', true, COALESCE(issuance_journal_id,'')
			  FROM customer_invoices WHERE tenant_id = $1 AND legal_entity_id = $2
			ORDER BY 1, 2, 3`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f PostingFact
			var raw int64
			if err := rows.Scan(&f.SourceType, &f.SourceID, &f.SourceEventID, &f.Currency, &raw, &f.Origin, &f.Legacy, &f.JournalID); err != nil {
				rows.Close()
				return err
			}
			f.Amount = kit.Amount(raw)
			facts = append(facts, f)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		qr, err := tx.Query(ctx, `
			SELECT source_event_id, source_type, status, attempts, created_at, COALESCE(journal_id,''), COALESCE(last_error,'')
			  FROM ar_accounting_posting_requests WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY source_event_id`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer qr.Close()
		for qr.Next() {
			var q QueueRow
			if err := qr.Scan(&q.SourceEventID, &q.SourceType, &q.Status, &q.Attempts, &q.CreatedAt, &q.JournalID, &q.LastError); err != nil {
				return err
			}
			queue = append(queue, q)
		}
		return qr.Err()
	})
	return facts, queue, kit.MapPgErr(err)
}

// ---- roll-up exception sources over AR-04 / AR-06 / AR-08 / AR-05 (AR-10 is adapted from its exported method) ----

// Rollup returns the Pg-backed exception sources. e-invoice PENDING_UNKNOWN older than einvoiceAfter is an exception.
func (s PgSources) Rollup(now func() time.Time, einvoiceAfter time.Duration) []ExceptionSource {
	q := func(name, sql string, args func(t, e string) []any) ExceptionSource {
		return NewExceptionSource(name, func(ctx context.Context, tenantID, entityID string) ([]ExceptionItem, error) {
			var out []ExceptionItem
			err := s.read(ctx, tenantID, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, sql, args(tenantID, entityID)...)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var e ExceptionItem
					e.Source = name
					if err := rows.Scan(&e.Kind, &e.AggregateID, &e.Detail); err != nil {
						return err
					}
					out = append(out, e)
				}
				return rows.Err()
			})
			return out, kit.MapPgErr(err)
		})
	}
	std := func(t, e string) []any { return []any{t, e} }
	cutoff := func(t, e string) []any { return []any{t, e, now().Add(-einvoiceAfter)} }
	return []ExceptionSource{
		q("AR04", `SELECT 'ACCOUNTING_MISSING', i.invoice_id::text, 'issued invoice has no accounting posting request'
			FROM ar04_invoices i WHERE i.tenant_id = $1 AND i.legal_entity_id = $2 AND i.lifecycle_state IN ('ISSUED','CORRECTED')
			AND NOT EXISTS (SELECT 1 FROM ar_accounting_posting_requests p WHERE p.tenant_id = i.tenant_id AND p.source_event_id = i.invoice_id::text)
			ORDER BY 2 LIMIT 200`, std),
		q("AR06", `SELECT 'ISSUED_NOTE_WITHOUT_POSTING_REQUEST', c.correction_id::text, 'issued ' || c.kind || ' has no accounting posting request'
			FROM ar06_corrections c WHERE c.tenant_id = $1 AND c.legal_entity_id = $2 AND c.state IN ('ISSUED','APPLIED','SETTLED')
			AND NOT EXISTS (SELECT 1 FROM ar_accounting_posting_requests p WHERE p.tenant_id = c.tenant_id AND p.source_event_id = c.correction_id::text)
			ORDER BY 2 LIMIT 200`, std),
		q("AR08", `SELECT CASE WHEN r.blocked THEN 'RECEIPT_BLOCKED' ELSE 'RECEIPT_CONTROL_EXCEPTION' END, r.receipt_id::text,
			COALESCE(r.control_exception, 'receipt is blocked')
			FROM ar08_receipts r WHERE r.tenant_id = $1 AND r.legal_entity_id = $2 AND (r.blocked OR COALESCE(r.control_exception,'') <> '')
			UNION ALL
			SELECT 'NO_POSTING', a.application_id::text, 'the cash application has no accounting posting request'
			FROM ar08_applications a WHERE a.tenant_id = $1 AND a.legal_entity_id = $2
			AND NOT EXISTS (SELECT 1 FROM ar_accounting_posting_requests p WHERE p.tenant_id = a.tenant_id AND p.source_event_id = a.application_id::text)
			ORDER BY 2 LIMIT 200`, std),
		q("AR05", `SELECT 'EINVOICE_PENDING_UNKNOWN', s.submission_id::text, 'e-invoice submission state is unknown beyond the threshold'
			FROM ar05_einvoice_submissions s WHERE s.tenant_id = $1 AND s.legal_entity_id = $2 AND s.state = 'PENDING_UNKNOWN' AND s.updated_at < $3
			ORDER BY 2 LIMIT 200`, cutoff),
	}
}

// ---- population page type re-export for the registry wiring ----

// PopulationFunc is the control population provider signature (same as handler.PopulationFunc).
type PopulationFunc = func(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error)
