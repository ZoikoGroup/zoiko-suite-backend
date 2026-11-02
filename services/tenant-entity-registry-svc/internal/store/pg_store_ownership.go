package store

// ORG-04 ownership and control relationships (migration 000014).
//
// Every write goes through withRLS, so the tenant is the verified one; the
// tenant_id column is never taken from a request body.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const ownershipColumns = `
	relationship_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
	relationship_kind, ownership_pct, voting_pct, control_type, beneficial_notes,
	valid_from, valid_to, recorded_at, status, evidence_ref, reason,
	proposed_by_principal_id, proposed_at, decided_by_principal_id, decided_at,
	decision_note, COALESCE(correlation_id, ''), record_version`

func scanOwnership(row pgx.Row) (*domain.OwnershipRelationship, error) {
	var r domain.OwnershipRelationship
	var ctl *string
	err := row.Scan(
		&r.RelationshipID, &r.TenantID, &r.ParentLegalEntityID, &r.ChildLegalEntityID,
		&r.RelationshipKind, &r.OwnershipPct, &r.VotingPct, &ctl, &r.BeneficialNotes,
		&r.ValidFrom, &r.ValidTo, &r.RecordedAt, &r.Status, &r.EvidenceRef, &r.Reason,
		&r.ProposedByPrincipalID, &r.ProposedAt, &r.DecidedByPrincipalID, &r.DecidedAt,
		&r.DecisionNote, &r.CorrelationID, &r.RecordVersion)
	if err != nil {
		return nil, err
	}
	if ctl != nil {
		c := domain.ControlType(*ctl)
		r.ControlType = &c
	}
	return &r, nil
}

// CreateOwnershipRelationship files a PROPOSED relationship.
func (s *PgStore) CreateOwnershipRelationship(ctx context.Context, r *domain.OwnershipRelationship) error {
	s.log.Debug("store.CreateOwnershipRelationship", zap.String("relationship_id", r.RelationshipID))
	tid := tenantFromCtxOrFallback(ctx, r.TenantID)
	if r.RecordVersion < 1 {
		r.RecordVersion = 1
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		var ctl *string
		if r.ControlType != nil {
			c := string(*r.ControlType)
			ctl = &c
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO ownership_relationships (
				relationship_id, tenant_id, parent_legal_entity_id, child_legal_entity_id,
				relationship_kind, ownership_pct, voting_pct, control_type, beneficial_notes,
				valid_from, valid_to, recorded_at, status, evidence_ref, reason,
				proposed_by_principal_id, proposed_at, correlation_id, record_version
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NULLIF($18,''),$19)`,
			r.RelationshipID, tid, r.ParentLegalEntityID, r.ChildLegalEntityID,
			string(r.RelationshipKind), r.OwnershipPct, r.VotingPct, ctl, r.BeneficialNotes,
			r.ValidFrom, r.ValidTo, r.RecordedAt, string(r.Status), r.EvidenceRef, r.Reason,
			r.ProposedByPrincipalID, r.ProposedAt, r.CorrelationID, r.RecordVersion)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: this relationship is already proposed or active from that date", registry.ErrConflict)
			}
			return err
		}
		return s.enqueueFor(ctx, tx, r)
	})
}

// GetOwnershipRelationship returns one relationship, or (nil, nil) when it does
// not exist in the caller's tenant.
func (s *PgStore) GetOwnershipRelationship(ctx context.Context, relationshipID string) (*domain.OwnershipRelationship, error) {
	s.log.Debug("store.GetOwnershipRelationship", zap.String("relationship_id", relationshipID))
	tid := domain.TenantFromContext(ctx)

	var out *domain.OwnershipRelationship
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		r, err := scanOwnership(tx.QueryRow(ctx,
			`SELECT `+ownershipColumns+` FROM ownership_relationships
			  WHERE relationship_id = $1 AND tenant_id = $2`, relationshipID, tid))
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// DecideOwnershipRelationship approves (ACTIVE) or rejects (REJECTED) a PROPOSED
// relationship in one guarded UPDATE, so the state check, the version check and
// the write cannot be separated by a concurrent decision.
//
// The decider must differ from the proposer. The service checks first so the
// refusal can say why; the or_no_self_approval CHECK is the backstop, and a
// violation of it is reported as the same SoD refusal.
func (s *PgStore) DecideOwnershipRelationship(ctx context.Context, relationshipID string, expectedVersion int64,
	status domain.OwnershipStatus, decidedBy, note, correlationID string) (*domain.OwnershipRelationship, error) {
	s.log.Debug("store.DecideOwnershipRelationship",
		zap.String("relationship_id", relationshipID), zap.String("status", string(status)))
	tid := domain.TenantFromContext(ctx)

	var out *domain.OwnershipRelationship
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		r, err := scanOwnership(tx.QueryRow(ctx, `
			UPDATE ownership_relationships
			   SET status = $1, decided_by_principal_id = $2, decided_at = $3,
			       decision_note = NULLIF($4, ''),
			       correlation_id = COALESCE(NULLIF($5, ''), correlation_id),
			       record_version = record_version + 1
			 WHERE relationship_id = $6 AND tenant_id = $7
			   AND status = 'PROPOSED' AND record_version = $8
			RETURNING `+ownershipColumns,
			string(status), decidedBy, now, note, correlationID, relationshipID, tid, expectedVersion))
		if errors.Is(err, pgx.ErrNoRows) {
			return ownershipDecisionRefusal(ctx, tx, relationshipID, tid, expectedVersion)
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "or_no_self_approval" {
				return registry.ErrSelfApproval
			}
			return err
		}
		out = r
		return s.enqueueFor(ctx, tx, r)
	})
	return out, err
}

// ownershipDecisionRefusal explains a guarded UPDATE that matched no row: the
// relationship is absent, was already decided, or moved since the caller read it.
func ownershipDecisionRefusal(ctx context.Context, tx pgx.Tx, relationshipID, tenantID string, expectedVersion int64) error {
	var status string
	var version int64
	err := tx.QueryRow(ctx,
		`SELECT status, record_version FROM ownership_relationships WHERE relationship_id = $1 AND tenant_id = $2`,
		relationshipID, tenantID).Scan(&status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != string(domain.OwnershipProposed) {
		return fmt.Errorf("%w: the relationship is %s, only a PROPOSED relationship can be decided",
			registry.ErrInvalidTransition, status)
	}
	return fmt.Errorf("%w: you supplied expected_version %d but this relationship is at %d — reload and retry",
		registry.ErrVersionConflict, expectedVersion, version)
}
