// Package store is the PgStore persistence layer for
// analytical-data-platform-svc (DATA-04, ZS-SVC-N-001 §4). Every method
// runs inside one transaction that first declares app.tenant_id for RLS,
// then performs the write.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/analytical-data-platform-svc/internal/domain"
	"zoiko.io/analytical-data-platform-svc/internal/outbox"
)

// Store is the DATA-04 persistence contract.
type Store interface {
	CreateDatasetVersion(ctx context.Context, tenantID string, req domain.CreateDatasetVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.DatasetVersion, error)
	BuildSnapshot(ctx context.Context, tenantID string, req domain.BuildSnapshotRequest, actor string, claim domain.IdempotencyClaim) (*domain.Snapshot, error)
	RebuildPartition(ctx context.Context, tenantID string, req domain.RebuildPartitionRequest, actor string, claim domain.IdempotencyClaim) (*domain.Partition, error)
	PublishDatasetVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.DataProductCertification, error)
	DeprecateDataset(ctx context.Context, tenantID, versionID, reason, actor string, claim domain.IdempotencyClaim) (*domain.DatasetVersion, error)

	GetDatasetVersion(ctx context.Context, tenantID, versionID string) (*domain.DatasetVersion, error)
	GetLatestSnapshot(ctx context.Context, tenantID, versionID string) (*domain.Snapshot, error)
	GetPartitions(ctx context.Context, tenantID, versionID string) ([]domain.Partition, error)
	GetCertification(ctx context.Context, tenantID, versionID string) (*domain.DataProductCertification, error)
	GetFreshness(ctx context.Context, tenantID, versionID string, asOf time.Time) (*domain.Freshness, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

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

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case strings.Contains(pgErr.Message, "is") && strings.Contains(pgErr.Message, "immutable") && strings.Contains(pgErr.Message, "Published"):
		return fmt.Errorf("%w: %s", domain.ErrVersionPublished, pgErr.Message)
	case strings.Contains(pgErr.Message, "immutable"), strings.Contains(pgErr.Message, "cannot be deleted"):
		return fmt.Errorf("%w: %s", domain.ErrImmutableViolation, pgErr.Message)
	}
	return err
}

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

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

// ── CreateDatasetVersion ─────────────────────────────────────────────────────

const versionColumns = `version_id, tenant_id, dataset_id, version_number, schema_definition, transformation_version,
	source_checkpoint_ref, residency_region, classification, max_staleness_seconds, status, created_at, created_by,
	published_at, published_by`

