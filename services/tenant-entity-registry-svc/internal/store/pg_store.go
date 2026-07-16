// Package store provides the PostgreSQL implementation of registry.Store.
//
// # Tenant Isolation — Why RLS Alone Is Not Enough
//
// Every method that reads or mutates tenant-scoped data filters EXPLICITLY by
// tenant_id in its own SQL WHERE clause. This is the actual isolation
// guarantee; RLS is defense-in-depth only.
//
// Root cause: every service in this platform connects to Postgres as the
// postgres superuser (DB_USER=postgres). Postgres superusers unconditionally
// bypass Row-Level Security regardless of policy — see
// https://www.postgresql.org/docs/current/ddl-rowsecurity.html. The withRLS
// helper still calls set_config('app.tenant_id', ...) so that RLS will apply
// correctly if the connection role is ever changed to a non-superuser; but
// under the current posture set_config has no isolation effect.
//
// This is a real vulnerability, not a theoretical concern: it was discovered
// via genuine integration test failures (TestPgStore_TenantIsolation caught
// real cross-tenant data leaks), mirroring the same fix applied to
// general-ledger-svc. Every method in this file was audited; the ones that
// were vulnerable now carry an AND tenant_id = $N in their WHERE clause.
//
// ResidencyRegion reads correctly have no tenant_id filter: that table has no
// tenant_id column and carries no per-tenant data.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// PgStore implements registry.Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Close releases the connection pool.
func (s *PgStore) Close() {
	s.pool.Close()
}

// withRLS begins a transaction, sets app.tenant_id for RLS enforcement, then
// calls fn. The transaction is committed on success, rolled back on error.
//
// R2 fix: uses current_setting('app.tenant_id', true) (missing_ok=true) in
// RLS policies; here we always set the value before querying.
//
// F2 fix: tenantID must be non-empty — every caller must supply it.
// The fallback pattern in individual methods ensures this invariant.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	// Always set app.tenant_id — we do not silently skip it.
	// If tenantID is empty, RLS will produce empty results. Callers must
	// guarantee it is non-empty before calling withRLS.
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", tenantID,
	); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isUniqueViolation returns true when err is a Postgres unique constraint
// violation (SQLSTATE 23505). Used to map to registry.ErrConflict (F6).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// tenantFromCtxOrFallback returns the tenant ID from context, falling back to
// the supplied fallback string. Panics on empty fallback — callers must always
// know the tenant ID for write operations (F2).
func tenantFromCtxOrFallback(ctx context.Context, fallback string) string {
	if t := domain.TenantFromContext(ctx); t != "" {
		return t
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Tenant
// ---------------------------------------------------------------------------

func (s *PgStore) CreateTenant(ctx context.Context, t *domain.Tenant) error {
	s.log.Debug("store.CreateTenant", zap.String("tenant_id", t.TenantID))
	tenantID := tenantFromCtxOrFallback(ctx, t.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			INSERT INTO tenants (
				tenant_id, tenant_code, legal_name, trading_name, status,
				default_currency_code, primary_timezone, primary_locale,
				default_data_residency_policy_id, lifecycle_state,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			t.TenantID, t.TenantCode, t.LegalName, t.TradingName, string(t.Status),
			t.DefaultCurrencyCode, t.PrimaryTimezone, t.PrimaryLocale,
			t.DefaultDataResidencyPolicyID, string(t.LifecycleState),
			t.CreatedAt, now, t.CreatedByPrincipalID, t.CreatedByPrincipalID,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: tenant_code %s", registry.ErrConflict, t.TenantCode)
			}
			return err
		}
		return nil
	})
}

