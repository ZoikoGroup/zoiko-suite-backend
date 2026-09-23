// BIZ-07 Product & Service Catalog — bolted on beside INV-01..INV-05 in
// this same store. See migration 000006's own doc comment for the full
// design decision (why lifecycle lives on OfferingVersion, not Offering)
// and internal/domain/types.go's own BIZ-07 section for the authority
// boundary against COM-01.
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

const offeringColumns = `
	offering_id, tenant_id, legal_entity_id, sku_code, category, owner_principal_id,
	created_at, created_by_principal_id`

func scanOffering(row pgx.Row) (*domain.Offering, error) {
	var o domain.Offering
	if err := row.Scan(
		&o.OfferingID, &o.TenantID, &o.LegalEntityID, &o.SKUCode, &o.Category, &o.OwnerPrincipalID,
		&o.CreatedAt, &o.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &o, nil
}

const offeringVersionColumns = `
	version_id, tenant_id, offering_id, version_number, description, unit, availability_rules, status,
	created_at, created_by_principal_id,
	approved_at, approved_by_principal_id,
	activated_at, activated_by_principal_id,
	suspended_at, suspended_by_principal_id, suspension_reason,
	retired_at, retired_by_principal_id, retirement_reason,
	superseded_at`

func scanOfferingVersion(row pgx.Row) (*domain.OfferingVersion, error) {
	var v domain.OfferingVersion
	var availabilityRules *string
	if err := row.Scan(
		&v.VersionID, &v.TenantID, &v.OfferingID, &v.VersionNumber, &v.Description, &v.Unit, &availabilityRules, &v.Status,
		&v.CreatedAt, &v.CreatedByPrincipalID,
		&v.ApprovedAt, &v.ApprovedByPrincipalID,
		&v.ActivatedAt, &v.ActivatedByPrincipalID,
		&v.SuspendedAt, &v.SuspendedByPrincipalID, &v.SuspensionReason,
		&v.RetiredAt, &v.RetiredByPrincipalID, &v.RetirementReason,
		&v.SupersededAt,
	); err != nil {
		return nil, err
	}
	if availabilityRules != nil {
		v.AvailabilityRules = *availabilityRules
	}
	return &v, nil
}

// CreateOffering creates the offering identity and its first version
// (v1, DRAFT) in one transaction.
func (s *PgStore) CreateOffering(ctx context.Context, o *domain.Offering, v *domain.OfferingVersion, variants []domain.CatalogVariantInput) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_catalog_offerings (
				offering_id, tenant_id, legal_entity_id, sku_code, category, owner_principal_id,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, o.OfferingID, tenantID, o.LegalEntityID, o.SKUCode, o.Category, o.OwnerPrincipalID, o.CreatedAt, o.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateOfferingSKU
			}
			return err
		}
		return insertOfferingVersion(ctx, tx, tenantID, v, variants)
	})
}

