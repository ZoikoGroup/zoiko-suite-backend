// Package store provides the PostgreSQL implementation of
// commercial-account-svc's persistence layer.
//
// Tenant scoping here is BOTH an explicit organization_id predicate in
// every query AND a Postgres RLS policy (migration 000005). The previous
// version of this comment argued the explicit filter made RLS unnecessary,
// because "this platform's pools connect as a Postgres superuser, which
// unconditionally bypasses RLS". The superuser observation is true and
// important — it is why every RLS test in this repo now creates a
// NOSUPERUSER NOBYPASSRLS role — but it does not describe production: the
// runtime role is zoiko_app, a non-owner, so RLS does apply to real
// traffic. The conclusion drawn from it (skip RLS) removed the backstop on
// the strength of a property of the test harness.
//
// That mattered, because the explicit filters did not hold on their own:
//
//   - GetCommercialAccount and GetMembership wrote the tenant predicate as
//     ($2 = '' OR organization_id::text = $2), where $2 is the tenant from
//     the request context. TenantFromContext returns "" when the request
//     carried no X-Tenant-Id, and middleware.TenantContext lets such a
//     request through — so a header-less caller disabled its own tenant
//     predicate and could read any account or membership by id. It is the
//     fail-open sibling of the fabricated-"default-tenant" bug found across
//     the connector services: rather than inventing a tenant, it removes
//     the boundary.
//
//   - ListMembershipsByOrganization filtered on an organization_id taken
//     from the URL path, with no comparison to the verified tenant.
//
// Both now take the tenant from the request context and require it. The
// belt (predicates) and the braces (RLS) are both present because either
// one alone has now been observed to fail in this service.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// isUniqueViolation returns true when err is a Postgres unique constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreateCommercialAccount(ctx context.Context, a *domain.CommercialAccount) error
	GetCommercialAccount(ctx context.Context, commercialAccountID string) (*domain.CommercialAccount, error)
	GetCommercialAccountByOrganization(ctx context.Context, organizationID string) (*domain.CommercialAccount, error)
	CreateMembership(ctx context.Context, m *domain.Membership) error
	GetMembership(ctx context.Context, membershipID string) (*domain.Membership, error)
	// ListMemberships lists the verified tenant's own memberships. It
	// deliberately takes NO organization parameter: the previous signature
	// accepted one, the handler filled it from the {organizationID} URL
	// path, and nothing compared it to the caller's verified tenant — so any
	// caller could enumerate any organization's roster. Removing the
	// parameter removes the ability to express that.
	ListMemberships(ctx context.Context) ([]domain.Membership, error)
	DeactivateMembership(ctx context.Context, membershipID, organizationID string) error

	// ── Chunk 6: Plans, Pricing & Entitlements ──────────────────────────────
	CreatePriceCatalog(ctx context.Context, c *domain.PriceCatalog) error
	GetPriceCatalog(ctx context.Context, catalogVersionID string) (*domain.PriceCatalog, error)
	CreatePlan(ctx context.Context, p *domain.Plan) error
	GetPlan(ctx context.Context, planID string) (*domain.Plan, error)
	SetEntitlementLimit(ctx context.Context, l *domain.EntitlementLimit) error
	ListEntitlementLimitsByPlan(ctx context.Context, planID string) ([]domain.EntitlementLimit, error)
	CreateSubscription(ctx context.Context, sub *domain.CommercialSubscription) error
	GetSubscription(ctx context.Context, subscriptionID string) (*domain.CommercialSubscription, error)
	UpdateSubscriptionPlan(ctx context.Context, subscriptionID, planID, catalogVersionID string) error
	CreateEvaluationProgram(ctx context.Context, e *domain.EvaluationProgram) error
	GetEvaluationProgramBySubscription(ctx context.Context, subscriptionID string) (*domain.EvaluationProgram, error)
	CreateOverlay(ctx context.Context, o *domain.ContractEntitlementOverlay) error
	ResolveEntitlement(ctx context.Context, subscriptionID, metricType string) (*domain.ResolvedEntitlement, error)
	RecordUsageEvent(ctx context.Context, e *domain.UsageMeterEvent) error
	CreateChangeRequest(ctx context.Context, c *domain.SubscriptionChangeRequest) error
	GetChangeRequest(ctx context.Context, changeRequestID string) (*domain.SubscriptionChangeRequest, error)
	ApplyChangeRequest(ctx context.Context, changeRequestID string) (*domain.CommercialSubscription, error)

	// ── Chunk 8: Zoiko One Billing & Double-Charge Prevention ───────────────
	TransitionSubscriptionStatus(ctx context.Context, subscriptionID string, newStatus domain.SubscriptionStatus, allowedPriors []domain.SubscriptionStatus, reason *string, principalID string) error
	CreateBillingSourceTransfer(ctx context.Context, transfer *domain.BillingSourceTransfer, newSub *domain.CommercialSubscription) error
	ListStatusEventsBySubscription(ctx context.Context, subscriptionID string) ([]domain.SubscriptionStatusEvent, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so the policies added in migration 000005 have a value
// to enforce against.
//
// A transaction is required, not incidental: set_config's third argument is
// is_local, and only a transaction-local setting is safe here. Setting the
// GUC session-wide on a pooled connection would leak one request's tenant
// into whichever request acquires that connection next — which is a
// cross-tenant read with extra steps.
//
// The tenant is read from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a request body, query string
// or URL path. TenantFromContext returns "" when absent rather than a
// fabricated default, and "" matches no organization_id, so a call that
// somehow arrives without a verified tenant sees and writes nothing.
//
// Every method touching a tenant-scoped table must go through this. That is
// not a style preference: with FORCE RLS enabled, a query that does not set
// app.tenant_id matches zero rows and rejects every insert, so a method
// left un-converted fails loudly rather than silently — but it fails.
// Methods touching only the platform-scope catalog tables (price_catalogs,
// plans, entitlement_limits) deliberately do NOT use this: those tables
// have no policy, and wrapping them would imply a tenant boundary that
// doc7 §U1 says does not exist.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.withScope(ctx, svcmiddleware.TenantFromContext(ctx), fn)
}

