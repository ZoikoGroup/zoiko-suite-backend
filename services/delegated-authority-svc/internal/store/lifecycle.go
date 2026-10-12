package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/events"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
)

// TransitionKind names a protected state change (ORG-06 lifecycle
// Proposed → Active → Suspended/Expired/Revoked, plus ExtendDelegation).
type TransitionKind string

const (
	Activate TransitionKind = "ACTIVATE"
	Suspend  TransitionKind = "SUSPEND"
	Resume   TransitionKind = "RESUME"
	Revoke   TransitionKind = "REVOKE"
	Extend   TransitionKind = "EXTEND"
)

// Transition applies one protected change to a delegation, in one
// transaction: the version the caller read must still be current (ORG-06
// "all protected changes require authoritative current version"), the change
// must be legal from the current state, the history gains a row and the event
// is enqueued beside it.
//
// Activate and Resume re-enter ACTIVE, so they refuse a lapsed window (no
// grace extension) and the exclusion constraint re-checks overlap; Extend
// refuses a window that has already ended rather than reviving it.
func (s *PgStore) Transition(ctx context.Context, delegationID string, kind TransitionKind, in domain.TransitionInput) (*domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	if in.ExpectedVersion <= 0 {
		return nil, domain.ErrVersionRequired
	}

	var out domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var cur domain.DelegationGrant
		row := tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2 FOR UPDATE", tenantID, delegationID)
		if err := scanDelegation(row, &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDelegationNotFound
			}
			return err
		}
		now := time.Now().UTC()

		var (
			set        string
			args       []any
			transition string
			event      string
		)
		switch kind {
		case Activate:
			if cur.Status != domain.DelegationStatusProposed {
				return domain.ErrInvalidTransition
			}
			set = "status = 'ACTIVE', approved_by_principal_id = $1, approved_at = $2, approval_method = $3"
			args = []any{in.ActorPrincipalID, now, in.ApprovalMethod}
			transition, event = domain.TransitionActivated, events.EventDelegated
		case Suspend:
			if cur.Status != domain.DelegationStatusActive {
				return domain.ErrInvalidTransition
			}
			set = "status = 'SUSPENDED', suspended_by_principal_id = $1, suspended_at = $2, suspension_reason = $3"
			args = []any{in.ActorPrincipalID, now, in.Reason}
			transition, event = domain.TransitionSuspended, events.EventSuspended
		case Resume:
			if cur.Status != domain.DelegationStatusSuspended {
				return domain.ErrInvalidTransition
			}
			set = "status = 'ACTIVE'"
			transition, event = domain.TransitionResumed, events.EventResumed
		case Revoke:
			switch cur.Status {
			case domain.DelegationStatusProposed, domain.DelegationStatusActive, domain.DelegationStatusSuspended:
			default:
				return domain.ErrInvalidTransition
			}
			set = "status = 'REVOKED', revoked_by_principal_id = $1, revoked_at = $2, revocation_reason = $3"
			args = []any{in.ActorPrincipalID, now, in.Reason}
			transition, event = domain.TransitionRevoked, events.EventRevoked
		case Extend:
			if cur.Status != domain.DelegationStatusActive {
				return domain.ErrCannotExtend
			}
			if !in.NewEffectiveTo.After(cur.EffectiveTo) {
				return domain.ErrCannotExtend
			}
			set = "effective_to = $1"
			args = []any{in.NewEffectiveTo}
			transition, event = domain.TransitionExtended, events.EventExtended
		default:
			return fmt.Errorf("unknown transition %q", kind)
		}
		// Version after the state check: a caller late to a terminal grant is
		// told it is terminal, which is the more useful answer.
		if cur.Version != in.ExpectedVersion {
			return domain.ErrVersionMismatch
		}
		if (kind == Activate || kind == Resume || kind == Extend) && !cur.EffectiveTo.After(now) {
			return domain.ErrLapsed
		}

		n := len(args)
		args = append(args, now, tenantID, delegationID)
		q := fmt.Sprintf(`UPDATE delegation_grants SET %s, updated_at = $%d, version = version + 1
			WHERE tenant_id = $%d AND delegation_id = $%d`, set, n+1, n+2, n+3)
		if set == "status = 'ACTIVE'" {
			q = fmt.Sprintf(`UPDATE delegation_grants SET status = 'ACTIVE', updated_at = $1, version = version + 1
				WHERE tenant_id = $2 AND delegation_id = $3`)
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return err
		}
		row = tx.QueryRow(ctx, "SELECT "+delegationColumns+" FROM delegation_grants WHERE tenant_id = $1 AND delegation_id = $2", tenantID, delegationID)
		if err := scanDelegation(row, &out); err != nil {
			return err
		}
		if err := recordHistory(ctx, tx, &out, transition, in.ActorPrincipalID, in.Reason); err != nil {
			return err
		}
		return enqueueBy(ctx, tx, event, out, in.ActorPrincipalID, in.Reason)
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return &out, nil
}

