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
func (s *PgStore) UpsertRoleAssignment(ctx context.Context, tenantID string, a domain.PrincipalRoleAssignment) error {
	if tenantID == "" || a.AssignmentID == "" || a.PrincipalID == "" || a.RoleID == "" {
		return errors.New("UpsertRoleAssignment: tenant_id, assignment_id, principal_id and role_id are required")
	}
	effectiveTo := any("infinity")
	if !a.EffectiveTo.IsZero() {
		effectiveTo = a.EffectiveTo
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO principal_role_assignments
				(assignment_id, principal_id, tenant_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7::timestamptz,$8)
			ON CONFLICT (assignment_id) DO UPDATE SET
				role_id = EXCLUDED.role_id,
				legal_entity_id = EXCLUDED.legal_entity_id,
				effective_from = EXCLUDED.effective_from`,
			a.AssignmentID, a.PrincipalID, tenantID, a.RoleID, a.LegalEntityID, a.EffectiveFrom, effectiveTo, a.AssignedBy)
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
// Never extends one: effective_to only moves earlier. An assignment the
// projection never held is not an error.
func (s *PgStore) EndRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error {
	if tenantID == "" || assignmentID == "" {
		return errors.New("EndRoleAssignment: tenant_id and assignment_id are required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE principal_role_assignments SET effective_to = LEAST(effective_to, $1)
			 WHERE assignment_id = $2 AND tenant_id = $3`, at, assignmentID, tenantID)
		return err
	})
}
