package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ApplyRegistryFact projects one establishment or registration status event, atomically with its
// de-duplication row. applied=false for a redelivery (the event id, or the same object version, was seen).
func (s *PgStore) ApplyRegistryFact(ctx context.Context, eventID, eventType string, e domain.RegistryFactEvent) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	r := e.Row
	var expiry any
	if r.ExpiryDate != "" {
		expiry = r.ExpiryDate
	}
	tag, err = tx.Exec(ctx, `
		INSERT INTO registry_fact_events (event_id, tenant_id, legal_entity_id, fact_kind, fact_id, subtype, jurisdiction,
			address_id, status, effective_date, expiry_date, object_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::date, $11::date, $12)
		ON CONFLICT (tenant_id, fact_kind, fact_id, object_version) DO NOTHING`,
		eventID, e.TenantID, e.LegalEntityID, r.FactKind, r.FactID, r.Subtype, r.Jurisdiction, r.AddressID, r.Status, r.EffectiveDate, expiry, r.ObjectVersion)
	if err != nil {
		if isInvalidTextRepresentation(err) || isCheckViolation(err) || isStringDataRightTruncation(err) || isDatetimeFieldOverflow(err) {
			_ = tx.Rollback(ctx)
			if _, derr := s.pool.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType); derr != nil {
				return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, derr)
			}
			return false, fmt.Errorf("%w: %v", domain.ErrInvalidEvent, err)
		}
		s.log.Error("pg ApplyRegistryFact failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	applied := tag.RowsAffected() == 1
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return applied, nil
}

// ApplyAddressCountry projects an address event. A version older than the one held changes nothing.
func (s *PgStore) ApplyAddressCountry(ctx context.Context, eventID, eventType string, e domain.AddressCountryEvent) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType)
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	tag, err = tx.Exec(ctx, `
		INSERT INTO registry_address_countries (tenant_id, address_id, country, object_version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, address_id) DO UPDATE SET country = EXCLUDED.country, object_version = EXCLUDED.object_version, updated_at = NOW()
		WHERE registry_address_countries.object_version < EXCLUDED.object_version`, e.TenantID, e.AddressID, e.Country, e.Version)
	if err != nil {
		if isStringDataRightTruncation(err) {
			_ = tx.Rollback(ctx)
			if _, derr := s.pool.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType); derr != nil {
				return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, derr)
			}
			return false, fmt.Errorf("%w: %v", domain.ErrInvalidEvent, err)
		}
		s.log.Error("pg ApplyAddressCountry failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	applied := tag.RowsAffected() == 1
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return applied, nil
}

// ListEntityFacts returns the establishment and registration status events of a tenant's legal entity effective
// on or before at, with each establishment's address country. Another tenant's facts are never returned.
func (s *PgStore) ListEntityFacts(ctx context.Context, tenantID, legalEntityID string, at time.Time) ([]domain.FactRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.event_id, f.fact_kind, f.fact_id, f.subtype, f.jurisdiction, f.address_id, COALESCE(a.country, ''),
		       f.status, to_char(f.effective_date, 'YYYY-MM-DD'), COALESCE(to_char(f.expiry_date, 'YYYY-MM-DD'), ''), f.object_version
		FROM registry_fact_events f
		LEFT JOIN registry_address_countries a ON a.tenant_id = f.tenant_id AND a.address_id = f.address_id
		WHERE f.tenant_id = $1 AND f.legal_entity_id = $2 AND f.effective_date <= $3::date
		ORDER BY f.fact_kind, f.fact_id, f.effective_date, f.object_version`, tenantID, legalEntityID, at.UTC())
	if err != nil {
		s.log.Error("pg ListEntityFacts failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	out := []domain.FactRow{}
	for rows.Next() {
		var r domain.FactRow
		if err := rows.Scan(&r.EventID, &r.FactKind, &r.FactID, &r.Subtype, &r.Jurisdiction, &r.AddressID, &r.AddressCtry,
			&r.Status, &r.EffectiveDate, &r.ExpiryDate, &r.ObjectVersion); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// isDatetimeFieldOverflow reports whether err is SQLSTATE 22008 (an impossible date).
func isDatetimeFieldOverflow(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22008"
}
