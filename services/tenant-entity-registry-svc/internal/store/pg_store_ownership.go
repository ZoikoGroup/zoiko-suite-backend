package store

// ORG-04 ownership and control relationships (migrations 000014, 000015).
//
// Every write goes through withRLS, so the tenant is the verified one; the
// tenant_id column is never taken from a request body.
//
// GRAPH WRITES ARE SERIALIZED PER TENANT. A cycle or an over-100% total is a
// property of several rows, so two transactions that each look fine alone can
// be wrong together (A->B and B->A proposed approved at once). Anything that
// can make a relationship ACTIVE first takes a per-tenant advisory lock, reads
// the active set, runs domain.CheckOwnershipGraph and only then writes. The
// lock is always taken before any row lock, so two writers cannot deadlock.

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
	decision_note, COALESCE(correlation_id, ''), record_version,
	supersedes_relationship_id, ended_by_principal_id, ended_at, end_reason, end_evidence_ref`

func scanOwnership(row pgx.Row) (*domain.OwnershipRelationship, error) {
	var r domain.OwnershipRelationship
	var ctl *string
	err := row.Scan(
		&r.RelationshipID, &r.TenantID, &r.ParentLegalEntityID, &r.ChildLegalEntityID,
		&r.RelationshipKind, &r.OwnershipPct, &r.VotingPct, &ctl, &r.BeneficialNotes,
		&r.ValidFrom, &r.ValidTo, &r.RecordedAt, &r.Status, &r.EvidenceRef, &r.Reason,
		&r.ProposedByPrincipalID, &r.ProposedAt, &r.DecidedByPrincipalID, &r.DecidedAt,
		&r.DecisionNote, &r.CorrelationID, &r.RecordVersion,
		&r.SupersedesRelationshipID, &r.EndedByPrincipalID, &r.EndedAt, &r.EndReason, &r.EndEvidenceRef)
	if err != nil {
		return nil, err
	}
	if ctl != nil {
		c := domain.ControlType(*ctl)
		r.ControlType = &c
	}
	return &r, nil
}

// lockOwnershipGraph serializes graph writes for one tenant until commit.
func lockOwnershipGraph(ctx context.Context, tx pgx.Tx, tenantID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ownership:' || $1::text, 0))`, tenantID)
	return err
}

