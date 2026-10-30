package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.MigrationRepo = (*KernelRepo)(nil)

const migCols = `b.batch_id::text, b.legal_entity_id::text, b.accounting_book_id::text, b.batch_code, b.batch_kind, COALESCE(b.corrects_batch_id::text, ''), b.cutover_date::text, b.currency,
	b.source_system, b.source_extract_ref, b.source_extract_hash, b.mapping_ref, b.source_totals, b.control_totals, b.line_count, b.total_debit::text, b.total_credit::text, b.lines_hash,
	b.status, b.validation_report, b.created_by, b.created_at, COALESCE(b.validated_by, ''), b.validated_at, COALESCE(b.approved_by, ''), b.approved_at, COALESCE(b.authority_ref, ''),
	b.window_start, b.window_end, COALESCE(b.journal_id::text, ''), COALESCE(j.journal_number, ''), COALESCE(b.posted_by, ''), b.posted_at, b.reconciliation, COALESCE(b.reconciled_by, ''),
	b.reconciled_at, COALESCE(b.evidence_hash, ''), COALESCE(b.cancelled_by, ''), b.cancelled_at, COALESCE(b.cancel_reason, '')`

const migFrom = ` FROM migration_batches b LEFT JOIN journal_headers j ON j.journal_id = b.journal_id`

func scanMigration(row pgx.Row, b *kernel.MigrationBatch) error {
	var totals, controls, report, recon []byte
	if err := row.Scan(&b.BatchID, &b.LegalEntityID, &b.BookID, &b.BatchCode, &b.Kind, &b.CorrectsBatchID, &b.CutoverDate, &b.Currency, &b.SourceSystem, &b.SourceExtractRef,
		&b.SourceExtractHash, &b.MappingRef, &totals, &controls, &b.LineCount, &b.TotalDebit, &b.TotalCredit, &b.LinesHash, &b.Status, &report, &b.CreatedBy, &b.CreatedAt,
		&b.ValidatedBy, &b.ValidatedAt, &b.ApprovedBy, &b.ApprovedAt, &b.AuthorityRef, &b.WindowStart, &b.WindowEnd, &b.JournalID, &b.JournalNumber, &b.PostedBy, &b.PostedAt,
		&recon, &b.ReconciledBy, &b.ReconciledAt, &b.EvidenceHash, &b.CancelledBy, &b.CancelledAt, &b.CancelReason); err != nil {
		return err
	}
	if err := json.Unmarshal(totals, &b.SourceTotals); err != nil {
		return err
	}
	if err := json.Unmarshal(controls, &b.ControlTotals); err != nil {
		return err
	}
	if len(report) > 0 {
		b.Report = &kernel.MigrationReport{}
		if err := json.Unmarshal(report, b.Report); err != nil {
			return err
		}
	}
	if len(recon) > 0 {
		b.Reconciliation = &kernel.MigrationReconciliation{}
		if err := json.Unmarshal(recon, b.Reconciliation); err != nil {
			return err
		}
	}
	return nil
}

func (r *KernelRepo) migEvent(ctx context.Context, t pgx.Tx, tenant, batch, kind, actor string, at time.Time, detail map[string]any) error {
	d, _ := json.Marshal(detail)
	_, err := t.Exec(ctx, `INSERT INTO migration_batch_events (event_id, tenant_id, batch_id, kind, actor, at, detail) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7::jsonb)`,
		uuid.NewString(), tenant, batch, kind, actor, at, string(d))
	return err
}

