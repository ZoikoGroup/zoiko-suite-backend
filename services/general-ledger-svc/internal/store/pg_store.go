// Package store provides the PostgreSQL implementation of general-ledger-svc's
// persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction before running any query, enforcing the Row-Level Security
// policies defined in deployments/migrations/000001_initial_schema.up.sql —
// same doctrine as tenant-entity-registry-svc.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
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

// CreateJournal inserts a journal header (status PENDING) and all of its
// lines in a single transaction. Balance validation (sum debits == sum
// credits) happens at ValidateJournal, not here — PENDING is deliberately
// allowed to be unbalanced, matching the Tri-Phase Commit spec's intent that
// Pending is a draft state.
func (s *PgStore) CreateJournal(ctx context.Context, h *domain.JournalHeader, lines []domain.JournalLine) error {
	tenantID := tenantFromCtxOrFallback(ctx, h.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_headers (
				journal_id, tenant_id, legal_entity_id, fiscal_period, status,
				description, created_by_principal_id, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, h.JournalID, h.TenantID, h.LegalEntityID, h.FiscalPeriod, string(h.Status),
			h.Description, h.CreatedByPrincipalID, h.CorrelationID, now)
		if err != nil {
			return err
		}
		h.CreatedAt = now

		for i := range lines {
			lines[i].JournalLineID = uuid.NewString()
			lines[i].JournalID = h.JournalID
			lines[i].LineNumber = i + 1
			_, err := tx.Exec(ctx, `
				INSERT INTO journal_lines (
					journal_line_id, journal_id, tenant_id, line_number,
					account_code, debit_amount, credit_amount, description
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, lines[i].JournalLineID, h.JournalID, h.TenantID, lines[i].LineNumber,
				lines[i].AccountCode, lines[i].DebitAmount, lines[i].CreditAmount, lines[i].Description)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// GetJournal returns a journal header plus its lines. Returns (nil, nil, nil)
// if not found.
func (s *PgStore) GetJournal(ctx context.Context, journalID string) (*domain.JournalHeader, []domain.JournalLine, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)

	var h domain.JournalHeader
	var status string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT journal_id, tenant_id, legal_entity_id, fiscal_period, status,
			       reversal_of_journal_id, description, created_by_principal_id,
			       validated_by_principal_id, posted_by_principal_id, reversed_by_principal_id,
			       correlation_id, created_at, validated_at, posted_at, reversed_at
			FROM journal_headers WHERE journal_id = $1
		`, journalID)
		if err := row.Scan(
			&h.JournalID, &h.TenantID, &h.LegalEntityID, &h.FiscalPeriod, &status,
			&h.ReversalOfJournalID, &h.Description, &h.CreatedByPrincipalID,
			&h.ValidatedByPrincipalID, &h.PostedByPrincipalID, &h.ReversedByPrincipalID,
			&h.CorrelationID, &h.CreatedAt, &h.ValidatedAt, &h.PostedAt, &h.ReversedAt,
		); err != nil {
			return err
		}
		h.Status = domain.JournalStatus(status)
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	lines, err := s.listLines(ctx, h.TenantID, journalID)
	if err != nil {
		return nil, nil, err
	}
	return &h, lines, nil
}

func (s *PgStore) listLines(ctx context.Context, tenantID, journalID string) ([]domain.JournalLine, error) {
	var lines []domain.JournalLine
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT journal_line_id, journal_id, line_number, account_code,
			       debit_amount, credit_amount, COALESCE(description, '')
			FROM journal_lines WHERE journal_id = $1 ORDER BY line_number ASC
		`, journalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.JournalLine
			if err := rows.Scan(&l.JournalLineID, &l.JournalID, &l.LineNumber, &l.AccountCode,
				&l.DebitAmount, &l.CreditAmount, &l.Description); err != nil {
				return err
			}
			lines = append(lines, l)
		}
		return rows.Err()
	})
	return lines, err
}

// ListJournals returns journal headers matching the given filter (tenant_id
// is required; the others are optional).
func (s *PgStore) ListJournals(ctx context.Context, filter domain.ListJournalsFilter) ([]domain.JournalHeader, error) {
	var out []domain.JournalHeader
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		query := `
			SELECT journal_id, tenant_id, legal_entity_id, fiscal_period, status,
			       reversal_of_journal_id, description, created_by_principal_id,
			       validated_by_principal_id, posted_by_principal_id, reversed_by_principal_id,
			       correlation_id, created_at, validated_at, posted_at, reversed_at
			FROM journal_headers
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id::text = $2)
			  AND ($3 = '' OR fiscal_period = $3)
			  AND ($4 = '' OR status = $4)
			ORDER BY created_at DESC
		`
		rows, err := tx.Query(ctx, query, filter.TenantID, filter.LegalEntityID, filter.FiscalPeriod, filter.Status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h domain.JournalHeader
			var status string
			if err := rows.Scan(
				&h.JournalID, &h.TenantID, &h.LegalEntityID, &h.FiscalPeriod, &status,
				&h.ReversalOfJournalID, &h.Description, &h.CreatedByPrincipalID,
				&h.ValidatedByPrincipalID, &h.PostedByPrincipalID, &h.ReversedByPrincipalID,
				&h.CorrelationID, &h.CreatedAt, &h.ValidatedAt, &h.PostedAt, &h.ReversedAt,
			); err != nil {
				return err
			}
			h.Status = domain.JournalStatus(status)
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// TransitionJournal atomically moves a journal from fromStatus to toStatus,
// stamping the actor and timestamp column appropriate to toStatus. Uses
// WHERE status = $fromStatus so the transition and the state-machine check
// are one atomic UPDATE — no separate read, no race window (same pattern as
// tenant-entity-registry-svc's TransitionEntityStatus). Returns
// domain.ErrInvalidTransition if zero rows were affected (either the journal
// doesn't exist or wasn't in fromStatus).
func (s *PgStore) TransitionJournal(ctx context.Context, tenantID, journalID string, fromStatus, toStatus domain.JournalStatus, actorPrincipalID string) error {
	actorColumn, timeColumn := transitionColumns(toStatus)
	query := fmt.Sprintf(`
		UPDATE journal_headers
		SET status = $1, %s = $2, %s = $3
		WHERE journal_id = $4 AND status = $5
	`, actorColumn, timeColumn)

	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, string(toStatus), actorPrincipalID, time.Now().UTC(), journalID, string(fromStatus))
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

func transitionColumns(to domain.JournalStatus) (actorColumn, timeColumn string) {
	switch to {
	case domain.JournalStatusValidated:
		return "validated_by_principal_id", "validated_at"
	case domain.JournalStatusFinalized:
		return "posted_by_principal_id", "posted_at"
	case domain.JournalStatusReversed:
		return "reversed_by_principal_id", "reversed_at"
	default:
		return "posted_by_principal_id", "posted_at"
	}
}

// SumLines returns the total debit and credit amounts for a journal's lines —
// used by the service layer to enforce the double-entry balance invariant
// before allowing a PENDING -> VALIDATED transition.
func (s *PgStore) SumLines(ctx context.Context, tenantID, journalID string) (debitTotal, creditTotal float64, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(debit_amount), 0), COALESCE(SUM(credit_amount), 0)
			FROM journal_lines WHERE journal_id = $1
		`, journalID)
		return row.Scan(&debitTotal, &creditTotal)
	})
	return debitTotal, creditTotal, err
}
