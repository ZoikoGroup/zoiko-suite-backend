// Package store is search-indexer-svc's PostgreSQL control plane.
//
// Two access paths, and the difference between them is the security boundary:
//
//	withRLS(ctx, tenantID, fn)  — sets app.tenant_id, so the RLS policy in
//	                              000002 scopes every row the transaction can
//	                              see or write. The default for everything.
//	withPlatformScope(ctx, fn)  — sets app.platform_scope, which the policy
//	                              treats as visible regardless of tenant. Used
//	                              ONLY by the estate-wide sweeps that have no
//	                              tenant to be scoped by: restriction
//	                              verification and checkpoint accounting.
//
// An empty tenant on the RLS path is refused before it reaches Postgres, for
// the reason tenant-entity-registry-svc's store already records: the cast of
// ” to uuid raises, so an unscoped read failed as a 500 — a SERVER FAULT
// indistinguishable in monitoring from a real outage — rather than as the
// "you are scoped to nothing, so nothing is visible" that it actually is.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/search-indexer-svc/internal/domain"
)

// Store is the control-plane persistence surface.
type Store interface {
	// ── ESR-01 ───────────────────────────────────────────────────────────
	CreateSource(ctx context.Context, s domain.SearchSource) error
	GetSource(ctx context.Context, sourceID string) (*domain.SearchSource, error)
	GetSourceByType(ctx context.Context, sourceType string) (*domain.SearchSource, error)
	ListSources(ctx context.Context) ([]domain.SearchSource, error)

	CreateContract(ctx context.Context, c domain.IndexContract) error
	GetContract(ctx context.Context, contractID string) (*domain.IndexContract, error)
	GetPublishedContract(ctx context.Context, scopeName string) (*domain.IndexContract, error)
	ListContracts(ctx context.Context, scopeName string) ([]domain.IndexContract, error)
	TransitionContract(ctx context.Context, contractID string, from, to domain.ContractState) error
	NextContractVersion(ctx context.Context, scopeName string) (int, error)

	// ── ESR-05 ───────────────────────────────────────────────────────────
	CreateGeneration(ctx context.Context, g domain.IndexGeneration) error
	GetGeneration(ctx context.Context, generationID string) (*domain.IndexGeneration, error)
	ListGenerations(ctx context.Context, scopeName string) ([]domain.IndexGeneration, error)
	GetActiveGeneration(ctx context.Context, scopeName string) (*domain.IndexGeneration, error)
	TransitionGeneration(ctx context.Context, generationID string, from, to domain.GenerationState, digest, note string) error

	UpsertCheckpoint(ctx context.Context, cp domain.IndexCheckpoint) error
	ListCheckpoints(ctx context.Context, scopeName string) ([]domain.IndexCheckpoint, error)

	// ── ESR-02 (tenant-scoped) ───────────────────────────────────────────
	GetProjectionRecord(ctx context.Context, tenantID, scope, sourceType, sourceID string) (*domain.ProjectionRecord, error)
	UpsertProjectionRecord(ctx context.Context, r domain.ProjectionRecord) (applied bool, err error)
	CountProjections(ctx context.Context, scopeName string) (live int64, tombstoned int64, err error)

	UpsertTombstone(ctx context.Context, t domain.RestrictionTombstone, tombstoneID string) (applied bool, err error)
	MarkTombstoneState(ctx context.Context, tenantID, sourceType, sourceID, sourceEventID string, state domain.PropagationState, reason string) error
	ListPendingVerification(ctx context.Context, limit int) ([]domain.RestrictionTombstone, error)
	ListTombstones(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.RestrictionTombstone, error)

	// ── ESR-03/04 ────────────────────────────────────────────────────────
	RecordEvidence(ctx context.Context, e domain.SearchEvidence) error
	ListEvidence(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.SearchEvidence, error)

	Ping(ctx context.Context) error
	Close()
}

// PgStore implements Store against PostgreSQL.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) Close() { s.pool.Close() }

