package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/kernel"
)

const postingKind = "KERNEL_EVENT"

// FindPosting implements kernel.Repo.
func (r *KernelRepo) FindPosting(ctx context.Context, tenant, eventID, bookID, basis string) (*kernel.PostResult, error) {
	var out *kernel.PostResult
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		var e error
		out, e = findPostingTx(ctx, t, tenant, eventID, bookID, basis)
		return e
	})
	return out, err
}

func findPostingTx(ctx context.Context, t pgx.Tx, tenant, eventID, bookID, basis string) (*kernel.PostResult, error) {
	var p kernel.PostResult
	var number *string
	var watermark *int64
	var postedAt time.Time
	err := t.QueryRow(ctx, `
		SELECT l.accounting_event_id::text, l.accounting_book_id::text, l.posting_basis, l.posting_execution_id::text, l.journal_id::text,
		       h.journal_number, l.posting_rule_set_id::text, l.rule_version, l.proposal_hash, e.ledger_watermark, COALESCE(h.posted_at, l.created_at)
		FROM event_posting_links l
		JOIN journal_headers h ON h.journal_id = l.journal_id
		JOIN posting_executions e ON e.execution_id = l.posting_execution_id
		WHERE l.tenant_id = $1::uuid AND l.accounting_event_id = $2::uuid AND l.accounting_book_id = $3::uuid AND l.posting_basis = $4`,
		tenant, eventID, bookID, basis).
		Scan(&p.AccountingEventID, &p.BookID, &p.PostingBasis, &p.ExecutionID, &p.JournalID, &number, &p.RuleSetID, &p.RuleVersion, &p.ProposalHash, &watermark, &postedAt)
	if err != nil {
		return nil, kernelErr(err)
	}
	if number != nil {
		p.JournalNumber = *number
	}
	if watermark != nil {
		p.LedgerWatermark = *watermark
	}
	p.PostedAt = postedAt
	return &p, nil
}

// journalPostedEnvelope builds the standard journal.posted event (the same type every other posting path emits) with the
// kernel's lineage added, so consumers keep working and gain the event, book and rule the journal came from.
func (r *KernelRepo) journalPostedEnvelope(u *kernel.PostingUnit, number string, watermark int64) (*envelope.Envelope, error) {
	return envelope.New(envelope.Spec{
		Type: "com.zoikosuite.accounting.journal.posted", LegacyType: "journal.posted", Service: "general-ledger-svc", SchemaVersion: "1.0.0",
		OccurredAt: u.PostedAt, TenantID: u.Tenant, LegalEntityID: u.Event.LegalEntityID, AggregateType: "journal", AggregateID: u.JournalID,
		CorrelationID: correlationOf(u.Actor, u.Event), ActorID: u.Actor.PrincipalID, ResidencyRegion: r.eventRegion, Classification: envelope.Confidential,
		Data: map[string]any{
			"journal_id": u.JournalID, "journal_number": number, "accounting_event_id": u.Event.AccountingEventID, "book_id": u.Book.BookID,
			"posting_basis": u.Basis, "posting_execution_id": u.ExecutionID, "posting_rule_set_id": u.Proposal.Provenance.RuleSetID,
			"rule_version": u.Proposal.Provenance.RuleVersion, "proposal_hash": u.Proposal.ProposalHash, "ledger_watermark": watermark,
			"posting_date": u.Proposal.PostingDate, "fiscal_period": u.FiscalPeriod,
		},
	})
}

func correlationOf(a kernel.Actor, ev *kernel.AccountingEvent) string {
	switch {
	case a.CorrelationID != "":
		return a.CorrelationID
	case ev != nil && ev.CorrelationID != "":
		return ev.CorrelationID
	}
	return uuid.NewString()
}

