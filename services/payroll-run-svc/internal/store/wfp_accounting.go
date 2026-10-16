package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// AccountingChangeKind says which command produced an AccountingChange.
type AccountingChangeKind string

const (
	AccChangeCreated   AccountingChangeKind = "CREATED"
	AccChangeSubmitted AccountingChangeKind = "SUBMITTED"
	AccChangePosted    AccountingChangeKind = "POSTED"
	AccChangeRejected  AccountingChangeKind = "REJECTED"
	AccChangeCorrected AccountingChangeKind = "CORRECTED"
)

// AccountingChange is what a committed command changed; the builder turns it into
// outbox events inside the same transaction.
type AccountingChange struct {
	Kind     AccountingChangeKind
	Bridge   *domain.AccountingBridge
	Original *domain.AccountingBridge // the subject a reversal or correction concerns
	Reason   string
}

type AccountingEventBuilder func(AccountingChange) ([]domain.OutboxEvent, error)

func enqueueAccounting(ctx context.Context, tx pgx.Tx, mk AccountingEventBuilder, ch AccountingChange) error {
	if mk == nil {
		return nil
	}
	evs, err := mk(ch)
	if err != nil {
		return fmt.Errorf("build events: %w", err)
	}
	for _, ev := range evs {
		if !domain.EventCatalogue[ev.EventType] {
			return fmt.Errorf("event type %q is not in the WFP catalogue", ev.EventType)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO wfp_outbox (tenant_id, object_id, event_type, payload) VALUES ($1, $2, $3, $4)`,
			ev.TenantID, ev.ObjectID, ev.EventType, ev.Payload); err != nil {
			return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
		}
	}
	return nil
}

const bridgeCols = `bridge_id, tenant_id, run_ref_id::text, result_id::text, legal_entity_id, accounting_book_id, posting_purpose,
	original_bridge_id::text, source_event_id, fiscal_period, document_date::text, posting_date::text, transaction_currency,
	description, lines, line_count, total_debit::text, total_credit::text, evidence_flags, state, correction_kind,
	rejection_reason, execution_id, journal_id, attempts, last_error, submitted_by, submitted_at, posted_at,
	reconciliation_state, reconciled_at, reconciliation_detail, version, created_by, correlation_id, created_at, updated_at`

func scanBridge(row pgx.Row) (*domain.AccountingBridge, error) {
	var b domain.AccountingBridge
	var lines, detail []byte
	err := row.Scan(&b.BridgeID, &b.TenantID, &b.RunRefID, &b.ResultID, &b.LegalEntityID, &b.AccountingBookID, &b.PostingPurpose,
		&b.OriginalBridgeID, &b.SourceEventID, &b.FiscalPeriod, &b.DocumentDate, &b.PostingDate, &b.TransactionCurrency,
		&b.Description, &lines, &b.LineCount, &b.TotalDebit, &b.TotalCredit, &b.EvidenceFlags, &b.State, &b.CorrectionKind,
		&b.RejectionReason, &b.ExecutionID, &b.JournalID, &b.Attempts, &b.LastError, &b.SubmittedBy, &b.SubmittedAt, &b.PostedAt,
		&b.ReconciliationState, &b.ReconciledAt, &detail, &b.Version, &b.CreatedBy, &b.CorrelationID, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBridgeNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Lines = []domain.PostingLine{}
	if len(lines) > 0 {
		if err := json.Unmarshal(lines, &b.Lines); err != nil {
			return nil, fmt.Errorf("decode posting lines: %w", err)
		}
	}
	if len(detail) > 0 {
		b.ReconciliationDetail = json.RawMessage(detail)
	}
	if b.EvidenceFlags == nil {
		b.EvidenceFlags = []string{}
	}
	return &b, nil
}

// mapAccountingGuard turns the database guard's refusals into domain errors.
func mapAccountingGuard(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23000" {
		return err
	}
	switch {
	case strings.Contains(pgErr.Message, "separation of duties"):
		return domain.ErrSoDSamePrincipal
	case strings.Contains(pgErr.Message, "cannot be submitted for posting"), strings.Contains(pgErr.Message, "only an ACCEPTED"),
		strings.Contains(pgErr.Message, "only for an ACCEPTED result"):
		return domain.ErrResultNotPostable
	case strings.Contains(pgErr.Message, "withdrawn only when"):
		return domain.ErrBridgeNotWithdrawable
	case strings.Contains(pgErr.Message, "reversal needs a POSTED original"), strings.Contains(pgErr.Message, "REVERSED only after"):
		return domain.ErrBridgeNotReversible
	}
	return err
}

// ── building the proposal input ──────────────────────────────────────────────

// ResultPostingInputFor reads what an ACCEPTED result says (gross, net, and the
// sum of each component kind) as exact rationals, with the result and its run
// reference for the caller's eligibility checks.
func (s *PgStore) ResultPostingInputFor(ctx context.Context, runRefID string) (domain.ResultPostingInput, *domain.PayrollResult, *domain.PayrollRunRef, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return domain.ResultPostingInput{}, nil, nil, err
	}
	var in domain.ResultPostingInput
	var res *domain.PayrollResult
	var ref *domain.PayrollRunRef
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		if ref, e = scanRunRef(tx.QueryRow(ctx, `SELECT `+runRefCols+` FROM wfp_payroll_run_refs
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID)); e != nil {
			return e
		}
		if res, e = scanResult(tx.QueryRow(ctx, `SELECT `+resultCols+` FROM wfp_payroll_results
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid`, tenantID, runRefID)); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT kind, COALESCE(sum(amount), 0)::text FROM wfp_payroll_result_components
			WHERE tenant_id = $1 AND result_id = $2::uuid GROUP BY kind`, tenantID, res.ResultID)
		if e != nil {
			return e
		}
		defer rows.Close()
		in.ByKind = map[string]*big.Rat{}
		for rows.Next() {
			var kind, sum string
			if e := rows.Scan(&kind, &sum); e != nil {
				return e
			}
			r, perr := domain.ParseDecimal(sum)
			if perr != nil {
				return perr
			}
			in.ByKind[kind] = r
		}
		return rows.Err()
	})
	if err != nil {
		return domain.ResultPostingInput{}, nil, nil, err
	}
	if in.Gross, err = domain.ParseDecimal(res.TotalGross); err != nil {
		return in, res, ref, err
	}
	if in.Net, err = domain.ParseDecimal(res.TotalNetPay); err != nil {
		return in, res, ref, err
	}
	return in, res, ref, nil
}

// ── create / submit / reverse / withdraw ─────────────────────────────────────

func insertBridgeTx(ctx context.Context, tx pgx.Tx, tenantID string, b *domain.AccountingBridge) (stored *domain.AccountingBridge, created bool, err error) {
	lines, err := json.Marshal(b.Lines)
	if err != nil {
		return nil, false, err
	}
	flags := b.EvidenceFlags
	if flags == nil {
		flags = []string{}
	}
	stored, err = scanBridge(tx.QueryRow(ctx, `
		INSERT INTO wfp_payroll_accounting_bridges
			(bridge_id, tenant_id, run_ref_id, result_id, legal_entity_id, accounting_book_id, posting_purpose, original_bridge_id,
			 source_event_id, fiscal_period, document_date, posting_date, transaction_currency, description, lines, line_count,
			 total_debit, total_credit, evidence_flags, state, created_by, correlation_id)
		VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7, $8::uuid, $9, $10, $11::date, $12::date, $13, $14, $15::jsonb, $16,
		        $17::numeric, $18::numeric, $19, 'EVENT_CREATED', $20, $21)
		ON CONFLICT (tenant_id, result_id, accounting_book_id, posting_purpose) DO NOTHING
		RETURNING `+bridgeCols,
		b.BridgeID, tenantID, b.RunRefID, b.ResultID, b.LegalEntityID, b.AccountingBookID, b.PostingPurpose, b.OriginalBridgeID,
		b.SourceEventID, b.FiscalPeriod, b.DocumentDate, b.PostingDate, b.TransactionCurrency, b.Description, string(lines),
		b.LineCount, b.TotalDebit, b.TotalCredit, flags, b.CreatedBy, b.CorrelationID))
	if errors.Is(err, domain.ErrBridgeNotFound) { // DO NOTHING returned no row: the subject already exists
		existing, err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND result_id = $2::uuid AND accounting_book_id = $3 AND posting_purpose = $4`,
			tenantID, b.ResultID, b.AccountingBookID, b.PostingPurpose))
		return existing, false, err
	}
	return stored, err == nil, mapAccountingGuard(err)
}

