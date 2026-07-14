// Package store is document-vault-svc's Postgres persistence layer. It never
// touches document bytes — that's internal/storage's job. This package only
// ever holds metadata: current-state pointers, append-only version lineage,
// and append-only access history.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// CreateDocument inserts the document row and its first version in one
// transaction — a document is never left pointing at a version that doesn't
// exist.
func (s *PgStore) CreateDocument(ctx context.Context, doc *domain.Document, firstVersion *domain.DocumentVersion) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		INSERT INTO documents (tenant_id, legal_entity_id, title, classification, retention_policy,
			residency_region_code, current_version, status, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 'ACTIVE', $7)
		RETURNING document_id, created_at, updated_at
	`, doc.TenantID, doc.LegalEntityID, doc.Title, string(doc.Classification), doc.RetentionPolicy,
		doc.ResidencyRegionCode, doc.CreatedByPrincipalID,
	).Scan(&doc.DocumentID, &doc.CreatedAt, &doc.UpdatedAt)
	if err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	doc.CurrentVersion = 1
	doc.Status = domain.StatusActive

	firstVersion.DocumentID = doc.DocumentID
	firstVersion.Version = 1
	err = tx.QueryRow(ctx, `
		INSERT INTO document_versions (document_id, version, checksum_sha256, storage_key, size_bytes,
			content_type, created_by_principal_id)
		VALUES ($1, 1, $2, $3, $4, $5, $6)
		RETURNING document_version_id, created_at
	`, firstVersion.DocumentID, firstVersion.ChecksumSHA256, firstVersion.StorageKey, firstVersion.SizeBytes,
		firstVersion.ContentType, firstVersion.CreatedByPrincipalID,
	).Scan(&firstVersion.DocumentVersionID, &firstVersion.CreatedAt)
	if err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}

	return tx.Commit(ctx)
}

// AddVersion appends a new immutable version row and bumps documents.current_version
// — the ONLY mutation ever applied to the documents row post-creation.
func (s *PgStore) AddVersion(ctx context.Context, documentID string, v *domain.DocumentVersion) (*domain.Document, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	defer tx.Rollback(ctx)

	var nextVersion int
	err = tx.QueryRow(ctx, `
		UPDATE documents SET current_version = current_version + 1, updated_at = now()
		WHERE document_id = $1
		RETURNING current_version
	`, documentID).Scan(&nextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}

	v.DocumentID = documentID
	v.Version = nextVersion
	err = tx.QueryRow(ctx, `
		INSERT INTO document_versions (document_id, version, checksum_sha256, storage_key, size_bytes,
			content_type, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING document_version_id, created_at
	`, documentID, nextVersion, v.ChecksumSHA256, v.StorageKey, v.SizeBytes, v.ContentType, v.CreatedByPrincipalID,
	).Scan(&v.DocumentVersionID, &v.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	return s.FindDocumentByID(ctx, documentID)
}

func (s *PgStore) FindDocumentByID(ctx context.Context, documentID string) (*domain.Document, error) {
	var d domain.Document
	err := s.pool.QueryRow(ctx, `
		SELECT document_id, tenant_id, legal_entity_id, title, classification, retention_policy,
			residency_region_code, current_version, status, created_by_principal_id, created_at, updated_at
		FROM documents WHERE document_id = $1
	`, documentID).Scan(&d.DocumentID, &d.TenantID, &d.LegalEntityID, &d.Title, &d.Classification, &d.RetentionPolicy,
		&d.ResidencyRegionCode, &d.CurrentVersion, &d.Status, &d.CreatedByPrincipalID, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	return &d, nil
}

func (s *PgStore) FindVersion(ctx context.Context, documentID string, version int) (*domain.DocumentVersion, error) {
	var v domain.DocumentVersion
	err := s.pool.QueryRow(ctx, `
		SELECT document_version_id, document_id, version, checksum_sha256, storage_key, size_bytes,
			content_type, created_by_principal_id, created_at
		FROM document_versions WHERE document_id = $1 AND version = $2
	`, documentID, version).Scan(&v.DocumentVersionID, &v.DocumentID, &v.Version, &v.ChecksumSHA256, &v.StorageKey,
		&v.SizeBytes, &v.ContentType, &v.CreatedByPrincipalID, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDocumentVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	return &v, nil
}

func (s *PgStore) ListVersions(ctx context.Context, documentID string) ([]domain.DocumentVersion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT document_version_id, document_id, version, checksum_sha256, storage_key, size_bytes,
			content_type, created_by_principal_id, created_at
		FROM document_versions WHERE document_id = $1 ORDER BY version ASC
	`, documentID)
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	defer rows.Close()

	var out []domain.DocumentVersion
	for rows.Next() {
		var v domain.DocumentVersion
		if err := rows.Scan(&v.DocumentVersionID, &v.DocumentID, &v.Version, &v.ChecksumSHA256, &v.StorageKey,
			&v.SizeBytes, &v.ContentType, &v.CreatedByPrincipalID, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("document store unavailable: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecordAccess appends one row to the access log. This is called on EVERY
// read (metadata or download) — never skipped, never batched away — per the
// append-only access-history doctrine.
func (s *PgStore) RecordAccess(ctx context.Context, log *domain.DocumentAccessLog) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO document_access_log (document_id, document_version_id, accessed_by_principal_id,
			access_type, correlation_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING access_log_id, accessed_at
	`, log.DocumentID, log.DocumentVersionID, log.AccessedByPrincipalID, string(log.AccessType), log.CorrelationID,
	).Scan(&log.AccessLogID, &log.AccessedAt)
	if err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	return nil
}

func (s *PgStore) ListAccessLog(ctx context.Context, documentID string) ([]domain.DocumentAccessLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT access_log_id, document_id, document_version_id, accessed_by_principal_id, access_type,
			correlation_id, accessed_at
		FROM document_access_log WHERE document_id = $1 ORDER BY accessed_at ASC
	`, documentID)
	if err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", err)
	}
	defer rows.Close()

	var out []domain.DocumentAccessLog
	for rows.Next() {
		var a domain.DocumentAccessLog
		if err := rows.Scan(&a.AccessLogID, &a.DocumentID, &a.DocumentVersionID, &a.AccessedByPrincipalID,
			&a.AccessType, &a.CorrelationID, &a.AccessedAt); err != nil {
			return nil, fmt.Errorf("document store unavailable: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
