// Package asyncpgx is the PostgreSQL implementation of
// apicontract.OperationStore. It is a separate package so services that only
// need the wire contract do not gain a pgx dependency.
package asyncpgx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/identity-envelope/apicontract"
)

const SchemaSQL = `
CREATE TABLE IF NOT EXISTS async_operations (
    id                  text PRIMARY KEY,
    tenant_id           text NOT NULL,
    principal_id        text NOT NULL DEFAULT '',
    route               text NOT NULL DEFAULT '',
    type                text NOT NULL,
    state               text NOT NULL CHECK (state IN
        ('queued','running','waiting_external','succeeded','failed','cancel_requested','cancelled')),
    progress_completed  bigint NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
    progress_total      bigint NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
    resource_ref        text NOT NULL DEFAULT '',
    problem             jsonb,
    cancelable          boolean NOT NULL DEFAULT true,
    idempotency_key     text NOT NULL DEFAULT '',
    request_hash        text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    started_at          timestamptz,
    finished_at         timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS async_operations_idempotency_idx
    ON async_operations (tenant_id, principal_id, route, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS async_operations_tenant_created_idx
    ON async_operations (tenant_id, created_at DESC);
`

const RLSSQL = `
ALTER TABLE async_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE async_operations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS async_operations_tenant_isolation ON async_operations;
CREATE POLICY async_operations_tenant_isolation ON async_operations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
`

type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	db DB
}

func New(db DB) *Store {
	return &Store{db: db}
}

func NewPool(pool *pgxpool.Pool) *Store {
	return &Store{db: pool}
}

func ApplySchema(ctx context.Context, db DB) error {
	return Apply(ctx, db, SchemaSQL)
}

func Apply(ctx context.Context, db DB, script string) error {
	for _, statement := range strings.Split(script, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := db.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

const selectColumns = `id, tenant_id, principal_id, route, type, state, progress_completed,
	progress_total, resource_ref, problem, cancelable, idempotency_key, request_hash,
	created_at, updated_at, started_at, finished_at`

func (s *Store) Create(ctx context.Context, op apicontract.Operation) (apicontract.Operation, bool, error) {
	if op.ID == "" {
		op.ID = apicontract.NewOperationID()
	}
	if op.State == "" {
		op.State = apicontract.OperationQueued
	}
	if op.CreatedAt.IsZero() {
		op.CreatedAt = time.Now().UTC()
	}
	op.UpdatedAt = op.CreatedAt
	if err := op.Validate(); err != nil {
		return apicontract.Operation{}, false, err
	}

	var problemJSON []byte
	if op.Problem != nil {
		raw, err := json.Marshal(op.Problem)
		if err != nil {
			return apicontract.Operation{}, false, err
		}
		problemJSON = raw
	}

	const insert = `INSERT INTO async_operations (id, tenant_id, principal_id, route, type, state,
		progress_completed, progress_total, resource_ref, problem, cancelable, idempotency_key,
		request_hash, created_at, updated_at, started_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (tenant_id, principal_id, route, idempotency_key) WHERE idempotency_key <> ''
		DO NOTHING
		RETURNING ` + selectColumns

	args := operationArgs(op, problemJSON)
	row := s.db.QueryRow(ctx, insert, args...)
	created, err := scanOperation(row)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, false, err
	}
	existing, found, err := s.FindByKey(ctx, op.TenantID, op.PrincipalID, op.Route, op.IdempotencyKey)
	if err != nil {
		return apicontract.Operation{}, false, err
	}
	if !found {
		return apicontract.Operation{}, false, fmt.Errorf("asyncpgx: insert conflicted but no existing operation was found")
	}
	if existing.RequestHash != op.RequestHash {
		return apicontract.Operation{}, false, apicontract.ErrOperationConflict
	}
	return existing, false, nil
}

func (s *Store) Get(ctx context.Context, tenantID, operationID string) (apicontract.Operation, error) {
	row := s.db.QueryRow(ctx, `SELECT `+selectColumns+` FROM async_operations WHERE tenant_id = $1 AND id = $2`, tenantID, operationID)
	op, err := scanOperation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, apicontract.ErrOperationNotFound
	}
	return op, err
}

func (s *Store) FindByKey(ctx context.Context, tenantID, principalID, route, idempotencyKey string) (apicontract.Operation, bool, error) {
	if idempotencyKey == "" {
		return apicontract.Operation{}, false, nil
	}
	row := s.db.QueryRow(ctx, `SELECT `+selectColumns+` FROM async_operations
		WHERE tenant_id = $1 AND principal_id = $2 AND route = $3 AND idempotency_key = $4`,
		tenantID, principalID, route, idempotencyKey)
	op, err := scanOperation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, false, nil
	}
	if err != nil {
		return apicontract.Operation{}, false, err
	}
	return op, true, nil
}

