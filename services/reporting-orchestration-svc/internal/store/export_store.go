package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// ExportStore is AUD-10's own persistence contract for this service — kept
// separate from Store above, same "self-contained composed interface"
// shape used for every other AUD-0X capability built this session. Only
// PgStore implements it: MemoryStore's whole reason to exist is a
// DB-unavailable fallback for the pre-existing report endpoints, and export
// approval/seal/deliver correctness genuinely depends on the transactional
// CAS predicates below — an in-memory equivalent would not be testing the
// real mechanism.
type ExportStore interface {
	CreateExportRequest(ctx context.Context, p domain.CreateExportRequestParams) (*domain.AuditExportRequest, error)
	GetExportRequest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, error)
	ApproveExport(ctx context.Context, p domain.ApproveExportParams) (*domain.AuditExportRequest, error)
	RecordManifestEntry(ctx context.Context, tenantID, exportID, artifactName, sha256 string) (*domain.ExportManifest, error)
	MarkExportBuilding(ctx context.Context, tenantID, exportID string) error
	MarkExportFailed(ctx context.Context, tenantID, exportID, reason string) error
	RecordRedaction(ctx context.Context, p domain.RecordRedactionParams) (*domain.RedactionDecision, error)
	SealExport(ctx context.Context, p domain.SealExportParams) (*domain.AuditExportRequest, error)
	DeliverExport(ctx context.Context, p domain.DeliverExportParams) (*domain.AuditExportRequest, error)
	RevokeExport(ctx context.Context, p domain.RevokeExportParams) (*domain.AuditExportRequest, error)
	ListManifest(ctx context.Context, tenantID, exportID string) ([]domain.ExportManifest, error)
}

const exportColumns = `export_id, tenant_id, legal_entity_id, archive_id, purpose, status,
	requested_by_principal_id, approved_by_principal_id, delivered_to_principal_id, failure_reason,
	created_at, approved_at, sealed_at, delivered_at, revoked_at`

func scanExport(row pgx.Row) (*domain.AuditExportRequest, error) {
	e := &domain.AuditExportRequest{}
	var failureReason *string
	err := row.Scan(&e.ExportID, &e.TenantID, &e.LegalEntityID, &e.ArchiveID, &e.Purpose, &e.Status,
		&e.RequestedByPrincipalID, &e.ApprovedByPrincipalID, &e.DeliveredToPrincipalID, &failureReason,
		&e.CreatedAt, &e.ApprovedAt, &e.SealedAt, &e.DeliveredAt, &e.RevokedAt)
	if failureReason != nil {
		e.FailureReason = *failureReason
	}
	return e, err
}

func (s *PgStore) exportSetTenant(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

func (s *PgStore) CreateExportRequest(ctx context.Context, p domain.CreateExportRequestParams) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO audit_export_requests
		(tenant_id, legal_entity_id, archive_id, purpose, requested_by_principal_id)
		VALUES ($1,$2,$3,$4,$5) RETURNING `+exportColumns,
		p.TenantID, p.LegalEntityID, p.ArchiveID, p.Purpose, p.RequestedByPrincipalID)
	e, err := scanExport(row)
	if err != nil {
		return nil, fmt.Errorf("insert audit export request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) GetExportRequest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	e, err := scanExport(tx.QueryRow(ctx, `SELECT `+exportColumns+` FROM audit_export_requests WHERE export_id=$1`, exportID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrExportNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ApproveExport is the real, DB-enforced proof of AUD-10's
// approval-segregation control: the CAS predicate below refuses if the
// approver is the same principal who requested the export (defense in
// depth alongside migration 000002's own CHECK constraint), and only
// succeeds from REQUESTED.
func (s *PgStore) ApproveExport(ctx context.Context, p domain.ApproveExportParams) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE audit_export_requests SET status=$1, approved_by_principal_id=$2, approved_at=now()
		WHERE export_id=$3 AND status=$4 AND requested_by_principal_id <> $2
		RETURNING `+exportColumns, domain.ExportApproved, p.ApprovedByPrincipalID, p.ExportID, domain.ExportRequested)
	e, err := scanExport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var requestedBy string
		chkErr := tx.QueryRow(ctx, `SELECT requested_by_principal_id FROM audit_export_requests WHERE export_id=$1`, p.ExportID).Scan(&requestedBy)
		if errors.Is(chkErr, pgx.ErrNoRows) {
			return nil, domain.ErrExportNotFound
		}
		if chkErr != nil {
			return nil, chkErr
		}
		if requestedBy == p.ApprovedByPrincipalID {
			return nil, domain.ErrExportSelfApproval
		}
		return nil, domain.ErrExportInvalidState
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) MarkExportBuilding(ctx context.Context, tenantID, exportID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `UPDATE audit_export_requests SET status=$1 WHERE export_id=$2 AND status=$3`,
		domain.ExportBuilding, exportID, domain.ExportApproved)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return domain.ErrExportInvalidState
	}
	return tx.Commit(ctx)
}