func (s *PgStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// withRLS runs fn inside a transaction scoped to tenantID.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		s.log.Debug("store access refused: no verified tenant on the request")
		return domain.ErrTenantRequired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // discarded intentionally on the commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withPlatformScope runs fn with the RLS platform flag set.
//
// Only two callers, both genuinely estate-wide: the restriction verifier
// (which must find every unverified tombstone across every tenant, and cannot
// know the tenants in advance) and the checkpoint accountant (which counts
// population per scope, not per tenant). Every other path is tenant-scoped,
// including the consumer's per-message write, which sets the tenant from the
// event envelope.
func (s *PgStore) withPlatformScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return fmt.Errorf("set_config app.platform_scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withPool runs fn against a plain connection, for the platform-scoped tables
// that carry no tenant dimension at all (sources, contracts, generations,
// checkpoints). They have no RLS policy, so setting a GUC would be theatre.
func (s *PgStore) withPool(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ── ESR-01: sources ──────────────────────────────────────────────────────────

func (s *PgStore) CreateSource(ctx context.Context, src domain.SearchSource) error {
	// A nil Go slice marshals to SQL NULL, and both array columns are NOT
	// NULL with a '{}' default — a default a NULL does not trigger, because
	// an explicit NULL is a value. So a source with no restriction event
	// types, which is an ordinary and legal registration (not every domain
	// emits a deletion event), was refused by the database and surfaced as a
	// 500 rather than as a created source.
	//
	// Normalised here rather than in the handler because the store is what
	// the column belongs to, and a second caller would hit it again.
	if src.EventTypes == nil {
		src.EventTypes = []string{}
	}
	if src.RestrictionEventTypes == nil {
		src.RestrictionEventTypes = []string{}
	}
	return s.withPool(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_sources (
				source_id, owner_service, source_type, tenant_scope, residency_region,
				sensitivity_ceiling, event_topic, event_types, restriction_event_types,
				freshness_class, max_lag_seconds, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			src.SourceID, src.OwnerService, src.SourceType, src.TenantScope, src.ResidencyRegion,
			string(src.SensitivityCeiling), src.EventTopic, src.EventTypes, src.RestrictionEventTypes,
			src.FreshnessClass, src.MaxLagSeconds, src.CreatedByPrincipalID)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: source_type %q is already registered", domain.ErrConflict, src.SourceType)
		}
		return err
	})
}

const sourceColumns = `source_id, owner_service, source_type, tenant_scope, residency_region,
	sensitivity_ceiling, event_topic, event_types, restriction_event_types,
	freshness_class, max_lag_seconds, created_at, created_by_principal_id`

func scanSource(row pgx.Row) (*domain.SearchSource, error) {
	var s domain.SearchSource
	var ceiling string
	err := row.Scan(&s.SourceID, &s.OwnerService, &s.SourceType, &s.TenantScope, &s.ResidencyRegion,
		&ceiling, &s.EventTopic, &s.EventTypes, &s.RestrictionEventTypes,
		&s.FreshnessClass, &s.MaxLagSeconds, &s.CreatedAt, &s.CreatedByPrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.SensitivityCeiling = domain.SensitivityClass(ceiling)
	return &s, nil
}

func (s *PgStore) GetSource(ctx context.Context, sourceID string) (*domain.SearchSource, error) {
	var out *domain.SearchSource
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = scanSource(tx.QueryRow(ctx,
			`SELECT `+sourceColumns+` FROM search_sources WHERE source_id = $1`, sourceID))
		return e
	})
	return out, err
}

func (s *PgStore) GetSourceByType(ctx context.Context, sourceType string) (*domain.SearchSource, error) {
	var out *domain.SearchSource
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = scanSource(tx.QueryRow(ctx,
			`SELECT `+sourceColumns+` FROM search_sources WHERE source_type = $1`, sourceType))
		return e
	})
	return out, err
}

func (s *PgStore) ListSources(ctx context.Context) ([]domain.SearchSource, error) {
	out := []domain.SearchSource{}
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+sourceColumns+` FROM search_sources ORDER BY source_type`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			src, err := scanSource(rows)
			if err != nil {
				return err
			}
			out = append(out, *src)
		}
		return rows.Err()
	})
	return out, err
}

// ── ESR-01: contracts ────────────────────────────────────────────────────────

