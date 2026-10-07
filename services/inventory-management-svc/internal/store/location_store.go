package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

const locationColumns = `
	location_id, tenant_id, legal_entity_id, location_code, location_type, description, custodian_entity, status,
	created_at, created_by_principal_id,
	activated_at, activated_by_principal_id,
	suspended_at, suspended_by_principal_id, suspension_reason,
	quarantined_at, quarantined_by_principal_id, quarantine_reason,
	released_at, released_by_principal_id,
	retired_at, retired_by_principal_id, retirement_reason`

func scanLocation(row pgx.Row) (*domain.InventoryLocation, error) {
	var l domain.InventoryLocation
	var description, custodianEntity *string
	if err := row.Scan(
		&l.LocationID, &l.TenantID, &l.LegalEntityID, &l.LocationCode, &l.LocationType, &description, &custodianEntity, &l.Status,
		&l.CreatedAt, &l.CreatedByPrincipalID,
		&l.ActivatedAt, &l.ActivatedByPrincipalID,
		&l.SuspendedAt, &l.SuspendedByPrincipalID, &l.SuspensionReason,
		&l.QuarantinedAt, &l.QuarantinedByPrincipalID, &l.QuarantineReason,
		&l.ReleasedAt, &l.ReleasedByPrincipalID,
		&l.RetiredAt, &l.RetiredByPrincipalID, &l.RetirementReason,
	); err != nil {
		return nil, err
	}
	if description != nil {
		l.Description = *description
	}
	if custodianEntity != nil {
		l.CustodianEntity = *custodianEntity
	}
	return &l, nil
}

// CreateLocation inserts a new location in DRAFT and its own first
// hierarchy version, in one transaction — a location never exists without
// at least one (possibly root, parent=NULL) hierarchy row.
func (s *PgStore) CreateLocation(ctx context.Context, l *domain.InventoryLocation, parentLocationID *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_locations (
				location_id, tenant_id, legal_entity_id, location_code, location_type, description, custodian_entity,
				status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, l.LocationID, tenantID, l.LegalEntityID, l.LocationCode, l.LocationType, nullIfEmpty(l.Description), nullIfEmpty(l.CustodianEntity),
			l.Status, l.CreatedAt, l.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateLocationCode
			}
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_location_hierarchy (
				hierarchy_version_id, version, tenant_id, location_id, parent_location_id, effective_from, created_at, created_by_principal_id
			) VALUES ($1, 1, $2, $3, $4, $5, $6, $7)
		`, uuidNewString(), tenantID, l.LocationID, parentLocationID, l.CreatedAt, l.CreatedAt, l.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetLocation(ctx context.Context, locationID string) (*domain.InventoryLocation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var l *domain.InventoryLocation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM inventory_locations WHERE location_id = $1 AND tenant_id = $2`, locationID, tenantID)
		var err error
		l, err = scanLocation(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLocationNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (s *PgStore) ListLocations(ctx context.Context, legalEntityID string, eligibleOnly bool) ([]domain.InventoryLocation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.InventoryLocation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + locationColumns + ` FROM inventory_locations WHERE tenant_id = $1 AND legal_entity_id = $2`
		args := []any{tenantID, legalEntityID}
		if eligibleOnly {
			query += ` AND status = $3`
			args = append(args, domain.LocationStatusActive)
		}
		query += ` ORDER BY created_at DESC`
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLocation(rows)
			if err != nil {
				return err
			}
			out = append(out, *l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) transitionLocation(ctx context.Context, locationID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, locationID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE inventory_locations SET status = $1%s
			WHERE location_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidLocationTransition
		}
		return nil
	})
}

func (s *PgStore) ActivateLocation(ctx context.Context, locationID, principalID string, at time.Time) error {
	return s.transitionLocation(ctx, locationID, domain.LocationStatusDraft, domain.LocationStatusActive,
		", activated_at = $2, activated_by_principal_id = $3", at, principalID)
}

func (s *PgStore) SuspendLocation(ctx context.Context, locationID, principalID, reason string, at time.Time) error {
	return s.transitionLocation(ctx, locationID, domain.LocationStatusActive, domain.LocationStatusSuspended,
		", suspended_at = $2, suspended_by_principal_id = $3, suspension_reason = $4", at, principalID, reason)
}

