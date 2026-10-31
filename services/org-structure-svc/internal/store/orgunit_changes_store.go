package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/org-structure-svc/internal/domain"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
)

// ORG-05 maker-checker persistence. A proposed reparent is stored as a change
// request; approving it applies the move and records the decision in one
// transaction, using the same reparentTx as the direct command.

const changeRequestCols = `request_id, tenant_id, legal_entity_id, unit_id, command, new_parent_unit_id,
	effective_at::text, reason, unit_version, impact, status, requested_by, requested_at,
	decided_by, decided_at, decision_reason, failure_code`

const pendingUnitIndexPredicate = "status = 'PENDING'"

func scanChangeRequest(row pgx.Row) (*domain.ChangeRequest, error) {
	var cr domain.ChangeRequest
	var impact []byte
	err := row.Scan(&cr.RequestID, &cr.TenantID, &cr.LegalEntityID, &cr.UnitID, &cr.Command, &cr.NewParentUnitID,
		&cr.EffectiveAt, &cr.Reason, &cr.UnitVersion, &impact, &cr.Status, &cr.RequestedBy, &cr.RequestedAt,
		&cr.DecidedBy, &cr.DecidedAt, &cr.DecisionReason, &cr.FailureCode)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(impact, &cr.Impact); err != nil {
		return nil, err
	}
	return &cr, nil
}

// ReparentImpact reports what moving a unit reaches on effectiveAt: the unit,
// everything beneath it that day, and which of those carry a cost/profit-center
// mapping. Retired units are ignored: they have nothing left to account for.
func (s *PgStore) ReparentImpact(ctx context.Context, unitID, effectiveAt string) (*domain.ImpactReport, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var report domain.ImpactReport
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_units WHERE tenant_id = $1 AND unit_id = $2)`,
			tenantID, unitID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return domain.ErrOrgUnitNotFound
		}

		const subtree = `
			WITH RECURSIVE sub(id) AS (
				SELECT $1::uuid
				UNION
				SELECT hv.unit_id
				FROM org_unit_hierarchy_versions hv JOIN sub ON hv.parent_unit_id = sub.id
				WHERE hv.superseded_at IS NULL
				  AND hv.valid_from <= $2::date AND (hv.valid_to IS NULL OR hv.valid_to > $2::date)
			)`

		var affected int
		if err := tx.QueryRow(ctx, subtree+` SELECT COUNT(*) FROM sub JOIN org_units u ON u.unit_id = sub.id WHERE u.status <> 'INACTIVE' OR u.unit_id = $1::uuid`,
			unitID, effectiveAt).Scan(&affected); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, subtree+`
			SELECT u.unit_id, u.code, mp.cost_center_code, mp.profit_center_code
			FROM sub
			JOIN org_units u ON u.unit_id = sub.id AND u.status <> 'INACTIVE'
			JOIN org_unit_dimension_mappings mp
			  ON mp.unit_id = u.unit_id
			 AND mp.valid_from <= $2::date AND (mp.valid_to IS NULL OR mp.valid_to > $2::date)
			WHERE mp.cost_center_code <> '' OR mp.profit_center_code <> ''
			ORDER BY u.code ASC, u.unit_id ASC`, unitID, effectiveAt)
		if err != nil {
			return err
		}
		defer rows.Close()
		var mapped []domain.MappedUnit
		for rows.Next() {
			var m domain.MappedUnit
			if err := rows.Scan(&m.UnitID, &m.Code, &m.CostCenterCode, &m.ProfitCenterCode); err != nil {
				return err
			}
			mapped = append(mapped, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		report = domain.NewImpactReport(unitID, affected, mapped)
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
			return nil, domain.ErrOrgUnitNotFound
		}
		return nil, err
	}
	return &report, nil
}

// CreateReparentRequest stores a proposed reparent for independent approval.
//
// The move is first run for real inside a savepoint and rolled back, so every
// rule (version, parent, loop, dates) is checked at proposal time and an
// impossible move is refused to its maker at once instead of waiting for an
// approver. The unit keeps exactly one pending change; proposing the same move
// again returns the pending request (created=false), a different one is refused.
func (s *PgStore) CreateReparentRequest(ctx context.Context, unitID string, req *domain.ReparentOrgUnitRequest, expectedVersion int64, actorID string, impact domain.ImpactReport) (*domain.ChangeRequest, bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, false, domain.ErrIdentityMissing
	}

	var out *domain.ChangeRequest
	var created bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		var dry *domain.OrgUnit
		applyErr := reparentTx(ctx, sp, tenantID, unitID, req, expectedVersion, actorID, "", &dry)
		_ = sp.Rollback(ctx)
		if applyErr != nil {
			return applyErr
		}

		var entityID string
		if err := tx.QueryRow(ctx, `SELECT legal_entity_id FROM org_units WHERE tenant_id = $1 AND unit_id = $2`,
			tenantID, unitID).Scan(&entityID); err != nil {
			return err
		}
		impactJSON, err := json.Marshal(impact)
		if err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO org_unit_change_requests (
				request_id, tenant_id, legal_entity_id, unit_id, command, new_parent_unit_id,
				effective_at, reason, unit_version, impact, status, requested_by, requested_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7::date, $8, $9, $10::jsonb, 'PENDING', $11, $12)
			ON CONFLICT (tenant_id, unit_id) WHERE `+pendingUnitIndexPredicate+` DO NOTHING`,
			uuid.NewString(), tenantID, entityID, unitID, domain.ChangeCommandReparent, req.NewParentUnitID,
			req.EffectiveAt, req.Reason, expectedVersion, string(impactJSON), actorID, time.Now().UTC())
		if err != nil {
			return err
		}

		existing, err := scanChangeRequest(tx.QueryRow(ctx, `
			SELECT `+changeRequestCols+` FROM org_unit_change_requests
			WHERE tenant_id = $1 AND unit_id = $2 AND status = 'PENDING'`, tenantID, unitID))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if existing.Command != domain.ChangeCommandReparent || !sameParent(existing.NewParentUnitID, req.NewParentUnitID) ||
				existing.EffectiveAt != req.EffectiveAt {
				return domain.ErrChangePending
			}
		}
		out, created = existing, tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return nil, false, mapReparentErr(err)
	}
	return out, created, nil
}