func scanVersion(row pgx.Row) (*domain.DatasetVersion, error) {
	var v domain.DatasetVersion
	var schemaJSON []byte
	if err := row.Scan(&v.VersionID, &v.TenantID, &v.DatasetID, &v.VersionNumber, &schemaJSON, &v.TransformationVersion,
		&v.SourceCheckpointRef, &v.ResidencyRegion, &v.Classification, &v.MaxStalenessSeconds, &v.Status, &v.CreatedAt,
		&v.CreatedBy, &v.PublishedAt, &v.PublishedBy); err != nil {
		return nil, err
	}
	if len(schemaJSON) > 0 {
		if err := json.Unmarshal(schemaJSON, &v.SchemaDefinition); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

func loadVersion(ctx context.Context, tx pgx.Tx, versionID string, forUpdate bool) (*domain.DatasetVersion, error) {
	q := `SELECT ` + versionColumns + ` FROM dataset_versions WHERE version_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	v, err := scanVersion(tx.QueryRow(ctx, q, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDatasetVersionNotFound
	}
	return v, err
}

func (s *PgStore) GetDatasetVersion(ctx context.Context, tenantID, versionID string) (*domain.DatasetVersion, error) {
	var out *domain.DatasetVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadVersion(ctx, tx, versionID, false)
		out = v
		return err
	})
	return out, err
}

// CreateDatasetVersion finds-or-creates the named dataset, then always
// creates a NEW version row (Draft) — the identity/name is stable, but
// content always versions forward.
func (s *PgStore) CreateDatasetVersion(ctx context.Context, tenantID string, req domain.CreateDatasetVersionRequest, actor string, claim domain.IdempotencyClaim) (*domain.DatasetVersion, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.DatasetVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		datasetID := newID(domain.PrefixDataset)
		tag, err := tx.Exec(ctx, `
			INSERT INTO analytical_datasets (dataset_id, tenant_id, name, created_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, name) DO NOTHING`,
			datasetID, tenantID, req.DatasetName, actor)
		if err != nil {
			return fmt.Errorf("find-or-create dataset: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT dataset_id FROM analytical_datasets WHERE tenant_id = $1 AND name = $2`,
				tenantID, req.DatasetName).Scan(&datasetID); err != nil {
				return fmt.Errorf("look up dataset: %w", err)
			}
		}

		versionID := newID(domain.PrefixDatasetVersion)
		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		var nextVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM dataset_versions WHERE tenant_id = $1 AND dataset_id = $2`,
			tenantID, datasetID).Scan(&nextVersion); err != nil {
			return fmt.Errorf("compute next version number: %w", err)
		}

		schemaJSON, err := json.Marshal(req.SchemaDefinition)
		if err != nil {
			return err
		}
		checkpointRef := "" // set by the first BuildSnapshot call's watermark evidence
		got, err := scanVersion(tx.QueryRow(ctx, `
			INSERT INTO dataset_versions (version_id, tenant_id, dataset_id, version_number, schema_definition,
				transformation_version, source_checkpoint_ref, residency_region, classification, max_staleness_seconds,
				status, created_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, 'Draft', $11)
			RETURNING `+versionColumns,
			versionID, tenantID, datasetID, nextVersion, schemaJSON, req.TransformationVersion, checkpointRef,
			req.ResidencyRegion, req.Classification, req.MaxStalenessSeconds, actor))
		if err != nil {
			return fmt.Errorf("insert dataset version: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dataset_version", AggregateID: got.VersionID,
			EventType: "DATA.DatasetVersionCreated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── BuildSnapshot ────────────────────────────────────────────────────────────

const snapshotColumns = `snapshot_id, tenant_id, version_id, watermark, row_count, content_hash, built_at, built_by`

func scanSnapshot(row pgx.Row) (*domain.Snapshot, error) {
	var sn domain.Snapshot
	if err := row.Scan(&sn.SnapshotID, &sn.TenantID, &sn.VersionID, &sn.Watermark, &sn.RowCount, &sn.ContentHash,
		&sn.BuiltAt, &sn.BuiltBy); err != nil {
		return nil, err
	}
	return &sn, nil
}

// BuildSnapshot materializes a version's data. Refused outright once the
// version is Published/Deprecated/Quarantined — a rebuild of published
// content is always a brand new version (the doc's own named acceptance
// test), never another snapshot bolted onto the one already live.
func (s *PgStore) BuildSnapshot(ctx context.Context, tenantID string, req domain.BuildSnapshotRequest, actor string, claim domain.IdempotencyClaim) (*domain.Snapshot, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.Snapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, req.VersionID, true)
		if err != nil {
			return err
		}
		if version.Status != domain.VersionDraft && version.Status != domain.VersionBuilding && version.Status != domain.VersionValidated {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		snapshotID := newID(domain.PrefixSnapshot)
		claim.ResourceID = snapshotID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		now := time.Now().UTC()
		got, err := scanSnapshot(tx.QueryRow(ctx, `
			INSERT INTO dataset_snapshots (snapshot_id, tenant_id, version_id, watermark, row_count, content_hash, built_at, built_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+snapshotColumns,
			snapshotID, tenantID, req.VersionID, req.Watermark, req.RowCount, req.ContentHash, now, actor))
		if err != nil {
			return fmt.Errorf("insert snapshot: %w", err)
		}

		if version.Status != domain.VersionValidated {
			if _, err := tx.Exec(ctx, `UPDATE dataset_versions SET status = 'Validated', source_checkpoint_ref = $2 WHERE version_id = $1`,
				req.VersionID, req.Watermark); err != nil {
				return fmt.Errorf("mark version validated: %w", err)
			}
		}

		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dataset_snapshot", AggregateID: got.SnapshotID,
			EventType: "DATA.SnapshotBuilt", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetLatestSnapshot(ctx context.Context, tenantID, versionID string) (*domain.Snapshot, error) {
	var out *domain.Snapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sn, err := loadLatestSnapshot(ctx, tx, versionID)
		out = sn
		return err
	})
	return out, err
}

