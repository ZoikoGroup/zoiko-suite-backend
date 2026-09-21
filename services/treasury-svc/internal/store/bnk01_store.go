// BNK-01's own persistence — ownership verification, metadata amendment,
// operational-status lifecycle and identifier-token rotation. Kept
// separate from pg_store.go's original bank-account CRUD to keep this
// capability's own scope self-contained, same pattern used elsewhere in
// this build for a service that gains a new capability without becoming
// a new service.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// VerifyBankAccountOwnership works in two modes depending on current
// status: from DRAFT/PENDING_VERIFICATION callers typically follow up by
// also calling a status transition explicitly (this service kept
// RegisterBankAccount's create-time default at ACTIVE for backward
// compatibility — see migration 000003's doc comment), so this method
// itself never changes account_status. It only ever records evidence,
// superseding whatever evidence row previously stood — the concrete form
// of "ownership verification state is orthogonal and versioned."
func (s *PgStore) VerifyBankAccountOwnership(ctx context.Context, p domain.VerifyOwnershipParams) (*domain.OwnershipEvidence, error) {
	var evidence *domain.OwnershipEvidence
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bank_accounts WHERE bank_account_id = $1 AND tenant_id = $2)`,
			p.BankAccountID, p.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrBankAccountNotFound
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO bank_account_ownership_evidence (evidence_id, bank_account_id, tenant_id, verification_method, evidence_ref, verified_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING evidence_id, bank_account_id, tenant_id, verification_method, evidence_ref, verified_by_principal_id, verified_at, superseded_by`,
			uuid.New().String(), p.BankAccountID, p.TenantID, p.VerificationMethod, p.EvidenceRef, p.VerifiedByPrincipalID)
		var e domain.OwnershipEvidence
		if err := row.Scan(&e.EvidenceID, &e.BankAccountID, &e.TenantID, &e.VerificationMethod, &e.EvidenceRef, &e.VerifiedByPrincipalID, &e.VerifiedAt, &e.SupersededBy); err != nil {
			return err
		}
		evidence = &e

		_, err := tx.Exec(ctx, `
			UPDATE bank_account_ownership_evidence SET superseded_by = $1
			WHERE bank_account_id = $2 AND evidence_id <> $1 AND superseded_by IS NULL`,
			e.EvidenceID, p.BankAccountID)
		return err
	})
	if errors.Is(err, domain.ErrBankAccountNotFound) {
		return nil, domain.ErrBankAccountNotFound
	}
	if err != nil {
		s.log.Error("pg VerifyBankAccountOwnership failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return evidence, nil
}

func (s *PgStore) ListOwnershipEvidence(ctx context.Context, tenantID, bankAccountID string) ([]domain.OwnershipEvidence, error) {
	var out []domain.OwnershipEvidence
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, bank_account_id, tenant_id, verification_method, evidence_ref, verified_by_principal_id, verified_at, superseded_by
			FROM bank_account_ownership_evidence WHERE tenant_id = $1 AND bank_account_id = $2 ORDER BY verified_at DESC`, tenantID, bankAccountID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.OwnershipEvidence
			if err := rows.Scan(&e.EvidenceID, &e.BankAccountID, &e.TenantID, &e.VerificationMethod, &e.EvidenceRef, &e.VerifiedByPrincipalID, &e.VerifiedAt, &e.SupersededBy); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// GetBankAccountAsOf returns the account's mutable-field values as they
// stood at asOf — the history row whose effective_at is the latest one
// not after asOf. Falls back to the live bank_accounts row when asOf is
// now-or-later, since the most recent history row and the live row are
// the same snapshot by construction (recordAccountHistory runs in the
// same transaction as every mutation). Returns (nil, nil) if the account
// doesn't exist, or existed but not yet as of asOf (a genuinely later
// creation), mirroring GetBankAccount's own not-found convention.
func (s *PgStore) GetBankAccountAsOf(ctx context.Context, tenantID, bankAccountID string, asOf time.Time) (*domain.AccountHistoryEntry, error) {
	var e domain.AccountHistoryEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT history_id, bank_account_id, tenant_id, account_name, masked_account_number, bank_identifier,
			       account_status, branch_ref, country, account_type, requested_operational_use, token_version,
			       changed_by_principal_id, effective_at, superseded_by
			FROM bank_account_history
			WHERE bank_account_id = $1 AND tenant_id = $2 AND effective_at <= $3
			ORDER BY effective_at DESC
			LIMIT 1`,
			bankAccountID, tenantID, asOf)
		return row.Scan(&e.HistoryID, &e.BankAccountID, &e.TenantID, &e.AccountName, &e.MaskedAccountNumber, &e.BankIdentifier,
			&e.AccountStatus, &e.BranchRef, &e.Country, &e.AccountType, &e.RequestedOperationalUse, &e.TokenVersion,
			&e.ChangedByPrincipalID, &e.EffectiveAt, &e.SupersededBy)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &e, nil
}

