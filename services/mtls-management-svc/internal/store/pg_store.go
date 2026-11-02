package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/mtls-management-svc/internal/domain"
)

// PgStore is the production Store: certificate records and communication
// policies in Postgres (deployments/migrations/000001_initial_schema).
//
// Every statement runs inside a transaction that first sets app.tenant_id, so
// the tables' FORCEd row-level security applies even to the owner; each query
// also carries its own tenant_id predicate (two independent controls).
//
// Lifecycle transitions (rotate, revoke) lock the row (SELECT … FOR UPDATE /
// UPDATE … RETURNING) inside one transaction, so a concurrent rotate and
// revoke serialise: neither can overwrite the other, and a rotation can never
// land on a record that a concurrent revoke has already made terminal.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore returns a store backed by pool.
func NewPgStore(pool *pgxpool.Pool) *PgStore { return &PgStore{pool: pool} }

var _ Store = (*PgStore)(nil)

const certColumns = `id, tenant_id, legal_entity_id, service_name, common_name, issuer,
	serial_number, fingerprint, certificate_pem, prior_serial_numbers,
	valid_from, valid_to, rotation_days, auto_rotate, status, revoked_at,
	created_at, updated_at`

func scanCert(row pgx.Row) (*domain.MtlsCertificate, error) {
	var c domain.MtlsCertificate
	var status string
	err := row.Scan(&c.ID, &c.TenantID, &c.LegalEntityID, &c.ServiceName, &c.CommonName, &c.Issuer,
		&c.SerialNumber, &c.Fingerprint, &c.CertificatePEM, &c.PriorSerialNumbers,
		&c.ValidFrom, &c.ValidTo, &c.RotationDays, &c.AutoRotate, &status, &c.RevokedAt,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Status = domain.CertStatus(status)
	if len(c.PriorSerialNumbers) == 0 {
		c.PriorSerialNumbers = nil
	}
	return &c, nil
}

// Ping checks the pool can reach the database.
func (s *PgStore) Ping(ctx context.Context) error {
	if s.pool == nil {
		return errors.New("database pool not initialised")
	}
	return s.pool.Ping(ctx)
}

// CreateCert inserts a record. ID and tenant come from the store and the
// verified tenant, never from the caller-populated struct.
func (s *PgStore) CreateCert(ctx context.Context, tenantID string, cert *domain.MtlsCertificate) error {
	cert.TenantID = tenantID
	cert.ID = uuid.New().String()
	now := time.Now().UTC()
	if cert.CreatedAt.IsZero() {
		cert.CreatedAt = now
	}
	if cert.UpdatedAt.IsZero() {
		cert.UpdatedAt = cert.CreatedAt
	}
	prior := cert.PriorSerialNumbers
	if prior == nil {
		prior = []string{}
	}
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO mtls_certificates (id, tenant_id, legal_entity_id, service_name, common_name, issuer,
				serial_number, fingerprint, certificate_pem, prior_serial_numbers,
				valid_from, valid_to, rotation_days, auto_rotate, status, revoked_at, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			cert.ID, tenantID, cert.LegalEntityID, cert.ServiceName, cert.CommonName, cert.Issuer,
			cert.SerialNumber, cert.Fingerprint, cert.CertificatePEM, prior,
			cert.ValidFrom, cert.ValidTo, cert.RotationDays, cert.AutoRotate, string(cert.Status), cert.RevokedAt,
			cert.CreatedAt, cert.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert certificate: %w", err)
		}
		return nil
	})
}