// SetQuarantine sets QUARANTINE (from ACTIVE) when quarantine=true, or
// releases it (back to ACTIVE) when quarantine=false — one real toggle,
// matching SetQuarantineState's own name. Release additionally verifies
// releasingPrincipalID differs from whoever set it — negative path #4 —
// by reading the row's own quarantined_by_principal_id inside the same
// transaction as the guarded status UPDATE.
func (s *PgStore) SetQuarantine(ctx context.Context, locationID, principalID, reason string, quarantine bool, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if quarantine {
			tag, err := tx.Exec(ctx, `
				UPDATE inventory_locations SET status = $1, quarantined_at = $2, quarantined_by_principal_id = $3, quarantine_reason = $4
				WHERE location_id = $5 AND status = $6 AND tenant_id = $7
			`, domain.LocationStatusQuarantine, at, principalID, reason, locationID, domain.LocationStatusActive, tenantID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrInvalidLocationTransition
			}
			return nil
		}

		var quarantinedBy *string
		err := tx.QueryRow(ctx, `
			SELECT quarantined_by_principal_id FROM inventory_locations
			WHERE location_id = $1 AND status = $2 AND tenant_id = $3
		`, locationID, domain.LocationStatusQuarantine, tenantID).Scan(&quarantinedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLocationNotQuarantined
		}
		if err != nil {
			return err
		}
		if quarantinedBy != nil && *quarantinedBy == principalID {
			return domain.ErrSelfQuarantineReleaseNotPermitted
		}

		tag, err := tx.Exec(ctx, `
			UPDATE inventory_locations SET status = $1, released_at = $2, released_by_principal_id = $3
			WHERE location_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.LocationStatusActive, at, principalID, locationID, domain.LocationStatusQuarantine, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidLocationTransition
		}
		return nil
	})
}

// RetireLocation accepts any of ACTIVE/SUSPENDED/QUARANTINE as the
// starting status — the location's own "physical operational" states —
// landing in the real terminal RETIRED status no command ever leaves.
func (s *PgStore) RetireLocation(ctx context.Context, locationID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_locations SET status = $1, retired_at = $2, retired_by_principal_id = $3, retirement_reason = $4
			WHERE location_id = $5 AND status IN ($6, $7, $8) AND tenant_id = $9
		`, domain.LocationStatusRetired, at, principalID, reason,
			locationID, domain.LocationStatusActive, domain.LocationStatusSuspended, domain.LocationStatusQuarantine, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidLocationTransition
		}
		return nil
	})
}

// AmendMetadata never touches legal_entity_id or location_type — see
// migration 000002's doc comment on negative path #1.
func (s *PgStore) AmendLocationMetadata(ctx context.Context, locationID string, description, custodianEntity *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_locations SET
				description = COALESCE($1, description),
				custodian_entity = COALESCE($2, custodian_entity)
			WHERE location_id = $3 AND tenant_id = $4
		`, description, custodianEntity, locationID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrLocationNotFound
		}
		return nil
	})
}

// ── Hierarchy ────────────────────────────────────────────────────────────────

func (s *PgStore) getCurrentParent(ctx context.Context, tx pgx.Tx, tenantID, locationID string) (*string, error) {
	var parent *string
	err := tx.QueryRow(ctx, `
		SELECT parent_location_id FROM inventory_location_hierarchy
		WHERE tenant_id = $1 AND location_id = $2 AND effective_to IS NULL
	`, tenantID, locationID).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return parent, err
}

func (s *PgStore) GetCurrentParent(ctx context.Context, locationID string) (*string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var parent *string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		parent, err = s.getCurrentParent(ctx, tx, tenantID, locationID)
		return err
	})
	return parent, err
}

// GetAncestorChain walks the CURRENT hierarchy from locationID up to the
// root, returning the ordered chain of ancestor location IDs (nearest
// first). Backs both GetLocationHierarchy and ReparentLocationControlled's
// own circular-hierarchy check.
func (s *PgStore) GetAncestorChain(ctx context.Context, locationID string) ([]string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var chain []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		current := locationID
		for i := 0; i < 1000; i++ { // hard cap — a real cycle must never hang this call
			parent, err := s.getCurrentParent(ctx, tx, tenantID, current)
			if err != nil {
				return err
			}
			if parent == nil {
				return nil
			}
			chain = append(chain, *parent)
			current = *parent
		}
		return nil
	})
	return chain, err
}

// ReparentLocation creates a new current hierarchy version pointing at
// newParentLocationID, end-dating the prior one — never an in-place edit.
// The caller (handler) has already run the cross-entity and circular-
// hierarchy checks against a consistent read; this method re-verifies
// the circular check one more time inside its own transaction as the
// authoritative guard against a race.
func (s *PgStore) ReparentLocation(ctx context.Context, locationID, newParentLocationID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var newParentEntity string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM inventory_locations WHERE location_id = $1 AND tenant_id = $2`, newParentLocationID, tenantID).Scan(&newParentEntity); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrLocationNotFound
			}
			return err
		}
		var childEntity string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM inventory_locations WHERE location_id = $1 AND tenant_id = $2`, locationID, tenantID).Scan(&childEntity); err != nil {
			return err
		}
		if newParentEntity != childEntity {
			return domain.ErrReparentAcrossLegalEntities
		}

		current := newParentLocationID
		for i := 0; i < 1000; i++ {
			if current == locationID {
				return domain.ErrCircularLocationHierarchy
			}
			parent, err := s.getCurrentParent(ctx, tx, tenantID, current)
			if err != nil {
				return err
			}
			if parent == nil {
				break
			}
			current = *parent
		}

		var currentVersion int
		err := tx.QueryRow(ctx, `
			UPDATE inventory_location_hierarchy SET effective_to = $1
			WHERE tenant_id = $2 AND location_id = $3 AND effective_to IS NULL
			RETURNING version
		`, at, tenantID, locationID).Scan(&currentVersion)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_location_hierarchy (
				hierarchy_version_id, version, tenant_id, location_id, parent_location_id, effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, uuidNewString(), currentVersion+1, tenantID, locationID, newParentLocationID, at, at, principalID)
		return err
	})
}

// GetParentAsOf backs GetLocationAsOf — the real proof that hierarchy
// changes are versioned, not rewritten.
func (s *PgStore) GetParentAsOf(ctx context.Context, locationID string, at time.Time) (*string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var parent *string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT parent_location_id FROM inventory_location_hierarchy
			WHERE tenant_id = $1 AND location_id = $2 AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		`, tenantID, locationID, at)
		err := row.Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return parent, err
}