// lockMigration reads the batch with its row locked.
func lockMigration(ctx context.Context, t pgx.Tx, tenant, id string) (*kernel.MigrationBatch, error) {
	var b kernel.MigrationBatch
	if err := scanMigration(t.QueryRow(ctx, `SELECT `+migCols+migFrom+` WHERE b.tenant_id = $1::uuid AND b.batch_id = $2::uuid FOR UPDATE OF b`, tenant, id), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// InsertMigrationBatch implements kernel.MigrationRepo.
func (r *KernelRepo) InsertMigrationBatch(ctx context.Context, tenant string, b *kernel.MigrationBatch, lines []kernel.MigrationLine, out kernel.OutEvent) (*kernel.MigrationBatch, bool, error) {
	var stored *kernel.MigrationBatch
	existing := false
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "migbatch:"+tenant+":"+b.LegalEntityID+":"+b.BookID+":"+b.BatchCode); e != nil {
			return e
		}
		var prior kernel.MigrationBatch
		switch e := scanMigration(t.QueryRow(ctx, `SELECT `+migCols+migFrom+` WHERE b.tenant_id = $1::uuid AND b.legal_entity_id = $2::uuid AND b.accounting_book_id = $3::uuid AND b.batch_code = $4`,
			tenant, b.LegalEntityID, b.BookID, b.BatchCode), &prior); {
		case e == nil:
			// the same batch again is the same batch; a different extract under the same code is a conflict, never a quiet replacement
			if prior.LinesHash != b.LinesHash || prior.SourceExtractHash != b.SourceExtractHash || prior.CutoverDate != b.CutoverDate || prior.Kind != b.Kind {
				return kernel.ErrExists
			}
			stored, existing = &prior, true
			return nil
		case e != pgx.ErrNoRows:
			return e
		}
		src, _ := json.Marshal(b.SourceTotals)
		ctl, _ := json.Marshal(b.ControlTotals)
		if _, e := t.Exec(ctx, `INSERT INTO migration_batches (batch_id, tenant_id, legal_entity_id, accounting_book_id, batch_code, batch_kind, corrects_batch_id, cutover_date, currency, source_system,
				source_extract_ref, source_extract_hash, mapping_ref, source_totals, control_totals, line_count, total_debit, total_credit, lines_hash, status, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, NULLIF($7::text, '')::uuid, $8::date, $9, $10, $11, $12, $13, $14::jsonb, $15::jsonb, $16, $17::numeric, $18::numeric, $19, 'STAGED', $20, $21)`,
			b.BatchID, tenant, b.LegalEntityID, b.BookID, b.BatchCode, b.Kind, b.CorrectsBatchID, b.CutoverDate, b.Currency, b.SourceSystem, b.SourceExtractRef, b.SourceExtractHash, b.MappingRef,
			string(src), string(ctl), b.LineCount, b.TotalDebit, b.TotalCredit, b.LinesHash, b.CreatedBy, b.CreatedAt); e != nil {
			return e
		}
		for from := 0; from < len(lines); from += 5000 {
			to := from + 5000
			if to > len(lines) {
				to = len(lines)
			}
			n := to - from
			no, ref, sacct, acct, dr, cr, cp, txa, txc, rate, dims, desc := make([]int32, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n),
				make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
			for i, l := range lines[from:to] {
				no[i], ref[i], sacct[i], acct[i], dr[i], cr[i] = int32(l.LineNo), l.SourceRef, l.SourceAccount, l.AccountCode, l.Debit, l.Credit
				cp[i], txa[i], txc[i], rate[i], desc[i] = l.CounterpartyID, l.TransactionAmount, l.TransactionCurrency, l.ExchangeRate, l.Description
				if len(l.Dimensions) > 0 {
					d, _ := json.Marshal(l.Dimensions)
					dims[i] = string(d)
				}
			}
			if _, e := t.Exec(ctx, `INSERT INTO migration_batch_lines (tenant_id, batch_id, line_no, source_ref, source_account, account_code, debit_amount, credit_amount, counterparty_id,
					transaction_amount, transaction_currency, exchange_rate, dimensions, description)
				SELECT $1::uuid, $2::uuid, u.line_no, u.source_ref, u.source_account, u.account_code, u.debit::numeric, u.credit::numeric, NULLIF(u.cp, ''), NULLIF(u.txa, '')::numeric,
					NULLIF(u.txc, ''), NULLIF(u.rate, '')::numeric, NULLIF(u.dims, '')::jsonb, u.descr
				FROM unnest($3::int[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::text[])
					AS u(line_no, source_ref, source_account, account_code, debit, credit, cp, txa, txc, rate, dims, descr)`,
				tenant, b.BatchID, no, ref, sacct, acct, dr, cr, cp, txa, txc, rate, dims, desc); e != nil {
				return e
			}
		}
		if e := r.migEvent(ctx, t, tenant, b.BatchID, "STAGED", b.CreatedBy, b.CreatedAt, map[string]any{"line_count": b.LineCount, "lines_hash": b.LinesHash, "source_extract_hash": b.SourceExtractHash}); e != nil {
			return e
		}
		if e := r.enqueue(ctx, t, tenant, out); e != nil {
			return e
		}
		if e := scanMigration(t.QueryRow(ctx, `SELECT `+migCols+migFrom+` WHERE b.tenant_id = $1::uuid AND b.batch_id = $2::uuid`, tenant, b.BatchID), &prior); e != nil {
			return e
		}
		stored = &prior
		return nil
	})
	if err != nil {
		return nil, false, kernelErr(err)
	}
	return stored, existing, nil
}

