package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

// The governance store: the permission taxonomy, role templates, assignment
// requests and access-review campaigns (migrations 000008-000011). Same rules
// as pg_store.go: every tenant-owned read and write runs under withRLS with an
// explicit tenant predicate as well, and every state change enqueues its event
// in the transaction that made it.

func requestTenant(ctx context.Context) (string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return "", domain.ErrIdentityMissing
	}
	return tenantID, nil
}

// requestCorrelation is the correlation id of the request in flight, falling
// back to the stored one. An event stamped with the id of the request that
// created a row cannot be joined to the later request that changed it.
func requestCorrelation(ctx context.Context, fallback string) string {
	if cid := svcmiddleware.CorrelationFromContext(ctx); cid != "" {
		return cid
	}
	return fallback
}

// ── permission taxonomy (000008) ────────────────────────────────────────────

// PermissionCatalogue reads permission_definitions. Platform-wide: the
// request tenant is set only because every transaction here runs under
// withRLS; the read policy is USING (true).
type PermissionCatalogue struct{ s *PgStore }

func NewPermissionCatalogue(s *PgStore) *PermissionCatalogue { return &PermissionCatalogue{s: s} }

const permissionColumns = `
	p.action_name, p.naming, p.risk_tier, p.description,
	EXISTS (SELECT 1 FROM protected_permissions pp WHERE pp.action_name = p.action_name AND pp.active_flag) AS protected
`

// Lookup returns the ACTIVE definitions among actions, keyed by exact name.
// An action absent from the map is unregistered. Exact match on purpose:
// "Payment.Release" is not "payment.release", and a bundle granting the
// former would grant something no service checks.
func (c *PermissionCatalogue) Lookup(ctx context.Context, actions []string) (map[string]domain.PermissionDefinition, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.PermissionDefinition, len(actions))
	if len(actions) == 0 {
		return out, nil
	}
	err = c.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+permissionColumns+" FROM permission_definitions p WHERE p.active_flag AND p.action_name = ANY($1)", actions)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.PermissionDefinition
			if err := rows.Scan(&d.ActionName, &d.Naming, &d.RiskTier, &d.Description, &d.Protected); err != nil {
				return err
			}
			out[d.ActionName] = d
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read permission_definitions: %w", err)
	}
	return out, nil
}

