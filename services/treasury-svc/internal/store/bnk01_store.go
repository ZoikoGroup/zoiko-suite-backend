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
		return scanBankAccount(row, &acct)
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
		return scanBankAccount(row, &acct)
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
		return scanBankAccount(row, &acct)
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
		return scanBankAccount(row, &acct)
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
		return scanBankAccount(row, &acct)
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
		return scanBankAccount(row, &acct)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.notFoundOrInvalidErr(ctx, p.BankAccountID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &acct, nil
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
