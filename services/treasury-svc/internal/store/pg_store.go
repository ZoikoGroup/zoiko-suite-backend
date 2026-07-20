package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := svcmiddleware.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// CreateBankAccount registers a new bank account.
func (s *PgStore) CreateBankAccount(ctx context.Context, acct *domain.BankAccount) error {
	tenantID := tenantFromCtxOrFallback(ctx, acct.TenantID)
	now := time.Now().UTC()

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO bank_accounts (
				bank_account_id, tenant_id, legal_entity_id, account_name,
				masked_account_number, bank_identifier, currency_code, account_status,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, acct.BankAccountID, tenantID, acct.LegalEntityID, acct.AccountName,
			acct.MaskedAccountNumber, acct.BankIdentifier, acct.CurrencyCode, acct.AccountStatus,
			now, now)
		if err != nil {
			return err
		}
		acct.TenantID = tenantID
		acct.CreatedAt = now
		acct.UpdatedAt = now
		return nil
	})
}

// GetBankAccount retrieves a bank account by ID, tenant-scoped.
func (s *PgStore) GetBankAccount(ctx context.Context, bankAccountID string) (*domain.BankAccount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var acct domain.BankAccount
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT bank_account_id, tenant_id, legal_entity_id, account_name,
			       masked_account_number, bank_identifier, currency_code, account_status,
			       created_at, updated_at
			FROM bank_accounts
			WHERE bank_account_id = $1 AND tenant_id = $2
		`, bankAccountID, tenantID)
		return row.Scan(
			&acct.BankAccountID, &acct.TenantID, &acct.LegalEntityID, &acct.AccountName,
			&acct.MaskedAccountNumber, &acct.BankIdentifier, &acct.CurrencyCode, &acct.AccountStatus,
			&acct.CreatedAt, &acct.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &acct, nil
}

// ListBankAccounts lists registered bank accounts scoped by Tenant & LegalEntity.
func (s *PgStore) ListBankAccounts(ctx context.Context, legalEntityID string) ([]domain.BankAccount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.BankAccount
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT bank_account_id, tenant_id, legal_entity_id, account_name,
			       masked_account_number, bank_identifier, currency_code, account_status,
			       created_at, updated_at
			FROM bank_accounts
			WHERE tenant_id = $1 AND ($2 = '' OR legal_entity_id = $2)
			ORDER BY created_at DESC
		`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var acct domain.BankAccount
			if err := rows.Scan(
				&acct.BankAccountID, &acct.TenantID, &acct.LegalEntityID, &acct.AccountName,
				&acct.MaskedAccountNumber, &acct.BankIdentifier, &acct.CurrencyCode, &acct.AccountStatus,
				&acct.CreatedAt, &acct.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, acct)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateBankAccountStatus updates bank account status (ACTIVE, SUSPENDED, CLOSED).
func (s *PgStore) UpdateBankAccountStatus(ctx context.Context, bankAccountID, status string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE bank_accounts
			SET account_status = $1, updated_at = $2
			WHERE bank_account_id = $3 AND tenant_id = $4
		`, status, time.Now().UTC(), bankAccountID, tenantID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return domain.ErrBankAccountNotFound
	}
	return nil
}

// CreateCashBalance inserts a balance trace record.
func (s *PgStore) CreateCashBalance(ctx context.Context, bal *domain.CashBalance) error {
	tenantID := tenantFromCtxOrFallback(ctx, bal.TenantID)
	now := time.Now().UTC()

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO cash_balances (
				balance_id, tenant_id, bank_account_id, ledger_balance,
				available_balance, as_of_timestamp, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, bal.BalanceID, tenantID, bal.BankAccountID, bal.LedgerBalance,
			bal.AvailableBalance, bal.AsOfTimestamp, bal.CorrelationID, now)
		if err != nil {
			return err
		}
		bal.TenantID = tenantID
		bal.CreatedAt = now
		return nil
	})
}

// GetLatestCashBalance retrieves the latest cash balance snapshot for a bank account.
func (s *PgStore) GetLatestCashBalance(ctx context.Context, bankAccountID string) (*domain.CashBalance, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var bal domain.CashBalance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT balance_id, tenant_id, bank_account_id, ledger_balance,
			       available_balance, as_of_timestamp, correlation_id, created_at
			FROM cash_balances
			WHERE bank_account_id = $1 AND tenant_id = $2
			ORDER BY as_of_timestamp DESC
			LIMIT 1
		`, bankAccountID, tenantID)
		return row.Scan(
			&bal.BalanceID, &bal.TenantID, &bal.BankAccountID, &bal.LedgerBalance,
			&bal.AvailableBalance, &bal.AsOfTimestamp, &bal.CorrelationID, &bal.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &bal, nil
}

// SetLiquidityThreshold registers/replaces minimum liquidity requirements.
func (s *PgStore) SetLiquidityThreshold(ctx context.Context, threshold *domain.LiquidityThreshold) error {
	tenantID := tenantFromCtxOrFallback(ctx, threshold.TenantID)
	now := time.Now().UTC()

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// UPSERT threshold
		_, err := tx.Exec(ctx, `
			INSERT INTO liquidity_thresholds (
				threshold_id, tenant_id, legal_entity_id, currency_code,
				minimum_required_balance, escalation_email, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (threshold_id) DO UPDATE SET
				minimum_required_balance = EXCLUDED.minimum_required_balance,
				escalation_email = EXCLUDED.escalation_email
		`, threshold.ThresholdID, tenantID, threshold.LegalEntityID, threshold.CurrencyCode,
			threshold.MinimumRequiredBalance, threshold.EscalationEmail, now)
		if err != nil {
			return err
		}
		threshold.TenantID = tenantID
		threshold.CreatedAt = now
		return nil
	})
}

