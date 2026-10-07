// Package store provides the PostgreSQL implementation of
// project-accounting-svc's persistence layer.
//
// Every write is wrapped in withRLS, which sets app.tenant_id on the
// transaction before running any query — the Row-Level Security policies
// are real, but every method ALSO filters explicitly by tenant_id in its
// own SQL: this pool connects as a Postgres superuser (DB_USER=postgres,
// same as every other service in this platform), and Postgres superusers
// unconditionally bypass Row-Level Security regardless of policy. The
// explicit filters are the actual isolation guarantee; RLS is
// defense-in-depth.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func uuidNewString() string {
	return uuid.NewString()
}

// roundCents rounds v to the nearest cent — money is NUMERIC(18,2), and a
// calculation that doesn't land on a whole cent is wrong, not merely
// imprecise. Same helper this platform already uses in
// financial-close-svc/asset-management-svc/inventory-management-svc.
func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const projectColumns = `
	project_id, tenant_id, legal_entity_id, project_code, name, project_type,
	customer_ref, contract_ref, manager_principal_id, cost_center, group_reference, start_date, end_date,
	status, created_at, created_by_principal_id,
	approved_at, approved_by_principal_id, activated_at,
	suspended_at, suspended_by_principal_id, suspension_reason,
	closed_at, closed_by_principal_id, close_reason,
	reopened_at, reopened_by_principal_id, reopen_reason`

func scanProject(row pgx.Row) (*domain.Project, error) {
	var p domain.Project
	var projectType, customerRef, managerPrincipalID, costCenter, groupReference *string
	if err := row.Scan(
		&p.ProjectID, &p.TenantID, &p.LegalEntityID, &p.ProjectCode, &p.Name, &projectType,
		&customerRef, &p.ContractRef, &managerPrincipalID, &costCenter, &groupReference, &p.StartDate, &p.EndDate,
		&p.Status, &p.CreatedAt, &p.CreatedByPrincipalID,
		&p.ApprovedAt, &p.ApprovedByPrincipalID, &p.ActivatedAt,
		&p.SuspendedAt, &p.SuspendedByPrincipalID, &p.SuspensionReason,
		&p.ClosedAt, &p.ClosedByPrincipalID, &p.CloseReason,
		&p.ReopenedAt, &p.ReopenedByPrincipalID, &p.ReopenReason,
	); err != nil {
		return nil, err
	}
	if projectType != nil {
		p.ProjectType = *projectType
	}
	if customerRef != nil {
		p.CustomerRef = *customerRef
	}
	if managerPrincipalID != nil {
		p.ManagerPrincipalID = *managerPrincipalID
	}
	if costCenter != nil {
		p.CostCenter = *costCenter
	}
	if groupReference != nil {
		p.GroupReference = *groupReference
	}
	return &p, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) CreateProject(ctx context.Context, p *domain.Project) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO projects (
				project_id, tenant_id, legal_entity_id, project_code, name, project_type,
				customer_ref, manager_principal_id, cost_center, group_reference, start_date, end_date,
				status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, p.ProjectID, tenantID, p.LegalEntityID, p.ProjectCode, p.Name, nullIfEmpty(p.ProjectType),
			nullIfEmpty(p.CustomerRef), nullIfEmpty(p.ManagerPrincipalID), nullIfEmpty(p.CostCenter), nullIfEmpty(p.GroupReference), p.StartDate, p.EndDate,
			p.Status, p.CreatedAt, p.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateProjectCode
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetProject(ctx context.Context, projectID string) (*domain.Project, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var p *domain.Project
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE project_id = $1 AND tenant_id = $2`, projectID, tenantID)
		var err error
		p, err = scanProject(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrProjectNotFound
		}
		if err != nil {
			return err
		}
		p.WorkPackages, err = s.listWorkPackages(ctx, tx, tenantID, projectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) ListProjects(ctx context.Context, legalEntityID string) ([]domain.Project, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.Project
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProject(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) transitionProject(ctx context.Context, projectID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, projectID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE projects SET status = $1%s
			WHERE project_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidProjectTransition
		}
		return nil
	})
}

func (s *PgStore) ApproveProject(ctx context.Context, projectID, principalID string, at time.Time) error {
	return s.transitionProject(ctx, projectID, domain.ProjectStatusDraft, domain.ProjectStatusApproved,
		", approved_at = $2, approved_by_principal_id = $3", at, principalID)
}

func (s *PgStore) ActivateProject(ctx context.Context, projectID string, at time.Time) error {
	return s.transitionProject(ctx, projectID, domain.ProjectStatusApproved, domain.ProjectStatusActive,
		", activated_at = $2", at)
}

func (s *PgStore) SuspendProject(ctx context.Context, projectID, principalID, reason string, at time.Time) error {
	return s.transitionProject(ctx, projectID, domain.ProjectStatusActive, domain.ProjectStatusSuspended,
		", suspended_at = $2, suspended_by_principal_id = $3, suspension_reason = $4", at, principalID, reason)
}

// CloseProject accepts either ACTIVE or SUSPENDED as the starting status
// — collapsing the spec's own named "Closing" intermediate state, see
// migration 000001's doc comment.
func (s *PgStore) CloseProject(ctx context.Context, projectID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE projects SET status = $1, closed_at = $2, closed_by_principal_id = $3, close_reason = $4
			WHERE project_id = $5 AND status IN ($6, $7) AND tenant_id = $8
		`, domain.ProjectStatusClosed, at, principalID, reason,
			projectID, domain.ProjectStatusActive, domain.ProjectStatusSuspended, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidProjectTransition
		}
		return nil
	})
}

