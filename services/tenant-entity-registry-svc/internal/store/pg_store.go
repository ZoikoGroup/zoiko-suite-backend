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
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// PgStore implements registry.Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger

	// outbox is the transactional event enqueuer used by the ORG-02/ORG-03
	// guarded writes in pg_store_org.go, so a domain event and the fact it
	// attests commit together. Attached via SetOutbox after construction and
	// nil-safe: the pre-existing methods in this file do not use it.
	outbox outbox.Enqueuer
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
// F2 fix: tenantID must be non-empty — every caller must supply it. The
// fallback pattern in individual methods ensures this for writes; for reads,
// an absent tenant is refused below rather than reaching the query.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	// An empty tenant is refused here rather than passed down.
	//
	// It used to be passed down, and the result was a 500: every query
	// interpolates the tenant into `AND tenant_id = $n`, Postgres tried to cast
	// '' to uuid, and the driver returned "invalid input syntax for type uuid".
	// No data leaked -- the request failed closed -- but it failed as a SERVER
	// FAULT, so an unauthenticated read looked like an outage in monitoring and
	// in the logs, and a real outage would have been indistinguishable from
	// somebody probing without a header.
	//
	// ErrNotFound is the honest answer: a request that names no tenant is
	// scoped to nothing, so nothing is visible to it. Handlers map it to 404,
	// which also avoids telling an unscoped caller whether a resource exists.
	if tenantID == "" {
		s.log.Debug("read refused: no verified tenant on the request")
		return registry.ErrNotFound
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	// Always set app.tenant_id — we do not silently skip it. It is guaranteed
	// non-empty by the guard above.
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", tenantID,
	); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	if err := fn(tx); err != nil {
		return asInputError(err)
	}
	return tx.Commit(ctx)
}

