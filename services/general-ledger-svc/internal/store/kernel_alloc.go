package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.AllocRepo = (*KernelRepo)(nil)

const allocRuleCols = `allocation_rule_id::text, rule_code, version, status, content, content_hash, effective_from::text, created_by, created_at, COALESCE(approved_by, ''), approved_at,
	COALESCE(retired_by, ''), retired_at`

func scanAllocRule(row pgx.Row, r *kernel.AllocRule) error {
	var content []byte
	if err := row.Scan(&r.RuleID, &r.RuleCode, &r.Version, &r.Status, &content, &r.ContentHash, &r.EffectiveFrom, &r.CreatedBy, &r.CreatedAt, &r.ApprovedBy, &r.ApprovedAt, &r.RetiredBy, &r.RetiredAt); err != nil {
		return err
	}
	return json.Unmarshal(content, &r.Content)
}

// InsertAllocRule implements kernel.AllocRepo: the next version of the code, as a draft.
func (r *KernelRepo) InsertAllocRule(ctx context.Context, tenant string, x *kernel.AllocRule, out kernel.OutEvent) (*kernel.AllocRule, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "allocrule:"+tenant+":"+x.RuleCode); e != nil {
			return e
		}
		if e := t.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM allocation_rules WHERE tenant_id = $1::uuid AND rule_code = $2`, tenant, x.RuleCode).Scan(&x.Version); e != nil {
			return e
		}
		content, _ := json.Marshal(x.Content)
		if _, e := t.Exec(ctx, `INSERT INTO allocation_rules (allocation_rule_id, tenant_id, rule_code, version, status, content, content_hash, effective_from, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, 'DRAFT', $5::jsonb, $6, $7::date, $8, $9)`, x.RuleID, tenant, x.RuleCode, x.Version, string(content), x.ContentHash, x.EffectiveFrom, x.CreatedBy, x.CreatedAt); e != nil {
			return e
		}
		out.Data["version"] = x.Version
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return x, nil
}

// GetAllocRule implements kernel.AllocRepo.
func (r *KernelRepo) GetAllocRule(ctx context.Context, tenant, id string) (*kernel.AllocRule, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var x kernel.AllocRule
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanAllocRule(t.QueryRow(ctx, `SELECT `+allocRuleCols+` FROM allocation_rules WHERE tenant_id = $1::uuid AND allocation_rule_id = $2::uuid`, tenant, id), &x)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &x, nil
}

// ListAllocRules implements kernel.AllocRepo.
func (r *KernelRepo) ListAllocRules(ctx context.Context, tenant, code string) ([]kernel.AllocRule, error) {
	out := []kernel.AllocRule{}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+allocRuleCols+` FROM allocation_rules WHERE tenant_id = $1::uuid AND ($2 = '' OR rule_code = $2) ORDER BY rule_code, version DESC LIMIT 200`, tenant, code)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var x kernel.AllocRule
			if e := scanAllocRule(rows, &x); e != nil {
				return e
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// ApproveAllocRule implements kernel.AllocRepo.
func (r *KernelRepo) ApproveAllocRule(ctx context.Context, tenant, id, actor string, at time.Time, out kernel.OutEvent) (*kernel.AllocRule, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var code, status string
		if e := t.QueryRow(ctx, `SELECT rule_code, status FROM allocation_rules WHERE tenant_id = $1::uuid AND allocation_rule_id = $2::uuid FOR UPDATE`, tenant, id).Scan(&code, &status); e != nil {
			return e
		}
		if status != "DRAFT" {
			return kernel.ErrState
		}
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "allocrule:"+tenant+":"+code); e != nil {
			return e
		}
		if _, e := t.Exec(ctx, `UPDATE allocation_rules SET status = 'RETIRED', retired_by = $3, retired_at = $4 WHERE tenant_id = $1::uuid AND rule_code = $2 AND status = 'ACTIVE'`, tenant, code, actor, at); e != nil {
			return e
		}
		if _, e := t.Exec(ctx, `UPDATE allocation_rules SET status = 'ACTIVE', approved_by = $3, approved_at = $4 WHERE tenant_id = $1::uuid AND allocation_rule_id = $2::uuid`, tenant, id, actor, at); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return r.GetAllocRule(ctx, tenant, id)
}

// RetireAllocRule implements kernel.AllocRepo.
func (r *KernelRepo) RetireAllocRule(ctx context.Context, tenant, id, actor string, at time.Time, out kernel.OutEvent) (*kernel.AllocRule, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var status string
		if e := t.QueryRow(ctx, `SELECT status FROM allocation_rules WHERE tenant_id = $1::uuid AND allocation_rule_id = $2::uuid FOR UPDATE`, tenant, id).Scan(&status); e != nil {
			return e
		}
		if status == "RETIRED" {
			return kernel.ErrState
		}
		if _, e := t.Exec(ctx, `UPDATE allocation_rules SET status = 'RETIRED', retired_by = $3, retired_at = $4 WHERE tenant_id = $1::uuid AND allocation_rule_id = $2::uuid`, tenant, id, actor, at); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, out)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return r.GetAllocRule(ctx, tenant, id)
}

const poolWhere = `tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND account_code = ANY($4) AND posting_date >= $5::date AND posting_date <= $6::date
	AND ($7::jsonb = '{}'::jsonb OR dimensions @> $7::jsonb)`

func dimsArg(d map[string]string) string {
	if len(d) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(d)
	return string(b)
}

// PoolBalances implements kernel.AllocRepo.
func (r *KernelRepo) PoolBalances(ctx context.Context, tenant, entity, bookCode string, accounts []string, dims map[string]string, from, to time.Time) ([]kernel.PoolLine, int64, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return nil, 0, kernel.ErrNotFound
	}
	t, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return nil, 0, err
	}
	var wm int64
	if err := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, tenant, entity).Scan(&wm); err != nil {
		return nil, 0, kernelErr(err)
	}
	rows, err := t.Query(ctx, `SELECT account_code, (sum(debit_amount) - sum(credit_amount))::text FROM ledger_entries WHERE `+poolWhere+` AND entry_seq <= $8
		GROUP BY account_code HAVING sum(debit_amount) <> sum(credit_amount) ORDER BY account_code`, tenant, entity, bookCode, accounts, from, to, dimsArg(dims), wm)
	if err != nil {
		return nil, 0, kernelErr(err)
	}
	defer rows.Close()
	out := []kernel.PoolLine{}
	for rows.Next() {
		var l kernel.PoolLine
		if err := rows.Scan(&l.Account, &l.Net); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, wm, kernelErr(rows.Err())
}

const allocRunCols = `r.allocation_run_id::text, r.allocation_rule_id::text, r.legal_entity_id::text, r.accounting_book_id::text, r.fiscal_period, r.status, r.ledger_watermark, r.pool_amount::text,
	r.population, COALESCE(r.journal_id::text, ''), COALESCE(j.journal_number, ''), COALESCE(r.reversal_journal_id::text, ''), COALESCE(rj.journal_number, ''), r.created_by, r.created_at,
	COALESCE(r.reversed_by, ''), r.reversed_at, COALESCE(r.reversal_reason, ''), r.remote_journals, r.remote_reversals`

const allocRunFrom = ` FROM allocation_runs r LEFT JOIN journal_headers j ON j.journal_id = r.journal_id LEFT JOIN journal_headers rj ON rj.journal_id = r.reversal_journal_id`

func scanAllocRun(row pgx.Row, v *kernel.AllocRun) error {
	var pop, remote, reversals []byte
	if err := row.Scan(&v.RunID, &v.RuleID, &v.LegalEntityID, &v.BookID, &v.FiscalPeriod, &v.Status, &v.LedgerWatermark, &v.PoolAmount, &pop, &v.JournalID, &v.JournalNumber,
		&v.ReversalJournalID, &v.ReversalNumber, &v.CreatedBy, &v.CreatedAt, &v.ReversedBy, &v.ReversedAt, &v.ReversalReason, &remote, &reversals); err != nil {
		return err
	}
	if err := json.Unmarshal(remote, &v.RemoteJournals); err != nil {
		return err
	}
	var rev map[string]string
	if err := json.Unmarshal(reversals, &rev); err != nil {
		return err
	}
	for i := range v.RemoteJournals {
		v.RemoteJournals[i].ReversalJournalID = rev[v.RemoteJournals[i].JournalID]
	}
	return json.Unmarshal(pop, &v.Population)
}

// GetAllocRun implements kernel.AllocRepo.
func (r *KernelRepo) GetAllocRun(ctx context.Context, tenant, id string) (*kernel.AllocRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var v kernel.AllocRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanAllocRun(t.QueryRow(ctx, `SELECT `+allocRunCols+allocRunFrom+` WHERE r.tenant_id = $1::uuid AND r.allocation_run_id = $2::uuid`, tenant, id), &v)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &v, nil
}

// ListAllocRuns implements kernel.AllocRepo.
func (r *KernelRepo) ListAllocRuns(ctx context.Context, tenant, entity, period string) ([]kernel.AllocRun, error) {
	out := []kernel.AllocRun{}
	if entity != "" {
		if _, err := uuid.Parse(entity); err != nil {
			return out, nil
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+allocRunCols+allocRunFrom+` WHERE r.tenant_id = $1::uuid AND ($2 = '' OR r.legal_entity_id::text = $2) AND ($3 = '' OR r.fiscal_period = $3)
			ORDER BY r.created_at DESC LIMIT 200`, tenant, entity, period)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v kernel.AllocRun
			if e := scanAllocRun(rows, &v); e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// LiveAllocRun implements kernel.AllocRepo.
func (r *KernelRepo) LiveAllocRun(ctx context.Context, tenant, ruleID, bookID, period string) (*kernel.AllocRun, error) {
	var v kernel.AllocRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanAllocRun(t.QueryRow(ctx, `SELECT `+allocRunCols+allocRunFrom+` WHERE r.tenant_id = $1::uuid AND r.allocation_rule_id = $2::uuid AND r.accounting_book_id = $3::uuid
			AND r.fiscal_period = $4 AND r.status = 'POSTED'`, tenant, ruleID, bookID, period), &v)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &v, nil
}

func allocSysLines(lines []kernel.AllocLine) []SysLine {
	out := make([]SysLine, 0, len(lines))
	for _, l := range lines {
		n, _ := new(big.Rat).SetString(l.Amount)
		sl := SysLine{Account: l.Account, Debit: "0", Credit: "0", Description: l.Description, RuleLineRef: l.Ref, Dimensions: l.Dimensions, Counterparty: l.Counterparty}
		if n.Sign() > 0 {
			sl.Debit = n.FloatString(2)
		} else {
			sl.Credit = new(big.Rat).Neg(n).FloatString(2)
		}
		out = append(out, sl)
	}
	return out
}

// WriteAllocation implements kernel.AllocRepo.
func (r *KernelRepo) WriteAllocation(ctx context.Context, p *kernel.AllocPlan) (*kernel.AllocRun, error) {
	run := p.Run
	var out *kernel.AllocRun
	err := r.tx(ctx, p.Tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "alloc:"+p.Tenant+":"+run.RuleID+":"+run.BookID+":"+run.FiscalPeriod); e != nil {
			return e
		}
		var existing kernel.AllocRun
		switch e := scanAllocRun(t.QueryRow(ctx, `SELECT `+allocRunCols+allocRunFrom+` WHERE r.tenant_id = $1::uuid AND r.allocation_rule_id = $2::uuid AND r.accounting_book_id = $3::uuid
			AND r.fiscal_period = $4 AND r.status = 'POSTED'`, p.Tenant, run.RuleID, run.BookID, run.FiscalPeriod), &existing); {
		case e == nil:
			existing.Existing = true
			out = &existing
			return nil
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		var late int
		if e := t.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE `+poolWhere+` AND entry_seq > $8`, p.Tenant, run.LegalEntityID, p.Book.BookCode, p.SourceAccts, p.PeriodStart, p.PostingDate,
			dimsArg(p.SourceDims), run.LedgerWatermark).Scan(&late); e != nil {
			return e
		}
		if late > 0 {
			return &kernel.Exception{Family: kernel.FamConc, Code: "POPULATION_CHANGED", Detail: fmt.Sprintf("%d entries were posted into the pool after it was read; run the allocation again", late)}
		}
		trace, _ := json.Marshal(map[string]any{"allocation_run_id": run.RunID, "rule_code": run.Population.RuleCode, "rule_version": run.Population.RuleVersion, "rule_hash": run.Population.RuleHash,
			"ledger_watermark": run.LedgerWatermark, "driver": run.Population.DriverName, "driver_ref": run.Population.DriverRef})
		posted, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: run.LegalEntityID, Period: run.FiscalPeriod, BookID: p.Book.BookID, BookCode: p.Book.BookCode, Type: domain.JournalTypeAllocation,
			Purpose: "ALLOCATION", Description: fmt.Sprintf("Allocation %s v%d for %s", run.Population.RuleCode, run.Population.RuleVersion, run.FiscalPeriod), Correlation: "alloc:" + run.RunID,
			PostingDate: p.PostingDate, Currency: p.Book.FunctionalCurrency, Actor: p.Actor, At: run.CreatedAt, ExecutionKind: "KERNEL_ALLOC", Trace: string(trace), Lines: allocSysLines(p.Lines)})
		if e != nil {
			return e
		}
		run.JournalID, run.JournalNumber = posted.JournalID, posted.Number
		// a cross-entity allocation posts the cost and the intercompany payable in each recipient entity's own book, in the same transaction
		run.RemoteJournals = []kernel.AllocRemoteJournal{}
		for _, rm := range p.Remote {
			rtrace, _ := json.Marshal(map[string]any{"allocation_run_id": run.RunID, "source_entity": run.LegalEntityID, "source_journal_id": run.JournalID, "rule_code": run.Population.RuleCode,
				"rule_version": run.Population.RuleVersion, "rule_hash": run.Population.RuleHash})
			rp, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: rm.Book.LegalEntityID, Period: run.FiscalPeriod, BookID: rm.Book.BookID, BookCode: rm.Book.BookCode, Type: domain.JournalTypeAllocation,
				Purpose: "ALLOCATION", Description: fmt.Sprintf("Allocation %s v%d from %s for %s", run.Population.RuleCode, run.Population.RuleVersion, run.LegalEntityID, run.FiscalPeriod),
				Correlation: "alloc:" + run.RunID + ":" + rm.Book.BookID, PostingDate: p.PostingDate, Currency: rm.Book.FunctionalCurrency, Actor: p.Actor, At: run.CreatedAt,
				ExecutionKind: "KERNEL_ALLOC", Trace: string(rtrace), Lines: allocSysLines(rm.Lines)})
			if e != nil {
				return e
			}
			run.RemoteJournals = append(run.RemoteJournals, kernel.AllocRemoteJournal{LegalEntityID: rm.Book.LegalEntityID, BookID: rm.Book.BookID, BookCode: rm.Book.BookCode,
				Currency: rm.Book.FunctionalCurrency, JournalID: rp.JournalID, JournalNumber: rp.Number})
		}
		pop, _ := json.Marshal(run.Population)
		remote, _ := json.Marshal(run.RemoteJournals)
		if _, e := t.Exec(ctx, `INSERT INTO allocation_runs (allocation_run_id, tenant_id, allocation_rule_id, legal_entity_id, accounting_book_id, fiscal_period, status, ledger_watermark, pool_amount, population,
				journal_id, created_by, created_at, remote_journals)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid, $6, 'POSTED', $7, $8::numeric, $9::jsonb, $10::uuid, $11, $12, $13::jsonb)`,
			run.RunID, p.Tenant, run.RuleID, run.LegalEntityID, run.BookID, run.FiscalPeriod, run.LedgerWatermark, run.PoolAmount, string(pop), run.JournalID, run.CreatedBy, run.CreatedAt, string(remote)); e != nil {
			return e
		}
		if e := r.enqueue(ctx, t, p.Tenant, kernel.OutEvent{Fact: "allocation-run.posted", AggregateType: "allocation-run", AggregateID: run.RunID, Actor: run.CreatedBy, CorrelationID: p.Correlation,
			LegalEntityID: run.LegalEntityID, Data: map[string]any{"allocation_run_id": run.RunID, "allocation_rule_id": run.RuleID, "book_id": run.BookID, "fiscal_period": run.FiscalPeriod,
				"pool_amount": run.PoolAmount, "journal_id": run.JournalID, "rounding_residual": run.Population.Residual}}); e != nil {
			return e
		}
		out = &run
		return nil
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return out, nil
}

