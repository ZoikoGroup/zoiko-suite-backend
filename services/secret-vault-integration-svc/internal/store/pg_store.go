// Package store provides the PostgreSQL implementation of the secret vault store.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// SecretStore is the data access interface.
type SecretStore interface {
	CreateSecret(ctx context.Context, secret *domain.Secret) error
	GetSecret(ctx context.Context, tenantID, name string) (*domain.Secret, error)
	GetSecretByID(ctx context.Context, tenantID, secretID string) (*domain.Secret, error)
	ListSecrets(ctx context.Context, tenantID string, limit, offset int) ([]*domain.Secret, error)
	UpdateSecret(ctx context.Context, secret *domain.Secret) error
	DeleteSecret(ctx context.Context, tenantID, secretID string) error

	CreateSecretVersion(ctx context.Context, version *domain.SecretVersion) error
	GetSecretVersion(ctx context.Context, tenantID, secretID string, version int) (*domain.SecretVersion, error)
	ListSecretVersions(ctx context.Context, tenantID, secretID string) ([]*domain.SecretVersion, error)

	CreateKeyPair(ctx context.Context, keypair *domain.KeyPair) error
	GetKeyPair(ctx context.Context, tenantID, keyID string) (*domain.KeyPair, error)
	ListKeyPairs(ctx context.Context, tenantID string, activeOnly bool) ([]*domain.KeyPair, error)
	UpdateKeyPair(ctx context.Context, keypair *domain.KeyPair) error
	DeleteKeyPair(ctx context.Context, tenantID, keyID string) error

	// JWKS operations
	GetJWKS(ctx context.Context, tenantID string) (*domain.JWKS, error)

	HealthCheck(ctx context.Context) error
}

// PgStore implements SecretStore against PostgreSQL.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) HealthCheck(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// --- Secrets ---

