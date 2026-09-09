// Package store provides the PostgreSQL implementation of
// inventory-management-svc's persistence layer.
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

	"zoiko.io/inventory-management-svc/internal/domain"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
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
// financial-close-svc/asset-management-svc.
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

const itemColumns = `
	item_id, tenant_id, legal_entity_id, sku, description, base_uom, item_type,
	physical_characteristics, catalog_item_id, status,
	created_at, created_by_principal_id,
	activated_at, activated_by_principal_id,
	retired_at, retired_by_principal_id, retirement_reason`

func scanItem(row pgx.Row) (*domain.InventoryItem, error) {
	var it domain.InventoryItem
	var itemType, physicalCharacteristics *string
	if err := row.Scan(
		&it.ItemID, &it.TenantID, &it.LegalEntityID, &it.SKU, &it.Description, &it.BaseUOM, &itemType,
		&physicalCharacteristics, &it.CatalogItemID, &it.Status,
		&it.CreatedAt, &it.CreatedByPrincipalID,
		&it.ActivatedAt, &it.ActivatedByPrincipalID,
		&it.RetiredAt, &it.RetiredByPrincipalID, &it.RetirementReason,
	); err != nil {
		return nil, err
	}
	if itemType != nil {
		it.ItemType = *itemType
	}
	if physicalCharacteristics != nil {
		it.PhysicalCharacteristics = *physicalCharacteristics
	}
	return &it, nil
}

func (s *PgStore) CreateItem(ctx context.Context, it *domain.InventoryItem) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inventory_items (
				item_id, tenant_id, legal_entity_id, sku, description, base_uom, item_type,
				physical_characteristics, status, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, it.ItemID, tenantID, it.LegalEntityID, it.SKU, it.Description, it.BaseUOM, nullIfEmpty(it.ItemType),
			nullIfEmpty(it.PhysicalCharacteristics), it.Status, it.CreatedAt, it.CreatedByPrincipalID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.ErrDuplicateSKU
			}
			return err
		}
		return nil
	})
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) GetItem(ctx context.Context, itemID string) (*domain.InventoryItem, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var it *domain.InventoryItem
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+itemColumns+` FROM inventory_items WHERE item_id = $1 AND tenant_id = $2`, itemID, tenantID)
		var err error
		it, err = scanItem(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrItemNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return it, nil
}

func (s *PgStore) ListItems(ctx context.Context, legalEntityID string) ([]domain.InventoryItem, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out []domain.InventoryItem
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+itemColumns+` FROM inventory_items WHERE tenant_id = $1 AND legal_entity_id = $2 ORDER BY created_at DESC`, tenantID, legalEntityID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) transitionItem(ctx context.Context, itemID, fromStatus, toStatus, extraSet string, extraArgs ...any) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		args := append([]any{toStatus}, extraArgs...)
		args = append(args, itemID, fromStatus, tenantID)
		query := fmt.Sprintf(`
			UPDATE inventory_items SET status = $1%s
			WHERE item_id = $%d AND status = $%d AND tenant_id = $%d
		`, extraSet, len(args)-2, len(args)-1, len(args))
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInvalidItemTransition
		}
		return nil
	})
}

func (s *PgStore) ActivateItem(ctx context.Context, itemID, principalID string, at time.Time) error {
	return s.transitionItem(ctx, itemID, domain.ItemStatusDraft, domain.ItemStatusActive,
		", activated_at = $2, activated_by_principal_id = $3", at, principalID)
}

func (s *PgStore) RetireItem(ctx context.Context, itemID, principalID, reason string, at time.Time) error {
	return s.transitionItem(ctx, itemID, domain.ItemStatusActive, domain.ItemStatusRetired,
		", retired_at = $2, retired_by_principal_id = $3, retirement_reason = $4", at, principalID, reason)
}

// AmendProfile never touches base_uom or sku — see migration 000001's doc
// comment on negative path #2.
func (s *PgStore) AmendProfile(ctx context.Context, itemID string, description, itemType, physicalCharacteristics *string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE inventory_items SET
				description = COALESCE($1, description),
				item_type = COALESCE($2, item_type),
				physical_characteristics = COALESCE($3, physical_characteristics)
			WHERE item_id = $4 AND tenant_id = $5
		`, description, itemType, physicalCharacteristics, itemID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrItemNotFound
		}
		return nil
	})
}

func (s *PgStore) LinkCatalogItem(ctx context.Context, itemID, catalogItemID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE inventory_items SET catalog_item_id = $1 WHERE item_id = $2 AND tenant_id = $3`, catalogItemID, itemID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrItemNotFound
		}
		return nil
	})
}

// ── Tracking policy ──────────────────────────────────────────────────────────

const trackingPolicyColumns = `
	policy_version_id, policy_id, version, item_id,
	requires_lot_tracking, requires_serial_tracking, requires_expiry_tracking,
	effective_from, effective_to, created_at, created_by_principal_id`

