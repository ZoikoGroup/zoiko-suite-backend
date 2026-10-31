package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/org-structure-svc/internal/domain"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
)

// ORG-05 org-unit persistence. Every method runs under withRLS, so the tenant
// boundary is enforced by the database as well as by the WHERE clauses.

const (
	pgUniqueViolation = "23505"
	pgInvalidText     = "22P02"
	orgUnitCodeIndex  = "idx_org_units_code"
)

// orgUnitSelect reads a unit plus its parent, manager and mapping as of $2
// (empty string means today). The interval test is valid_from <= d < valid_to.
const orgUnitSelect = `
	SELECT u.unit_id, u.tenant_id, u.legal_entity_id, u.code, u.name, u.unit_type,
	       u.status, u.location_ref, u.record_version, u.correlation_id,
	       u.created_by, u.created_at, u.updated_at,
	       hv.parent_unit_id, mg.manager_employee_id,
	       COALESCE(mp.cost_center_code, ''), COALESCE(mp.profit_center_code, '')
	FROM d CROSS JOIN org_units u
	LEFT JOIN org_unit_hierarchy_versions hv
	       ON hv.unit_id = u.unit_id AND hv.superseded_at IS NULL
	      AND hv.valid_from <= d.d AND (hv.valid_to IS NULL OR hv.valid_to > d.d)
	LEFT JOIN org_unit_manager_refs mg
	       ON mg.unit_id = u.unit_id
	      AND mg.valid_from <= d.d AND (mg.valid_to IS NULL OR mg.valid_to > d.d)
	LEFT JOIN org_unit_dimension_mappings mp
	       ON mp.unit_id = u.unit_id
	      AND mp.valid_from <= d.d AND (mp.valid_to IS NULL OR mp.valid_to > d.d)
`

func scanOrgUnit(row pgx.Row) (*domain.OrgUnit, error) {
	var u domain.OrgUnit
	var parent, manager *string
	err := row.Scan(&u.UnitID, &u.TenantID, &u.LegalEntityID, &u.Code, &u.Name, &u.UnitType,
		&u.Status, &u.LocationRef, &u.RecordVersion, &u.CorrelationID,
		&u.CreatedBy, &u.CreatedAt, &u.UpdatedAt,
		&parent, &manager, &u.CostCenterCode, &u.ProfitCenterCode)
	if err != nil {
		return nil, err
	}
	u.ParentUnitID, u.ManagerEmployeeID = parent, manager
	return &u, nil
}

