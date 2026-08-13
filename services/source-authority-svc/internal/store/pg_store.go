// Package store provides the PostgreSQL implementation of
// source-authority-svc's persistence layer.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/source-authority-svc/internal/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreateSourceAuthorityMap(ctx context.Context, m *domain.SourceAuthorityMap) error
	ListSourceAuthorityMaps(ctx context.Context, fieldFamily string) ([]domain.SourceAuthorityMap, error)

	RecordFact(ctx context.Context, f *domain.NormalizedFact) error
	// ResolveAuthoritativeFact composes precedence with conflict detection
	// for a (field_family, entity_ref) pair — see package doc comment.
	ResolveAuthoritativeFact(ctx context.Context, fieldFamily, entityRef string) (*domain.FactResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// ── source_authority_maps ────────────────────────────────────────────────────

const mapColumns = `
	source_authority_map_id, field_family, source_system, precedence_rank,
	conflict_route, allowed_correction_path, effective_from, effective_to,
	created_at, created_by_principal_id`

func scanMap(row pgx.Row) (*domain.SourceAuthorityMap, error) {
	m := &domain.SourceAuthorityMap{}
	err := row.Scan(
		&m.SourceAuthorityMapID, &m.FieldFamily, &m.SourceSystem, &m.PrecedenceRank,
		&m.ConflictRoute, &m.AllowedCorrectionPath, &m.EffectiveFrom, &m.EffectiveTo,
		&m.CreatedAt, &m.CreatedByPrincipalID,
	)
	return m, err
}

func (s *PgStore) CreateSourceAuthorityMap(ctx context.Context, m *domain.SourceAuthorityMap) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO source_authority_maps (
			source_authority_map_id, field_family, source_system, precedence_rank,
			conflict_route, allowed_correction_path, effective_from, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, m.SourceAuthorityMapID, m.FieldFamily, m.SourceSystem, m.PrecedenceRank,
		m.ConflictRoute, m.AllowedCorrectionPath, m.EffectiveFrom, m.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w", domain.ErrConflict)
		}
		return fmt.Errorf("insert source authority map: %w", err)
	}
	return nil
}

