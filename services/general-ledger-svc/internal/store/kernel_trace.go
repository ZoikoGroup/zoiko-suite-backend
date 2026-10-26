package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

type tracePoint struct{ eventID, bookID, basis string }

// PostingTrace implements kernel.Repo (GetPostingTrace, standard §32): the lineage of a posting from the journal back to the
// accounting event and rule version, and forward to the reversals and corrections that relate to it. Journals that did not
// come from the kernel are traced as far as their own header allows.
func (r *KernelRepo) PostingTrace(ctx context.Context, tenant string, q kernel.TraceQuery) (*kernel.Trace, error) {
	var out *kernel.Trace
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var e error
		out, e = buildTrace(ctx, t, tenant, q)
		return e
	})
	return out, err
}

func buildTrace(ctx context.Context, t pgx.Tx, tenant string, q kernel.TraceQuery) (*kernel.Trace, error) {
	var points []tracePoint
	var legacyJournal string
	eventID := q.AccountingEventID

	switch {
	case q.JournalID != "":
		var ev, book *string
		if err := t.QueryRow(ctx, `SELECT source_accounting_event_id::text, accounting_book_id::text FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`,
			tenant, q.JournalID).Scan(&ev, &book); err != nil {
			return nil, kernelErr(err)
		}
		if ev == nil {
			legacyJournal = q.JournalID
			break
		}
		eventID = *ev
		// a reversal points at the event of the journal it reverses; walk back to the posting that owns it
		cur := q.JournalID
		for i := 0; i < 50; i++ {
			var prev *string
			if err := t.QueryRow(ctx, `SELECT COALESCE(reversal_of_journal_id, correction_of_journal_id)::text FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, tenant, cur).Scan(&prev); err != nil {
				return nil, kernelErr(err)
			}
			if prev == nil {
				break
			}
			cur = *prev
		}
		var basis, bookID string
		err := t.QueryRow(ctx, `SELECT accounting_book_id::text, posting_basis FROM event_posting_links WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, tenant, cur).Scan(&bookID, &basis)
		switch {
		case err == nil:
			points = append(points, tracePoint{eventID, bookID, basis})
		case errors.Is(err, pgx.ErrNoRows):
			legacyJournal = q.JournalID
		default:
			return nil, err
		}
	case q.ExecutionID != "":
		var ev, book, basis *string
		if err := t.QueryRow(ctx, `SELECT accounting_event_id::text, accounting_book_id::text, posting_basis FROM posting_executions WHERE tenant_id = $1::uuid AND execution_id = $2::uuid`,
			tenant, q.ExecutionID).Scan(&ev, &book, &basis); err != nil {
			return nil, kernelErr(err)
		}
		if ev == nil || book == nil {
			return nil, kernel.ErrNotFound
		}
		eventID = *ev
		b := kernel.DefaultPostingBasis
		if basis != nil {
			b = *basis
		}
		points = append(points, tracePoint{eventID, *book, b})
	case eventID != "":
		rows, err := t.Query(ctx, `SELECT accounting_book_id::text, posting_basis FROM event_posting_links WHERE tenant_id = $1::uuid AND accounting_event_id = $2::uuid ORDER BY created_at`, tenant, eventID)
		if err != nil {
			return nil, kernelErr(err)
		}
		for rows.Next() {
			p := tracePoint{eventID: eventID}
			if err := rows.Scan(&p.bookID, &p.basis); err != nil {
				rows.Close()
				return nil, err
			}
			points = append(points, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		return nil, kernel.ErrBadRequest
	}

	tr := &kernel.Trace{Postings: []kernel.TracePosting{}}
	if eventID != "" {
		ev, err := getEventTx(ctx, t, tenant, eventID)
		if err != nil {
			return nil, err
		}
		tr.Event = ev
		rows, err := t.Query(ctx, `SELECT accounting_event_id::text FROM accounting_events WHERE tenant_id = $1::uuid AND supersedes_event_id = $2::uuid ORDER BY registered_at`, tenant, eventID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			tr.SupersededBy = append(tr.SupersededBy, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if legacyJournal != "" {
		tp := kernel.TracePosting{Journals: []kernel.TraceJournal{}}
		if err := traceJournalChain(ctx, t, tenant, legacyJournal, &tp); err != nil {
			return nil, err
		}
		tr.Postings = append(tr.Postings, tp)
	}
	for _, p := range points {
		tp, err := tracePosting(ctx, t, tenant, p)
		if err != nil {
			return nil, err
		}
		tr.Postings = append(tr.Postings, *tp)
	}
	if len(tr.Postings) == 0 && tr.Event == nil {
		return nil, kernel.ErrNotFound
	}
	return tr, nil
}

func tracePosting(ctx context.Context, t pgx.Tx, tenant string, p tracePoint) (*kernel.TracePosting, error) {
	tp := &kernel.TracePosting{Basis: p.basis, Journals: []kernel.TraceJournal{}}

	var b kernel.TraceBook
	if err := t.QueryRow(ctx, `SELECT book_id::text, book_code, book_type, accounting_framework_id, functional_currency FROM accounting_books WHERE tenant_id = $1::uuid AND book_id = $2::uuid`,
		tenant, p.bookID).Scan(&b.BookID, &b.BookCode, &b.BookType, &b.Framework, &b.FunctionalCurrency); err != nil {
		return nil, kernelErr(err)
	}
	tp.Book = &b

	var jid, execID, ruleID string
	if err := t.QueryRow(ctx, `SELECT journal_id::text, posting_execution_id::text, posting_rule_set_id::text FROM event_posting_links
		WHERE tenant_id = $1::uuid AND accounting_event_id = $2::uuid AND accounting_book_id = $3::uuid AND posting_basis = $4`,
		tenant, p.eventID, p.bookID, p.basis).Scan(&jid, &execID, &ruleID); err != nil {
		return nil, kernelErr(err)
	}

	var rule kernel.TraceRule
	if err := t.QueryRow(ctx, `SELECT posting_rule_set_id::text, name, version, content_hash, test_pack_id, status FROM posting_rule_sets WHERE tenant_id = $1::uuid AND posting_rule_set_id = $2::uuid`,
		tenant, ruleID).Scan(&rule.RuleSetID, &rule.Name, &rule.Version, &rule.ContentHash, &rule.TestPackID, &rule.Status); err != nil {
		return nil, kernelErr(err)
	}
	tp.Rule = &rule

	var ex kernel.TraceExecution
	var calc []byte
	var jev *string
	if err := t.QueryRow(ctx, `SELECT execution_id::text, kind, status, COALESCE(proposal_hash, ''), ledger_watermark, journal_event_id, created_by_principal_id, created_at, committed_at, calculation_trace::text
		FROM posting_executions WHERE tenant_id = $1::uuid AND execution_id = $2::uuid`, tenant, execID).
		Scan(&ex.ExecutionID, &ex.Kind, &ex.Status, &ex.ProposalHash, &ex.LedgerWatermark, &jev, &ex.CreatedBy, &ex.CreatedAt, &ex.CommittedAt, &calc); err != nil {
		return nil, kernelErr(err)
	}
	if jev != nil {
		ex.OutboxEventID = *jev
	}
	ex.Calculation = json.RawMessage(calc)
	tp.Execution = &ex

	if err := traceJournalChain(ctx, t, tenant, jid, tp); err != nil {
		return nil, err
	}

	rows, err := t.Query(ctx, `SELECT execution_id::text, status, COALESCE(failure_reason, ''), created_at FROM posting_executions
		WHERE tenant_id = $1::uuid AND accounting_event_id = $2::uuid AND accounting_book_id = $3::uuid AND status IN ('FAILED', 'QUARANTINED', 'RESOLVED') ORDER BY created_at`,
		tenant, p.eventID, p.bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f kernel.TraceFailure
		if err := rows.Scan(&f.ExecutionID, &f.Status, &f.Reason, &f.At); err != nil {
			return nil, err
		}
		tp.Failures = append(tp.Failures, f)
	}
	return tp, rows.Err()
}

// traceJournalChain loads a journal and then everything that reverses or corrects it, in order, with the relations between them.
func traceJournalChain(ctx context.Context, t pgx.Tx, tenant, first string, tp *kernel.TracePosting) error {
	queue := []struct{ id, role string }{{first, "POSTING"}}
	seen := map[string]bool{}
	for len(queue) > 0 && len(seen) < 50 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.id] {
			continue
		}
		seen[cur.id] = true
		j, err := loadTraceJournal(ctx, t, tenant, cur.id, cur.role)
		if err != nil {
			return err
		}
		tp.Journals = append(tp.Journals, *j)
		rows, err := t.Query(ctx, `SELECT journal_id::text, reversal_of_journal_id IS NOT NULL FROM journal_headers
			WHERE tenant_id = $1::uuid AND (reversal_of_journal_id = $2::uuid OR correction_of_journal_id = $2::uuid) ORDER BY created_at`, tenant, cur.id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var isReversal bool
			if err := rows.Scan(&id, &isReversal); err != nil {
				rows.Close()
				return err
			}
			role, kind := "CORRECTION", "CORRECTS"
			if isReversal {
				role, kind = "REVERSAL", "REVERSES"
			}
			tp.Relations = append(tp.Relations, kernel.TraceRelation{Kind: kind, From: id, To: cur.id})
			queue = append(queue, struct{ id, role string }{id, role})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func loadTraceJournal(ctx context.Context, t pgx.Tx, tenant, id, role string) (*kernel.TraceJournal, error) {
	j := kernel.TraceJournal{Role: role, Lines: []kernel.TraceLine{}}
	var number, hash, postedBy, revOf, corrOf, reason, kind *string
	var postingDate time.Time
	if err := t.QueryRow(ctx, `SELECT journal_id::text, journal_number, status, journal_type, posting_date, fiscal_period, currency_code, journal_hash,
			posted_by_principal_id, reversal_of_journal_id::text, correction_of_journal_id::text, reversal_reason, correction_kind
		FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, tenant, id).
		Scan(&j.JournalID, &number, &j.Status, &j.JournalType, &postingDate, &j.FiscalPeriod, &j.Currency, &hash, &postedBy, &revOf, &corrOf, &reason, &kind); err != nil {
		return nil, kernelErr(err)
	}
	j.PostingDate = postingDate.Format("2006-01-02")
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	j.JournalNumber, j.Hash, j.PostedBy, j.ReversalOf, j.CorrectionOf, j.Reason, j.CorrectionKnd =
		deref(number), deref(hash), deref(postedBy), deref(revOf), deref(corrOf), deref(reason), deref(kind)

	rows, err := t.Query(ctx, `SELECT l.line_number, l.account_code, l.debit_amount::text, l.credit_amount::text, l.transaction_amount::text, l.transaction_currency,
			l.exchange_rate::text, l.rate_ref, l.posting_rule_line_ref, l.dimensions, COALESCE(l.description, ''), e.entry_seq
		FROM journal_lines l LEFT JOIN ledger_entries e ON e.tenant_id = l.tenant_id AND e.journal_id = l.journal_id AND e.journal_line_id = l.journal_line_id
		WHERE l.tenant_id = $1::uuid AND l.journal_id = $2::uuid ORDER BY l.line_number`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l kernel.TraceLine
		var txAmt, txCcy, rate, rateRef, ruleRef *string
		var dims []byte
		if err := rows.Scan(&l.LineNo, &l.AccountCode, &l.Debit, &l.Credit, &txAmt, &txCcy, &rate, &rateRef, &ruleRef, &dims, &l.Description, &l.LedgerEntrySeq); err != nil {
			return nil, err
		}
		l.TransactionAmt, l.TransactionCcy, l.ExchangeRate, l.RateRef, l.RuleLineRef = deref(txAmt), deref(txCcy), deref(rate), deref(rateRef), deref(ruleRef)
		if len(dims) > 0 {
			_ = json.Unmarshal(dims, &l.Dimensions)
		}
		j.Lines = append(j.Lines, l)
	}
	return &j, rows.Err()
}

func getEventTx(ctx context.Context, t pgx.Tx, tenant, id string) (*kernel.AccountingEvent, error) {
	var e kernel.AccountingEvent
	if err := scanEvent(t.QueryRow(ctx, `SELECT `+eventCols+` FROM accounting_events WHERE tenant_id=$1::uuid AND accounting_event_id=$2::uuid`, tenant, id), &e); err != nil {
		return nil, kernelErr(err)
	}
	return &e, nil
}