// withScope is withTenant with the organization stated explicitly, for the
// two provisioning writes that legitimately act on an organization other
// than the caller's own.
//
// CreateCommercialAccount and CreateMembership are onboarding operations:
// the organization being provisioned is not necessarily the caller's own
// tenant, and for the very first account of a new organization there is no
// prior tenant context to inherit. Binding them to X-Tenant-Id would make
// onboarding impossible, so they pass the organization from the validated
// request instead.
//
// Being precise about what that means: for these two inserts RLS's WITH
// CHECK is satisfied by construction and therefore adds nothing. Their
// actual control is the per-organization authorization check the handler
// already performs — h.authorize(..., req.OrganizationID, ...) against
// authorization-svc, which defaults to DENIED with basis "no_grant". RLS
// still governs every READ of these rows, and every other write in the
// service goes through withTenant. This is documented rather than papered
// over: an insert whose scope comes from the request is only as strong as
// the authz check in front of it, and a reader of this file should not have
// to reverse-engineer that.
// setTenantOnTx sets app.tenant_id on a transaction the caller already
// opened, for the multi-statement methods in subscription_store.go that
// need their own explicit transaction boundary (a status transition plus
// its status event, a billing-source transfer plus the replacement
// subscription). Those cannot be wrapped in withTenant without nesting
// transactions, so they call this immediately after Begin instead.
//
// Same contract as withTenant: transaction-local, tenant from context
// only, "" matches nothing.
func setTenantOnTx(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx),
	)
	return err
}

func (s *PgStore) withScope(ctx context.Context, organizationID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", organizationID,
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateCommercialAccount provisions the account for an organization. It
// scopes to the organization being created, not the caller's header tenant
// — see withScope for why, and for what the actual control is.
func (s *PgStore) CreateCommercialAccount(ctx context.Context, a *domain.CommercialAccount) error {
	now := time.Now().UTC()
	err := s.withScope(ctx, a.OrganizationID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO commercial_accounts (
				commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
				contact_email, contract_reference, processor_customer_ref, status,
				created_at, updated_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, a.CommercialAccountID, a.OrganizationID, a.LegalCustomerName, a.BillingCurrencyCode,
			a.ContactEmail, a.ContractReference, a.ProcessorCustomerRef, string(a.Status),
			a.CreatedAt, now, a.CreatedByPrincipalID,
		)
		return err
	})
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: organization_id %s", domain.ErrConflict, a.OrganizationID)
		}
		return fmt.Errorf("insert commercial account: %w", err)
	}
	return nil
}

