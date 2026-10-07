// Package store provides the PostgreSQL implementation of
// source-authority-svc's persistence layer.
//
// Two access patterns, because the tenant boundary runs between the two tables
// (see the domain package doc): source_authority_maps is platform-wide
// reference data and is read and written without a tenant, while every
// statement touching normalized_facts carries an explicit tenant_id predicate
// AND runs inside a transaction that installs app.tenant_id for the row-level
// security policy. Both, not either: the predicate is the isolation, and the
// policy is the backstop that catches a future query written without one.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/source-authority-svc/internal/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// mapPgError turns a malformed identifier into "not found".
//
// source_authority_map_id is a uuid column, so a caller passing a non-UUID
// string reaches Postgres and comes back as 22P02
// invalid_text_representation. Surfacing that as a 500 reports a request the
// caller got wrong as an outage. A malformed id cannot name a row, which is
// exactly what 404 means.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrMapNotFound
	}
	return err
}

type Store interface {
	// CreateSourceAuthorityMap is idempotent on correlation_id: created is
	// false when the call replayed an existing rule, and m is overwritten with
	// the stored one.
	CreateSourceAuthorityMap(ctx context.Context, m *domain.SourceAuthorityMap) (created bool, err error)
	GetSourceAuthorityMap(ctx context.Context, mapID string) (*domain.SourceAuthorityMap, error)
	ListSourceAuthorityMaps(ctx context.Context, f domain.ListSourceAuthorityMapsFilter) ([]domain.SourceAuthorityMap, error)
	// SupersedeSourceAuthorityMap end-dates a rule. The only UPDATE this
	// service performs, and it touches effective_to and the supersession
	// evidence columns only.
	SupersedeSourceAuthorityMap(ctx context.Context, mapID string, effectiveTo time.Time, principalID string) (*domain.SourceAuthorityMap, error)

	// RecordFact is idempotent on (tenant_id, correlation_id).
	RecordFact(ctx context.Context, f *domain.NormalizedFact) (created bool, err error)
	ListNormalizedFacts(ctx context.Context, f domain.ListNormalizedFactsFilter) ([]domain.NormalizedFact, error)
	// ResolveAuthoritativeFact composes precedence with conflict detection
	// for a (tenant, field_family, entity_ref) triple — see package doc comment.
	ResolveAuthoritativeFact(ctx context.Context, tenantID, fieldFamily, entityRef string) (*domain.FactResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenantRLS runs fn in a transaction that has installed app.tenant_id, so
// the FORCE ROW LEVEL SECURITY policy added in migration 000002 applies.
func (s *PgStore) withTenantRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

// ── source_authority_maps ────────────────────────────────────────────────────

const mapColumns = `
	source_authority_map_id, field_family, source_system, precedence_rank,
	conflict_route, allowed_correction_path, effective_from, effective_to,
	created_at, created_by_principal_id, correlation_id,
	superseded_at, superseded_by_principal_id`

func scanMap(row pgx.Row) (*domain.SourceAuthorityMap, error) {
	m := &domain.SourceAuthorityMap{}
	err := row.Scan(
		&m.SourceAuthorityMapID, &m.FieldFamily, &m.SourceSystem, &m.PrecedenceRank,
		&m.ConflictRoute, &m.AllowedCorrectionPath, &m.EffectiveFrom, &m.EffectiveTo,
		&m.CreatedAt, &m.CreatedByPrincipalID, &m.CorrelationID,
		&m.SupersededAt, &m.SupersededByPrincipalID,
	)
	return m, err
}

// CreateSourceAuthorityMap inserts a precedence rule, idempotent on
// correlation_id.
//
// The idempotency check comes FIRST and is deliberately separate from the
// (field_family, source_system, effective_from) uniqueness that produces
// ErrConflict. Without it a retry — the client never saw the first response —
// trips that second index and is told its rule conflicts with itself, which is
// both wrong and unactionable: the operator is looking at a 409 naming a rule
// they successfully created a moment ago.
func (s *PgStore) CreateSourceAuthorityMap(ctx context.Context, m *domain.SourceAuthorityMap) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO source_authority_maps (
			source_authority_map_id, field_family, source_system, precedence_rank,
			conflict_route, allowed_correction_path, effective_from, created_by_principal_id,
			correlation_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (correlation_id) WHERE correlation_id IS NOT NULL DO NOTHING
	`, m.SourceAuthorityMapID, m.FieldFamily, m.SourceSystem, m.PrecedenceRank,
		m.ConflictRoute, m.AllowedCorrectionPath, m.EffectiveFrom, m.CreatedByPrincipalID,
		m.CorrelationID,
	)
	if err != nil {
		// A unique violation reaching here is the OTHER index — a genuinely
		// different rule for the same field family, source and start instant.
		if isUniqueViolation(err) {
			return false, domain.ErrConflict
		}
		return false, fmt.Errorf("insert source authority map: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}

	// Replay: return the rule this correlation_id already created.
	row := s.pool.QueryRow(ctx, "SELECT "+mapColumns+" FROM source_authority_maps WHERE correlation_id = $1", m.CorrelationID)
	stored, err := scanMap(row)
	if err != nil {
		return false, fmt.Errorf("read replayed source authority map: %w", err)
	}
	*m = *stored
	return false, nil
}

func (s *PgStore) GetSourceAuthorityMap(ctx context.Context, mapID string) (*domain.SourceAuthorityMap, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+mapColumns+" FROM source_authority_maps WHERE source_authority_map_id = $1", mapID)
	m, err := scanMap(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMapNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	return m, nil
}

func (s *PgStore) ListSourceAuthorityMaps(ctx context.Context, f domain.ListSourceAuthorityMapsFilter) ([]domain.SourceAuthorityMap, error) {
	query := "SELECT " + mapColumns + " FROM source_authority_maps WHERE 1 = 1"
	var args []any

	if f.FieldFamily != "" {
		args = append(args, f.FieldFamily)
		query += fmt.Sprintf(" AND field_family = $%d", len(args))
	}
	if f.SourceSystem != "" {
		args = append(args, f.SourceSystem)
		query += fmt.Sprintf(" AND source_system = $%d", len(args))
	}
	if !f.IncludeSuperseded {
		// The rules actually in force right now. A list that mixes these with
		// ended ones and offers no way to tell them apart is how an operator
		// concludes one source is ranked twice.
		query += " AND effective_from <= NOW() AND (effective_to IS NULL OR effective_to > NOW())"
	}

	// precedence_rank is not a total order — two sources may legitimately share
	// a rank, which is the tie that makes a resolution ambiguous — so the
	// primary key breaks the tie. Without it a paged read can return one rule
	// twice and skip another.
	query += " ORDER BY precedence_rank ASC, source_authority_map_id ASC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	if f.Offset > 0 {
		args = append(args, f.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
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

// SupersedeSourceAuthorityMap end-dates a precedence rule.
//
// Re-reads the row FOR UPDATE inside the same transaction as the write, so two
// concurrent supersessions cannot both observe an open window and both claim
// to have closed it.
func (s *PgStore) SupersedeSourceAuthorityMap(ctx context.Context, mapID string, effectiveTo time.Time, principalID string) (*domain.SourceAuthorityMap, error) {
	var out domain.SourceAuthorityMap

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, "SELECT "+mapColumns+" FROM source_authority_maps WHERE source_authority_map_id = $1 FOR UPDATE", mapID)
	current, err := scanMap(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMapNotFound
	}
	if err != nil {
		return nil, mapPgError(err)
	}
	if current.SupersededAt != nil {
		return nil, domain.ErrAlreadySuperseded
	}
	if !effectiveTo.After(current.EffectiveFrom) {
		return nil, domain.ErrSupersedeBeforeStart
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE source_authority_maps
		SET effective_to = $1, superseded_at = $2, superseded_by_principal_id = $3
		WHERE source_authority_map_id = $4
	`, effectiveTo, now, principalID, mapID); err != nil {
		return nil, fmt.Errorf("supersede source authority map: %w", err)
	}

	row = tx.QueryRow(ctx, "SELECT "+mapColumns+" FROM source_authority_maps WHERE source_authority_map_id = $1", mapID)
	updated, err := scanMap(row)
	if err != nil {
		return nil, fmt.Errorf("read superseded source authority map: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	out = *updated
	return &out, nil
}

// ── normalized_facts ──────────────────────────────────────────────────────────

const factColumns = `
	normalized_fact_id, tenant_id, field_family, entity_ref, source_system, source_record,
	source_version, fact_value, observed_at, effective_at, transformation_version,
	authority_class, created_at, created_by_principal_id, correlation_id`

// lpsFactColumns is factColumns qualified to the latest_per_source CTE.
//
// The resolver is the one query that reads facts through a JOIN, and the rule
// side (current_rule) also exposes field_family and source_system — so the bare
// list above makes both names ambiguous and Postgres rejects the whole query
// with SQLSTATE 42702. Every other caller of factColumns selects from a single
// table, where qualification would be noise, so this stays a separate constant
// rather than a change to that one. Column order must match factColumns: both
// feed the same scan order.
const lpsFactColumns = `
	lps.normalized_fact_id, lps.tenant_id, lps.field_family, lps.entity_ref, lps.source_system, lps.source_record,
	lps.source_version, lps.fact_value, lps.observed_at, lps.effective_at, lps.transformation_version,
	lps.authority_class, lps.created_at, lps.created_by_principal_id, lps.correlation_id`

func scanFact(row pgx.Row, f *domain.NormalizedFact) error {
	return row.Scan(
		&f.NormalizedFactID, &f.TenantID, &f.FieldFamily, &f.EntityRef, &f.SourceSystem, &f.SourceRecord,
		&f.SourceVersion, &f.FactValue, &f.ObservedAt, &f.EffectiveAt, &f.TransformationVersion,
		&f.AuthorityClass, &f.CreatedAt, &f.CreatedByPrincipalID, &f.CorrelationID,
	)
}

// RecordFact appends one observation, idempotent on (tenant_id, correlation_id).
func (s *PgStore) RecordFact(ctx context.Context, f *domain.NormalizedFact) (bool, error) {
	var created bool
	err := s.withTenantRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO normalized_facts (
				normalized_fact_id, tenant_id, field_family, entity_ref, source_system, source_record,
				source_version, fact_value, observed_at, effective_at, transformation_version,
				authority_class, created_by_principal_id, correlation_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id IS NOT NULL DO NOTHING
		`, f.NormalizedFactID, f.TenantID, f.FieldFamily, f.EntityRef, f.SourceSystem, f.SourceRecord,
			f.SourceVersion, f.FactValue, f.ObservedAt, f.EffectiveAt, f.TransformationVersion,
			f.AuthorityClass, f.CreatedByPrincipalID, f.CorrelationID,
		)
		if err != nil {
			return fmt.Errorf("insert normalized fact: %w", err)
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx,
			"SELECT "+factColumns+" FROM normalized_facts WHERE tenant_id = $1 AND correlation_id = $2",
			f.TenantID, f.CorrelationID)
		return scanFact(row, f)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) ListNormalizedFacts(ctx context.Context, f domain.ListNormalizedFactsFilter) ([]domain.NormalizedFact, error) {
	var out []domain.NormalizedFact
	err := s.withTenantRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		query := "SELECT " + factColumns + " FROM normalized_facts WHERE tenant_id = $1"
		args := []any{f.TenantID}

		if f.FieldFamily != "" {
			args = append(args, f.FieldFamily)
			query += fmt.Sprintf(" AND field_family = $%d", len(args))
		}
		if f.EntityRef != "" {
			args = append(args, f.EntityRef)
			query += fmt.Sprintf(" AND entity_ref = $%d", len(args))
		}
		if f.SourceSystem != "" {
			args = append(args, f.SourceSystem)
			query += fmt.Sprintf(" AND source_system = $%d", len(args))
		}

		// Newest observation first, primary key as tiebreaker: two facts can
		// share an effective_at, so it is not a total order on its own.
		query += " ORDER BY effective_at DESC, normalized_fact_id DESC"
		if f.Limit > 0 {
			args = append(args, f.Limit)
			query += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if f.Offset > 0 {
			args = append(args, f.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list normalized facts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var fact domain.NormalizedFact
			if err := scanFact(rows, &fact); err != nil {
				return fmt.Errorf("scan normalized fact: %w", err)
			}
			out = append(out, fact)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveAuthoritativeFact finds, for each source_system that has reported a
// fact for (tenantID, fieldFamily, entityRef), that source's latest applicable
// fact (effective_at <= now, most recent wins), joins each to its current
// precedence_rank, and returns the single fact from the highest-precedence
// source (lowest rank number) — UNLESS more than one source shares that top
// rank and their fact_values differ, in which case doc7 §D2 requires blocking
// rather than guessing.
//
// Three things this query does that the original did not:
//
//  1. It is scoped to one tenant. normalized_facts had no tenant column, so
//     this read composed one tenant's precedence over every tenant's facts.
//  2. It picks ONE precedence rule per source — the most recently effective —
//     rather than joining every rule whose window is open. Superseding a rule
//     by adding a new row (the documented way, before there was a supersede
//     operation) left both open, and joining both put the same source in the
//     ranking twice at two different ranks. The resolver then took the better
//     of the two, so demoting a source silently did nothing.
//  3. It LEFT JOINs, so a source that has reported facts but has no rule in
//     force is reported in UnmappedSources instead of vanishing. An inner join
//     made "nobody has ranked this source yet" indistinguishable from "this
//     source lost", which on a register built to surface disagreement is the
//     one confusion it cannot afford.
func (s *PgStore) ResolveAuthoritativeFact(ctx context.Context, tenantID, fieldFamily, entityRef string) (*domain.FactResolution, error) {
	result := &domain.FactResolution{FieldFamily: fieldFamily, EntityRef: entityRef}

	err := s.withTenantRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const query = `
			WITH latest_per_source AS (
				SELECT DISTINCT ON (source_system)
					normalized_fact_id, tenant_id, field_family, entity_ref, source_system, source_record,
					source_version, fact_value, observed_at, effective_at, transformation_version,
					authority_class, created_at, created_by_principal_id, correlation_id
				FROM normalized_facts
				WHERE tenant_id = $1 AND field_family = $2 AND entity_ref = $3 AND effective_at <= NOW()
				ORDER BY source_system, effective_at DESC, normalized_fact_id DESC
			),
			current_rule AS (
				SELECT DISTINCT ON (field_family, source_system)
					field_family, source_system, precedence_rank, conflict_route
				FROM source_authority_maps
				WHERE field_family = $2
				  AND effective_from <= NOW()
				  AND (effective_to IS NULL OR effective_to > NOW())
				ORDER BY field_family, source_system, effective_from DESC, source_authority_map_id DESC
			)
			SELECT ` + lpsFactColumns + `, cr.precedence_rank, cr.conflict_route
			FROM latest_per_source lps
			LEFT JOIN current_rule cr
			  ON cr.field_family = lps.field_family AND cr.source_system = lps.source_system
			ORDER BY cr.precedence_rank ASC NULLS LAST, lps.source_system ASC;`

		rows, err := tx.Query(ctx, query, tenantID, fieldFamily, entityRef)
		if err != nil {
			return fmt.Errorf("resolve authoritative fact: %w", err)
		}
		defer rows.Close()

		type rankedFact struct {
			fact           domain.NormalizedFact
			precedenceRank int
			conflictRoute  string
		}
		var ranked []rankedFact
		for rows.Next() {
			var f domain.NormalizedFact
			var precedenceRank *int
			var conflictRoute *string
			if err := rows.Scan(
				&f.NormalizedFactID, &f.TenantID, &f.FieldFamily, &f.EntityRef, &f.SourceSystem, &f.SourceRecord,
				&f.SourceVersion, &f.FactValue, &f.ObservedAt, &f.EffectiveAt, &f.TransformationVersion,
				&f.AuthorityClass, &f.CreatedAt, &f.CreatedByPrincipalID, &f.CorrelationID,
				&precedenceRank, &conflictRoute,
			); err != nil {
				return fmt.Errorf("scan ranked fact: %w", err)
			}
			if precedenceRank == nil {
				result.UnmappedSources = append(result.UnmappedSources, f.SourceSystem)
				continue
			}
			route := ""
			if conflictRoute != nil {
				route = *conflictRoute
			}
			ranked = append(ranked, rankedFact{fact: f, precedenceRank: *precedenceRank, conflictRoute: route})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("resolve authoritative fact rows: %w", err)
		}
		if len(ranked) == 0 {
			return nil
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
			return nil
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
			return nil
		}

		result.Ambiguous = true
		result.ConflictRoute = &topTier[0].conflictRoute
		for _, r := range topTier {
			result.ConflictingFacts = append(result.ConflictingFacts, r.fact)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
