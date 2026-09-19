package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

const conflictSelectColumns = `
	conflict_id, tenant_id, legal_entity_id,
	run_id, population_id, statement_line_id,
	matched_transaction_id, matched_journal_id,
	payment_id, provider_request_id,
	bank_rec_status, provider_confirmed_status,
	conflict_type, conflict_reason, conflict_status, source_event_id,
	raised_at, raised_by_principal_id,
	resolved_at, resolved_by_principal_id, resolution_note,
	correlation_id
`

func scanConflict(row interface{ Scan(...any) error }, c *domain.EvidenceConflict) error {
	return row.Scan(
		&c.ConflictID, &c.TenantID, &c.LegalEntityID,
		&c.RunID, &c.PopulationID, &c.StatementLineID,
		&c.MatchedTransactionID, &c.MatchedJournalID,
		&c.PaymentID, &c.ProviderRequestID,
		&c.BankRecStatus, &c.ProviderConfirmedStatus,
		&c.ConflictType, &c.ConflictReason, &c.ConflictStatus, &c.SourceEventID,
		&c.RaisedAt, &c.RaisedByPrincipalID,
		&c.ResolvedAt, &c.ResolvedByPrincipalID, &c.ResolutionNote,
		&c.CorrelationID,
	)
}

// ── RaiseEvidenceConflict ─────────────────────────────────────────────────────

// RaiseEvidenceConflict inserts a new OPEN conflict. Idempotent: if an open
// conflict for (statement_line_id, payment_id) already exists, returns the
// existing conflict with created=false. This is the safe path for replayed
// payment-status events.
func (s *PgStore) RaiseEvidenceConflict(ctx context.Context, tenantID string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error) {
	var conflict domain.EvidenceConflict
	var created bool

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// Idempotent check: existing open conflict?
		existRow := tx.QueryRow(ctx, `
			SELECT `+conflictSelectColumns+`
			FROM evidence_conflicts
			WHERE tenant_id = $1
			  AND statement_line_id = $2
			  AND payment_id = $3
			  AND conflict_status = 'OPEN'
		`, tenantID, req.StatementLineID, req.PaymentID)
		err := scanConflict(existRow, &conflict)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			created = false
			return nil
		}

		// Insert new conflict.
		row := tx.QueryRow(ctx, `
			INSERT INTO evidence_conflicts (
				tenant_id, legal_entity_id, statement_line_id,
				payment_id, provider_request_id,
				bank_rec_status, provider_confirmed_status,
				conflict_reason, source_event_id,
				raised_by_principal_id, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'system',$10)
			RETURNING `+conflictSelectColumns,
			tenantID, req.LegalEntityID, req.StatementLineID,
			req.PaymentID, req.ProviderRequestID,
			req.BankRecStatus, req.ProviderConfirmedStatus,
			req.ConflictReason, req.SourceEventID,
			req.CorrelationID,
		)
		if err := scanConflict(row, &conflict); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, mapPgError(err)
	}
	return &conflict, created, nil
}

// ── IsEventProcessed / MarkEventProcessed ────────────────────────────────────

// IsEventProcessed checks the inbox table to see if a source event has
// already been processed (idempotency guard for event consumers).
func (s *PgStore) IsEventProcessed(ctx context.Context, tenantID, eventID string) (bool, error) {
	var processed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM evidence_conflict_inbox
				WHERE source_event_id = $1
			)
		`, eventID)
		return row.Scan(&processed)
	})
	if err != nil {
		return false, mapPgError(err)
	}
	return processed, nil
}

// MarkEventProcessed inserts the event into the inbox. Idempotent via
// unique index on source_event_id.
func (s *PgStore) MarkEventProcessed(ctx context.Context, tenantID, eventID string) error {
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO evidence_conflict_inbox (tenant_id, source_event_id)
			VALUES ($1, $2)
			ON CONFLICT (source_event_id) DO NOTHING
		`, tenantID, eventID)
		return err
	})
	return mapPgError(err)
}

// ── GetEvidenceConflict ───────────────────────────────────────────────────────

func (s *PgStore) GetEvidenceConflict(ctx context.Context, tenantID, conflictID string) (*domain.EvidenceConflict, error) {
	var conflict domain.EvidenceConflict
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+conflictSelectColumns+`
			FROM evidence_conflicts
			WHERE conflict_id = $1 AND tenant_id = $2
		`, conflictID, tenantID)
		return scanConflict(row, &conflict)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrConflictNotFound
		}
		return nil, mapPgError(err)
	}
	return &conflict, nil
}

// ── ListOpenConflicts ─────────────────────────────────────────────────────────

// ListOpenConflicts returns OPEN evidence conflicts for a tenant, most
// recently raised first. limit=0 uses a sensible default.
func (s *PgStore) ListOpenConflicts(ctx context.Context, tenantID string, limit int) ([]domain.EvidenceConflict, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var conflicts []domain.EvidenceConflict
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+conflictSelectColumns+`
			FROM evidence_conflicts
			WHERE tenant_id = $1 AND conflict_status = 'OPEN'
			ORDER BY raised_at DESC
			LIMIT $2
		`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.EvidenceConflict
			if err := scanConflict(rows, &c); err != nil {
				return err
			}
			conflicts = append(conflicts, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return conflicts, nil
}

// ── ResolveEvidenceConflict ───────────────────────────────────────────────────

// ResolveEvidenceConflict transitions an OPEN conflict to RESOLVED.
func (s *PgStore) ResolveEvidenceConflict(ctx context.Context, tenantID, conflictID, principalID, note string) (*domain.EvidenceConflict, error) {
	var conflict domain.EvidenceConflict
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		row := tx.QueryRow(ctx, `
			UPDATE evidence_conflicts
			SET conflict_status = 'RESOLVED',
			    resolved_at = $1,
			    resolved_by_principal_id = $2,
			    resolution_note = $3
			WHERE conflict_id = $4 AND tenant_id = $5 AND conflict_status = 'OPEN'
			RETURNING `+conflictSelectColumns,
			now, principalID, note, conflictID, tenantID,
		)
		err := scanConflict(row, &conflict)
		if errors.Is(err, pgx.ErrNoRows) {
			// Either not found or already resolved — check which.
			existRow := tx.QueryRow(ctx, `
				SELECT conflict_status FROM evidence_conflicts
				WHERE conflict_id = $1 AND tenant_id = $2
			`, conflictID, tenantID)
			var status string
			if scanErr := existRow.Scan(&status); errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.ErrConflictNotFound
			} else if scanErr != nil {
				return scanErr
			}
			if status != "OPEN" {
				return domain.ErrConflictAlreadyResolved
			}
			return domain.ErrConflictNotFound
		}
		return err
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return &conflict, nil
}