func scanTrackingPolicy(row pgx.Row) (*domain.TrackingPolicy, error) {
	var p domain.TrackingPolicy
	if err := row.Scan(
		&p.PolicyVersionID, &p.PolicyID, &p.Version, &p.ItemID,
		&p.RequiresLotTracking, &p.RequiresSerialTracking, &p.RequiresExpiryTracking,
		&p.EffectiveFrom, &p.EffectiveTo, &p.CreatedAt, &p.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetTrackingPolicy end-dates the current version (if any) and inserts a
// new one — never an in-place edit, the same versioned-entity pattern as
// SetValuationPolicy below.
func (s *PgStore) SetTrackingPolicy(ctx context.Context, p *domain.TrackingPolicy, at time.Time) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var currentVersion int
		var policyID string
		err := tx.QueryRow(ctx, `
			UPDATE inventory_tracking_policies SET effective_to = $1
			WHERE tenant_id = $2 AND item_id = $3 AND effective_to IS NULL
			RETURNING policy_id, version
		`, at, tenantID, p.ItemID).Scan(&policyID, &currentVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if policyID != "" {
			p.PolicyID = policyID
			p.Version = currentVersion + 1
		} else {
			p.PolicyID = uuidNewString()
			p.Version = 1
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_tracking_policies (
				policy_version_id, policy_id, version, tenant_id, item_id,
				requires_lot_tracking, requires_serial_tracking, requires_expiry_tracking,
				effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, p.PolicyVersionID, p.PolicyID, p.Version, tenantID, p.ItemID,
			p.RequiresLotTracking, p.RequiresSerialTracking, p.RequiresExpiryTracking,
			p.EffectiveFrom, p.CreatedAt, p.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetCurrentTrackingPolicy(ctx context.Context, itemID string) (*domain.TrackingPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var p *domain.TrackingPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+trackingPolicyColumns+` FROM inventory_tracking_policies WHERE tenant_id = $1 AND item_id = $2 AND effective_to IS NULL`, tenantID, itemID)
		var err error
		p, err = scanTrackingPolicy(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no policy set yet — a real, valid state, not an error
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ── Valuation policy ──────────────────────────────────────────────────────────

const valuationPolicyColumns = `
	policy_version_id, policy_id, version, item_id, valuation_method,
	effective_from, effective_to, created_at, created_by_principal_id`

func scanValuationPolicy(row pgx.Row) (*domain.ValuationPolicy, error) {
	var p domain.ValuationPolicy
	if err := row.Scan(
		&p.PolicyVersionID, &p.PolicyID, &p.Version, &p.ItemID, &p.ValuationMethod,
		&p.EffectiveFrom, &p.EffectiveTo, &p.CreatedAt, &p.CreatedByPrincipalID,
	); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetValuationPolicy end-dates the current version's effective window at
// the NEW version's own effective_from (never "now") and inserts the new
// version — the current version keeps governing every movement up until
// the future date the new one takes over, so nothing already recorded is
// reinterpreted.
func (s *PgStore) SetValuationPolicy(ctx context.Context, p *domain.ValuationPolicy) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var currentVersion int
		var policyID string
		err := tx.QueryRow(ctx, `
			UPDATE inventory_valuation_policies SET effective_to = $1
			WHERE tenant_id = $2 AND item_id = $3 AND effective_to IS NULL
			RETURNING policy_id, version
		`, p.EffectiveFrom, tenantID, p.ItemID).Scan(&policyID, &currentVersion)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if policyID != "" {
			p.PolicyID = policyID
			p.Version = currentVersion + 1
		} else {
			p.PolicyID = uuidNewString()
			p.Version = 1
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inventory_valuation_policies (
				policy_version_id, policy_id, version, tenant_id, item_id, valuation_method,
				effective_from, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, p.PolicyVersionID, p.PolicyID, p.Version, tenantID, p.ItemID, p.ValuationMethod,
			p.EffectiveFrom, p.CreatedAt, p.CreatedByPrincipalID)
		return err
	})
}

func (s *PgStore) GetCurrentValuationPolicy(ctx context.Context, itemID string) (*domain.ValuationPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var p *domain.ValuationPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+valuationPolicyColumns+` FROM inventory_valuation_policies
			WHERE tenant_id = $1 AND item_id = $2 AND effective_to IS NULL
			ORDER BY effective_from DESC LIMIT 1
		`, tenantID, itemID)
		var err error
		p, err = scanValuationPolicy(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetProfileAsOf backs the spec's own GetInventoryProfileAsOf query —
// whichever tracking/valuation policy version actually governed the item
// at instant `at`, proving the versioning in migration 000001 genuinely
// preserves history (negative path #2).
func (s *PgStore) GetProfileAsOf(ctx context.Context, itemID string, at time.Time) (*domain.TrackingPolicy, *domain.ValuationPolicy, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil, domain.ErrIdentityMissing
	}
	var tp *domain.TrackingPolicy
	var vp *domain.ValuationPolicy
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		trow := tx.QueryRow(ctx, `
			SELECT `+trackingPolicyColumns+` FROM inventory_tracking_policies
			WHERE tenant_id = $1 AND item_id = $2 AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		`, tenantID, itemID, at)
		var err error
		tp, err = scanTrackingPolicy(trow)
		if errors.Is(err, pgx.ErrNoRows) {
			tp, err = nil, nil
		}
		if err != nil {
			return err
		}

		vrow := tx.QueryRow(ctx, `
			SELECT `+valuationPolicyColumns+` FROM inventory_valuation_policies
			WHERE tenant_id = $1 AND item_id = $2 AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		`, tenantID, itemID, at)
		vp, err = scanValuationPolicy(vrow)
		if errors.Is(err, pgx.ErrNoRows) {
			vp, err = nil, nil
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return tp, vp, nil
}
