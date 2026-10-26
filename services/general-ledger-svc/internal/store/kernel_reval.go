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

var _ kernel.RevalRepo = (*KernelRepo)(nil)

// monetaryFilter selects the ledger entries of monetary accounts held in a foreign currency: an account the chart flags as
// monetary, or one of a control type that holds money owed or held.
const monetaryFilter = `e.transaction_currency IS NOT NULL AND e.transaction_currency <> $4 AND (c.monetary OR c.control_type IN ('AR', 'AP', 'BANK', 'CLEARING', 'PAYROLL', 'INTERCOMPANY'))`

// ForeignPositions implements kernel.RevalRepo: the open foreign monetary positions of a book as of a date, read in one
// repeatable-read snapshot together with the ledger watermark of that snapshot.
func (r *KernelRepo) ForeignPositions(ctx context.Context, tenant, entity, bookCode, functional string, asOf time.Time) ([]kernel.ForeignPosition, int64, error) {
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
	rows, err := t.Query(ctx, `
		SELECT e.account_code, COALESCE(e.counterparty_id, ''), e.transaction_currency,
		       sum(CASE WHEN e.debit_amount > 0 THEN e.transaction_amount ELSE -e.transaction_amount END)::text,
		       (sum(e.debit_amount) - sum(e.credit_amount))::text
		FROM ledger_entries e JOIN chart_of_accounts c ON c.tenant_id = e.tenant_id AND c.account_code = e.account_code
		WHERE e.tenant_id = $1::uuid AND e.legal_entity_id = $2::uuid AND e.book_id = $3 AND e.posting_date <= $5::date AND e.entry_seq <= $6 AND `+monetaryFilter+`
		GROUP BY e.account_code, COALESCE(e.counterparty_id, ''), e.transaction_currency
		HAVING sum(CASE WHEN e.debit_amount > 0 THEN e.transaction_amount ELSE -e.transaction_amount END) <> 0 OR sum(e.debit_amount) <> sum(e.credit_amount)
		ORDER BY e.account_code, 2, 3`, tenant, entity, bookCode, functional, asOf, wm)
	if err != nil {
		return nil, 0, kernelErr(err)
	}
	defer rows.Close()
	out := []kernel.ForeignPosition{}
	for rows.Next() {
		var p kernel.ForeignPosition
		if err := rows.Scan(&p.AccountCode, &p.CounterpartyID, &p.Currency, &p.TransactionBalance, &p.CarryingFunctional); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, wm, kernelErr(rows.Err())
}

const revalCols = `r.revaluation_run_id::text, r.legal_entity_id::text, r.accounting_book_id::text, r.fiscal_period, r.run_type, r.valuation_date::text, r.policy_id, r.ledger_watermark,
	r.population, r.item_count, r.total_gain::text, r.total_loss::text, COALESCE(r.journal_id::text, ''), COALESCE(j.journal_number, ''), COALESCE(r.reversal_journal_id::text, ''),
	COALESCE(rj.journal_number, ''), r.created_by, r.created_at`

const revalFrom = ` FROM revaluation_runs r LEFT JOIN journal_headers j ON j.journal_id = r.journal_id LEFT JOIN journal_headers rj ON rj.journal_id = r.reversal_journal_id`

func scanReval(row pgx.Row, v *kernel.RevalRun) error {
	var pop []byte
	if err := row.Scan(&v.RunID, &v.LegalEntityID, &v.BookID, &v.FiscalPeriod, &v.RunType, &v.ValuationDate, &v.PolicyID, &v.LedgerWatermark, &pop, &v.ItemCount, &v.TotalGain, &v.TotalLoss,
		&v.JournalID, &v.JournalNumber, &v.ReversalJournalID, &v.ReversalNumber, &v.CreatedBy, &v.CreatedAt); err != nil {
		return err
	}
	return json.Unmarshal(pop, &v.Population)
}

// GetRevalRun implements kernel.RevalRepo.
func (r *KernelRepo) GetRevalRun(ctx context.Context, tenant, entity, bookID, period string) (*kernel.RevalRun, error) {
	var v kernel.RevalRun
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanReval(t.QueryRow(ctx, `SELECT `+revalCols+revalFrom+` WHERE r.tenant_id = $1::uuid AND r.legal_entity_id = $2::uuid AND r.accounting_book_id = $3::uuid AND r.fiscal_period = $4 AND r.run_type = 'UNREALIZED'`,
			tenant, entity, bookID, period), &v)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &v, nil
}

// ListRevalRuns implements kernel.RevalRepo.
func (r *KernelRepo) ListRevalRuns(ctx context.Context, tenant, entity string) ([]kernel.RevalRun, error) {
	out := []kernel.RevalRun{}
	if entity != "" {
		if _, err := uuid.Parse(entity); err != nil {
			return out, nil
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+revalCols+revalFrom+` WHERE r.tenant_id = $1::uuid AND ($2 = '' OR r.legal_entity_id::text = $2) ORDER BY r.created_at DESC LIMIT 200`, tenant, entity)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v kernel.RevalRun
			if e := scanReval(rows, &v); e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

// WriteRevaluation implements kernel.RevalRepo: the run, the revaluation journal and its reversal in one transaction.
func (r *KernelRepo) WriteRevaluation(ctx context.Context, p *kernel.RevalPlan) (*kernel.RevalRun, error) {
	var out *kernel.RevalRun
	err := r.tx(ctx, p.Tenant, func(t pgx.Tx) error {
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "reval:"+p.Tenant+":"+p.Entity+":"+p.BookID+":"+p.Period); e != nil {
			return e
		}
		var existing kernel.RevalRun
		switch e := scanReval(t.QueryRow(ctx, `SELECT `+revalCols+revalFrom+` WHERE r.tenant_id = $1::uuid AND r.legal_entity_id = $2::uuid AND r.accounting_book_id = $3::uuid AND r.fiscal_period = $4 AND r.run_type = 'UNREALIZED'`,
			p.Tenant, p.Entity, p.BookID, p.Period), &existing); {
		case e == nil:
			existing.Existing = true
			out = &existing
			return nil
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		// entries that belong in the population but were posted after the snapshot would make the run value an incomplete picture
		var late int
		if e := t.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e JOIN chart_of_accounts c ON c.tenant_id = e.tenant_id AND c.account_code = e.account_code
			WHERE e.tenant_id = $1::uuid AND e.legal_entity_id = $2::uuid AND e.book_id = $3 AND e.posting_date <= $5::date AND e.entry_seq > $6 AND `+monetaryFilter,
			p.Tenant, p.Entity, p.BookCode, p.Currency, p.ValuationDate, p.Watermark).Scan(&late); e != nil {
			return e
		}
		if late > 0 {
			return &kernel.Exception{Family: kernel.FamConc, Code: "POPULATION_CHANGED", Detail: fmt.Sprintf("%d entries were posted into the population after it was read; run the revaluation again", late)}
		}

		run := &kernel.RevalRun{RunID: uuid.NewString(), LegalEntityID: p.Entity, BookID: p.BookID, FiscalPeriod: p.Period, RunType: "UNREALIZED", ValuationDate: p.ValuationDate.Format("2006-01-02"),
			PolicyID: p.Policy, LedgerWatermark: p.Watermark, Population: p.Population, ItemCount: len(p.Population.Items), TotalGain: p.TotalGain, TotalLoss: p.TotalLoss, CreatedBy: p.Actor, CreatedAt: p.At}
		var lines, back []SysLine
		for _, it := range p.Population.Items {
			if it.Outcome == "UNCHANGED" {
				continue
			}
			d := new(big.Rat)
			d.SetString(it.Difference)
			abs := new(big.Rat).Abs(d).FloatString(2)
			l := SysLine{Account: it.AccountCode, TxAmount: "0", TxCurrency: it.Currency, ExchangeRate: it.Rate, RateRef: it.RateID, Counterparty: it.CounterpartyID,
				Description: fmt.Sprintf("revaluation %s at %s", it.Currency, it.Rate), RuleLineRef: "REVAL"}
			l.Debit, l.Credit = "0", "0"
			if d.Sign() > 0 {
				l.Debit = abs
			} else {
				l.Credit = abs
			}
			lines = append(lines, l)
		}
		if len(lines) > 0 {
			if g := new(big.Rat); true {
				g.SetString(p.TotalGain)
				if g.Sign() > 0 {
					lines = append(lines, SysLine{Account: p.GainAccount, Debit: "0", Credit: g.FloatString(2), Description: "unrealized exchange gain", RuleLineRef: "REVAL_GAIN"})
				}
			}
			if l := new(big.Rat); true {
				l.SetString(p.TotalLoss)
				if l.Sign() > 0 {
					lines = append(lines, SysLine{Account: p.LossAccount, Debit: l.FloatString(2), Credit: "0", Description: "unrealized exchange loss", RuleLineRef: "REVAL_LOSS"})
				}
			}
			trace, _ := json.Marshal(map[string]any{"revaluation_run_id": run.RunID, "policy_id": p.Policy, "ledger_watermark": p.Watermark, "valuation_date": run.ValuationDate, "items": len(p.Population.Items)})
			posted, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: p.Entity, Period: p.Period, BookID: p.BookID, BookCode: p.BookCode, Type: domain.JournalTypeRevaluation, Purpose: "REVALUATION",
				Description: fmt.Sprintf("Unrealized exchange revaluation %s (%s)", p.Period, p.Policy), Correlation: fmt.Sprintf("reval:%s:%s:%s", p.Entity, p.BookID, p.Period), PostingDate: p.ValuationDate,
				Currency: p.Currency, Actor: p.Actor, At: p.At, ExecutionKind: "KERNEL_REVAL", Trace: string(trace), Lines: lines})
			if e != nil {
				return e
			}
			run.JournalID, run.JournalNumber = posted.JournalID, posted.Number
			if p.AutoReverse {
				for _, l := range lines {
					back = append(back, SysLine{Account: l.Account, Debit: l.Credit, Credit: l.Debit, TxAmount: l.TxAmount, TxCurrency: l.TxCurrency, ExchangeRate: l.ExchangeRate, RateRef: l.RateRef,
						Counterparty: l.Counterparty, Description: "reversal of " + l.Description, RuleLineRef: l.RuleLineRef})
				}
				rtrace, _ := json.Marshal(map[string]any{"revaluation_run_id": run.RunID, "reverses": posted.JournalID})
				rev, e := r.postSystemJournal(ctx, t, &SysJournal{Tenant: p.Tenant, Entity: p.Entity, Period: p.ReversalPeriod, BookID: p.BookID, BookCode: p.BookCode, Type: domain.JournalTypeReversal,
					Purpose: "REVALUATION", Description: fmt.Sprintf("Reversal of unrealized exchange revaluation %s", p.Period), Correlation: fmt.Sprintf("reval-rev:%s:%s:%s", p.Entity, p.BookID, p.Period),
					PostingDate: p.ReversalDate, Currency: p.Currency, Actor: p.Actor, At: p.At, ReversalOf: posted.JournalID, ReversalReason: "automatic reversal of the unrealized revaluation",
					ExecutionKind: "KERNEL_REVAL_REV", Trace: string(rtrace), Lines: back})
				if e != nil {
					return e
				}
				run.ReversalJournalID, run.ReversalNumber = rev.JournalID, rev.Number
			}
		}
		pop, _ := json.Marshal(run.Population)
		if _, e := t.Exec(ctx, `INSERT INTO revaluation_runs (revaluation_run_id, tenant_id, legal_entity_id, accounting_book_id, fiscal_period, run_type, valuation_date, policy_id, ledger_watermark,
				population, item_count, total_gain, total_loss, journal_id, reversal_journal_id, created_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, 'UNREALIZED', $6::date, $7, $8, $9::jsonb, $10, $11::numeric, $12::numeric, NULLIF($13, '')::uuid, NULLIF($14, '')::uuid, $15, $16)`,
			run.RunID, p.Tenant, p.Entity, p.BookID, p.Period, run.ValuationDate, p.Policy, p.Watermark, string(pop), run.ItemCount, run.TotalGain, run.TotalLoss, run.JournalID, run.ReversalJournalID, p.Actor, p.At); e != nil {
			return e
		}
		if e := r.enqueue(ctx, t, p.Tenant, kernel.OutEvent{Fact: "revaluation.completed", AggregateType: "revaluation-run", AggregateID: run.RunID, Actor: p.Actor, LegalEntityID: p.Entity,
			Data: map[string]any{"revaluation_run_id": run.RunID, "book_id": p.BookID, "fiscal_period": p.Period, "valuation_date": run.ValuationDate, "items": run.ItemCount,
				"total_gain": run.TotalGain, "total_loss": run.TotalLoss, "journal_id": run.JournalID, "reversal_journal_id": run.ReversalJournalID}}); e != nil {
			return e
		}
		out = run
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
