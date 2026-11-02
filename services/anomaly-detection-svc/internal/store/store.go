// Package store is the Postgres persistence layer of anomaly-detection-svc.
//
// Every public method runs inside exactly one transaction that first declares
// the caller's tenant with set_config('app.tenant_id', $1, true) so Postgres
// row-level security (FORCE RLS on every tenant table) filters every read and
// checks every write. Queries additionally filter tenant_id explicitly, so
// isolation does not depend on a single mechanism.
//
// Write commands follow the same five steps, in this order:
//
//  1. lock / load the aggregate                 (404 if missing for tenant)
//  2. claim the Idempotency-Key                 (replay → return original)
//  3. check If-Match against the version        (412 if stale)
//  4. check the state machine / business rules  (409 / 403 / 422)
//  5. write the change + its outbox event in the same transaction
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// Store is everything the HTTP handlers need from persistence.
type Store interface {
	// Readiness.
	Ping(ctx context.Context) error

	// Rule-engine surface (/v1/anomalies).
	CreateRule(ctx context.Context, cmd domain.CommandContext, req domain.CreateRuleRequest) (domain.CommandResult[domain.AnomalyRule], error)
	GetRule(ctx context.Context, tenantID, ruleID string) (*domain.AnomalyRule, error)
	ListRules(ctx context.Context, tenantID string, filter domain.RuleFilter, page domain.PageRequest) ([]domain.AnomalyRule, string, error)
	DetectAnomaly(ctx context.Context, cmd domain.CommandContext, req domain.DetectAnomalyRequest) (domain.CommandResult[domain.AnomalyRecord], error)
	GetAnomaly(ctx context.Context, tenantID, anomalyID string) (*domain.AnomalyRecord, error)
	ListAnomalies(ctx context.Context, tenantID string, filter domain.AnomalyFilter, page domain.PageRequest) ([]domain.AnomalyRecord, string, error)
	UpdateAnomalyStatus(ctx context.Context, cmd domain.CommandContext, anomalyID string, req domain.UpdateStatusRequest) (domain.CommandResult[domain.AnomalyRecord], error)

	// AI-04 governed advisory layer (/v1/anomaly-governance).
	RegisterAnomalyModel(ctx context.Context, cmd domain.CommandContext, req domain.RegisterAnomalyModelRequest) (domain.CommandResult[domain.AnomalyModel], error)
	UpdateAnomalyModel(ctx context.Context, cmd domain.CommandContext, modelID string, req domain.UpdateAnomalyModelRequest) (domain.CommandResult[domain.AnomalyModel], error)
	GetAnomalyModel(ctx context.Context, tenantID, modelID string) (*domain.AnomalyModel, error)
	ListAnomalyModels(ctx context.Context, tenantID string, filter domain.ModelFilter, page domain.PageRequest) ([]domain.AnomalyModel, string, error)
	GetModelDriftMetrics(ctx context.Context, tenantID, modelID string) (*domain.DriftMetrics, error)

	RunDetection(ctx context.Context, cmd domain.CommandContext, req domain.RunDetectionRequest) (domain.CommandResult[domain.DetectionRun], error)
	GetDetectionRun(ctx context.Context, tenantID, runID string) (*domain.DetectionRun, error)
	GetSignalsByRun(ctx context.Context, tenantID, runID string) ([]domain.GovernedAnomalySignal, error)

	ListSignals(ctx context.Context, tenantID string, filter domain.SignalFilter, page domain.PageRequest) ([]domain.GovernedAnomalySignal, string, error)
	GetGovernedSignal(ctx context.Context, tenantID, signalID string) (*domain.GovernedAnomalySignal, error)
	AcknowledgeSignal(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.AcknowledgeSignalRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error)
	EscalateForReview(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.EscalateForReviewRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error)
	CloseSignal(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.CloseSignalRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error)

	GetDisposition(ctx context.Context, tenantID, signalID string) (*domain.ReviewDisposition, error)
	ListDispositions(ctx context.Context, tenantID string, filter domain.DispositionFilter, page domain.PageRequest) ([]domain.ReviewDisposition, string, error)
}