// List reads the registry, optionally narrowed by naming and a name search.
func (c *PermissionCatalogue) List(ctx context.Context, naming, query string) ([]domain.PermissionDefinition, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.PermissionDefinition
	err = c.s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + permissionColumns + " FROM permission_definitions p WHERE p.active_flag"
		args := []any{}
		if naming != "" {
			args = append(args, naming)
			q += fmt.Sprintf(" AND p.naming = $%d", len(args))
		}
		if query != "" {
			args = append(args, "%"+query+"%")
			q += fmt.Sprintf(" AND p.action_name ILIKE $%d", len(args))
		}
		q += " ORDER BY p.naming DESC, p.action_name"
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.PermissionDefinition
			if err := rows.Scan(&d.ActionName, &d.Naming, &d.RiskTier, &d.Description, &d.Protected); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// ── role templates (000009) ─────────────────────────────────────────────────

const templateSelect = `
	SELECT t.template_code, t.template_name, t.default_intent, t.role_scope_type, t.is_archetype, t.status,
	       COALESCE(v.template_version, 0), COALESCE(v.permitted_actions, '{}')
	  FROM role_templates t
	  LEFT JOIN LATERAL (
	        SELECT template_version, permitted_actions FROM role_template_versions
	         WHERE template_code = t.template_code ORDER BY template_version DESC LIMIT 1
	  ) v ON true
`

func scanTemplate(row pgx.Row, t *domain.RoleTemplate) error {
	return row.Scan(&t.TemplateCode, &t.TemplateName, &t.DefaultIntent, &t.RoleScopeType, &t.IsArchetype, &t.Status,
		&t.LatestVersion, &t.LatestActions)
}

func (s *PgStore) ListTemplates(ctx context.Context) ([]domain.RoleTemplate, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.RoleTemplate
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, templateSelect+" ORDER BY t.is_archetype DESC, t.template_code")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.RoleTemplate
			if err := scanTemplate(rows, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetTemplate(ctx context.Context, code string) (*domain.RoleTemplate, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var t domain.RoleTemplate
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanTemplate(tx.QueryRow(ctx, templateSelect+" WHERE t.template_code = $1", code), &t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *PgStore) ListTemplateVersions(ctx context.Context, code string) ([]domain.RoleTemplateVersion, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.RoleTemplateVersion
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT template_code, template_version, permitted_actions, change_note, published_by, published_at
			  FROM role_template_versions WHERE template_code = $1 ORDER BY template_version DESC`, code)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.RoleTemplateVersion
			if err := rows.Scan(&v.TemplateCode, &v.TemplateVersion, &v.PermittedActions, &v.ChangeNote, &v.PublishedBy, &v.PublishedAt); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// GetTemplateVersion reads one version; version 0 means the latest.
func (s *PgStore) GetTemplateVersion(ctx context.Context, code string, version int) (*domain.RoleTemplateVersion, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var v domain.RoleTemplateVersion
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT template_code, template_version, permitted_actions, change_note, published_by, published_at
			  FROM role_template_versions
			 WHERE template_code = $1 AND ($2 = 0 OR template_version = $2)
			 ORDER BY template_version DESC LIMIT 1`, code, version).
			Scan(&v.TemplateCode, &v.TemplateVersion, &v.PermittedActions, &v.ChangeNote, &v.PublishedBy, &v.PublishedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrTemplateVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateTemplateRole records a role instantiated from a template AND its one
// template-managed bundle, in one transaction, with their events. Idempotent
// on the role's (tenant_id, correlation_id): a replay answers the original
// role and bundle and enqueues nothing.
func (s *PgStore) CreateTemplateRole(ctx context.Context, r *domain.RoleDefinition, b *domain.PermissionBundleDef, actorID string) (created bool, err error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO role_definitions (
				role_definition_id, tenant_id, role_code, role_name, role_scope_type,
				status, created_by_principal_id, correlation_id, created_at, updated_at,
				template_code, template_version
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			r.RoleDefinitionID, tenantID, r.RoleCode, r.RoleName, r.RoleScopeType,
			string(r.Status), r.CreatedByPrincipalID, r.CorrelationID, r.CreatedAt, r.UpdatedAt,
			r.TemplateCode, r.TemplateVersion)
		if err != nil {
			if uniqueViolation(err, "role_definitions_tenant_id_role_code_key") {
				return domain.ErrRoleCodeExists
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			if err := scanRole(tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND correlation_id = $2", tenantID, r.CorrelationID), r); err != nil {
				return err
			}
			return scanBundle(tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND template_code IS NOT NULL", tenantID, r.RoleDefinitionID), b)
		}
		created = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO permission_bundle_defs (
				bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
				active_flag, correlation_id, created_at, updated_at, template_code, template_version
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			b.BundleID, tenantID, b.RoleDefinitionID, b.BundleCode, b.PermittedActions,
			b.ActiveFlag, b.CorrelationID, b.CreatedAt, b.UpdatedAt, b.TemplateCode, b.TemplateVersion); err != nil {
			return err
		}
		roleEv, err := events.RoleCreated(*r, actorID)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, tx, tenantID, roleEv); err != nil {
			return err
		}
		if err := enqueueBundleChange(ctx, tx, *b, r, actorID); err != nil {
			return err
		}
		pub, err := events.RolePublished(*r, b.PermittedActions, actorID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, pub)
	})
	return created, err
}

// GetTemplateBundle reads the template-managed bundle of a role.
func (s *PgStore) GetTemplateBundle(ctx context.Context, roleDefinitionID string) (*domain.PermissionBundleDef, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(roleDefinitionID) {
		return nil, domain.ErrRoleNotFound
	}
	var b domain.PermissionBundleDef
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanBundle(tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND template_code IS NOT NULL", tenantID, roleDefinitionID), &b)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotTemplateRole
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpgradeTemplateRole moves a role to a newer template version: the
// template-managed bundle takes the version's actions, and both rows record
// the version. Enqueues the bundle pair (permission.bundle.updated +
// role.updated, which ends the sessions holding the old grant) and
// iam.role.published.
func (s *PgStore) UpgradeTemplateRole(ctx context.Context, roleDefinitionID string, version int, actions []string, actorID string) (*domain.InstantiatedRole, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(roleDefinitionID) {
		return nil, domain.ErrRoleNotFound
	}
	var out domain.InstantiatedRole
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.RoleDefinition
		if err := scanRole(tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2 FOR UPDATE", tenantID, roleDefinitionID), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRoleNotFound
			}
			return err
		}
		if cur.TemplateCode == "" {
			return domain.ErrNotTemplateRole
		}
		if version <= cur.TemplateVersion {
			return domain.ErrTemplateVersionNotNewer
		}
		if _, err := tx.Exec(ctx, `
			UPDATE permission_bundle_defs
			   SET permitted_actions = $1, template_version = $2, updated_by_principal_id = $3, updated_at = now()
			 WHERE tenant_id = $4 AND role_definition_id = $5 AND template_code IS NOT NULL`,
			actions, version, actorID, tenantID, roleDefinitionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE role_definitions SET template_version = $1, updated_by_principal_id = $2, updated_at = now()
			 WHERE tenant_id = $3 AND role_definition_id = $4`,
			version, actorID, tenantID, roleDefinitionID); err != nil {
			return err
		}
		if err := scanRole(tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2", tenantID, roleDefinitionID), &out.Role); err != nil {
			return err
		}
		if err := scanBundle(tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND template_code IS NOT NULL", tenantID, roleDefinitionID), &out.Bundle); err != nil {
			return err
		}
		forEvent := out.Role
		forEvent.CorrelationID = requestCorrelation(ctx, forEvent.CorrelationID)
		bundleForEvent := out.Bundle
		bundleForEvent.CorrelationID = forEvent.CorrelationID
		if err := enqueueBundleChange(ctx, tx, bundleForEvent, &forEvent, actorID); err != nil {
			return err
		}
		pub, err := events.RolePublished(forEvent, actions, actorID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, pub)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── assignment requests (000010) ────────────────────────────────────────────

const assignmentColumns = `
	request_id, tenant_id, target_principal_id, role_definition_id, legal_entity_id, effective_from,
	justification, risk_tier, approval_required, approval_reason, status, requested_by_principal_id,
	COALESCE(decided_by_principal_id, ''), COALESCE(decision_reason, ''), decided_at,
	COALESCE(authz_assignment_id::text, ''), COALESCE(revoked_by_principal_id, ''), COALESCE(revocation_reason, ''),
	revoked_at, correlation_id, created_at, updated_at, effective_to, COALESCE(group_assignment_id::text, '')
`

func scanAssignment(row pgx.Row, a *domain.AssignmentRequest) error {
	return row.Scan(&a.RequestID, &a.TenantID, &a.TargetPrincipalID, &a.RoleDefinitionID, &a.LegalEntityID, &a.EffectiveFrom,
		&a.Justification, &a.RiskTier, &a.ApprovalRequired, &a.ApprovalReason, &a.Status, &a.RequestedByPrincipalID,
		&a.DecidedByPrincipalID, &a.DecisionReason, &a.DecidedAt,
		&a.AuthzAssignmentID, &a.RevokedByPrincipalID, &a.RevocationReason,
		&a.RevokedAt, &a.CorrelationID, &a.CreatedAt, &a.UpdatedAt, &a.EffectiveTo, &a.GroupAssignmentID)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// FindAssignmentByCorrelation answers a replay BEFORE anything is provisioned.
// Returns (nil, nil) when the correlation id is unused.
func (s *PgStore) FindAssignmentByCorrelation(ctx context.Context, correlationID string) (*domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var a domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND correlation_id = $2", tenantID, correlationID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// PendingAssignmentExists reports an open request for the same subject, role
// and entity, so a second request cannot queue behind the first and be
// approved twice.
func (s *PgStore) PendingAssignmentExists(ctx context.Context, target, roleDefinitionID, legalEntityID string) (bool, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	var exists bool
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM assignment_requests
			 WHERE tenant_id = $1 AND target_principal_id = $2 AND role_definition_id = $3
			   AND legal_entity_id = $4 AND status = 'PENDING_APPROVAL')`,
			tenantID, target, roleDefinitionID, legalEntityID).Scan(&exists)
	})
	return exists, err
}

// CreateAssignmentRequest records a request. A PROVISIONED row (a STANDARD
// request provisioned on submission) enqueues iam.assignment.granted; a
// PENDING_APPROVAL row enqueues iam.assignment.requested.
func (s *PgStore) CreateAssignmentRequest(ctx context.Context, a *domain.AssignmentRequest, actorID string) (created bool, err error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	if !validID(a.RoleDefinitionID) {
		return false, domain.ErrRoleNotFound
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO assignment_requests (
				request_id, tenant_id, target_principal_id, role_definition_id, legal_entity_id, effective_from,
				justification, risk_tier, approval_required, approval_reason, status, requested_by_principal_id,
				authz_assignment_id, correlation_id, created_at, updated_at, effective_to, group_assignment_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			a.RequestID, tenantID, a.TargetPrincipalID, a.RoleDefinitionID, a.LegalEntityID, a.EffectiveFrom,
			a.Justification, a.RiskTier, a.ApprovalRequired, a.ApprovalReason, a.Status, a.RequestedByPrincipalID,
			nullable(a.AuthzAssignmentID), a.CorrelationID, a.CreatedAt, a.UpdatedAt, a.EffectiveTo, nullable(a.GroupAssignmentID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND correlation_id = $2", tenantID, a.CorrelationID), a)
		}
		created = true
		var ev events.Outbound
		if a.Status == domain.AssignmentProvisioned {
			ev, err = events.AssignmentGranted(*a, actorID)
		} else {
			ev, err = events.AssignmentRequested(*a, actorID)
		}
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	return created, err
}

func (s *PgStore) GetAssignmentRequest(ctx context.Context, requestID string) (*domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(requestID) {
		return nil, domain.ErrAssignmentNotFound
	}
	var a domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2", tenantID, requestID), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAssignmentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) ListAssignmentRequests(ctx context.Context, f domain.AssignmentListFilter) ([]domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + assignmentColumns + " FROM assignment_requests WHERE tenant_id = $1"
		args := []any{tenantID}
		if f.Status != "" {
			args = append(args, f.Status)
			q += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if f.TargetPrincipalID != "" {
			args = append(args, f.TargetPrincipalID)
			q += fmt.Sprintf(" AND target_principal_id = $%d", len(args))
		}
		q += " ORDER BY created_at DESC"
		if f.Limit > 0 {
			args = append(args, f.Limit)
			q += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if f.Offset > 0 {
			args = append(args, f.Offset)
			q += fmt.Sprintf(" OFFSET $%d", len(args))
		}
		rows, err := tx.Query(ctx, q, args...)
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

// DecideAssignmentRequest moves a PENDING_APPROVAL request to PROVISIONED
// (with authorization-svc's assignment id), REJECTED or CANCELLED. The status
// is re-read under FOR UPDATE, so two approvers racing cannot both provision:
// the second finds the row no longer pending and gets ErrAssignmentState.
func (s *PgStore) DecideAssignmentRequest(ctx context.Context, requestID, toStatus, deciderID, reason, authzAssignmentID string) (*domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(requestID) {
		return nil, domain.ErrAssignmentNotFound
	}
	var a domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE", tenantID, requestID), &a); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAssignmentNotFound
			}
			return err
		}
		if a.Status != domain.AssignmentPendingApproval {
			return domain.ErrAssignmentState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assignment_requests
			   SET status = $1, decided_by_principal_id = $2, decision_reason = $3, decided_at = now(),
			       authz_assignment_id = COALESCE($4::uuid, authz_assignment_id), updated_at = now()
			 WHERE tenant_id = $5 AND request_id = $6`,
			toStatus, deciderID, reason, nullable(authzAssignmentID), tenantID, requestID); err != nil {
			return err
		}
		if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2", tenantID, requestID), &a); err != nil {
			return err
		}
		if toStatus != domain.AssignmentProvisioned {
			return nil
		}
		forEvent := a
		forEvent.CorrelationID = requestCorrelation(ctx, a.CorrelationID)
		ev, err := events.AssignmentGranted(forEvent, deciderID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// RevokeAssignmentRequest moves a PROVISIONED request to REVOKED and enqueues
// iam.assignment.revoked, which identity-context-svc acts on by ending the
// subject's sessions.
func (s *PgStore) RevokeAssignmentRequest(ctx context.Context, requestID, revokerID, reason string) (*domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(requestID) {
		return nil, domain.ErrAssignmentNotFound
	}
	var a domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE", tenantID, requestID), &a); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAssignmentNotFound
			}
			return err
		}
		if a.Status != domain.AssignmentProvisioned {
			return domain.ErrAssignmentState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assignment_requests
			   SET status = 'REVOKED', revoked_by_principal_id = $1, revocation_reason = $2, revoked_at = now(), updated_at = now()
			 WHERE tenant_id = $3 AND request_id = $4`,
			revokerID, reason, tenantID, requestID); err != nil {
			return err
		}
		if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2", tenantID, requestID), &a); err != nil {
			return err
		}
		ev, err := events.AssignmentRevoked(a, requestCorrelation(ctx, a.CorrelationID), revokerID, reason)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ScheduleAssignmentEnd records an effective-dated revoke (§9): the request
// stays PROVISIONED until at, and records who scheduled the end and why. The
// handler has already scheduled the same instant in authorization-svc, which
// enforces it; ExpireDueAssignments closes the row and enqueues
// iam.assignment.revoked once it passes. Only ever brings an end earlier.
func (s *PgStore) ScheduleAssignmentEnd(ctx context.Context, requestID, revokerID, reason string, at time.Time) (*domain.AssignmentRequest, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(requestID) {
		return nil, domain.ErrAssignmentNotFound
	}
	var a domain.AssignmentRequest
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE", tenantID, requestID), &a); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAssignmentNotFound
			}
			return err
		}
		if a.Status != domain.AssignmentProvisioned {
			return domain.ErrAssignmentState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assignment_requests
			   SET effective_to = LEAST(COALESCE(effective_to, 'infinity'::timestamptz), $1),
			       revoked_by_principal_id = $2, revocation_reason = $3, updated_at = now()
			 WHERE tenant_id = $4 AND request_id = $5`,
			at, revokerID, reason, tenantID, requestID); err != nil {
			return err
		}
		return scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2", tenantID, requestID), &a)
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// dueAssignment is one row the expiry sweep found.
type dueAssignment struct {
	tenantID, requestID string
}

// ExpireDueAssignments closes up to limit PROVISIONED requests whose
// effective_to has passed, and enqueues iam.assignment.revoked for each (§23
// "assignment revoked/expired"), so identity-context-svc ends the subject's
// sessions and closes its projection. A row whose end was scheduled by a revoke
// becomes REVOKED; one whose end was set at grant time becomes EXPIRED.
//
// Finding the rows crosses tenants, through migration 000013's SELECT-only
// app.assignment_expiry disjunct. Closing each one does not: it runs in a
// transaction scoped to that row's tenant, re-checks the row under FOR UPDATE,
// and enqueues there, so the outbox's tenant policy admits the event exactly
// as it would a request's.
func (s *PgStore) ExpireDueAssignments(ctx context.Context, limit int) (int, error) {
	var due []dueAssignment
	err := s.withExpiryScan(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, request_id::text FROM assignment_requests
			 WHERE status = 'PROVISIONED' AND effective_to IS NOT NULL AND effective_to <= now()
			 ORDER BY effective_to LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d dueAssignment
			if err := rows.Scan(&d.tenantID, &d.requestID); err != nil {
				return err
			}
			due = append(due, d)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("scan due assignments: %w", err)
	}
	closed := 0
	for _, d := range due {
		ended := false
		err := s.withRLS(ctx, d.tenantID, func(tx pgx.Tx) error {
			var a domain.AssignmentRequest
			if err := scanAssignment(tx.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM assignment_requests WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE", d.tenantID, d.requestID), &a); err != nil {
				return err
			}
			if a.Status != domain.AssignmentProvisioned || a.EffectiveTo == nil || a.EffectiveTo.After(time.Now()) {
				return nil // closed or moved by someone else meanwhile
			}
			status, actor, reason := domain.AssignmentExpired, "system:assignment-expiry", "assignment reached its effective_to"
			if a.RevokedByPrincipalID != "" {
				status, actor, reason = domain.AssignmentRevoked, a.RevokedByPrincipalID, a.RevocationReason
			}
			if _, err := tx.Exec(ctx, `
				UPDATE assignment_requests SET status = $1, revoked_at = effective_to, updated_at = now()
				 WHERE tenant_id = $2 AND request_id = $3`, status, d.tenantID, d.requestID); err != nil {
				return err
			}
			a.Status = status
			ev, err := events.AssignmentEnded(a, actor, reason)
			if err != nil {
				return err
			}
			if err := enqueue(ctx, tx, d.tenantID, ev); err != nil {
				return err
			}
			ended = true
			return nil
		})
		if err != nil {
			return closed, fmt.Errorf("expire assignment %s: %w", d.requestID, err)
		}
		if ended {
			closed++
		}
	}
	return closed, nil
}

// withExpiryScan runs fn with app.assignment_expiry installed: the expiry
// sweep's one cross-tenant read. Named rather than unscoped for the reason
// withRelay is: under FORCE RLS an unscoped reader sees nothing and reports
// nothing wrong.
func (s *PgStore) withExpiryScan(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin expiry scan: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.assignment_expiry', 'true', true)"); err != nil {
		return fmt.Errorf("set expiry scan context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── access reviews (000011) ─────────────────────────────────────────────────

const campaignColumns = `
	campaign_id, tenant_id, campaign_name, review_type, trigger_reason, legal_entity_id,
	default_reviewer_principal_id, status, due_at, created_by_principal_id,
	COALESCE(completed_by_principal_id, ''), completed_at, correlation_id, created_at, updated_at, dormancy_days
`

func scanCampaign(row pgx.Row, c *domain.ReviewCampaign) error {
	return row.Scan(&c.CampaignID, &c.TenantID, &c.CampaignName, &c.ReviewType, &c.TriggerReason, &c.LegalEntityID,
		&c.DefaultReviewerPrincipalID, &c.Status, &c.DueAt, &c.CreatedByPrincipalID,
		&c.CompletedByPrincipalID, &c.CompletedAt, &c.CorrelationID, &c.CreatedAt, &c.UpdatedAt, &c.DormancyDays)
}

const itemColumns = `
	item_id, campaign_id, tenant_id, authz_assignment_id::text, target_principal_id, role_definition_id::text,
	role_code, COALESCE(legal_entity_id, ''), granted_actions, risk_tier, flags, reviewer_principal_id, status,
	COALESCE(decision, ''), COALESCE(decision_reason, ''), COALESCE(decided_by_principal_id, ''), decided_at,
	revocation_applied, created_at, updated_at, last_granted_at, COALESCE(subject_status, '')
`

func scanItem(row pgx.Row, it *domain.ReviewItem) error {
	return row.Scan(&it.ItemID, &it.CampaignID, &it.TenantID, &it.AuthzAssignmentID, &it.TargetPrincipalID, &it.RoleDefinitionID,
		&it.RoleCode, &it.LegalEntityID, &it.GrantedActions, &it.RiskTier, &it.Flags, &it.ReviewerPrincipalID, &it.Status,
		&it.Decision, &it.DecisionReason, &it.DecidedByPrincipalID, &it.DecidedAt,
		&it.RevocationApplied, &it.CreatedAt, &it.UpdatedAt, &it.LastGrantedAt, &it.SubjectStatus)
}

func readItems(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]domain.ReviewItem, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReviewItem
	for rows.Next() {
		var it domain.ReviewItem
		if err := scanItem(rows, &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FindCampaignByCorrelation answers a replay before the snapshot reads
// authorization-svc again. (nil, nil) when unused.
func (s *PgStore) FindCampaignByCorrelation(ctx context.Context, correlationID string) (*domain.ReviewCampaign, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var c domain.ReviewCampaign
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignColumns+" FROM access_review_campaigns WHERE tenant_id = $1 AND correlation_id = $2", tenantID, correlationID), &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateCampaign records a campaign and its snapshot of items, and enqueues
// iam.access_review.started.
func (s *PgStore) CreateCampaign(ctx context.Context, c *domain.ReviewCampaign, items []domain.ReviewItem, actorID string) (created bool, err error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return false, err
	}
	if c.DormancyDays == 0 {
		c.DormancyDays = domain.DefaultDormancyDays
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO access_review_campaigns (
				campaign_id, tenant_id, campaign_name, review_type, trigger_reason, legal_entity_id,
				default_reviewer_principal_id, status, due_at, created_by_principal_id, correlation_id, created_at, updated_at,
				dormancy_days
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING`,
			c.CampaignID, tenantID, c.CampaignName, c.ReviewType, c.TriggerReason, c.LegalEntityID,
			c.DefaultReviewerPrincipalID, c.Status, c.DueAt, c.CreatedByPrincipalID, c.CorrelationID, c.CreatedAt, c.UpdatedAt,
			c.DormancyDays)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			if err := scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignColumns+" FROM access_review_campaigns WHERE tenant_id = $1 AND correlation_id = $2", tenantID, c.CorrelationID), c); err != nil {
				return err
			}
			c.Items, err = readItems(ctx, tx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY risk_tier DESC, target_principal_id", tenantID, c.CampaignID)
			return err
		}
		created = true
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO access_review_items (
					item_id, campaign_id, tenant_id, authz_assignment_id, target_principal_id, role_definition_id,
					role_code, legal_entity_id, granted_actions, risk_tier, flags, reviewer_principal_id, status,
					created_at, updated_at, last_granted_at, subject_status
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
				it.ItemID, c.CampaignID, tenantID, it.AuthzAssignmentID, it.TargetPrincipalID, it.RoleDefinitionID,
				it.RoleCode, nullable(it.LegalEntityID), it.GrantedActions, it.RiskTier, it.Flags, it.ReviewerPrincipalID,
				it.Status, it.CreatedAt, it.UpdatedAt, it.LastGrantedAt, nullable(it.SubjectStatus)); err != nil {
				return err
			}
		}
		c.Items = items
		ev, err := events.AccessReviewStarted(*c, len(items), actorID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	return created, err
}

func (s *PgStore) GetCampaign(ctx context.Context, campaignID string) (*domain.ReviewCampaign, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(campaignID) {
		return nil, domain.ErrCampaignNotFound
	}
	var c domain.ReviewCampaign
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignColumns+" FROM access_review_campaigns WHERE tenant_id = $1 AND campaign_id = $2", tenantID, campaignID), &c); err != nil {
			return err
		}
		c.Items, err = readItems(ctx, tx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY risk_tier DESC, target_principal_id", tenantID, campaignID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCampaignNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Summary = summarize(c.Items)
	return &c, nil
}