func insertOfferingVersion(ctx context.Context, tx pgx.Tx, tenantID string, v *domain.OfferingVersion, variants []domain.CatalogVariantInput) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO inventory_catalog_offering_versions (
			version_id, tenant_id, offering_id, version_number, description, unit, availability_rules, status,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, v.VersionID, tenantID, v.OfferingID, v.VersionNumber, v.Description, v.Unit, nullIfEmpty(v.AvailabilityRules), v.Status,
		v.CreatedAt, v.CreatedByPrincipalID)
	if err != nil {
		return err
	}
	for _, vi := range variants {
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory_catalog_variants (variant_id, tenant_id, version_id, variant_code, variant_name)
			VALUES ($1, $2, $3, $4, $5)
		`, uuidNewString(), tenantID, v.VersionID, vi.VariantCode, vi.VariantName); err != nil {
			return err
		}
	}
	return nil
}

func (s *PgStore) GetOffering(ctx context.Context, offeringID string) (*domain.Offering, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var o *domain.Offering
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+offeringColumns+` FROM inventory_catalog_offerings WHERE offering_id = $1 AND tenant_id = $2`, offeringID, tenantID)
		var err error
		o, err = scanOffering(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOfferingNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

// GetOfferingVersion fetches one version by ID.
func (s *PgStore) GetOfferingVersion(ctx context.Context, versionID string) (*domain.OfferingVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var v *domain.OfferingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+offeringVersionColumns+` FROM inventory_catalog_offering_versions WHERE version_id = $1 AND tenant_id = $2`, versionID, tenantID)
		var err error
		v, err = scanOfferingVersion(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOfferingVersionNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// GetCurrentOfferingVersion resolves whichever version currently
// represents the offering's live state — the ACTIVE version if one
// exists, else the most recent APPROVED version, else the most recent
// DRAFT version. Backs GetOffering's own summary and ListVariants.
func (s *PgStore) GetCurrentOfferingVersion(ctx context.Context, offeringID string) (*domain.OfferingVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var v *domain.OfferingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+offeringVersionColumns+` FROM inventory_catalog_offering_versions
			WHERE tenant_id = $1 AND offering_id = $2
			ORDER BY
				CASE status WHEN 'ACTIVE' THEN 0 WHEN 'APPROVED' THEN 1 WHEN 'DRAFT' THEN 2 ELSE 3 END,
				version_number DESC
			LIMIT 1
		`, tenantID, offeringID)
		var err error
		v, err = scanOfferingVersion(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrOfferingVersionNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// CreateVersion adds a new DRAFT version on top of an existing offering
// — content is never edited in place, only superseded via a later
// Activate.
func (s *PgStore) CreateVersion(ctx context.Context, v *domain.OfferingVersion, variants []domain.CatalogVariantInput) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var offeringExists int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory_catalog_offerings WHERE offering_id = $1 AND tenant_id = $2`, v.OfferingID, tenantID).Scan(&offeringExists); err != nil {
			return err
		}
		if offeringExists == 0 {
			return domain.ErrOfferingNotFound
		}
		var maxVersion int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) FROM inventory_catalog_offering_versions WHERE tenant_id = $1 AND offering_id = $2`, tenantID, v.OfferingID).Scan(&maxVersion); err != nil {
			return err
		}
		v.VersionNumber = maxVersion + 1
		return insertOfferingVersion(ctx, tx, tenantID, v, variants)
	})
}

func (s *PgStore) transitionOfferingVersion(ctx context.Context, versionID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return execTransition(ctx, tx, "inventory_catalog_offering_versions", "version_id", versionID, tenantID, fromStatus, toStatus, extraSet, extraArgs...)
	})
}

// execTransition is the shared CAS-transition helper for BIZ-07's
// version rows — same shape as transitionItem above, generalized over
// table/id-column since this domain has one status column reused by
// several distinct commands.
func execTransition(ctx context.Context, tx pgx.Tx, table, idColumn, id, tenantID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	args := append([]any{toStatus}, extraArgs...)
	args = append(args, id, fromStatus, tenantID)
	query := fmt.Sprintf(`UPDATE %s SET status = $1%s WHERE %s = $%d AND status = $%d AND tenant_id = $%d`,
		table, extraSet, idColumn, len(args)-2, len(args)-1, len(args))
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrInvalidCatalogVersionTransition
	}
	return nil
}

// ApproveOffering — DRAFT -> APPROVED for a specific version.
func (s *PgStore) ApproveOfferingVersion(ctx context.Context, versionID, principalID string, at time.Time) error {
	return s.transitionOfferingVersion(ctx, versionID, domain.CatalogVersionStatusDraft, domain.CatalogVersionStatusApproved,
		", approved_at = $2, approved_by_principal_id = $3", at, principalID)
}

// ActivateOfferingVersion — APPROVED -> ACTIVE, and in the same
// transaction forces any other currently-ACTIVE version of the same
// offering to SUPERSEDED. Returns the version_id of whichever version
// was superseded, if any — the caller uses this to publish
// OfferingVersionSuperseded.
func (s *PgStore) ActivateOfferingVersion(ctx context.Context, offeringID, versionID, principalID string, at time.Time) (supersededVersionID *string, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, execErr := tx.Exec(ctx, `
			UPDATE inventory_catalog_offering_versions
			SET status = $1, activated_at = $2, activated_by_principal_id = $3
			WHERE version_id = $4 AND status = $5 AND tenant_id = $6
		`, domain.CatalogVersionStatusActive, at, principalID, versionID, domain.CatalogVersionStatusApproved, tenantID)
		if execErr != nil {
			return execErr
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCatalogVersionTransition
		}
		row := tx.QueryRow(ctx, `
			UPDATE inventory_catalog_offering_versions
			SET status = $1, superseded_at = $2
			WHERE offering_id = $3 AND tenant_id = $4 AND status = $5 AND version_id != $6
			RETURNING version_id
		`, domain.CatalogVersionStatusSuperseded, at, offeringID, tenantID, domain.CatalogVersionStatusActive, versionID)
		var supersededID string
		scanErr := row.Scan(&supersededID)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		supersededVersionID = &supersededID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return supersededVersionID, nil
}

func (s *PgStore) SuspendOfferingVersion(ctx context.Context, versionID, principalID, reason string, at time.Time) error {
	return s.transitionOfferingVersion(ctx, versionID, domain.CatalogVersionStatusActive, domain.CatalogVersionStatusSuspended,
		", suspended_at = $2, suspended_by_principal_id = $3, suspension_reason = $4", at, principalID, reason)
}

// RetireOfferingVersion accepts either ACTIVE or SUSPENDED as the
// originating status — the same two-predecessor shape as
// InventoryLocation's own RetireLocation.
func (s *PgStore) RetireOfferingVersion(ctx context.Context, versionID, principalID, reason string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_catalog_offering_versions
			SET status = $1, retired_at = $2, retired_by_principal_id = $3, retirement_reason = $4
			WHERE version_id = $5 AND status IN ($6, $7) AND tenant_id = $8
		`, domain.CatalogVersionStatusRetired, at, principalID, reason, versionID,
			domain.CatalogVersionStatusActive, domain.CatalogVersionStatusSuspended, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidCatalogVersionTransition
		}
		return nil
	})
}

