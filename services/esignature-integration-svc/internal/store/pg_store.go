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
	_, err := p.pool.Exec(ctx, query, env.EnvelopeID, env.TenantID, env.LegalEntityID, env.Provider, env.DocumentTitle, env.SignerEmail, env.SignerName, env.Status, env.ExternalRef, env.CreatedAt, env.UpdatedAt)
	return err
}

func (p *PgStore) GetEnvelopeByID(ctx context.Context, id string) (*domain.SignatureEnvelope, error) {
	query := `
		SELECT envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
		FROM signature_envelopes WHERE envelope_id = $1
	`
	var env domain.SignatureEnvelope
	err := p.pool.QueryRow(ctx, query, id).Scan(&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEnvelopeNotFound
		}
		return nil, err
	}
	return &env, nil
}

func (p *PgStore) ListEnvelopes(ctx context.Context, legalEntityID string) ([]domain.SignatureEnvelope, error) {
	query := `
		SELECT envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
		FROM signature_envelopes
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, query, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := make([]domain.SignatureEnvelope, 0)
	for rows.Next() {
		var env domain.SignatureEnvelope
		if err := rows.Scan(&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt); err != nil {
			return nil, err
		}
		res = append(res, env)
	}
	return res, nil
}

func (p *PgStore) UpdateEnvelopeStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.SignatureEnvelope, error) {
	query := `
		UPDATE signature_envelopes
		SET status = $1, external_ref = $2, updated_at = $3
		WHERE envelope_id = $4
		RETURNING envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status, COALESCE(external_ref,''), created_at, updated_at
	`
	var env domain.SignatureEnvelope
	err := p.pool.QueryRow(ctx, query, req.Status, req.ExternalRef, time.Now().UTC(), id).Scan(&env.EnvelopeID, &env.TenantID, &env.LegalEntityID, &env.Provider, &env.DocumentTitle, &env.SignerEmail, &env.SignerName, &env.Status, &env.ExternalRef, &env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrEnvelopeNotFound
		}
		return nil, err
	}
	return &env, nil
}