// CreateContract writes the contract and its fields in ONE transaction.
//
// A contract with no fields, or with only the fields that happened to insert
// before a failure, is the thing that produces a generation whose mapping
// silently omits a searchable field — a search that returns nothing and no
// error. The all-or-nothing write removes that state entirely.
func (s *PgStore) CreateContract(ctx context.Context, c domain.IndexContract) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO index_contracts (
				contract_id, source_id, scope_name, version, schema_digest,
				publication_state, freshness_class, retrieval_class, analyzer_profile,
				authz_action, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			c.ContractID, c.SourceID, c.ScopeName, c.Version, c.SchemaDigest,
			string(c.State), c.FreshnessClass, string(c.RetrievalClass), c.AnalyzerProfile,
			c.AuthzAction, c.CreatedByPrincipalID)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: scope %q version %d already exists", domain.ErrConflict, c.ScopeName, c.Version)
		}
		if err != nil {
			return err
		}

		for _, f := range c.Fields {
			_, err := tx.Exec(ctx, `
				INSERT INTO search_field_definitions (
					contract_id, source_path, field_name, field_type,
					searchable, filterable, facetable, sortable, snippet_allowed,
					returnable, exportable, sensitivity_class, analyzer_profile)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				c.ContractID, f.SourcePath, f.FieldID, f.Type,
				f.Searchable, f.Filterable, f.Facetable, f.Sortable, f.SnippetAllowed,
				f.Returnable, f.Exportable, string(f.SensitivityClass), f.AnalyzerProfile)
			if err != nil {
				return fmt.Errorf("field %q: %w", f.FieldID, err)
			}
		}
		return nil
	})
}

const contractColumns = `contract_id, source_id, scope_name, version, schema_digest,
	publication_state, freshness_class, retrieval_class, analyzer_profile, authz_action,
	created_at, published_at, created_by_principal_id`

func scanContract(row pgx.Row) (*domain.IndexContract, error) {
	var c domain.IndexContract
	var state, retrieval string
	err := row.Scan(&c.ContractID, &c.SourceID, &c.ScopeName, &c.Version, &c.SchemaDigest,
		&state, &c.FreshnessClass, &retrieval, &c.AnalyzerProfile, &c.AuthzAction,
		&c.CreatedAt, &c.PublishedAt, &c.CreatedByPrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.State = domain.ContractState(state)
	c.RetrievalClass = domain.RetrievalClass(retrieval)
	return &c, nil
}

func (s *PgStore) loadFields(ctx context.Context, tx pgx.Tx, contractID string) ([]domain.SearchFieldDefinition, error) {
	rows, err := tx.Query(ctx, `
		SELECT field_name, source_path, field_type, searchable, filterable, facetable,
		       sortable, snippet_allowed, returnable, exportable, sensitivity_class, analyzer_profile
		FROM search_field_definitions WHERE contract_id = $1 ORDER BY field_name`, contractID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.SearchFieldDefinition{}
	for rows.Next() {
		var f domain.SearchFieldDefinition
		var sensitivity string
		if err := rows.Scan(&f.FieldID, &f.SourcePath, &f.Type, &f.Searchable, &f.Filterable,
			&f.Facetable, &f.Sortable, &f.SnippetAllowed, &f.Returnable, &f.Exportable,
			&sensitivity, &f.AnalyzerProfile); err != nil {
			return nil, err
		}
		f.SensitivityClass = domain.SensitivityClass(sensitivity)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PgStore) GetContract(ctx context.Context, contractID string) (*domain.IndexContract, error) {
	var out *domain.IndexContract
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		c, err := scanContract(tx.QueryRow(ctx,
			`SELECT `+contractColumns+` FROM index_contracts WHERE contract_id = $1`, contractID))
		if err != nil {
			return err
		}
		if c.Fields, err = s.loadFields(ctx, tx, c.ContractID); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

// GetPublishedContract returns the one PUBLISHED contract for a scope.
// Exactly one can exist — index_contracts_one_published_per_scope guarantees it.
func (s *PgStore) GetPublishedContract(ctx context.Context, scopeName string) (*domain.IndexContract, error) {
	var out *domain.IndexContract
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		c, err := scanContract(tx.QueryRow(ctx,
			`SELECT `+contractColumns+` FROM index_contracts
			 WHERE scope_name = $1 AND publication_state = 'PUBLISHED'`, scopeName))
		if err != nil {
			return err
		}
		if c.Fields, err = s.loadFields(ctx, tx, c.ContractID); err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

func (s *PgStore) ListContracts(ctx context.Context, scopeName string) ([]domain.IndexContract, error) {
	out := []domain.IndexContract{}
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if scopeName == "" {
			rows, err = tx.Query(ctx, `SELECT `+contractColumns+` FROM index_contracts ORDER BY scope_name, version DESC`)
		} else {
			rows, err = tx.Query(ctx, `SELECT `+contractColumns+` FROM index_contracts WHERE scope_name = $1 ORDER BY version DESC`, scopeName)
		}
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			c, err := scanContract(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, *c)
			ids = append(ids, c.ContractID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i, id := range ids {
			fields, err := s.loadFields(ctx, tx, id)
			if err != nil {
				return err
			}
			out[i].Fields = fields
		}
		return nil
	})
	return out, err
}

// TransitionContract moves a contract between publication states.
//
// The `from` state is part of the WHERE clause, not checked and then written.
// Two concurrent publishes would otherwise both read DRAFT, both find the
// transition legal, and both write — the compare-and-set makes the second one
// affect zero rows and report a conflict.
func (s *PgStore) TransitionContract(ctx context.Context, contractID string, from, to domain.ContractState) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		publishedAt := "published_at"
		if to == domain.ContractPublished {
			publishedAt = "now()"
		}
		tag, err := tx.Exec(ctx, `
			UPDATE index_contracts
			SET publication_state = $1, published_at = `+publishedAt+`
			WHERE contract_id = $2 AND publication_state = $3`,
			string(to), contractID, string(from))
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: another version of this scope is already PUBLISHED", domain.ErrConflict)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: contract is not in state %s", domain.ErrConflict, from)
		}
		return nil
	})
}

func (s *PgStore) NextContractVersion(ctx context.Context, scopeName string) (int, error) {
	var next int
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM index_contracts WHERE scope_name = $1`,
			scopeName).Scan(&next)
	})
	return next, err
}

