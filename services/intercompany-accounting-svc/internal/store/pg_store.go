package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *PgStore) CreateEntry(ctx context.Context, entry *domain.IntercompanyEntry) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO intercompany_entries (
				intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, target_journal_id, amount, currency_code, match_status,
				mismatch_reason, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, entry.IntercompanyEntryID, tenantID, entry.SourceLegalEntityID, entry.TargetLegalEntityID,
			entry.SourceJournalID, entry.TargetJournalID, entry.Amount, entry.CurrencyCode,
			entry.MatchStatus, entry.MismatchReason, entry.CreatedAt, entry.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var entry domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
			       source_journal_id, target_journal_id, amount, currency_code, match_status,
			       mismatch_reason, created_at, updated_at
			FROM intercompany_entries
			WHERE intercompany_entry_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&entry.IntercompanyEntryID, &entry.TenantID, &entry.SourceLegalEntityID, &entry.TargetLegalEntityID,
			&entry.SourceJournalID, &entry.TargetJournalID, &entry.Amount, &entry.CurrencyCode, &entry.MatchStatus,
			&entry.MismatchReason, &entry.CreatedAt, &entry.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func (s *PgStore) ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
			       source_journal_id, target_journal_id, amount, currency_code, match_status,
			       mismatch_reason, created_at, updated_at
			FROM intercompany_entries
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if sourceEntityID != "" {
			args = append(args, sourceEntityID)
			query += fmt.Sprintf(" AND source_legal_entity_id = $%d", len(args))
		}
		if targetEntityID != "" {
			args = append(args, targetEntityID)
			query += fmt.Sprintf(" AND target_legal_entity_id = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var entry domain.IntercompanyEntry
			if err := rows.Scan(
				&entry.IntercompanyEntryID, &entry.TenantID, &entry.SourceLegalEntityID, &entry.TargetLegalEntityID,
				&entry.SourceJournalID, &entry.TargetJournalID, &entry.Amount, &entry.CurrencyCode, &entry.MatchStatus,
				&entry.MismatchReason, &entry.CreatedAt, &entry.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateMatch(ctx context.Context, id, targetJournalID, matchStatus string, mismatchReason *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET target_journal_id = $1, match_status = $2, mismatch_reason = $3, updated_at = $4
			WHERE intercompany_entry_id = $5 AND tenant_id = $6
		`, targetJournalID, matchStatus, mismatchReason, now, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrEntryNotFound
		}
		return nil
	})
}