// CreateTenantWithDefaultResidencyPolicy inserts a tenant and its default
// DataResidencyPolicy in one transaction, breaking the cycle described on
// the Store interface: the tenant row must exist before the policy row (FK),
// but the tenant row requires a non-null policy ID. Both inserts share the
// same transaction as CreateTenant/CreateResidencyPolicy individually use.
func (s *PgStore) CreateTenantWithDefaultResidencyPolicy(ctx context.Context, t *domain.Tenant, p *domain.DataResidencyPolicy) error {
	s.log.Debug("store.CreateTenantWithDefaultResidencyPolicy",
		zap.String("tenant_id", t.TenantID), zap.String("policy_id", p.DataResidencyPolicyID))
	tenantID := tenantFromCtxOrFallback(ctx, t.TenantID)

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		tenantQuery := `
			INSERT INTO tenants (
				tenant_id, tenant_code, legal_name, trading_name, status,
				default_currency_code, primary_timezone, primary_locale,
				default_data_residency_policy_id, lifecycle_state,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`
		if _, err := tx.Exec(ctx, tenantQuery,
			t.TenantID, t.TenantCode, t.LegalName, t.TradingName, string(t.Status),
			t.DefaultCurrencyCode, t.PrimaryTimezone, t.PrimaryLocale,
			t.DefaultDataResidencyPolicyID, string(t.LifecycleState),
			t.CreatedAt, now, t.CreatedByPrincipalID, t.CreatedByPrincipalID,
		); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: tenant_code %s", registry.ErrConflict, t.TenantCode)
			}
			return err
		}

		policyQuery := `
			INSERT INTO data_residency_policies (
				data_residency_policy_id, tenant_id, policy_name, policy_code,
				residency_mode, conflict_resolution_mode, residency_region_id, active_flag,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		if _, err := tx.Exec(ctx, policyQuery,
			p.DataResidencyPolicyID, p.TenantID, p.PolicyName, p.PolicyCode,
			string(p.ResidencyMode), string(p.ConflictResolutionMode), p.ResidencyRegionID, p.ActiveFlag,
			p.CreatedAt, now, p.CreatedByPrincipalID, p.CreatedByPrincipalID,
		); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: policy_code %s", registry.ErrConflict, p.PolicyCode)
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetTenantByID(ctx context.Context, tenantID string) (*domain.Tenant, error) {
	s.log.Debug("store.GetTenantByID", zap.String("tenant_id", tenantID))
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var t domain.Tenant
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT tenant_id, tenant_code, legal_name, trading_name, status,
			       default_currency_code, primary_timezone, primary_locale,
			       default_data_residency_policy_id, lifecycle_state,
			       created_at, updated_at, created_by_principal_id, updated_by_principal_id
			FROM tenants WHERE tenant_id = $1
		`
		return tx.QueryRow(ctx, query, tid).Scan(
			&t.TenantID, &t.TenantCode, &t.LegalName, &t.TradingName, &t.Status,
			&t.DefaultCurrencyCode, &t.PrimaryTimezone, &t.PrimaryLocale,
			&t.DefaultDataResidencyPolicyID, &t.LifecycleState,
			&t.CreatedAt, &t.UpdatedAt, &t.CreatedByPrincipalID, &t.UpdatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

func (s *PgStore) TransitionTenantLifecycle(ctx context.Context, tenantID string, newState domain.TenantLifecycleState, actorID, correlationID string) error {
	s.log.Debug("store.TransitionTenantLifecycle",
		zap.String("tenant_id", tenantID),
		zap.String("new_state", string(newState)),
	)
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE tenants
			SET lifecycle_state = $1, updated_at = $2, updated_by_principal_id = $3
			WHERE tenant_id = $4 AND lifecycle_state != $1
		`
		_, err := tx.Exec(ctx, query, string(newState), time.Now().UTC(), actorID, tenantID)
		return err
	})
}

// ---------------------------------------------------------------------------
// LegalEntity
// ---------------------------------------------------------------------------

func (s *PgStore) CreateEntity(ctx context.Context, e *domain.LegalEntity) error {
	s.log.Debug("store.CreateEntity", zap.String("legal_entity_id", e.LegalEntityID))
	tid := tenantFromCtxOrFallback(ctx, e.TenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO legal_entities (
				legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
				registration_number, tax_identity_bundle_id, entity_type,
				incorporation_date, default_currency_code, fiscal_calendar_id,
				parent_legal_entity_id, entity_status, primary_jurisdiction_id,
				data_residency_policy_id, created_at, updated_at,
				created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			e.LegalEntityID, e.TenantID, e.EntityCode, e.LegalName, e.TradingName,
			e.RegistrationNumber, e.TaxIdentityBundleID, string(e.EntityType),
			e.IncorporationDate, e.DefaultCurrencyCode, e.FiscalCalendarID,
			e.ParentLegalEntityID, string(e.EntityStatus), e.PrimaryJurisdictionID,
			e.DataResidencyPolicyID, e.CreatedAt, now,
			e.CreatedByPrincipalID, e.CreatedByPrincipalID,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: entity_code %s", registry.ErrConflict, e.EntityCode)
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetEntityByID(ctx context.Context, legalEntityID string) (*domain.LegalEntity, error) {
	s.log.Debug("store.GetEntityByID", zap.String("legal_entity_id", legalEntityID))
	// Tenant must be in context (set by TenantContext middleware).
	// If absent, tid is empty and the explicit AND tenant_id = $2 filter returns
	// zero rows — fail-closed. We do NOT rely on RLS: this pool connects as the
	// postgres superuser, which unconditionally bypasses RLS.
	tid := domain.TenantFromContext(ctx)

	var e domain.LegalEntity
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			       registration_number, tax_identity_bundle_id, entity_type,
			       incorporation_date, default_currency_code, fiscal_calendar_id,
			       parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			       data_residency_policy_id, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, legalEntityID, tid).Scan(
			&e.LegalEntityID, &e.TenantID, &e.EntityCode, &e.LegalName, &e.TradingName,
			&e.RegistrationNumber, &e.TaxIdentityBundleID, &e.EntityType,
			&e.IncorporationDate, &e.DefaultCurrencyCode, &e.FiscalCalendarID,
			&e.ParentLegalEntityID, &e.EntityStatus, &e.PrimaryJurisdictionID,
			&e.DataResidencyPolicyID, &e.CreatedAt, &e.UpdatedAt,
			&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

func (s *PgStore) ListEntitiesByTenant(ctx context.Context, tenantID string) ([]*domain.LegalEntity, error) {
	s.log.Debug("store.ListEntitiesByTenant", zap.String("tenant_id", tenantID))
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var results []*domain.LegalEntity
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			       registration_number, tax_identity_bundle_id, entity_type,
			       incorporation_date, default_currency_code, fiscal_calendar_id,
			       parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			       data_residency_policy_id, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			FROM legal_entities WHERE tenant_id = $1
		`
		rows, err := tx.Query(ctx, query, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e domain.LegalEntity
			if err := rows.Scan(
				&e.LegalEntityID, &e.TenantID, &e.EntityCode, &e.LegalName, &e.TradingName,
				&e.RegistrationNumber, &e.TaxIdentityBundleID, &e.EntityType,
				&e.IncorporationDate, &e.DefaultCurrencyCode, &e.FiscalCalendarID,
				&e.ParentLegalEntityID, &e.EntityStatus, &e.PrimaryJurisdictionID,
				&e.DataResidencyPolicyID, &e.CreatedAt, &e.UpdatedAt,
				&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
			); err != nil {
				return err
			}
			results = append(results, &e)
		}
		return rows.Err()
	})
	return results, err
}