// CreateAccountingBridge records the accounting subject for one result version +
// book + purpose. A second request for the same subject returns the first
// (created=false); the database allows exactly one.
func (s *PgStore) CreateAccountingBridge(ctx context.Context, b *domain.AccountingBridge, mk AccountingEventBuilder) (*domain.AccountingBridge, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
	}
	var out *domain.AccountingBridge
	var created bool
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, created, e = insertBridgeTx(ctx, tx, tenantID, b)
		if e != nil || !created {
			return e
		}
		return enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeCreated, Bridge: out})
	})
	return out, created, err
}

func lockBridgeTx(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.AccountingBridge, error) {
	return scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
		WHERE tenant_id = $1 AND bridge_id = $2::uuid FOR UPDATE`, tenantID, id))
}

func (s *PgStore) GetAccountingBridge(ctx context.Context, id string) (*domain.AccountingBridge, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var b *domain.AccountingBridge
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		b, e = scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND bridge_id = $2::uuid`, tenantID, id))
		return e
	})
	return b, err
}

// ListAccountingBridges returns every subject of a run version (original and reversal).
func (s *PgStore) ListAccountingBridges(ctx context.Context, runRefID string) ([]domain.AccountingBridge, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.AccountingBridge{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND run_ref_id = $2::uuid ORDER BY created_at`, tenantID, runRefID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBridge(rows)
			if err != nil {
				return err
			}
			out = append(out, *b)
		}
		return rows.Err()
	})
	return out, err
}

// SubmitAccountingBridge queues an EVENT_CREATED (or previously REJECTED) subject
// for the dispatcher. The database refuses a submitter who received the result,
// and a result that is no longer ACCEPTED.
func (s *PgStore) SubmitAccountingBridge(ctx context.Context, id string, expectedVersion int, actor string, mk AccountingEventBuilder) (*domain.AccountingBridge, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.AccountingBridge
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockBridgeTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrBridgeVersionConflict
		}
		if cur.State != domain.BridgeEventCreated && cur.State != domain.BridgeRejected {
			return domain.ErrBridgeNotSubmittable
		}
		out, err = scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges
			SET state = 'SUBMITTED', submitted_by = $3, submitted_at = now(), attempts = 0, last_error = NULL,
			    rejection_reason = NULL, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, tenantID, id, actor))
		if err != nil {
			return mapAccountingGuard(err)
		}
		return enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeSubmitted, Bridge: out})
	})
	return out, err
}

// CreateReversalBridge creates the reversal subject for a POSTED subject whose
// result was SUPERSEDED. The reversal carries no lines of its own: the ledger
// builds the reversing journal from the original journal.
func (s *PgStore) CreateReversalBridge(ctx context.Context, originalID, reason, creator, correlationID string, mk AccountingEventBuilder) (*domain.AccountingBridge, bool, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, false, err
	}
	var out *domain.AccountingBridge
	var created bool
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		orig, err := lockBridgeTx(ctx, tx, tenantID, originalID)
		if err != nil {
			return err
		}
		if orig.PostingPurpose != domain.PurposeResult || orig.State != domain.BridgePosted || orig.JournalID == nil {
			return domain.ErrBridgeNotReversible
		}
		rev := &domain.AccountingBridge{
			BridgeID: newUUID(), RunRefID: orig.RunRefID, ResultID: orig.ResultID, LegalEntityID: orig.LegalEntityID,
			AccountingBookID: orig.AccountingBookID, PostingPurpose: domain.PurposeReversal, OriginalBridgeID: &orig.BridgeID,
			SourceEventID: domain.SourceEventID(orig.ResultID, orig.AccountingBookID, domain.PurposeReversal),
			FiscalPeriod:  orig.FiscalPeriod, DocumentDate: orig.DocumentDate, PostingDate: orig.PostingDate,
			TransactionCurrency: orig.TransactionCurrency, Description: "Reversal of " + orig.Description + ": " + reason,
			Lines: []domain.PostingLine{}, LineCount: 0, TotalDebit: orig.TotalDebit, TotalCredit: orig.TotalCredit,
			EvidenceFlags: orig.EvidenceFlags, CreatedBy: creator, CorrelationID: correlationID,
		}
		out, created, err = insertBridgeTx(ctx, tx, tenantID, rev)
		if err != nil || !created {
			return err
		}
		return enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeCreated, Bridge: out, Original: orig, Reason: reason})
	})
	return out, created, err
}

// WithdrawAccountingBridge ends an unposted subject whose result was superseded,
// so a superseded result can never reach the ledger.
func (s *PgStore) WithdrawAccountingBridge(ctx context.Context, id string, expectedVersion int, reason string, mk AccountingEventBuilder) (*domain.AccountingBridge, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *domain.AccountingBridge
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := lockBridgeTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if cur.Version != expectedVersion {
			return domain.ErrBridgeVersionConflict
		}
		if cur.State == domain.BridgePosted || cur.State == domain.BridgeCorrected {
			return domain.ErrBridgeNotWithdrawable
		}
		out, err = scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges
			SET state = 'CORRECTED', correction_kind = 'WITHDRAWN', rejection_reason = $3, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, tenantID, id, reason))
		if err != nil {
			return mapAccountingGuard(err)
		}
		return enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeCorrected, Bridge: out, Reason: reason})
	})
	return out, err
}

// RecordReconciliation stores the outcome of comparing a posted subject with the
// ledger's journal. It changes nothing else.
func (s *PgStore) RecordReconciliation(ctx context.Context, id, state string, detail any) (*domain.AccountingBridge, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	var out *domain.AccountingBridge
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var e error
		out, e = scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges SET reconciliation_state = $3, reconciled_at = now(),
			       reconciliation_detail = $4::jsonb, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, tenantID, id, state, string(raw)))
		return e
	})
	return out, err
}

// ── dispatcher ───────────────────────────────────────────────────────────────

// PostOutcome is what the dispatcher learned from the ledger.
type PostOutcome struct {
	Posted      bool
	Final       bool // a refusal the ledger will not change its mind about (not transient)
	ExecutionID string
	JournalID   string
	Err         string
}

// PostFunc submits one subject to the ledger. originalJournalID is set only for a reversal.
type PostFunc func(ctx context.Context, b domain.AccountingBridge, originalJournalID string) PostOutcome

// DispatchPending claims SUBMITTED subjects (SKIP LOCKED, so replicas never
// double-submit), posts each through post, and records the outcome and its events
// in the same transaction. It returns how many were POSTED.
//
// The ledger is idempotent on source_event_id, so a crash between a successful
// post and this commit re-posts the same subject and gets the same execution back.
// Transient failures leave the subject SUBMITTED with attempts counted; after
// MaxPostingAttempts it waits for an operator to requeue it.
func (s *PgStore) DispatchPending(ctx context.Context, limit int, post PostFunc, mk AccountingEventBuilder) (int, error) {
	posted := 0
	err := s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE state = 'SUBMITTED' AND attempts < $1 ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`,
			domain.MaxPostingAttempts, limit)
		if err != nil {
			return fmt.Errorf("claim accounting subjects: %w", err)
		}
		var claimed []domain.AccountingBridge
		for rows.Next() {
			b, err := scanBridge(rows)
			if err != nil {
				rows.Close()
				return err
			}
			claimed = append(claimed, *b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range claimed {
			ok, err := s.dispatchOne(ctx, tx, &claimed[i], post, mk)
			if err != nil {
				return err
			}
			if ok {
				posted++
			}
		}
		return nil
	})
	return posted, err
}

