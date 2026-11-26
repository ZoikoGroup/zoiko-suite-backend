package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reporting-orchestration-svc/internal/domain"
)

// AUD-10 export persistence.
//
// Every transition follows the same four steps inside one transaction:
//
//  1. lockExportForTransition: SELECT ... FOR UPDATE (404 if missing or
//     another tenant's), check If-Match (412), check the current status is
//     an allowed "from" state (409);
//  2. the transition-specific UPDATE (bumping version);
//  3. any append-only evidence row (manifest, redaction, receipt);
//  4. the outbox event.

const exportColumns = `export_id::text, tenant_id, legal_entity_id, archive_id, purpose, status, version,
	requested_by_principal_id, approved_by_principal_id, sealed_by_principal_id, delivered_to_principal_id,
	revoked_by_principal_id, COALESCE(revocation_reason, ''), COALESCE(seal_digest, ''), COALESCE(failure_reason, ''),
	created_at, approved_at, sealed_at, delivered_at, revoked_at, build_started_at, build_attempts`

func scanExport(row pgx.Row) (*domain.AuditExportRequest, error) {
	export := &domain.AuditExportRequest{}
	err := row.Scan(
		&export.ExportID, &export.TenantID, &export.LegalEntityID, &export.ArchiveID, &export.Purpose,
		&export.Status, &export.Version,
		&export.RequestedByPrincipalID, &export.ApprovedByPrincipalID, &export.SealedByPrincipalID,
		&export.DeliveredToPrincipalID, &export.RevokedByPrincipalID, &export.RevocationReason,
		&export.SealDigest, &export.FailureReason,
		&export.CreatedAt, &export.ApprovedAt, &export.SealedAt, &export.DeliveredAt, &export.RevokedAt,
		&export.BuildStartedAt, &export.BuildAttempts,
	)
	if err != nil {
		return nil, err
	}
	return export, nil
}

// lockExportForTransition loads and locks the export, then checks the
// If-Match precondition and that the current status is one of allowedFrom.
func lockExportForTransition(ctx context.Context, tx pgx.Tx, tenantID, exportID string, expectedVersion int, allowedFrom []domain.ExportStatus, notAllowedErr error) (*domain.AuditExportRequest, error) {
	current, err := scanExport(tx.QueryRow(ctx,
		`SELECT `+exportColumns+` FROM audit_export_requests WHERE export_id = $1 AND tenant_id = $2 FOR UPDATE`,
		exportID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrExportNotFound
	}
	if err != nil {
		return nil, err
	}

	if expectedVersion != 0 && expectedVersion != current.Version {
		return nil, domain.ErrVersionMismatch
	}

	for _, allowed := range allowedFrom {
		if current.Status == allowed {
			return current, nil
		}
	}
	return nil, notAllowedErr
}

// CreateExportRequest records a new REQUESTED export.
func (s *PgStore) CreateExportRequest(ctx context.Context, p domain.CreateExportRequestParams) (*domain.AuditExportRequest, error) {
	var created *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		var err error
		created, err = scanExport(tx.QueryRow(ctx, `
			INSERT INTO audit_export_requests
			  (tenant_id, legal_entity_id, archive_id, purpose, requested_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING `+exportColumns,
			p.TenantID, p.LegalEntityID, p.ArchiveID, p.Purpose, p.RequestedByPrincipalID))
		if err != nil {
			return fmt.Errorf("insert audit export request: %w", err)
		}
		event := p.Event
		event.AggregateID = created.ExportID
		return insertOutboxEvent(ctx, tx, event, created)
	})
	return created, err
}

// GetExportRequest returns the tenant's export, or ErrExportNotFound.
func (s *PgStore) GetExportRequest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, error) {
	var found *domain.AuditExportRequest
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		found, err = scanExport(tx.QueryRow(ctx,
			`SELECT `+exportColumns+` FROM audit_export_requests WHERE export_id = $1 AND tenant_id = $2`,
			exportID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrExportNotFound
		}
		return err
	})
	return found, err
}

// ExportFilter narrows ListExportRequests; empty fields are ignored.
type ExportFilter struct {
	Status        string
	LegalEntityID string
}

