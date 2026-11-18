package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/identity-context-svc/internal/domain"
)

// ErrUnknownPrincipal: see domain.ErrUnknownPrincipal.
var ErrUnknownPrincipal = domain.ErrUnknownPrincipal

// UpsertRoleAssignment projects an assignment access-control-svc provisioned
// (iam.assignment.granted) into principal_role_assignments.
//
// ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
//
// The resolver frames a session's roles from this table, and handleRoleUpdated
// finds a role's holders in it. Nothing wrote it outside tests and the local
// verification seed: on the dev database it held zero rows, so every
// role.updated resolved zero holders and ended nobody's session, and an
// assignment provisioned through access-control-svc never reached a session at
// all. The governed assignment events are the feed it was missing.
//
// effective_to is NOT NULL here; an open-ended assignment is 'infinity'.
//
// A grant never opens an assignment past a recorded revocation
// (revoked_role_assignments, 000010): a grant delivered after its revoke — a
// reorder, a retry or a replay — is projected already ended. effective_to only
// ever moves earlier, here as in EndRoleAssignment.
//
// IC-12 (S1-10 K-2): If a suspension was recorded before the grant (suspension
// before grant), the suspension is applied by setting suspended_at. The grant
// does not frame the role until a resume arrives. That pre-grant state lives in
// pending_role_assignment_suspensions (000020) because a stub projection row
// cannot satisfy principal_id's foreign key; it is merged here and cleared.
func (s *PgStore) UpsertRoleAssignment(ctx context.Context, tenantID string, a domain.PrincipalRoleAssignment) error {
	if tenantID == "" || a.AssignmentID == "" || a.PrincipalID == "" || a.RoleID == "" {
		return errors.New("UpsertRoleAssignment: tenant_id, assignment_id, principal_id and role_id are required")
	}
	effectiveTo := any("infinity")
	if !a.EffectiveTo.IsZero() {
		effectiveTo = a.EffectiveTo
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// A suspension or resume that arrived before this grant is held in
		// pending_role_assignment_suspensions. Merge both timestamps so the
		// answer still does not depend on event order.
		var pendingSuspendedAt, pendingResumedAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT suspended_at, resumed_at
			  FROM pending_role_assignment_suspensions
			 WHERE tenant_id = $1 AND assignment_id = $2
		`, tenantID, a.AssignmentID).Scan(&pendingSuspendedAt, &pendingResumedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var suspendedAt, resumedAt any
		if pendingSuspendedAt != nil {
			suspendedAt = *pendingSuspendedAt
		}
		if pendingResumedAt != nil {
			resumedAt = *pendingResumedAt
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO principal_role_assignments
				(assignment_id, principal_id, tenant_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by, suspended_at, resumed_at)
			VALUES ($1,$2,$3,$4,$5,$6,
				LEAST($7::timestamptz, COALESCE(
					(SELECT revoked_at FROM revoked_role_assignments WHERE tenant_id = $9::text AND assignment_id = $10::text),
					'infinity'::timestamptz)),
				$8, $11, $12)
			ON CONFLICT (assignment_id) DO UPDATE SET
				role_id = EXCLUDED.role_id,
				legal_entity_id = EXCLUDED.legal_entity_id,
				effective_from = EXCLUDED.effective_from,
				effective_to = LEAST(principal_role_assignments.effective_to, EXCLUDED.effective_to),
				principal_id = EXCLUDED.principal_id,
				assigned_by = EXCLUDED.assigned_by,
				suspended_at = GREATEST(principal_role_assignments.suspended_at, EXCLUDED.suspended_at),
				resumed_at = GREATEST(principal_role_assignments.resumed_at, EXCLUDED.resumed_at)`,
			a.AssignmentID, a.PrincipalID, tenantID, a.RoleID, a.LegalEntityID, a.EffectiveFrom, effectiveTo, a.AssignedBy,
			tenantID, a.AssignmentID, suspendedAt, resumedAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM pending_role_assignment_suspensions
			 WHERE tenant_id = $1 AND assignment_id = $2`, tenantID, a.AssignmentID)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrUnknownPrincipal
	}
	if err != nil {
		return fmt.Errorf("project role assignment: %w", err)
	}
	return nil
}

// EndRoleAssignment closes a projected assignment (iam.assignment.revoked).
// Never extends one: effective_to only moves earlier. The revocation is also
// recorded as a tombstone, in the same transaction, so a grant for this
// assignment that arrives LATER cannot re-open it (S1-4). An assignment the
// projection never held is not an error; its tombstone is what matters.
func (s *PgStore) EndRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error {
	if tenantID == "" || assignmentID == "" {
		return errors.New("EndRoleAssignment: tenant_id and assignment_id are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO revoked_role_assignments (tenant_id, assignment_id, revoked_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id, assignment_id) DO UPDATE
			   SET revoked_at = LEAST(revoked_role_assignments.revoked_at, EXCLUDED.revoked_at)`,
			tenantID, assignmentID, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE principal_role_assignments SET effective_to = LEAST(effective_to, $1)
			 WHERE assignment_id = $2 AND tenant_id = $3`, at, assignmentID, tenantID)
		return err
	})
}