func (s *PgStore) GetCertByID(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *domain.MtlsCertificate
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := scanCert(tx.QueryRow(ctx,
			`SELECT `+certColumns+` FROM mtls_certificates WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get certificate: %w", err)
		}
		out = c
		return nil
	})
	return out, err
}

func (s *PgStore) ListCerts(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.MtlsCertificate, error) {
	var out []domain.MtlsCertificate
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+certColumns+` FROM mtls_certificates
			 WHERE tenant_id = $1
			   AND ($2 = '' OR legal_entity_id = $2)
			   AND ($3 = '' OR status = $3)
			 ORDER BY created_at, id`, tenantID, legalEntityID, status)
		if err != nil {
			return fmt.Errorf("list certificates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCert(rows)
			if err != nil {
				return fmt.Errorf("scan certificate: %w", err)
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// ReplaceCertMaterial locks the record, refuses a revoked one, and swaps in the
// new material in the same transaction — the superseded serial is appended to
// prior_serial_numbers so a later revocation still covers it.
func (s *PgStore) ReplaceCertMaterial(ctx context.Context, tenantID, id string, serialNumber, fingerprint, certificatePEM string, validFrom, validTo time.Time) (*domain.MtlsCertificate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *domain.MtlsCertificate
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT status FROM mtls_certificates WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock certificate: %w", err)
		}
		if domain.CertStatus(status) == domain.CertStatusRevoked {
			return ErrCertRevoked
		}
		c, err := scanCert(tx.QueryRow(ctx, `
			UPDATE mtls_certificates
			   SET prior_serial_numbers = CASE WHEN serial_number = '' THEN prior_serial_numbers
			                                   ELSE array_append(prior_serial_numbers, serial_number) END,
			       serial_number = $3, fingerprint = $4, certificate_pem = $5,
			       valid_from = $6, valid_to = $7,
			       version = version + 1, updated_at = now()
			 WHERE tenant_id = $1 AND id = $2
			RETURNING `+certColumns,
			tenantID, id, serialNumber, fingerprint, certificatePEM, validFrom, validTo))
		if err != nil {
			return fmt.Errorf("replace certificate material: %w", err)
		}
		out = c
		return nil
	})
	return out, err
}

// RevokeCert marks the record REVOKED (idempotently: a second revoke keeps the
// first revoked_at) and returns it as it stands afterwards, under the row lock
// the UPDATE takes.
func (s *PgStore) RevokeCert(ctx context.Context, tenantID, id string) (*domain.MtlsCertificate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *domain.MtlsCertificate
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		c, err := scanCert(tx.QueryRow(ctx, `
			UPDATE mtls_certificates
			   SET status = 'REVOKED',
			       revoked_at = COALESCE(revoked_at, now()),
			       version = CASE WHEN status = 'REVOKED' THEN version ELSE version + 1 END,
			       updated_at = CASE WHEN status = 'REVOKED' THEN updated_at ELSE now() END
			 WHERE tenant_id = $1 AND id = $2
			RETURNING `+certColumns, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("revoke certificate: %w", err)
		}
		out = c
		return nil
	})
	return out, err
}

func (s *PgStore) CreatePolicy(ctx context.Context, tenantID string, pol *domain.CommunicationPolicy) error {
	pol.ID = uuid.New().String()
	pol.TenantID = tenantID
	pol.CreatedAt = time.Now().UTC()
	return s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO mtls_communication_policies (id, tenant_id, policy_name, source_service, target_service,
				action, requires_mtls, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			pol.ID, tenantID, pol.PolicyName, pol.SourceService, pol.TargetService,
			string(pol.Action), pol.RequiresMtls, pol.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert policy: %w", err)
		}
		return nil
	})
}

func (s *PgStore) ListPolicies(ctx context.Context, tenantID string) ([]domain.CommunicationPolicy, error) {
	var out []domain.CommunicationPolicy
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, policy_name, source_service, target_service, action, requires_mtls, created_at
			  FROM mtls_communication_policies WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
		if err != nil {
			return fmt.Errorf("list policies: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p domain.CommunicationPolicy
			var action string
			if err := rows.Scan(&p.ID, &p.TenantID, &p.PolicyName, &p.SourceService, &p.TargetService,
				&action, &p.RequiresMtls, &p.CreatedAt); err != nil {
				return fmt.Errorf("scan policy: %w", err)
			}
			p.Action = domain.PolicyAction(action)
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// inTenantTx runs fn in a transaction scoped to tenantID for row-level
// security. An empty tenant is refused outright rather than run unscoped.
func (s *PgStore) inTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if s.pool == nil {
		return errors.New("database pool not initialised")
	}
	if tenantID == "" {
		return errors.New("tenant scope is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
