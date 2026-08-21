// Package store provides the PostgreSQL implementation of the schema
// registry. This is the only layer that touches the database directly.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/domain"
)

// Store is the interface consumed by the handler.
type Store interface {
	// LatestVersion returns the highest-versioned schema for eventName, or
	// nil if none has been registered yet.
	LatestVersion(ctx context.Context, eventName string) (*domain.EventSchema, error)
	// Version returns a specific version of eventName's schema, or nil if
	// that version doesn't exist.
	Version(ctx context.Context, eventName string, version int) (*domain.EventSchema, error)
	// Versions returns one page of eventName's registered versions, oldest
	// first.
	Versions(ctx context.Context, eventName string, limit, offset int) ([]*domain.EventSchema, error)
	// EventNames returns one page of the distinct event names with at least
	// one registered version.
	EventNames(ctx context.Context, limit, offset int) ([]string, error)
	// Insert appends a new version row, assigning the version number
	// atomically from the current maximum and returning the stored row.
	//
	// The version is computed inside the INSERT rather than by the caller.
	// Previously the handler read the latest version, added one, and inserted
	// — so two concurrent registrations both computed the same number and the
	// loser hit the (event_name, version) primary key. That collision was
	// reported as a store failure, i.e. a 503, making an ordinary race look
	// like a database outage.
	//
	// expectedVersion is the version the caller ran its compatibility check
	// against. If another registration has landed since, Insert returns
	// domain.ErrVersionRaced rather than writing: the proposed schema was
	// validated against a baseline that is no longer latest, so retrying
	// server-side would skip the check against the version that actually won.
	Insert(ctx context.Context, s *domain.EventSchema, expectedVersion int) (*domain.EventSchema, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

const schemaColumns = `event_name, version, json_schema, compatibility_mode, owning_service, registered_by, registered_at`

func scanEventSchema(row pgx.Row) (*domain.EventSchema, error) {
	var s domain.EventSchema
	var registeredBy, owningService *string
	var rawSchema []byte
	if err := row.Scan(&s.EventName, &s.Version, &rawSchema, &s.CompatibilityMode, &owningService, &registeredBy, &s.RegisteredAt); err != nil {
		return nil, err
	}
	s.JSONSchema = json.RawMessage(rawSchema)
	if registeredBy != nil {
		s.RegisteredBy = *registeredBy
	}
	if owningService != nil {
		s.OwningService = *owningService
	}
	return &s, nil
}

func (s *PgStore) LatestVersion(ctx context.Context, eventName string) (*domain.EventSchema, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+schemaColumns+`
		FROM event_schemas
		WHERE event_name = $1
		ORDER BY version DESC
		LIMIT 1`, eventName)

	schema, err := scanEventSchema(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query latest version: %w", err)
	}
	return schema, nil
}

func (s *PgStore) Version(ctx context.Context, eventName string, version int) (*domain.EventSchema, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+schemaColumns+`
		FROM event_schemas
		WHERE event_name = $1 AND version = $2`, eventName, version)

	schema, err := scanEventSchema(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query version: %w", err)
	}
	return schema, nil
}

func (s *PgStore) Versions(ctx context.Context, eventName string, limit, offset int) ([]*domain.EventSchema, error) {
	// (event_name, version) is the primary key, so ordering by version is
	// already a total order — no tiebreaker is needed for stable paging here.
	rows, err := s.pool.Query(ctx, `
		SELECT `+schemaColumns+`
		FROM event_schemas
		WHERE event_name = $1
		ORDER BY version ASC
		LIMIT $2 OFFSET $3`, eventName, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query versions: %w", mapPgError(err))
	}
	defer rows.Close()

	var out []*domain.EventSchema
	for rows.Next() {
		schema, err := scanEventSchema(rows)
		if err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, schema)
	}
	return out, rows.Err()
}

func (s *PgStore) EventNames(ctx context.Context, limit, offset int) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT event_name FROM event_schemas ORDER BY event_name ASC LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query event names: %w", mapPgError(err))
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan event name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *PgStore) Insert(ctx context.Context, sch *domain.EventSchema, expectedVersion int) (*domain.EventSchema, error) {
	var registeredBy, owningService *string
	if sch.RegisteredBy != "" {
		registeredBy = &sch.RegisteredBy
	}
	if sch.OwningService != "" {
		owningService = &sch.OwningService
	}

	// The version is derived inside the statement from the current maximum,
	// so two concurrent registrations cannot both compute the same number.
	// The WHERE clause is the optimistic-concurrency guard: it only inserts
	// if the latest version is still the one the caller checked against, so a
	// race is refused rather than silently accepted against a stale baseline.
	// COALESCE handles the first version of a new event name, where MAX is NULL.
	// Every parameter is cast explicitly. $1 appears both as an inserted value
	// and in the WHERE clause, and without a cast Postgres cannot deduce one
	// type for both uses — it fails with "inconsistent types deduced for
	// parameter $1" (42P08) rather than anything that names the real problem.
	const query = `
		INSERT INTO event_schemas (event_name, version, json_schema, compatibility_mode, owning_service, registered_by)
		SELECT $1::varchar, COALESCE(MAX(version), 0) + 1, $2::jsonb, $3::varchar, $4::varchar, $5::varchar
		FROM   event_schemas
		WHERE  event_name = $1::varchar
		HAVING COALESCE(MAX(version), 0) = $6::int
		RETURNING ` + schemaColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		sch.EventName, []byte(sch.JSONSchema), sch.CompatibilityMode, owningService, registeredBy, expectedVersion)

	stored, err := scanEventSchema(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// HAVING excluded the row: the latest version moved between the
			// caller's read and this write.
			return nil, domain.ErrVersionRaced
		}
		if isUniqueViolation(err) {
			// Belt and braces — the primary key would also catch a collision
			// the HAVING guard somehow missed. Same meaning, same answer, and
			// specifically not reported as a store outage.
			return nil, domain.ErrVersionRaced
		}
		return nil, fmt.Errorf("insert schema version: %w", mapPgError(err))
	}
	return stored, nil
}

// isUniqueViolation reports whether err is a Postgres unique/PK violation
// (SQLSTATE 23505). Without this a concurrent registration surfaced as a
// generic store error and therefore a 503 — an ordinary race reported as an
// outage, which sends the reader to look for a broken database.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// mapPgError turns the Postgres failures that are really caller mistakes into
// domain errors, so they stop arriving at the handler as "the store is
// unavailable".
//
// 22001 is string_data_right_truncation: a value wider than its VARCHAR(255).
// event_name and owning_service are both validated at the boundary now, so
// this is the backstop — and the reason it matters is that the previous
// behaviour was a 503, an outage status for a name that was simply too long.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22001" {
		return domain.ErrFieldTooLong
	}
	return err
}