func (s *PgStore) dispatchOne(ctx context.Context, tx pgx.Tx, b *domain.AccountingBridge, post PostFunc, mk AccountingEventBuilder) (bool, error) {
	// Everything below reads and writes this subject's tenant's tables.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", b.TenantID); err != nil {
		return false, fmt.Errorf("set tenant context: %w", err)
	}

	var refState string
	if err := tx.QueryRow(ctx, `SELECT acceptance_state FROM wfp_payroll_run_refs WHERE tenant_id = $1 AND run_ref_id = $2::uuid`,
		b.TenantID, b.RunRefID).Scan(&refState); err != nil {
		return false, err
	}
	// A result superseded after submission must not reach the ledger.
	if b.PostingPurpose == domain.PurposeResult && refState != domain.AcceptanceAccepted {
		out, err := scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges SET state = 'CORRECTED', correction_kind = 'WITHDRAWN',
			       rejection_reason = 'result superseded before posting', version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, b.TenantID, b.BridgeID))
		if err != nil {
			return false, err
		}
		return false, enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeCorrected, Bridge: out, Reason: "result superseded before posting"})
	}

	originalJournal := ""
	var original *domain.AccountingBridge
	if b.PostingPurpose == domain.PurposeReversal {
		o, err := scanBridge(tx.QueryRow(ctx, `SELECT `+bridgeCols+` FROM wfp_payroll_accounting_bridges
			WHERE tenant_id = $1 AND bridge_id = $2::uuid FOR UPDATE`, b.TenantID, *b.OriginalBridgeID))
		if err != nil {
			return false, err
		}
		if o.JournalID == nil {
			return false, fmt.Errorf("reversal %s: original has no journal", b.BridgeID)
		}
		original, originalJournal = o, *o.JournalID
	}

	res := post(ctx, *b, originalJournal)
	switch {
	case res.Posted:
		out, err := scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges SET state = 'POSTED', execution_id = $3, journal_id = $4, posted_at = now(),
			       attempts = attempts + 1, last_error = NULL, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, b.TenantID, b.BridgeID, res.ExecutionID, res.JournalID))
		if err != nil {
			return false, err
		}
		if err := enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangePosted, Bridge: out, Original: original}); err != nil {
			return false, err
		}
		if original != nil {
			corrected, err := scanBridge(tx.QueryRow(ctx, `
				UPDATE wfp_payroll_accounting_bridges SET state = 'CORRECTED', correction_kind = 'REVERSED', version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, b.TenantID, original.BridgeID))
			if err != nil {
				return false, err
			}
			if err := enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeCorrected, Bridge: corrected, Original: out, Reason: "reversed"}); err != nil {
				return false, err
			}
		}
		return true, nil
	case res.Final:
		out, err := scanBridge(tx.QueryRow(ctx, `
			UPDATE wfp_payroll_accounting_bridges SET state = 'REJECTED', rejection_reason = $3, attempts = attempts + 1,
			       last_error = $3, version = version + 1, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid RETURNING `+bridgeCols, b.TenantID, b.BridgeID, truncate(res.Err, 1000)))
		if err != nil {
			return false, err
		}
		return false, enqueueAccounting(ctx, tx, mk, AccountingChange{Kind: AccChangeRejected, Bridge: out, Reason: res.Err})
	default:
		_, err := tx.Exec(ctx, `UPDATE wfp_payroll_accounting_bridges SET attempts = attempts + 1, last_error = $3, updated_at = now()
			WHERE tenant_id = $1 AND bridge_id = $2::uuid`, b.TenantID, b.BridgeID, truncate(res.Err, 1000))
		return false, err
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// AccountingDispatchBacklog reports how many subjects wait for the dispatcher and how long the oldest has waited.
func (s *PgStore) AccountingDispatchBacklog(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(submitted_at) FROM wfp_payroll_accounting_bridges WHERE state = 'SUBMITTED'`).Scan(&pending, &oldest); err != nil {
			return err
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}

func newUUID() string { return uuid.NewString() }