func (s *PgStore) GetCommercialAccount(ctx context.Context, commercialAccountID string) (*domain.CommercialAccount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var a domain.CommercialAccount
	var status string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
			       contact_email, contract_reference, processor_customer_ref, status,
			       created_at, updated_at, created_by_principal_id
			FROM commercial_accounts
			WHERE commercial_account_id = $1 AND organization_id::text = $2
		`, commercialAccountID, tenantID).Scan(
			&a.CommercialAccountID, &a.OrganizationID, &a.LegalCustomerName, &a.BillingCurrencyCode,
			&a.ContactEmail, &a.ContractReference, &a.ProcessorCustomerRef, &status,
			&a.CreatedAt, &a.UpdatedAt, &a.CreatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommercialAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Status = domain.CommercialAccountStatus(status)
	return &a, nil
}

// GetCommercialAccountByOrganization looks up an account by organization,
// and refuses any organization other than the caller's verified tenant.
//
// This method currently has no non-test caller, which is the only reason
// its unscoped `WHERE organization_id = $1` was not an exposed read. It is
// scoped anyway: an unscoped store method with no caller today is an
// exposed read the moment someone wires it to a handler, and the next
// person to do that will not re-derive this analysis.
func (s *PgStore) GetCommercialAccountByOrganization(ctx context.Context, organizationID string) (*domain.CommercialAccount, error) {
	if tenantID := svcmiddleware.TenantFromContext(ctx); tenantID == "" || tenantID != organizationID {
		return nil, domain.ErrCommercialAccountNotFound
	}
	var a domain.CommercialAccount
	var status string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
			       contact_email, contract_reference, processor_customer_ref, status,
			       created_at, updated_at, created_by_principal_id
			FROM commercial_accounts WHERE organization_id = $1
		`, organizationID).Scan(
			&a.CommercialAccountID, &a.OrganizationID, &a.LegalCustomerName, &a.BillingCurrencyCode,
			&a.ContactEmail, &a.ContractReference, &a.ProcessorCustomerRef, &status,
			&a.CreatedAt, &a.UpdatedAt, &a.CreatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommercialAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Status = domain.CommercialAccountStatus(status)
	return &a, nil
}

// CreateMembership adds a principal to an organization. Like
// CreateCommercialAccount it scopes to the organization being written
// rather than the caller's header tenant — see withScope.
func (s *PgStore) CreateMembership(ctx context.Context, m *domain.Membership) error {
	err := s.withScope(ctx, m.OrganizationID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO memberships (
				membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
				status, effective_from, effective_to, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, m.MembershipID, m.PrincipalID, m.OrganizationID, m.WorkspaceID, m.LegalEntityID,
			string(m.Status), m.EffectiveFrom, m.EffectiveTo, m.CreatedAt, m.CreatedByPrincipalID,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert membership: %w", err)
	}
	return nil
}

func (s *PgStore) GetMembership(ctx context.Context, membershipID string) (*domain.Membership, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var m domain.Membership
	var status string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
			       status, effective_from, effective_to, created_at, created_by_principal_id
			FROM memberships
			WHERE membership_id = $1 AND organization_id::text = $2
		`, membershipID, tenantID).Scan(
			&m.MembershipID, &m.PrincipalID, &m.OrganizationID, &m.WorkspaceID, &m.LegalEntityID,
			&status, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMembershipNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Status = domain.MembershipStatus(status)
	return &m, nil
}

// ListMemberships lists the verified tenant's own memberships.
//
// The organization is read from the request context, never from a
// parameter. This query previously filtered on an organization_id that the
// handler took straight from the {organizationID} URL path, with no authz
// check and no comparison to the caller's verified tenant — so putting any
// organization's id in the URL returned its full roster: principal ids,
// workspace and legal-entity ids, roles and effective dates. That is
// caller-declared tenant identity, the same class as ai-governance-svc's
// body-supplied tenant_id, but with nothing standing in front of it.
func (s *PgStore) ListMemberships(ctx context.Context) ([]domain.Membership, error) {
	organizationID := svcmiddleware.TenantFromContext(ctx)
	var out []domain.Membership
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
			       status, effective_from, effective_to, created_at, created_by_principal_id
			FROM memberships WHERE organization_id::text = $1
			ORDER BY created_at DESC
		`, organizationID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var m domain.Membership
			var status string
			if err := rows.Scan(
				&m.MembershipID, &m.PrincipalID, &m.OrganizationID, &m.WorkspaceID, &m.LegalEntityID,
				&status, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
			); err != nil {
				return err
			}
			m.Status = domain.MembershipStatus(status)
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeactivateMembership ends a membership. Unlike the two provisioning
// inserts this goes through withTenant: the caller is administering an
// organization that already exists, so it must be acting within its own
// verified tenant. The organizationID argument is retained and still
// checked in SQL — the handler derives it from the row it fetched through
// the tenant-scoped GetMembership, so predicate and policy agree.
func (s *PgStore) DeactivateMembership(ctx context.Context, membershipID, organizationID string) error {
	now := time.Now().UTC()
	var affected int64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE memberships
			SET status = $1, effective_to = $2
			WHERE membership_id = $3 AND organization_id = $4 AND status = $5
		`, string(domain.MembershipStatusDeactivated), now, membershipID, organizationID, string(domain.MembershipStatusActive))
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("deactivate membership: %w", err)
	}
	if affected == 0 {
		return domain.ErrMembershipNotFound
	}
	return nil
}
