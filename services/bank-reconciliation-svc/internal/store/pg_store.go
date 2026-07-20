// Package store provides the PostgreSQL implementation of
// bank-reconciliation-svc's persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction — the Row-Level Security policy is real and correctly written.
// But every method ALSO filters explicitly by tenant_id in its own SQL,
// rather than relying on RLS alone: this pool connects as a Postgres
// superuser (DB_USER=postgres, same as every other service in this
// platform), and Postgres superusers unconditionally bypass Row-Level
// Security regardless of policy. This was found via genuine CI failures in
// general-ledger-svc and tenant-entity-registry-svc, so this service is
// built with the explicit filter from day one rather than discovering the
// same gap a third time.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
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
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

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

const selectColumns = `
	statement_line_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
	amount, currency_code, bank_reference, status,
	matched_journal_id, matched_by_principal_id, matched_at,
	exception_reason, flagged_by_principal_id, flagged_at,
	correlation_id, created_at
`

func scanLine(row interface{ Scan(...any) error }, l *domain.StatementLine) error {
	var status string
	if err := row.Scan(
		&l.StatementLineID, &l.TenantID, &l.LegalEntityID, &l.BankAccountID, &l.StatementDate,
		&l.Amount, &l.CurrencyCode, &l.BankReference, &status,
		&l.MatchedJournalID, &l.MatchedByPrincipalID, &l.MatchedAt,
		&l.ExceptionReason, &l.FlaggedByPrincipalID, &l.FlaggedAt,
		&l.CorrelationID, &l.CreatedAt,
	); err != nil {
		return err
	}
	l.Status = domain.StatementLineStatus(status)
	return nil
}

// CreateStatementLine inserts an ingested bank statement line in UNMATCHED status.
func (s *PgStore) CreateStatementLine(ctx context.Context, l *domain.StatementLine) error {
	tenantID := tenantFromCtxOrFallback(ctx, l.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, `
			INSERT INTO statement_lines (
				statement_line_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
				amount, currency_code, bank_reference, status, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, l.StatementLineID, l.TenantID, l.LegalEntityID, l.BankAccountID, l.StatementDate,
			l.Amount, l.CurrencyCode, l.BankReference, string(l.Status), l.CorrelationID, now)
		if err != nil {
			return err
		}
		l.CreatedAt = now
		return nil
	})
}

// GetStatementLine returns (nil, nil) if not found — including when the
// caller's tenant scope doesn't match the line's tenant (explicit filter,
// not RLS-only — see package doc).
func (s *PgStore) GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var l domain.StatementLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+selectColumns+`
			FROM statement_lines WHERE statement_line_id = $1 AND tenant_id = $2
		`, statementLineID, tenantID)
		return scanLine(row, &l)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ListStatementLines returns statement lines matching the given filter
// (tenant_id is required; the others are optional).
func (s *PgStore) ListStatementLines(ctx context.Context, filter domain.ListStatementLinesFilter) ([]domain.StatementLine, error) {
	var out []domain.StatementLine
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+selectColumns+`
			FROM statement_lines
			WHERE tenant_id = $1
			  AND ($2 = '' OR bank_account_id::text = $2)
			  AND ($3 = '' OR statement_date = $3::date)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC
		`, filter.TenantID, filter.BankAccountID, filter.StatementDate, filter.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.StatementLine
			if err := scanLine(rows, &l); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// MatchStatementLine transitions a line from UNMATCHED or EXCEPTION to
// MATCHED, atomically — the fromStatus check, the transition, and the
// tenant scope are one statement, no separate read, no race window. The
// journal itself must already have been verified against general-ledger-svc
// by the caller (internal/handler) before this is invoked; this method only
// persists the outcome.
func (s *PgStore) MatchStatementLine(ctx context.Context, tenantID, statementLineID, journalID, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'MATCHED', matched_journal_id = $1, matched_by_principal_id = $2, matched_at = $3
			WHERE statement_line_id = $4 AND status IN ('UNMATCHED', 'EXCEPTION') AND tenant_id = $5
		`, journalID, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
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
		return domain.ErrInvalidTransition
	}
	return nil
}

// FlagException transitions a line from UNMATCHED to EXCEPTION, atomically.
func (s *PgStore) FlagException(ctx context.Context, tenantID, statementLineID, reason, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE statement_lines
			SET status = 'EXCEPTION', exception_reason = $1, flagged_by_principal_id = $2, flagged_at = $3
			WHERE statement_line_id = $4 AND status = 'UNMATCHED' AND tenant_id = $5
		`, reason, actorPrincipalID, time.Now().UTC(), statementLineID, tenantID)
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
		return domain.ErrInvalidTransition
	}
	return nil
}

// CountUnmatched returns how many lines are still UNMATCHED for the given
// bank account + statement date — used to decide whether the statement can
// be marked complete. tenant_id is a mandatory, explicit filter argument
// (not derived from context), matching ListStatementLines' pattern.
func (s *PgStore) CountUnmatched(ctx context.Context, tenantID, bankAccountID, statementDate string) (int, error) {
	var count int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM statement_lines
			WHERE tenant_id = $1 AND bank_account_id = $2::uuid AND statement_date = $3::date AND status = 'UNMATCHED'
		`, tenantID, bankAccountID, statementDate).Scan(&count)
	})
	return count, err
}