// GetMigrationBatch implements kernel.MigrationRepo.
func (r *KernelRepo) GetMigrationBatch(ctx context.Context, tenant, id string) (*kernel.MigrationBatch, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var b kernel.MigrationBatch
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanMigration(t.QueryRow(ctx, `SELECT `+migCols+migFrom+` WHERE b.tenant_id = $1::uuid AND b.batch_id = $2::uuid`, tenant, id), &b)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &b, nil
}

// ListMigrationBatches implements kernel.MigrationRepo.
func (r *KernelRepo) ListMigrationBatches(ctx context.Context, tenant, entity, status string, limit int) ([]kernel.MigrationBatch, error) {
	out := []kernel.MigrationBatch{}
	if entity != "" {
		if _, err := uuid.Parse(entity); err != nil {
			return out, nil
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+migCols+migFrom+` WHERE b.tenant_id = $1::uuid AND ($2 = '' OR b.legal_entity_id::text = $2) AND ($3 = '' OR b.status = $3)
			ORDER BY b.created_at DESC LIMIT $4`, tenant, entity, strings.ToUpper(status), clampLimit(limit))
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var b kernel.MigrationBatch
			if e := scanMigration(rows, &b); e != nil {
				return e
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// MigrationLines implements kernel.MigrationRepo.
func (r *KernelRepo) MigrationLines(ctx context.Context, tenant, id string, offset, limit int) ([]kernel.MigrationLine, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var lim any
	if limit > 0 {
		lim = limit
	}
	out := []kernel.MigrationLine{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT line_no, source_ref, source_account, account_code, debit_amount::text, credit_amount::text, COALESCE(counterparty_id, ''), COALESCE(transaction_amount::text, ''),
				COALESCE(transaction_currency, ''), COALESCE(exchange_rate::text, ''), dimensions, description
			FROM migration_batch_lines WHERE tenant_id = $1::uuid AND batch_id = $2::uuid ORDER BY line_no OFFSET $3 LIMIT $4`, tenant, id, offset, lim)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var l kernel.MigrationLine
			var dims []byte
			if e := rows.Scan(&l.LineNo, &l.SourceRef, &l.SourceAccount, &l.AccountCode, &l.Debit, &l.Credit, &l.CounterpartyID, &l.TransactionAmount, &l.TransactionCurrency, &l.ExchangeRate, &dims, &l.Description); e != nil {
				return e
			}
			if len(dims) > 0 {
				_ = json.Unmarshal(dims, &l.Dimensions)
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// MigrationEvents implements kernel.MigrationRepo.
func (r *KernelRepo) MigrationEvents(ctx context.Context, tenant, id string) ([]kernel.MigrationEvent, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	out := []kernel.MigrationEvent{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT kind, actor, at, detail FROM migration_batch_events WHERE tenant_id = $1::uuid AND batch_id = $2::uuid ORDER BY at, event_id`, tenant, id)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var ev kernel.MigrationEvent
			var d []byte
			if e := rows.Scan(&ev.Kind, &ev.Actor, &ev.At, &d); e != nil {
				return e
			}
			_ = json.Unmarshal(d, &ev.Detail)
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// step runs one locked transition of a batch: the batch must be in one of the given statuses.
func (r *KernelRepo) migStep(ctx context.Context, tenant, id string, from []string, fn func(t pgx.Tx, b *kernel.MigrationBatch) error) (*kernel.MigrationBatch, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		b, e := lockMigration(ctx, t, tenant, id)
		if e != nil {
			return e
		}
		ok := false
		for _, s := range from {
			ok = ok || b.Status == s
		}
		if !ok {
			return kernel.ErrState
		}
		return fn(t, b)
	})
	if err != nil {
		if _, isExc := kernel.AsException(err); isExc {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return r.GetMigrationBatch(ctx, tenant, id)
}

// MarkMigrationValidated implements kernel.MigrationRepo.
func (r *KernelRepo) MarkMigrationValidated(ctx context.Context, tenant, id, actor string, report kernel.MigrationReport, at time.Time, out kernel.OutEvent) (*kernel.MigrationBatch, error) {
	return r.migStep(ctx, tenant, id, []string{kernel.MigStaged, kernel.MigValidated}, func(t pgx.Tx, b *kernel.MigrationBatch) error {
		rep, _ := json.Marshal(report)
		kind := "VALIDATION_FAILED"
		if report.Valid {
			kind = "VALIDATED"
			if _, e := t.Exec(ctx, `UPDATE migration_batches SET validation_report = $3::jsonb, status = 'VALIDATED', validated_by = $4, validated_at = $5 WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`,
				tenant, id, string(rep), actor, at); e != nil {
				return e
			}
		} else if _, e := t.Exec(ctx, `UPDATE migration_batches SET validation_report = $3::jsonb WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`, tenant, id, string(rep)); e != nil {
			return e
		}
		if e := r.migEvent(ctx, t, tenant, id, kind, actor, at, map[string]any{"issues": len(report.Issues)}); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

// ApproveMigration implements kernel.MigrationRepo.
func (r *KernelRepo) ApproveMigration(ctx context.Context, tenant, id, actor, authority string, ws, we, at time.Time, out kernel.OutEvent) (*kernel.MigrationBatch, error) {
	return r.migStep(ctx, tenant, id, []string{kernel.MigValidated}, func(t pgx.Tx, b *kernel.MigrationBatch) error {
		if _, e := t.Exec(ctx, `UPDATE migration_batches SET status = 'APPROVED', approved_by = $3, approved_at = $4, authority_ref = $5, window_start = $6, window_end = $7 WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`,
			tenant, id, actor, at, authority, ws, we); e != nil {
			return e
		}
		if e := r.migEvent(ctx, t, tenant, id, "APPROVED", actor, at, map[string]any{"authority_ref": authority, "window_start": ws, "window_end": we}); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

// CancelMigration implements kernel.MigrationRepo.
func (r *KernelRepo) CancelMigration(ctx context.Context, tenant, id, actor, reason string, at time.Time, out kernel.OutEvent) (*kernel.MigrationBatch, error) {
	return r.migStep(ctx, tenant, id, []string{kernel.MigStaged, kernel.MigValidated, kernel.MigApproved}, func(t pgx.Tx, b *kernel.MigrationBatch) error {
		if _, e := t.Exec(ctx, `UPDATE migration_batches SET status = 'CANCELLED', cancelled_by = $3, cancelled_at = $4, cancel_reason = $5 WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`,
			tenant, id, actor, at, reason); e != nil {
			return e
		}
		if e := r.migEvent(ctx, t, tenant, id, "CANCELLED", actor, at, map[string]any{"reason": reason}); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}

// PostMigration implements kernel.MigrationRepo.
func (r *KernelRepo) PostMigration(ctx context.Context, p *kernel.MigrationPost) (*kernel.MigrationBatch, error) {
	b := p.Batch
	posted := false
	err := r.tx(ctx, p.Tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "migpost:"+p.Tenant+":"+b.LegalEntityID+":"+b.BookID); e != nil {
			return e
		}
		cur, e := lockMigration(ctx, t, p.Tenant, b.BatchID)
		if e != nil {
			return e
		}
		if cur.Status == kernel.MigPosted || cur.Status == kernel.MigReconciled {
			return nil // a concurrent post won: this is the same batch
		}
		if cur.Status != kernel.MigApproved {
			return kernel.ErrState
		}
		cut, _ := time.Parse("2006-01-02", b.CutoverDate)
		sys := make([]SysLine, 0, len(p.Lines))
		for _, l := range p.Lines {
			sl := SysLine{Account: l.AccountCode, Debit: l.Debit, Credit: l.Credit, TxAmount: l.TransactionAmount, TxCurrency: l.TransactionCurrency, ExchangeRate: l.ExchangeRate,
				Counterparty: l.CounterpartyID, Description: l.Description, RuleLineRef: fmt.Sprintf("MIG_%d", l.LineNo), Dimensions: l.Dimensions}
			if l.TransactionCurrency != "" {
				sl.RateRef = "migration:" + b.BatchCode
			}
			sys = append(sys, sl)
		}
		purpose := "OPENING_BALANCE"
		if b.Kind == kernel.MigCorrection {
			purpose = "MIGRATION"
		}
		trace, _ := json.Marshal(map[string]any{"migration_batch_id": b.BatchID, "batch_code": b.BatchCode, "batch_kind": b.Kind, "source_system": b.SourceSystem, "source_extract_hash": b.SourceExtractHash,
			"lines_hash": b.LinesHash, "mapping_ref": b.MappingRef, "approved_by": cur.ApprovedBy, "authority_ref": cur.AuthorityRef, "window_start": cur.WindowStart, "window_end": cur.WindowEnd})
		sp, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: b.LegalEntityID, Period: kernel.FiscalPeriodOf(cut), BookID: p.Book.BookID, BookCode: p.Book.BookCode, Type: domain.JournalTypeOpening,
			Purpose: purpose, Description: fmt.Sprintf("Opening balances: migration batch %s (%s)", b.BatchCode, b.SourceSystem), Correlation: p.Correlation, PostingDate: cut, Currency: p.Book.FunctionalCurrency,
			Actor: p.Actor, At: p.At, ExecutionKind: "KERNEL_MIGRATION", Trace: string(trace), Lines: sys})
		if e != nil {
			return e
		}
		if _, e := t.Exec(ctx, `UPDATE migration_batches SET status = 'POSTED', journal_id = $3::uuid, posted_by = $4, posted_at = $5 WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`,
			p.Tenant, b.BatchID, sp.JournalID, p.Actor, p.At); e != nil {
			return e
		}
		if e := r.migEvent(ctx, t, p.Tenant, b.BatchID, "POSTED", p.Actor, p.At, map[string]any{"journal_id": sp.JournalID, "journal_number": sp.Number, "ledger_watermark": sp.Watermark}); e != nil {
			return e
		}
		posted = true
		return r.enqueue(ctx, t, p.Tenant, kernel.OutEvent{Fact: "migration-batch.posted", AggregateType: "migration-batch", AggregateID: b.BatchID, Actor: p.Actor, LegalEntityID: b.LegalEntityID,
			Data: map[string]any{"batch_id": b.BatchID, "batch_code": b.BatchCode, "batch_kind": b.Kind, "legal_entity_id": b.LegalEntityID, "book_id": b.BookID, "cutover_date": b.CutoverDate,
				"journal_id": sp.JournalID, "journal_number": sp.Number, "line_count": b.LineCount, "lines_hash": b.LinesHash}})
	})
	if err != nil {
		if _, isExc := kernel.AsException(err); isExc {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	out, err := r.GetMigrationBatch(ctx, p.Tenant, b.BatchID)
	if err == nil && !posted {
		out.Existing = true
	}
	return out, err
}

// MigrationLedger implements kernel.MigrationRepo.
func (r *KernelRepo) MigrationLedger(ctx context.Context, tenant, journalID string) (*kernel.MigrationLedgerSummary, error) {
	if _, err := uuid.Parse(journalID); err != nil {
		return nil, kernel.ErrNotFound
	}
	out := &kernel.MigrationLedgerSummary{ByControl: map[string]string{}}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := t.QueryRow(ctx, `SELECT COALESCE(sum(debit_amount), 0)::text, COALESCE(sum(credit_amount), 0)::text FROM ledger_entries WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`,
			tenant, journalID).Scan(&out.Debit, &out.Credit); e != nil {
			return e
		}
		if e := t.QueryRow(ctx, `SELECT COALESCE(journal_number, ''), COALESCE(journal_hash, '') FROM journal_headers WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`,
			tenant, journalID).Scan(&out.JournalNumber, &out.JournalHash); e != nil {
			return e
		}
		rows, e := t.Query(ctx, `SELECT control_type, (sum(debit_amount) - sum(credit_amount))::text FROM ledger_entries WHERE tenant_id = $1::uuid AND journal_id = $2::uuid AND control_type IS NOT NULL GROUP BY 1`,
			tenant, journalID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if e := rows.Scan(&k, &v); e != nil {
				return e
			}
			out.ByControl[k] = v
		}
		return rows.Err()
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return out, nil
}

// RecordMigrationReconciliation implements kernel.MigrationRepo. A match freezes the batch (RECONCILED, with its evidence hash); a break
// is recorded and the batch stays POSTED.
func (r *KernelRepo) RecordMigrationReconciliation(ctx context.Context, tenant, id, actor string, rec kernel.MigrationReconciliation, evidenceHash string, at time.Time, out kernel.OutEvent) (*kernel.MigrationBatch, error) {
	return r.migStep(ctx, tenant, id, []string{kernel.MigPosted}, func(t pgx.Tx, b *kernel.MigrationBatch) error {
		body, _ := json.Marshal(rec)
		if rec.Status == "MATCHED" {
			if _, e := t.Exec(ctx, `UPDATE migration_batches SET status = 'RECONCILED', reconciliation = $3::jsonb, reconciled_by = $4, reconciled_at = $5, evidence_hash = $6 WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`,
				tenant, id, string(body), actor, at, evidenceHash); e != nil {
				return e
			}
		} else if _, e := t.Exec(ctx, `UPDATE migration_batches SET reconciliation = $3::jsonb WHERE tenant_id = $1::uuid AND batch_id = $2::uuid`, tenant, id, string(body)); e != nil {
			return e
		}
		kind := "RECONCILED"
		if rec.Status != "MATCHED" {
			kind = "RECONCILIATION_BREAK"
		}
		if e := r.migEvent(ctx, t, tenant, id, kind, actor, at, map[string]any{"differences": len(rec.Differences), "evidence_hash": evidenceHash}); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
}
