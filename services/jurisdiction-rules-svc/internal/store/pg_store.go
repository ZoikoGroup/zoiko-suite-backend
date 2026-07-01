// Package store provides the PostgreSQL implementation of the jurisdiction
// rules read model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers, service, or domain packages.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// Store is the interface consumed by the handler for the validation path.
// Keeping it narrow — only methods needed for the validation endpoint now.
// Rule-fetch and admin mutations are added incrementally.
type Store interface {
	// FindByID returns the Jurisdiction with the given jurisdiction_id.
	// Returns domain.ErrJurisdictionNotFound if no active record exists.
	// Returns domain.ErrStoreUnavailable on any DB error.
	FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// FindByID looks up a jurisdiction by its UUID primary key.
//
// Contract (matching HTTPJurisdictionValidator in tenant-entity-registry-svc):
//   - Returns *Jurisdiction if jurisdiction_id exists AND active_flag = true
//     AND (effective_to IS NULL OR effective_to > NOW()).
//   - Returns domain.ErrJurisdictionNotFound if not found or inactive.
//   - Returns domain.ErrStoreUnavailable on any database error.
func (s *PgStore) FindByID(ctx context.Context, jurisdictionID string) (*domain.Jurisdiction, error) {
	const query = `
		SELECT
			jurisdiction_id,
			jurisdiction_code,
			jurisdiction_name,
			jurisdiction_type,
			parent_jurisdiction_id,
			authority_type,
			effective_from,
			effective_to,
			active_flag,
			created_at,
			created_by_principal_id,
			schema_version
		FROM jurisdictions
		WHERE jurisdiction_id    = $1
		  AND active_flag        = TRUE
		  AND (effective_to IS NULL OR effective_to > NOW())`

	row := s.pool.QueryRow(ctx, query, jurisdictionID)

	j := &domain.Jurisdiction{}
	err := row.Scan(
		&j.JurisdictionID,
		&j.JurisdictionCode,
		&j.JurisdictionName,
		&j.JurisdictionType,
		&j.ParentJurisdictionID,
		&j.AuthorityType,
		&j.EffectiveFrom,
		&j.EffectiveTo,
		&j.ActiveFlag,
		&j.CreatedAt,
		&j.CreatedByPrincipalID,
		&j.SchemaVersion,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg FindByID failed",
			zap.String("jurisdiction_id", jurisdictionID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return j, nil
}