func (s *Store) Advance(ctx context.Context, tenantID, operationID string, to apicontract.OperationState, progress *apicontract.OperationProgress, problem *apicontract.Problem, resourceRef string) (apicontract.Operation, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return apicontract.Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanOperation(tx.QueryRow(ctx, `SELECT `+selectColumns+`
		FROM async_operations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, apicontract.ErrOperationNotFound
	}
	if err != nil {
		return apicontract.Operation{}, err
	}
	if !apicontract.CanTransition(current.State, to) {
		return apicontract.Operation{}, fmt.Errorf("%w: cannot move operation from %s to %s",
			apicontract.ErrOperationConflict, current.State, to)
	}

	var problemJSON []byte
	if problem != nil {
		raw, marshalErr := json.Marshal(problem)
		if marshalErr != nil {
			return apicontract.Operation{}, marshalErr
		}
		problemJSON = raw
	}
	completed, total := current.ProgressValues()
	if progress != nil {
		completed, total = progress.CompletedUnits, progress.TotalUnits
	}

	now := time.Now().UTC()
	const update = `UPDATE async_operations SET state = $3, progress_completed = $4, progress_total = $5,
		problem = COALESCE($6, problem), resource_ref = CASE WHEN $7 <> '' THEN $7 ELSE resource_ref END,
		updated_at = $8,
		started_at = CASE WHEN $3 = 'running' THEN COALESCE(started_at, $8) ELSE started_at END,
		finished_at = CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN $8 ELSE finished_at END,
		cancelable = CASE WHEN $3 IN ('succeeded','failed','cancelled') THEN false ELSE cancelable END
		WHERE tenant_id = $1 AND id = $2
		RETURNING ` + selectColumns
	op, err := scanOperation(tx.QueryRow(ctx, update, tenantID, operationID, to, completed, total, problemJSON, resourceRef, now))
	if err != nil {
		return apicontract.Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apicontract.Operation{}, err
	}
	return op, nil
}

func (s *Store) RequestCancel(ctx context.Context, tenantID, operationID string) (apicontract.Operation, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return apicontract.Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanOperation(tx.QueryRow(ctx, `SELECT `+selectColumns+`
		FROM async_operations WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return apicontract.Operation{}, apicontract.ErrOperationNotFound
	}
	if err != nil {
		return apicontract.Operation{}, err
	}
	if current.State.Terminal() || !current.Cancelable {
		return apicontract.Operation{}, apicontract.ErrOperationNotCancelable
	}
	if current.State == apicontract.OperationCancelRequested {
		if err := tx.Commit(ctx); err != nil {
			return apicontract.Operation{}, err
		}
		return current, nil
	}
	now := time.Now().UTC()
	op, err := scanOperation(tx.QueryRow(ctx, `UPDATE async_operations SET state = $3, updated_at = $4
		WHERE tenant_id = $1 AND id = $2 RETURNING `+selectColumns,
		tenantID, operationID, apicontract.OperationCancelRequested, now))
	if err != nil {
		return apicontract.Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return apicontract.Operation{}, err
	}
	return op, nil
}

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (s *Store) begin(ctx context.Context) (pgx.Tx, error) {
	b, ok := s.db.(beginner)
	if !ok {
		return nil, errors.New("asyncpgx: the supplied DB does not support transactions")
	}
	return b.Begin(ctx)
}

func scanOperation(row pgx.Row) (apicontract.Operation, error) {
	var (
		op                            apicontract.Operation
		state                         string
		problemJSON                   []byte
		createdAt, updatedAt          time.Time
		startedAt, finishedAt         *time.Time
		completed, total              int64
		resourceRef, idemKey, reqHash string
		principalID, route            string
		cancelable                    bool
		tenantID, id, opType          string
	)
	err := row.Scan(&id, &tenantID, &principalID, &route, &opType, &state, &completed, &total,
		&resourceRef, &problemJSON, &cancelable, &idemKey, &reqHash, &createdAt, &updatedAt,
		&startedAt, &finishedAt)
	if err != nil {
		return apicontract.Operation{}, err
	}
	op = apicontract.Operation{
		ID: id, TenantID: tenantID, PrincipalID: principalID, Route: route, Type: opType,
		State: apicontract.OperationState(state), ResourceRef: resourceRef, Cancelable: cancelable,
		IdempotencyKey: idemKey, RequestHash: reqHash, CreatedAt: createdAt, UpdatedAt: updatedAt,
		StartedAt: startedAt, FinishedAt: finishedAt,
	}
	if completed != 0 || total != 0 {
		op.Progress = &apicontract.OperationProgress{CompletedUnits: completed, TotalUnits: total}
	}
	if len(problemJSON) > 0 {
		var p apicontract.Problem
		if err := json.Unmarshal(problemJSON, &p); err != nil {
			return apicontract.Operation{}, err
		}
		op.Problem = &p
	}
	return op, nil
}

func operationArgs(op apicontract.Operation, problemJSON []byte) []any {
	completed, total := op.ProgressValues()
	return []any{op.ID, op.TenantID, op.PrincipalID, op.Route, op.Type, op.State,
		completed, total, op.ResourceRef, problemJSON, op.Cancelable, op.IdempotencyKey,
		op.RequestHash, op.CreatedAt, op.UpdatedAt, op.StartedAt, op.FinishedAt}
}