func (s *PgStore) GetChangeRequest(ctx context.Context, requestID string) (*domain.ChangeRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ChangeRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cr, err := scanChangeRequest(tx.QueryRow(ctx, `
			SELECT `+changeRequestCols+` FROM org_unit_change_requests WHERE tenant_id = $1 AND request_id = $2`,
			tenantID, requestID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrChangeRequestNotFound
		}
		out = cr
		return err
	})
	return out, mapBadID(err, domain.ErrChangeRequestNotFound)
}

// ListChangeRequests returns a legal entity's change requests, newest first,
// optionally only those in one status.
func (s *PgStore) ListChangeRequests(ctx context.Context, legalEntityID, status string) ([]domain.ChangeRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := []domain.ChangeRequest{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+changeRequestCols+` FROM org_unit_change_requests
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND ($3 = '' OR status = $3)
			ORDER BY requested_at DESC, request_id ASC`, tenantID, legalEntityID, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			cr, err := scanChangeRequest(rows)
			if err != nil {
				return err
			}
			out = append(out, *cr)
		}
		return rows.Err()
	})
	return out, err
}

// lockPendingRequest reads a request FOR UPDATE and checks it can be decided by
// decidedBy: it must be PENDING.
func lockPendingRequest(ctx context.Context, tx pgx.Tx, tenantID, requestID string) (*domain.ChangeRequest, error) {
	cr, err := scanChangeRequest(tx.QueryRow(ctx, `
		SELECT `+changeRequestCols+` FROM org_unit_change_requests
		WHERE tenant_id = $1 AND request_id = $2 FOR UPDATE`, tenantID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrChangeRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	if cr.Status != domain.ChangePending {
		return nil, domain.ErrChangeRequestDecided
	}
	return cr, nil
}

// ApproveChangeRequest applies a pending change and records the approval in one
// transaction. The approver must not be the person who proposed it. The move is
// re-validated against the unit as it is now: if it can no longer be applied as
// written (the unit changed since, a loop appeared, the parent was retired) the
// request is closed as FAILED and the error is returned, so a stale proposal
// cannot linger or be forced through.
func (s *PgStore) ApproveChangeRequest(ctx context.Context, requestID, approverID string) (*domain.ChangeRequest, *domain.OrgUnit, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, nil, domain.ErrIdentityMissing
	}

	var cr *domain.ChangeRequest
	var unit *domain.OrgUnit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		locked, err := lockPendingRequest(ctx, tx, tenantID, requestID)
		if err != nil {
			return err
		}
		if locked.RequestedBy == approverID {
			return domain.ErrSelfApproval
		}

		move := &domain.ReparentOrgUnitRequest{NewParentUnitID: locked.NewParentUnitID, EffectiveAt: locked.EffectiveAt, Reason: locked.Reason}
		// The placement is attributed to the maker; the request id links it to
		// the approval.
		if err := reparentTx(ctx, tx, tenantID, locked.UnitID, move, locked.UnitVersion, locked.RequestedBy, locked.RequestID, &unit); err != nil {
			return err
		}

		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE org_unit_change_requests SET status = 'APPROVED', decided_by = $1, decided_at = $2
			WHERE tenant_id = $3 AND request_id = $4`, approverID, now, tenantID, requestID); err != nil {
			return err
		}
		locked.Status, locked.DecidedBy, locked.DecidedAt = domain.ChangeApproved, &approverID, &now
		cr = locked
		return nil
	})
	if err != nil {
		err = mapReparentErr(err)
		if domain.IsTerminalApplyError(err) {
			_ = s.failChangeRequest(ctx, tenantID, requestID, approverID, err)
		}
		return nil, nil, err
	}
	return cr, unit, nil
}

// failChangeRequest closes a pending request that can never be applied. It runs
// in its own transaction because the approval's was rolled back.
func (s *PgStore) failChangeRequest(ctx context.Context, tenantID, requestID, decidedBy string, cause error) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE org_unit_change_requests
			SET status = 'FAILED', decided_by = $1, decided_at = $2, failure_code = $3
			WHERE tenant_id = $4 AND request_id = $5 AND status = 'PENDING'`,
			decidedBy, time.Now().UTC(), domain.FailureCode(cause), tenantID, requestID)
		return err
	})
}

// RejectChangeRequest closes a pending request without applying it.
func (s *PgStore) RejectChangeRequest(ctx context.Context, requestID, deciderID, reason string) (*domain.ChangeRequest, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	var out *domain.ChangeRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cr, err := lockPendingRequest(ctx, tx, tenantID, requestID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE org_unit_change_requests
			SET status = 'REJECTED', decided_by = $1, decided_at = $2, decision_reason = $3
			WHERE tenant_id = $4 AND request_id = $5`, deciderID, now, reason, tenantID, requestID); err != nil {
			return err
		}
		cr.Status, cr.DecidedBy, cr.DecidedAt, cr.DecisionReason = domain.ChangeRejected, &deciderID, &now, reason
		out = cr
		return nil
	})
	return out, mapBadID(err, domain.ErrChangeRequestNotFound)
}

// mapBadID turns a malformed id (Postgres 22P02) into notFound.
func mapBadID(err, notFound error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidText {
		return notFound
	}
	return err
}