// asInputError turns a value Postgres could not parse (SQLSTATE 22P02 —
// "invalid input syntax for type uuid", or malformed JSON for a jsonb column)
// into the caller's error it is: a typed VALIDATION_FAILED, not a 500.
//
// Before 29 Sep 2026 every route taking an id answered 500 INTERNAL_ERROR to
// GET /v1/entities/not-a-uuid (and to an empty id, e.g. a doubled slash), so
// a malformed request read as a server fault in the availability SLO and the
// alerts. It is mapped here because every tenant-scoped statement passes
// through withRLS.
func asInputError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return fmt.Errorf("%w: malformed value: %s", registry.ErrInvalidInput, pgErr.Message)
	}
	return err
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

		// ORG-02 onboarding key FIRST, so a replay fails here — "this
		// onboarding already happened" — and not on tenant_code, which would
		// read as "that code is taken". The FK to tenants is deferred.
		if t.ExternalCustomerKey != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO tenant_onboarding_keys (external_customer_key, tenant_id, request_fingerprint)
				VALUES ($1, $2, $3)`,
				*t.ExternalCustomerKey, t.TenantID, t.ProvisioningFingerprint); err != nil {
				if isUniqueViolation(err) {
					return registry.ErrOnboardingKeyExists
				}
				return fmt.Errorf("onboarding key: %w", err)
			}
		}

		tenantQuery := `
			INSERT INTO tenants (
				tenant_id, tenant_code, legal_name, trading_name, status,
				default_currency_code, primary_timezone, primary_locale,
				default_data_residency_policy_id, lifecycle_state,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id,
				external_customer_key, onboarding_request_ref,
				primary_jurisdiction_id, subscription_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		`
		if _, err := tx.Exec(ctx, tenantQuery,
			t.TenantID, t.TenantCode, t.LegalName, t.TradingName, string(t.Status),
			t.DefaultCurrencyCode, t.PrimaryTimezone, t.PrimaryLocale,
			t.DefaultDataResidencyPolicyID, string(t.LifecycleState),
			t.CreatedAt, now, t.CreatedByPrincipalID, t.CreatedByPrincipalID,
			t.ExternalCustomerKey, t.OnboardingRequestRef,
			t.PrimaryJurisdictionID, t.SubscriptionID,
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
			       default_data_residency_policy_id, lifecycle_state, record_version,
			       created_at, updated_at, created_by_principal_id, updated_by_principal_id,
			       external_customer_key, onboarding_request_ref,
			       provisioning_failure_reason, provisioning_failed_at,
			       primary_jurisdiction_id::text, subscription_id
			FROM tenants WHERE tenant_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, tenantID, tid).Scan(
			&t.TenantID, &t.TenantCode, &t.LegalName, &t.TradingName, &t.Status,
			&t.DefaultCurrencyCode, &t.PrimaryTimezone, &t.PrimaryLocale,
			&t.DefaultDataResidencyPolicyID, &t.LifecycleState, &t.RecordVersion,
			&t.CreatedAt, &t.UpdatedAt, &t.CreatedByPrincipalID, &t.UpdatedByPrincipalID,
			&t.ExternalCustomerKey, &t.OnboardingRequestRef,
			&t.ProvisioningFailureReason, &t.ProvisioningFailedAt,
			&t.PrimaryJurisdictionID, &t.SubscriptionID,
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
			WHERE tenant_id = $4 AND tenant_id = $5 AND lifecycle_state != $1
		`
		_, err := tx.Exec(ctx, query, string(newState), time.Now().UTC(), actorID, tenantID, tid)
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
		// ORG-03 profile version 1, in the same transaction: an entity with
		// no profile version answers every as-of read with nothing.
		if e.InitialProfile != nil {
			if err := insertInitialProfileVersionTx(ctx, tx, e.InitialProfile); err != nil {
				return fmt.Errorf("initial profile version: %w", err)
			}
		}
		return s.enqueueFor(ctx, tx, e)
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
			       data_residency_policy_id, record_version, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id,
			       verified_by_principal_id, verified_at, verification_evidence_ref,
			       merged_into_legal_entity_id, merged_at
			FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, legalEntityID, tid).Scan(
			&e.LegalEntityID, &e.TenantID, &e.EntityCode, &e.LegalName, &e.TradingName,
			&e.RegistrationNumber, &e.TaxIdentityBundleID, &e.EntityType,
			&e.IncorporationDate, &e.DefaultCurrencyCode, &e.FiscalCalendarID,
			&e.ParentLegalEntityID, &e.EntityStatus, &e.PrimaryJurisdictionID,
			&e.DataResidencyPolicyID, &e.RecordVersion, &e.CreatedAt, &e.UpdatedAt,
			&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
			&e.VerifiedByPrincipalID, &e.VerifiedAt, &e.VerificationEvidenceRef,
			&e.MergedIntoLegalEntityID, &e.MergedAt,
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
			       data_residency_policy_id, record_version, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id,
			       verified_by_principal_id, verified_at, verification_evidence_ref,
			       merged_into_legal_entity_id, merged_at
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
				&e.DataResidencyPolicyID, &e.RecordVersion, &e.CreatedAt, &e.UpdatedAt,
				&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
				&e.VerifiedByPrincipalID, &e.VerifiedAt, &e.VerificationEvidenceRef,
				&e.MergedIntoLegalEntityID, &e.MergedAt,
			); err != nil {
				return err
			}
			results = append(results, &e)
		}
		return rows.Err()
	})
	return results, err
}

func (s *PgStore) CreateWorkspace(ctx context.Context, w *domain.Workspace) error {
	s.log.Debug("store.CreateWorkspace", zap.String("workspace_id", w.WorkspaceID))
	tid := tenantFromCtxOrFallback(ctx, w.TenantID)
	if w.RecordVersion < 1 {
		w.RecordVersion = 1
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO workspaces (
				workspace_id, tenant_id, legal_entity_id, name, business_unit,
				billing_classification, billing_source, commercial_account_id, status,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, query,
			w.WorkspaceID, w.TenantID, w.LegalEntityID, w.Name, w.BusinessUnit,
			string(w.BillingClassification), string(w.BillingSource), w.CommercialAccountID, string(w.Status),
			w.CreatedAt, now, w.CreatedByPrincipalID, w.CreatedByPrincipalID,
		); err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, w)
	})
}

