package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/delegated-authority-svc/internal/domain"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const delegationColumns = `
	delegation_id, tenant_id, legal_entity_id, delegator_principal_id, delegate_principal_id,
	action_type, effective_from, effective_to, status, created_by_principal_id, correlation_id,
	created_at, updated_at, revoked_by_principal_id, revoked_at, expired_at
`

func scanDelegation(row pgx.Row, d *domain.DelegationGrant) error {
	var status string
	if err := row.Scan(
		&d.DelegationID, &d.TenantID, &d.LegalEntityID, &d.DelegatorPrincipalID, &d.DelegatePrincipalID,
		&d.ActionType, &d.EffectiveFrom, &d.EffectiveTo, &status, &d.CreatedByPrincipalID, &d.CorrelationID,
		&d.CreatedAt, &d.UpdatedAt, &d.RevokedByPrincipalID, &d.RevokedAt, &d.ExpiredAt,
	); err != nil {
		return err
	}
	d.Status = domain.DelegationStatus(status)
	return nil
}

// CreateDelegation inserts a new delegation grant, idempotent on
// (tenant_id, correlation_id).
func (s *PgStore) CreateDelegation(ctx context.Context, d *domain.DelegationGrant) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO delegation_grants (
				delegation_id, tenant_id, legal_entity_id, delegator_principal_id, delegate_principal_id,
				action_type, effective_from, effective_to, status, created_by_principal_id, correlation_id,
				created_at, updated_at, revoked_by_principal_id, revoked_at, expired_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, d.DelegationID, tenantID, d.LegalEntityID, d.DelegatorPrincipalID, d.DelegatePrincipalID,
			d.ActionType, d.EffectiveFrom, d.EffectiveTo, string(d.Status), d.CreatedByPrincipalID, d.CorrelationID,
			d.CreatedAt, d.UpdatedAt, d.RevokedByPrincipalID, d.RevokedAt, d.ExpiredAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND correlation_id = $2", tenantID, d.CorrelationID)
		return scanDelegation(row, d)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// ExpireDue lazily flips any ACTIVE delegation whose EffectiveTo has passed
// to EXPIRED, in a single statement, and returns the rows that actually
// flipped — so the caller can publish authority.expired for each one
// exactly once, at the moment the flip is observed, rather than running a
// separate scheduler process.
func (s *PgStore) ExpireDue(ctx context.Context) ([]domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		rows, err := tx.Query(ctx, `
			UPDATE delegation_grants
			SET status = 'EXPIRED', expired_at = $1, updated_at = $1
			WHERE tenant_id = $2 AND status = 'ACTIVE' AND effective_to < $1
			RETURNING `+delegationColumns, now, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.DelegationGrant
			if err := scanDelegation(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetDelegation(ctx context.Context, delegationID string) (*domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var d domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2", tenantID, delegationID)
		return scanDelegation(row, &d)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDelegationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *PgStore) ListDelegations(ctx context.Context, legalEntityID, delegatorPrincipalID, delegatePrincipalID, status string) ([]domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + delegationColumns + " FROM delegation_grants WHERE tenant_id = $1"
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if delegatorPrincipalID != "" {
			args = append(args, delegatorPrincipalID)
			query += fmt.Sprintf(" AND delegator_principal_id = $%d", len(args))
		}
		if delegatePrincipalID != "" {
			args = append(args, delegatePrincipalID)
			query += fmt.Sprintf(" AND delegate_principal_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.DelegationGrant
			if err := scanDelegation(rows, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeDelegation transitions a delegation from ACTIVE to REVOKED,
// re-checking the current status inside the same transaction the update
// runs in.
func (s *PgStore) RevokeDelegation(ctx context.Context, delegationID, revokedByPrincipalID string) (*domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var current domain.DelegationGrant
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2 FOR UPDATE", tenantID, delegationID)
		if err := scanDelegation(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDelegationNotFound
			}
			return err
		}
		if current.Status != domain.DelegationStatusActive {
			return domain.ErrInvalidTransition
		}

		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE delegation_grants
			SET status = $1, revoked_by_principal_id = $2, revoked_at = $3, updated_at = $3
			WHERE tenant_id = $4 AND delegation_id = $5
		`, string(domain.DelegationStatusRevoked), revokedByPrincipalID, now, tenantID, delegationID); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2", tenantID, delegationID)
		return scanDelegation(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
