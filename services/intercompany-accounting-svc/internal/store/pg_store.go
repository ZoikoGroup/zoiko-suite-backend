// Package store provides the PostgreSQL implementation of
// intercompany-accounting-svc's persistence layer.
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

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
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
	intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
	source_journal_entry_id, target_journal_entry_id,
	amount, currency_code, description, match_status,
	mismatch_reason,
	created_by_principal_id, matched_by_principal_id, correlation_id,
	created_at, matched_at, mismatched_at
`

func scanEntry(row interface{ Scan(...any) error }, e *domain.IntercompanyEntry) error {
	var status string
	if err := row.Scan(
		&e.IntercompanyEntryID, &e.TenantID, &e.SourceLegalEntityID, &e.TargetLegalEntityID,
		&e.SourceJournalEntryID, &e.TargetJournalEntryID,
		&e.Amount, &e.CurrencyCode, &e.Description, &status,
		&e.MismatchReason,
		&e.CreatedByPrincipalID, &e.MatchedByPrincipalID, &e.CorrelationID,
		&e.CreatedAt, &e.MatchedAt, &e.MismatchedAt,
	); err != nil {
		return err
	}
	e.MatchStatus = domain.MatchStatus(status)
	return nil
}

// CreateEntry inserts a new intercompany entry in PENDING status.
func (s *PgStore) CreateEntry(ctx context.Context, e *domain.IntercompanyEntry) error {
	tenantID := tenantFromCtxOrFallback(ctx, e.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, `
			INSERT INTO intercompany_entries (
				intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
				source_journal_entry_id, amount, currency_code, description, match_status,
				created_by_principal_id, correlation_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, e.IntercompanyEntryID, e.TenantID, e.SourceLegalEntityID, e.TargetLegalEntityID,
			e.SourceJournalEntryID, e.Amount, e.CurrencyCode, e.Description, string(e.MatchStatus),
			e.CreatedByPrincipalID, e.CorrelationID, now)
		if err != nil {
			return err
		}
		e.CreatedAt = now
		return nil
	})
}

// GetEntry returns (nil, nil) if not found — including when the caller's
// tenant scope doesn't match the entry's tenant (explicit filter, not
// RLS-only — see package doc).
func (s *PgStore) GetEntry(ctx context.Context, intercompanyEntryID string) (*domain.IntercompanyEntry, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil
	}

	var e domain.IntercompanyEntry
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+selectColumns+`
			FROM intercompany_entries WHERE intercompany_entry_id = $1 AND tenant_id = $2
		`, intercompanyEntryID, tenantID)
		return scanEntry(row, &e)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListEntries returns intercompany entries matching the given filter
// (tenant_id is required; match_status is optional).
func (s *PgStore) ListEntries(ctx context.Context, filter domain.ListEntriesFilter) ([]domain.IntercompanyEntry, error) {
	var out []domain.IntercompanyEntry
	err := s.withRLS(ctx, filter.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+selectColumns+`
			FROM intercompany_entries
			WHERE tenant_id = $1
			  AND ($2 = '' OR match_status = $2)
			ORDER BY created_at DESC
		`, filter.TenantID, filter.MatchStatus)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.IntercompanyEntry
			if err := scanEntry(rows, &e); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// MatchEntry transitions an entry from PENDING or MISMATCHED to MATCHED,
// atomically — the fromStatus check, the transition, and the tenant scope
// are one statement, no separate read, no race window. The two journals
// themselves must already have been verified against general-ledger-svc by
// the caller (internal/handler) before this is invoked; this method only
// persists the outcome.
func (s *PgStore) MatchEntry(ctx context.Context, tenantID, intercompanyEntryID, targetJournalEntryID, actorPrincipalID string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET match_status = 'MATCHED', target_journal_entry_id = $1,
			    matched_by_principal_id = $2, matched_at = $3
			WHERE intercompany_entry_id = $4 AND match_status IN ('PENDING', 'MISMATCHED') AND tenant_id = $5
		`, targetJournalEntryID, actorPrincipalID, time.Now().UTC(), intercompanyEntryID, tenantID)
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

// MismatchEntry transitions an entry from PENDING or MISMATCHED to
// MISMATCHED — recording the attempted target journal and the
// system-detected reason it failed verification. Re-attempting from
// MISMATCHED with a different target journal is allowed (that's how a
// mismatch gets investigated and retried), so the fromStatus set includes
// MISMATCHED itself.
func (s *PgStore) MismatchEntry(ctx context.Context, tenantID, intercompanyEntryID, targetJournalEntryID, reason string) error {
	var affected int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE intercompany_entries
			SET match_status = 'MISMATCHED', target_journal_entry_id = $1, mismatch_reason = $2, mismatched_at = $3
			WHERE intercompany_entry_id = $4 AND match_status IN ('PENDING', 'MISMATCHED') AND tenant_id = $5
		`, targetJournalEntryID, reason, time.Now().UTC(), intercompanyEntryID, tenantID)
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