// ── ESR-05: generations ──────────────────────────────────────────────────────

func (s *PgStore) CreateGeneration(ctx context.Context, g domain.IndexGeneration) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO index_generations (
				generation_id, contract_id, contract_version, scope_name, physical_index,
				state, build_from, created_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			g.GenerationID, g.ContractID, g.ContractVersion, g.ScopeName, g.PhysicalIndex,
			string(g.State), g.BuildFrom, g.CreatedByPrincipalID)
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: generation %s already exists", domain.ErrConflict, g.GenerationID)
		}
		return err
	})
}

const generationColumns = `generation_id, contract_id, contract_version, scope_name, physical_index,
	state, build_from, validation_digest, validation_note, activated_at, retired_at,
	created_at, created_by_principal_id`

func scanGeneration(row pgx.Row) (*domain.IndexGeneration, error) {
	var g domain.IndexGeneration
	var state string
	err := row.Scan(&g.GenerationID, &g.ContractID, &g.ContractVersion, &g.ScopeName, &g.PhysicalIndex,
		&state, &g.BuildFrom, &g.ValidationDigest, &g.ValidationNote, &g.ActivatedAt, &g.RetiredAt,
		&g.CreatedAt, &g.CreatedByPrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.State = domain.GenerationState(state)
	return &g, nil
}

func (s *PgStore) GetGeneration(ctx context.Context, generationID string) (*domain.IndexGeneration, error) {
	var out *domain.IndexGeneration
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = scanGeneration(tx.QueryRow(ctx,
			`SELECT `+generationColumns+` FROM index_generations WHERE generation_id = $1`, generationID))
		return e
	})
	return out, err
}

func (s *PgStore) GetActiveGeneration(ctx context.Context, scopeName string) (*domain.IndexGeneration, error) {
	var out *domain.IndexGeneration
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = scanGeneration(tx.QueryRow(ctx,
			`SELECT `+generationColumns+` FROM index_generations
			 WHERE scope_name = $1 AND state = 'ACTIVE'`, scopeName))
		return e
	})
	return out, err
}

