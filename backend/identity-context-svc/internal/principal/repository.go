// Package principal provides the data-access layer for Principal records.
// This service is the authoritative owner of Principal and
// PrincipalRoleAssignment (data-model §06.1).
package principal

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Repository implements PrincipalStore against a PostgreSQL/Aurora backing store.
//
// Stub: all methods are no-op stubs with structured log output and SQL
// comments documenting the intended queries.
// Wire to pgx or database/sql with Aurora PostgreSQL before Phase 1 exit criteria.
//
// Idempotency rules:
//   - FindByIDPSubject: safe to repeat
//   - UpdateStatus: re-applying the same status is a DB no-op (WHERE status != $new)
//   - No soft-delete: status transitions only; no DELETE ever issued (doctrine §2.11)
type Repository struct {
	log *zap.Logger
	// db *pgxpool.Pool  — TODO: inject pgx pool
}

func NewRepository(log *zap.Logger) *Repository {
	return &Repository{log: log}
}

// FindByIDPSubject looks up a principal by the IdP subject claim and tenant.
//
// SQL (stub):
//   SELECT * FROM principals
//   WHERE identity_provider_subject = $1
//     AND tenant_id = $2
//     AND status != 'DISABLED'
func (r *Repository) FindByIDPSubject(ctx context.Context, subject, tenantID string) (*domain.Principal, error) {
	r.log.Debug("FindByIDPSubject", zap.String("subject", subject), zap.String("tenant_id", tenantID))
	return nil, nil // stub — returns nil (not found) until DB is wired
}

// FindByID looks up a principal by its internal primary key.
//
// SQL (stub):
//   SELECT * FROM principals WHERE principal_id = $1
func (r *Repository) FindByID(ctx context.Context, principalID string) (*domain.Principal, error) {
	r.log.Debug("FindByID", zap.String("principal_id", principalID))
	return nil, errors.New("not implemented — wire DB before Phase 1 exit criteria")
}

// FindActiveRoleAssignments returns effective role assignments for a principal
// scoped to a legal entity (or all entities if legalEntityID is nil).
//
// SQL (stub):
//   SELECT * FROM principal_role_assignments
//   WHERE principal_id = $1
//     AND (legal_entity_id = $2 OR legal_entity_id IS NULL OR $2 IS NULL)
//     AND effective_from <= NOW()
//     AND effective_to   >  NOW()
func (r *Repository) FindActiveRoleAssignments(
	ctx context.Context,
	principalID string,
	legalEntityID *string,
) ([]domain.PrincipalRoleAssignment, error) {
	r.log.Debug("FindActiveRoleAssignments",
		zap.String("principal_id", principalID),
		zap.Stringp("legal_entity_id", legalEntityID),
	)
	return []domain.PrincipalRoleAssignment{}, nil // stub
}

// FindActiveDelegations returns all active, non-expired delegated authority
// records where this principal is the delegate.
//
// SQL (stub):
//   SELECT * FROM delegated_authorities
//   WHERE delegate_principal_id = $1
//     AND revocation_status = 'ACTIVE'
//     AND effective_from <= NOW()
//     AND effective_to   >  NOW()
func (r *Repository) FindActiveDelegations(
	ctx context.Context,
	principalID string,
) ([]domain.DelegatedAuthority, error) {
	r.log.Debug("FindActiveDelegations", zap.String("principal_id", principalID))
	return []domain.DelegatedAuthority{}, nil // stub
}

// UpdateStatus transitions principal status. No DELETE issued.
// Idempotent on same-status re-apply (WHERE status != $new prevents no-op writes).
//
// SQL (stub):
//   UPDATE principals SET status = $1, updated_at = NOW()
//   WHERE principal_id = $2 AND status != $1
//   RETURNING principal_id
//
//   INSERT INTO access_decision_log (principal_id, action_type, decision_outcome,
//     decision_basis, correlation_id, decided_at)
//   VALUES ($2, 'STATUS_TRANSITION', 'APPLIED', $1, $3, NOW())
func (r *Repository) UpdateStatus(
	ctx context.Context,
	principalID string,
	newStatus domain.PrincipalStatus,
	actorID string,
	correlationID string,
) error {
	r.log.Info("principal.status.changed",
		zap.String("principal_id", principalID),
		zap.String("new_status", string(newStatus)),
		zap.String("actor_id", actorID),
		zap.String("correlation_id", correlationID),
	)
	return nil // stub
}