func (s *PgStore) ListSourceAuthorityMaps(ctx context.Context, fieldFamily string) ([]domain.SourceAuthorityMap, error) {
	const query = `
		SELECT ` + mapColumns + `
		FROM source_authority_maps
		WHERE field_family = $1
		ORDER BY precedence_rank ASC;`

	rows, err := s.pool.Query(ctx, query, fieldFamily)
	if err != nil {
		return nil, fmt.Errorf("list source authority maps: %w", err)
	}
	defer rows.Close()

	var out []domain.SourceAuthorityMap
	for rows.Next() {
		m, err := scanMap(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source authority map: %w", err)
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ── normalized_facts ──────────────────────────────────────────────────────────

const factColumns = `
	normalized_fact_id, field_family, entity_ref, source_system, source_record,
	source_version, fact_value, observed_at, effective_at, transformation_version,
	authority_class, created_at, created_by_principal_id`

func scanFact(row pgx.Row) (*domain.NormalizedFact, error) {
	f := &domain.NormalizedFact{}
	err := row.Scan(
		&f.NormalizedFactID, &f.FieldFamily, &f.EntityRef, &f.SourceSystem, &f.SourceRecord,
		&f.SourceVersion, &f.FactValue, &f.ObservedAt, &f.EffectiveAt, &f.TransformationVersion,
		&f.AuthorityClass, &f.CreatedAt, &f.CreatedByPrincipalID,
	)
	return f, err
}

func (s *PgStore) RecordFact(ctx context.Context, f *domain.NormalizedFact) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO normalized_facts (
			normalized_fact_id, field_family, entity_ref, source_system, source_record,
			source_version, fact_value, observed_at, effective_at, transformation_version,
			authority_class, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, f.NormalizedFactID, f.FieldFamily, f.EntityRef, f.SourceSystem, f.SourceRecord,
		f.SourceVersion, f.FactValue, f.ObservedAt, f.EffectiveAt, f.TransformationVersion,
		f.AuthorityClass, f.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert normalized fact: %w", err)
	}
	return nil
}

// ResolveAuthoritativeFact finds, for each source_system that has
// reported a fact for (fieldFamily, entityRef), that source's latest
// applicable fact (effective_at <= now, most recent wins), joins each to
// its current precedence_rank from source_authority_maps, and returns the
// single fact from the highest-precedence source (lowest rank number) —
// UNLESS more than one source shares that top rank and their fact_values
// differ, in which case doc7 §D2 requires blocking rather than guessing.
func (s *PgStore) ResolveAuthoritativeFact(ctx context.Context, fieldFamily, entityRef string) (*domain.FactResolution, error) {
	const query = `
		WITH latest_per_source AS (
			SELECT DISTINCT ON (source_system)
				normalized_fact_id, field_family, entity_ref, source_system, source_record,
				source_version, fact_value, observed_at, effective_at, transformation_version,
				authority_class, created_at, created_by_principal_id
			FROM normalized_facts
			WHERE field_family = $1 AND entity_ref = $2 AND effective_at <= NOW()
			ORDER BY source_system, effective_at DESC
		),
		ranked AS (
			SELECT lps.*, sam.precedence_rank, sam.conflict_route
			FROM latest_per_source lps
			JOIN source_authority_maps sam
			  ON sam.field_family = lps.field_family AND sam.source_system = lps.source_system
			 AND sam.effective_from <= NOW() AND (sam.effective_to IS NULL OR sam.effective_to > NOW())
		)
		SELECT ` + factColumns + `, precedence_rank, conflict_route
		FROM ranked
		ORDER BY precedence_rank ASC;`

	rows, err := s.pool.Query(ctx, query, fieldFamily, entityRef)
	if err != nil {
		return nil, fmt.Errorf("resolve authoritative fact: %w", err)
	}
	defer rows.Close()

	type rankedFact struct {
		fact           domain.NormalizedFact
		precedenceRank int
		conflictRoute  string
	}
	var ranked []rankedFact
	for rows.Next() {
		f := domain.NormalizedFact{}
		var precedenceRank int
		var conflictRoute string
		if err := rows.Scan(
			&f.NormalizedFactID, &f.FieldFamily, &f.EntityRef, &f.SourceSystem, &f.SourceRecord,
			&f.SourceVersion, &f.FactValue, &f.ObservedAt, &f.EffectiveAt, &f.TransformationVersion,
			&f.AuthorityClass, &f.CreatedAt, &f.CreatedByPrincipalID,
			&precedenceRank, &conflictRoute,
		); err != nil {
			return nil, fmt.Errorf("scan ranked fact: %w", err)
		}
		ranked = append(ranked, rankedFact{fact: f, precedenceRank: precedenceRank, conflictRoute: conflictRoute})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve authoritative fact rows: %w", err)
	}

	result := &domain.FactResolution{FieldFamily: fieldFamily, EntityRef: entityRef}
	if len(ranked) == 0 {
		return result, nil
	}

	topRank := ranked[0].precedenceRank
	var topTier []rankedFact
	for _, r := range ranked {
		if r.precedenceRank == topRank {
			topTier = append(topTier, r)
		}
	}

	if len(topTier) == 1 {
		result.AuthoritativeFact = &topTier[0].fact
		return result, nil
	}

	// More than one source shares the top precedence tier — ambiguous
	// only if their values actually differ; identical values from two
	// equally-ranked sources is agreement, not a conflict.
	firstValue := string(topTier[0].fact.FactValue)
	allAgree := true
	for _, r := range topTier[1:] {
		if string(r.fact.FactValue) != firstValue {
			allAgree = false
			break
		}
	}
	if allAgree {
		result.AuthoritativeFact = &topTier[0].fact
		return result, nil
	}

	result.Ambiguous = true
	result.ConflictRoute = &topTier[0].conflictRoute
	for _, r := range topTier {
		result.ConflictingFacts = append(result.ConflictingFacts, r.fact)
	}
	return result, nil
}
