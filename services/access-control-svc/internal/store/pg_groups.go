package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/access-control-svc/internal/domain"
)

// Groups (migration 000014): §2's administrative collections of subjects and
// §9's group assignments. Same rules as pg_governance.go: tenant-owned rows,
// read and written under withRLS with an explicit tenant predicate as well.
//
// A group assignment enqueues no event of its own: what it grants is the
// member requests it fans out to, and each of those emits the §23
// iam.assignment.* events an individual request does.

const groupColumns = `group_id, tenant_id, legal_entity_id, group_code, group_name, status, source,
	created_by_principal_id, correlation_id, created_at, updated_at`

func scanGroup(row pgx.Row, g *domain.Group) error {
	return row.Scan(&g.GroupID, &g.TenantID, &g.LegalEntityID, &g.GroupCode, &g.GroupName, &g.Status, &g.Source,
		&g.CreatedByPrincipalID, &g.CorrelationID, &g.CreatedAt, &g.UpdatedAt)
}

const groupAssignmentColumns = `group_assignment_id, tenant_id, group_id, role_definition_id, legal_entity_id,
	effective_from, effective_to, justification, status, created_by_principal_id,
	COALESCE(revoked_by_principal_id, ''), COALESCE(revocation_reason, ''), revoked_at, correlation_id, created_at`

func scanGroupAssignment(row pgx.Row, a *domain.GroupAssignment) error {
	return row.Scan(&a.GroupAssignmentID, &a.TenantID, &a.GroupID, &a.RoleDefinitionID, &a.LegalEntityID,
		&a.EffectiveFrom, &a.EffectiveTo, &a.Justification, &a.Status, &a.CreatedByPrincipalID,
		&a.RevokedByPrincipalID, &a.RevocationReason, &a.RevokedAt, &a.CorrelationID, &a.CreatedAt)
}

// CreateGroup records a group. Idempotent on (tenant_id, correlation_id): a
// replay is answered with the stored group and created=false.
func (s *PgStore) CreateGroup(ctx context.Context, g *domain.Group) (created bool, err error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO iam_groups (group_id, tenant_id, legal_entity_id, group_code, group_name, status, source,
				created_by_principal_id, correlation_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			g.GroupID, tenantID, g.LegalEntityID, g.GroupCode, g.GroupName, g.Status, g.Source,
			g.CreatedByPrincipalID, g.CorrelationID, g.CreatedAt, g.UpdatedAt)
		if err != nil {
			if uniqueViolation(err, "iam_groups_tenant_id_group_code_key") {
				return domain.ErrGroupCodeExists
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			return scanGroup(tx.QueryRow(ctx, "SELECT "+groupColumns+" FROM iam_groups WHERE tenant_id = $1 AND correlation_id = $2", tenantID, g.CorrelationID), g)
		}
		created = true
		return nil
	})
	return created, err
}

