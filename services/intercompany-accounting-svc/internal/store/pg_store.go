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

// CreateEntry inserts an intercompany entry in UNMATCHED status.
//
// Idempotent on (tenant_id, source_journal_id): a retried call (e.g. a
// client timeout on a POST that actually succeeded server-side) hits the
// unique index added in 000002 and resolves to the ORIGINAL entry —
// mutating *entry in place to reflect it — rather than creating a
// duplicate. Returns created=false when the row already existed.
func (s *PgStore) CreateEntry(ctx context.Context, entry *domain.IntercompanyEntry) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO intercompany_entries (
				intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_id, target_journal_id, amount, currency_code, match_status,
				mismatch_reason, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, source_journal_id) DO NOTHING
		`, entry.IntercompanyEntryID, tenantID, entry.SourceLegalEntityID, entry.TargetLegalEntityID,
			entry.SourceJournalID, entry.TargetJournalID, entry.Amount, entry.CurrencyCode,
			entry.MatchStatus, entry.MismatchReason, entry.CreatedAt, entry.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			row := tx.QueryRow(ctx, `
				SELECT intercompany_entry_id, source_legal_entity_id, target_legal_entity_id, target_journal_id,
				       amount, currency_code, match_status, mismatch_reason, created_at, updated_at
				FROM intercompany_entries WHERE tenant_id = $1 AND source_journal_id = $2
			`, tenantID, entry.SourceJournalID)
			if err := row.Scan(
				&entry.IntercompanyEntryID, &entry.SourceLegalEntityID, &entry.TargetLegalEntityID, &entry.TargetJournalID,
				&entry.Amount, &entry.CurrencyCode, &entry.MatchStatus, &entry.MismatchReason, &entry.CreatedAt, &entry.UpdatedAt,
			); err != nil {
				return err
			}
			created = false
			return nil
		}
		created = true
		return nil
	})
	return created, err
}

const intercompanyEntryColumns = `
	intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
	source_journal_id, target_journal_id, amount, currency_code, match_status,
	mismatch_reason, acknowledged_at, acknowledged_by_principal_id,
	disputed_at, disputed_by_principal_id, dispute_reason,
	resolved_at, resolved_by_principal_id, resolution_note,
	created_at, updated_at`

func scanIntercompanyEntry(row pgx.Row, entry *domain.IntercompanyEntry) error {
	return row.Scan(
		&entry.IntercompanyEntryID, &entry.TenantID, &entry.SourceLegalEntityID, &entry.TargetLegalEntityID,
		&entry.SourceJournalID, &entry.TargetJournalID, &entry.Amount, &entry.CurrencyCode, &entry.MatchStatus,
		&entry.MismatchReason, &entry.AcknowledgedAt, &entry.AcknowledgedByPrincipalID,
		&entry.DisputedAt, &entry.DisputedByPrincipalID, &entry.DisputeReason,
		&entry.ResolvedAt, &entry.ResolvedByPrincipalID, &entry.ResolutionNote,
		&entry.CreatedAt, &entry.UpdatedAt,
	)
}

func (s *PgStore) GetEntry(ctx context.Context, id string) (*domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var entry domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanIntercompanyEntry(tx.QueryRow(ctx, `
			SELECT `+intercompanyEntryColumns+`
			FROM intercompany_entries
			WHERE intercompany_entry_id = $1 AND tenant_id = $2
		`, id, tenantID), &entry)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// AcknowledgeCounterparty is ACC-11's own AcknowledgeCounterparty command
// — moves a pair from OPEN ("UNMATCHED") to AWAITING_COUNTERPARTY. Guarded
// to only ever succeed from UNMATCHED, matching the spec's own state model
// order.
func (s *PgStore) AcknowledgeCounterparty(ctx context.Context, id, principalID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET match_status = $1, acknowledged_at = $2, acknowledged_by_principal_id = $3, updated_at = $2
			WHERE intercompany_entry_id = $4 AND tenant_id = $5 AND match_status = $6
		`, domain.MatchStatusAwaitingCounterparty, now, principalID, id, tenantID, domain.MatchStatusUnmatched)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidPairTransition
		}
		return nil
	})
}

// DisputeIntercompany is ACC-11's own DisputeIntercompany command — moves
// a pair from MISMATCH to DISPUTED. Only reachable from MISMATCH: there
// is nothing to dispute about a pair that already matched or was never
// even checked.
func (s *PgStore) DisputeIntercompany(ctx context.Context, id, principalID, reason string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET match_status = $1, disputed_at = $2, disputed_by_principal_id = $3, dispute_reason = $4, updated_at = $2
			WHERE intercompany_entry_id = $5 AND tenant_id = $6 AND match_status = $7
		`, domain.MatchStatusDisputed, now, principalID, reason, id, tenantID, domain.MatchStatusMismatch)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidPairTransition
		}
		return nil
	})
}

// ResolveMismatch is ACC-11's own ResolveMismatch command — moves a pair
// from DISPUTED to RESOLVED, the spec's own terminal outcome for a
// disputed pair. Never re-runs matching itself: a resolution is a human
// decision recorded as evidence, not an automatic recheck.
func (s *PgStore) ResolveMismatch(ctx context.Context, id, principalID, resolutionNote string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET match_status = $1, resolved_at = $2, resolved_by_principal_id = $3, resolution_note = $4, updated_at = $2
			WHERE intercompany_entry_id = $5 AND tenant_id = $6 AND match_status = $7
		`, domain.MatchStatusResolved, now, principalID, resolutionNote, id, tenantID, domain.MatchStatusDisputed)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidPairTransition
		}
		return nil
	})
}

func (s *PgStore) ListEntries(ctx context.Context, sourceEntityID, targetEntityID string) ([]domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT ` + intercompanyEntryColumns + `
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
			if err := scanIntercompanyEntry(rows, &entry); err != nil {
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