// ReverseAllocation implements kernel.AllocRepo. The source journal and every journal the run posted in a recipient entity's book are
// reversed together, or none is.
func (r *KernelRepo) ReverseAllocation(ctx context.Context, tenant, id, actor, reason string, at, postingDate time.Time, book kernel.Book) (*kernel.AllocRun, error) {
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var status, jid, entity, period, ccy string
		var remoteRaw []byte
		if e := t.QueryRow(ctx, `SELECT status, journal_id::text, legal_entity_id::text, fiscal_period, population->>'currency', remote_journals FROM allocation_runs WHERE tenant_id = $1::uuid AND allocation_run_id = $2::uuid FOR UPDATE`,
			tenant, id).Scan(&status, &jid, &entity, &period, &ccy, &remoteRaw); e != nil {
			return e
		}
		if status != "POSTED" {
			return kernel.ErrState
		}
		var remote []kernel.AllocRemoteJournal
		if e := json.Unmarshal(remoteRaw, &remote); e != nil {
			return e
		}
		// reverseOne posts the mirror of one journal in the book it was posted in
		reverseOne := func(journalID, ent, bookID, bookCode, currency, corr string) (*SysPosted, error) {
			rows, e := t.Query(ctx, `SELECT account_code, debit_amount::text, credit_amount::text, dimensions, COALESCE(counterparty_id, '') FROM journal_lines WHERE tenant_id = $1::uuid AND journal_id = $2::uuid ORDER BY line_number`, tenant, journalID)
			if e != nil {
				return nil, e
			}
			var back []SysLine
			for rows.Next() {
				var l SysLine
				var dr, cr string
				var dims domain.Dimensions
				if e := rows.Scan(&l.Account, &dr, &cr, &dims, &l.Counterparty); e != nil {
					rows.Close()
					return nil, e
				}
				l.Debit, l.Credit, l.Dimensions, l.Description, l.RuleLineRef = cr, dr, dims, "reversal: allocation withdrawn", "ALLOC_REVERSAL"
				back = append(back, l)
			}
			rows.Close()
			if e := rows.Err(); e != nil {
				return nil, e
			}
			trace, _ := json.Marshal(map[string]any{"allocation_run_id": id, "reverses": journalID, "reason": reason})
			return r.postSystemJournal(ctx, t, &SysJournal{Tenant: tenant, Entity: ent, Period: period, BookID: bookID, BookCode: bookCode, Type: domain.JournalTypeReversal, Purpose: "ALLOCATION",
				Description: "Reversal of allocation for " + period, Correlation: corr, PostingDate: postingDate, Currency: currency, Actor: actor, At: at, ReversalOf: journalID, ReversalReason: reason,
				ExecutionKind: "KERNEL_ALLOC_REV", Trace: string(trace), Lines: back})
		}
		rev, e := reverseOne(jid, entity, book.BookID, book.BookCode, ccy, "alloc-rev:"+id)
		if e != nil {
			return e
		}
		reversals := map[string]string{}
		for _, rj := range remote {
			rr, e := reverseOne(rj.JournalID, rj.LegalEntityID, rj.BookID, rj.BookCode, rj.Currency, "alloc-rev:"+id+":"+rj.BookID)
			if e != nil {
				return e
			}
			reversals[rj.JournalID] = rr.JournalID
		}
		revJSON, _ := json.Marshal(reversals)
		if _, e := t.Exec(ctx, `UPDATE allocation_runs SET status = 'REVERSED', reversal_journal_id = $3::uuid, reversed_by = $4, reversed_at = $5, reversal_reason = $6, remote_reversals = $7::jsonb WHERE tenant_id = $1::uuid AND allocation_run_id = $2::uuid`,
			tenant, id, rev.JournalID, actor, at, reason, string(revJSON)); e != nil {
			return e
		}
		return r.enqueue(ctx, t, tenant, kernel.OutEvent{Fact: "allocation-run.reversed", AggregateType: "allocation-run", AggregateID: id, Actor: actor, LegalEntityID: entity,
			Data: map[string]any{"allocation_run_id": id, "reversal_journal_id": rev.JournalID, "reason": reason, "recipient_journals_reversed": len(remote)}})
	})
	if err != nil {
		if _, ok := kernel.AsException(err); ok {
			return nil, err
		}
		return nil, kernelErr(err)
	}
	return r.GetAllocRun(ctx, tenant, id)
}