// ListExportRequests returns one page of the tenant's exports, newest first.
func (s *PgStore) ListExportRequests(ctx context.Context, tenantID string, filter ExportFilter, page Page) ([]domain.AuditExportRequest, error) {
	exports := []domain.AuditExportRequest{}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+exportColumns+`
			FROM audit_export_requests
			WHERE tenant_id = $1
			  AND ($2 = '' OR status = $2)
			  AND ($3 = '' OR legal_entity_id = $3)
			ORDER BY created_at DESC, export_id
			LIMIT $4 OFFSET $5`,
			tenantID, filter.Status, filter.LegalEntityID, page.Limit, page.Offset)
		if err != nil {
			return fmt.Errorf("list audit exports: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			export, err := scanExport(rows)
			if err != nil {
				return fmt.Errorf("scan audit export: %w", err)
			}
			exports = append(exports, *export)
		}
		return rows.Err()
	})
	return exports, err
}

// ApproveExport moves REQUESTED -> APPROVED. Segregation of duties: the
// approver may never be the requester (403), checked here and by migration
// 000002's CHECK constraint.
func (s *PgStore) ApproveExport(ctx context.Context, p domain.ApproveExportParams) (*domain.AuditExportRequest, error) {
	var approved *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			[]domain.ExportStatus{domain.ExportRequested}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		if current.RequestedByPrincipalID == p.ApprovedByPrincipalID {
			return domain.ErrExportSelfApproval
		}

		approved, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests
			SET status = $1, approved_by_principal_id = $2, approved_at = now(), version = version + 1
			WHERE export_id = $3 AND tenant_id = $4
			RETURNING `+exportColumns,
			domain.ExportApproved, p.ApprovedByPrincipalID, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("approve audit export: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, approved)
	})
	return approved, err
}

// BuildLease is how long a claimed build may stay BUILDING before a retried
// build request may re-claim it. It is far longer than the archive fetch
// timeout (3s), so a live build is never taken over.
const BuildLease = 2 * time.Minute

// StartBuild claims the build and commits, so the (slow) archive fetch
// happens outside any transaction:
//
//   - APPROVED -> BUILDING (first attempt);
//   - BUILDING whose lease expired -> BUILDING again (the previous attempt
//     died between claim and outcome; its result is unknown, so the build
//     is redone from the archive, which is deterministic);
//   - BUILDING within its lease, or any other state -> 409.
func (s *PgStore) StartBuild(ctx context.Context, p domain.StartBuildParams) (*domain.AuditExportRequest, error) {
	var building *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			[]domain.ExportStatus{domain.ExportApproved, domain.ExportBuilding}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		if current.Status == domain.ExportBuilding {
			var expired bool
			if err := tx.QueryRow(ctx, `
				SELECT build_started_at IS NULL OR build_started_at < now() - ($3::int * interval '1 second')
				FROM audit_export_requests WHERE export_id = $1 AND tenant_id = $2`,
				p.ExportID, p.TenantID, int(BuildLease/time.Second)).Scan(&expired); err != nil {
				return fmt.Errorf("check build lease: %w", err)
			}
			if !expired {
				return domain.ErrBuildInProgress
			}
		}
		building, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests
			SET status = $1, version = version + 1, build_started_at = now(), build_attempts = build_attempts + 1
			WHERE export_id = $2 AND tenant_id = $3
			RETURNING `+exportColumns,
			domain.ExportBuilding, p.ExportID, p.TenantID))
		return err
	})
	return building, err
}

// CompleteBuild records the manifest entry fetched from the archive and
// moves BUILDING -> VALIDATING.
func (s *PgStore) CompleteBuild(ctx context.Context, p domain.CompleteBuildParams) (*domain.AuditExportRequest, error) {
	var validating *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, 0,
			[]domain.ExportStatus{domain.ExportBuilding}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO export_manifests (export_id, tenant_id, artifact_name, sha256) VALUES ($1,$2,$3,$4)`,
			p.ExportID, p.TenantID, p.ArtifactName, p.Sha256); err != nil {
			return fmt.Errorf("insert export manifest: %w", err)
		}
		validating, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests SET status = $1, version = version + 1
			WHERE export_id = $2 AND tenant_id = $3
			RETURNING `+exportColumns,
			domain.ExportValidating, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("mark audit export validating: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, validating)
	})
	return validating, err
}