// ReopenProjectControlled refuses self-reopen — the spec's own SoD,
// "closed project reopen requires independent approval" — enforced
// inside the same transaction as the guarded status UPDATE, the same
// pattern INV-02's own SetQuarantine release check uses.
func (s *PgStore) ReopenProjectControlled(ctx context.Context, projectID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var closedBy *string
		err := tx.QueryRow(ctx, `
			SELECT closed_by_principal_id FROM projects WHERE project_id = $1 AND status = $2 AND tenant_id = $3
		`, projectID, domain.ProjectStatusClosed, tenantID).Scan(&closedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidProjectTransition
		}
		if err != nil {
			return err
		}
		if closedBy != nil && *closedBy == principalID {
			return domain.ErrSelfReopenNotPermitted
		}
		tag, err := tx.Exec(ctx, `
			UPDATE projects SET status = $1, reopened_at = $2, reopened_by_principal_id = $3, reopen_reason = $4
			WHERE project_id = $5 AND status = $6 AND tenant_id = $7
		`, domain.ProjectStatusActive, at, principalID, reason, projectID, domain.ProjectStatusClosed, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidProjectTransition
		}
		return nil
	})
}

func (s *PgStore) LinkContract(ctx context.Context, projectID, contractRef string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE projects SET contract_ref = $1 WHERE project_id = $2 AND tenant_id = $3`, contractRef, projectID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrProjectNotFound
		}
		return nil
	})
}

// ── Work packages (WBS) ─────────────────────────────────────────────────────

func (s *PgStore) listWorkPackages(ctx context.Context, tx pgx.Tx, tenantID, projectID string) ([]domain.WorkPackage, error) {
	rows, err := tx.Query(ctx, `
		SELECT wbs_id, project_id, wbs_code, description, parent_wbs_id, effective_from, created_at, created_by_principal_id
		FROM project_work_packages WHERE tenant_id = $1 AND project_id = $2 ORDER BY created_at
	`, tenantID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorkPackage
	for rows.Next() {
		var w domain.WorkPackage
		var description *string
		if err := rows.Scan(&w.WBSID, &w.ProjectID, &w.WBSCode, &description, &w.ParentWBSID, &w.EffectiveFrom, &w.CreatedAt, &w.CreatedByPrincipalID); err != nil {
			return nil, err
		}
		if description != nil {
			w.Description = *description
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AddWorkPackage is the ONLY write path to project_work_packages —
// append-only, see migration 000001's own reject-mutation trigger.
func (s *PgStore) AddWorkPackage(ctx context.Context, w *domain.WorkPackage) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO project_work_packages (
				wbs_id, tenant_id, project_id, wbs_code, description, parent_wbs_id, effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, w.WBSID, tenantID, w.ProjectID, w.WBSCode, nullIfEmpty(w.Description), w.ParentWBSID, w.EffectiveFrom, w.CreatedAt, w.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateWBSCode
			}
			return err
		}
		return nil
	})
}

// ── Financial profiles ───────────────────────────────────────────────────────

func scanFinancialProfile(row pgx.Row) (*domain.FinancialProfile, error) {
	var f domain.FinancialProfile
	if err := row.Scan(
		&f.ProfileVersionID, &f.ProfileID, &f.Version, &f.ProjectID, &f.RecognitionMethod, &f.BillingType,
		&f.Currency, &f.EffectiveFrom, &f.EffectiveTo, &f.CreatedAt, &f.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &f, nil
}

// AmendFinancialProfile end-dates the current version (at the NEW
// version's own effective_from, never "now") and inserts the new one —
// the ONLY way a profile's own parameters change, the real enforcement
// of "Recognition policy changed after run without invalidation."
func (s *PgStore) AmendFinancialProfile(ctx context.Context, projectID string, newVersion *domain.FinancialProfile) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var currentVersion int
		var profileID string
		err := tx.QueryRow(ctx, `
			UPDATE project_financial_profiles SET effective_to = $1
			WHERE tenant_id = $2 AND project_id = $3 AND effective_to IS NULL
			RETURNING profile_id, version
		`, newVersion.EffectiveFrom, tenantID, projectID).Scan(&profileID, &currentVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if profileID != "" {
			newVersion.ProfileID = profileID
			newVersion.Version = currentVersion + 1
		} else {
			newVersion.ProfileID = uuidNewString()
			newVersion.Version = 1
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO project_financial_profiles (
				profile_version_id, profile_id, version, tenant_id, project_id,
				recognition_method, billing_type, currency, effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, newVersion.ProfileVersionID, newVersion.ProfileID, newVersion.Version, tenantID, projectID,
			newVersion.RecognitionMethod, newVersion.BillingType, newVersion.Currency, newVersion.EffectiveFrom, newVersion.CreatedAt, newVersion.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetCurrentFinancialProfile(ctx context.Context, projectID string) (*domain.FinancialProfile, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var f *domain.FinancialProfile
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT profile_version_id, profile_id, version, project_id, recognition_method, billing_type,
				currency, effective_from, effective_to, created_at, created_by_principal_id
			FROM project_financial_profiles WHERE tenant_id = $1 AND project_id = $2 AND effective_to IS NULL
		`, tenantID, projectID)
		var err error
		f, err = scanFinancialProfile(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no profile set yet — a real, valid state, not an error
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// GetProfileAsOf backs GetProjectAsOf's own real point-in-time behavior —
// proof that financial-profile changes are versioned, not rewritten.
func (s *PgStore) GetFinancialProfileAsOf(ctx context.Context, projectID string, at time.Time) (*domain.FinancialProfile, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var f *domain.FinancialProfile
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT profile_version_id, profile_id, version, project_id, recognition_method, billing_type,
				currency, effective_from, effective_to, created_at, created_by_principal_id
			FROM project_financial_profiles
			WHERE tenant_id = $1 AND project_id = $2 AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		`, tenantID, projectID, at)
		var err error
		f, err = scanFinancialProfile(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}
