package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SuspendRoleAssignment holds a projected assignment back from session
// resolution (iam.assignment.suspended). The assignment is not ended: it can be
// resumed, so nothing is tombstoned.
//
// at is when the suspension happened (the event's emitted_at). Only the latest
// suspension is kept, and the assignment is suspended while that is newer than
// the latest resumption, so redelivery and reordering cannot leave it in the
// wrong state. An assignment the projection never held is not an error; a stub
// row is inserted with the suspension timestamp.
func (s *PgStore) SuspendRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error {
	if tenantID == "" || assignmentID == "" {
		return errors.New("SuspendRoleAssignment: tenant_id and assignment_id are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// First try to update an existing assignment
		cmdTag, err := tx.Exec(ctx, `
			UPDATE principal_role_assignments
			   SET suspended_at = GREATEST(COALESCE(suspended_at, '-infinity'::timestamptz), $1)
			 WHERE assignment_id = $2 AND tenant_id = $3`, at, assignmentID, tenantID)
		if err != nil {
			return err
		}
		// If no row was updated, insert a stub row with the suspension
		if cmdTag.RowsAffected() == 0 {
			_, err = tx.Exec(ctx, `
				INSERT INTO principal_role_assignments
					(assignment_id, principal_id, tenant_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by, suspended_at)
				VALUES ($1, '', $2, '', NULL, '-infinity'::timestamptz, 'infinity'::timestamptz, '', $3)
				ON CONFLICT (assignment_id) DO NOTHING`,
				assignmentID, tenantID, at)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// ResumeRoleAssignment lifts a suspension (iam.assignment.resumed). It does not
// reopen an assignment that has ended: effective_to is untouched.
func (s *PgStore) ResumeRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error {
	if tenantID == "" || assignmentID == "" {
		return errors.New("ResumeRoleAssignment: tenant_id and assignment_id are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE principal_role_assignments
			   SET resumed_at = GREATEST(COALESCE(resumed_at, '-infinity'::timestamptz), $1)
			 WHERE assignment_id = $2 AND tenant_id = $3`, at, assignmentID, tenantID)
		return err
	})
}

// notSuspended is the predicate every query that frames or counts a role holder
// adds: the latest suspension, if any, is not newer than the latest resumption.
const notSuspended = `
		  AND NOT (suspended_at IS NOT NULL AND (resumed_at IS NULL OR suspended_at > resumed_at))`
