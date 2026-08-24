package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so the tenant_isolation_policy added in migration
// 002_add_rls.sql has a value to enforce against.
//
// The tenant is read from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a request body or query
// parameter. GetTenantID returns "" when absent rather than a fabricated
// default, and "" matches no real tenant_id — so a call that somehow
// arrives without a verified tenant sees and writes nothing, instead of
// operating on a synthetic "default-tenant" bucket shared with every other
// such caller.
func (p *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", middleware.GetTenantID(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *PgStore) CreateInterface(ctx context.Context, tf *domain.TaxInterface) error {
	tenantID := middleware.GetTenantID(ctx)
	if tf.InterfaceID == "" {
		tf.InterfaceID = uuid.New().String()
	}
	now := time.Now().UTC()
	tf.CreatedAt = now
	tf.UpdatedAt = now
	tf.TenantID = tenantID

	query := `
		INSERT INTO tax_interfaces (interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, tf.InterfaceID, tf.TenantID, tf.LegalEntityID, tf.Jurisdiction, tf.AuthorityName, tf.Protocol, tf.Status, tf.CreatedAt, tf.UpdatedAt)
		return err
	})
}

// GetInterfaceByID looks up a tax-authority interface scoped to the
// caller's tenant.
//
// The tenant predicate is new. This query was `WHERE interface_id = $1`
// alone, so any caller holding (or guessing) an interface_id could read
// another tenant's jurisdiction and authority_name — which tax
// authorities that tenant files with. Returns ErrInterfaceNotFound rather
// than a distinct forbidden error, so a probe cannot confirm that another
// tenant's interface_id exists.
func (p *PgStore) GetInterfaceByID(ctx context.Context, id string) (*domain.TaxInterface, error) {
	query := `
		SELECT interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at
		FROM tax_interfaces
		WHERE interface_id = $1
		  AND tenant_id = $2
	`
	var tf domain.TaxInterface
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&tf.InterfaceID, &tf.TenantID, &tf.LegalEntityID, &tf.Jurisdiction, &tf.AuthorityName, &tf.Protocol, &tf.Status, &tf.CreatedAt, &tf.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrInterfaceNotFound
		}
		return nil, err
	}
	return &tf, nil
}

// ListInterfaces lists the caller's own tenant's interfaces, optionally
// narrowed to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's tax-authority interfaces.
func (p *PgStore) ListInterfaces(ctx context.Context, legalEntityID string) ([]domain.TaxInterface, error) {
	query := `
		SELECT interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol, status, created_at, updated_at
		FROM tax_interfaces
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.TaxInterface, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var tf domain.TaxInterface
			if err := rows.Scan(&tf.InterfaceID, &tf.TenantID, &tf.LegalEntityID, &tf.Jurisdiction, &tf.AuthorityName, &tf.Protocol, &tf.Status, &tf.CreatedAt, &tf.UpdatedAt); err != nil {
				return err
			}
			res = append(res, tf)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *PgStore) CreateSubmission(ctx context.Context, sub *domain.TaxFilingSubmission) error {
	tenantID := middleware.GetTenantID(ctx)
	if sub.SubmissionID == "" {
		sub.SubmissionID = uuid.New().String()
	}
	sub.SubmittedAt = time.Now().UTC()
	sub.TenantID = tenantID

	query := `
		INSERT INTO tax_filing_submissions (submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, ack_reference, submitted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, sub.SubmissionID, sub.InterfaceID, sub.TenantID, sub.TaxPeriod, sub.FilingType, sub.TaxAmount, sub.Status, sub.AckReference, sub.SubmittedAt)
		return err
	})
}

// GetSubmissionByID looks up a filing submission scoped to the caller's
// tenant.
//
// The tenant predicate is new, and this is the most sensitive read in the
// service. The query was `WHERE submission_id = $1` alone, so any caller
// holding another tenant's submission_id could read its tax_amount,
// tax_period, filing_type and ack_reference — that tenant's actual tax
// filing figures and the authority's acknowledgement of them.
func (p *PgStore) GetSubmissionByID(ctx context.Context, id string) (*domain.TaxFilingSubmission, error) {
	query := `
		SELECT submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, COALESCE(ack_reference, ''), submitted_at
		FROM tax_filing_submissions
		WHERE submission_id = $1
		  AND tenant_id = $2
	`
	var sub domain.TaxFilingSubmission
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&sub.SubmissionID, &sub.InterfaceID, &sub.TenantID, &sub.TaxPeriod, &sub.FilingType, &sub.TaxAmount, &sub.Status, &sub.AckReference, &sub.SubmittedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFilingNotFound
		}
		return nil, err
	}
	return &sub, nil
}

// ListSubmissions lists filing submissions for the caller's tenant,
// optionally narrowed to one interface.
//
// The tenant predicate is new, and this was a self-disabling filter: the
// ONLY predicate matched when the interface_id parameter was the empty
// string OR equalled the column, so calling it with no interface_id
// returned every tenant's tax filings, amounts included. The interface
// dimension stays optional; the tenant dimension never is.
func (p *PgStore) ListSubmissions(ctx context.Context, interfaceID string) ([]domain.TaxFilingSubmission, error) {
	query := `
		SELECT submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, COALESCE(ack_reference, ''), submitted_at
		FROM tax_filing_submissions
		WHERE tenant_id = $2
		  AND ($1 = '' OR interface_id = $1)
		ORDER BY submitted_at DESC
	`
	res := make([]domain.TaxFilingSubmission, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, interfaceID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var sub domain.TaxFilingSubmission
			if err := rows.Scan(&sub.SubmissionID, &sub.InterfaceID, &sub.TenantID, &sub.TaxPeriod, &sub.FilingType, &sub.TaxAmount, &sub.Status, &sub.AckReference, &sub.SubmittedAt); err != nil {
				return err
			}
			res = append(res, sub)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
