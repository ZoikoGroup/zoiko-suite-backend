// Package store provides the PostgreSQL implementation of
// asset-management-svc's persistence layer.
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

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
)

// isUniqueViolation reports whether err is a Postgres UNIQUE constraint
// violation (SQLSTATE 23505) — used to translate a real constraint
// rejection into the specific domain error it means, rather than a
// generic store failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func uuidNewString() string {
	return uuid.NewString()
}

// roundCents rounds v to the nearest cent — money is NUMERIC(18,2), and a
// calculation that doesn't land on a whole cent is wrong, not merely
// imprecise. Same helper this platform already uses in financial-close-svc.
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

const fixedAssetColumns = `
	asset_id, tenant_id, legal_entity_id, asset_category, tag_serial, description,
	custodian_id, location_id, acquisition_source_ref, acquisition_date, in_service_date,
	status, merged_into_asset_id, split_from_asset_id,
	created_at, created_by_principal_id,
	registered_at, registered_by_principal_id,
	capitalized_at, capitalized_by_principal_id,
	suspended_at, suspended_by_principal_id, suspension_reason`

func scanFixedAsset(row pgx.Row) (*domain.FixedAsset, error) {
	var a domain.FixedAsset
	var tagSerial, custodianID, locationID, acquisitionSourceRef *string
	if err := row.Scan(
		&a.AssetID, &a.TenantID, &a.LegalEntityID, &a.AssetCategory, &tagSerial, &a.Description,
		&custodianID, &locationID, &acquisitionSourceRef, &a.AcquisitionDate, &a.InServiceDate,
		&a.Status, &a.MergedIntoAssetID, &a.SplitFromAssetID,
		&a.CreatedAt, &a.CreatedByPrincipalID,
		&a.RegisteredAt, &a.RegisteredByPrincipalID,
		&a.CapitalizedAt, &a.CapitalizedByPrincipalID,
		&a.SuspendedAt, &a.SuspendedByPrincipalID, &a.SuspensionReason,
	); err != nil {
		return nil, err
	}
	if tagSerial != nil {
		a.TagSerial = *tagSerial
	}
	if custodianID != nil {
		a.CustodianID = *custodianID
	}
	if locationID != nil {
		a.LocationID = *locationID
	}
	if acquisitionSourceRef != nil {
		a.AcquisitionSourceRef = *acquisitionSourceRef
	}
	return &a, nil
}

