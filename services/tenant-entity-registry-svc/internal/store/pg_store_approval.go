package store

// Approval requests — verified maker-checker for ORG-02 §4.2 / ORG-03 §4.3.
//
// Two properties this file exists to hold:
//
//  1. An approval that releases a write is recorded in the SAME transaction as
//     that write (decideApprovalTx). An approval marked APPROVED whose command
//     never ran, or a command that ran under an approval still PENDING, are
//     both impossible.
//  2. The decision UPDATE is guarded on status = 'PENDING' (and, for
//     APPROVED, on the TTL). Two approvers racing, or an approval arriving
//     after expiry, affect zero rows and are refused.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const approvalColumns = `
	approval_request_id, tenant_id, subject_type, subject_id, command_name,
	payload, expected_version, payload_fingerprint, reason,
	requested_by_principal_id, requested_at, expires_at,
	status, decided_by_principal_id, decided_at, decision_note, correlation_id`

func scanApproval(row pgx.Row) (*domain.ApprovalRequest, error) {
	var a domain.ApprovalRequest
	var payload []byte
	if err := row.Scan(
		&a.ApprovalRequestID, &a.TenantID, &a.SubjectType, &a.SubjectID, &a.CommandName,
		&payload, &a.ExpectedVersion, &a.PayloadFingerprint, &a.Reason,
		&a.RequestedByPrincipalID, &a.RequestedAt, &a.ExpiresAt,
		&a.Status, &a.DecidedByPrincipalID, &a.DecidedAt, &a.DecisionNote, &a.CorrelationID,
	); err != nil {
		return nil, err
	}
	a.Payload = payload
	return &a, nil
}

// CreateApprovalRequest files a PENDING request.
func (s *PgStore) CreateApprovalRequest(ctx context.Context, a *domain.ApprovalRequest) error {
	tid := tenantFromCtxOrFallback(ctx, a.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		// An abandoned proposal must not block its subject forever: the
		// partial unique index admits one PENDING row per subject, so an
		// expired one is retired first, in the same transaction.
		if _, err := tx.Exec(ctx, `
			UPDATE approval_requests
			   SET status = 'EXPIRED', decided_at = NOW(),
			       decision_note = 'expired before a decision was made'
			 WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
			   AND status = 'PENDING' AND expires_at <= NOW()`,
			a.TenantID, string(a.SubjectType), a.SubjectID); err != nil {
			return fmt.Errorf("expire stale approvals: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO approval_requests (
				approval_request_id, tenant_id, subject_type, subject_id, command_name,
				payload, expected_version, payload_fingerprint, reason,
				requested_by_principal_id, requested_at, expires_at, status, correlation_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'PENDING',$13)`,
			a.ApprovalRequestID, a.TenantID, string(a.SubjectType), a.SubjectID, a.CommandName,
			[]byte(a.Payload), a.ExpectedVersion, a.PayloadFingerprint, a.Reason,
			a.RequestedByPrincipalID, a.RequestedAt, a.ExpiresAt, a.CorrelationID)
		if isUniqueViolation(err) {
			return registry.ErrApprovalPending
		}
		return err
	})
}

// GetApprovalRequest returns one request, or (nil, nil).
func (s *PgStore) GetApprovalRequest(ctx context.Context, id string) (*domain.ApprovalRequest, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.ApprovalRequest
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		a, err := scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalColumns+` FROM approval_requests
			  WHERE approval_request_id = $1 AND tenant_id = $2`, id, tid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = a
		return err
	})
	return out, err
}

