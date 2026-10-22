package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

// Upload reservations (ZS-SVC-S-001 §3.4). Everything here runs in the caller's
// tenant: the table is under forced row-level security and every query also
// carries the tenant explicitly.

const reservationColumns = `
	reservation_id, tenant_id, document_id, reserved_by_principal_id, purpose, expected_size_bytes,
	declared_content_type, commit_status, failure_reason, committed_version_id, created_at, completed_at
`

func scanReservation(row pgx.Row, r *domain.UploadReservation) error {
	return row.Scan(&r.ReservationID, &r.TenantID, &r.DocumentID, &r.ReservedBy, &r.Purpose, &r.ExpectedSizeBytes,
		&r.DeclaredContentType, &r.CommitStatus, &r.FailureReason, &r.CommittedVersionID, &r.CreatedAt, &r.CompletedAt)
}

// CreateUploadReservation reserves an upload for an existing document. It is
// idempotent on (tenant, principal, Idempotency-Key): the same key with the same
// request returns the original reservation (replayed = true) and creates nothing;
// the same key with a different request is domain.ErrIdempotencyMismatch. Two
// concurrent identical requests produce one reservation (the unique key decides).
func (s *PgStore) CreateUploadReservation(ctx context.Context, p domain.CreateUploadReservationParams) (*domain.UploadReservation, bool, error) {
	var out domain.UploadReservation
	replayed := false
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, p.DocumentID, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO upload_reservations (tenant_id, document_id, reserved_by_principal_id, purpose,
				expected_size_bytes, declared_content_type, idempotency_key, request_hash)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant_id, reserved_by_principal_id, idempotency_key) DO NOTHING
			RETURNING `+reservationColumns,
			tenantID, p.DocumentID, p.ReservedByPrincipal, p.Purpose, p.ExpectedSizeBytes, p.DeclaredContentType,
			p.IdempotencyKey, p.RequestHash)
		err := scanReservation(row, &out)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		// The key already exists (this or a concurrent request created it).
		var hash string
		existing := tx.QueryRow(ctx, `SELECT request_hash, `+reservationColumns+` FROM upload_reservations
			WHERE tenant_id::text = $1 AND reserved_by_principal_id = $2 AND idempotency_key = $3`,
			tenantID, p.ReservedByPrincipal, p.IdempotencyKey)
		if err := existing.Scan(&hash, &out.ReservationID, &out.TenantID, &out.DocumentID, &out.ReservedBy, &out.Purpose,
			&out.ExpectedSizeBytes, &out.DeclaredContentType, &out.CommitStatus, &out.FailureReason, &out.CommittedVersionID,
			&out.CreatedAt, &out.CompletedAt); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if hash != p.RequestHash {
			return domain.ErrIdempotencyMismatch
		}
		replayed = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &out, replayed, nil
}

// GetUploadReservation reads one reservation in the caller's tenant. An id that is
// not a UUID, or belongs to another tenant, is "not found".
func (s *PgStore) GetUploadReservation(ctx context.Context, reservationID string) (*domain.UploadReservation, error) {
	if _, err := uuid.Parse(reservationID); err != nil {
		return nil, domain.ErrUploadReservationNotFound
	}
	var out domain.UploadReservation
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM upload_reservations
			WHERE reservation_id = $1 AND tenant_id::text = $2`, reservationID, tenantID)
		if err := scanReservation(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrUploadReservationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CompleteUploadReservation commits the version for a RESERVED reservation, in ONE
// transaction: the reservation row is locked, the version is added by the same
// code AddVersion uses (version row, current_version bump, outbox event), and the
// reservation moves to COMMITTED pointing at it. Because the lock serialises
// completions, retrying a completed reservation — sequentially or concurrently —
// can never commit a second version: a reservation that is already COMMITTED
// returns its version with alreadyCommitted = true and writes nothing.
//
// A reservation that is FAILED or QUARANTINED is terminal: ErrUploadReservationClosed.
// v is filled in on a fresh commit; on alreadyCommitted the stored version is returned.
func (s *PgStore) CompleteUploadReservation(ctx context.Context, reservationID string, v *domain.DocumentVersion, correlationID string) (res *domain.UploadReservation, version *domain.DocumentVersion, alreadyCommitted bool, err error) {
	if _, perr := uuid.Parse(reservationID); perr != nil {
		return nil, nil, false, domain.ErrUploadReservationNotFound
	}
	var out domain.UploadReservation
	var committed domain.DocumentVersion
	err = s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM upload_reservations
			WHERE reservation_id = $1 AND tenant_id::text = $2 FOR UPDATE`, reservationID, tenantID)
		if err := scanReservation(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrUploadReservationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		switch out.CommitStatus {
		case domain.ReservationCommitted:
			vrow := tx.QueryRow(ctx, `SELECT `+versionColumns+` FROM document_versions WHERE document_version_id = $1`, *out.CommittedVersionID)
			if err := scanVersion(vrow, &committed); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			alreadyCommitted = true
			return nil
		case domain.ReservationFailed, domain.ReservationQuarantined:
			return domain.ErrUploadReservationClosed
		}

		if _, err := s.addVersionTx(ctx, tx, tenantID, out.DocumentID, v, correlationID); err != nil {
			return err
		}
		upd := tx.QueryRow(ctx, `
			UPDATE upload_reservations SET commit_status = 'COMMITTED', committed_version_id = $2, completed_at = now()
			WHERE reservation_id = $1 RETURNING `+reservationColumns, reservationID, v.DocumentVersionID)
		if err := scanReservation(upd, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	if alreadyCommitted {
		return &out, &committed, true, nil
	}
	return &out, v, false, nil
}

// CloseUploadReservation moves a RESERVED reservation to a terminal FAILED or
// QUARANTINED state with the reason. It is idempotent: a reservation that has
// already left RESERVED is returned unchanged. Nothing is deleted, and no version
// is created for a failed or quarantined upload.
func (s *PgStore) CloseUploadReservation(ctx context.Context, reservationID, status, reason string) (*domain.UploadReservation, error) {
	if status != domain.ReservationFailed && status != domain.ReservationQuarantined {
		return nil, fmt.Errorf("close reservation: %q is not a terminal failure state", status)
	}
	if _, err := uuid.Parse(reservationID); err != nil {
		return nil, domain.ErrUploadReservationNotFound
	}
	var out domain.UploadReservation
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM upload_reservations
			WHERE reservation_id = $1 AND tenant_id::text = $2 FOR UPDATE`, reservationID, tenantID)
		if err := scanReservation(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrUploadReservationNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		if out.CommitStatus != domain.ReservationReserved {
			return nil
		}
		upd := tx.QueryRow(ctx, `
			UPDATE upload_reservations SET commit_status = $2, failure_reason = $3, completed_at = now()
			WHERE reservation_id = $1 RETURNING `+reservationColumns, reservationID, status, reason)
		return scanReservation(upd, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