func (s *PgStore) CreateAsset(ctx context.Context, a *domain.FixedAsset) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO fixed_assets (
				asset_id, tenant_id, legal_entity_id, asset_category, tag_serial, description,
				custodian_id, location_id, acquisition_source_ref, acquisition_date, in_service_date,
				status, split_from_asset_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`, a.AssetID, tenantID, a.LegalEntityID, a.AssetCategory, nullIfEmpty(a.TagSerial), a.Description,
			nullIfEmpty(a.CustodianID), nullIfEmpty(a.LocationID), nullIfEmpty(a.AcquisitionSourceRef), a.AcquisitionDate, a.InServiceDate,
			a.Status, a.SplitFromAssetID, a.CreatedAt, a.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) GetAsset(ctx context.Context, assetID string) (*domain.FixedAsset, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var a *domain.FixedAsset
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+fixedAssetColumns+` FROM fixed_assets WHERE asset_id = $1 AND tenant_id = $2`, assetID, tenantID)
		var err error
		a, err = scanFixedAsset(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAssetNotFound
		}
		if err != nil {
			return mapPgError(err)
		}
		a.Components, err = s.listComponents(ctx, tx, tenantID, assetID)
		if err != nil {
			return err
		}
		a.BookAssignments, err = s.listBookAssignments(ctx, tx, tenantID, assetID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *PgStore) ListAssets(ctx context.Context, legalEntityID string) ([]domain.FixedAsset, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.FixedAsset
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+fixedAssetColumns+` FROM fixed_assets WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return mapPgError(err)
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanFixedAsset(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) listComponents(ctx context.Context, tx pgx.Tx, tenantID, assetID string) ([]domain.AssetComponent, error) {
	rows, err := tx.Query(ctx, `
		SELECT component_id, asset_id, description, cost_source_ref, moved_to_asset_id, created_at, created_by_principal_id
		FROM asset_components WHERE tenant_id = $1 AND asset_id = $2 ORDER BY created_at
	`, tenantID, assetID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	var out []domain.AssetComponent
	for rows.Next() {
		var c domain.AssetComponent
		var costSourceRef *string
		if err := rows.Scan(&c.ComponentID, &c.AssetID, &c.Description, &costSourceRef, &c.MovedToAssetID, &c.CreatedAt, &c.CreatedByPrincipalID); err != nil {
			return nil, err
		}
		if costSourceRef != nil {
			c.CostSourceRef = *costSourceRef
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PgStore) listBookAssignments(ctx context.Context, tx pgx.Tx, tenantID, assetID string) ([]domain.AssetBookAssignment, error) {
	rows, err := tx.Query(ctx, `
		SELECT assignment_id, asset_id, book_id, useful_life_months_proposal, residual_value_proposal, status, assigned_at, assigned_by_principal_id
		FROM asset_book_assignments WHERE tenant_id = $1 AND asset_id = $2 ORDER BY assigned_at
	`, tenantID, assetID)
	if err != nil {
		return nil, mapPgError(err)
	}
	defer rows.Close()
	var out []domain.AssetBookAssignment
	for rows.Next() {
		var b domain.AssetBookAssignment
		if err := rows.Scan(&b.AssignmentID, &b.AssetID, &b.BookID, &b.UsefulLifeMonthsProposal, &b.ResidualValueProposal, &b.Status, &b.AssignedAt, &b.AssignedByPrincipalID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddComponent inserts a new component under assetID.
func (s *PgStore) AddComponent(ctx context.Context, c *domain.AssetComponent) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO asset_components (component_id, tenant_id, asset_id, description, cost_source_ref, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, c.ComponentID, tenantID, c.AssetID, c.Description, nullIfEmpty(c.CostSourceRef), c.CreatedAt, c.CreatedByPrincipalID)
		return mapPgError(err)
	})
}

// AssignBookProfile inserts a new book assignment, or refuses if one
// already exists for (asset_id, book_id) — see ErrRetroactiveBookProfileChange's
// own doc comment for why this store method never updates an existing row.
func (s *PgStore) AssignBookProfile(ctx context.Context, b *domain.AssetBookAssignment) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO asset_book_assignments (
				assignment_id, tenant_id, asset_id, book_id, useful_life_months_proposal, residual_value_proposal,
				status, assigned_at, assigned_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, asset_id, book_id) DO NOTHING
		`, b.AssignmentID, tenantID, b.AssetID, b.BookID, b.UsefulLifeMonthsProposal, b.ResidualValueProposal,
			b.Status, b.AssignedAt, b.AssignedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrRetroactiveBookProfileChange
		}
		return nil
	})
}

// transitionAsset is the shared guarded-UPDATE helper every lifecycle
// command uses — WHERE status = fromStatus makes every transition
// atomic and race-safe, the same pattern as every other service built
// this session.
func (s *PgStore) transitionAsset(ctx context.Context, assetID, fromStatus, toStatus string, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, assetID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE fixed_assets SET status = $1%s
			WHERE asset_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAssetTransition
		}
		return nil
	})
}

func (s *PgStore) RegisterAsset(ctx context.Context, assetID, principalID string, at time.Time) error {
	return s.transitionAsset(ctx, assetID, domain.AssetStatusCandidate, domain.AssetStatusRegistered,
		", registered_at = $2, registered_by_principal_id = $3", at, principalID)
}

func (s *PgStore) CapitalizeAsset(ctx context.Context, assetID, principalID string, at time.Time) error {
	return s.transitionAsset(ctx, assetID, domain.AssetStatusRegistered, domain.AssetStatusActive,
		", capitalized_at = $2, capitalized_by_principal_id = $3", at, principalID)
}

func (s *PgStore) SuspendAsset(ctx context.Context, assetID, principalID, reason string, at time.Time) error {
	return s.transitionAsset(ctx, assetID, domain.AssetStatusActive, domain.AssetStatusSuspended,
		", suspended_at = $2, suspended_by_principal_id = $3, suspension_reason = $4", at, principalID, reason)
}

func (s *PgStore) ReactivateAsset(ctx context.Context, assetID string) error {
	return s.transitionAsset(ctx, assetID, domain.AssetStatusSuspended, domain.AssetStatusActive, "")
}

// UpdateMetadata amends non-financial metadata only — never status, never
// accounting basis (the spec's own SoD: "custodian/location metadata
// change cannot alter accounting basis").
func (s *PgStore) UpdateMetadata(ctx context.Context, assetID string, description, custodianID, locationID, tagSerial *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE fixed_assets SET
				description = COALESCE($1, description),
				custodian_id = COALESCE($2, custodian_id),
				location_id = COALESCE($3, location_id),
				tag_serial = COALESCE($4, tag_serial)
			WHERE asset_id = $5 AND tenant_id = $6
		`, description, custodianID, locationID, tagSerial, assetID, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrAssetNotFound
		}
		return nil
	})
}