// FailBuild moves BUILDING -> FAILED with the reason.
func (s *PgStore) FailBuild(ctx context.Context, p domain.FailBuildParams) (*domain.AuditExportRequest, error) {
	var failed *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, 0,
			[]domain.ExportStatus{domain.ExportBuilding}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		failed, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests SET status = $1, failure_reason = $2, version = version + 1
			WHERE export_id = $3 AND tenant_id = $4
			RETURNING `+exportColumns,
			domain.ExportFailed, p.Reason, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("mark audit export failed: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, failed)
	})
	return failed, err
}

// RecordRedaction appends a redaction decision. Allowed only before the seal
// (domain.RedactableExportStates); a sealed package is immutable (409). The
// export's version is bumped because its protected content changed.
func (s *PgStore) RecordRedaction(ctx context.Context, p domain.RecordRedactionParams) (*domain.RedactionDecision, error) {
	var decision *domain.RedactionDecision
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			domain.RedactableExportStates, domain.ErrExportNotRedactable)
		if err != nil {
			return err
		}

		decision = &domain.RedactionDecision{}
		err = tx.QueryRow(ctx, `
			INSERT INTO redaction_decisions (export_id, tenant_id, field_or_scope, reason, decided_by_principal_id)
			VALUES ($1,$2,$3,$4,$5)
			RETURNING decision_id::text, export_id::text, field_or_scope, reason, decided_by_principal_id, decided_at`,
			p.ExportID, p.TenantID, p.FieldOrScope, p.Reason, p.DecidedByPrincipalID,
		).Scan(&decision.DecisionID, &decision.ExportID, &decision.FieldOrScope, &decision.Reason,
			&decision.DecidedByPrincipalID, &decision.DecidedAt)
		if err != nil {
			return fmt.Errorf("insert redaction decision: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE audit_export_requests SET version = version + 1 WHERE export_id = $1 AND tenant_id = $2`,
			p.ExportID, p.TenantID); err != nil {
			return fmt.Errorf("bump audit export version: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, decision)
	})
	return decision, err
}

// SealExport moves VALIDATING -> SEALED and stores the package seal digest
// computed over the manifest and redaction log (domain.ComputePackageDigest).
// An empty package (no manifest rows) cannot be sealed (409).
func (s *PgStore) SealExport(ctx context.Context, p domain.SealExportParams) (*domain.AuditExportRequest, error) {
	var sealed *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		current, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			[]domain.ExportStatus{domain.ExportValidating}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}

		entries, err := listManifestEntries(ctx, tx, p.TenantID, p.ExportID)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return domain.ErrManifestRequired
		}
		redactions, err := listRedactions(ctx, tx, p.TenantID, p.ExportID)
		if err != nil {
			return err
		}
		sealDigest := domain.ComputePackageDigest(current, entries, redactions)

		sealed, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests
			SET status = $1, sealed_at = now(), sealed_by_principal_id = $2, seal_digest = $3, version = version + 1
			WHERE export_id = $4 AND tenant_id = $5
			RETURNING `+exportColumns,
			domain.ExportSealed, p.SealedByPrincipalID, sealDigest, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("seal audit export: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, sealed)
	})
	return sealed, err
}

// DeliverExport moves SEALED -> DELIVERED and appends the delivery receipt.
func (s *PgStore) DeliverExport(ctx context.Context, p domain.DeliverExportParams) (*domain.AuditExportRequest, error) {
	var delivered *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			[]domain.ExportStatus{domain.ExportSealed}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		delivered, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests
			SET status = $1, delivered_to_principal_id = $2, delivered_at = now(), version = version + 1
			WHERE export_id = $3 AND tenant_id = $4
			RETURNING `+exportColumns,
			domain.ExportDelivered, p.DeliveredToPrincipalID, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("deliver audit export: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO delivery_receipts (export_id, tenant_id, delivered_to_principal_id, delivery_channel)
			VALUES ($1,$2,$3,$4)`,
			p.ExportID, p.TenantID, p.DeliveredToPrincipalID, p.DeliveryChannel); err != nil {
			return fmt.Errorf("insert delivery receipt: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, delivered)
	})
	return delivered, err
}

// RevokeExport moves SEALED or DELIVERED -> REVOKED, recording who and why.
func (s *PgStore) RevokeExport(ctx context.Context, p domain.RevokeExportParams) (*domain.AuditExportRequest, error) {
	var revoked *domain.AuditExportRequest
	err := s.inTenantTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := lockExportForTransition(ctx, tx, p.TenantID, p.ExportID, p.ExpectedVersion,
			[]domain.ExportStatus{domain.ExportSealed, domain.ExportDelivered}, domain.ErrExportInvalidState)
		if err != nil {
			return err
		}
		revoked, err = scanExport(tx.QueryRow(ctx, `
			UPDATE audit_export_requests
			SET status = $1, revoked_at = now(), revoked_by_principal_id = $2, revocation_reason = $3, version = version + 1
			WHERE export_id = $4 AND tenant_id = $5
			RETURNING `+exportColumns,
			domain.ExportRevoked, p.RevokedByPrincipalID, p.Reason, p.ExportID, p.TenantID))
		if err != nil {
			return fmt.Errorf("revoke audit export: %w", err)
		}
		return insertOutboxEvent(ctx, tx, p.Event, revoked)
	})
	return revoked, err
}

// GetPackageManifest returns the export's manifest entries and redaction log
// together with the export itself (so the caller can verify the seal).
// 404 when the export does not exist for this tenant.
func (s *PgStore) GetPackageManifest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, []domain.ExportManifest, []domain.RedactionDecision, error) {
	var (
		export     *domain.AuditExportRequest
		entries    []domain.ExportManifest
		redactions []domain.RedactionDecision
	)
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		export, err = scanExport(tx.QueryRow(ctx,
			`SELECT `+exportColumns+` FROM audit_export_requests WHERE export_id = $1 AND tenant_id = $2`,
			exportID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrExportNotFound
		}
		if err != nil {
			return err
		}
		if entries, err = listManifestEntries(ctx, tx, tenantID, exportID); err != nil {
			return err
		}
		redactions, err = listRedactions(ctx, tx, tenantID, exportID)
		return err
	})
	return export, entries, redactions, err
}

// ListRedactions returns the export's redaction log (404 if no such export).
func (s *PgStore) ListRedactions(ctx context.Context, tenantID, exportID string) ([]domain.RedactionDecision, error) {
	var redactions []domain.RedactionDecision
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireExportExists(ctx, tx, tenantID, exportID); err != nil {
			return err
		}
		var err error
		redactions, err = listRedactions(ctx, tx, tenantID, exportID)
		return err
	})
	return redactions, err
}

// ListDeliveries returns the export's delivery history (404 if no such export).
func (s *PgStore) ListDeliveries(ctx context.Context, tenantID, exportID string) ([]domain.DeliveryReceipt, error) {
	receipts := []domain.DeliveryReceipt{}
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := requireExportExists(ctx, tx, tenantID, exportID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT receipt_id::text, export_id::text, delivered_to_principal_id, delivery_channel, delivered_at
			FROM delivery_receipts WHERE export_id = $1 AND tenant_id = $2
			ORDER BY delivered_at, receipt_id`, exportID, tenantID)
		if err != nil {
			return fmt.Errorf("list delivery receipts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var receipt domain.DeliveryReceipt
			if err := rows.Scan(&receipt.ReceiptID, &receipt.ExportID, &receipt.DeliveredToPrincipalID,
				&receipt.DeliveryChannel, &receipt.DeliveredAt); err != nil {
				return fmt.Errorf("scan delivery receipt: %w", err)
			}
			receipts = append(receipts, receipt)
		}
		return rows.Err()
	})
	return receipts, err
}

