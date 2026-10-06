package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/webhook"
)

// ZS-SVC-Y-001 NCD-04 7.1: the direct path's delivery evidence trail (migration 000024).

// RecordDeliveryEvidence appends one normalized fact about a direct-send attempt, under the
// tenant's row-level security. It returns (true, nil) when the fact is new and (false, nil)
// for a replayed provider event, so a provider that delivers the same callback twice adds one
// fact. The notification is taken from the attempt, never from the caller, so a fact cannot
// be filed against the wrong notification.
func (s *PgStore) RecordDeliveryEvidence(ctx context.Context, ev *domain.DeliveryEvidence) (bool, error) {
	if ev.TenantID == "" || ev.AttemptID == "" || ev.SourceEventID == "" || ev.Fact == "" || ev.Strength == "" {
		return false, errors.New("evidence needs a tenant, attempt, source event, fact and strength")
	}
	if len(ev.Diagnostic) > domain.MaxDiagnosticLength {
		ev.Diagnostic = ev.Diagnostic[:domain.MaxDiagnosticLength]
	}
	var inserted bool
	err := s.withRLS(ctx, ev.TenantID, func(tx pgx.Tx) error {
		var notificationID, legalEntityID string
		if err := tx.QueryRow(ctx, `SELECT a.notification_id::text, n.legal_entity_id FROM notification_delivery_attempts a
			JOIN notifications n ON n.notification_id = a.notification_id AND n.tenant_id = a.tenant_id
			WHERE a.attempt_id::text = $1 AND a.tenant_id = $2`, ev.AttemptID, ev.TenantID).Scan(&notificationID, &legalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return webhook.ErrAttemptNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO notification_delivery_evidence
				(tenant_id, notification_id, attempt_id, source_event_id, provider, fact, strength, diagnostic, occurred_at)
			VALUES ($1,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, attempt_id, source_event_id) DO NOTHING`,
			ev.TenantID, notificationID, ev.AttemptID, ev.SourceEventID, ev.Provider, ev.Fact, ev.Strength,
			nullIfEmpty(ev.Diagnostic), ev.OccurredAt)
		if err != nil {
			return err
		}
		inserted = tag.RowsAffected() == 1
		if !inserted {
			return nil
		}
		ev.NotificationID = notificationID
		out, err := events.EvidenceRecorded(*ev, legalEntityID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, ev.TenantID, out)
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// ListDeliveryEvidence returns every fact recorded for a notification, oldest first, each
// with the stated limits of its kind.
func (s *PgStore) ListDeliveryEvidence(ctx context.Context, notificationID string) ([]domain.DeliveryEvidence, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := []domain.DeliveryEvidence{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id::text, tenant_id, notification_id::text, attempt_id::text, source_event_id, provider,
			       fact, strength, COALESCE(diagnostic,''), occurred_at, recorded_at
			FROM notification_delivery_evidence
			WHERE tenant_id = $1 AND notification_id::text = $2
			ORDER BY occurred_at, recorded_at`, tenantID, notificationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.DeliveryEvidence
			if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.NotificationID, &e.AttemptID, &e.SourceEventID, &e.Provider,
				&e.Fact, &e.Strength, &e.Diagnostic, &e.OccurredAt, &e.RecordedAt); err != nil {
				return err
			}
			e.Limits = domain.EvidenceLimits(e.Fact)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}