// activeOwnership is every ACTIVE relationship of the tenant. Group structures
// are small (hundreds of rows), so the rules run over the whole set in memory,
// where they are also testable without a database.
func activeOwnership(ctx context.Context, tx pgx.Tx, tenantID string) ([]domain.OwnershipRelationship, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+ownershipColumns+` FROM ownership_relationships WHERE tenant_id = $1 AND status = 'ACTIVE'`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OwnershipRelationship
	for rows.Next() {
		r, err := scanOwnership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// checkGraph runs the ORG-04 graph rules for a relationship about to become (or
// be proposed as) ACTIVE. exclude is the relationship it replaces, if any.
func checkGraph(ctx context.Context, tx pgx.Tx, tenantID string, candidate domain.OwnershipRelationship, exclude string) error {
	active, err := activeOwnership(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	if err := domain.CheckOwnershipGraph(active, candidate, exclude); err != nil {
		return fmt.Errorf("%w: %w", registry.ErrOwnershipConflict, err)
	}
	return nil
}

// CreateOwnershipRelationship files a PROPOSED relationship (or, with
// SupersedesRelationshipID set, a proposed replacement of an ACTIVE one).
//
// The graph rules run here as well as at approval: a proposal that already
// contradicts the active structure is refused now rather than left to rot, and
// approval re-checks because the structure may have moved in between.
func (s *PgStore) CreateOwnershipRelationship(ctx context.Context, r *domain.OwnershipRelationship) error {
	s.log.Debug("store.CreateOwnershipRelationship", zap.String("relationship_id", r.RelationshipID))
	tid := tenantFromCtxOrFallback(ctx, r.TenantID)
	if r.RecordVersion < 1 {
		r.RecordVersion = 1
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := lockOwnershipGraph(ctx, tx, tid); err != nil {
			return err
		}
		exclude := ""
		if r.SupersedesRelationshipID != nil {
			exclude = *r.SupersedesRelationshipID
			old, err := lockOwnership(ctx, tx, exclude, tid)
			if err != nil {
				return err
			}
			if old.Status != domain.OwnershipActive {
				return fmt.Errorf("%w: only an ACTIVE relationship can be superseded; this one is %s",
					registry.ErrInvalidTransition, old.Status)
			}
			if old.ParentLegalEntityID != r.ParentLegalEntityID || old.ChildLegalEntityID != r.ChildLegalEntityID ||
				old.RelationshipKind != r.RelationshipKind {
				return fmt.Errorf("%w: a supersession keeps the same parent, child and kind", registry.ErrInvalidInput)
			}
		}
		if err := checkGraph(ctx, tx, tid, *r, exclude); err != nil {
			return err
		}

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
				proposed_by_principal_id, proposed_at, correlation_id, record_version,
				supersedes_relationship_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,NULLIF($18,''),$19,$20)`,
			r.RelationshipID, tid, r.ParentLegalEntityID, r.ChildLegalEntityID,
			string(r.RelationshipKind), r.OwnershipPct, r.VotingPct, ctl, r.BeneficialNotes,
			r.ValidFrom, r.ValidTo, r.RecordedAt, string(r.Status), r.EvidenceRef, r.Reason,
			r.ProposedByPrincipalID, r.ProposedAt, r.CorrelationID, r.RecordVersion,
			r.SupersedesRelationshipID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: this relationship (or a replacement for it) is already proposed or active from that date", registry.ErrConflict)
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

// lockOwnership reads one relationship and holds its row lock until commit.
func lockOwnership(ctx context.Context, tx pgx.Tx, relationshipID, tenantID string) (*domain.OwnershipRelationship, error) {
	r, err := scanOwnership(tx.QueryRow(ctx,
		`SELECT `+ownershipColumns+` FROM ownership_relationships
		  WHERE relationship_id = $1 AND tenant_id = $2 FOR UPDATE`, relationshipID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrNotFound
	}
	return r, err
}

// DecideOwnershipRelationship approves (ACTIVE) or rejects (REJECTED) a PROPOSED
// relationship.
//
// Approval re-runs the graph rules against the structure as it is now and, for
// a supersession, retires the replaced relationship in the same transaction:
// its status becomes SUPERSEDED and its valid_to is cut to the replacement's
// valid_from, so the two never overlap.
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
		if status == domain.OwnershipActive {
			if err := lockOwnershipGraph(ctx, tx, tid); err != nil {
				return err
			}
		}
		cur, err := lockOwnership(ctx, tx, relationshipID, tid)
		if err != nil {
			return err
		}
		if cur.Status != domain.OwnershipProposed {
			return fmt.Errorf("%w: the relationship is %s, only a PROPOSED relationship can be decided",
				registry.ErrInvalidTransition, cur.Status)
		}
		if cur.RecordVersion != expectedVersion {
			return fmt.Errorf("%w: you supplied expected_version %d but this relationship is at %d — reload and retry",
				registry.ErrVersionConflict, expectedVersion, cur.RecordVersion)
		}

		exclude := ""
		if status == domain.OwnershipActive {
			if cur.SupersedesRelationshipID != nil {
				exclude = *cur.SupersedesRelationshipID
			}
			if err := checkGraph(ctx, tx, tid, *cur, exclude); err != nil {
				return err
			}
		}

		r, err := scanOwnership(tx.QueryRow(ctx, `
			UPDATE ownership_relationships
			   SET status = $1, decided_by_principal_id = $2, decided_at = $3,
			       decision_note = NULLIF($4, ''),
			       correlation_id = COALESCE(NULLIF($5, ''), correlation_id),
			       record_version = record_version + 1
			 WHERE relationship_id = $6 AND tenant_id = $7
			RETURNING `+ownershipColumns,
			string(status), decidedBy, time.Now().UTC(), note, correlationID, relationshipID, tid))
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "or_no_self_approval" {
				return registry.ErrSelfApproval
			}
			return err
		}

		if status == domain.OwnershipActive && exclude != "" {
			tag, err := tx.Exec(ctx, `
				UPDATE ownership_relationships
				   SET status = 'SUPERSEDED',
				       valid_to = CASE WHEN valid_to IS NULL OR valid_to > $1 THEN $1 ELSE valid_to END,
				       record_version = record_version + 1
				 WHERE relationship_id = $2 AND tenant_id = $3 AND status = 'ACTIVE'`,
				r.ValidFrom, exclude, tid)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%w: the relationship being superseded is no longer ACTIVE", registry.ErrStateConflict)
			}
		}
		out = r
		return s.enqueueFor(ctx, tx, r)
	})
	return out, err
}

// EndOwnershipRelationship records that an ACTIVE relationship stopped on
// endDate. The date must fall after its start and not after its current end.
func (s *PgStore) EndOwnershipRelationship(ctx context.Context, relationshipID string, expectedVersion int64,
	endDate time.Time, endedBy, reason, evidenceRef, correlationID string) (*domain.OwnershipRelationship, error) {
	s.log.Debug("store.EndOwnershipRelationship", zap.String("relationship_id", relationshipID))
	tid := domain.TenantFromContext(ctx)

	var out *domain.OwnershipRelationship
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		cur, err := lockOwnership(ctx, tx, relationshipID, tid)
		if err != nil {
			return err
		}
		if cur.Status != domain.OwnershipActive {
			return fmt.Errorf("%w: only an ACTIVE relationship can be ended; this one is %s",
				registry.ErrInvalidTransition, cur.Status)
		}
		if cur.RecordVersion != expectedVersion {
			return fmt.Errorf("%w: you supplied expected_version %d but this relationship is at %d — reload and retry",
				registry.ErrVersionConflict, expectedVersion, cur.RecordVersion)
		}
		if !endDate.After(cur.ValidFrom) {
			return fmt.Errorf("%w: end_date must be after the relationship's valid_from", registry.ErrInvalidInput)
		}
		if cur.ValidTo != nil && endDate.After(*cur.ValidTo) {
			return fmt.Errorf("%w: end_date is after the relationship's existing valid_to; an end can only bring it forward", registry.ErrInvalidInput)
		}

		r, err := scanOwnership(tx.QueryRow(ctx, `
			UPDATE ownership_relationships
			   SET status = 'ENDED', valid_to = $1, ended_by_principal_id = $2, ended_at = $3,
			       end_reason = $4, end_evidence_ref = $5,
			       correlation_id = COALESCE(NULLIF($6, ''), correlation_id),
			       record_version = record_version + 1
			 WHERE relationship_id = $7 AND tenant_id = $8
			RETURNING `+ownershipColumns,
			endDate, endedBy, time.Now().UTC(), reason, evidenceRef, correlationID, relationshipID, tid))
		if err != nil {
			return err
		}
		out = r
		return s.enqueueFor(ctx, tx, r)
	})
	return out, err
}