// ── shared child-table readers (run inside an existing tenant transaction) ──

func requireExportExists(ctx context.Context, tx pgx.Tx, tenantID, exportID string) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_export_requests WHERE export_id = $1 AND tenant_id = $2)`,
		exportID, tenantID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return domain.ErrExportNotFound
	}
	return nil
}

func listManifestEntries(ctx context.Context, tx pgx.Tx, tenantID, exportID string) ([]domain.ExportManifest, error) {
	entries := []domain.ExportManifest{}
	rows, err := tx.Query(ctx, `
		SELECT manifest_id::text, export_id::text, artifact_name, sha256, recorded_at
		FROM export_manifests WHERE export_id = $1 AND tenant_id = $2
		ORDER BY recorded_at, manifest_id`, exportID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list export manifests: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry domain.ExportManifest
		if err := rows.Scan(&entry.ManifestID, &entry.ExportID, &entry.ArtifactName, &entry.Sha256, &entry.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan export manifest: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func listRedactions(ctx context.Context, tx pgx.Tx, tenantID, exportID string) ([]domain.RedactionDecision, error) {
	redactions := []domain.RedactionDecision{}
	rows, err := tx.Query(ctx, `
		SELECT decision_id::text, export_id::text, field_or_scope, reason, decided_by_principal_id, decided_at
		FROM redaction_decisions WHERE export_id = $1 AND tenant_id = $2
		ORDER BY decided_at, decision_id`, exportID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list redaction decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var decision domain.RedactionDecision
		if err := rows.Scan(&decision.DecisionID, &decision.ExportID, &decision.FieldOrScope, &decision.Reason,
			&decision.DecidedByPrincipalID, &decision.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan redaction decision: %w", err)
		}
		redactions = append(redactions, decision)
	}
	return redactions, rows.Err()
}
