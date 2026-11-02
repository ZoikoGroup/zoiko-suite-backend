package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/events"
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

		if err := emit(ctx, tx, tenantID, events.OrgEvent{
			Name: events.EventOrgUnitCreated, LegalEntityID: req.LegalEntityID, UnitID: unitID,
			ObjectType: events.ObjectOrgUnit, ObjectID: unitID, ObjectVersion: 1,
			EffectiveAt: req.EffectiveFrom, RecordedAt: now, Actor: actorID, CorrelationID: tracingID(ctx, req.CorrelationID),
			Details: map[string]any{
				"code": req.Code, "name": req.Name, "unit_type": req.UnitType, "status": domain.UnitStatusDraft,
				"parent_unit_id": req.ParentUnitID, "manager_employee_id": req.ManagerEmployeeID,
				"cost_center_code": req.CostCenterCode, "profit_center_code": req.ProfitCenterCode,
				"location_ref": req.LocationRef, "reason": req.Reason,
			},
		}); err != nil {
			return err
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

// TransitionOrgUnit applies ACTIVATE or DEACTIVATE to a unit.
//
// The row is locked first, so two concurrent commands cannot both pass the
// checks. expectedVersion must equal the unit's current record_version
// (ErrVersionConflict otherwise). Activating needs the parent, if any, to be
// ACTIVE today; deactivating needs every child placed under the unit today to
// be INACTIVE. Success bumps record_version by one.
func (s *PgStore) TransitionOrgUnit(ctx context.Context, unitID, command string, expectedVersion int64) (*domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out *domain.OrgUnit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status, entityID string
		var version int64
		err := tx.QueryRow(ctx,
			`SELECT status, record_version, legal_entity_id FROM org_units WHERE tenant_id = $1 AND unit_id = $2 FOR UPDATE`,
			tenantID, unitID).Scan(&status, &version, &entityID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOrgUnitNotFound
		}
		if err != nil {
			return err
		}
		if version != expectedVersion {
			return domain.ErrVersionConflict
		}

		next, err := domain.OrgUnitTransition(status, command)
		if err != nil {
			return err
		}

		switch command {
		case domain.UnitCommandActivate:
			var parentStatus *string
			if err := tx.QueryRow(ctx, `
				SELECT p.status
				FROM org_unit_hierarchy_versions hv
				LEFT JOIN org_units p ON p.unit_id = hv.parent_unit_id
				WHERE hv.unit_id = $1 AND hv.superseded_at IS NULL
				  AND hv.valid_from <= CURRENT_DATE AND (hv.valid_to IS NULL OR hv.valid_to > CURRENT_DATE)`,
				unitID).Scan(&parentStatus); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if parentStatus != nil && *parentStatus != domain.UnitStatusActive {
				return domain.ErrParentNotActive
			}
		case domain.UnitCommandDeactivate:
			var live int
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*)
				FROM org_unit_hierarchy_versions hv
				JOIN org_units c ON c.unit_id = hv.unit_id
				WHERE hv.parent_unit_id = $1 AND hv.superseded_at IS NULL
				  AND hv.valid_from <= CURRENT_DATE AND (hv.valid_to IS NULL OR hv.valid_to > CURRENT_DATE)
				  AND c.status <> 'INACTIVE'`, unitID).Scan(&live); err != nil {
				return err
			}
			if live > 0 {
				return domain.ErrHasActiveChildren
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE org_units SET status = $1, record_version = record_version + 1, updated_at = $2
			WHERE tenant_id = $3 AND unit_id = $4`, next, time.Now().UTC(), tenantID, unitID); err != nil {
			return err
		}

		eventName := events.EventOrgUnitActivated
		if command == domain.UnitCommandDeactivate {
			eventName = events.EventOrgUnitDeactivated
		}
		if err := emit(ctx, tx, tenantID, events.OrgEvent{
			Name: eventName, LegalEntityID: entityID, UnitID: unitID,
			ObjectType: events.ObjectOrgUnit, ObjectID: unitID, ObjectVersion: version + 1, EffectiveAt: today(),
			Details: map[string]any{"from_status": status, "to_status": next},
		}); err != nil {
			return err
		}

		u, err := scanOrgUnit(tx.QueryRow(ctx, `
			WITH d AS (SELECT CURRENT_DATE AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND u.unit_id = $2`, tenantID, unitID))
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

// ReparentOrgUnit moves a unit under newParentID (nil = root) from effectiveAt
// on, by closing its open placement and inserting a new one; nothing is
// rewritten. expectedVersion must be the unit's current record_version.
//
// Reparents in one legal entity are serialized by an advisory lock, so two
// concurrent moves cannot each look safe and together make a loop. The loop
// check walks up from the new parent over every placement still in force at or
// after effectiveAt; that is conservative, so it also refuses a move that would
// only become a loop through an already-scheduled later change.
func (s *PgStore) ReparentOrgUnit(ctx context.Context, unitID string, req *domain.ReparentOrgUnitRequest, expectedVersion int64, actorID string) (*domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out *domain.OrgUnit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return reparentTx(ctx, tx, tenantID, unitID, req, expectedVersion, actorID, "", &out)
	})
	if err != nil {
		return nil, mapReparentErr(err)
	}
	return out, nil
}

