package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// SnapshotTrace implements kernel.SnapshotTraceRepo (standard §31, §32). The entries are exactly those the snapshot summed: the
// entity's book, the account, posted on or before the snapshot's date and at or before its ledger watermark — so the lineage
// cannot include a posting that arrived after the snapshot was taken, and what it adds up to is checked against the snapshot's figure.
func (r *KernelRepo) SnapshotTrace(ctx context.Context, tenant string, q kernel.SnapshotTraceQuery) (*kernel.SnapshotTrace, error) {
	if _, err := uuid.Parse(q.SnapshotID); err != nil {
		return nil, kernel.ErrNotFound
	}
	limit := q.Limit
	if limit <= 0 {
		limit = kernel.DefaultSnapshotTracePage
	}
	if limit > kernel.MaxSnapshotTracePage {
		limit = kernel.MaxSnapshotTracePage
	}
	var out *kernel.SnapshotTrace
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var s kernel.BalanceSnapshot
		if e := scanSnapshot(t.QueryRow(ctx, `SELECT `+snapCols+` FROM balance_snapshots WHERE tenant_id = $1::uuid AND snapshot_id = $2::uuid`, tenant, q.SnapshotID), &s); e != nil {
			return kernelErr(e)
		}
		var bookCode string
		if e := t.QueryRow(ctx, `SELECT book_code FROM accounting_books WHERE tenant_id = $1::uuid AND book_id = $2::uuid`, tenant, s.BookID).Scan(&bookCode); e != nil {
			return kernelErr(e)
		}
		res := &kernel.SnapshotTrace{SnapshotID: s.SnapshotID, LegalEntityID: s.LegalEntityID, BookID: s.BookID, BookCode: bookCode, FiscalPeriod: s.FiscalPeriod,
			AsOf: s.AsOf, LedgerWatermark: s.LedgerWatermark, ContentHash: s.ContentHash, AccountCode: q.AccountCode, Entries: []kernel.SnapshotTraceEntry{}}
		for _, l := range s.Lines {
			if l.AccountCode == q.AccountCode {
				res.Figure = l
			}
		}

		if e := t.QueryRow(ctx, `
			SELECT COALESCE(sum(debit_amount), 0)::text, COALESCE(sum(credit_amount), 0)::text, COALESCE(sum(debit_amount) - sum(credit_amount), 0)::text, count(*)
			FROM ledger_entries
			WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND account_code = $4 AND posting_date <= $5::date AND entry_seq <= $6`,
			tenant, s.LegalEntityID, bookCode, q.AccountCode, s.AsOf, s.LedgerWatermark).
			Scan(&res.Traced.Debit, &res.Traced.Credit, &res.Traced.Net, &res.Traced.Entries); e != nil {
			return kernelErr(e)
		}
		if res.Figure.AccountCode == "" && res.Traced.Entries == 0 {
			return kernel.ErrNotFound
		}
		res.Reconciles = res.Figure.AccountCode != "" && res.Figure.Debit == res.Traced.Debit && res.Figure.Credit == res.Traced.Credit

		rows, e := t.Query(ctx, `
			SELECT e.entry_seq, e.journal_id::text, h.journal_number, h.journal_type, h.journal_purpose, e.posting_date::text, e.fiscal_period, e.line_number,
			       e.debit_amount::text, e.credit_amount::text, COALESCE(l.description, ''), l.posting_rule_line_ref, l.tax_detail_ref, l.source_line_ref, h.evidence_bundle_id, h.evidence_refs,
			       ev.accounting_event_id::text, ev.event_type, ev.source_system, ev.source_object_type, ev.source_object_id, ev.source_event_id, ev.evidence_refs,
			       px.execution_id::text, px.posting_rule_set_id::text, rs.name, px.rule_version
			FROM ledger_entries e
			JOIN journal_headers h ON h.tenant_id = e.tenant_id AND h.journal_id = e.journal_id
			JOIN journal_lines l ON l.tenant_id = e.tenant_id AND l.journal_line_id = e.journal_line_id
			LEFT JOIN accounting_events ev ON ev.tenant_id = h.tenant_id AND ev.accounting_event_id = h.source_accounting_event_id
			LEFT JOIN posting_executions px ON px.tenant_id = h.tenant_id AND px.execution_id = h.posting_execution_id
			LEFT JOIN posting_rule_sets rs ON rs.tenant_id = px.tenant_id AND rs.posting_rule_set_id = px.posting_rule_set_id
			WHERE e.tenant_id = $1::uuid AND e.legal_entity_id = $2::uuid AND e.book_id = $3 AND e.account_code = $4
			  AND e.posting_date <= $5::date AND e.entry_seq <= $6 AND e.entry_seq > $7
			ORDER BY e.entry_seq LIMIT $8`,
			tenant, s.LegalEntityID, bookCode, q.AccountCode, s.AsOf, s.LedgerWatermark, q.After, limit+1)
		if e != nil {
			return kernelErr(e)
		}
		defer rows.Close()
		for rows.Next() {
			var en kernel.SnapshotTraceEntry
			var number, purpose, ruleRef, taxRef, srcRef, bundle, evID, evType, evSys, evObjType, evObj, evSrc, execID, ruleID, ruleName, ruleVer *string
			var hdrRefs []string
			var evRefs []byte
			if e := rows.Scan(&en.EntrySeq, &en.JournalID, &number, &en.JournalType, &purpose, &en.PostingDate, &en.FiscalPeriod, &en.LineNo, &en.Debit, &en.Credit,
				&en.Description, &ruleRef, &taxRef, &srcRef, &bundle, &hdrRefs, &evID, &evType, &evSys, &evObjType, &evObj, &evSrc, &evRefs, &execID, &ruleID, &ruleName, &ruleVer); e != nil {
				return e
			}
			en.JournalNumber, en.JournalPurpose, en.RuleLineRef, en.TaxDetailRef, en.EvidenceBundleID, en.SourceLineRef = str(number), str(purpose), str(ruleRef), str(taxRef), str(bundle), str(srcRef)
			en.EvidenceRefs = hdrRefs
			if evID != nil {
				src := &kernel.TraceSource{AccountingEventID: *evID, EventType: str(evType), SourceSystem: str(evSys), SourceObjectType: str(evObjType), SourceObjectID: str(evObj), SourceEventID: str(evSrc)}
				if len(evRefs) > 0 {
					_ = json.Unmarshal(evRefs, &src.EvidenceRefs)
				}
				en.Source = src
			}
			if ruleID != nil {
				en.Rule = &kernel.TraceEntryRule{RuleSetID: *ruleID, Name: str(ruleName), Version: str(ruleVer), ExecutionID: str(execID)}
			}
			res.Entries = append(res.Entries, en)
		}
		if e := rows.Err(); e != nil {
			return e
		}
		if len(res.Entries) > limit {
			res.Entries = res.Entries[:limit]
			res.NextAfter = res.Entries[limit-1].EntrySeq
		}
		out = res
		return nil
	})
	return out, err
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}