func (s *PgStore) CreateSecret(ctx context.Context, secret *domain.Secret) error {
	const query = `
		INSERT INTO secrets (secret_id, tenant_id, name, description, version, value, content_type, created_by, expires_at, rotation_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, name) DO NOTHING
		RETURNING secret_id, version, created_at, updated_at;`

	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(ctx, query,
		secret.SecretID, secret.TenantID, secret.Name, secret.Description,
		secret.Version, secret.Value, secret.ContentType, secret.CreatedBy,
		secret.ExpiresAt, secret.RotationPolicy,
	).Scan(&secret.SecretID, &secret.Version, &createdAt, &updatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		s.log.Error("CreateSecret failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	secret.CreatedAt = createdAt
	secret.UpdatedAt = updatedAt
	return nil
}

func (s *PgStore) GetSecret(ctx context.Context, tenantID, name string) (*domain.Secret, error) {
	const query = `
		SELECT secret_id, tenant_id, name, description, version, value, content_type,
		       created_by, created_at, updated_at, expires_at, rotation_policy
		FROM secrets
		WHERE tenant_id = $1 AND name = $2;`
	return s.scanSecret(s.pool.QueryRow(ctx, query, tenantID, name))
}

func (s *PgStore) GetSecretByID(ctx context.Context, tenantID, secretID string) (*domain.Secret, error) {
	const query = `
		SELECT secret_id, tenant_id, name, description, version, value, content_type,
		       created_by, created_at, updated_at, expires_at, rotation_policy
		FROM secrets
		WHERE tenant_id = $1 AND secret_id = $2;`
	return s.scanSecret(s.pool.QueryRow(ctx, query, tenantID, secretID))
}

func (s *PgStore) ListSecrets(ctx context.Context, tenantID string, limit, offset int) ([]*domain.Secret, error) {
	const query = `
		SELECT secret_id, tenant_id, name, description, version, value, content_type,
		       created_by, created_at, updated_at, expires_at, rotation_policy
		FROM secrets
		WHERE tenant_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3;`
	rows, err := s.pool.Query(ctx, query, tenantID, limit, offset)
	if err != nil {
		s.log.Error("ListSecrets query failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var secrets []*domain.Secret
	for rows.Next() {
		secret, err := s.scanSecret(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

func (s *PgStore) UpdateSecret(ctx context.Context, secret *domain.Secret) error {
	const query = `
		UPDATE secrets
		SET description = $3, value = $4, content_type = $5, expires_at = $6,
		    rotation_policy = $7, version = version + 1, updated_at = NOW()
		WHERE tenant_id = $1 AND secret_id = $2
		RETURNING version, updated_at;`

	err := s.pool.QueryRow(ctx, query,
		secret.TenantID, secret.SecretID, secret.Description,
		secret.Value, secret.ContentType, secret.ExpiresAt, secret.RotationPolicy,
	).Scan(&secret.Version, &secret.UpdatedAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSecretNotFound
		}
		s.log.Error("UpdateSecret failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) DeleteSecret(ctx context.Context, tenantID, secretID string) error {
	const query = `DELETE FROM secrets WHERE tenant_id = $1 AND secret_id = $2;`
	cmd, err := s.pool.Exec(ctx, query, tenantID, secretID)
	if err != nil {
		s.log.Error("DeleteSecret failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if cmd.RowsAffected() == 0 {
		return domain.ErrSecretNotFound
	}
	return nil
}

func (s *PgStore) scanSecret(row interface {
	Scan(...interface{}) error
}) (*domain.Secret, error) {
	var secret domain.Secret
	var expiresAt sql.NullTime
	err := row.Scan(
		&secret.SecretID, &secret.TenantID, &secret.Name, &secret.Description,
		&secret.Version, &secret.Value, &secret.ContentType, &secret.CreatedBy,
		&secret.CreatedAt, &secret.UpdatedAt, &expiresAt, &secret.RotationPolicy,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSecretNotFound
		}
		return nil, err
	}
	if expiresAt.Valid {
		secret.ExpiresAt = &expiresAt.Time
	}
	return &secret, nil
}

// --- Secret Versions ---

func (s *PgStore) CreateSecretVersion(ctx context.Context, version *domain.SecretVersion) error {
	const query = `
		INSERT INTO secret_versions (secret_id, version, value, content_type, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (secret_id, version) DO NOTHING
		RETURNING created_at;`

	var createdAt time.Time
	err := s.pool.QueryRow(ctx, query,
		version.SecretID, version.Version, version.Value, version.ContentType, version.CreatedBy,
	).Scan(&createdAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		s.log.Error("CreateSecretVersion failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	version.CreatedAt = createdAt
	return nil
}

func (s *PgStore) GetSecretVersion(ctx context.Context, tenantID, secretID string, version int) (*domain.SecretVersion, error) {
	const query = `
		SELECT sv.secret_id, sv.version, sv.value, sv.content_type, sv.created_by, sv.created_at
		FROM secret_versions sv
		JOIN secrets s ON s.secret_id = sv.secret_id
		WHERE s.tenant_id = $1 AND sv.secret_id = $2 AND sv.version = $3;`
	row := s.pool.QueryRow(ctx, query, tenantID, secretID, version)
	return s.scanSecretVersion(row)
}

func (s *PgStore) ListSecretVersions(ctx context.Context, tenantID, secretID string) ([]*domain.SecretVersion, error) {
	const query = `
		SELECT sv.secret_id, sv.version, sv.value, sv.content_type, sv.created_by, sv.created_at
		FROM secret_versions sv
		JOIN secrets s ON s.secret_id = sv.secret_id
		WHERE s.tenant_id = $1 AND sv.secret_id = $2
		ORDER BY sv.version DESC;`
	rows, err := s.pool.Query(ctx, query, tenantID, secretID)
	if err != nil {
		s.log.Error("ListSecretVersions query failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var versions []*domain.SecretVersion
	for rows.Next() {
		v, err := s.scanSecretVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

func (s *PgStore) scanSecretVersion(row interface {
	Scan(...interface{}) error
}) (*domain.SecretVersion, error) {
	var v domain.SecretVersion
	err := row.Scan(&v.SecretID, &v.Version, &v.Value, &v.ContentType, &v.CreatedBy, &v.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrVersionNotFound
		}
		return nil, err
	}
	return &v, nil
}

// --- Key Pairs ---

func (s *PgStore) CreateKeyPair(ctx context.Context, kp *domain.KeyPair) error {
	const query = `
		INSERT INTO key_pairs (key_id, tenant_id, algorithm, public_key, private_key, created_by, expires_at, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, key_id) DO NOTHING
		RETURNING created_at;`

	var createdAt time.Time
	err := s.pool.QueryRow(ctx, query,
		kp.KeyID, kp.TenantID, kp.Algorithm, kp.PublicKey, kp.PrivateKey,
		kp.CreatedBy, kp.ExpiresAt, kp.Active,
	).Scan(&createdAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrConflict
		}
		s.log.Error("CreateKeyPair failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	kp.CreatedAt = createdAt
	return nil
}

func (s *PgStore) GetKeyPair(ctx context.Context, tenantID, keyID string) (*domain.KeyPair, error) {
	const query = `
		SELECT key_id, tenant_id, algorithm, public_key, private_key, created_by, created_at, expires_at, active
		FROM key_pairs
		WHERE tenant_id = $1 AND key_id = $2;`
	return s.scanKeyPair(s.pool.QueryRow(ctx, query, tenantID, keyID))
}

func (s *PgStore) ListKeyPairs(ctx context.Context, tenantID string, activeOnly bool) ([]*domain.KeyPair, error) {
	query := `
		SELECT key_id, tenant_id, algorithm, public_key, private_key, created_by, created_at, expires_at, active
		FROM key_pairs
		WHERE tenant_id = $1`
	if activeOnly {
		query += ` AND active = true`
	}
	query += ` ORDER BY created_at DESC;`

	rows, err := s.pool.Query(ctx, query, tenantID)
	if err != nil {
		s.log.Error("ListKeyPairs query failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var keypairs []*domain.KeyPair
	for rows.Next() {
		kp, err := s.scanKeyPair(rows)
		if err != nil {
			return nil, err
		}
		keypairs = append(keypairs, kp)
	}
	return keypairs, rows.Err()
}

func (s *PgStore) UpdateKeyPair(ctx context.Context, kp *domain.KeyPair) error {
	const query = `
		UPDATE key_pairs
		SET public_key = $3, private_key = $4, expires_at = $5, active = $6
		WHERE tenant_id = $1 AND key_id = $2;`

	cmd, err := s.pool.Exec(ctx, query,
		kp.TenantID, kp.KeyID, kp.PublicKey, kp.PrivateKey, kp.ExpiresAt, kp.Active)
	if err != nil {
		s.log.Error("UpdateKeyPair failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if cmd.RowsAffected() == 0 {
		return domain.ErrKeyNotFound
	}
	return nil
}

func (s *PgStore) DeleteKeyPair(ctx context.Context, tenantID, keyID string) error {
	const query = `DELETE FROM key_pairs WHERE tenant_id = $1 AND key_id = $2;`
	cmd, err := s.pool.Exec(ctx, query, tenantID, keyID)
	if err != nil {
		s.log.Error("DeleteKeyPair failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if cmd.RowsAffected() == 0 {
		return domain.ErrKeyNotFound
	}
	return nil
}

func (s *PgStore) scanKeyPair(row interface {
	Scan(...interface{}) error
}) (*domain.KeyPair, error) {
	var kp domain.KeyPair
	var expiresAt sql.NullTime
	err := row.Scan(
		&kp.KeyID, &kp.TenantID, &kp.Algorithm, &kp.PublicKey, &kp.PrivateKey,
		&kp.CreatedBy, &kp.CreatedAt, &expiresAt, &kp.Active,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrKeyNotFound
		}
		return nil, err
	}
	if expiresAt.Valid {
		kp.ExpiresAt = &expiresAt.Time
	}
	return &kp, nil
}

// --- JWKS ---

func (s *PgStore) GetJWKS(ctx context.Context, tenantID string) (*domain.JWKS, error) {
	const query = `
		SELECT key_id, algorithm, public_key
		FROM key_pairs
		WHERE tenant_id = $1 AND active = true;`

	rows, err := s.pool.Query(ctx, query, tenantID)
	if err != nil {
		s.log.Error("GetJWKS query failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	jwks := &domain.JWKS{Keys: []domain.JWK{}}
	for rows.Next() {
		var keyID, algorithm, publicKeyPEM string
		if err := rows.Scan(&keyID, &algorithm, &publicKeyPEM); err != nil {
			return nil, err
		}
		jwk := s.pemToJWK(keyID, algorithm, publicKeyPEM)
		if jwk != nil {
			jwks.Keys = append(jwks.Keys, *jwk)
		}
	}
	return jwks, rows.Err()
}

func (s *PgStore) pemToJWK(keyID, algorithm, pem string) *domain.JWK {
	// Simplified: only RSA keys for now
	if algorithm != "RS256" {
		return nil
	}
	// In production, parse the PEM and extract n, e
	// For now return a minimal JWK structure
	return &domain.JWK{
		Kty: "RSA",
		Use: "sig",
		Kid: keyID,
		Alg: algorithm,
		N:   "extracted-from-pem", // Would parse actual PEM
		E:   "AQAB",
	}
}