// Package store provides the PostgreSQL implementation of
// commercial-account-svc's persistence layer.
//
// Every read/write filters explicitly by organization_id in its own SQL,
// rather than relying on Postgres RLS: this platform's pools connect as a
// Postgres superuser, which unconditionally bypasses RLS regardless of
// policy — see accounts-payable-svc/tenant-entity-registry-svc's own
// pg_store.go doc comments for the same, previously-discovered issue. This
// service is built with the explicit filter from day one.
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
	ListMembershipsByOrganization(ctx context.Context, organizationID string) ([]domain.Membership, error)
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

func (s *PgStore) CreateCommercialAccount(ctx context.Context, a *domain.CommercialAccount) error {
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO commercial_accounts (
			commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
			contact_email, contract_reference, processor_customer_ref, status,
			created_at, updated_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, a.CommercialAccountID, a.OrganizationID, a.LegalCustomerName, a.BillingCurrencyCode,
		a.ContactEmail, a.ContractReference, a.ProcessorCustomerRef, string(a.Status),
		a.CreatedAt, now, a.CreatedByPrincipalID,
	)
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
	err := s.pool.QueryRow(ctx, `
		SELECT commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
		       contact_email, contract_reference, processor_customer_ref, status,
		       created_at, updated_at, created_by_principal_id
		FROM commercial_accounts
		WHERE commercial_account_id = $1 AND ($2 = '' OR organization_id::text = $2)
	`, commercialAccountID, tenantID).Scan(
		&a.CommercialAccountID, &a.OrganizationID, &a.LegalCustomerName, &a.BillingCurrencyCode,
		&a.ContactEmail, &a.ContractReference, &a.ProcessorCustomerRef, &status,
		&a.CreatedAt, &a.UpdatedAt, &a.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommercialAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Status = domain.CommercialAccountStatus(status)
	return &a, nil
}

func (s *PgStore) GetCommercialAccountByOrganization(ctx context.Context, organizationID string) (*domain.CommercialAccount, error) {
	var a domain.CommercialAccount
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT commercial_account_id, organization_id, legal_customer_name, billing_currency_code,
		       contact_email, contract_reference, processor_customer_ref, status,
		       created_at, updated_at, created_by_principal_id
		FROM commercial_accounts WHERE organization_id = $1
	`, organizationID).Scan(
		&a.CommercialAccountID, &a.OrganizationID, &a.LegalCustomerName, &a.BillingCurrencyCode,
		&a.ContactEmail, &a.ContractReference, &a.ProcessorCustomerRef, &status,
		&a.CreatedAt, &a.UpdatedAt, &a.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCommercialAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Status = domain.CommercialAccountStatus(status)
	return &a, nil
}

func (s *PgStore) CreateMembership(ctx context.Context, m *domain.Membership) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memberships (
			membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
			status, effective_from, effective_to, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, m.MembershipID, m.PrincipalID, m.OrganizationID, m.WorkspaceID, m.LegalEntityID,
		string(m.Status), m.EffectiveFrom, m.EffectiveTo, m.CreatedAt, m.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert membership: %w", err)
	}
	return nil
}

func (s *PgStore) GetMembership(ctx context.Context, membershipID string) (*domain.Membership, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var m domain.Membership
	var status string
	err := s.pool.QueryRow(ctx, `
		SELECT membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
		       status, effective_from, effective_to, created_at, created_by_principal_id
		FROM memberships
		WHERE membership_id = $1 AND ($2 = '' OR organization_id::text = $2)
	`, membershipID, tenantID).Scan(
		&m.MembershipID, &m.PrincipalID, &m.OrganizationID, &m.WorkspaceID, &m.LegalEntityID,
		&status, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMembershipNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Status = domain.MembershipStatus(status)
	return &m, nil
}

func (s *PgStore) ListMembershipsByOrganization(ctx context.Context, organizationID string) ([]domain.Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT membership_id, principal_id, organization_id, workspace_id, legal_entity_id,
		       status, effective_from, effective_to, created_at, created_by_principal_id
		FROM memberships WHERE organization_id = $1
		ORDER BY created_at DESC
	`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Membership
	for rows.Next() {
		var m domain.Membership
		var status string
		if err := rows.Scan(
			&m.MembershipID, &m.PrincipalID, &m.OrganizationID, &m.WorkspaceID, &m.LegalEntityID,
			&status, &m.EffectiveFrom, &m.EffectiveTo, &m.CreatedAt, &m.CreatedByPrincipalID,
		); err != nil {
			return nil, err
		}
		m.Status = domain.MembershipStatus(status)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PgStore) DeactivateMembership(ctx context.Context, membershipID, organizationID string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE memberships
		SET status = $1, effective_to = $2
		WHERE membership_id = $3 AND organization_id = $4 AND status = $5
	`, string(domain.MembershipStatusDeactivated), now, membershipID, organizationID, string(domain.MembershipStatusActive))
	if err != nil {
		return fmt.Errorf("deactivate membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMembershipNotFound
	}
	return nil
}