// ListApprovalRequests lists the caller's tenant's requests, newest first.
func (s *PgStore) ListApprovalRequests(ctx context.Context, pendingOnly bool) ([]*domain.ApprovalRequest, error) {
	tid := domain.TenantFromContext(ctx)
	out := []*domain.ApprovalRequest{}
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		q := `SELECT ` + approvalColumns + ` FROM approval_requests WHERE tenant_id = $1`
		if pendingOnly {
			// An expired row is still PENDING until something retires it;
			// it is not awaiting anyone and is not shown as if it were.
			q += ` AND status = 'PENDING' AND expires_at > NOW()`
		}
		q += ` ORDER BY requested_at DESC, approval_request_id DESC`
		rows, err := tx.Query(ctx, q, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanApproval(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// LatestApprovalForSubject returns the newest request for a subject.
func (s *PgStore) LatestApprovalForSubject(ctx context.Context, subjectType domain.ApprovalSubjectType, subjectID string) (*domain.ApprovalRequest, error) {
	tid := tenantFromCtxOrFallback(ctx, subjectID)
	var out *domain.ApprovalRequest
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		a, err := scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalColumns+` FROM approval_requests
			  WHERE tenant_id = $1 AND subject_type = $2 AND subject_id = $3
			  ORDER BY requested_at DESC, approval_request_id DESC
			  LIMIT 1`, tid, string(subjectType), subjectID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = a
		return err
	})
	return out, err
}

// DecideApprovalRequest moves a PENDING request to status on its own.
func (s *PgStore) DecideApprovalRequest(ctx context.Context, d domain.ApprovalDecision, status domain.ApprovalStatus) error {
	tid := tenantFromCtxOrFallback(ctx, d.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		return decideApprovalTx(ctx, tx, tid, d, status)
	})
}

// decideApprovalTx records a decision inside an existing transaction.
func decideApprovalTx(ctx context.Context, tx pgx.Tx, tenantID string, d domain.ApprovalDecision, status domain.ApprovalStatus) error {
	ct, err := tx.Exec(ctx, `
		UPDATE approval_requests
		   SET status = $1, decided_by_principal_id = $2,
		       decided_at = $3, decision_note = $4
		 WHERE approval_request_id = $5 AND tenant_id = $6
		   AND status = 'PENDING'
		   AND ($1 <> 'APPROVED' OR expires_at > $3)`,
		string(status), nullableString(d.DecidedByPrincipalID), time.Now().UTC(),
		nullableString(d.Note), d.ApprovalRequestID, tenantID)
	if err != nil {
		return fmt.Errorf("decide approval: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return registry.ErrApprovalNotPending
	}
	return nil
}

// GetRegistryConflict returns one conflict, or (nil, nil).
func (s *PgStore) GetRegistryConflict(ctx context.Context, conflictID string) (*domain.EntityRegistryConflict, error) {
	tid := domain.TenantFromContext(ctx)
	var out *domain.EntityRegistryConflict
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var c domain.EntityRegistryConflict
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT conflict_id, tenant_id, registration_number, jurisdiction_id,
			       existing_legal_entity_id, attempted_payload, status,
			       resolution_note, resolved_by_principal_id, resolved_at,
			       approved_by_principal_id, approval_request_id,
			       detected_at, detected_by_principal_id, correlation_id
			  FROM entity_registry_conflicts
			 WHERE conflict_id = $1 AND tenant_id = $2`, conflictID, tid).Scan(
			&c.ConflictID, &c.TenantID, &c.RegistrationNumber, &c.JurisdictionID,
			&c.ExistingLegalEntityID, &payload, &c.Status,
			&c.ResolutionNote, &c.ResolvedByPrincipalID, &c.ResolvedAt,
			&c.ApprovedByPrincipalID, &c.ApprovalRequestID,
			&c.DetectedAt, &c.DetectedByPrincipalID, &c.CorrelationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &c.AttemptedPayload)
		}
		out = &c
		return nil
	})
	return out, err
}

// ResolveRegistryConflictApproved records an approved resolution and its
// approval decision atomically. erc_no_self_approval backs the service's
// check that the approver is neither the resolver nor the detector.
func (s *PgStore) ResolveRegistryConflictApproved(ctx context.Context, conflictID string, status domain.RegistryConflictStatus, note, resolvedBy string, d domain.ApprovalDecision) error {
	tid := tenantFromCtxOrFallback(ctx, d.TenantID)
	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := decideApprovalTx(ctx, tx, tid, d, domain.ApprovalApproved); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE entity_registry_conflicts
			   SET status = $1, resolution_note = $2,
			       resolved_by_principal_id = $3, resolved_at = $4,
			       approved_by_principal_id = $5, approval_request_id = $6
			 WHERE conflict_id = $7 AND tenant_id = $8 AND status = 'OPEN'`,
			string(status), nullableString(note), resolvedBy, time.Now().UTC(),
			d.DecidedByPrincipalID, d.ApprovalRequestID, conflictID, tid)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return registry.ErrConflict
		}
		return nil
	})
}