func (s *PgStore) ListGenerations(ctx context.Context, scopeName string) ([]domain.IndexGeneration, error) {
	out := []domain.IndexGeneration{}
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if scopeName == "" {
			rows, err = tx.Query(ctx, `SELECT `+generationColumns+` FROM index_generations ORDER BY created_at DESC`)
		} else {
			rows, err = tx.Query(ctx, `SELECT `+generationColumns+` FROM index_generations WHERE scope_name = $1 ORDER BY created_at DESC`, scopeName)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			g, err := scanGeneration(rows)
			if err != nil {
				return err
			}
			out = append(out, *g)
		}
		return rows.Err()
	})
	return out, err
}

// TransitionGeneration is a compare-and-set on state, with the same reasoning
// as TransitionContract. The unique partial index on ACTIVE means two
// concurrent activations of different generations cannot both succeed even if
// both pass the CAS — the second gets a unique violation, reported as a
// conflict rather than as a 500.
func (s *PgStore) TransitionGeneration(ctx context.Context, generationID string, from, to domain.GenerationState, digest, note string) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		var timestampSet string
		switch to {
		case domain.GenerationActive:
			timestampSet = ", activated_at = now()"
		case domain.GenerationRetired:
			timestampSet = ", retired_at = now()"
		}
		tag, err := tx.Exec(ctx, `
			UPDATE index_generations
			SET state = $1,
			    validation_digest = CASE WHEN $2 = '' THEN validation_digest ELSE $2 END,
			    validation_note   = CASE WHEN $3 = '' THEN validation_note   ELSE $3 END`+
			timestampSet+`
			WHERE generation_id = $4 AND state = $5`,
			string(to), digest, note, generationID, string(from))
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: another generation is already ACTIVE for this scope", domain.ErrConflict)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: generation is not in state %s", domain.ErrConflict, from)
		}
		return nil
	})
}

// ── ESR-02: checkpoints ──────────────────────────────────────────────────────