// mapReparentErr turns a malformed parent id (Postgres 22P02) into the typed
// error callers expect.
func mapReparentErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
		return domain.ErrParentInvalid
	}
	return err
}

// reparentTx is the whole reparent inside a caller-owned transaction, so the
// direct command, the approval of a pending request and a dry run (rolled back
// by the caller) all run the very same checks. approvalRef, when set, is the
// change request that authorised the move and is kept on the new placement as
// evidence. The updated unit is written to *result.
func reparentTx(ctx context.Context, tx pgx.Tx, tenantID, unitID string, req *domain.ReparentOrgUnitRequest, expectedVersion int64, actorID, approvalRef string, result **domain.OrgUnit) error {
	// The advisory lock comes first, before any row lock. Taking a row lock
	// first deadlocks two opposing moves: each holds its own unit, waits
	// for the advisory lock, and the winner then needs the other's row. A
	// unit's legal entity never changes, so reading it unlocked is safe.
	var entityID string
	err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM org_units WHERE tenant_id = $1 AND unit_id = $2`,
		tenantID, unitID).Scan(&entityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrOrgUnitNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, tenantID+"/"+entityID); err != nil {
		return err
	}

	var status string
	var version int64
	if err := tx.QueryRow(ctx, `
			SELECT status, record_version FROM org_units
			WHERE tenant_id = $1 AND unit_id = $2 FOR UPDATE`, tenantID, unitID).Scan(&status, &version); err != nil {
		return err
	}
	if version != expectedVersion {
		return domain.ErrVersionConflict
	}
	if status == domain.UnitStatusInactive {
		return domain.ErrInvalidTransition
	}

	if req.NewParentUnitID != nil {
		var parentEntity, parentStatus string
		err := tx.QueryRow(ctx, `
				SELECT legal_entity_id, status FROM org_units
				WHERE tenant_id = $1 AND unit_id = $2 FOR SHARE`, tenantID, *req.NewParentUnitID).Scan(&parentEntity, &parentStatus)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && parentEntity != entityID {
			return domain.ErrParentInvalid
		}
		if err != nil {
			return err
		}
		if parentStatus == domain.UnitStatusInactive || status == domain.UnitStatusActive && parentStatus != domain.UnitStatusActive {
			return domain.ErrParentNotActive
		}

		var loop bool
		if err := tx.QueryRow(ctx, `
				WITH RECURSIVE anc(id) AS (
					SELECT $1::uuid
					UNION
					SELECT hv.parent_unit_id
					FROM org_unit_hierarchy_versions hv JOIN anc ON hv.unit_id = anc.id
					WHERE hv.superseded_at IS NULL AND hv.parent_unit_id IS NOT NULL
					  AND (hv.valid_to IS NULL OR hv.valid_to > $2::date)
				)
				SELECT EXISTS (SELECT 1 FROM anc WHERE id = $3::uuid)`,
			*req.NewParentUnitID, req.EffectiveAt, unitID).Scan(&loop); err != nil {
			return err
		}
		if loop {
			return domain.ErrHierarchyLoop
		}
	}

	var openVersion string
	var currentParent *string
	var after bool
	if err := tx.QueryRow(ctx, `
			SELECT version_id, parent_unit_id, valid_from < $2::date
			FROM org_unit_hierarchy_versions
			WHERE unit_id = $1 AND valid_to IS NULL AND superseded_at IS NULL`,
		unitID, req.EffectiveAt).Scan(&openVersion, &currentParent, &after); err != nil {
		return err
	}
	if sameParent(currentParent, req.NewParentUnitID) {
		return domain.ErrNoHierarchyChange
	}
	if !after {
		return domain.ErrEffectiveDateInvalid
	}

	now := time.Now().UTC()
	// closed_at records when the placement was closed, so a read "as known
	// at" an earlier time still sees it open-ended.
	if _, err := tx.Exec(ctx, `UPDATE org_unit_hierarchy_versions SET valid_to = $1::date, closed_at = $3 WHERE version_id = $2`,
		req.EffectiveAt, openVersion, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
			INSERT INTO org_unit_hierarchy_versions (
				version_id, tenant_id, unit_id, parent_unit_id, valid_from, recorded_at, change_reason, actor_id, approval_ref
			) VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8, $9)`,
		uuid.NewString(), tenantID, unitID, req.NewParentUnitID, req.EffectiveAt, now, req.Reason, actorID, approvalRef); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
			UPDATE org_units SET record_version = record_version + 1, updated_at = $1
			WHERE tenant_id = $2 AND unit_id = $3`, now, tenantID, unitID); err != nil {
		return err
	}

	u, err := scanOrgUnit(tx.QueryRow(ctx, `
			WITH d AS (SELECT CURRENT_DATE AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND u.unit_id = $2`, tenantID, unitID))
	if err != nil {
		return err
	}
	*result = u

	// The event is part of this transaction. For an approved change the actor is
	// the maker (who proposed it) and evidence_ref is the approved request.
	return emit(ctx, tx, tenantID, events.OrgEvent{
		Name: events.EventOrgUnitReparented, LegalEntityID: entityID, UnitID: unitID,
		ObjectType: events.ObjectOrgUnit, ObjectID: unitID, ObjectVersion: version + 1,
		EffectiveAt: req.EffectiveAt, RecordedAt: now, Actor: actorID, EvidenceRef: approvalRef,
		Details: map[string]any{
			"previous_parent_unit_id": currentParent, "new_parent_unit_id": req.NewParentUnitID, "reason": req.Reason,
		},
	})
}

func sameParent(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ChangeOrgUnitManager names a new manager from effectiveAt on by closing the
// open manager reference and inserting a new one; the previous manager stays
// in history. expectedVersion must be the unit's current record_version. The
// manager must already have been resolved against employee-master-svc.
func (s *PgStore) ChangeOrgUnitManager(ctx context.Context, unitID string, req *domain.ChangeManagerRequest, expectedVersion int64, actorID string) (*domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out *domain.OrgUnit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status, entityID string
		var version int64
		err := tx.QueryRow(ctx, `
			SELECT status, record_version, legal_entity_id FROM org_units
			WHERE tenant_id = $1 AND unit_id = $2 FOR UPDATE`, tenantID, unitID).Scan(&status, &version, &entityID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOrgUnitNotFound
		}
		if err != nil {
			return err
		}
		if version != expectedVersion {
			return domain.ErrVersionConflict
		}
		if status == domain.UnitStatusInactive {
			return domain.ErrInvalidTransition
		}

		var openRef, current string
		var after bool
		err = tx.QueryRow(ctx, `
			SELECT ref_id, manager_employee_id::text, valid_from < $2::date
			FROM org_unit_manager_refs WHERE unit_id = $1 AND valid_to IS NULL`,
			unitID, req.EffectiveAt).Scan(&openRef, &current, &after)
		hasOpen := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := time.Now().UTC()
		if hasOpen {
			if current == req.ManagerEmployeeID {
				return domain.ErrNoManagerChange
			}
			if !after {
				return domain.ErrEffectiveDateInvalid
			}
			if _, err := tx.Exec(ctx, `UPDATE org_unit_manager_refs SET valid_to = $1::date, closed_at = $3 WHERE ref_id = $2`,
				req.EffectiveAt, openRef, now); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO org_unit_manager_refs (
				ref_id, tenant_id, unit_id, manager_employee_id, valid_from, recorded_at, actor_id
			) VALUES ($1, $2, $3, $4, $5::date, $6, $7)`,
			uuid.NewString(), tenantID, unitID, req.ManagerEmployeeID, req.EffectiveAt, now, actorID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE org_units SET record_version = record_version + 1, updated_at = $1
			WHERE tenant_id = $2 AND unit_id = $3`, now, tenantID, unitID); err != nil {
			return err
		}

		u, err := scanOrgUnit(tx.QueryRow(ctx, `
			WITH d AS (SELECT CURRENT_DATE AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND u.unit_id = $2`, tenantID, unitID))
		if err != nil {
			return err
		}
		out = u

		var previous *string // nil when the unit had no manager
		if hasOpen {
			previous = &current
		}
		return emit(ctx, tx, tenantID, events.OrgEvent{
			Name: events.EventOrgUnitManagerChanged, LegalEntityID: entityID, UnitID: unitID,
			ObjectType: events.ObjectOrgUnit, ObjectID: unitID, ObjectVersion: version + 1,
			EffectiveAt: req.EffectiveAt, RecordedAt: now, Actor: actorID,
			Details: map[string]any{
				"previous_manager_employee_id": previous, "new_manager_employee_id": req.ManagerEmployeeID, "reason": req.Reason,
			},
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
			return nil, domain.ErrManagerNotFound
		}
		return nil, err
	}
	return out, nil
}

// FindUnitsByManager returns the units an employee manages as of the given
// date (empty = today), across legal entities, ordered by entity then code.
// Retired (INACTIVE) units are left out: they have nothing to manage.
func (s *PgStore) FindUnitsByManager(ctx context.Context, managerEmployeeID, asOf string) ([]domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	out := []domain.OrgUnit{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH d AS (SELECT COALESCE(NULLIF($2, '')::date, CURRENT_DATE) AS d) `+orgUnitSelect+`
			WHERE u.tenant_id = $1 AND mg.manager_employee_id = $3::uuid AND u.status <> 'INACTIVE'
			ORDER BY u.legal_entity_id ASC, u.code ASC`, tenantID, asOf, managerEmployeeID)
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
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
			return []domain.OrgUnit{}, nil // not a valid employee id, so it manages nothing
		}
		return nil, err
	}
	return out, nil
}

