// BNK-09 Treasury Transfer persistence — see migration
// 000004_add_bnk09_treasury_transfer for the schema this operates on.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/treasury-svc/internal/domain"
)

const treasuryTransferColumns = `
	transfer_id, tenant_id, source_bank_account_id, target_bank_account_id, amount, currency_code, correlation_id, is_cross_entity,
	protected_field_hash, status, maker_principal_id, checker_principal_id, reject_reason, payment_attempt_id, source_journal_id, intercompany_entry_id,
	created_at, updated_at`

func scanTreasuryTransfer(row pgx.Row, t *domain.TreasuryTransfer) error {
	return row.Scan(&t.TransferID, &t.TenantID, &t.SourceBankAccountID, &t.TargetBankAccountID, &t.Amount, &t.CurrencyCode, &t.CorrelationID, &t.IsCrossEntity,
		&t.ProtectedFieldHash, &t.Status, &t.MakerPrincipalID, &t.CheckerPrincipalID, &t.RejectReason, &t.PaymentAttemptID, &t.SourceJournalID, &t.IntercompanyEntryID,
		&t.CreatedAt, &t.UpdatedAt)
}

// protectedFieldHash hashes the fields a treasury transfer's approval must
// stay honest about — recomputed at ApproveTreasuryTransfer time and
// compared against the value captured at creation.
func protectedFieldHash(amount float64, currencyCode, srcAcctID, tgtAcctID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%.4f|%s|%s|%s", amount, currencyCode, srcAcctID, tgtAcctID)))
	return hex.EncodeToString(sum[:])
}

// CreateTreasuryTransfer is BNK-09's maker entry point. Idempotent on
// (tenant_id, correlation_id): a retried create returns the original
// transfer rather than creating a second one.
func (s *PgStore) CreateTreasuryTransfer(ctx context.Context, p domain.CreateTreasuryTransferParams) (*domain.TreasuryTransfer, bool, error) {
	created := false
	var t domain.TreasuryTransfer
	hash := protectedFieldHash(p.Amount, p.CurrencyCode, p.SourceBankAccountID, p.TargetBankAccountID)
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO treasury_transfers (
				tenant_id, source_bank_account_id, target_bank_account_id, amount, currency_code, correlation_id, is_cross_entity,
				protected_field_hash, maker_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id <> '' DO NOTHING
			RETURNING `+treasuryTransferColumns,
			p.TenantID, p.SourceBankAccountID, p.TargetBankAccountID, p.Amount, p.CurrencyCode, p.CorrelationID, p.IsCrossEntity,
			hash, p.MakerPrincipalID)
		err := scanTreasuryTransfer(row, &t)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return scanTreasuryTransfer(tx.QueryRow(ctx, `SELECT `+treasuryTransferColumns+` FROM treasury_transfers WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID), &t)
	})
	if err != nil {
		return nil, false, err
	}
	return &t, created, nil
}

func (s *PgStore) GetTreasuryTransfer(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTreasuryTransfer(tx.QueryRow(ctx, `SELECT `+treasuryTransferColumns+` FROM treasury_transfers WHERE transfer_id=$1 AND tenant_id=$2`, transferID, tenantID), &t)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTransferNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) notFoundOrInvalidTransfer(ctx context.Context, tenantID, transferID string) error {
	if _, err := s.GetTreasuryTransfer(ctx, tenantID, transferID); err != nil {
		return err
	}
	return domain.ErrInvalidTransferTransition
}

// ApproveTreasuryTransfer is BNK-09's checker entry point. Enforces two
// real controls before the CAS update ever runs: the checker must differ
// from the maker (also backed by migration 000004's own trigger), and the
// transfer's protected fields must still hash to what they were at
// creation — a mismatch means something altered amount/currency/accounts
// between creation and approval, which must invalidate the approval
// rather than silently proceed.
func (s *PgStore) ApproveTreasuryTransfer(ctx context.Context, p domain.ApproveTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID == p.CheckerPrincipalID {
		return nil, domain.ErrTransferSelfApproval
	}
	if protectedFieldHash(existing.Amount, existing.CurrencyCode, existing.SourceBankAccountID, existing.TargetBankAccountID) != existing.ProtectedFieldHash {
		return nil, domain.ErrTransferHashMismatch
	}

	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, checker_principal_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='PENDING_APPROVAL'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferApproved, p.CheckerPrincipalID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) RejectTreasuryTransfer(ctx context.Context, p domain.RejectTreasuryTransferParams) (*domain.TreasuryTransfer, error) {
	existing, err := s.GetTreasuryTransfer(ctx, p.TenantID, p.TransferID)
	if err != nil {
		return nil, err
	}
	if existing.MakerPrincipalID == p.CheckerPrincipalID {
		return nil, domain.ErrTransferSelfApproval
	}
	var t domain.TreasuryTransfer
	err = s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, checker_principal_id=$4, reject_reason=$5, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='PENDING_APPROVAL'
			RETURNING `+treasuryTransferColumns,
			p.TransferID, p.TenantID, domain.TransferRejected, p.CheckerPrincipalID, p.Reason)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, p.TenantID, p.TransferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTransferSubmitted, MarkTransferLedgerPosted,
// MarkTransferIntercompanyPaired and MarkTransferCompleted are the saga's
// individual step transitions, each a narrow CAS update driven by
// ExecuteTreasuryTransfer's resumable handler loop — see
// internal/handler/bnk09_handler.go.

func (s *PgStore) MarkTransferSubmitted(ctx context.Context, tenantID, transferID, paymentAttemptID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, payment_attempt_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='APPROVED'
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferSubmitted, paymentAttemptID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) MarkTransferLedgerPosted(ctx context.Context, tenantID, transferID, sourceJournalID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, source_journal_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='SUBMITTED'
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferLedgerPosted, sourceJournalID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) MarkTransferIntercompanyPaired(ctx context.Context, tenantID, transferID, intercompanyEntryID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, intercompany_entry_id=$4, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status='LEDGER_POSTED'
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferIntercompanyPaired, intercompanyEntryID)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTransferCompleted accepts either SUBMITTED (the same-entity path,
// which never touches general-ledger-svc/intercompany-accounting-svc at
// all) or INTERCOMPANY_PAIRED (the cross-entity path) as its predecessor
// state — ExecuteTreasuryTransfer picks the right one to call from based
// on IsCrossEntity.
func (s *PgStore) MarkTransferCompleted(ctx context.Context, tenantID, transferID string) (*domain.TreasuryTransfer, error) {
	var t domain.TreasuryTransfer
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE treasury_transfers SET status=$3, updated_at=now()
			WHERE transfer_id=$1 AND tenant_id=$2 AND status IN ('SUBMITTED','INTERCOMPANY_PAIRED')
			RETURNING `+treasuryTransferColumns,
			transferID, tenantID, domain.TransferCompleted)
		return scanTreasuryTransfer(row, &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidTransfer(ctx, tenantID, transferID)
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