func (s *PgStore) UpsertCheckpoint(ctx context.Context, cp domain.IndexCheckpoint) error {
	return s.withPool(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO index_checkpoints (
				scope_name, source_partition, watermark, committed_at, observed_at,
				lag_ms, freshness, indexed_live, indexed_tombstoned)
			VALUES ($1,$2,$3,$4,now(),$5,$6,$7,$8)
			ON CONFLICT (scope_name, source_partition) DO UPDATE SET
				-- GREATEST, never assignment. NP-17: "index checkpoint falsely
				-- advances past missing events." A watermark that can move
				-- BACKWARD is worse than one that stalls — a replay from an
				-- earlier offset would rewrite the high-water mark and make a
				-- gap look like progress.
				watermark          = GREATEST(index_checkpoints.watermark, EXCLUDED.watermark),
				committed_at       = EXCLUDED.committed_at,
				observed_at        = now(),
				lag_ms             = EXCLUDED.lag_ms,
				freshness          = EXCLUDED.freshness,
				indexed_live       = EXCLUDED.indexed_live,
				indexed_tombstoned = EXCLUDED.indexed_tombstoned`,
			cp.ScopeName, cp.SourcePartition, cp.Watermark, cp.CommittedAt,
			cp.LagMS, string(cp.Freshness), cp.IndexedLive, cp.IndexedTombstoned)
		return err
	})
}

func (s *PgStore) ListCheckpoints(ctx context.Context, scopeName string) ([]domain.IndexCheckpoint, error) {
	out := []domain.IndexCheckpoint{}
	err := s.withPool(ctx, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		q := `SELECT scope_name, source_partition, watermark, committed_at, observed_at,
		             lag_ms, freshness, indexed_live, indexed_tombstoned
		      FROM index_checkpoints`
		if scopeName == "" {
			rows, err = tx.Query(ctx, q+` ORDER BY scope_name, source_partition`)
		} else {
			rows, err = tx.Query(ctx, q+` WHERE scope_name = $1 ORDER BY source_partition`, scopeName)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cp domain.IndexCheckpoint
			var freshness string
			if err := rows.Scan(&cp.ScopeName, &cp.SourcePartition, &cp.Watermark, &cp.CommittedAt,
				&cp.ObservedAt, &cp.LagMS, &freshness, &cp.IndexedLive, &cp.IndexedTombstoned); err != nil {
				return err
			}
			cp.Freshness = domain.Freshness(freshness)
			out = append(out, cp)
		}
		return rows.Err()
	})
	return out, err
}

// ── ESR-02: projection ledger ────────────────────────────────────────────────

func (s *PgStore) GetProjectionRecord(ctx context.Context, tenantID, scope, sourceType, sourceID string) (*domain.ProjectionRecord, error) {
	var out *domain.ProjectionRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var r domain.ProjectionRecord
		err := tx.QueryRow(ctx, `
			SELECT tenant_id, scope_name, source_type, source_id, source_version,
			       restriction_epoch, content_hash, tombstoned, last_event_id, indexed_at
			FROM projection_ledger
			WHERE tenant_id = $1 AND scope_name = $2 AND source_type = $3 AND source_id = $4`,
			tenantID, scope, sourceType, sourceID).Scan(
			&r.TenantID, &r.ScopeName, &r.SourceType, &r.SourceID, &r.SourceVersion,
			&r.RestrictionEpoch, &r.ContentHash, &r.Tombstoned, &r.LastEventID, &r.IndexedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		out = &r
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	return out, err
}

// UpsertProjectionRecord applies a projection only if it is not older than what
// is already recorded, and reports whether it was applied.
//
// This single WHERE clause is where §5.2's two hardest rules live:
//
//	"Index writes are idempotent by tenant + source_ref + source_version"
//	"A newer restriction/deletion tombstone cannot be overwritten by an older
//	 replay event."
//
// The epoch comparison comes FIRST and is strict: a write whose restriction
// epoch is lower than the stored one is refused outright, whatever its source
// version says. That is NP-11 ("older replay event arrives after deletion
// tombstone → tombstone wins; no resurrection") and NP-48. Only at an EQUAL
// epoch does source_version decide, which is ordinary replay suppression.
//
// Doing it in the WHERE clause rather than as a read-then-write is what makes
// it safe under concurrency: two consumers processing two versions of the same
// record cannot interleave into the older one winning.
func (s *PgStore) UpsertProjectionRecord(ctx context.Context, r domain.ProjectionRecord) (bool, error) {
	var applied bool
	err := s.withRLS(ctx, r.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO projection_ledger (
				tenant_id, scope_name, source_type, source_id, source_version,
				restriction_epoch, content_hash, tombstoned, last_event_id, indexed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
			ON CONFLICT (tenant_id, scope_name, source_type, source_id) DO UPDATE SET
				source_version    = EXCLUDED.source_version,
				restriction_epoch = EXCLUDED.restriction_epoch,
				content_hash      = EXCLUDED.content_hash,
				tombstoned        = EXCLUDED.tombstoned,
				last_event_id     = EXCLUDED.last_event_id,
				indexed_at        = now()
			WHERE EXCLUDED.restriction_epoch > projection_ledger.restriction_epoch
			   OR (EXCLUDED.restriction_epoch = projection_ledger.restriction_epoch
			       AND EXCLUDED.source_version > projection_ledger.source_version)`,
			r.TenantID, r.ScopeName, r.SourceType, r.SourceID, r.SourceVersion,
			r.RestrictionEpoch, r.ContentHash, r.Tombstoned, r.LastEventID)
		if err != nil {
			return err
		}
		applied = tag.RowsAffected() > 0
		return nil
	})
	return applied, err
}