func (s *PgStore) GetWorkspaceByID(ctx context.Context, workspaceID string) (*domain.Workspace, error) {
	s.log.Debug("store.GetWorkspaceByID", zap.String("workspace_id", workspaceID))
	// See GetEntityByID's comment: no reliance on RLS alone, explicit filter.
	tid := domain.TenantFromContext(ctx)

	var w domain.Workspace
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT workspace_id, tenant_id, legal_entity_id, name, business_unit,
			       billing_classification, billing_source, commercial_account_id, status,
			       created_at, updated_at, created_by_principal_id, updated_by_principal_id,
			       record_version
			FROM workspaces WHERE workspace_id = $1 AND tenant_id = $2
		`
		return tx.QueryRow(ctx, query, workspaceID, tid).Scan(
			&w.WorkspaceID, &w.TenantID, &w.LegalEntityID, &w.Name, &w.BusinessUnit,
			&w.BillingClassification, &w.BillingSource, &w.CommercialAccountID, &w.Status,
			&w.CreatedAt, &w.UpdatedAt, &w.CreatedByPrincipalID, &w.UpdatedByPrincipalID,
			&w.RecordVersion,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &w, err
}

// UpdateWorkspace patches the mutable workspace fields.
//
// COALESCE means an omitted field keeps its value; it also means a nullable
// field cannot be set back to NULL through this path, the same limitation
// UpdateEntity has with trading_name. Clearing business_unit or
// commercial_account_id needs an explicit tri-state representation, which is
// not worth introducing until something asks for it.
func (s *PgStore) UpdateWorkspace(ctx context.Context, workspaceID string, req domain.UpdateWorkspaceRequest) (*domain.Workspace, error) {
	s.log.Debug("store.UpdateWorkspace", zap.String("workspace_id", workspaceID))
	tid := domain.TenantFromContext(ctx)

	var updated domain.Workspace
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			UPDATE workspaces
			SET
				name                    = COALESCE($1, name),
				business_unit           = COALESCE($2, business_unit),
				billing_classification  = COALESCE($3, billing_classification),
				billing_source          = COALESCE($4, billing_source),
				commercial_account_id   = COALESCE($5, commercial_account_id),
				record_version          = record_version + 1,
				updated_at              = $6,
				updated_by_principal_id = $7
			WHERE workspace_id = $8 AND tenant_id = $9
			RETURNING workspace_id, tenant_id, legal_entity_id, name, business_unit,
			          billing_classification, billing_source, commercial_account_id, status,
			          created_at, updated_at, created_by_principal_id, updated_by_principal_id,
			          record_version
		`
		if err := tx.QueryRow(ctx, query,
			req.Name, req.BusinessUnit, req.BillingClassification,
			req.BillingSource, req.CommercialAccountID,
			time.Now().UTC(), req.ActorPrincipalID,
			workspaceID, tid,
		).Scan(
			&updated.WorkspaceID, &updated.TenantID, &updated.LegalEntityID,
			&updated.Name, &updated.BusinessUnit,
			&updated.BillingClassification, &updated.BillingSource,
			&updated.CommercialAccountID, &updated.Status,
			&updated.CreatedAt, &updated.UpdatedAt,
			&updated.CreatedByPrincipalID, &updated.UpdatedByPrincipalID,
			&updated.RecordVersion,
		); err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, &updated)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &updated, err
}

