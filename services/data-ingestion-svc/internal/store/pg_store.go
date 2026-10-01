// Package store is the PgStore persistence layer for data-ingestion-svc
// (DATA-01, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write —
// tenant scoping is enforced by the database, not by an application-level
// WHERE clause the caller could get wrong.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/outbox"
)

// Store is the DATA-01 persistence contract.
type Store interface {
	StartIngestion(ctx context.Context, tenantID string, run *domain.IngestionRun, claim domain.IdempotencyClaim) (*domain.IngestionRun, error)
	CommitBatch(ctx context.Context, tenantID string, req domain.CommitBatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.LandingObject, error)
	QuarantineBatch(ctx context.Context, tenantID string, req domain.QuarantineBatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.QuarantineItem, error)
	ReplayFromCheckpoint(ctx context.Context, tenantID string, req domain.ReplayFromCheckpointRequest, actor string, claim domain.IdempotencyClaim) (*domain.SourceCheckpoint, error)
	CloseRun(ctx context.Context, tenantID string, req domain.CloseRunRequest, finalStatus domain.IngestionRunStatus, actor string, claim domain.IdempotencyClaim) (*domain.IngestionRun, error)

	GetRun(ctx context.Context, tenantID, runID string) (*domain.IngestionRun, error)
	GetCheckpoint(ctx context.Context, tenantID, sourceID string) (*domain.SourceCheckpoint, error)
	ListQuarantineItems(ctx context.Context, tenantID, runID string) ([]domain.QuarantineItem, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

// withTenant runs fn inside a transaction that has declared tenantID for
// RLS. Empty tenantID is refused outright — see forecasting-svc's
// middleware.go for why a fallback/default tenant here would be worse
// than no check at all.
func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

// mapErr translates raw immutability-trigger/lifecycle errors into typed
// domain errors so callers get ErrRunAlreadyClosed etc. rather than a raw
// Postgres message.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case strings.Contains(pgErr.Message, "is terminal") && strings.Contains(pgErr.Message, "immutable"):
		return fmt.Errorf("%w: %s", domain.ErrRunAlreadyClosed, pgErr.Message)
	case strings.Contains(pgErr.Message, "immutable"), strings.Contains(pgErr.Message, "cannot be deleted"):
		return fmt.Errorf("%w: %s", domain.ErrIngestionRunInvalidState, pgErr.Message)
	}
	return err
}

// claimIdempotency records claim before the change it guards, in the same
// transaction. A replay with the same key+request returns
// IdempotentReplayError; a replay with the same key but a different
// request is refused outright. tenantID comes from the already-declared
// app.tenant_id GUC (read back via current_setting, not re-threaded as a
// parameter) so the claim can never be recorded under a different tenant
// than the transaction actually declared.
func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

// ── IngestionRun ─────────────────────────────────────────────────────────────

const runColumns = `run_id, tenant_id, source_id, status, started_at, closed_at, checkpoint_ref,
	created_by, residency_region, classification, purpose`

func scanRun(row pgx.Row) (*domain.IngestionRun, error) {
	var r domain.IngestionRun
	if err := row.Scan(&r.RunID, &r.TenantID, &r.SourceID, &r.Status, &r.StartedAt, &r.ClosedAt,
		&r.CheckpointRef, &r.CreatedBy, &r.ResidencyRegion, &r.Classification, &r.Purpose); err != nil {
		return nil, err
	}
	return &r, nil
}

func loadRun(ctx context.Context, tx pgx.Tx, runID string, forUpdate bool) (*domain.IngestionRun, error) {
	q := `SELECT ` + runColumns + ` FROM ingestion_runs WHERE run_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanRun(tx.QueryRow(ctx, q, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIngestionRunNotFound
	}
	return r, err
}

// StartIngestion opens a new IngestionRun in Running state — Draft exists
// as a lifecycle state for callers that stage a run before starting it,
// but this command's own job is to start one, so it lands directly in
// Running (matching CommitBatch/QuarantineBatch's requirement that the run
// be Running or PartiallyQuarantined).
func (s *PgStore) StartIngestion(ctx context.Context, tenantID string, run *domain.IngestionRun, claim domain.IdempotencyClaim) (*domain.IngestionRun, error) {
	var out *domain.IngestionRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if run.RunID == "" {
			run.RunID = domain.PrefixIngestionRun + uuid.NewString()
		}
		run.TenantID = tenantID
		run.Status = domain.IngestionRunRunning
		run.StartedAt = time.Now().UTC()
		if err := domain.ValidateIngestionRun(run); err != nil {
			return err
		}
		got, err := scanRun(tx.QueryRow(ctx, `
			INSERT INTO ingestion_runs (run_id, tenant_id, source_id, status, started_at, created_by,
				residency_region, classification, purpose)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+runColumns,
			run.RunID, tenantID, run.SourceID, run.Status, run.StartedAt, run.CreatedBy,
			run.ResidencyRegion, run.Classification, run.Purpose))
		if err != nil {
			return err
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "ingestion_run", AggregateID: got.RunID,
			EventType: "DATA.IngestionStarted", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetRun(ctx context.Context, tenantID, runID string) (*domain.IngestionRun, error) {
	var out *domain.IngestionRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := loadRun(ctx, tx, runID, false)
		out = r
		return err
	})
	return out, err
}

