// Package store is evidence-manifest-svc's Postgres persistence layer.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) CreateManifest(ctx context.Context, m *domain.EvidenceManifest) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO evidence_manifests (tenant_id, legal_entity_id, scenario_type, requested_by, status)
		VALUES ($1, $2, $3, $4, 'PENDING')
		RETURNING manifest_id, requested_at
	`, m.TenantID, m.LegalEntityID, string(m.ScenarioType), m.RequestedBy).Scan(&m.ManifestID, &m.RequestedAt)
	if err != nil {
		return fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	m.Status = domain.StatusPending
	return nil
}

// AddRecord appends one immutable ManifestRecord — never updated, never
// deleted, same doctrine as every other evidential store in this platform.
func (s *PgStore) AddRecord(ctx context.Context, r *domain.ManifestRecord) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO manifest_records (manifest_id, source_type, source_record_id, record_snapshot)
		VALUES ($1, $2, $3, $4)
		RETURNING manifest_record_id, fetched_at
	`, r.ManifestID, string(r.SourceType), r.SourceRecordID, r.RecordSnapshot,
	).Scan(&r.ManifestRecordID, &r.FetchedAt)
	if err != nil {
		return fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return nil
}

// FinalizeGenerated marks a manifest GENERATED with its checksum. This is the
// LAST mutation ever applied to a manifest row — after this, it and its
// records are immutable evidence.
func (s *PgStore) FinalizeGenerated(ctx context.Context, manifestID, checksumSHA256 string) (*domain.EvidenceManifest, error) {
	m, err := s.finalize(ctx, manifestID, `
		UPDATE evidence_manifests SET status = 'GENERATED', checksum_sha256 = $2, generated_at = now()
		WHERE manifest_id = $1
		RETURNING manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
			checksum_sha256, failure_reason, requested_at, generated_at
	`, checksumSHA256)
	return m, err
}

// FinalizeFailed marks a manifest FAILED with a reason — still a terminal,
// immutable state; a retry creates a brand-new manifest, it never resurrects
// this one.
func (s *PgStore) FinalizeFailed(ctx context.Context, manifestID, reason string) (*domain.EvidenceManifest, error) {
	m, err := s.finalize(ctx, manifestID, `
		UPDATE evidence_manifests SET status = 'FAILED', failure_reason = $2
		WHERE manifest_id = $1
		RETURNING manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
			checksum_sha256, failure_reason, requested_at, generated_at
	`, reason)
	return m, err
}

func (s *PgStore) finalize(ctx context.Context, manifestID, query, arg2 string) (*domain.EvidenceManifest, error) {
	var m domain.EvidenceManifest
	err := s.pool.QueryRow(ctx, query, manifestID, arg2).Scan(
		&m.ManifestID, &m.TenantID, &m.LegalEntityID, &m.ScenarioType, &m.RequestedBy, &m.Status,
		&m.ChecksumSHA256, &m.FailureReason, &m.RequestedAt, &m.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManifestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return &m, nil
}

func (s *PgStore) FindManifestByID(ctx context.Context, manifestID string) (*domain.EvidenceManifest, error) {
	var m domain.EvidenceManifest
	err := s.pool.QueryRow(ctx, `
		SELECT manifest_id, tenant_id, legal_entity_id, scenario_type, requested_by, status,
			checksum_sha256, failure_reason, requested_at, generated_at
		FROM evidence_manifests WHERE manifest_id = $1
	`, manifestID).Scan(&m.ManifestID, &m.TenantID, &m.LegalEntityID, &m.ScenarioType, &m.RequestedBy, &m.Status,
		&m.ChecksumSHA256, &m.FailureReason, &m.RequestedAt, &m.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrManifestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	return &m, nil
}

func (s *PgStore) ListRecords(ctx context.Context, manifestID string) ([]domain.ManifestRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT manifest_record_id, manifest_id, source_type, source_record_id, record_snapshot, fetched_at
		FROM manifest_records WHERE manifest_id = $1 ORDER BY fetched_at ASC
	`, manifestID)
	if err != nil {
		return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
	}
	defer rows.Close()

	var out []domain.ManifestRecord
	for rows.Next() {
		var r domain.ManifestRecord
		if err := rows.Scan(&r.ManifestRecordID, &r.ManifestID, &r.SourceType, &r.SourceRecordID,
			&r.RecordSnapshot, &r.FetchedAt); err != nil {
			return nil, fmt.Errorf("evidence manifest store unavailable: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
