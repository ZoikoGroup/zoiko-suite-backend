package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

const applicabilityColumns = `applicability_id, tenant_id, legal_entity_id, jurisdiction_id::text,
	applicability_type, effective_from, effective_to, source_object_version,
	source_event_type, source_event_id, evidence_ref, recorded_at, updated_at`

// ApplyEntityApplicability records one consumed event's effect on the entity
// applicability projection, atomically with the event's de-duplication row.
//
// Returns applied=false when the event id was already consumed (a redelivery),
// or when the row already reflects a newer source_object_version than this
// event (an out-of-order delivery — an ASSIGNED arriving after its END_DATED
// must not reopen the assignment).
func (s *PgStore) ApplyEntityApplicability(ctx context.Context, eventID, eventType string, a domain.EntityJurisdictionApplicability) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING`, eventID, eventType)
	if err != nil {
		s.log.Error("pg ApplyEntityApplicability: dedupe insert failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	tag, err = tx.Exec(ctx, `
		INSERT INTO entity_jurisdiction_applicability (
			applicability_id, tenant_id, legal_entity_id, jurisdiction_id, applicability_type,
			effective_from, effective_to, source_object_version, source_event_type,
			source_event_id, evidence_ref)
		VALUES ($1, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (applicability_id) DO UPDATE SET
			jurisdiction_id       = EXCLUDED.jurisdiction_id,
			applicability_type    = EXCLUDED.applicability_type,
			effective_from        = EXCLUDED.effective_from,
			effective_to          = EXCLUDED.effective_to,
			source_object_version = EXCLUDED.source_object_version,
			source_event_type     = EXCLUDED.source_event_type,
			source_event_id       = EXCLUDED.source_event_id,
			evidence_ref          = COALESCE(EXCLUDED.evidence_ref, entity_jurisdiction_applicability.evidence_ref),
			updated_at            = NOW()
		WHERE entity_jurisdiction_applicability.tenant_id = EXCLUDED.tenant_id
		  AND entity_jurisdiction_applicability.legal_entity_id = EXCLUDED.legal_entity_id
		  AND entity_jurisdiction_applicability.source_object_version < EXCLUDED.source_object_version`,
		a.ApplicabilityID, a.TenantID, a.LegalEntityID, a.JurisdictionID, a.ApplicabilityType,
		a.EffectiveFrom, a.EffectiveTo, a.SourceObjectVersion, eventType, eventID, a.EvidenceRef)
	if err != nil {
		if isInvalidTextRepresentation(err) || isCheckViolation(err) || isStringDataRightTruncation(err) {
			// A malformed jurisdiction id or an inverted period is a bad event,
			// not an outage: record it as consumed so it is not retried
			// forever. The failed statement aborted the transaction (a commit
			// now would silently roll back), so the record is its own write.
			_ = tx.Rollback(ctx)
			if _, derr := s.pool.Exec(ctx, `
				INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2)
				ON CONFLICT (event_id) DO NOTHING`, eventID, eventType); derr != nil {
				return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, derr)
			}
			return false, fmt.Errorf("%w: %v", domain.ErrInvalidEvent, err)
		}
		s.log.Error("pg ApplyEntityApplicability: upsert failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	applied := tag.RowsAffected() == 1

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return applied, nil
}

// ListEntityJurisdictions returns the jurisdictions applying to a tenant's
// legal entity at `at`, oldest first. An unknown entity, or one belonging to
// another tenant, is an empty list — never another tenant's rows.
func (s *PgStore) ListEntityJurisdictions(ctx context.Context, tenantID, legalEntityID string, at time.Time) ([]*domain.EntityJurisdictionApplicability, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+applicabilityColumns+`
		FROM entity_jurisdiction_applicability
		WHERE tenant_id = $1 AND legal_entity_id = $2
		  AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		ORDER BY effective_from ASC, applicability_id ASC`, tenantID, legalEntityID, at)
	if err != nil {
		s.log.Error("pg ListEntityJurisdictions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	out := []*domain.EntityJurisdictionApplicability{}
	for rows.Next() {
		a, err := scanApplicability(rows)
		if err != nil {
			s.log.Error("pg ListEntityJurisdictions scan failed", zap.Error(err))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func scanApplicability(row pgx.Row) (*domain.EntityJurisdictionApplicability, error) {
	a := &domain.EntityJurisdictionApplicability{}
	err := row.Scan(&a.ApplicabilityID, &a.TenantID, &a.LegalEntityID, &a.JurisdictionID,
		&a.ApplicabilityType, &a.EffectiveFrom, &a.EffectiveTo, &a.SourceObjectVersion,
		&a.SourceEventType, &a.SourceEventID, &a.EvidenceRef, &a.RecordedAt, &a.UpdatedAt)
	return a, err
}

// isCheckViolation reports whether err is SQLSTATE 23514.
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}