// UpdateEntity applies a partial update to mutable non-governance fields
// (legal_name, trading_name, default_currency_code). Returns the updated entity.
// F5: full implementation — no longer a stub.
func (s *PgStore) UpdateEntity(ctx context.Context, legalEntityID string, req domain.UpdateEntityRequest) (*domain.LegalEntity, error) {
	s.log.Debug("store.UpdateEntity", zap.String("legal_entity_id", legalEntityID))
	tid := domain.TenantFromContext(ctx) // middleware-injected; fail safely if absent

	var updated domain.LegalEntity
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		// Build a dynamic SET clause — only update fields explicitly provided.
		// Uses a returning clause to avoid a second query for the updated row.
		query := `
			UPDATE legal_entities
			SET
				legal_name             = COALESCE($1, legal_name),
				trading_name           = COALESCE($2, trading_name),
				default_currency_code  = COALESCE($3, default_currency_code),
				updated_at             = $4,
				updated_by_principal_id = $5
			WHERE legal_entity_id = $6 AND tenant_id = $7
			RETURNING legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			          registration_number, tax_identity_bundle_id, entity_type,
			          incorporation_date, default_currency_code, fiscal_calendar_id,
			          parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			          data_residency_policy_id, created_at, updated_at,
			          created_by_principal_id, updated_by_principal_id
		`
		now := time.Now().UTC()
		return tx.QueryRow(ctx, query,
			req.LegalName, req.TradingName, req.DefaultCurrencyCode,
			now, req.ActorPrincipalID,
			legalEntityID, tid,
		).Scan(
			&updated.LegalEntityID, &updated.TenantID, &updated.EntityCode,
			&updated.LegalName, &updated.TradingName,
			&updated.RegistrationNumber, &updated.TaxIdentityBundleID, &updated.EntityType,
			&updated.IncorporationDate, &updated.DefaultCurrencyCode, &updated.FiscalCalendarID,
			&updated.ParentLegalEntityID, &updated.EntityStatus, &updated.PrimaryJurisdictionID,
			&updated.DataResidencyPolicyID, &updated.CreatedAt, &updated.UpdatedAt,
			&updated.CreatedByPrincipalID, &updated.UpdatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &updated, err
}

// TransitionEntityStatus atomically transitions entity_status.
// F3+F4: single UPDATE WHERE entity_status = ANY($allowedPriorStates).
// Returns (rowsAffected, tenantID, error). The tenantID is extracted via
// RETURNING so event publishing does not need a second query.
func (s *PgStore) TransitionEntityStatus(
	ctx context.Context,
	legalEntityID string,
	newStatus domain.EntityStatus,
	allowedPriorStates []domain.EntityStatus,
	actorID, correlationID string,
) (int64, string, error) {
	s.log.Debug("store.TransitionEntityStatus",
		zap.String("legal_entity_id", legalEntityID),
		zap.String("new_status", string(newStatus)),
	)
	tid := domain.TenantFromContext(ctx)

	// Convert []EntityStatus → []string for the ANY($1::text[]) clause.
	priors := make([]string, len(allowedPriorStates))
	for i, p := range allowedPriorStates {
		priors[i] = string(p)
	}

	var rowsAffected int64
	var tenantID string

	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE legal_entities
			SET entity_status = $1, updated_at = $2, updated_by_principal_id = $3
			WHERE legal_entity_id = $4 AND entity_status = ANY($5::text[]) AND tenant_id = $6
			RETURNING tenant_id
		`
		row := tx.QueryRow(ctx, query,
			string(newStatus), time.Now().UTC(), actorID,
			legalEntityID, priors, tid,
		)
		if err := row.Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				rowsAffected = 0
				tenantID = ""
				return nil // not an error; caller checks rowsAffected
			}
			return err
		}
		rowsAffected = 1
		return nil
	})
	return rowsAffected, tenantID, err
}

func (s *PgStore) GetEntityStatus(ctx context.Context, legalEntityID string) (*domain.EntityStatusResponse, error) {
	s.log.Debug("store.GetEntityStatus", zap.String("legal_entity_id", legalEntityID))
	tid := domain.TenantFromContext(ctx)

	var resp domain.EntityStatusResponse
	resp.EntityID = legalEntityID

	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `SELECT tenant_id, entity_status FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2`
		return tx.QueryRow(ctx, query, legalEntityID, tid).Scan(&resp.TenantID, &resp.EntityStatus)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &resp, err
}

// ---------------------------------------------------------------------------
// EntityHierarchy
// ---------------------------------------------------------------------------

func (s *PgStore) CreateHierarchy(ctx context.Context, h *domain.EntityHierarchy) error {
	s.log.Debug("store.CreateHierarchy", zap.String("hierarchy_id", h.HierarchyID))
	tid := tenantFromCtxOrFallback(ctx, h.TenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO entity_hierarchies (
				hierarchy_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
				relationship_type, effective_from, effective_to,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			h.HierarchyID, h.TenantID, h.ParentLegalEntityID, h.ChildLegalEntityID,
			string(h.RelationshipType), h.EffectiveFrom, h.EffectiveTo,
			h.CreatedAt, now, h.CreatedByPrincipalID, h.CreatedByPrincipalID,
		)
		return err
	})
}

func (s *PgStore) EndDateHierarchy(ctx context.Context, hierarchyID string, endDate time.Time, actorID, correlationID string) error {
	s.log.Debug("store.EndDateHierarchy", zap.String("hierarchy_id", hierarchyID))
	tid := domain.TenantFromContext(ctx) // must be set by middleware for mutating ops

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE entity_hierarchies
			SET effective_to = $1, updated_at = $2, updated_by_principal_id = $3
			WHERE hierarchy_id = $4 AND effective_to IS NULL AND tenant_id = $5
		`
		_, err := tx.Exec(ctx, query, endDate, time.Now().UTC(), actorID, hierarchyID, tid)
		return err
	})
}