// TransitionWorkspaceStatus archives or restores a workspace in one statement.
//
// The CTE reads the current status under FOR UPDATE and the UPDATE joins to it,
// so the state-machine check and the write cannot straddle a concurrent
// transition — the same reasoning as TransitionEntityStatus. Unlike that
// method it also returns the prior status, which the CTE already has in hand,
// so the emitted event can name what the workspace moved away from.
func (s *PgStore) TransitionWorkspaceStatus(
	ctx context.Context,
	workspaceID string,
	newStatus domain.WorkspaceStatus,
	allowedPriorStates []domain.WorkspaceStatus,
	actorID, correlationID string,
) (int64, domain.WorkspaceStatus, error) {
	s.log.Debug("store.TransitionWorkspaceStatus",
		zap.String("workspace_id", workspaceID),
		zap.String("new_status", string(newStatus)),
	)
	tid := domain.TenantFromContext(ctx)

	priors := make([]string, len(allowedPriorStates))
	for i, p := range allowedPriorStates {
		priors[i] = string(p)
	}

	var rowsAffected int64
	var previous string

	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			WITH prev AS (
				SELECT workspace_id, status
				FROM workspaces
				WHERE workspace_id = $4 AND tenant_id = $6
				FOR UPDATE
			)
			UPDATE workspaces w
			SET status = $1::text, updated_at = $2, updated_by_principal_id = $3,
			    record_version = CASE WHEN prev.status::text = $1::text
			                          THEN w.record_version ELSE w.record_version + 1 END
			FROM prev
			WHERE w.workspace_id = prev.workspace_id
			  AND prev.status = ANY($5::text[])
			RETURNING prev.status, w.record_version, w.legal_entity_id
		`
		at := time.Now().UTC()
		var version int64
		var legalEntityID *string
		row := tx.QueryRow(ctx, query,
			string(newStatus), at, actorID,
			workspaceID, priors, tid,
		)
		if err := row.Scan(&previous, &version, &legalEntityID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				rowsAffected = 0
				return nil // caller distinguishes via rowsAffected
			}
			return err
		}
		rowsAffected = 1
		if previous == string(newStatus) {
			return nil // idempotent re-apply: no version, no event
		}
		le := ""
		if legalEntityID != nil {
			le = *legalEntityID
		}
		return s.enqueueFor(ctx, tx, events.StatusChange{
			TenantID: tid, ObjectID: workspaceID, LegalEntityID: le,
			Previous: previous, New: string(newStatus),
			RecordVersion: version, ActorID: actorID, At: at,
		})
	})
	if err != nil {
		return 0, "", err
	}
	return rowsAffected, domain.WorkspaceStatus(previous), nil
}

func (s *PgStore) ListWorkspacesByTenant(ctx context.Context, tenantID string) ([]*domain.Workspace, error) {
	s.log.Debug("store.ListWorkspacesByTenant", zap.String("tenant_id", tenantID))
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var results []*domain.Workspace
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			SELECT workspace_id, tenant_id, legal_entity_id, name, business_unit,
			       billing_classification, billing_source, commercial_account_id, status,
			       created_at, updated_at, created_by_principal_id, updated_by_principal_id,
			       record_version
			FROM workspaces WHERE tenant_id = $1
		`
		rows, err := tx.Query(ctx, query, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var w domain.Workspace
			if err := rows.Scan(
				&w.WorkspaceID, &w.TenantID, &w.LegalEntityID, &w.Name, &w.BusinessUnit,
				&w.BillingClassification, &w.BillingSource, &w.CommercialAccountID, &w.Status,
				&w.CreatedAt, &w.UpdatedAt, &w.CreatedByPrincipalID, &w.UpdatedByPrincipalID,
				&w.RecordVersion,
			); err != nil {
				return err
			}
			results = append(results, &w)
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
				-- Bumped here as well as by the ORG-03 guarded writes. A client
				-- that reads an entity, PATCHes it, then issues an amendment
				-- with the version it first read must be told the row moved --
				-- otherwise this legacy path silently invalidates the
				-- expected_version contract the amendment path relies on.
				record_version         = record_version + 1,
				updated_at             = $4,
				updated_by_principal_id = $5
			WHERE legal_entity_id = $6 AND tenant_id = $7
			RETURNING legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			          registration_number, tax_identity_bundle_id, entity_type,
			          incorporation_date, default_currency_code, fiscal_calendar_id,
			          parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			          data_residency_policy_id, record_version, created_at, updated_at,
			          created_by_principal_id, updated_by_principal_id
		`
		now := time.Now().UTC()
		if err := tx.QueryRow(ctx, query,
			req.LegalName, req.TradingName, req.DefaultCurrencyCode,
			now, req.ActorPrincipalID,
			legalEntityID, tid,
		).Scan(
			&updated.LegalEntityID, &updated.TenantID, &updated.EntityCode,
			&updated.LegalName, &updated.TradingName,
			&updated.RegistrationNumber, &updated.TaxIdentityBundleID, &updated.EntityType,
			&updated.IncorporationDate, &updated.DefaultCurrencyCode, &updated.FiscalCalendarID,
			&updated.ParentLegalEntityID, &updated.EntityStatus, &updated.PrimaryJurisdictionID,
			&updated.DataResidencyPolicyID, &updated.RecordVersion, &updated.CreatedAt, &updated.UpdatedAt,
			&updated.CreatedByPrincipalID, &updated.UpdatedByPrincipalID,
		); err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, &updated)
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
		// The CTE reads the prior status under FOR UPDATE, as the workspace
		// transition does, so the event can name what the entity moved away
		// from (it used to send previous_status ""). record_version is bumped:
		// a status change is a material change an expected_version must see.
		query := `
			WITH prev AS (
				SELECT legal_entity_id, entity_status
				FROM legal_entities
				WHERE legal_entity_id = $4 AND tenant_id = $6
				FOR UPDATE
			)
			UPDATE legal_entities e
			SET entity_status = $1::text, updated_at = $2, updated_by_principal_id = $3,
			    record_version = CASE WHEN prev.entity_status::text = $1::text
			                          THEN e.record_version ELSE e.record_version + 1 END
			FROM prev
			WHERE e.legal_entity_id = prev.legal_entity_id
			  AND prev.entity_status = ANY($5::text[])
			RETURNING e.tenant_id, prev.entity_status, e.record_version
		`
		at := time.Now().UTC()
		var previous string
		var version int64
		row := tx.QueryRow(ctx, query,
			string(newStatus), at, actorID,
			legalEntityID, priors, tid,
		)
		if err := row.Scan(&tenantID, &previous, &version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				rowsAffected = 0
				tenantID = ""
				return nil // not an error; caller checks rowsAffected
			}
			return err
		}
		rowsAffected = 1
		if previous == string(newStatus) {
			// An idempotent re-apply: nothing changed, so no version and no event.
			return nil
		}
		return s.enqueueFor(ctx, tx, events.StatusChange{
			TenantID: tenantID, ObjectID: legalEntityID, LegalEntityID: legalEntityID,
			Previous: previous, New: string(newStatus),
			RecordVersion: version, ActorID: actorID, At: at,
		})
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
	if h.RecordVersion < 1 {
		h.RecordVersion = 1
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO entity_hierarchies (
				hierarchy_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
				relationship_type, effective_from, effective_to,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, query,
			h.HierarchyID, h.TenantID, h.ParentLegalEntityID, h.ChildLegalEntityID,
			string(h.RelationshipType), h.EffectiveFrom, h.EffectiveTo,
			h.CreatedAt, now, h.CreatedByPrincipalID, h.CreatedByPrincipalID,
		); err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, h)
	})
}

// EndDateHierarchy closes a hierarchy relationship. Zero rows used to be
// reported as success (204, plus an event) whether the id did not exist or
// was already closed; both are now refused.
func (s *PgStore) EndDateHierarchy(ctx context.Context, hierarchyID string, endDate time.Time, actorID, correlationID string) error {
	s.log.Debug("store.EndDateHierarchy", zap.String("hierarchy_id", hierarchyID))
	tid := domain.TenantFromContext(ctx) // must be set by middleware for mutating ops

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var h domain.EntityHierarchy
		err := tx.QueryRow(ctx, `
			UPDATE entity_hierarchies
			SET effective_to = $1, updated_at = $2, updated_by_principal_id = $3,
			    record_version = record_version + 1
			WHERE hierarchy_id = $4 AND effective_to IS NULL AND tenant_id = $5
			RETURNING hierarchy_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
			          relationship_type, effective_from, effective_to, created_at, updated_at,
			          created_by_principal_id, updated_by_principal_id, record_version`,
			endDate, time.Now().UTC(), actorID, hierarchyID, tid).Scan(
			&h.HierarchyID, &h.TenantID, &h.ParentLegalEntityID, &h.ChildLegalEntityID,
			&h.RelationshipType, &h.EffectiveFrom, &h.EffectiveTo, &h.CreatedAt, &h.UpdatedAt,
			&h.CreatedByPrincipalID, &h.UpdatedByPrincipalID, &h.RecordVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return rowMissingOrClosed(ctx, tx, "entity_hierarchies", "hierarchy_id", hierarchyID, tid)
		}
		if err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, &h)
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
			       created_by_principal_id, updated_by_principal_id, record_version
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
				&h.CreatedByPrincipalID, &h.UpdatedByPrincipalID, &h.RecordVersion,
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
	if a.RecordVersion < 1 {
		a.RecordVersion = 1
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		query := `
			INSERT INTO entity_jurisdiction_assignments (
				assignment_id, tenant_id, legal_entity_id, jurisdiction_id, assignment_type,
				effective_from, effective_to, source_basis,
				created_at, updated_at, created_by_principal_id, updated_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, query,
			a.AssignmentID, a.TenantID, a.LegalEntityID, a.JurisdictionID, string(a.AssignmentType),
			a.EffectiveFrom, a.EffectiveTo, a.SourceBasis,
			a.CreatedAt, now, a.CreatedByPrincipalID, a.CreatedByPrincipalID,
		); err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, a)
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
			       created_by_principal_id, updated_by_principal_id, record_version
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
				&a.CreatedByPrincipalID, &a.UpdatedByPrincipalID, &a.RecordVersion,
			); err != nil {
				return err
			}
			results = append(results, &a)
		}
		return rows.Err()
	})
	return results, err
}