// GetGroup reads a group with its live members.
func (s *PgStore) GetGroup(ctx context.Context, groupID string) (*domain.Group, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(groupID) {
		return nil, domain.ErrGroupNotFound
	}
	var g domain.Group
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanGroup(tx.QueryRow(ctx, "SELECT "+groupColumns+" FROM iam_groups WHERE tenant_id = $1 AND group_id = $2", tenantID, groupID), &g); err != nil {
			return err
		}
		g.Members, err = liveMembers(ctx, tx, tenantID, groupID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func liveMembers(ctx context.Context, tx pgx.Tx, tenantID, groupID string) ([]domain.GroupMember, error) {
	rows, err := tx.Query(ctx, `
		SELECT principal_id, added_by_principal_id, added_at FROM iam_group_members
		 WHERE tenant_id = $1 AND group_id = $2 AND removed_at IS NULL ORDER BY added_at, principal_id`, tenantID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.GroupMember{}
	for rows.Next() {
		var m domain.GroupMember
		if err := rows.Scan(&m.PrincipalID, &m.AddedByPrincipalID, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PgStore) ListGroups(ctx context.Context) ([]domain.Group, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.Group{}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+groupColumns+" FROM iam_groups WHERE tenant_id = $1 ORDER BY group_code", tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g domain.Group
			if err := scanGroup(rows, &g); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

// AddGroupMember opens a membership. A principal already a live member is
// ErrGroupMemberExists; a retired group takes no members.
func (s *PgStore) AddGroupMember(ctx context.Context, groupID, principalID, actorID string) (*domain.GroupMember, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(groupID) {
		return nil, domain.ErrGroupNotFound
	}
	var m domain.GroupMember
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, "SELECT status FROM iam_groups WHERE tenant_id = $1 AND group_id = $2 FOR UPDATE", tenantID, groupID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrGroupNotFound
			}
			return err
		}
		if status != domain.GroupActive {
			return domain.ErrGroupRetired
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO iam_group_members (group_id, tenant_id, principal_id, added_by_principal_id)
			VALUES ($1, $2, $3, $4)
			RETURNING principal_id, added_by_principal_id, added_at`,
			groupID, tenantID, principalID, actorID).Scan(&m.PrincipalID, &m.AddedByPrincipalID, &m.AddedAt)
		if uniqueViolation(err, "idx_iam_group_members_live") {
			return domain.ErrGroupMemberExists
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE iam_groups SET updated_at = now() WHERE tenant_id = $1 AND group_id = $2", tenantID, groupID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// RemoveGroupMember closes a live membership; the row is kept as history.
func (s *PgStore) RemoveGroupMember(ctx context.Context, groupID, principalID, actorID, reason string) error {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return err
	}
	if !validID(groupID) {
		return domain.ErrGroupNotFound
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE iam_group_members SET removed_at = now(), removed_by_principal_id = $1, removal_reason = $2
			 WHERE tenant_id = $3 AND group_id = $4 AND principal_id = $5 AND removed_at IS NULL`,
			actorID, reason, tenantID, groupID, principalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrGroupMemberNotFound
		}
		_, err = tx.Exec(ctx, "UPDATE iam_groups SET updated_at = now() WHERE tenant_id = $1 AND group_id = $2", tenantID, groupID)
		return err
	})
}

// FindGroupAssignmentByCorrelation returns (nil, nil) for an unused id.
func (s *PgStore) FindGroupAssignmentByCorrelation(ctx context.Context, correlationID string) (*domain.GroupAssignment, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var a domain.GroupAssignment
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanGroupAssignment(tx.QueryRow(ctx, "SELECT "+groupAssignmentColumns+" FROM iam_group_assignments WHERE tenant_id = $1 AND correlation_id = $2", tenantID, correlationID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateGroupAssignment records "group + role + scope + dates". Idempotent on
// the correlation id. The caller fans it out to the members afterwards.
func (s *PgStore) CreateGroupAssignment(ctx context.Context, a *domain.GroupAssignment) (created bool, err error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	if !validID(a.GroupID) {
		return false, domain.ErrGroupNotFound
	}
	if !validID(a.RoleDefinitionID) {
		return false, domain.ErrRoleNotFound
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO iam_group_assignments (group_assignment_id, tenant_id, group_id, role_definition_id, legal_entity_id,
				effective_from, effective_to, justification, status, created_by_principal_id, correlation_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			a.GroupAssignmentID, tenantID, a.GroupID, a.RoleDefinitionID, a.LegalEntityID,
			a.EffectiveFrom, a.EffectiveTo, a.Justification, a.Status, a.CreatedByPrincipalID, a.CorrelationID, a.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return scanGroupAssignment(tx.QueryRow(ctx, "SELECT "+groupAssignmentColumns+" FROM iam_group_assignments WHERE tenant_id = $1 AND correlation_id = $2", tenantID, a.CorrelationID), a)
		}
		created = true
		return nil
	})
	return created, err
}

func (s *PgStore) GetGroupAssignment(ctx context.Context, groupAssignmentID string) (*domain.GroupAssignment, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(groupAssignmentID) {
		return nil, domain.ErrGroupAssignmentNotFound
	}
	var a domain.GroupAssignment
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanGroupAssignment(tx.QueryRow(ctx, "SELECT "+groupAssignmentColumns+" FROM iam_group_assignments WHERE tenant_id = $1 AND group_assignment_id = $2", tenantID, groupAssignmentID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrGroupAssignmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListGroupAssignments lists a group's assignments; activeOnly narrows to the
// ones a new member must be fanned out to.
func (s *PgStore) ListGroupAssignments(ctx context.Context, groupID string, activeOnly bool) ([]domain.GroupAssignment, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.GroupAssignment{}
	if !validID(groupID) {
		return out, nil
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + groupAssignmentColumns + " FROM iam_group_assignments WHERE tenant_id = $1 AND group_id = $2"
		if activeOnly {
			q += " AND status = 'ACTIVE'"
		}
		rows, err := tx.Query(ctx, q+" ORDER BY created_at", tenantID, groupID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.GroupAssignment
			if err := scanGroupAssignment(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// MarkGroupAssignmentRevoked closes a group assignment once every member
// request it fanned out to has been ended.
func (s *PgStore) MarkGroupAssignmentRevoked(ctx context.Context, groupAssignmentID, actorID, reason string) (*domain.GroupAssignment, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(groupAssignmentID) {
		return nil, domain.ErrGroupAssignmentNotFound
	}
	var a domain.GroupAssignment
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE iam_group_assignments
			   SET status = 'REVOKED', revoked_by_principal_id = $1, revocation_reason = $2, revoked_at = $3
			 WHERE tenant_id = $4 AND group_assignment_id = $5 AND status = 'ACTIVE'`,
			actorID, reason, time.Now().UTC(), tenantID, groupAssignmentID)
		if err != nil {
			return err
		}
		if err := scanGroupAssignment(tx.QueryRow(ctx, "SELECT "+groupAssignmentColumns+" FROM iam_group_assignments WHERE tenant_id = $1 AND group_assignment_id = $2", tenantID, groupAssignmentID), &a); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrGroupAssignmentRevoked
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrGroupAssignmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListGroupAssignmentRequests lists the member requests a group assignment
// fanned out to; principalID narrows to one member.
func (s *PgStore) ListGroupAssignmentRequests(ctx context.Context, groupAssignmentID, principalID string) ([]domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.AssignmentRequest{}
	if !validID(groupAssignmentID) {
		return out, nil
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + assignmentColumns + " FROM assignment_requests WHERE tenant_id = $1 AND group_assignment_id = $2"
		args := []any{tenantID, groupAssignmentID}
		if principalID != "" {
			q += " AND target_principal_id = $3"
			args = append(args, principalID)
		}
		rows, err := tx.Query(ctx, q+" ORDER BY created_at", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.AssignmentRequest
			if err := scanAssignment(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}