func loadLatestSnapshot(ctx context.Context, tx pgx.Tx, versionID string) (*domain.Snapshot, error) {
	sn, err := scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+`
		FROM dataset_snapshots WHERE version_id = $1 ORDER BY built_at DESC LIMIT 1`, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoSnapshotYet
	}
	return sn, err
}

// ── RebuildPartition ─────────────────────────────────────────────────────────

const partitionColumns = `partition_id, tenant_id, version_id, snapshot_id, partition_key, row_count, content_hash, built_at, built_by`

func scanPartition(row pgx.Row) (*domain.Partition, error) {
	var p domain.Partition
	if err := row.Scan(&p.PartitionID, &p.TenantID, &p.VersionID, &p.SnapshotID, &p.PartitionKey, &p.RowCount,
		&p.ContentHash, &p.BuiltAt, &p.BuiltBy); err != nil {
		return nil, err
	}
	return &p, nil
}

// RebuildPartition always creates a NEW partition row for the given key —
// reproducible rebuild, never an in-place edit — and is refused once the
// owning version is Published/Deprecated/Quarantined, same doctrine as
// BuildSnapshot.
func (s *PgStore) RebuildPartition(ctx context.Context, tenantID string, req domain.RebuildPartitionRequest, actor string, claim domain.IdempotencyClaim) (*domain.Partition, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.Partition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, req.VersionID, false)
		if err != nil {
			return err
		}
		if version.Status != domain.VersionBuilding && version.Status != domain.VersionValidated {
			return fmt.Errorf("%w: version is %s", domain.ErrVersionPublished, version.Status)
		}

		var snapshotExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dataset_snapshots WHERE snapshot_id = $1 AND version_id = $2)`,
			req.SnapshotID, req.VersionID).Scan(&snapshotExists); err != nil {
			return err
		}
		if !snapshotExists {
			return domain.ErrSnapshotNotFound
		}

		partitionID := newID(domain.PrefixPartition)
		claim.ResourceID = partitionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		now := time.Now().UTC()
		got, err := scanPartition(tx.QueryRow(ctx, `
			INSERT INTO dataset_partitions (partition_id, tenant_id, version_id, snapshot_id, partition_key, row_count, content_hash, built_at, built_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+partitionColumns,
			partitionID, tenantID, req.VersionID, req.SnapshotID, req.PartitionKey, req.RowCount, req.ContentHash, now, actor))
		if err != nil {
			return fmt.Errorf("insert partition: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dataset_partition", AggregateID: got.PartitionID,
			EventType: "DATA.PartitionRebuilt", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) GetPartitions(ctx context.Context, tenantID, versionID string) ([]domain.Partition, error) {
	var out []domain.Partition
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+partitionColumns+` FROM dataset_partitions WHERE version_id = $1 ORDER BY built_at`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPartition(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// ── PublishDatasetVersion ────────────────────────────────────────────────────

// PublishDatasetVersion moves a Validated version to Published and seals
// a DataProductCertification capturing the latest snapshot's evidence.
// Once published, the DB trigger makes the version row (and every
// snapshot/partition under it) immutable outright.
func (s *PgStore) PublishDatasetVersion(ctx context.Context, tenantID, versionID, actor string, claim domain.IdempotencyClaim) (*domain.DataProductCertification, error) {
	var out *domain.DataProductCertification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if version.Status != domain.VersionValidated {
			return domain.ErrVersionNotValidated
		}
		snapshot, err := loadLatestSnapshot(ctx, tx, versionID)
		if err != nil {
			return err
		}

		certID := newID(domain.PrefixCertification)
		claim.ResourceID = certID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		summary := domain.CertificationSummary{
			VersionID: version.VersionID, DatasetID: version.DatasetID, VersionNumber: version.VersionNumber,
			SnapshotID: snapshot.SnapshotID, Watermark: snapshot.Watermark, RowCount: snapshot.RowCount,
			ContentHash: snapshot.ContentHash, TransformationVersion: version.TransformationVersion,
		}
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(summaryJSON)
		summarySHA256 := hex.EncodeToString(sum[:])
		now := time.Now().UTC()

		var cert domain.DataProductCertification
		if err := tx.QueryRow(ctx, `
			INSERT INTO data_product_certifications (certification_id, tenant_id, version_id, summary, summary_sha256, certified_at, certified_by)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
			RETURNING certification_id, tenant_id, version_id, summary_sha256, certified_at, certified_by`,
			certID, tenantID, versionID, summaryJSON, summarySHA256, now, actor,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.VersionID, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy); err != nil {
			return fmt.Errorf("insert certification: %w", err)
		}
		cert.Summary = summary

		if _, err := tx.Exec(ctx, `UPDATE dataset_versions SET status = 'Published', published_at = $2, published_by = $3 WHERE version_id = $1`,
			versionID, now, actor); err != nil {
			return fmt.Errorf("mark version published: %w", err)
		}

		out = &cert
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "data_product_certification", AggregateID: cert.CertificationID,
			EventType: "DATA.DatasetPublished", TenantID: &tenantID, Payload: cert})
	})
	return out, err
}