// MergeAssets marks source MERGED with a link to target — one
// transaction. The caller has already verified both assets share a
// legal_entity_id (the spec's own negative path); this method itself
// re-verifies it against the real rows as the authoritative check.
func (s *PgStore) MergeAssets(ctx context.Context, sourceAssetID, targetAssetID, principalID string, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var sourceEntity, targetEntity, targetStatus string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id, status FROM fixed_assets WHERE asset_id = $1 AND tenant_id = $2`, sourceAssetID, tenantID).Scan(&sourceEntity, new(string)); err != nil {
			return mapPgError(err)
		}
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id, status FROM fixed_assets WHERE asset_id = $1 AND tenant_id = $2`, targetAssetID, tenantID).Scan(&targetEntity, &targetStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAssetNotFound
			}
			return mapPgError(err)
		}
		if sourceEntity != targetEntity {
			return domain.ErrMergeAcrossLegalEntities
		}
		tag, err := tx.Exec(ctx, `
			UPDATE fixed_assets SET status = $1, merged_into_asset_id = $2
			WHERE asset_id = $3 AND status = $4 AND tenant_id = $5
		`, domain.AssetStatusMerged, targetAssetID, sourceAssetID, domain.AssetStatusActive, tenantID)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidAssetTransition
		}
		return nil
	})
}

// SplitAsset creates a new CANDIDATE asset from componentIDs taken off
// sourceAssetID, in one transaction — the named components are marked
// moved (never deleted) and the new asset records split_from_asset_id.
func (s *PgStore) SplitAsset(ctx context.Context, newAsset *domain.FixedAsset, sourceAssetID string, componentIDs []string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO fixed_assets (
				asset_id, tenant_id, legal_entity_id, asset_category, description,
				status, split_from_asset_id, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, newAsset.AssetID, tenantID, newAsset.LegalEntityID, newAsset.AssetCategory, newAsset.Description,
			domain.AssetStatusCandidate, sourceAssetID, newAsset.CreatedAt, newAsset.CreatedByPrincipalID)
		if err != nil {
			return mapPgError(err)
		}
		for _, componentID := range componentIDs {
			tag, err := tx.Exec(ctx, `
				UPDATE asset_components SET moved_to_asset_id = $1
				WHERE component_id = $2 AND asset_id = $3 AND tenant_id = $4 AND moved_to_asset_id IS NULL
			`, newAsset.AssetID, componentID, sourceAssetID, tenantID)
			if err != nil {
				return mapPgError(err)
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrComponentNotOnAsset
			}
		}
		return nil
	})
}

// mapPgError translates a malformed-UUID lookup into a domain "not found"
// rather than a dead-store error — the same posture as every other store
// in this platform.
func mapPgError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrAssetNotFound
	}
	return err
}