// PostUnit implements kernel.Repo: the atomic posting transaction of standard §7.1.
func (r *KernelRepo) PostUnit(ctx context.Context, u *kernel.PostingUnit) (*kernel.PostResult, error) {
	var out *kernel.PostResult
	err := r.tx(ctx, u.Tenant, func(t pgx.Tx) error {
		// The posting lock (§36): two workers posting the same event in the same book serialise here, and the loser finds
		// the winner's consequence below instead of posting a second one.
		if _, e := t.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"post:"+u.Tenant+":"+u.Event.AccountingEventID+":"+u.Book.BookID+":"+u.Basis); e != nil {
			return e
		}
		if existing, e := findPostingTx(ctx, t, u.Tenant, u.Event.AccountingEventID, u.Book.BookID, u.Basis); e == nil {
			existing.Existing = true
			out = existing
			return nil
		} else if !errors.Is(e, kernel.ErrNotFound) {
			return e
		}

		// The period seal (§36): a posting that reaches here either commits before the seal's watermark or is refused by it.
		if e := assertPeriodOpen(ctx, t, u.Tenant, u.Event.LegalEntityID, u.FiscalPeriod); e != nil {
			if errors.Is(e, domain.ErrPeriodLocked) {
				return &kernel.Exception{Family: kernel.FamPeriod, Code: "PERIOD_SEALED", Detail: e.Error(), Facts: map[string]any{"fiscal_period": u.FiscalPeriod}}
			}
			return e
		}

		p := u.Proposal
		trace, e := json.Marshal(p)
		if e != nil {
			return e
		}
		now := u.PostedAt
		correlation := fmt.Sprintf("kernel:%s:%s:%s", u.Event.AccountingEventID, u.Book.BookID, u.Basis)
		if _, e := t.Exec(ctx, `
			INSERT INTO posting_executions (execution_id, tenant_id, legal_entity_id, kind, status, calculation_trace, correlation_id,
				created_at, created_by_principal_id, committed_at, accounting_event_id, accounting_book_id, posting_basis,
				posting_rule_set_id, rule_version, proposal_hash)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'COMMITTED', $5::jsonb, $6, $7, $8, $7, $9::uuid, $10::uuid, $11, $12::uuid, $13, $14)`,
			u.ExecutionID, u.Tenant, u.Event.LegalEntityID, postingKind, string(trace), correlation, now, u.Actor.PrincipalID,
			u.Event.AccountingEventID, u.Book.BookID, u.Basis, p.Provenance.RuleSetID, p.Provenance.RuleVersion, p.ProposalHash); e != nil {
			return e
		}

		txDate := u.Event.OccurredAt
		if u.Event.DocumentDate != nil {
			txDate = *u.Event.DocumentDate
		}
		postingDate, e := time.Parse("2006-01-02", p.PostingDate)
		if e != nil {
			return e
		}
		jid, bookCode, evID := u.JournalID, u.Book.BookCode, u.Event.AccountingEventID
		bookUUID, execID, srcEvent := u.Book.BookID, u.ExecutionID, u.Event.SourceEventID
		softReason := u.SoftCloseReason
		actor := u.Actor.PrincipalID
		h := &domain.JournalHeader{
			JournalID: jid, TenantID: u.Tenant, LegalEntityID: u.Event.LegalEntityID, FiscalPeriod: u.FiscalPeriod,
			Status: domain.JournalStatusFinalized, Description: u.Description, CreatedByPrincipalID: actor,
			ValidatedByPrincipalID: &actor, PostedByPrincipalID: &actor, ValidatedAt: &now, PostedAt: &now,
			CorrelationID: correlation, SourceEventID: &srcEvent, JournalType: domain.JournalTypeStandard,
			TransactionDate: domain.Date{Time: txDate}, PostingDate: domain.Date{Time: postingDate}, CurrencyCode: p.FunctionalCcy,
			BookID: &bookCode, ApprovalStatus: domain.ApprovalStatusPosted, SoftCloseOverrideReason: &softReason,
			AccountingBookID: &bookUUID, SourceAccountingEventID: &evID, PostingExecutionID: &execID, CreatedAt: now,
			EvidenceRefs: u.Event.EvidenceRefs,
		}
		// an event that names its evidence bundle keeps that name; otherwise the journal's bundle follows its references
		if b := strings.TrimSpace(u.Event.Facts["evidence_bundle_id"]); b != "" {
			h.EvidenceBundleID = &b
		}
		if _, created, e := insertJournal(ctx, t, u.Tenant, h, nil); e != nil {
			return e
		} else if !created {
			return fmt.Errorf("journal correlation %s already belongs to another journal", correlation)
		}

		for _, l := range p.Lines {
			debit, credit := "0", "0"
			if l.Side == "DEBIT" {
				debit = l.AmountFunctional
			} else {
				credit = l.AmountFunctional
			}
			var rate, rateRef *string
			if p.FX != nil && l.Currency != p.FunctionalCcy {
				rate = &p.FX.Rate
				ref := p.FX.RateID
				if ref == "" {
					ref = strings.Join([]string{p.FX.Source, p.FX.RateType, p.FX.AsOf}, ":")
				}
				rateRef = &ref
			}
			txAmt, txCcy, ruleRef := l.AmountTx, l.Currency, l.LineRef
			if _, e := t.Exec(ctx, `
				INSERT INTO journal_lines (journal_line_id, journal_id, tenant_id, line_number, account_code, debit_amount, credit_amount,
					description, dimensions, transaction_amount, transaction_currency, exchange_rate, rate_ref, posting_rule_line_ref, counterparty_id, tax_detail_ref)
				VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8, $9, $10::numeric, $11, $12::numeric, $13, $14, NULLIF($15, ''), NULLIF($16, ''))`,
				uuid.NewString(), jid, u.Tenant, l.LineNo, l.AccountCode, debit, credit, l.Description, domain.Dimensions(l.Dimensions),
				txAmt, txCcy, rate, rateRef, ruleRef, l.CounterpartyID, l.TaxDetailRef); e != nil {
				return mapPgError(e)
			}
		}

		if e := appendLedgerEntries(ctx, t, u.Tenant, jid); e != nil {
			return e
		}
		number, e := sealJournal(ctx, t, u.Tenant, jid)
		if e != nil {
			if errors.Is(e, domain.ErrUnbalancedJournal) {
				return &kernel.Exception{Family: kernel.FamBal, Code: "UNBALANCED", Detail: e.Error()}
			}
			return e
		}
		var watermark int64
		if e := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND journal_id = $2::uuid`, u.Tenant, jid).Scan(&watermark); e != nil {
			return e
		}

		env, e := r.journalPostedEnvelope(u, number, watermark)
		if e != nil {
			return fmt.Errorf("build journal.posted event: %w", e)
		}
		if e := outbox.Enqueue(ctx, t, env); e != nil {
			return fmt.Errorf("enqueue journal.posted event: %w", e)
		}
		if _, e := t.Exec(ctx, `UPDATE posting_executions SET journal_id = $3::uuid, ledger_watermark = $4, journal_event_id = $5
			WHERE tenant_id = $1::uuid AND execution_id = $2::uuid`, u.Tenant, u.ExecutionID, jid, watermark, env.ID); e != nil {
			return e
		}
		if _, e := t.Exec(ctx, `
			INSERT INTO event_posting_links (tenant_id, accounting_event_id, accounting_book_id, posting_basis, journal_id, posting_execution_id,
				posting_rule_set_id, rule_version, proposal_hash, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6::uuid, $7::uuid, $8, $9, $10)`,
			u.Tenant, u.Event.AccountingEventID, u.Book.BookID, u.Basis, jid, u.ExecutionID, p.Provenance.RuleSetID, p.Provenance.RuleVersion, p.ProposalHash, now); e != nil {
			return e
		}
		// A correction settles what its original could not: failed attempts at the event this one supersedes are resolved.
		if u.Event.SupersedesEventID != "" {
			if _, e := t.Exec(ctx, `UPDATE posting_executions SET status = 'RESOLVED'
				WHERE tenant_id = $1::uuid AND accounting_event_id = $2::uuid AND status IN ('FAILED', 'QUARANTINED')`, u.Tenant, u.Event.SupersedesEventID); e != nil {
				return e
			}
		}
		// Earlier failed attempts for this event and book are no longer outstanding.
		if _, e := t.Exec(ctx, `UPDATE posting_executions SET status = 'RESOLVED'
			WHERE tenant_id = $1::uuid AND accounting_event_id = $2::uuid AND accounting_book_id = $3::uuid AND posting_basis = $4
			  AND status IN ('FAILED', 'QUARANTINED')`, u.Tenant, u.Event.AccountingEventID, u.Book.BookID, u.Basis); e != nil {
			return e
		}
		out = &kernel.PostResult{
			AccountingEventID: u.Event.AccountingEventID, BookID: u.Book.BookID, PostingBasis: u.Basis, ExecutionID: u.ExecutionID, JournalID: jid,
			JournalNumber: number, RuleSetID: p.Provenance.RuleSetID, RuleVersion: p.Provenance.RuleVersion, ProposalHash: p.ProposalHash,
			LedgerWatermark: watermark, PostedAt: now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordPostingFailure implements kernel.Repo.
func (r *KernelRepo) RecordPostingFailure(ctx context.Context, f *kernel.PostingFailure) error {
	return r.tx(ctx, f.Tenant, func(t pgx.Tx) error {
		trace, _ := json.Marshal(map[string]any{"family": f.Family, "code": f.Code, "detail": f.Detail, "facts": f.Facts})
		reason := fmt.Sprintf("%s %s: %s", f.Family, f.Code, f.Detail)
		if _, e := t.Exec(ctx, `
			INSERT INTO posting_executions (execution_id, tenant_id, legal_entity_id, kind, status, calculation_trace, failure_reason, correlation_id,
				created_at, created_by_principal_id, accounting_event_id, accounting_book_id, posting_basis)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::jsonb, $7, $8, $9, $10, $11::uuid, $12::uuid, $13)`,
			f.ExecutionID, f.Tenant, f.Event.LegalEntityID, postingKind, f.Status, string(trace), reason, correlationOf(f.Actor, f.Event), f.At,
			f.Actor.PrincipalID, f.Event.AccountingEventID, f.BookID, f.Basis); e != nil {
			return e
		}
		return r.enqueue(ctx, t, f.Tenant, kernel.OutEvent{
			Fact: "posting.exception-raised", AggregateType: "accounting-event", AggregateID: f.Event.AccountingEventID, Actor: f.Actor.PrincipalID,
			CorrelationID: correlationOf(f.Actor, f.Event), LegalEntityID: f.Event.LegalEntityID,
			Data: map[string]any{"accounting_event_id": f.Event.AccountingEventID, "book_id": f.BookID, "posting_execution_id": f.ExecutionID,
				"status": f.Status, "family": f.Family, "code": f.Code, "detail": f.Detail, "facts": f.Facts},
		})
	})
}
