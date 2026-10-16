package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

const idpCols = `idp_id, tenant_id, protocol, display_name, COALESCE(issuer,''), COALESCE(client_id,''),
	COALESCE(client_secret_ref,''), COALESCE(scopes,''), COALESCE(saml_idp_entity_id,''), COALESCE(saml_sso_url,''),
	COALESCE(saml_cert_pem,''), claim_email, claim_name, COALESCE(claim_groups,''), jit_provisioning, link_by_email,
	allowed_domains, status, version, created_by, created_at, updated_at`

func scanIdP(row pgx.Row) (*domain.IdentityProvider, error) {
	var p domain.IdentityProvider
	err := row.Scan(&p.IdPID, &p.TenantID, &p.Protocol, &p.DisplayName, &p.Issuer, &p.ClientID, &p.ClientSecretRef,
		&p.Scopes, &p.SAMLEntityID, &p.SAMLSSOURL, &p.SAMLCertPEM, &p.ClaimEmail, &p.ClaimName, &p.ClaimGroups,
		&p.JITProvisioning, &p.LinkByEmail, &p.AllowedDomains, &p.Status, &p.Version, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// CreateIdP registers a tenant's IdP.
func (s *PgStore) CreateIdP(ctx context.Context, p domain.IdentityProvider, recs ...outbox.Record) error {
	return s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO identity_providers (idp_id, tenant_id, protocol, display_name, issuer,
			    client_id, client_secret_ref, scopes, saml_idp_entity_id, saml_sso_url, saml_cert_pem, claim_email,
			    claim_name, claim_groups, jit_provisioning, link_by_email, allowed_domains, status, version, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,1,$19)`,
			p.IdPID, p.TenantID, p.Protocol, p.DisplayName, nullIfEmpty(p.Issuer), nullIfEmpty(p.ClientID),
			nullIfEmpty(p.ClientSecretRef), nullIfEmpty(p.Scopes), nullIfEmpty(p.SAMLEntityID), nullIfEmpty(p.SAMLSSOURL),
			nullIfEmpty(p.SAMLCertPEM), p.ClaimEmail, p.ClaimName, nullIfEmpty(p.ClaimGroups), p.JITProvisioning,
			p.LinkByEmail, p.AllowedDomains, p.Status, p.CreatedBy); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// SetIdPStatus enables or disables an IdP (version bumped).
func (s *PgStore) SetIdPStatus(ctx context.Context, tenantID, idpID, status string, recs ...outbox.Record) (bool, error) {
	changed := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE identity_providers SET status = $3, version = version + 1, updated_at = NOW()
			WHERE idp_id = $1 AND tenant_id = $2 AND status <> $3`, idpID, tenantID, status)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected() > 0
		if !changed {
			return nil
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	return changed, err
}

// GetIdP reads an IdP by id across tenants (the federation callback knows
// only the id from its own state); nil when absent.
func (s *PgStore) GetIdP(ctx context.Context, idpID string) (*domain.IdentityProvider, error) {
	var out *domain.IdentityProvider
	err := s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanIdP(tx.QueryRow(ctx, `SELECT `+idpCols+` FROM identity_providers WHERE idp_id = $1`, idpID))
		return err
	})
	return out, err
}

// ListIdPs lists a tenant's IdPs.
func (s *PgStore) ListIdPs(ctx context.Context, tenantID string) ([]domain.IdentityProvider, error) {
	var out []domain.IdentityProvider
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+idpCols+` FROM identity_providers WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanIdP(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

// FindFederatedIdentity returns the principal bound to (idp, subject).
func (s *PgStore) FindFederatedIdentity(ctx context.Context, tenantID, idpID, subject string) (*domain.FederatedIdentity, error) {
	var f domain.FederatedIdentity
	var email *string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT idp_id, external_subject, tenant_id, principal_id, email FROM federated_identities
			WHERE idp_id = $1 AND external_subject = $2 AND tenant_id = $3`, idpID, subject, tenantID).Scan(
			&f.IdPID, &f.ExternalSubject, &f.TenantID, &f.PrincipalID, &email)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if email != nil {
		f.Email = *email
	}
	return &f, nil
}

// BindFederatedIdentity binds (idp, subject) to a principal.
func (s *PgStore) BindFederatedIdentity(ctx context.Context, f domain.FederatedIdentity) error {
	return s.withRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO federated_identities (idp_id, external_subject, tenant_id, principal_id, email, last_login_at)
			VALUES ($1,$2,$3,$4,$5,NOW()) ON CONFLICT (idp_id, external_subject) DO NOTHING`,
			f.IdPID, f.ExternalSubject, f.TenantID, f.PrincipalID, nullIfEmpty(f.Email))
		return err
	})
}

// TouchFederatedIdentity records a federated login.
func (s *PgStore) TouchFederatedIdentity(ctx context.Context, tenantID, idpID, subject string, at time.Time) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE federated_identities SET last_login_at = $4
			WHERE idp_id = $1 AND external_subject = $2 AND tenant_id = $3`, idpID, subject, tenantID, at)
		return err
	})
}
