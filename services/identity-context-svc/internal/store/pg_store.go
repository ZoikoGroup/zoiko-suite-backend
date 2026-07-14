// Package store provides the PostgreSQL implementation of
// context.PrincipalStore, replacing the internal/principal stub.
//
// Scope note: PrincipalRoleAssignment and DelegatedAuthority are read-only
// here. This service does not own their write path — Access Control Service
// and Delegated Authority Service do (docs/architecture/03-microservices.md
// §9.3, §9.4) — so no Create/Update methods exist for those tables. Until
// those services exist and identity-context-svc's event consumer is wired to
// populate them (tracked separately), the tables will read back empty.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// PgStore implements identityctx.PrincipalStore against PostgreSQL via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call Close() on shutdown.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Close releases the connection pool.
func (s *PgStore) Close() {
	s.pool.Close()
}

// FindByIDPSubject looks up a principal by IdP subject claim and tenant.
// Returns (nil, nil) when no matching, non-disabled principal exists.
func (s *PgStore) FindByIDPSubject(ctx context.Context, subject, tenantID string) (*domain.Principal, error) {
	s.log.Debug("store.FindByIDPSubject", zap.String("subject", subject), zap.String("tenant_id", tenantID))

	const query = `
		SELECT principal_id, tenant_id, principal_type, identity_provider_subject,
		       email, display_name, status, created_at, data_classification
		FROM principals
		WHERE identity_provider_subject = $1
		  AND tenant_id = $2
		  AND status != 'DISABLED'
	`
	p, err := s.scanOnePrincipal(s.pool.QueryRow(ctx, query, subject, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// FindByID looks up a principal by its internal primary key.
// Returns (nil, nil) when the principal does not exist.
func (s *PgStore) FindByID(ctx context.Context, principalID string) (*domain.Principal, error) {
	s.log.Debug("store.FindByID", zap.String("principal_id", principalID))

	const query = `
		SELECT principal_id, tenant_id, principal_type, identity_provider_subject,
		       email, display_name, status, created_at, data_classification
		FROM principals
		WHERE principal_id = $1
	`
	p, err := s.scanOnePrincipal(s.pool.QueryRow(ctx, query, principalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *PgStore) scanOnePrincipal(row pgx.Row) (*domain.Principal, error) {
	var p domain.Principal
	err := row.Scan(
		&p.PrincipalID, &p.TenantID, &p.PrincipalType, &p.IdentityProviderSubject,
		&p.Email, &p.DisplayName, &p.Status, &p.CreatedAt, &p.DataClassification,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// FindActiveRoleAssignments returns effective role assignments for a principal,
// optionally scoped to a legal entity (all entities if legalEntityID is nil).
func (s *PgStore) FindActiveRoleAssignments(
	ctx context.Context,
	principalID string,
	legalEntityID *string,
) ([]domain.PrincipalRoleAssignment, error) {
	s.log.Debug("store.FindActiveRoleAssignments",
		zap.String("principal_id", principalID),
		zap.Stringp("legal_entity_id", legalEntityID),
	)

	const query = `
		SELECT assignment_id, principal_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by
		FROM principal_role_assignments
		WHERE principal_id = $1
		  AND (legal_entity_id = $2 OR legal_entity_id IS NULL OR $2 IS NULL)
		  AND effective_from <= NOW()
		  AND effective_to   >  NOW()
	`
	rows, err := s.pool.Query(ctx, query, principalID, legalEntityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assignments := []domain.PrincipalRoleAssignment{}
	for rows.Next() {
		var a domain.PrincipalRoleAssignment
		if err := rows.Scan(
			&a.AssignmentID, &a.PrincipalID, &a.RoleID, &a.LegalEntityID, &a.EffectiveFrom, &a.EffectiveTo, &a.AssignedBy,
		); err != nil {
			return nil, err
		}
		assignments = append(assignments, a)
	}
	return assignments, rows.Err()
}

// FindActiveDelegations returns active, non-expired delegations where the
// given principal is the delegate.
func (s *PgStore) FindActiveDelegations(
	ctx context.Context,
	principalID string,
) ([]domain.DelegatedAuthority, error) {
	s.log.Debug("store.FindActiveDelegations", zap.String("principal_id", principalID))

	const query = `
		SELECT delegated_authority_id, delegator_principal_id, delegate_principal_id,
		       scope_type, legal_entity_id, authority_limit_type, authority_limit_value,
		       effective_from, effective_to, revocation_status
		FROM delegated_authorities
		WHERE delegate_principal_id = $1
		  AND revocation_status = 'ACTIVE'
		  AND effective_from <= NOW()
		  AND effective_to   >  NOW()
	`
	rows, err := s.pool.Query(ctx, query, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	delegations := []domain.DelegatedAuthority{}
	for rows.Next() {
		var d domain.DelegatedAuthority
		if err := rows.Scan(
			&d.DelegatedAuthorityID, &d.DelegatorPrincipalID, &d.DelegatePrincipalID,
			&d.ScopeType, &d.LegalEntityID, &d.AuthorityLimitType, &d.AuthorityLimitValue,
			&d.EffectiveFrom, &d.EffectiveTo, &d.RevocationStatus,
		); err != nil {
			return nil, err
		}
		delegations = append(delegations, d)
	}
	return delegations, rows.Err()
}

// UpdateStatus transitions principal status and appends an access-decision-log
// evidence record in the same transaction. No DELETE is ever issued.
//
// Idempotent on same-status re-apply: the UPDATE's WHERE clause excludes rows
// already in the target status, so a repeat call is a no-op (no row updated,
// no evidence record appended) rather than a duplicate audit entry.
func (s *PgStore) UpdateStatus(
	ctx context.Context,
	principalID string,
	newStatus domain.PrincipalStatus,
	actorID string,
	correlationID string,
) error {
	s.log.Info("store.UpdateStatus",
		zap.String("principal_id", principalID),
		zap.String("new_status", string(newStatus)),
		zap.String("actor_id", actorID),
		zap.String("correlation_id", correlationID),
	)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	var tenantID string
	err = tx.QueryRow(ctx, `
		UPDATE principals SET status = $1, updated_at = $2
		WHERE principal_id = $3 AND status != $1
		RETURNING tenant_id
	`, string(newStatus), time.Now().UTC(), principalID).Scan(&tenantID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Already in target status (or principal does not exist) — idempotent no-op.
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO access_decision_log
			(decision_log_id, principal_id, tenant_id, action_type, decision_outcome, decision_basis, correlation_id, decided_at)
		VALUES ($1, $2, $3, 'STATUS_TRANSITION', 'APPLIED', $4, $5, $6)
	`, ulid.Make().String(), principalID, tenantID, string(newStatus), correlationID, time.Now().UTC())
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}