// CreateOrgUnit inserts a DRAFT unit with its first hierarchy placement and,
// when given, its manager and cost/profit-center mapping, in one transaction.
//
// Idempotent on (tenant_id, correlation_id): a retry returns the original unit
// with created=false. The parent must exist in the same legal entity.
func (s *PgStore) CreateOrgUnit(ctx context.Context, req *domain.CreateOrgUnitRequest, actorID string) (u *domain.OrgUnit, created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if req.ParentUnitID != nil {
			var parentEntity string
			err := tx.QueryRow(ctx,
				`SELECT legal_entity_id FROM org_units WHERE unit_id = $1`, *req.ParentUnitID).Scan(&parentEntity)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrParentInvalid
			}
			if err != nil {
				return err
			}
			if parentEntity != req.LegalEntityID {
				return domain.ErrParentInvalid
			}
		}

		now := time.Now().UTC()
		unitID := uuid.NewString()
		tag, err := tx.Exec(ctx, `
			INSERT INTO org_units (
				unit_id, tenant_id, legal_entity_id, code, name, unit_type, status,
				location_ref, record_version, correlation_id, created_by, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'DRAFT', $7, 1, $8, $9, $10, $10)
			ON CONFLICT (tenant_id, correlation_id) WHERE correlation_id <> '' DO NOTHING
		`, unitID, tenantID, req.LegalEntityID, req.Code, req.Name, req.UnitType,
			req.LocationRef, req.CorrelationID, actorID, now)
		if err != nil {
			return err
		}

		if tag.RowsAffected() == 0 {
			existing, err := scanOrgUnit(tx.QueryRow(ctx, `
				WITH d AS (SELECT CURRENT_DATE AS d) `+orgUnitSelect+`
				WHERE u.tenant_id = $1 AND u.correlation_id = $2`, tenantID, req.CorrelationID))
			if err != nil {
				return err
			}
			u, created = existing, false
			return nil
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO org_unit_hierarchy_versions (
				version_id, tenant_id, unit_id, parent_unit_id, valid_from,
				recorded_at, change_reason, actor_id
			) VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8)
		`, uuid.NewString(), tenantID, unitID, req.ParentUnitID, req.EffectiveFrom, now, req.Reason, actorID); err != nil {
			return err
		}
		if req.ManagerEmployeeID != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_unit_manager_refs (
					ref_id, tenant_id, unit_id, manager_employee_id, valid_from, recorded_at, actor_id
				) VALUES ($1, $2, $3, $4, $5::date, $6, $7)
			`, uuid.NewString(), tenantID, unitID, *req.ManagerEmployeeID, req.EffectiveFrom, now, actorID); err != nil {
				return err
			}
		}
		if req.CostCenterCode != "" || req.ProfitCenterCode != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO org_unit_dimension_mappings (
					mapping_id, tenant_id, unit_id, cost_center_code, profit_center_code,
					valid_from, recorded_at, actor_id
				) VALUES ($1, $2, $3, $4, $5, $6::date, $7, $8)
			`, uuid.NewString(), tenantID, unitID, req.CostCenterCode, req.ProfitCenterCode,
				req.EffectiveFrom, now, actorID); err != nil {
				return err
			}
		}

		u = &domain.OrgUnit{
			UnitID: unitID, TenantID: tenantID, LegalEntityID: req.LegalEntityID,
			Code: req.Code, Name: req.Name, UnitType: req.UnitType,
			Status: domain.UnitStatusDraft, LocationRef: req.LocationRef, RecordVersion: 1,
			CorrelationID: req.CorrelationID, CreatedBy: actorID, CreatedAt: now, UpdatedAt: now,
			ParentUnitID: req.ParentUnitID, ManagerEmployeeID: req.ManagerEmployeeID,
			CostCenterCode: req.CostCenterCode, ProfitCenterCode: req.ProfitCenterCode,
		}
		created = true
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == orgUnitCodeIndex:
				return nil, false, domain.ErrOrgUnitCodeTaken
			case pgErr.Code == pgInvalidText:
				return nil, false, domain.ErrParentInvalid
			}
		}
		return nil, false, err
	}
	return u, created, nil
}

// GetOrgUnit returns one unit with its parent, manager and mapping as of the
// given date (YYYY-MM-DD); an empty asOf means today.
func (s *PgStore) GetOrgUnit(ctx context.Context, unitID, asOf string) (*domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out *domain.OrgUnit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		u, err := scanOrgUnit(tx.QueryRow(ctx, `
			WITH d AS (SELECT COALESCE(NULLIF($2, '')::date, CURRENT_DATE) AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND u.unit_id = $3`, tenantID, asOf, unitID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOrgUnitNotFound
		}
		out = u
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
			return nil, domain.ErrOrgUnitNotFound
		}
		return nil, err
	}
	return out, nil
}

// ListOrgUnits returns the units of a legal entity (all entities when empty),
// with parent, manager and mapping as of the given date, ordered by code.
func (s *PgStore) ListOrgUnits(ctx context.Context, legalEntityID, asOf string) ([]domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	out := []domain.OrgUnit{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH d AS (SELECT COALESCE(NULLIF($2, '')::date, CURRENT_DATE) AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND ($3 = '' OR u.legal_entity_id = $3)
			ORDER BY u.code ASC`, tenantID, asOf, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanOrgUnit(rows)
			if err != nil {
				return err
			}
			out = append(out, *u)
		}
		return rows.Err()
	})
	return out, err
}