func (s *PgStore) CountProjections(ctx context.Context, scopeName string) (int64, int64, error) {
	var live, tombstoned int64
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE NOT tombstoned),
			       COUNT(*) FILTER (WHERE tombstoned)
			FROM projection_ledger WHERE scope_name = $1`, scopeName).Scan(&live, &tombstoned)
	})
	return live, tombstoned, err
}

// ── ESR-05: restriction tombstones ───────────────────────────────────────────

// UpsertTombstone records a restriction, idempotent by source event and
// monotonic by epoch. Reports whether this call changed anything.
func (s *PgStore) UpsertTombstone(ctx context.Context, t domain.RestrictionTombstone, tombstoneID string) (bool, error) {
	var applied bool
	err := s.withRLS(ctx, t.TenantID, func(tx pgx.Tx) error {
		// IDEMPOTENCY FIRST, then monotonicity. NP-47 and NP-48 are different
		// rules and conflating them produces a timing-dependent answer.
		//
		// A replay of the SAME source event is idempotent whenever it arrives
		// (NP-47). A DIFFERENT event carrying an older epoch is stale and must
		// be refused (NP-48). Checking the epoch first made the two
		// indistinguishable at the boundary: a replay arriving in a later
		// millisecond carried a newer epoch and fell through to the ON
		// CONFLICT as a no-op (200), while the same replay arriving inside the
		// same millisecond compared equal and was refused as stale (409). Two
		// answers for one request, decided by the clock.
		var alreadyApplied bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM restriction_tombstones
			               WHERE tenant_id = $1 AND source_type = $2
			                 AND source_id = $3 AND source_event_id = $4)`,
			t.TenantID, t.SourceType, t.SourceID, t.SourceEventID).Scan(&alreadyApplied); err != nil {
			return err
		}
		if alreadyApplied {
			// Not an error: the end state the caller asked for is in place.
			return nil
		}

		// The stored epoch for this source_ref, across all of its tombstones.
		// A restriction from a different event whose epoch is not strictly
		// newer is an out-of-order delivery and must not reopen visibility.
		var maxEpoch int64
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(epoch), -1) FROM restriction_tombstones
			WHERE tenant_id = $1 AND source_type = $2 AND source_id = $3`,
			t.TenantID, t.SourceType, t.SourceID).Scan(&maxEpoch)
		if err != nil {
			return err
		}
		if t.Epoch <= maxEpoch {
			return fmt.Errorf("%w: epoch %d is not newer than the applied epoch %d",
				domain.ErrStaleEpoch, t.Epoch, maxEpoch)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO restriction_tombstones (
				tombstone_id, tenant_id, scope_name, source_type, source_id, reason,
				epoch, source_event_id, effective_at, state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'PENDING')
			ON CONFLICT (tenant_id, source_type, source_id, source_event_id) DO NOTHING`,
			tombstoneID, t.TenantID, t.ScopeName, t.SourceType, t.SourceID, t.Reason,
			t.Epoch, t.SourceEventID, t.EffectiveAt)
		if err != nil {
			return err
		}
		applied = tag.RowsAffected() > 0
		return nil
	})
	return applied, err
}

func (s *PgStore) MarkTombstoneState(ctx context.Context, tenantID, sourceType, sourceID, sourceEventID string, state domain.PropagationState, reason string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var timestampSet string
		switch state {
		case domain.PropagationApplied:
			timestampSet = ", propagated_at = now()"
		case domain.PropagationVerified:
			timestampSet = ", verified_at = now()"
		}
		_, err := tx.Exec(ctx, `
			UPDATE restriction_tombstones
			SET state = $1, failure_reason = $2`+timestampSet+`
			WHERE tenant_id = $3 AND source_type = $4 AND source_id = $5 AND source_event_id = $6`,
			string(state), reason, tenantID, sourceType, sourceID, sourceEventID)
		return err
	})
}

const tombstoneColumns = `tenant_id, scope_name, source_type, source_id, reason, epoch,
	source_event_id, effective_at, propagated_at, verified_at, state, failure_reason`

func scanTombstone(rows pgx.Rows) (domain.RestrictionTombstone, error) {
	var t domain.RestrictionTombstone
	var state string
	err := rows.Scan(&t.TenantID, &t.ScopeName, &t.SourceType, &t.SourceID, &t.Reason, &t.Epoch,
		&t.SourceEventID, &t.EffectiveAt, &t.PropagatedAt, &t.VerifiedAt, &state, &t.FailureReason)
	t.State = domain.PropagationState(state)
	return t, err
}

