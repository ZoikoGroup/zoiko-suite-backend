package store

// REF-01 point-in-time reads and RetireJurisdiction (migration 000027).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// GetJurisdictionAsOf returns the jurisdiction as the version in force at `at`
// had it. The answer comes from jurisdiction_versions, so a rename, a move or a
// retirement since then does not change it.
//
// A jurisdiction that exists but was not in force at `at` is a
// *domain.NotEffectiveError (before it began, or after it was retired), never
// "not found": a caller resolving a historical record must be able to tell a
// real code from a mistyped one. An unknown id is ErrJurisdictionNotFound.
func (s *PgStore) GetJurisdictionAsOf(ctx context.Context, jurisdictionID string, at time.Time) (*domain.JurisdictionAsOf, error) {
	var out domain.JurisdictionAsOf
	var active bool
	var jTo *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT v.jurisdiction_id, j.jurisdiction_code,
		       v.jurisdiction_name, v.jurisdiction_type, v.parent_jurisdiction_id, v.authority_type,
		       v.version_no, v.valid_from, v.valid_to,
		       j.active_flag, j.effective_to, j.retirement_reason, j.superseded_by_jurisdiction_id
		  FROM jurisdiction_versions v
		  JOIN jurisdictions j ON j.jurisdiction_id = v.jurisdiction_id
		 WHERE v.jurisdiction_id = $1
		   AND v.valid_from <= $2
		   AND (v.valid_to IS NULL OR v.valid_to > $2)`, jurisdictionID, at).Scan(
		&out.JurisdictionID, &out.JurisdictionCode,
		&out.JurisdictionName, &out.JurisdictionType, &out.ParentJurisdictionID, &out.AuthorityType,
		&out.VersionNo, &out.ValidFrom, &out.ValidTo,
		&active, &jTo, &out.RetirementReason, &out.SupersededByJurisdictionID)
	if err == nil {
		out.AsOf = at.UTC()
		out.Retired = !active
		if !active {
			out.RetiredAt = jTo
		}
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		if isInvalidTextRepresentation(err) {
			return nil, domain.ErrJurisdictionNotFound
		}
		s.log.Error("pg GetJurisdictionAsOf failed", zap.String("jurisdiction_id", jurisdictionID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil, s.notEffectiveOrNotFound(ctx, jurisdictionID, at)
}

// notEffectiveOrNotFound explains an as-of read that matched no version.
func (s *PgStore) notEffectiveOrNotFound(ctx context.Context, jurisdictionID string, at time.Time) error {
	j, err := s.FindByIDAny(ctx, jurisdictionID)
	if err != nil {
		return err // ErrJurisdictionNotFound, or the store being unavailable
	}
	return &domain.NotEffectiveError{
		JurisdictionID: jurisdictionID, AsOf: at.UTC(), EffectiveFrom: j.EffectiveFrom, EffectiveTo: j.EffectiveTo,
	}
}

// GetHierarchy returns the tree of jurisdictions below rootID as it stood at
// `at`, to at most depth levels below the root. Parentage is read from the
// versions in force at `at`, so a jurisdiction that was moved is shown under
// its parent of that date, and one retired since is still shown if it was then
// in force.
func (s *PgStore) GetHierarchy(ctx context.Context, rootID string, at time.Time, depth int) (*domain.Hierarchy, error) {
	if depth < 0 || depth > domain.MaxHierarchyDepth {
		return nil, domain.ErrInvalidDepth
	}
	// Establish the root first, so a root that is not in force is the explicit
	// not-effective/not-found answer rather than an empty tree.
	if _, err := s.GetJurisdictionAsOf(ctx, rootID, at); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE tree AS (
			SELECT v.jurisdiction_id, j.jurisdiction_code, v.jurisdiction_name, v.jurisdiction_type,
			       v.authority_type, v.parent_jurisdiction_id, 0 AS depth, ARRAY[v.jurisdiction_id] AS path
			  FROM jurisdiction_versions v
			  JOIN jurisdictions j ON j.jurisdiction_id = v.jurisdiction_id
			 WHERE v.jurisdiction_id = $1
			   AND v.valid_from <= $2 AND (v.valid_to IS NULL OR v.valid_to > $2)
			UNION ALL
			SELECT c.jurisdiction_id, cj.jurisdiction_code, c.jurisdiction_name, c.jurisdiction_type,
			       c.authority_type, c.parent_jurisdiction_id, t.depth + 1, t.path || c.jurisdiction_id
			  FROM jurisdiction_versions c
			  JOIN jurisdictions cj ON cj.jurisdiction_id = c.jurisdiction_id
			  JOIN tree t ON c.parent_jurisdiction_id = t.jurisdiction_id
			 WHERE c.valid_from <= $2 AND (c.valid_to IS NULL OR c.valid_to > $2)
			   AND t.depth < $3
			   AND NOT (c.jurisdiction_id = ANY (t.path))
		)
		SELECT jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type, authority_type,
		       parent_jurisdiction_id, depth
		  FROM tree
		 ORDER BY depth, jurisdiction_code, jurisdiction_id
		 LIMIT $4`, rootID, at, depth, domain.MaxHierarchyRows+1)
	if err != nil {
		s.log.Error("pg GetHierarchy failed", zap.String("root", rootID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var flat []domain.HierarchyRow
	for rows.Next() {
		var r domain.HierarchyRow
		if err := rows.Scan(&r.JurisdictionID, &r.JurisdictionCode, &r.JurisdictionName, &r.JurisdictionType,
			&r.AuthorityType, &r.ParentJurisdictionID, &r.Depth); err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		flat = append(flat, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if len(flat) > domain.MaxHierarchyRows {
		return nil, domain.ErrHierarchyTooLarge
	}
	return domain.BuildHierarchy(at, flat), nil
}

// RetireJurisdiction retires a jurisdiction (REF-01 RetireJurisdiction), with
// the reason, the evidence and, optionally, the jurisdiction that replaces it.
// It is DeactivateJurisdiction plus the evidence s4.11 requires, and the bool
// means the same: true when this call retired it, false when it was already
// retired (an idempotent replay, whose original evidence is left untouched).
//
// The effective period is closed at the instant of retirement, which also
// closes the open version in jurisdiction_versions, so the jurisdiction stays
// readable as of any earlier date and is refused for anything new: FindByID
// excludes it, and so does CreateRule.
func (s *PgStore) RetireJurisdictionWithQuerier(ctx context.Context, q Querier, p domain.RetireJurisdictionParams) (*domain.Jurisdiction, bool, error) {
	if p.SupersededBy != nil {
		if *p.SupersededBy == p.JurisdictionID {
			return nil, false, domain.ErrInvalidSuccessor
		}
		if _, err := s.findActiveWith(ctx, q, *p.SupersededBy); err != nil {
			if errors.Is(err, domain.ErrJurisdictionNotFound) {
				return nil, false, domain.ErrInvalidSuccessor
			}
			return nil, false, err
		}
	}

	j, err := scanJurisdiction(q.QueryRow(ctx, `
		UPDATE jurisdictions
		   SET active_flag                   = FALSE,
		       effective_to                  = COALESCE(effective_to, NOW()),
		       retirement_reason             = NULLIF($3, ''),
		       retirement_evidence_ref       = NULLIF($4, ''),
		       superseded_by_jurisdiction_id = $5,
		       updated_at                    = NOW(),
		       updated_by_principal_id       = $2
		 WHERE jurisdiction_id = $1
		   AND ($6::bigint IS NULL OR row_version = $6)
		RETURNING `+jurisdictionColumns+`;`,
		p.JurisdictionID, p.ActorID, p.Reason, p.EvidenceRef, p.SupersededBy, p.ExpectedRowVersion))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The duplicate-deactivation trigger skipped the update (already
			// retired), or the row has moved past the caller's If-Match version.
			existing, findErr := s.FindByIDAny(ctx, p.JurisdictionID)
			if findErr != nil {
				return nil, false, findErr
			}
			if p.ExpectedRowVersion != nil && existing.RowVersion != *p.ExpectedRowVersion {
				return nil, false, domain.ErrVersionConflict
			}
			return existing, false, nil
		}
		if !isInvalidTextRepresentation(err) {
			s.log.Error("pg RetireJurisdiction failed", zap.String("id", p.JurisdictionID), zap.Error(err))
		}
		return nil, false, notFoundOr(err, domain.ErrJurisdictionNotFound)
	}
	return j, true, nil
}
