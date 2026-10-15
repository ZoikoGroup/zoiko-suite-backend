package ar08

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// querier is satisfied by pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func notFound(what string) *kit.Problem { return kit.NotFound(what + " not found") }

// ── receipts ─────────────────────────────────────────────────────────────────

const receiptCols = `receipt_id::text, legal_entity_id::text, bank_transaction_id::text, settlement_ref, COALESCE(customer_party_id::text,''),
	currency, amount, applied_amount, unapplied_amount, difference_amount, value_date, payer_text, remittance_text, related_party,
	state, blocked, COALESCE(control_exception,''), registered_by, version`

func scanReceipt(row pgx.Row) (*Receipt, error) {
	var r Receipt
	var vd time.Time
	if err := row.Scan(&r.ReceiptID, &r.LegalEntityID, &r.BankTransactionID, &r.SettlementRef, &r.CustomerPartyID, &r.Currency, &r.Amount,
		&r.AppliedAmount, &r.UnappliedAmount, &r.DifferenceAmount, &vd, &r.PayerText, &r.RemittanceText, &r.RelatedParty, &r.State, &r.Blocked,
		&r.ControlException, &r.RegisteredBy, &r.Version); err != nil {
		return nil, err
	}
	r.ValueDate = day(vd)
	return &r, nil
}

