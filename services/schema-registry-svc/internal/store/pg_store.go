// Package store provides the PostgreSQL implementation of the schema
// registry. This is the only layer that touches the database directly.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
	// Versions returns every registered version of eventName, oldest first.
	Versions(ctx context.Context, eventName string) ([]*domain.EventSchema, error)
	// EventNames returns every distinct event name with at least one
	// registered version.
	EventNames(ctx context.Context) ([]string, error)
	// Insert appends a new version row. Callers are responsible for
	// computing the next version number and running the compatibility
	// check — this method only persists.
	Insert(ctx context.Context, s *domain.EventSchema) error
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

const schemaColumns = `event_name, version, json_schema, registered_by, registered_at`

func scanEventSchema(row pgx.Row) (*domain.EventSchema, error) {
	var s domain.EventSchema
	var registeredBy *string
	var rawSchema []byte
	if err := row.Scan(&s.EventName, &s.Version, &rawSchema, &registeredBy, &s.RegisteredAt); err != nil {
		return nil, err
	}
	s.JSONSchema = json.RawMessage(rawSchema)
	if registeredBy != nil {
		s.RegisteredBy = *registeredBy
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

func (s *PgStore) Versions(ctx context.Context, eventName string) ([]*domain.EventSchema, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+schemaColumns+`
		FROM event_schemas
		WHERE event_name = $1
		ORDER BY version ASC`, eventName)
	if err != nil {
		return nil, fmt.Errorf("query versions: %w", err)
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

func (s *PgStore) EventNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT event_name FROM event_schemas ORDER BY event_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("query event names: %w", err)
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

func (s *PgStore) Insert(ctx context.Context, sch *domain.EventSchema) error {
	var registeredBy *string
	if sch.RegisteredBy != "" {
		registeredBy = &sch.RegisteredBy
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO event_schemas (event_name, version, json_schema, registered_by)
		VALUES ($1, $2, $3, $4)`,
		sch.EventName, sch.Version, []byte(sch.JSONSchema), registeredBy)
	if err != nil {
		return fmt.Errorf("insert schema version: %w", err)
	}
	return nil
}