// recordHistory appends one row to delegation_history (append-only by
// trigger, migration 000010) for the grant as it now stands.
func recordHistory(ctx context.Context, tx pgx.Tx, d *domain.DelegationGrant, transition, actor, reason string) error {
	var r any
	if reason != "" {
		r = reason
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO delegation_history (tenant_id, delegation_id, version, transition, status, effective_from, effective_to,
		                                actor_principal_id, reason, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		d.TenantID, d.DelegationID, d.Version, transition, string(d.Status), d.EffectiveFrom, d.EffectiveTo,
		actor, r, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record delegation history: %w", err)
	}
	return nil
}

// CheckOverlap returns ErrOverlapConflict when an ACTIVE delegation of the
// same action to the same delegate on the same entity overlaps
// [effectiveFrom, effectiveTo). Two exclusions, for two different reasons:
//
//   - excludeCorrelationID lets an idempotent replay of a create pass its own
//     original grant;
//   - excludeDelegationID lets a grant being extended, activated or resumed
//     pass ITSELF. Extend used to exclude by the extend request's own new
//     correlation id, so the grant always overlapped its own extended window
//     and every extend answered 409.
func (s *PgStore) CheckOverlap(ctx context.Context, tenantID, legalEntityID, delegatePrincipalID, actionType string, effectiveFrom, effectiveTo time.Time, excludeCorrelationID, excludeDelegationID string) error {
	if tenantID == "" {
		return domain.ErrTenantMissing
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM delegation_grants
				WHERE tenant_id = $1 AND legal_entity_id = $2 AND delegate_principal_id = $3 AND action_type = $4
				  AND status = 'ACTIVE'
				  AND effective_from < $5 AND effective_to > $6
				  AND ($7 = '' OR correlation_id <> $7)
				  AND ($8 = '' OR delegation_id::text <> $8)
			)`, tenantID, legalEntityID, delegatePrincipalID, actionType, effectiveTo, effectiveFrom,
			excludeCorrelationID, excludeDelegationID).Scan(&exists)
		if err != nil {
			return err
		}
		if exists {
			return domain.ErrOverlapConflict
		}
		return nil
	})
}

// ListEffectiveAsOf answers GetEffectiveDelegations for a point in time: the
// grants that conferred authority at asOf, each with the status and window
// it had THEN, reconstructed from delegation_history rather than from the
// current row — a grant revoked yesterday was effective last week.
func (s *PgStore) ListEffectiveAsOf(ctx context.Context, f domain.ListDelegationsFilter, asOf time.Time) ([]domain.DelegationGrant, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	var out []domain.DelegationGrant
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `SELECT ` + prefixedDelegationColumns("g") + `, h.status, h.effective_from, h.effective_to
			FROM delegation_grants g
			JOIN LATERAL (
				SELECT status, effective_from, effective_to FROM delegation_history h
				 WHERE h.tenant_id = g.tenant_id AND h.delegation_id = g.delegation_id AND h.recorded_at <= $2
				 ORDER BY h.version DESC LIMIT 1
			) h ON true
			WHERE g.tenant_id = $1 AND h.status = 'ACTIVE' AND h.effective_from <= $2 AND $2 < h.effective_to`
		args := []any{tenantID, asOf}
		if f.LegalEntityID != "" {
			args = append(args, f.LegalEntityID)
			query += fmt.Sprintf(" AND g.legal_entity_id = $%d", len(args))
		}
		if f.DelegatorPrincipalID != "" {
			args = append(args, f.DelegatorPrincipalID)
			query += fmt.Sprintf(" AND g.delegator_principal_id = $%d", len(args))
		}
		if f.DelegatePrincipalID != "" {
			args = append(args, f.DelegatePrincipalID)
			query += fmt.Sprintf(" AND g.delegate_principal_id = $%d", len(args))
		}
		if f.SelfPrincipalID != "" {
			args = append(args, f.SelfPrincipalID)
			query += fmt.Sprintf(" AND (g.delegator_principal_id = $%d OR g.delegate_principal_id = $%d)", len(args), len(args))
		}
		query += " ORDER BY g.created_at DESC, g.delegation_id DESC"
		if f.Limit > 0 {
			args = append(args, f.Limit)
			query += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if f.Offset > 0 {
			args = append(args, f.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.DelegationGrant
			var status string
			var from, to time.Time
			if err := scanDelegationPlus(rows, &d, &status, &from, &to); err != nil {
				return err
			}
			d.Status, d.EffectiveFrom, d.EffectiveTo = domain.DelegationStatus(status), from, to
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// ExplainDelegationChain returns the path of currently effective delegations
// by which authority for actionType flows from startPrincipalID to
// targetPrincipalID — every link, in order, along the shortest path.
//
// The previous query filtered its final rows to those ending at the target,
// so A→B→C answered with B→C alone: the one link that explained nothing about
// where the authority came from. It also trusted status = 'ACTIVE' without
// the window, which lags the expiry sweeper.
func (s *PgStore) ExplainDelegationChain(ctx context.Context, tenantID, legalEntityID, startPrincipalID, targetPrincipalID, actionType string) ([]domain.DelegationChainStep, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	var out []domain.DelegationChainStep
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var path []string
		err := tx.QueryRow(ctx, `
			WITH RECURSIVE chain AS (
				SELECT dg.delegate_principal_id, 1 AS depth,
				       ARRAY[dg.delegation_id::text] AS path_ids,
				       ARRAY[dg.delegator_principal_id::text, dg.delegate_principal_id::text] AS visited
				  FROM delegation_grants dg
				 WHERE dg.tenant_id = $1 AND dg.legal_entity_id = $2 AND dg.delegator_principal_id = $3
				   AND dg.action_type = $4 AND dg.status = 'ACTIVE'
				   AND dg.effective_from <= now() AND now() < dg.effective_to
				UNION ALL
				SELECT dg.delegate_principal_id, c.depth + 1,
				       c.path_ids || dg.delegation_id::text,
				       c.visited || dg.delegate_principal_id::text
				  FROM delegation_grants dg
				  JOIN chain c ON dg.delegator_principal_id = c.delegate_principal_id
				 WHERE dg.tenant_id = $1 AND dg.legal_entity_id = $2 AND dg.action_type = $4 AND dg.status = 'ACTIVE'
				   AND dg.effective_from <= now() AND now() < dg.effective_to
				   AND NOT (dg.delegate_principal_id = ANY (c.visited))
				   AND c.depth < 10
			)
			SELECT path_ids FROM chain WHERE delegate_principal_id = $5 ORDER BY depth LIMIT 1`,
			tenantID, legalEntityID, startPrincipalID, actionType, targetPrincipalID).Scan(&path)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT delegation_id, delegator_principal_id, delegate_principal_id, action_type, effective_from, effective_to, status
			  FROM delegation_grants
			 WHERE tenant_id = $1 AND delegation_id::text = ANY ($2)
			 ORDER BY array_position($2, delegation_id::text)`, tenantID, path)
		if err != nil {
			return err
		}
		defer rows.Close()
		for i := 1; rows.Next(); i++ {
			var st domain.DelegationChainStep
			if err := rows.Scan(&st.DelegationID, &st.DelegatorPrincipalID, &st.DelegatePrincipalID, &st.ActionType,
				&st.EffectiveFrom, &st.EffectiveTo, &st.Status); err != nil {
				return err
			}
			st.StepNumber = i
			out = append(out, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapPgError(err)
	}
	return out, nil
}

// ── Idempotency-Key replay (cross-service finding 2) ─────────────────────────

// ErrIdempotencyFingerprintMismatch: the key was used for a different request.
var ErrIdempotencyFingerprintMismatch = errors.New("idempotency key reused for a different request")

// IdempotencyInFlightLease is how long a claim with no answer blocks a retry
// before it is presumed abandoned by a crashed process and taken over.
const IdempotencyInFlightLease = 2 * time.Minute

// IdempotencyRecord is a stored answer; ResponseStatus 0 means in flight.
type IdempotencyRecord struct {
	RequestFingerprint string
	ResponseStatus     int
	ResponseBody       []byte
	CreatedAt          time.Time
}

// ClaimIdempotencyKey claims (tenant, endpoint, key). nil, nil means the
// caller holds the claim and must run the command; a record means a previous
// identical request owns the key (answered, or still in flight).
func (s *PgStore) ClaimIdempotencyKey(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*IdempotencyRecord, error) {
	var rec *IdempotencyRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, endpoint, idempotency_key, request_fingerprint)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, endpoint, idempotency_key) DO UPDATE SET created_at = now()
			 WHERE idempotency_keys.response_status = 0
			   AND idempotency_keys.created_at < now() - make_interval(secs => $5)
			   AND idempotency_keys.request_fingerprint = EXCLUDED.request_fingerprint`,
			tenantID, endpoint, key, fingerprint, IdempotencyInFlightLease.Seconds())
		if err != nil {
			return fmt.Errorf("claim idempotency key: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var r IdempotencyRecord
		if err := tx.QueryRow(ctx, `SELECT request_fingerprint, response_status, response_body, created_at
			FROM idempotency_keys WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3`,
			tenantID, endpoint, key).Scan(&r.RequestFingerprint, &r.ResponseStatus, &r.ResponseBody, &r.CreatedAt); err != nil {
			return fmt.Errorf("read idempotency key: %w", err)
		}
		if r.RequestFingerprint != fingerprint {
			return ErrIdempotencyFingerprintMismatch
		}
		rec = &r
		return nil
	})
	return rec, err
}

// CompleteIdempotencyKey stores the terminal answer for replay.
func (s *PgStore) CompleteIdempotencyKey(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	if len(body) == 0 {
		body = []byte("null")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE idempotency_keys SET response_status = $1, response_body = $2
			WHERE tenant_id = $3 AND endpoint = $4 AND idempotency_key = $5`, status, body, tenantID, endpoint, key)
		return err
	})
}

// ReleaseIdempotencyKey drops an unanswered claim so a retry genuinely retries.
func (s *PgStore) ReleaseIdempotencyKey(ctx context.Context, tenantID, endpoint, key string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM idempotency_keys
			WHERE tenant_id = $1 AND endpoint = $2 AND idempotency_key = $3 AND response_status = 0`, tenantID, endpoint, key)
		return err
	})
}