// IsOwnershipVerified is the derived fact BNK-06/BNK-09 (or any future
// caller) should check before treating an account as usable for
// protected outbound use — the doc's own negative path "unverified
// account used for payment." True iff at least one non-superseded
// evidence row exists.
func (s *PgStore) IsOwnershipVerified(ctx context.Context, tenantID, bankAccountID string) (bool, error) {
	var verified bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM bank_account_ownership_evidence WHERE tenant_id = $1 AND bank_account_id = $2 AND superseded_by IS NULL)`,
			tenantID, bankAccountID).Scan(&verified)
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return verified, nil
}

func (s *PgStore) AmendBankAccountMetadata(ctx context.Context, p domain.AmendBankAccountMetadataParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts SET account_name = $3, branch_ref = $4, bank_identifier = $5, country = $6, account_type = $7, updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status IN ('DRAFT','PENDING_VERIFICATION','ACTIVE','SUSPENDED')
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID, p.AccountName, p.BranchRef, p.BankIdentifier, p.Country, p.AccountType,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

func (s *PgStore) ChangeOperationalUse(ctx context.Context, p domain.ChangeOperationalUseParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts SET requested_operational_use = $3, updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status IN ('DRAFT','PENDING_VERIFICATION','ACTIVE','SUSPENDED')
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID, p.RequestedOperationalUse,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

func (s *PgStore) SuspendBankAccount(ctx context.Context, p domain.SuspendAccountParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts SET account_status = 'SUSPENDED', suspend_reason = $3, updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status = 'ACTIVE'
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID, p.Reason,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

func (s *PgStore) ReactivateBankAccount(ctx context.Context, p domain.ReactivateAccountParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts SET account_status = 'ACTIVE', suspend_reason = '', updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status = 'SUSPENDED'
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

func (s *PgStore) CloseBankAccount(ctx context.Context, p domain.CloseAccountParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts SET account_status = 'CLOSED', close_reason = $3, updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status IN ('DRAFT','PENDING_VERIFICATION','ACTIVE','SUSPENDED')
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID, p.Reason,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

// RotateAccountIdentifierToken is the ONLY legitimate path that can
// change masked_account_number/bank_identifier — migration 000003's own
// trigger requires token_version to increment by exactly 1 whenever
// either field changes, which this query does atomically with the CAS
// status check.
func (s *PgStore) RotateAccountIdentifierToken(ctx context.Context, p domain.RotateAccountTokenParams) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE bank_accounts
			SET masked_account_number = $3, bank_identifier = $4, token_version = token_version + 1, updated_at = now()
			WHERE bank_account_id = $1 AND tenant_id = $2 AND account_status <> 'CLOSED'
			RETURNING `+bankAccountColumns,
			p.BankAccountID, p.TenantID, p.NewMaskedAccountNumber, p.NewBankIdentifier,
		)
		if err := scanBankAccount(row, &acct); err != nil {
			return err
		}
		return s.recordAccountHistory(ctx, tx, &acct, p.ActorPrincipalID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
}

// recordAccountHistory inserts a new append-only snapshot of acct's
// current mutable fields and supersedes whatever history row previously
// stood for this account — the same insert-then-supersede pattern
// VerifyBankAccountOwnership already uses for ownership evidence. Called
// in the same transaction as the UPDATE that produced acct, by every
// BNK-01 command that can change a versioned field.
func (s *PgStore) recordAccountHistory(ctx context.Context, tx pgx.Tx, acct *domain.BankAccount, actorPrincipalID string) error {
	var historyID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO bank_account_history (
			history_id, bank_account_id, tenant_id, account_name, masked_account_number, bank_identifier,
			account_status, branch_ref, country, account_type, requested_operational_use, token_version,
			changed_by_principal_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING history_id`,
		uuid.New().String(), acct.BankAccountID, acct.TenantID, acct.AccountName, acct.MaskedAccountNumber, acct.BankIdentifier,
		acct.AccountStatus, acct.BranchRef, acct.Country, acct.AccountType, acct.RequestedOperationalUse, acct.TokenVersion,
		actorPrincipalID,
	).Scan(&historyID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE bank_account_history SET superseded_by = $1
		WHERE bank_account_id = $2 AND history_id <> $1 AND superseded_by IS NULL`,
		historyID, acct.BankAccountID)
	return err
}

// notFoundOrInvalidErr mirrors notFoundOrInvalid but returns the actual
// sentinel the handler expects (ErrBankAccountNotFound vs
// ErrInvalidTransition) rather than a nil-signals-not-found convention —
// used by every CAS method in this file.
func (s *PgStore) notFoundOrInvalidErr(ctx context.Context, bankAccountID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	acct, err := s.withRLSGetAccount(ctx, tenantID, bankAccountID)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if acct == nil {
		return domain.ErrBankAccountNotFound
	}
	return domain.ErrInvalidTransition
}

func (s *PgStore) withRLSGetAccount(ctx context.Context, tenantID, bankAccountID string) (*domain.BankAccount, error) {
	var acct domain.BankAccount
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+bankAccountColumns+` FROM bank_accounts WHERE bank_account_id = $1 AND tenant_id = $2`, bankAccountID, tenantID)
		return scanBankAccount(row, &acct)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &acct, nil
}
