package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

// FindVersionByID reads ONE exact, immutable version by its own identity
// (ZS-SVC-S-001 §3.6: GET /v1/document-versions/{version_id} — "No 'latest'
// substitution"). It never resolves a document's current or latest version:
// the id names a version or the answer is ErrDocumentVersionNotFound.
//
// The version row carries no tenant_id (see FindVersion), so the owning
// document is confirmed to belong to the caller's tenant in the SAME statement,
// in addition to the row-level-security policy on document_versions. A version
// of another tenant is therefore indistinguishable from a version that does not
// exist, and an id that is not a UUID is simply "not found" rather than a
// database error.
func (s *PgStore) FindVersionByID(ctx context.Context, versionID string) (*domain.DocumentVersion, error) {
	if _, err := uuid.Parse(versionID); err != nil {
		return nil, domain.ErrDocumentVersionNotFound
	}
	var v domain.DocumentVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+versionColumns+`
			FROM document_versions
			WHERE document_version_id = $1
			  AND EXISTS (SELECT 1 FROM documents d
			               WHERE d.document_id = document_versions.document_id
			                 AND d.tenant_id = $2::uuid)`, versionID, tenantID)
		if err := scanVersion(row, &v); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentVersionNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}
