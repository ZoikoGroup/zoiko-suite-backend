package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

// SysLine is one line of a journal the kernel itself originates (a revaluation, its reversal). Amounts are exact decimal strings in
// the book's functional currency.
type SysLine struct {
	Account       string
	Debit, Credit string
	TxAmount      string
	TxCurrency    string
	ExchangeRate  string
	RateRef       string
	Counterparty  string
	Description   string
	RuleLineRef   string
}

// SysJournal is a posted journal the kernel originates. It is written exactly like an event posting: header, lines, ledger entries,
// number, seal and the journal.posted event, all inside the caller's transaction.
type SysJournal struct {
	Tenant, Entity, Period string
	BookID, BookCode       string
	Type                   domain.JournalType
	Purpose                string
	Description            string
	Correlation            string
	PostingDate            time.Time
	Currency               string
	Actor                  string
	ReversalOf             string
	ReversalReason         string
	At                     time.Time
	ExecutionKind          string
	Trace                  string
	Lines                  []SysLine
}

// SysPosted is what a system journal produced.
type SysPosted struct {
	JournalID, Number, ExecutionID string
	Watermark                      int64
}

// postSystemJournal writes a kernel-originated journal inside tx. The period must be open for it (the seal is checked here, in the
// same transaction as the write).
func (r *KernelRepo) postSystemJournal(ctx context.Context, t pgx.Tx, j *SysJournal) (*SysPosted, error) {
	if e := assertPeriodOpen(ctx, t, j.Tenant, j.Entity, j.Period); e != nil {
		if errors.Is(e, domain.ErrPeriodLocked) {
			return nil, &kernel.Exception{Family: kernel.FamPeriod, Code: "PERIOD_SEALED", Detail: e.Error(), Facts: map[string]any{"fiscal_period": j.Period}}
		}
		return nil, e
	}
	out := &SysPosted{JournalID: uuid.NewString(), ExecutionID: uuid.NewString()}
	if _, e := t.Exec(ctx, `
		INSERT INTO posting_executions (execution_id, tenant_id, legal_entity_id, kind, status, calculation_trace, correlation_id, created_at, created_by_principal_id, committed_at, accounting_book_id, posting_basis)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'COMMITTED', $5::jsonb, $6, $7, $8, $7, $9::uuid, $10)`,
		out.ExecutionID, j.Tenant, j.Entity, j.ExecutionKind, j.Trace, j.Correlation, j.At, j.Actor, j.BookID, j.ExecutionKind); e != nil {
		return nil, e
	}
	actor, soft := j.Actor, ""
	bookID, execID := j.BookID, out.ExecutionID
	purpose := j.Purpose
	h := &domain.JournalHeader{
		JournalID: out.JournalID, TenantID: j.Tenant, LegalEntityID: j.Entity, FiscalPeriod: j.Period, Status: domain.JournalStatusFinalized, Description: j.Description,
		CreatedByPrincipalID: actor, ValidatedByPrincipalID: &actor, PostedByPrincipalID: &actor, ValidatedAt: &j.At, PostedAt: &j.At, CorrelationID: j.Correlation,
		JournalType: j.Type, TransactionDate: domain.Date{Time: j.PostingDate}, PostingDate: domain.Date{Time: j.PostingDate}, CurrencyCode: j.Currency, BookID: &j.BookCode,
		ApprovalStatus: domain.ApprovalStatusPosted, SoftCloseOverrideReason: &soft, AccountingBookID: &bookID, PostingExecutionID: &execID, JournalPurpose: &purpose, CreatedAt: j.At,
	}
	if j.ReversalOf != "" {
		rev, why := j.ReversalOf, j.ReversalReason
		h.ReversalOfJournalID, h.ReversalReason = &rev, &why
	}
	if _, created, e := insertJournal(ctx, t, j.Tenant, h, nil); e != nil {
		return nil, e
	} else if !created {
		return nil, fmt.Errorf("journal correlation %s already belongs to another journal", j.Correlation)
	}
	for i, l := range j.Lines {
		if _, e := t.Exec(ctx, `
			INSERT INTO journal_lines (journal_line_id, journal_id, tenant_id, line_number, account_code, debit_amount, credit_amount, description, transaction_amount, transaction_currency,
				exchange_rate, rate_ref, posting_rule_line_ref, counterparty_id)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8, $9::numeric, NULLIF($10, ''), $11::numeric, NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''))`,
			uuid.NewString(), out.JournalID, j.Tenant, i+1, l.Account, l.Debit, l.Credit, l.Description, nilIfEmpty(l.TxAmount), l.TxCurrency, nilIfEmpty(l.ExchangeRate), l.RateRef, l.RuleLineRef, l.Counterparty); e != nil {
			return nil, mapPgError(e)
		}
	}
	if e := appendLedgerEntries(ctx, t, j.Tenant, out.JournalID); e != nil {
		return nil, e
	}
	number, e := sealJournal(ctx, t, j.Tenant, out.JournalID)
	if e != nil {
		if errors.Is(e, domain.ErrUnbalancedJournal) {
			return nil, &kernel.Exception{Family: kernel.FamBal, Code: "UNBALANCED", Detail: e.Error()}
		}
		return nil, e
	}
	out.Number = number
	if e := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, j.Tenant, out.JournalID).Scan(&out.Watermark); e != nil {
		return nil, e
	}
	env, e := envelope.New(envelope.Spec{
		Type: "com.zoikosuite.accounting.journal.posted", LegacyType: "journal.posted", Service: "general-ledger-svc", SchemaVersion: "1.0.0", OccurredAt: j.At, TenantID: j.Tenant,
		LegalEntityID: j.Entity, AggregateType: "journal", AggregateID: out.JournalID, CorrelationID: j.Correlation, ActorID: j.Actor, ResidencyRegion: r.eventRegion, Classification: envelope.Confidential,
		Data: map[string]any{"journal_id": out.JournalID, "journal_number": number, "journal_type": string(j.Type), "book_id": j.BookID, "posting_execution_id": out.ExecutionID,
			"posting_date": j.PostingDate.Format("2006-01-02"), "fiscal_period": j.Period, "ledger_watermark": out.Watermark},
	})
	if e != nil {
		return nil, fmt.Errorf("build journal.posted event: %w", e)
	}
	if e := outbox.Enqueue(ctx, t, env); e != nil {
		return nil, fmt.Errorf("enqueue journal.posted event: %w", e)
	}
	if _, e := t.Exec(ctx, `UPDATE posting_executions SET journal_id = $3::uuid, ledger_watermark = $4, journal_event_id = $5 WHERE tenant_id = $1::uuid AND execution_id = $2::uuid`,
		j.Tenant, out.ExecutionID, out.JournalID, out.Watermark, env.ID); e != nil {
		return nil, e
	}
	if j.ReversalOf != "" {
		tag, e := t.Exec(ctx, `UPDATE journal_headers SET status = 'REVERSED', reversed_by_principal_id = $3, reversed_at = $4 WHERE tenant_id = $1::uuid AND journal_id = $2::uuid AND status = 'FINALIZED'`,
			j.Tenant, j.ReversalOf, j.Actor, j.At)
		if e != nil {
			return nil, e
		}
		if tag.RowsAffected() != 1 {
			return nil, kernel.ErrState
		}
	}
	return out, nil
}
