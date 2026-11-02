package store

// Assignment suspension and as-of reads (migration 000037; ORG-06 s4.6
// SuspendAssignment, GetAssignmentsAsOf).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// SuspendRoleAssignment withholds an APPROVED, unended assignment without
// ending it, and records the suspension.
//
// The status change and the evidence row are one transaction, and the status
// change is one guarded UPDATE: two administrators racing cannot both suspend,
// and a REJECTED or PENDING assignment cannot be "suspended" into a state the
// next resume would turn into a grant (the 000037 trigger refuses that too).
// Every decision query requires approval_status = 'APPROVED', so the
// assignment stops granting the instant this commits, and the
// assignment_version watermark (000028) invalidates cached decisions on every
// replica.
func (s *PgStore) SuspendRoleAssignment(ctx context.Context, assignmentID, tenantID, actor, reason string) (*domain.PrincipalRoleAssignment, error) {
	const query = `
		UPDATE principal_role_assignments
		   SET approval_status = 'SUSPENDED'
		 WHERE principal_role_assignment_id = $1
		   AND approval_status = 'APPROVED'
		   AND (effective_to IS NULL OR effective_to > NOW())
		   AND role_id IN (SELECT role_id FROM roles WHERE tenant_id = $2)
		RETURNING ` + assignmentColumns + `;`

	var a *domain.PrincipalRoleAssignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAssignment(tx.QueryRow(ctx, query, assignmentID, tenantID))
		if scanErr != nil {
			return scanErr
		}
		_, execErr := tx.Exec(ctx, `
			INSERT INTO assignment_suspensions (principal_role_assignment_id, tenant_id, suspended_by, suspension_reason)
			VALUES ($1, $2, $3, $4)`, assignmentID, tenantID, actor, reason)
		return execErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.explainSuspendRefusal(ctx, assignmentID, tenantID)
	}
	if err != nil {
		s.log.Error("pg SuspendRoleAssignment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) explainSuspendRefusal(ctx context.Context, assignmentID, tenantID string) error {
	if _, err := s.FindRoleAssignmentByID(ctx, assignmentID, tenantID); err != nil {
		return err // ErrRoleAssignmentNotFound, or the store being unavailable
	}
	return domain.ErrAssignmentNotSuspendable
}

// ResumeRoleAssignment lifts a suspension and closes its record.
//
// Resuming restores access, so the principal who holds the assignment cannot be
// the one resuming it, and a suspended assignment whose window has since closed
// is refused rather than flipped back to APPROVED with nothing to grant.
func (s *PgStore) ResumeRoleAssignment(ctx context.Context, assignmentID, tenantID, actor, reason string) (*domain.PrincipalRoleAssignment, error) {
	const query = `
		UPDATE principal_role_assignments
		   SET approval_status = 'APPROVED'
		 WHERE principal_role_assignment_id = $1
		   AND approval_status = 'SUSPENDED'
		   AND principal_id <> $3
		   AND (effective_to IS NULL OR effective_to > NOW())
		   AND role_id IN (SELECT role_id FROM roles WHERE tenant_id = $2)
		RETURNING ` + assignmentColumns + `;`

	var a *domain.PrincipalRoleAssignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAssignment(tx.QueryRow(ctx, query, assignmentID, tenantID, actor))
		if scanErr != nil {
			return scanErr
		}
		tag, execErr := tx.Exec(ctx, `
			UPDATE assignment_suspensions
			   SET resumed_by = $2, resumed_at = clock_timestamp(), resume_reason = NULLIF($3, '')
			 WHERE principal_role_assignment_id = $1 AND resumed_at IS NULL`, assignmentID, actor, reason)
		if execErr != nil {
			return execErr
		}
		if tag.RowsAffected() != 1 {
			// A SUSPENDED assignment with no open suspension is a broken
			// invariant, not something to paper over by resuming it.
			return fmt.Errorf("assignment %s is SUSPENDED but has no open suspension record", assignmentID)
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.explainResumeRefusal(ctx, assignmentID, tenantID, actor)
	}
	if err != nil {
		s.log.Error("pg ResumeRoleAssignment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) explainResumeRefusal(ctx context.Context, assignmentID, tenantID, actor string) error {
	cur, err := s.FindRoleAssignmentByID(ctx, assignmentID, tenantID)
	if err != nil {
		return err
	}
	switch {
	case cur.ApprovalStatus != domain.ApprovalSuspended:
		return domain.ErrAssignmentNotSuspended
	case cur.PrincipalID == actor:
		return domain.ErrSelfResume
	default:
		return domain.ErrAssignmentEnded
	}
}

// ListAssignmentSuspensions returns every suspension of an assignment, oldest
// first, including closed ones.
func (s *PgStore) ListAssignmentSuspensions(ctx context.Context, assignmentID, tenantID string) ([]domain.AssignmentSuspension, error) {
	if _, err := s.FindRoleAssignmentByID(ctx, assignmentID, tenantID); err != nil {
		return nil, err
	}
	out := []domain.AssignmentSuspension{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT suspension_id, principal_role_assignment_id, suspended_by, suspended_at, suspension_reason,
			       resumed_by, resumed_at, resume_reason
			  FROM assignment_suspensions
			 WHERE principal_role_assignment_id = $1 AND tenant_id = $2
			 ORDER BY suspended_at`, assignmentID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x domain.AssignmentSuspension
			if err := rows.Scan(&x.SuspensionID, &x.PrincipalRoleAssignmentID, &x.SuspendedBy, &x.SuspendedAt,
				&x.SuspensionReason, &x.ResumedBy, &x.ResumedAt, &x.ResumeReason); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListAssignmentSuspensions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// QueryAssignmentsAsOf reconstructs the assignments that were AUTHORIZING at
// one instant: approved by then, inside their effective window, and not
// suspended.
//
// Three things make that reconstructable from rows updated in place:
//   - effective_to is only ever set to a moment at or after the revocation or
//     scheduling that set it, so an instant before that still sees the
//     assignment in force;
//   - an assignment authorizes only from the moment it was approved
//     (approval_decided_at, or created_at for one granted directly), so a
//     backdated grant is not shown as authorizing before it existed;
//   - suspensions are recorded as periods, and an instant inside one excludes
//     the assignment.
//
// PENDING, REJECTED and EXPIRED assignments never authorized, so never appear.
func (s *PgStore) QueryAssignmentsAsOf(ctx context.Context, q domain.AssignmentsAsOfQuery) ([]domain.PrincipalRoleAssignment, error) {
	const query = `
		SELECT ` + assignmentColumns + `
		  FROM principal_role_assignments a
		 WHERE a.role_id IN (SELECT role_id FROM roles WHERE tenant_id = $1)
		   AND ($2 = '' OR a.principal_id = $2)
		   AND a.approval_status IN ('APPROVED', 'SUSPENDED')
		   AND a.effective_from <= $3
		   AND (a.effective_to IS NULL OR a.effective_to > $3)
		   AND COALESCE(a.approval_decided_at, a.created_at) <= $3
		   AND NOT EXISTS (
		         SELECT 1 FROM assignment_suspensions s
		          WHERE s.principal_role_assignment_id = a.principal_role_assignment_id
		            AND s.suspended_at <= $3
		            AND (s.resumed_at IS NULL OR s.resumed_at > $3))
		 ORDER BY a.principal_id, a.created_at, a.principal_role_assignment_id
		 LIMIT 1000;`

	out := []domain.PrincipalRoleAssignment{}
	err := s.withRLS(ctx, q.TenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query, q.TenantID, q.PrincipalID, q.AsOf)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.PrincipalRoleAssignment
			if scanErr := rows.Scan(assignmentDest(&a)...); scanErr != nil {
				return scanErr
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg QueryAssignmentsAsOf failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