func getReceipt(ctx context.Context, q querier, tenantID, id string, lock bool) (*Receipt, error) {
	if err := mustUUID("receipt_id", id); err != nil {
		return nil, err
	}
	sql := `SELECT ` + receiptCols + ` FROM ar08_receipts WHERE tenant_id = $1 AND receipt_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanReceipt(q.QueryRow(ctx, sql, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("receipt")
	}
	if err != nil {
		return nil, err
	}
	r.TenantID = tenantID
	return r, nil
}

func getReceiptByBankTxn(ctx context.Context, q querier, tenantID, bankTxnID string) (*Receipt, error) {
	r, err := scanReceipt(q.QueryRow(ctx, `SELECT `+receiptCols+` FROM ar08_receipts WHERE tenant_id = $1 AND bank_transaction_id = $2`, tenantID, bankTxnID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("receipt")
	}
	return r, err
}

// ── batches ──────────────────────────────────────────────────────────────────

func getBatch(ctx context.Context, q querier, tenantID, id string, lock bool) (*Batch, error) {
	if err := mustUUID("batch_id", id); err != nil {
		return nil, err
	}
	sql := `SELECT batch_id::text, legal_entity_id::text, currency, reference, state, version, created_by FROM ar08_batches WHERE tenant_id = $1 AND batch_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var b Batch
	err := q.QueryRow(ctx, sql, tenantID, id).Scan(&b.BatchID, &b.LegalEntityID, &b.Currency, &b.Reference, &b.State, &b.Version, &b.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("batch")
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func batchReceiptIDs(ctx context.Context, q querier, tenantID, batchID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT receipt_id::text FROM ar08_batch_receipts WHERE tenant_id = $1 AND batch_id = $2 ORDER BY receipt_id`, tenantID, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func batchOfReceipt(ctx context.Context, q querier, tenantID, receiptID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT batch_id::text FROM ar08_batch_receipts WHERE tenant_id = $1 AND receipt_id = $2`, tenantID, receiptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// recomputeBatch re-derives the batch state from its receipts (a RECONCILED batch that changes is no longer reconciled).
func recomputeBatch(ctx context.Context, tx pgx.Tx, tenantID, batchID string, now time.Time) (*Batch, error) {
	b, err := getBatch(ctx, tx, tenantID, batchID, true)
	if err != nil {
		return nil, err
	}
	var total, applied, full int
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE r.state <> 'UNAPPLIED'), count(*) FILTER (WHERE r.state = 'FULLY_APPLIED'),
			EXISTS (SELECT 1 FROM ar08_manual_matches m WHERE m.tenant_id = $1 AND m.batch_id = $2 AND m.status = 'PENDING')
			OR EXISTS (SELECT 1 FROM ar08_candidates cd JOIN ar08_batch_receipts b2 ON b2.receipt_id = cd.receipt_id AND b2.tenant_id = cd.tenant_id
			           JOIN ar08_receipts r2 ON r2.receipt_id = cd.receipt_id AND r2.tenant_id = cd.tenant_id
			           WHERE cd.tenant_id = $1 AND b2.batch_id = $2 AND r2.state = 'UNAPPLIED')
		FROM ar08_batch_receipts br JOIN ar08_receipts r ON r.receipt_id = br.receipt_id AND r.tenant_id = br.tenant_id
		WHERE br.tenant_id = $1 AND br.batch_id = $2`, tenantID, batchID).Scan(&total, &applied, &full, &pending); err != nil {
		return nil, err
	}
	next := BatchStateOf(total, applied, full, pending)
	if next != b.State {
		if _, err := tx.Exec(ctx, `UPDATE ar08_batches SET state = $3, version = version + 1, updated_at = $4 WHERE tenant_id = $1 AND batch_id = $2`, tenantID, batchID, next, now); err != nil {
			return nil, err
		}
		b.State, b.Version = next, b.Version+1
	}
	return b, nil
}

// ── applications ─────────────────────────────────────────────────────────────

const appCols = `application_id::text, legal_entity_id::text, receipt_id::text, batch_id::text, status, mode, request_hash, currency, receipt_amount,
	applied_amount, unapplied_amount, difference_amount, difference_kind, COALESCE(reason_code,''), COALESCE(evidence_id,''), COALESCE(match_id::text,''),
	COALESCE(reapplies_application_id::text,''), applied_by, COALESCE(approved_by,''), applied_at, COALESCE(unapplied_by,''), unapplied_at,
	COALESCE(unapply_reason_code,''), accounting_state, COALESCE(journal_id,''), COALESCE(accounting_error,''), COALESCE(unapply_accounting_state,''),
	COALESCE(unapply_journal_id,''), version`

func scanApp(row pgx.Row) (*Application, error) {
	var a Application
	if err := row.Scan(&a.ApplicationID, &a.LegalEntityID, &a.ReceiptID, &a.BatchID, &a.Status, &a.Mode, &a.RequestHash, &a.Currency, &a.ReceiptAmount,
		&a.AppliedAmount, &a.UnappliedAmount, &a.DifferenceAmount, &a.DifferenceKind, &a.ReasonCode, &a.EvidenceID, &a.MatchID, &a.Reapplies,
		&a.AppliedBy, &a.ApprovedBy, &a.AppliedAt, &a.UnappliedBy, &a.UnappliedAt, &a.UnapplyReason, &a.AccountingState, &a.JournalID,
		&a.AccountingError, &a.UnapplyState, &a.UnapplyJournalID, &a.Version); err != nil {
		return nil, err
	}
	a.Lines = []Line{}
	return &a, nil
}

func loadLines(ctx context.Context, q querier, tenantID, appID string) ([]Line, error) {
	rows, err := q.Query(ctx, `SELECT line_no, open_item_id::text, amount FROM ar08_application_lines WHERE tenant_id = $1 AND application_id = $2 ORDER BY line_no`, tenantID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.LineNo, &l.OpenItemID, &l.Amount); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func finishApp(ctx context.Context, q querier, tenantID string, a *Application, err error) (*Application, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Lines, err = loadLines(ctx, q, tenantID, a.ApplicationID)
	return a, err
}

// getApplication returns nil when absent.
func getApplication(ctx context.Context, q querier, tenantID, id string, lock bool) (*Application, error) {
	if err := mustUUID("application_id", id); err != nil {
		return nil, err
	}
	sql := `SELECT ` + appCols + ` FROM ar08_applications WHERE tenant_id = $1 AND application_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	a, err := scanApp(q.QueryRow(ctx, sql, tenantID, id))
	return finishApp(ctx, q, tenantID, a, err)
}

// activeApplication returns the receipt's APPLIED application, or nil.
func activeApplication(ctx context.Context, q querier, tenantID, receiptID string, lock bool) (*Application, error) {
	sql := `SELECT ` + appCols + ` FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2 AND status = 'APPLIED'`
	if lock {
		sql += ` FOR UPDATE`
	}
	a, err := scanApp(q.QueryRow(ctx, sql, tenantID, receiptID))
	return finishApp(ctx, q, tenantID, a, err)
}

// latestUnappliedApplication is the receipt's most recent reversed application, or nil.
func latestUnappliedApplication(ctx context.Context, q querier, tenantID, receiptID string) (*Application, error) {
	a, err := scanApp(q.QueryRow(ctx, `SELECT `+appCols+` FROM ar08_applications WHERE tenant_id = $1 AND receipt_id = $2 AND status = 'UNAPPLIED'
		ORDER BY seq DESC LIMIT 1`, tenantID, receiptID))
	return finishApp(ctx, q, tenantID, a, err)
}

// ── candidates / matches ─────────────────────────────────────────────────────

// loadCandidates reads one generation (0 = the latest) in rank order.
func loadCandidates(ctx context.Context, q querier, tenantID, receiptID string, generation int) (int, []Candidate, error) {
	if generation == 0 {
		if err := q.QueryRow(ctx, `SELECT COALESCE(max(generation),0) FROM ar08_candidates WHERE tenant_id = $1 AND receipt_id = $2`, tenantID, receiptID).Scan(&generation); err != nil {
			return 0, nil, err
		}
	}
	rows, err := q.Query(ctx, `SELECT candidate_id::text, generation, rank, open_item_id::text, customer_party_id::text, score, basis, residual_amount,
			suggested_amount, disputed, collection_hold FROM ar08_candidates WHERE tenant_id = $1 AND receipt_id = $2 AND generation = $3 ORDER BY rank`, tenantID, receiptID, generation)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		var basis []byte
		if err := rows.Scan(&c.CandidateID, &c.Generation, &c.Rank, &c.OpenItemID, &c.CustomerPartyID, &c.Score, &basis, &c.Residual, &c.Suggested, &c.Disputed, &c.CollectionHold); err != nil {
			return 0, nil, err
		}
		_ = json.Unmarshal(basis, &c.Basis)
		out = append(out, c)
	}
	return generation, out, rows.Err()
}

const matchCols = `match_id::text, receipt_id::text, batch_id::text, lines, difference_amount, difference_kind, reason_code, evidence_id, requires, status,
	proposed_by, COALESCE(approved_by,''), COALESCE(application_id::text,'')`

func scanMatch(row pgx.Row) (*ManualMatch, error) {
	var m ManualMatch
	var lines []byte
	if err := row.Scan(&m.MatchID, &m.ReceiptID, &m.BatchID, &lines, &m.DifferenceAmount, &m.DifferenceKind, &m.ReasonCode, &m.EvidenceID, &m.Requires, &m.Status,
		&m.ProposedBy, &m.ApprovedBy, &m.ApplicationID); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(lines, &m.Lines); err != nil {
		return nil, err
	}
	return &m, nil
}

func getMatch(ctx context.Context, q querier, tenantID, id string, lock bool) (*ManualMatch, string, error) {
	if err := mustUUID("match_id", id); err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + matchCols + `, legal_entity_id::text FROM ar08_manual_matches WHERE tenant_id = $1 AND match_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var m ManualMatch
	var lines []byte
	var entity string
	err := q.QueryRow(ctx, sql, tenantID, id).Scan(&m.MatchID, &m.ReceiptID, &m.BatchID, &lines, &m.DifferenceAmount, &m.DifferenceKind, &m.ReasonCode, &m.EvidenceID,
		&m.Requires, &m.Status, &m.ProposedBy, &m.ApprovedBy, &m.ApplicationID, &entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", notFound("manual match")
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(lines, &m.Lines); err != nil {
		return nil, "", err
	}
	return &m, entity, nil
}

func pendingMatchID(ctx context.Context, q querier, tenantID, receiptID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT match_id::text FROM ar08_manual_matches WHERE tenant_id = $1 AND receipt_id = $2 AND status = 'PENDING' LIMIT 1`, tenantID, receiptID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ── evidence helpers ─────────────────────────────────────────────────────────

func audit(ctx context.Context, tx pgx.Tx, c kit.Caller, aggType, aggID, command, reason, evidence string, before, after any, detail map[string]any) error {
	return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: aggType, AggregateID: aggID, Command: command, ReasonCode: reason, EvidenceID: evidence,
		Before: before, After: after, Detail: detail})
}

func newID() string { return uuid.NewString() }