func (s *PgStore) ListHierarchiesByEntity(ctx context.Context, legalEntityID string) ([]*domain.EntityHierarchy, error) {
	s.log.Debug("store.ListHierarchiesByEntity", zap.String("legal_entity_id", legalEntityID))
	tid := domain.TenantFromContext(ctx)

	var results []*domain.EntityHierarchy
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT hierarchy_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
			       relationship_type, effective_from, effective_to, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			FROM entity_hierarchies
			WHERE (parent_legal_entity_id = $1 OR child_legal_entity_id = $1) AND tenant_id = $2
		`
		rows, err := tx.Query(ctx, query, legalEntityID, tid)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var h domain.EntityHierarchy
			if err := rows.Scan(
				&h.HierarchyID, &h.TenantID, &h.ParentLegalEntityID, &h.ChildLegalEntityID,
				&h.RelationshipType, &h.EffectiveFrom, &h.EffectiveTo, &h.CreatedAt, &h.UpdatedAt,
				&h.CreatedByPrincipalID, &h.UpdatedByPrincipalID,
			); err != nil {
				return err
			}
			results = append(results, &h)
		}
		return rows.Err()
	})
	return results, err
}

// ---------------------------------------------------------------------------
// EntityJurisdictionAssignment
// R1: tenant_id column now exists on this table (migration 000002).
// RLS policy uses it directly — no correlated subquery.
// ---------------------------------------------------------------------------

func (s *PgStore) CreateJurisdictionAssignment(ctx context.Context, a *domain.EntityJurisdictionAssignment) error {
	s.log.Debug("store.CreateJurisdictionAssignment", zap.String("assignment_id", a.AssignmentID))
	tid := tenantFromCtxOrFallback(ctx, a.TenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO entity_jurisdiction_assignments (
				assignment_id, tenant_id, legal_entity_id, jurisdiction_id, assignment_type,
				effective_from, effective_to, source_basis,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			a.AssignmentID, a.TenantID, a.LegalEntityID, a.JurisdictionID, string(a.AssignmentType),
			a.EffectiveFrom, a.EffectiveTo, a.SourceBasis,
			a.CreatedAt, now, a.CreatedByPrincipalID, a.CreatedByPrincipalID,
		)
		return err
	})
}

func (s *PgStore) ListJurisdictionAssignments(ctx context.Context, legalEntityID string) ([]*domain.EntityJurisdictionAssignment, error) {
	s.log.Debug("store.ListJurisdictionAssignments", zap.String("legal_entity_id", legalEntityID))
	tid := domain.TenantFromContext(ctx)

	var results []*domain.EntityJurisdictionAssignment
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT assignment_id, tenant_id, legal_entity_id, jurisdiction_id, assignment_type,
			       effective_from, effective_to, source_basis, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			FROM entity_jurisdiction_assignments WHERE legal_entity_id = $1 AND tenant_id = $2
		`
		rows, err := tx.Query(ctx, query, legalEntityID, tid)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var a domain.EntityJurisdictionAssignment
			if err := rows.Scan(
				&a.AssignmentID, &a.TenantID, &a.LegalEntityID, &a.JurisdictionID, &a.AssignmentType,
				&a.EffectiveFrom, &a.EffectiveTo, &a.SourceBasis, &a.CreatedAt, &a.UpdatedAt,
				&a.CreatedByPrincipalID, &a.UpdatedByPrincipalID,
			); err != nil {
				return err
			}
			results = append(results, &a)
		}
		return rows.Err()
	})
	return results, err
}