func (s *PgStore) ListCampaigns(ctx context.Context, status string, limit, offset int) ([]domain.ReviewCampaign, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.ReviewCampaign
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + campaignColumns + " FROM access_review_campaigns WHERE tenant_id = $1"
		args := []any{tenantID}
		if status != "" {
			args = append(args, status)
			q += fmt.Sprintf(" AND status = $%d", len(args))
		}
		q += " ORDER BY created_at DESC"
		if limit > 0 {
			args = append(args, limit)
			q += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if offset > 0 {
			args = append(args, offset)
			q += fmt.Sprintf(" OFFSET $%d", len(args))
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.ReviewCampaign
			if err := scanCampaign(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// ListReviewItems reads the items assigned to one reviewer (§21 GET
// /v1/iam/access-reviews: "no broad IAM discovery beyond administrable scope").
func (s *PgStore) ListReviewItems(ctx context.Context, reviewerID, status string) ([]domain.ReviewItem, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.ReviewItem
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		q := "SELECT " + itemColumns + " FROM access_review_items WHERE tenant_id = $1 AND reviewer_principal_id = $2"
		args := []any{tenantID, reviewerID}
		if status != "" {
			args = append(args, status)
			q += fmt.Sprintf(" AND status = $%d", len(args))
		}
		q += " ORDER BY created_at DESC"
		out, err = readItems(ctx, tx, q, args...)
		return err
	})
	return out, err
}

func (s *PgStore) GetReviewItem(ctx context.Context, campaignID, itemID string) (*domain.ReviewItem, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(campaignID) || !validID(itemID) {
		return nil, domain.ErrReviewItemNotFound
	}
	var it domain.ReviewItem
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return scanItem(tx.QueryRow(ctx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 AND item_id = $3", tenantID, campaignID, itemID), &it)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReviewItemNotFound
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// lockOpenCampaign locks a campaign row and refuses a completed one.
func lockOpenCampaign(ctx context.Context, tx pgx.Tx, tenantID, campaignID string) (*domain.ReviewCampaign, error) {
	var c domain.ReviewCampaign
	if err := scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignColumns+" FROM access_review_campaigns WHERE tenant_id = $1 AND campaign_id = $2 FOR UPDATE", tenantID, campaignID), &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrCampaignNotFound
		}
		return nil, err
	}
	if c.Status != domain.CampaignOpen {
		return nil, domain.ErrCampaignClosed
	}
	return &c, nil
}

// DecideReviewItem records a decision. ESCALATE leaves the item ESCALATED (it
// still blocks completion if high-risk); the others leave it DECIDED. When
// revoked is true the handler has already revoked the assignment in
// authorization-svc: the item records it, any assignment request that created
// that assignment is closed as REVOKED, and iam.assignment.revoked is
// enqueued so the subject's sessions end.
func (s *PgStore) DecideReviewItem(ctx context.Context, campaignID, itemID, decision, reason, deciderID string, revoked bool) (*domain.ReviewItem, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(campaignID) || !validID(itemID) {
		return nil, domain.ErrReviewItemNotFound
	}
	var it domain.ReviewItem
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := lockOpenCampaign(ctx, tx, tenantID, campaignID); err != nil {
			return err
		}
		if err := scanItem(tx.QueryRow(ctx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 AND item_id = $3 FOR UPDATE", tenantID, campaignID, itemID), &it); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrReviewItemNotFound
			}
			return err
		}
		if it.Status == domain.ReviewItemDecided || it.Status == domain.ReviewItemExpired {
			return domain.ErrItemAlreadyDecided
		}
		status := domain.ReviewItemDecided
		if decision == domain.DecisionEscalate {
			status = domain.ReviewItemEscalated
		}
		if _, err := tx.Exec(ctx, `
			UPDATE access_review_items
			   SET status = $1, decision = $2, decision_reason = $3, decided_by_principal_id = $4, decided_at = now(),
			       revocation_applied = $5, updated_at = now()
			 WHERE tenant_id = $6 AND item_id = $7`,
			status, decision, reason, deciderID, revoked, tenantID, itemID); err != nil {
			return err
		}
		if err := scanItem(tx.QueryRow(ctx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND item_id = $2", tenantID, itemID), &it); err != nil {
			return err
		}
		if !revoked {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assignment_requests
			   SET status = 'REVOKED', revoked_by_principal_id = $1, revocation_reason = $2, revoked_at = now(), updated_at = now()
			 WHERE tenant_id = $3 AND authz_assignment_id = $4::uuid AND status = 'PROVISIONED'`,
			deciderID, "access review: "+reason, tenantID, it.AuthzAssignmentID); err != nil {
			return err
		}
		ev, err := events.AssignmentRevokedByReview(it, requestCorrelation(ctx, ""), deciderID, reason)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// ReassignReviewItem hands an undecided item to another reviewer.
func (s *PgStore) ReassignReviewItem(ctx context.Context, campaignID, itemID, reviewerID string) (*domain.ReviewItem, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(campaignID) || !validID(itemID) {
		return nil, domain.ErrReviewItemNotFound
	}
	var it domain.ReviewItem
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := lockOpenCampaign(ctx, tx, tenantID, campaignID); err != nil {
			return err
		}
		if err := scanItem(tx.QueryRow(ctx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 AND item_id = $3 FOR UPDATE", tenantID, campaignID, itemID), &it); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrReviewItemNotFound
			}
			return err
		}
		if it.Status == domain.ReviewItemDecided || it.Status == domain.ReviewItemExpired {
			return domain.ErrItemAlreadyDecided
		}
		// An escalated item goes back to OPEN for its new reviewer.
		if _, err := tx.Exec(ctx, `
			UPDATE access_review_items SET reviewer_principal_id = $1, status = 'OPEN',
			       decision = NULL, decision_reason = NULL, decided_by_principal_id = NULL, decided_at = NULL, updated_at = now()
			 WHERE tenant_id = $2 AND item_id = $3`, reviewerID, tenantID, itemID); err != nil {
			return err
		}
		return scanItem(tx.QueryRow(ctx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND item_id = $2", tenantID, itemID), &it)
	})
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func summarize(items []domain.ReviewItem) map[string]int {
	sum := map[string]int{"total": len(items)}
	for _, it := range items {
		sum["status_"+it.Status]++
		if it.Decision != "" {
			sum["decision_"+it.Decision]++
		}
		if it.RevocationApplied {
			sum["revocations"]++
		}
	}
	return sum
}

// CompleteCampaign closes a campaign. Refused while any HIGH/CRITICAL item is
// OPEN or ESCALATED (§24: an unresolved high-risk review cannot silently
// close). STANDARD items still OPEN become EXPIRED, counted in the summary
// and the completion event, so nothing closes without a record of it.
func (s *PgStore) CompleteCampaign(ctx context.Context, campaignID, actorID string) (*domain.ReviewCampaign, error) {
	tenantID, err := requestTenant(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(campaignID) {
		return nil, domain.ErrCampaignNotFound
	}
	var c *domain.ReviewCampaign
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		c, err = lockOpenCampaign(ctx, tx, tenantID, campaignID)
		if err != nil {
			return err
		}
		var unresolved int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM access_review_items
			 WHERE tenant_id = $1 AND campaign_id = $2 AND risk_tier IN ('HIGH', 'CRITICAL')
			   AND status IN ('OPEN', 'ESCALATED')`, tenantID, campaignID).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved > 0 {
			return fmt.Errorf("%w (%d)", domain.ErrUnresolvedHighRisk, unresolved)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE access_review_items SET status = 'EXPIRED', updated_at = now()
			 WHERE tenant_id = $1 AND campaign_id = $2 AND status IN ('OPEN', 'ESCALATED')`, tenantID, campaignID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE access_review_campaigns SET status = 'COMPLETED', completed_by_principal_id = $1, completed_at = now(), updated_at = now()
			 WHERE tenant_id = $2 AND campaign_id = $3`, actorID, tenantID, campaignID); err != nil {
			return err
		}
		if err := scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignColumns+" FROM access_review_campaigns WHERE tenant_id = $1 AND campaign_id = $2", tenantID, campaignID), c); err != nil {
			return err
		}
		c.Items, err = readItems(ctx, tx, "SELECT "+itemColumns+" FROM access_review_items WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY risk_tier DESC, target_principal_id", tenantID, campaignID)
		if err != nil {
			return err
		}
		c.Summary = summarize(c.Items)
		ev, err := events.AccessReviewCompleted(*c, requestCorrelation(ctx, c.CorrelationID), actorID, c.Summary)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ── idempotency keys (000012) ───────────────────────────────────────────────

// IdempotencyRecord is a command's first response.
type IdempotencyRecord struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	CreatedAt          time.Time
}

// ErrIdempotencyFingerprintMismatch: the key was used for a different request.
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused with a different request")

// IdempotencyInFlightLease is how long an unfinished claim is honoured before
// an identical retry may take it over. Well above the 15s WriteTimeout.
const IdempotencyInFlightLease = 5 * time.Minute

// ClaimIdempotencyKey reserves (tenant, endpoint, key). (nil, nil): execute.
// A record: a genuine retry, answer from it (status 0 = still in flight).
// ErrIdempotencyFingerprintMismatch: the key belongs to another request.
// The same algorithm as identity-context-svc's pg_idempotency.go.
func (s *PgStore) ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*IdempotencyRecord, error) {
	if tenantID == "" || endpoint == "" || key == "" {
		return nil, errors.New("ClaimIdempotencyKey: tenant_id, endpoint and idempotency_key are required")
	}
	var rec *IdempotencyRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint, response_status, response_body)
			VALUES ($1,$2,$3,$4,0,'null'::jsonb)
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO UPDATE SET created_at = now()
			 WHERE idempotency_keys.response_status = 0
			   AND idempotency_keys.created_at < now() - make_interval(secs => $5)
			   AND idempotency_keys.request_fingerprint = EXCLUDED.request_fingerprint`,
			tenantID, endpoint, key, fingerprint, IdempotencyInFlightLease.Seconds())
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var existing IdempotencyRecord
		if err := tx.QueryRow(ctx, `
			SELECT request_fingerprint, response_status, response_body, created_at
			  FROM idempotency_keys WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(&existing.RequestFingerprint, &existing.ResponseStatus, &existing.ResponseBody, &existing.CreatedAt); err != nil {
			return fmt.Errorf("read existing idempotency key: %w", err)
		}
		if existing.RequestFingerprint != fingerprint {
			return ErrIdempotencyFingerprintMismatch
		}
		rec = &existing
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// CompleteIdempotencyKey records the response a claimed command produced.
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	if len(body) == 0 {
		body = []byte("null")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET response_status = $1, response_body = $2
			 WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`,
			status, body, tenantID, endpoint, key)
		return err
	})
}

// ReleaseIdempotencyKey drops a claim whose command ended in a 5xx, so a
// retry can genuinely retry.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys
			 WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3 AND response_status = 0`,
			tenantID, endpoint, key)
		return err
	})
}