// GetVersionAsOf resolves whichever version was ACTIVE at instant `at`
// — the real implementation of "historical transactions pin the
// offering version." Returns ErrCatalogVersionInvalid if no version was
// active at that instant.
func (s *PgStore) GetVersionAsOf(ctx context.Context, offeringID string, at time.Time) (*domain.OfferingVersion, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var v *domain.OfferingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+offeringVersionColumns+` FROM inventory_catalog_offering_versions
			WHERE tenant_id = $1 AND offering_id = $2
				AND activated_at IS NOT NULL AND activated_at <= $3
				AND (superseded_at IS NULL OR superseded_at > $3)
				AND (retired_at IS NULL OR retired_at > $3)
		`, tenantID, offeringID, at)
		var err error
		v, err = scanOfferingVersion(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCatalogVersionInvalid
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SearchCatalog lists offerings for a legal entity, optionally narrowed
// by category, together with each offering's current version status.
func (s *PgStore) SearchCatalog(ctx context.Context, legalEntityID, category string) ([]domain.Offering, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.Offering
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+offeringColumns+` FROM inventory_catalog_offerings
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR category = $3)
			ORDER BY created_at DESC
		`, tenantID, legalEntityID, category)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOffering(rows)
			if err != nil {
				return err
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListVariants returns the variants belonging to a specific version.
func (s *PgStore) ListVariants(ctx context.Context, versionID string) ([]domain.CatalogVariant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.CatalogVariant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT variant_id, version_id, variant_code, variant_name FROM inventory_catalog_variants WHERE tenant_id = $1 AND version_id = $2 ORDER BY variant_code`, tenantID, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cv domain.CatalogVariant
			if err := rows.Scan(&cv.VariantID, &cv.VersionID, &cv.VariantCode, &cv.VariantName); err != nil {
				return err
			}
			out = append(out, cv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LinkMapping attaches a tax/accounting/product mapping reference to a
// specific version — append-only, one mapping per (version, type).
func (s *PgStore) LinkMapping(ctx context.Context, m *domain.CatalogMapping) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var versionExists int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory_catalog_offering_versions WHERE version_id = $1 AND tenant_id = $2`, m.VersionID, tenantID).Scan(&versionExists); err != nil {
			return err
		}
		if versionExists == 0 {
			return domain.ErrOfferingVersionNotFound
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_catalog_mappings (mapping_id, tenant_id, version_id, mapping_type, mapping_ref, linked_at, linked_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, m.MappingID, tenantID, m.VersionID, m.MappingType, m.MappingRef, m.LinkedAt, m.LinkedByPrincipalID)
		return err
	})
}

// GetMappings returns every mapping linked to a specific version.
func (s *PgStore) GetMappings(ctx context.Context, versionID string) ([]domain.CatalogMapping, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.CatalogMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT mapping_id, version_id, mapping_type, mapping_ref, linked_at, linked_by_principal_id FROM inventory_catalog_mappings WHERE tenant_id = $1 AND version_id = $2 ORDER BY mapping_type`, tenantID, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.CatalogMapping
			if err := rows.Scan(&m.MappingID, &m.VersionID, &m.MappingType, &m.MappingRef, &m.LinkedAt, &m.LinkedByPrincipalID); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