// ── CommitBatch ──────────────────────────────────────────────────────────────

// CommitBatch validates a batch against the run's resolved residency and
// the source's registered schema version, dedupes records against
// landed_records, and lands only the genuinely-new ones. A
// schema-incompatible batch is quarantined WHOLE — never landed with the
// incompatible fields silently dropped (the doc's own named acceptance
// test: "quarantines without partial silent coercion").
func (s *PgStore) CommitBatch(ctx context.Context, tenantID string, req domain.CommitBatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.LandingObject, error) {
	if err := domain.ValidateCommitBatchRequest(&req); err != nil {
		return nil, err
	}
	var out *domain.LandingObject
	var quarantinedReason string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, req.RunID, true)
		if err != nil {
			return err
		}
		if run.Status != domain.IngestionRunRunning && run.Status != domain.IngestionRunPartiallyQuarantined {
			return domain.ErrRunNotRunning
		}
		// Negative path: wrong-region batch is blocked before landing —
		// never partially landed, never quarantined either (residency is a
		// hard boundary, not a data-quality concern). Checked (and claimed
		// idempotency-wise) before any write, so a repeated wrong-region
		// call is refused the same way every time, not just the first.
		if req.ResidencyRegion != run.ResidencyRegion {
			return domain.ErrWrongResidencyRegion
		}

		cp, err := loadCheckpointForUpdate(ctx, tx, tenantID, run.SourceID)
		if err != nil && !errors.Is(err, domain.ErrSourceCheckpointNotFound) {
			return err
		}

		// The idempotency claim is recorded here — after the read-only
		// checks above (no writes yet), but only once the outcome resource
		// (LandingObject vs QuarantineItem) is known, so the claim's
		// resource_id is the ID a caller can actually GetX() on replay,
		// not an ID from an earlier stage of the request.
		if cp != nil && cp.SchemaVersion != req.SchemaVersion {
			// Negative path: schema-incompatible batch quarantines whole,
			// never landed with a partial silent coercion. This is a
			// SUCCESSFUL completion of the transaction (the quarantine
			// write must commit), so it returns nil here — the caller is
			// told via quarantinedReason, set below, after commit.
			quarantineID := domain.PrefixQuarantineItem + uuid.NewString()
			claim.ResourceID = quarantineID
			if err := claimIdempotency(ctx, tx, claim); err != nil {
				return err
			}
			reason := fmt.Sprintf("schema_version %s does not match registered %s", req.SchemaVersion, cp.SchemaVersion)
			if _, err := s.insertQuarantineItemWithID(ctx, tx, quarantineID, tenantID, run, req.Records, req.SchemaVersion,
				req.Classification, req.ResidencyRegion, reason, actor); err != nil {
				return err
			}
			quarantinedReason = reason
			return nil
		}

		landingID := domain.PrefixLandingObject + uuid.NewString()
		claim.ResourceID = landingID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		now := time.Now().UTC()
		var landedCount int
		var landedRecords []domain.BatchRecord
		for _, rec := range req.Records {
			tag, err := tx.Exec(ctx, `
				INSERT INTO landed_records (tenant_id, source_id, dedup_key, landing_id, source_event_id, landed_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, source_id, dedup_key) DO NOTHING`,
				tenantID, run.SourceID, rec.DedupKey, landingID, rec.SourceEventID, now)
			if err != nil {
				return fmt.Errorf("insert landed record: %w", err)
			}
			if tag.RowsAffected() == 1 {
				landedCount++
				landedRecords = append(landedRecords, rec)
			}
		}

		contentHash := domain.ComputeContentHash(landedRecords)
		got, err := scanLandingObject(tx.QueryRow(ctx, `
			INSERT INTO landing_objects (landing_id, run_id, source_id, tenant_id, record_count, content_hash,
				schema_version, classification, residency_region, landed_at, landed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING `+landingColumns,
			landingID, run.RunID, run.SourceID, tenantID, landedCount, contentHash, req.SchemaVersion,
			req.Classification, req.ResidencyRegion, now, actor))
		if err != nil {
			return fmt.Errorf("insert landing object: %w", err)
		}
		out = got

		// Advance the checkpoint to the caller's declared next position —
		// the whole point of the checkpoint existing, so a restart resumes
		// exactly here rather than reprocessing or skipping.
		if _, err := tx.Exec(ctx, `
			INSERT INTO source_checkpoints (checkpoint_id, source_id, tenant_id, last_position, last_committed_at, schema_version, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $5)
			ON CONFLICT (source_id, tenant_id) DO UPDATE
				SET last_position = EXCLUDED.last_position, last_committed_at = EXCLUDED.last_committed_at,
				    schema_version = EXCLUDED.schema_version, updated_at = EXCLUDED.last_committed_at`,
			domain.PrefixSourceCheckpoint+uuid.NewString(), run.SourceID, tenantID, req.NextPosition, now, req.SchemaVersion); err != nil {
			return fmt.Errorf("advance checkpoint: %w", err)
		}

		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "landing_object", AggregateID: got.LandingID,
			EventType: "DATA.CheckpointAdvanced", TenantID: &tenantID, Payload: got})
	})
	if err != nil {
		return nil, err
	}
	if quarantinedReason != "" {
		return nil, fmt.Errorf("%w: %s", domain.ErrSchemaIncompatible, quarantinedReason)
	}
	return out, nil
}