// GetLiquidityThreshold resolves the minimum required cash threshold for a legal entity & currency.
func (s *PgStore) GetLiquidityThreshold(ctx context.Context, legalEntityID, currencyCode string) (*domain.LiquidityThreshold, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var threshold domain.LiquidityThreshold
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT threshold_id, tenant_id, legal_entity_id, currency_code,
			       minimum_required_balance, escalation_email, created_at
			FROM liquidity_thresholds
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND currency_code = $3
			ORDER BY created_at DESC
			LIMIT 1
		`, tenantID, legalEntityID, currencyCode)
		return row.Scan(
			&threshold.ThresholdID, &threshold.TenantID, &threshold.LegalEntityID, &threshold.CurrencyCode,
			&threshold.MinimumRequiredBalance, &threshold.EscalationEmail, &threshold.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &threshold, nil
}

// ExecuteTransfer processes a cash transfer between two bank accounts under RLS.
func (s *PgStore) ExecuteTransfer(ctx context.Context, srcAcctID, tgtAcctID string, amount float64, currencyCode string, correlationID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	now := time.Now().UTC()

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Retrieve and lock both accounts to prevent concurrency race
		var srcAcct, tgtAcct domain.BankAccount
		err := tx.QueryRow(ctx, `
			SELECT bank_account_id, tenant_id, legal_entity_id, currency_code, account_status
			FROM bank_accounts WHERE bank_account_id = $1 AND tenant_id = $2 FOR UPDATE
		`, srcAcctID, tenantID).Scan(&srcAcct.BankAccountID, &srcAcct.TenantID, &srcAcct.LegalEntityID, &srcAcct.CurrencyCode, &srcAcct.AccountStatus)
		if err != nil {
			return fmt.Errorf("retrieve source account: %w", err)
		}

		err = tx.QueryRow(ctx, `
			SELECT bank_account_id, tenant_id, legal_entity_id, currency_code, account_status
			FROM bank_accounts WHERE bank_account_id = $1 AND tenant_id = $2 FOR UPDATE
		`, tgtAcctID, tenantID).Scan(&tgtAcct.BankAccountID, &tgtAcct.TenantID, &tgtAcct.LegalEntityID, &tgtAcct.CurrencyCode, &tgtAcct.AccountStatus)
		if err != nil {
			return fmt.Errorf("retrieve target account: %w", err)
		}

		if srcAcct.CurrencyCode != currencyCode || tgtAcct.CurrencyCode != currencyCode {
			return fmt.Errorf("currency mismatch: expected %s", currencyCode)
		}
		if srcAcct.AccountStatus != "ACTIVE" || tgtAcct.AccountStatus != "ACTIVE" {
			return fmt.Errorf("accounts must be ACTIVE")
		}

		// Retrieve latest balances
		var srcBal, tgtBal float64
		err = tx.QueryRow(ctx, `
			SELECT available_balance FROM cash_balances 
			WHERE bank_account_id = $1 AND tenant_id = $2 
			ORDER BY as_of_timestamp DESC LIMIT 1
		`, srcAcctID, tenantID).Scan(&srcBal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		err = tx.QueryRow(ctx, `
			SELECT available_balance FROM cash_balances 
			WHERE bank_account_id = $1 AND tenant_id = $2 
			ORDER BY as_of_timestamp DESC LIMIT 1
		`, tgtAcctID, tenantID).Scan(&tgtBal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if srcBal < amount {
			return fmt.Errorf("insufficient funds on source account: available=%f, transfer=%f", srcBal, amount)
		}

		// Write new balance records
		_, err = tx.Exec(ctx, `
			INSERT INTO cash_balances (
				balance_id, tenant_id, bank_account_id, ledger_balance, available_balance, as_of_timestamp, correlation_id, created_at
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
		`, tenantID, srcAcctID, srcBal-amount, srcBal-amount, now, correlationID, now)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO cash_balances (
				balance_id, tenant_id, bank_account_id, ledger_balance, available_balance, as_of_timestamp, correlation_id, created_at
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
		`, tenantID, tgtAcctID, tgtBal+amount, tgtBal+amount, now, correlationID, now)
		if err != nil {
			return err
		}

		return nil
	})
}