// ListPendingVerification returns tombstones that have not been proven
// invisible yet, across every tenant.
//
// Platform-scoped on purpose: the verifier is a background sweep with no
// inbound request and therefore no tenant to inherit. Running it per-tenant
// would need an enumeration of tenants this service has no business holding —
// the exact privileged cross-tenant surface tracker row 65a refused to invent.
func (s *PgStore) ListPendingVerification(ctx context.Context, limit int) ([]domain.RestrictionTombstone, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []domain.RestrictionTombstone{}
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+tombstoneColumns+` FROM restriction_tombstones
			WHERE state IN ('PENDING', 'APPLIED', 'FAILED')
			ORDER BY effective_at ASC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTombstone(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ListTombstones(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.RestrictionTombstone, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []domain.RestrictionTombstone{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if scopeName == "" {
			rows, err = tx.Query(ctx, `SELECT `+tombstoneColumns+` FROM restriction_tombstones
				WHERE tenant_id = $1 ORDER BY effective_at DESC LIMIT $2`, tenantID, limit)
		} else {
			rows, err = tx.Query(ctx, `SELECT `+tombstoneColumns+` FROM restriction_tombstones
				WHERE tenant_id = $1 AND scope_name = $2 ORDER BY effective_at DESC LIMIT $3`, tenantID, scopeName, limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTombstone(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ── ESR-03/04: evidence ──────────────────────────────────────────────────────

func (s *PgStore) RecordEvidence(ctx context.Context, e domain.SearchEvidence) error {
	// Same nil-slice-becomes-NULL trap as CreateSource. Both columns are NOT
	// NULL with a '{}' default, and an explicit NULL does not trigger a
	// default — so a search with no reason codes (which is every SUCCESSFUL
	// search) would have failed to record its evidence. The search itself
	// would still have returned results, because recordEvidence never fails a
	// request, so the symptom would have been an evidence table containing
	// only refusals.
	if e.PartitionSet == nil {
		e.PartitionSet = []string{}
	}
	if e.ReasonCodes == nil {
		e.ReasonCodes = []string{}
	}
	return s.withRLS(ctx, e.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO search_evidence (
				evidence_id, tenant_id, request_id, correlation_id, actor_id, workload_id,
				on_behalf_of_principal, purpose_context, scope_name, query_digest,
				mandatory_filters_digest, plan_digest, index_generation, partition_set,
				complexity_score, result_count, suppressed_count, completeness_state,
				reason_codes, duration_ms, trace_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			e.EvidenceID, e.TenantID, e.RequestID, e.CorrelationID, e.ActorID, e.WorkloadID,
			e.OnBehalfOfID, e.Purpose, e.ScopeName, e.QueryDigest,
			e.FiltersDigest, e.PlanDigest, e.IndexGeneration, e.PartitionSet,
			e.ComplexityScore, e.ResultCount, e.SuppressedCount, string(e.Completeness),
			e.ReasonCodes, e.DurationMS, e.TraceID)
		return err
	})
}

func (s *PgStore) ListEvidence(ctx context.Context, tenantID, scopeName string, limit int) ([]domain.SearchEvidence, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	out := []domain.SearchEvidence{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const cols = `evidence_id, tenant_id, request_id, correlation_id, actor_id, workload_id,
			on_behalf_of_principal, purpose_context, scope_name, query_digest,
			mandatory_filters_digest, plan_digest, index_generation, partition_set,
			complexity_score, result_count, suppressed_count, completeness_state,
			reason_codes, duration_ms, trace_id, created_at`
		var rows pgx.Rows
		var err error
		if scopeName == "" {
			rows, err = tx.Query(ctx, `SELECT `+cols+` FROM search_evidence
				WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
		} else {
			rows, err = tx.Query(ctx, `SELECT `+cols+` FROM search_evidence
				WHERE tenant_id = $1 AND scope_name = $2 ORDER BY created_at DESC LIMIT $3`, tenantID, scopeName, limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.SearchEvidence
			var completeness string
			if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.RequestID, &e.CorrelationID,
				&e.ActorID, &e.WorkloadID, &e.OnBehalfOfID, &e.Purpose, &e.ScopeName,
				&e.QueryDigest, &e.FiltersDigest, &e.PlanDigest, &e.IndexGeneration,
				&e.PartitionSet, &e.ComplexityScore, &e.ResultCount, &e.SuppressedCount,
				&completeness, &e.ReasonCodes, &e.DurationMS, &e.TraceID, &e.CreatedAt); err != nil {
				return err
			}
			e.Completeness = domain.Completeness(completeness)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Connect opens the pool. Separated from New so a caller can wrap the pool for
// tracing before handing it over.
func Connect(ctx context.Context, dsn string, log *zap.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	log.Info("database connected", zap.String("database", cfg.ConnConfig.Database))
	return pool, nil
}
