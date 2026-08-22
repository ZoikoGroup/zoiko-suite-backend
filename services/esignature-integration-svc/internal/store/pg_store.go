package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/middleware"
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

func (p *PgStore) CreateEnvelope(ctx context.Context, env *domain.SignatureEnvelope) error {
	tenantID := middleware.GetTenantID(ctx)
	if env.EnvelopeID == "" {
		env.EnvelopeID = uuid.New().String()
	}
	now := time.Now().UTC()
	env.CreatedAt = now
	env.UpdatedAt = now
	env.TenantID = tenantID

	query := `
		INSERT INTO signature_envelopes (envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, external_ref, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`
	return p.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, query, env.EnvelopeID, env.TenantID, env.LegalEntityID, env.Provider, env.DocumentTitle, env.SignerEmail, env.SignerName, env.Status, env.ExternalRef, env.CreatedAt, env.UpdatedAt)
		return err
	})
}

// GetEnvelopeByID looks up an envelope scoped to the caller's tenant.
//
// The tenant predicate is new. This query was `WHERE envelope_id = $1`
// alone, so any caller holding (or guessing) an envelope_id could read
// another tenant's document_title, signer_email and signer_name — the
// last two being personal data about the signer. Returns
// ErrEnvelopeNotFound rather than a distinct forbidden error, so a probe
// cannot confirm that another tenant's envelope_id exists.
func (p *PgStore) GetEnvelopeByID(ctx context.Context, id string) (*domain.SignatureEnvelope, error) {
	query := `
		SELECT envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
		FROM signature_envelopes
		WHERE envelope_id = $1
		  AND tenant_id = $2
	`
	var env domain.SignatureEnvelope
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, id, middleware.GetTenantID(ctx)).Scan(
			&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEnvelopeNotFound
		}
		return nil, err
	}
	return &env, nil
}

// ListEnvelopes lists the caller's own tenant's envelopes, optionally
// narrowed to one legal entity.
//
// The tenant predicate is new and, unlike the legal-entity one, is NOT
// optional. This query matched when the legal_entity_id parameter was the
// empty string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// every tenant's envelopes.
func (p *PgStore) ListEnvelopes(ctx context.Context, legalEntityID string) ([]domain.SignatureEnvelope, error) {
	query := `
		SELECT envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
		FROM signature_envelopes
		WHERE tenant_id = $2
		  AND ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	res := make([]domain.SignatureEnvelope, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, legalEntityID, middleware.GetTenantID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var env domain.SignatureEnvelope
			if err := rows.Scan(&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt); err != nil {
				return err
			}
			res = append(res, env)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// UpdateEnvelopeStatus transitions an envelope's status, scoped to the
// caller's tenant.
//
// The tenant predicate here closes the most serious hole in this service:
// this was an unscoped WRITE (`WHERE envelope_id = $4` alone), so any
// caller holding another tenant's envelope_id could change that envelope's
// signature status and external_ref — marking someone else's document
// signed or completed. Doc 03 §16.5 makes this service the governed
// external execution path for contracts, board resolutions and legal
// artifacts, so a forged status transition here is a legal-integrity
// problem, not just a data-integrity one. A cross-tenant attempt now
// updates no rows and reports ErrEnvelopeNotFound.
func (p *PgStore) UpdateEnvelopeStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.SignatureEnvelope, error) {
	query := `
		UPDATE signature_envelopes
		SET status = $1, external_ref = $2, updated_at = $3
		WHERE envelope_id = $4
		  AND tenant_id = $5
		RETURNING envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
	`
	var env domain.SignatureEnvelope
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, req.Status, req.ExternalRef, time.Now().UTC(), id, middleware.GetTenantID(ctx)).Scan(
			&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEnvelopeNotFound
		}
		return nil, err
	}
	return &env, nil
}