func (s *PgStore) GetCertification(ctx context.Context, tenantID, versionID string) (*domain.DataProductCertification, error) {
	var out *domain.DataProductCertification
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var cert domain.DataProductCertification
		var summaryJSON []byte
		err := tx.QueryRow(ctx, `SELECT certification_id, tenant_id, version_id, summary, summary_sha256, certified_at, certified_by
			FROM data_product_certifications WHERE version_id = $1`, versionID,
		).Scan(&cert.CertificationID, &cert.TenantID, &cert.VersionID, &summaryJSON, &cert.SummarySHA256, &cert.CertifiedAt, &cert.CertifiedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCertificationNotFound
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(summaryJSON, &cert.Summary); err != nil {
			return err
		}
		out = &cert
		return nil
	})
	return out, err
}

// ── DeprecateDataset ─────────────────────────────────────────────────────────

func (s *PgStore) DeprecateDataset(ctx context.Context, tenantID, versionID, reason, actor string, claim domain.IdempotencyClaim) (*domain.DatasetVersion, error) {
	var out *domain.DatasetVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, versionID, true)
		if err != nil {
			return err
		}
		if version.Status != domain.VersionPublished && version.Status != domain.VersionValidated {
			return domain.ErrVersionNotDeprecatable
		}
		claim.ResourceID = versionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanVersion(tx.QueryRow(ctx, `
			UPDATE dataset_versions SET status = 'Deprecated' WHERE version_id = $1
			RETURNING `+versionColumns, versionID))
		if err != nil {
			return fmt.Errorf("deprecate version: %w", err)
		}
		out = got
		_ = reason
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dataset_version", AggregateID: got.VersionID,
			EventType: "DATA.DatasetDeprecated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── GetFreshness ─────────────────────────────────────────────────────────────

// GetFreshness is a pure read: it computes staleness against asOf without
// mutating anything or emitting an event — a monitoring caller decides
// what to do with a stale result, this service just reports it honestly.
func (s *PgStore) GetFreshness(ctx context.Context, tenantID, versionID string, asOf time.Time) (*domain.Freshness, error) {
	var out *domain.Freshness
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		version, err := loadVersion(ctx, tx, versionID, false)
		if err != nil {
			return err
		}
		snapshot, err := loadLatestSnapshot(ctx, tx, versionID)
		if err != nil {
			return err
		}
		staleness := int64(asOf.Sub(snapshot.BuiltAt).Seconds())
		out = &domain.Freshness{
			VersionID: versionID, LatestSnapshotID: snapshot.SnapshotID, StalenessSeconds: staleness,
			MaxStalenessSeconds: version.MaxStalenessSeconds, IsStale: staleness > version.MaxStalenessSeconds,
		}
		return nil
	})
	return out, err
}