// HierarchySnapshot returns every unit of a legal entity that had a place in
// the hierarchy on asOf (YYYY-MM-DD, empty = today), as it was KNOWN at
// recordedAsOf (RFC 3339, empty = now), each with its parent, manager and
// cost/profit-center mapping.
//
// Two time axes: asOf is when the structure applied; recordedAsOf is how much of
// it had been written down. A placement or manager reference that was closed
// after recordedAsOf still reads as open-ended, and a unit created after it is
// absent, so a past report can be reproduced exactly as it was produced.
//
// Retired (INACTIVE) units are returned only when includeInactive is set; their
// status is the current one, because status changes are not themselves dated.
func (s *PgStore) HierarchySnapshot(ctx context.Context, legalEntityID, asOf, recordedAsOf string, includeInactive bool) ([]domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	out := []domain.OrgUnit{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH p AS (
				SELECT COALESCE(NULLIF($2, '')::date, CURRENT_DATE) AS d,
				       COALESCE(NULLIF($3, '')::timestamptz, now())  AS r
			)
			SELECT u.unit_id, u.tenant_id, u.legal_entity_id, u.code, u.name, u.unit_type,
			       u.status, u.location_ref, u.record_version, u.correlation_id,
			       u.created_by, u.created_at, u.updated_at,
			       hv.parent_unit_id, mg.manager_employee_id,
			       COALESCE(mp.cost_center_code, ''), COALESCE(mp.profit_center_code, '')
			FROM p CROSS JOIN org_units u
			JOIN org_unit_hierarchy_versions hv
			  ON hv.unit_id = u.unit_id
			 AND hv.recorded_at <= p.r
			 AND (hv.superseded_at IS NULL OR hv.superseded_at > p.r)
			 AND hv.valid_from <= p.d
			 AND (hv.valid_to IS NULL OR hv.closed_at > p.r OR hv.valid_to > p.d)
			LEFT JOIN org_unit_manager_refs mg
			  ON mg.unit_id = u.unit_id
			 AND mg.recorded_at <= p.r
			 AND mg.valid_from <= p.d
			 AND (mg.valid_to IS NULL OR mg.closed_at > p.r OR mg.valid_to > p.d)
			LEFT JOIN org_unit_dimension_mappings mp
			  ON mp.unit_id = u.unit_id
			 AND mp.recorded_at <= p.r
			 AND mp.valid_from <= p.d AND (mp.valid_to IS NULL OR mp.valid_to > p.d)
			WHERE u.tenant_id = $1 AND u.legal_entity_id = $4
			  AND u.created_at <= p.r
			  AND ($5 OR u.status <> 'INACTIVE')
			ORDER BY u.code ASC, u.unit_id ASC`,
			tenantID, asOf, recordedAsOf, legalEntityID, includeInactive)
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