func (s *PgStore) GetCheckpoint(ctx context.Context, tenantID, sourceID string) (*domain.SourceCheckpoint, error) {
	var out *domain.SourceCheckpoint
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cp, err := loadCheckpointForUpdate(ctx, tx, tenantID, sourceID)
		out = cp
		return err
	})
	return out, err
}

const checkpointColumns = `checkpoint_id, source_id, tenant_id, last_position, last_committed_at, schema_version, updated_at`

func scanCheckpoint(row pgx.Row) (*domain.SourceCheckpoint, error) {
	var c domain.SourceCheckpoint
	if err := row.Scan(&c.CheckpointID, &c.SourceID, &c.TenantID, &c.LastPosition, &c.LastCommittedAt,
		&c.SchemaVersion, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func loadCheckpointForUpdate(ctx context.Context, tx pgx.Tx, tenantID, sourceID string) (*domain.SourceCheckpoint, error) {
	cp, err := scanCheckpoint(tx.QueryRow(ctx, `SELECT `+checkpointColumns+`
		FROM source_checkpoints WHERE tenant_id = $1 AND source_id = $2`, tenantID, sourceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSourceCheckpointNotFound
	}
	return cp, err
}

const landingColumns = `landing_id, run_id, source_id, tenant_id, record_count, content_hash, schema_version,
	classification, residency_region, landed_at, landed_by`

func scanLandingObject(row pgx.Row) (*domain.LandingObject, error) {
	var l domain.LandingObject
	if err := row.Scan(&l.LandingID, &l.RunID, &l.SourceID, &l.TenantID, &l.RecordCount, &l.ContentHash,
		&l.SchemaVersion, &l.Classification, &l.ResidencyRegion, &l.LandedAt, &l.LandedBy); err != nil {
		return nil, err
	}
	return &l, nil
}

// ── QuarantineBatch ──────────────────────────────────────────────────────────

func (s *PgStore) QuarantineBatch(ctx context.Context, tenantID string, req domain.QuarantineBatchRequest, actor string, claim domain.IdempotencyClaim) (*domain.QuarantineItem, error) {
	if err := domain.ValidateQuarantineBatchRequest(&req); err != nil {
		return nil, err
	}
	var out *domain.QuarantineItem
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRun(ctx, tx, req.RunID, true)
		if err != nil {
			return err
		}
		if run.Status != domain.IngestionRunRunning && run.Status != domain.IngestionRunPartiallyQuarantined {
			return domain.ErrRunNotRunning
		}
		quarantineID := domain.PrefixQuarantineItem + uuid.NewString()
		claim.ResourceID = quarantineID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := s.insertQuarantineItemWithID(ctx, tx, quarantineID, tenantID, run, req.Records, req.SchemaVersion,
			req.Classification, req.ResidencyRegion, req.Reason, actor)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	return out, err
}

func (s *PgStore) insertQuarantineItemWithID(ctx context.Context, tx pgx.Tx, quarantineID, tenantID string, run *domain.IngestionRun,
	records []domain.BatchRecord, schemaVersion, classification, residencyRegion, reason, actor string) (*domain.QuarantineItem, error) {
	payloadRef := domain.ComputeContentHash(records)
	now := time.Now().UTC()
	got, err := scanQuarantineItem(tx.QueryRow(ctx, `
		INSERT INTO quarantine_items (quarantine_id, run_id, source_id, tenant_id, reason, payload_ref,
			schema_version, classification, residency_region, quarantined_at, quarantined_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+quarantineColumns,
		quarantineID, run.RunID, run.SourceID, tenantID, reason, payloadRef, schemaVersion, classification,
		residencyRegion, now, actor))
	if err != nil {
		return nil, fmt.Errorf("insert quarantine item: %w", err)
	}
	if run.Status == domain.IngestionRunRunning {
		if _, err := tx.Exec(ctx, `UPDATE ingestion_runs SET status = $2 WHERE run_id = $1`,
			run.RunID, domain.IngestionRunPartiallyQuarantined); err != nil {
			return nil, err
		}
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "quarantine_item", AggregateID: got.QuarantineID,
		EventType: "DATA.BatchQuarantined", TenantID: &tenantID, Payload: got}); err != nil {
		return nil, err
	}
	return got, nil
}

const quarantineColumns = `quarantine_id, run_id, source_id, tenant_id, reason, payload_ref, schema_version,
	classification, residency_region, quarantined_at, quarantined_by`

func scanQuarantineItem(row pgx.Row) (*domain.QuarantineItem, error) {
	var q domain.QuarantineItem
	if err := row.Scan(&q.QuarantineID, &q.RunID, &q.SourceID, &q.TenantID, &q.Reason, &q.PayloadRef,
		&q.SchemaVersion, &q.Classification, &q.ResidencyRegion, &q.QuarantinedAt, &q.QuarantinedBy); err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *PgStore) ListQuarantineItems(ctx context.Context, tenantID, runID string) ([]domain.QuarantineItem, error) {
	var out []domain.QuarantineItem
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+quarantineColumns+` FROM quarantine_items WHERE run_id = $1 ORDER BY quarantined_at`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			q, err := scanQuarantineItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *q)
		}
		return rows.Err()
	})
	return out, err
}

// ── ReplayFromCheckpoint / CloseRun ─────────────────────────────────────────

// ReplayFromCheckpoint rewinds (or advances) the source's checkpoint to an
// operator-supplied position — the explicit recovery path, distinct from
// CommitBatch's own forward advancement. Requires an existing checkpoint:
// there is nothing to replay from until a first batch has ever landed.
func (s *PgStore) ReplayFromCheckpoint(ctx context.Context, tenantID string, req domain.ReplayFromCheckpointRequest, actor string, claim domain.IdempotencyClaim) (*domain.SourceCheckpoint, error) {
	var out *domain.SourceCheckpoint
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		run, err := loadRun(ctx, tx, req.RunID, false)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		got, err := scanCheckpoint(tx.QueryRow(ctx, `
			UPDATE source_checkpoints SET last_position = $3, last_committed_at = $4, updated_at = $4
			WHERE tenant_id = $1 AND source_id = $2
			RETURNING `+checkpointColumns,
			tenantID, run.SourceID, req.Position, now))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSourceCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("replay checkpoint: %w", err)
		}
		out = got
		_ = actor
		return nil
	})
	return out, err
}

// CloseRun transitions a run to a terminal state (Completed/Failed/
// Superseded). The DB trigger enforces the transition is legal and, once
// applied, that the row can never change again.
func (s *PgStore) CloseRun(ctx context.Context, tenantID string, req domain.CloseRunRequest, finalStatus domain.IngestionRunStatus, actor string, claim domain.IdempotencyClaim) (*domain.IngestionRun, error) {
	var out *domain.IngestionRun
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		now := time.Now().UTC()
		got, err := scanRun(tx.QueryRow(ctx, `
			UPDATE ingestion_runs SET status = $2, closed_at = $3 WHERE run_id = $1
			RETURNING `+runColumns, req.RunID, finalStatus, now))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrIngestionRunNotFound
		}
		if err != nil {
			return err
		}
		out = got
		_ = actor
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "ingestion_run", AggregateID: got.RunID,
			EventType: "DATA.IngestionCompleted", TenantID: &tenantID, Payload: got})
	})
	return out, err
}