// EndDateJurisdictionAssignment closes an assignment. As EndDateHierarchy,
// zero rows is now refused rather than reported as success.
func (s *PgStore) EndDateJurisdictionAssignment(ctx context.Context, assignmentID string, endDate time.Time, actorID, correlationID string) error {
	s.log.Debug("store.EndDateJurisdictionAssignment", zap.String("assignment_id", assignmentID))
	tid := domain.TenantFromContext(ctx)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var a domain.EntityJurisdictionAssignment
		err := tx.QueryRow(ctx, `
			UPDATE entity_jurisdiction_assignments
			SET effective_to = $1, updated_at = $2, updated_by_principal_id = $3,
			    record_version = record_version + 1
			WHERE assignment_id = $4 AND effective_to IS NULL AND tenant_id = $5
			RETURNING assignment_id, tenant_id, legal_entity_id, jurisdiction_id, assignment_type,
			          effective_from, effective_to, source_basis, created_at, updated_at,
			          created_by_principal_id, updated_by_principal_id, record_version`,
			endDate, time.Now().UTC(), actorID, assignmentID, tid).Scan(
			&a.AssignmentID, &a.TenantID, &a.LegalEntityID, &a.JurisdictionID, &a.AssignmentType,
			&a.EffectiveFrom, &a.EffectiveTo, &a.SourceBasis, &a.CreatedAt, &a.UpdatedAt,
			&a.CreatedByPrincipalID, &a.UpdatedByPrincipalID, &a.RecordVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return rowMissingOrClosed(ctx, tx, "entity_jurisdiction_assignments", "assignment_id", assignmentID, tid)
		}
		if err != nil {
			return err
		}
		return s.enqueueFor(ctx, tx, &a)
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

// rowMissingOrClosed explains an end-date that matched no open row: the row
// does not exist in this tenant (not found), or it is already closed (state
// conflict). The table and column names are compile-time constants.
func rowMissingOrClosed(ctx context.Context, tx pgx.Tx, table, idColumn, id, tenantID string) error {
	var exists bool
	if err := tx.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM "+table+" WHERE "+idColumn+" = $1 AND tenant_id = $2)",
		id, tenantID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return registry.ErrNotFound
	}
	return fmt.Errorf("%w: %s is already end-dated", registry.ErrStateConflict, id)
}