func (s *PgStore) MarkExportFailed(ctx context.Context, tenantID, exportID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE audit_export_requests SET status=$1, failure_reason=$2 WHERE export_id=$3 AND status=$4`,
		domain.ExportFailed, reason, exportID, domain.ExportBuilding)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgStore) RecordManifestEntry(ctx context.Context, tenantID, exportID, artifactName, sha256 string) (*domain.ExportManifest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	// export_id belongs to this tenant only if the RLS-scoped SELECT here
	// finds it — the child table has no tenant_id of its own (see migration
	// 000002's header), so this existence check under the same app.tenant_id
	// is the actual tenant-isolation mechanism for every child-table write.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id=$1)`, exportID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, domain.ErrExportNotFound
	}
	row := tx.QueryRow(ctx, `INSERT INTO export_manifests (export_id, artifact_name, sha256) VALUES ($1,$2,$3)
		RETURNING manifest_id, export_id, artifact_name, sha256, recorded_at`, exportID, artifactName, sha256)
	m := &domain.ExportManifest{}
	if err := row.Scan(&m.ManifestID, &m.ExportID, &m.ArtifactName, &m.Sha256, &m.RecordedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) RecordRedaction(ctx context.Context, p domain.RecordRedactionParams) (*domain.RedactionDecision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id=$1)`, p.ExportID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, domain.ErrExportNotFound
	}
	row := tx.QueryRow(ctx, `INSERT INTO redaction_decisions (export_id, field_or_scope, reason, decided_by_principal_id)
		VALUES ($1,$2,$3,$4) RETURNING decision_id, export_id, field_or_scope, reason, decided_by_principal_id, decided_at`,
		p.ExportID, p.FieldOrScope, p.Reason, p.DecidedByPrincipalID)
	d := &domain.RedactionDecision{}
	if err := row.Scan(&d.DecisionID, &d.ExportID, &d.FieldOrScope, &d.Reason, &d.DecidedByPrincipalID, &d.DecidedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// SealExport is the real, DB-enforced proof of "no empty package": the CAS
// predicate below only succeeds when at least one manifest row exists.
func (s *PgStore) SealExport(ctx context.Context, p domain.SealExportParams) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE audit_export_requests SET status=$1, sealed_at=now()
		WHERE export_id=$2 AND status=$3 AND EXISTS(SELECT 1 FROM export_manifests WHERE export_id=$2)
		RETURNING `+exportColumns, domain.ExportSealed, p.ExportID, domain.ExportBuilding)
	e, err := scanExport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists, hasManifest bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id=$1)`, p.ExportID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrExportNotFound
		}
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM export_manifests WHERE export_id=$1)`, p.ExportID).Scan(&hasManifest); chkErr != nil {
			return nil, chkErr
		}
		if !hasManifest {
			return nil, domain.ErrManifestRequired
		}
		return nil, domain.ErrExportInvalidState
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) DeliverExport(ctx context.Context, p domain.DeliverExportParams) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE audit_export_requests SET status=$1, delivered_to_principal_id=$2, delivered_at=now()
		WHERE export_id=$3 AND status=$4 RETURNING `+exportColumns,
		domain.ExportDelivered, p.DeliveredToPrincipalID, p.ExportID, domain.ExportSealed)
	e, err := scanExport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id=$1)`, p.ExportID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrExportNotFound
		}
		return nil, domain.ErrExportInvalidState
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delivery_receipts (export_id, delivered_to_principal_id, delivery_channel)
		VALUES ($1,$2,$3)`, p.ExportID, p.DeliveredToPrincipalID, p.DeliveryChannel); err != nil {
		return nil, fmt.Errorf("insert delivery receipt for export %s: %w", p.ExportID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) RevokeExport(ctx context.Context, p domain.RevokeExportParams) (*domain.AuditExportRequest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, p.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `UPDATE audit_export_requests SET status=$1, revoked_at=now()
		WHERE export_id=$2 AND status=$3 RETURNING `+exportColumns,
		domain.ExportRevoked, p.ExportID, domain.ExportDelivered)
	e, err := scanExport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id=$1)`, p.ExportID).Scan(&exists); chkErr != nil {
			return nil, chkErr
		}
		if !exists {
			return nil, domain.ErrExportNotFound
		}
		return nil, domain.ErrExportInvalidState
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *PgStore) ListManifest(ctx context.Context, tenantID, exportID string) ([]domain.ExportManifest, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.exportSetTenant(ctx, tx, tenantID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT manifest_id, export_id, artifact_name, sha256, recorded_at
		FROM export_manifests WHERE export_id=$1 ORDER BY recorded_at`, exportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExportManifest
	for rows.Next() {
		var m domain.ExportManifest
		if err := rows.Scan(&m.ManifestID, &m.ExportID, &m.ArtifactName, &m.Sha256, &m.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