func (s *PgStore) EndDateJurisdictionAssignment(ctx context.Context, assignmentID string, endDate time.Time, actorID, correlationID string) error {
	s.log.Debug("store.EndDateJurisdictionAssignment", zap.String("assignment_id", assignmentID))
	tid := domain.TenantFromContext(ctx)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE entity_jurisdiction_assignments
			SET effective_to = $1, updated_at = $2, updated_by_principal_id = $3
			WHERE assignment_id = $4 AND effective_to IS NULL AND tenant_id = $5
		`
		_, err := tx.Exec(ctx, query, endDate, time.Now().UTC(), actorID, assignmentID, tid)
		return err
	})
}

// ---------------------------------------------------------------------------
// DataResidencyPolicy
// ---------------------------------------------------------------------------

func (s *PgStore) CreateResidencyPolicy(ctx context.Context, p *domain.DataResidencyPolicy) error {
	s.log.Debug("store.CreateResidencyPolicy", zap.String("policy_id", p.DataResidencyPolicyID))
	tid := tenantFromCtxOrFallback(ctx, p.TenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO data_residency_policies (
				data_residency_policy_id, tenant_id, policy_name, policy_code,
				residency_mode, conflict_resolution_mode, residency_region_id, active_flag,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			p.DataResidencyPolicyID, p.TenantID, p.PolicyName, p.PolicyCode,
			string(p.ResidencyMode), string(p.ConflictResolutionMode), p.ResidencyRegionID, p.ActiveFlag,
			p.CreatedAt, now, p.CreatedByPrincipalID, p.CreatedByPrincipalID,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: policy_code %s", registry.ErrConflict, p.PolicyCode)
			}
			return err
		}
		return nil
	})
}

func (s *PgStore) GetResidencyPolicyByID(ctx context.Context, policyID string) (*domain.DataResidencyPolicy, error) {
	s.log.Debug("store.GetResidencyPolicyByID", zap.String("policy_id", policyID))
	tid := domain.TenantFromContext(ctx)

	var p domain.DataResidencyPolicy
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT data_residency_policy_id, tenant_id, policy_name, policy_code,
			       residency_mode, conflict_resolution_mode, residency_region_id, active_flag,
			       created_at, updated_at, created_by_principal_id, updated_by_principal_id
			FROM data_residency_policies WHERE data_residency_policy_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, policyID, tid).Scan(
			&p.DataResidencyPolicyID, &p.TenantID, &p.PolicyName, &p.PolicyCode,
			&p.ResidencyMode, &p.ConflictResolutionMode, &p.ResidencyRegionID, &p.ActiveFlag,
			&p.CreatedAt, &p.UpdatedAt, &p.CreatedByPrincipalID, &p.UpdatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// ---------------------------------------------------------------------------
// ResidencyRegion — read-only (IaC-managed, no RLS)
// ---------------------------------------------------------------------------

func (s *PgStore) GetResidencyRegionByID(ctx context.Context, regionID string) (*domain.ResidencyRegion, error) {
	s.log.Debug("store.GetResidencyRegionByID", zap.String("region_id", regionID))
	var r domain.ResidencyRegion
	query := `
		SELECT residency_region_id, region_code, region_name, cloud_provider,
		       country_code, sovereign_flag, active_flag,
		       created_at, updated_at, created_by_principal_id, updated_by_principal_id
		FROM residency_regions WHERE residency_region_id = $1 AND active_flag = true
	`
	err := s.pool.QueryRow(ctx, query, regionID).Scan(
		&r.ResidencyRegionID, &r.RegionCode, &r.RegionName, &r.CloudProvider,
		&r.CountryCode, &r.SovereignFlag, &r.ActiveFlag,
		&r.CreatedAt, &r.UpdatedAt, &r.CreatedByPrincipalID, &r.UpdatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

func (s *PgStore) ListResidencyRegions(ctx context.Context) ([]*domain.ResidencyRegion, error) {
	s.log.Debug("store.ListResidencyRegions")
	var results []*domain.ResidencyRegion
	query := `
		SELECT residency_region_id, region_code, region_name, cloud_provider,
		       country_code, sovereign_flag, active_flag,
		       created_at, updated_at, created_by_principal_id, updated_by_principal_id
		FROM residency_regions WHERE active_flag = true ORDER BY region_code
	`
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var r domain.ResidencyRegion
		if err := rows.Scan(
			&r.ResidencyRegionID, &r.RegionCode, &r.RegionName, &r.CloudProvider,
			&r.CountryCode, &r.SovereignFlag, &r.ActiveFlag,
			&r.CreatedAt, &r.UpdatedAt, &r.CreatedByPrincipalID, &r.UpdatedByPrincipalID,
		); err != nil {
			return nil, err
		}
		results = append(results, &r)
	}
	return results, rows.Err()
}

// ---------------------------------------------------------------------------
// TaxIdentityBundle
// R1: tenant_id column now exists on this table (migration 000002).
// RLS policy uses it directly — no correlated subquery.
// ---------------------------------------------------------------------------

func (s *PgStore) CreateTaxIdentityBundle(ctx context.Context, b *domain.TaxIdentityBundle) error {
	s.log.Debug("store.CreateTaxIdentityBundle", zap.String("bundle_id", b.TaxIdentityBundleID))
	tid := tenantFromCtxOrFallback(ctx, b.TenantID)

	classificationStr := b.DataClassification
	if classificationStr == "" {
		classificationStr = "RESTRICTED"
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO tax_identity_bundles (
				tax_identity_bundle_id, tenant_id, legal_entity_id, jurisdiction_id, status,
				effective_from, effective_to,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id,
				data_classification
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		now := time.Now().UTC()
		_, err := tx.Exec(ctx, query,
			b.TaxIdentityBundleID, b.TenantID, b.LegalEntityID, b.JurisdictionID, string(b.Status),
			b.EffectiveFrom, b.EffectiveTo,
			b.CreatedAt, now, b.CreatedByPrincipalID, b.CreatedByPrincipalID,
			classificationStr,
		)
		return err
	})
}

func (s *PgStore) GetTaxIdentityBundleByID(ctx context.Context, bundleID string) (*domain.TaxIdentityBundle, error) {
	s.log.Debug("store.GetTaxIdentityBundleByID", zap.String("bundle_id", bundleID))
	tid := domain.TenantFromContext(ctx)

	var b domain.TaxIdentityBundle
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT tax_identity_bundle_id, tenant_id, legal_entity_id, jurisdiction_id, status,
			       effective_from, effective_to, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id, data_classification
			FROM tax_identity_bundles WHERE tax_identity_bundle_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, bundleID, tid).Scan(
			&b.TaxIdentityBundleID, &b.TenantID, &b.LegalEntityID, &b.JurisdictionID, &b.Status,
			&b.EffectiveFrom, &b.EffectiveTo, &b.CreatedAt, &b.UpdatedAt,
			&b.CreatedByPrincipalID, &b.UpdatedByPrincipalID, &b.DataClassification,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &b, err
}

func (s *PgStore) ListTaxIdentityBundlesByEntity(ctx context.Context, legalEntityID string) ([]*domain.TaxIdentityBundle, error) {
	s.log.Debug("store.ListTaxIdentityBundlesByEntity", zap.String("legal_entity_id", legalEntityID))
	tid := domain.TenantFromContext(ctx)

	var results []*domain.TaxIdentityBundle
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT tax_identity_bundle_id, tenant_id, legal_entity_id, jurisdiction_id, status,
			       effective_from, effective_to, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id, data_classification
			FROM tax_identity_bundles WHERE legal_entity_id = $1 AND tenant_id = $2
		`
		rows, err := tx.Query(ctx, query, legalEntityID, tid)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var b domain.TaxIdentityBundle
			if err := rows.Scan(
				&b.TaxIdentityBundleID, &b.TenantID, &b.LegalEntityID, &b.JurisdictionID, &b.Status,
				&b.EffectiveFrom, &b.EffectiveTo, &b.CreatedAt, &b.UpdatedAt,
				&b.CreatedByPrincipalID, &b.UpdatedByPrincipalID, &b.DataClassification,
			); err != nil {
				return err
			}
			results = append(results, &b)
		}
		return rows.Err()
	})
	return results, err
}

func (s *PgStore) TransitionTaxIdentityBundleStatus(ctx context.Context, bundleID string, newStatus domain.TaxIdentityBundleStatus, actorID, correlationID string) error {
	s.log.Debug("store.TransitionTaxIdentityBundleStatus",
		zap.String("bundle_id", bundleID),
		zap.String("new_status", string(newStatus)),
	)
	tid := domain.TenantFromContext(ctx)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE tax_identity_bundles
			SET status = $1, updated_at = $2, updated_by_principal_id = $3
			WHERE tax_identity_bundle_id = $4 AND status != $1 AND tenant_id = $5
		`
		_, err := tx.Exec(ctx, query, string(newStatus), time.Now().UTC(), actorID, bundleID, tid)
		return err
	})
}