// PgStore implements Store on a pgx pool. The pool must connect as the
// NOSUPERUSER NOBYPASSRLS application role, or RLS is not enforced.
type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// Ping is used by /readyz.
func (s *PgStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// ─────────────────────────────────────────────────────────────────────────────
// Transactions
// ─────────────────────────────────────────────────────────────────────────────

// errTenantMissing should never reach a client: the tenant middleware refuses
// header-less requests first. It exists so a programming error fails closed.
var errTenantMissing = errors.New("store: tenant id is required")

// inTenantTx runs fn inside one transaction scoped to tenantID for RLS, and
// commits only if fn returns nil.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if strings.TrimSpace(tenantID) == "" {
		return errTenantMissing
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}

	if err := fn(tx); err != nil {
		return translatePgError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return translatePgError(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// translatePgError turns the database constraint failures that a client can
// cause into domain errors, so they surface as 4xx instead of 500. Anything
// else is returned unchanged and becomes a 500.
func translatePgError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505": // unique_violation
		return &domain.Error{Kind: domain.KindStateConflict, Code: "duplicate_resource", Detail: "a resource with the same identity already exists"}
	case "23514", "22003", "22001": // check_violation, numeric overflow, string too long
		return &domain.Error{Kind: domain.KindValidation, Code: "field_invalid", Detail: "a value is outside the allowed range: " + pgErr.Message}
	case "23503": // foreign_key_violation
		return &domain.Error{Kind: domain.KindValidation, Code: "reference_invalid", Detail: "a referenced resource does not exist"}
	case "P0001": // raise_exception from a lifecycle trigger
		return &domain.Error{Kind: domain.KindStateConflict, Code: "transition_not_allowed", Detail: pgErr.Message}
	default:
		return err
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Idempotency (Global API contract §9)
// ─────────────────────────────────────────────────────────────────────────────

// claimResult tells a command what to do after claiming its key.
type claimResult struct {
	// IsReplay is true when this exact request was already executed; the
	// command must return the resource named by OriginalResourceID instead
	// of executing again.
	IsReplay           bool
	OriginalResourceID string
}

// claimIdempotencyKey records (tenant, principal, route, key) → resource.
//
//   - first use of the key            → stored; IsReplay=false
//   - same key, same request hash     → IsReplay=true with the original id
//   - same key, different request     → domain.ErrIdempotencyKeyReused (409)
//
// The claim lives in the command's transaction: if the command later fails,
// the claim rolls back with it, so a failed request never "uses up" a key.
// A concurrent request with the same key blocks on the primary key until
// this transaction ends, then sees the committed row and replays.
func claimIdempotencyKey(ctx context.Context, tx pgx.Tx, cmd domain.CommandContext, resourceType, resourceID string) (claimResult, error) {
	claim := cmd.Idempotency
	if claim.Key == "" {
		// Only reachable from internal callers (tests); every HTTP write
		// is refused earlier without an Idempotency-Key.
		return claimResult{}, nil
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys
			(tenant_id, principal_id, route_template, idempotency_key, request_sha256, resource_type, resource_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, principal_id, route_template, idempotency_key) DO NOTHING`,
		cmd.TenantID, cmd.PrincipalID, claim.RouteTemplate, claim.Key, claim.RequestSHA256, resourceType, resourceID)
	if err != nil {
		return claimResult{}, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return claimResult{}, nil
	}

	var storedHash, storedResourceID string
	err = tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id
		FROM idempotency_keys
		WHERE tenant_id = $1 AND principal_id = $2 AND route_template = $3 AND idempotency_key = $4`,
		cmd.TenantID, cmd.PrincipalID, claim.RouteTemplate, claim.Key).Scan(&storedHash, &storedResourceID)
	if err != nil {
		return claimResult{}, fmt.Errorf("read idempotency claim: %w", err)
	}

	if storedHash != claim.RequestSHA256 {
		return claimResult{}, domain.ErrIdempotencyKeyReused
	}
	return claimResult{IsReplay: true, OriginalResourceID: storedResourceID}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Optimistic concurrency (Global API contract §10)
// ─────────────────────────────────────────────────────────────────────────────

// checkExpectedVersion compares the If-Match version with the row's current
// version. Missing If-Match is 428; a mismatch is 412.
func checkExpectedVersion(expected *int, current int) error {
	if expected == nil {
		return domain.ErrPreconditionRequired
	}
	if *expected != current {
		return domain.ErrStaleVersion.WithDetail(fmt.Sprintf(
			"If-Match is \"%d\" but the current version is \"%d\"; re-read the resource and retry", *expected, current))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Transactional outbox
// ─────────────────────────────────────────────────────────────────────────────

// outboxEvent is one domain event to publish after commit.
type outboxEvent struct {
	EventType     string
	AggregateType string
	AggregateID   string
	Payload       any
}

// enqueueEvent writes the event into anomaly_outbox inside tx, so the event
// exists if and only if the state change it describes was committed. The
// relay in internal/outbox publishes it to Kafka.
func enqueueEvent(ctx context.Context, tx pgx.Tx, cmd domain.CommandContext, event outboxEvent) error {
	payloadJSON, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", event.EventType, err)
	}
	eventID := "evt-" + uuid.NewString()
	_, err = tx.Exec(ctx, `
		INSERT INTO anomaly_outbox
			(event_id, event_type, aggregate_type, aggregate_id, tenant_id, legal_entity_id, actor_id, correlation_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		eventID, event.EventType, event.AggregateType, event.AggregateID,
		cmd.TenantID, cmd.LegalEntityID, cmd.PrincipalID, cmd.CorrelationID, payloadJSON)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", event.EventType, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Small helpers
// ─────────────────────────────────────────────────────────────────────────────

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

// pageLimitPlusOne asks the database for one row more than the page size;
// if it comes back, there is a next page.
func pageLimitPlusOne(page domain.PageRequest) int {
	limit := page.Limit
	if limit <= 0 || limit > domain.MaxPageLimit {
		limit = domain.DefaultPageLimit
	}
	return limit + 1
}

// cursorArgs returns the (created_at, id) keyset arguments, or two nils for
// the first page. SQL uses them as:
//
//	AND ($n::timestamptz IS NULL OR (created_at, id) < ($n, $m))
func cursorArgs(page domain.PageRequest) (any, any) {
	if page.Cursor == nil {
		return nil, nil
	}
	return page.Cursor.CreatedAt, page.Cursor.ID
}

// splitPage drops the extra "is there more?" row fetched by pageLimitPlusOne
// and returns the cursor for the next page ("" when this is the last page).
func splitPage[T any](rows []T, page domain.PageRequest, cursorOf func(T) string) ([]T, string) {
	limit := pageLimitPlusOne(page) - 1
	if len(rows) <= limit {
		return rows, ""
	}
	pageRows := rows[:limit]
	lastRow := pageRows[len(pageRows)-1]
	return pageRows, cursorOf(lastRow)
}
