package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.SnapshotRepo = (*KernelRepo)(nil)

// Balances implements kernel.SnapshotRepo: the balances of a book from one repeatable-read snapshot of the ledger.
func (r *KernelRepo) Balances(ctx context.Context, tenant, entity, bookCode string, asOf, periodStart time.Time, upToSeq int64) (int64, []kernel.SnapshotLine, error) {
	if _, err := uuid.Parse(tenant); err != nil {
		return 0, nil, kernel.ErrNotFound
	}
	t, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck
	if _, err := t.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return 0, nil, err
	}
	wm := upToSeq
	if wm == 0 {
		if err := t.QueryRow(ctx, `SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, tenant, entity).Scan(&wm); err != nil {
			return 0, nil, kernelErr(err)
		}
	}
	rows, err := t.Query(ctx, `
		SELECT account_code, sum(debit_amount)::text, sum(credit_amount)::text, (sum(debit_amount) - sum(credit_amount))::text,
		       COALESCE(sum(debit_amount) FILTER (WHERE posting_date >= $5::date), 0)::text, COALESCE(sum(credit_amount) FILTER (WHERE posting_date >= $5::date), 0)::text
		FROM ledger_entries
		WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = $3 AND posting_date <= $4::date AND entry_seq <= $6
		GROUP BY account_code ORDER BY account_code`, tenant, entity, bookCode, asOf, periodStart, wm)
	if err != nil {
		return 0, nil, kernelErr(err)
	}
	defer rows.Close()
	out := []kernel.SnapshotLine{}
	for rows.Next() {
		var l kernel.SnapshotLine
		if err := rows.Scan(&l.AccountCode, &l.Debit, &l.Credit, &l.Net, &l.PeriodDebit, &l.PeriodCredit); err != nil {
			return 0, nil, err
		}
		out = append(out, l)
	}
	return wm, out, kernelErr(rows.Err())
}

// InsertSnapshot implements kernel.SnapshotRepo.
func (r *KernelRepo) InsertSnapshot(ctx context.Context, s *kernel.BalanceSnapshot, out kernel.OutEvent) error {
	return r.tx(ctx, s.TenantID, func(t pgx.Tx) error {
		lines, _ := json.Marshal(s.Lines)
		if _, e := t.Exec(ctx, `INSERT INTO balance_snapshots (snapshot_id, tenant_id, legal_entity_id, accounting_book_id, fiscal_period, as_of, purpose, ledger_watermark, lines, account_count,
				total_debit, total_credit, content_hash, signature, key_id, signed_by, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6::date, $7, $8, $9::jsonb, $10, $11::numeric, $12::numeric, $13, $14, $15, $16, $17)`,
			s.SnapshotID, s.TenantID, s.LegalEntityID, s.BookID, s.FiscalPeriod, s.AsOf, s.Purpose, s.LedgerWatermark, string(lines), s.AccountCount, s.TotalDebit, s.TotalCredit,
			s.ContentHash, s.Signature, s.KeyID, s.SignedBy, s.CreatedAt); e != nil {
			return e
		}
		return r.enqueue(ctx, t, s.TenantID, out)
	})
}

const snapCols = `snapshot_id::text, tenant_id::text, legal_entity_id::text, accounting_book_id::text, fiscal_period, as_of::text, purpose, ledger_watermark, lines, account_count,
	total_debit::text, total_credit::text, content_hash, signature, key_id, signed_by, created_at`

func scanSnapshot(row pgx.Row, s *kernel.BalanceSnapshot) error {
	var lines []byte
	if err := row.Scan(&s.SnapshotID, &s.TenantID, &s.LegalEntityID, &s.BookID, &s.FiscalPeriod, &s.AsOf, &s.Purpose, &s.LedgerWatermark, &lines, &s.AccountCount,
		&s.TotalDebit, &s.TotalCredit, &s.ContentHash, &s.Signature, &s.KeyID, &s.SignedBy, &s.CreatedAt); err != nil {
		return err
	}
	return json.Unmarshal(lines, &s.Lines)
}

// GetSnapshot implements kernel.SnapshotRepo.
func (r *KernelRepo) GetSnapshot(ctx context.Context, tenant, id string) (*kernel.BalanceSnapshot, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kernel.ErrNotFound
	}
	var s kernel.BalanceSnapshot
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return scanSnapshot(t.QueryRow(ctx, `SELECT `+snapCols+` FROM balance_snapshots WHERE tenant_id = $1::uuid AND snapshot_id = $2::uuid`, tenant, id), &s)
	})
	if err != nil {
		return nil, kernelErr(err)
	}
	return &s, nil
}

// ListSnapshots implements kernel.SnapshotRepo.
func (r *KernelRepo) ListSnapshots(ctx context.Context, tenant, entity, bookID, period string) ([]kernel.BalanceSnapshot, error) {
	out := []kernel.BalanceSnapshot{}
	for _, id := range []string{entity, bookID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return out, nil
			}
		}
	}
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		rows, e := t.Query(ctx, `SELECT `+snapCols+` FROM balance_snapshots WHERE tenant_id = $1::uuid AND ($2 = '' OR legal_entity_id::text = $2) AND ($3 = '' OR accounting_book_id::text = $3)
			AND ($4 = '' OR fiscal_period = $4) ORDER BY created_at DESC LIMIT 200`, tenant, entity, bookID, period)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var s kernel.BalanceSnapshot
			if e := scanSnapshot(rows, &s); e != nil {
				return e
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, kernelErr(err)
}

var _ kernel.PendingRepo = (*KernelRepo)(nil)

// PendingJournals implements kernel.PendingRepo: the journals of an entity and period that are neither posted nor withdrawn, the
// review queue a close must have cleared.
func (r *KernelRepo) PendingJournals(ctx context.Context, tenant, entity, period string) (int, error) {
	var n int
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND fiscal_period = $3
			AND status IN ('PENDING', 'VALIDATED') AND approval_status <> 'CANCELLED'`, tenant, entity, period).Scan(&n)
	})
	return n, kernelErr(err)
}
