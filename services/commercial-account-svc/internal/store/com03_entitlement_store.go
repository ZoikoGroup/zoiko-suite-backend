// COM-03 Entitlement persistence (migration 000010).
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/outbox"
)

// EntitlementStore is the COM-03 persistence contract. Every decision is
// computed fresh from immutable inputs; nothing here caches a grant.
type EntitlementStore interface {
	EvaluateCapability(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error)
	GetEffectiveEntitlements(ctx context.Context, now time.Time) ([]domain.CapabilityDecision, error)
	GetLimit(ctx context.Context, capabilityKey string, now time.Time) (*domain.CapabilityDecision, error)
	ExplainDecision(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error)
	RecomputeEntitlements(ctx context.Context, actor string, now time.Time) ([]domain.CapabilityDecision, error)

	ApplyRestriction(ctx context.Context, r *domain.CommercialRestriction, claim domain.IdempotencyClaim) (*domain.CommercialRestriction, error)
	RemoveRestriction(ctx context.Context, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error)
	ListRestrictions(ctx context.Context) ([]domain.CommercialRestriction, error)

	PublishEntitlementPolicy(ctx context.Context, p *domain.EntitlementPolicyVersion, claim domain.IdempotencyClaim) (*domain.EntitlementPolicyVersion, error)
	GetEntitlementPolicy(ctx context.Context, now time.Time) (*domain.EntitlementPolicyVersion, error)
}

var _ EntitlementStore = (*PgStore)(nil)

// loadEntitlementBasis reads the organization's subscription state and
// bound capabilities as of now, from the caller's tenant-scoped transaction.
// A tenant sees only its own subscription (RLS); the seller-plane price
// version read behind it sees only published/retired content either way.
func loadEntitlementBasis(ctx context.Context, tx pgx.Tx, now time.Time) (domain.SubscriptionEntitlementBasis, error) {
	rows, err := tx.Query(ctx, `SELECT subscription_id FROM subscriptions WHERE starts_at <= $1 AND (ends_at IS NULL OR ends_at > $1)`, now)
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	if len(ids) == 0 {
		// No subscription live at now: could be none ever, or one that has
		// already ended. Distinguish for the ended-grace calculation.
		return loadMostRecentlyEnded(ctx, tx, now)
	}

	a, err := loadAggregate(ctx, tx, ids[0], false)
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	cur := effectiveAt(a.versions, now)
	if cur == nil {
		return domain.SubscriptionEntitlementBasis{}, nil
	}
	b := domain.SubscriptionEntitlementBasis{SubscriptionID: a.sub.SubscriptionID, Status: cur.LifecycleStatus,
		Capabilities: map[string]domain.PlanCapability{}}
	if cur.LifecycleStatus.Terminal() {
		t := cur.EffectiveFrom
		b.EndedAt = &t
	}
	for _, it := range cur.Items {
		pv := a.pvs[it.PriceVersionID]
		if pv == nil {
			continue
		}
		for _, c := range pv.Capabilities {
			b.Capabilities[c.CapabilityKey] = c
		}
	}
	return b, nil
}

// loadMostRecentlyEnded finds an organization's most recently ended
// subscription so its grace period can still be evaluated. An organization
// that never subscribed returns the zero basis (Status == "": DENY).
func loadMostRecentlyEnded(ctx context.Context, tx pgx.Tx, now time.Time) (domain.SubscriptionEntitlementBasis, error) {
	var subID string
	err := tx.QueryRow(ctx, `SELECT subscription_id FROM subscriptions WHERE ends_at IS NOT NULL AND ends_at <= $1
		ORDER BY ends_at DESC LIMIT 1`, now).Scan(&subID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SubscriptionEntitlementBasis{}, nil
	}
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	a, err := loadAggregate(ctx, tx, subID, false)
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	cur := effectiveAt(a.versions, now)
	if cur == nil || !cur.LifecycleStatus.Terminal() {
		// A version scheduled after ends_at that isn't terminal would be a
		// data inconsistency; treat conservatively as no active basis.
		return domain.SubscriptionEntitlementBasis{}, nil
	}
	t := cur.EffectiveFrom
	return domain.SubscriptionEntitlementBasis{SubscriptionID: a.sub.SubscriptionID, Status: cur.LifecycleStatus, EndedAt: &t,
		Capabilities: map[string]domain.PlanCapability{}}, nil
}

func loadActiveRestrictions(ctx context.Context, tx pgx.Tx) ([]domain.CommercialRestriction, error) {
	rows, err := tx.Query(ctx, `
		SELECT restriction_id, organization_id::text, level, reason_code, policy_ref, basis_ref, applied_at,
		       applied_by_principal_id, lifted_at, lifted_by_principal_id, lift_reason
		FROM commercial_restrictions ORDER BY applied_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CommercialRestriction
	for rows.Next() {
		var r domain.CommercialRestriction
		if err := rows.Scan(&r.RestrictionID, &r.OrganizationID, &r.Level, &r.ReasonCode, &r.PolicyRef, &r.BasisRef,
			&r.AppliedAt, &r.AppliedByPrincipalID, &r.LiftedAt, &r.LiftedByPrincipalID, &r.LiftReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadEntitlementPolicyAt(ctx context.Context, tx pgx.Tx, now time.Time) (*domain.EntitlementPolicyVersion, error) {
	var p domain.EntitlementPolicyVersion
	err := tx.QueryRow(ctx, `
		SELECT policy_version, ended_outcome, ended_read_only_days, effective_from, reason, created_at, created_by_principal_id
		FROM entitlement_policy_versions WHERE effective_from <= $1 ORDER BY effective_from DESC, policy_version DESC LIMIT 1`, now).
		Scan(&p.PolicyVersion, &p.EndedOutcome, &p.EndedReadOnlyDays, &p.EffectiveFrom, &p.Reason, &p.CreatedAt, &p.CreatedByPrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

func (s *PgStore) decide(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error) {
	var out *domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		basis, err := loadEntitlementBasis(ctx, tx, now)
		if err != nil {
			return err
		}
		restrictions, err := loadActiveRestrictions(ctx, tx)
		if err != nil {
			return err
		}
		policy, err := loadEntitlementPolicyAt(ctx, tx, now)
		if err != nil {
			return err
		}
		d := domain.EvaluateCapability(capabilityKey, basis, restrictions, policy, requestedQuantity, now)
		out = &d
		return nil
	})
	return out, err
}

func (s *PgStore) EvaluateCapability(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error) {
	return s.decide(ctx, capabilityKey, requestedQuantity, now)
}

func (s *PgStore) GetLimit(ctx context.Context, capabilityKey string, now time.Time) (*domain.CapabilityDecision, error) {
	return s.decide(ctx, capabilityKey, nil, now)
}

func (s *PgStore) ExplainDecision(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error) {
	return s.decide(ctx, capabilityKey, requestedQuantity, now)
}

func (s *PgStore) GetEffectiveEntitlements(ctx context.Context, now time.Time) ([]domain.CapabilityDecision, error) {
	var out []domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		basis, err := loadEntitlementBasis(ctx, tx, now)
		if err != nil {
			return err
		}
		restrictions, err := loadActiveRestrictions(ctx, tx)
		if err != nil {
			return err
		}
		policy, err := loadEntitlementPolicyAt(ctx, tx, now)
		if err != nil {
			return err
		}
		out = domain.EffectiveEntitlements(basis, restrictions, policy, now)
		return nil
	})
	return out, err
}

// RecomputeEntitlements is the explicit cache-invalidation signal (§4.3):
// EntitlementSnapshotChanged, carrying the freshly computed result, so a
// cache-fronted caller has something concrete to invalidate against. The
// decision itself is unaffected by calling this — it is always fresh.
func (s *PgStore) RecomputeEntitlements(ctx context.Context, actor string, now time.Time) ([]domain.CapabilityDecision, error) {
	ds, err := s.GetEffectiveEntitlements(ctx, now)
	if err != nil {
		return nil, err
	}
	org := svcmiddleware.TenantFromContext(ctx)
	_ = s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "commercial_entitlement", AggregateID: org,
			EventType: "entitlement_snapshot.changed", TenantID: &org,
			Payload: map[string]any{"organization_id": org, "decisions": ds, "actor_id": actor, "occurred_at": now.UTC()}})
	})
	return ds, err
}

// ── Restrictions ─────────────────────────────────────────────────────────────

const restrictionColumns = `restriction_id, organization_id::text, level, reason_code, policy_ref, basis_ref, applied_at,
	applied_by_principal_id, lifted_at, lifted_by_principal_id, lift_reason`

func scanRestriction(row pgx.Row) (*domain.CommercialRestriction, error) {
	var r domain.CommercialRestriction
	if err := row.Scan(&r.RestrictionID, &r.OrganizationID, &r.Level, &r.ReasonCode, &r.PolicyRef, &r.BasisRef, &r.AppliedAt,
		&r.AppliedByPrincipalID, &r.LiftedAt, &r.LiftedByPrincipalID, &r.LiftReason); err != nil {
		return nil, err
	}
	return &r, nil
}

func restrictionEvent(ctx context.Context, tx pgx.Tx, eventType string, r *domain.CommercialRestriction) error {
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "commercial_restriction", AggregateID: r.RestrictionID,
		EventType: eventType, TenantID: &r.OrganizationID, Payload: r})
}

// ApplyRestriction lowers an organization's access under a named policy.
func (s *PgStore) ApplyRestriction(ctx context.Context, r *domain.CommercialRestriction, claim domain.IdempotencyClaim) (*domain.CommercialRestriction, error) {
	var out *domain.CommercialRestriction
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanRestriction(tx.QueryRow(ctx, `
			INSERT INTO commercial_restrictions (restriction_id, organization_id, level, reason_code, policy_ref, basis_ref,
				applied_at, applied_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+restrictionColumns,
			r.RestrictionID, r.OrganizationID, r.Level, r.ReasonCode, r.PolicyRef, r.BasisRef, r.AppliedAt, r.AppliedByPrincipalID))
		if err != nil {
			return err
		}
		out = got
		eventType := "entitlement.capability_restricted"
		if got.Level == domain.RestrictionReadOnly {
			eventType = "entitlement.grace_period_started"
		}
		return restrictionEvent(ctx, tx, eventType, got)
	})
	return out, err
}

// RemoveRestriction lifts a restriction. Naturally idempotent in effect —
// lifted stays lifted — so a repeat is refused, not replayed.
func (s *PgStore) RemoveRestriction(ctx context.Context, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error) {
	var out *domain.CommercialRestriction
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		cur, err := scanRestriction(tx.QueryRow(ctx, `SELECT `+restrictionColumns+` FROM commercial_restrictions
			WHERE restriction_id = $1 FOR UPDATE`, restrictionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRestrictionNotFound
		}
		if err != nil {
			return err
		}
		if cur.LiftedAt != nil {
			return domain.ErrRestrictionAlreadyLifted
		}
		if _, err := tx.Exec(ctx, `UPDATE commercial_restrictions SET lifted_at = $2, lifted_by_principal_id = $3, lift_reason = $4
			WHERE restriction_id = $1`, restrictionID, now, actor, reason); err != nil {
			return err
		}
		got, err := scanRestriction(tx.QueryRow(ctx, `SELECT `+restrictionColumns+` FROM commercial_restrictions WHERE restriction_id = $1`, restrictionID))
		if err != nil {
			return err
		}
		out = got
		eventType := "entitlement.capability_revoked"
		if got.Level == domain.RestrictionReadOnly {
			eventType = "entitlement.grace_period_ended"
		}
		return restrictionEvent(ctx, tx, eventType, got)
	})
	return out, err
}

func (s *PgStore) ListRestrictions(ctx context.Context) ([]domain.CommercialRestriction, error) {
	var out []domain.CommercialRestriction
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+restrictionColumns+` FROM commercial_restrictions ORDER BY applied_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRestriction(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// ── Ended-access policy ──────────────────────────────────────────────────────

func (s *PgStore) PublishEntitlementPolicy(ctx context.Context, p *domain.EntitlementPolicyVersion, claim domain.IdempotencyClaim) (*domain.EntitlementPolicyVersion, error) {
	var out *domain.EntitlementPolicyVersion
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(policy_version), 0) + 1 FROM entitlement_policy_versions`).Scan(&next); err != nil {
			return err
		}
		p.PolicyVersion = next
		if _, err := tx.Exec(ctx, `
			INSERT INTO entitlement_policy_versions (policy_version, ended_outcome, ended_read_only_days, effective_from,
				reason, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.PolicyVersion, p.EndedOutcome, p.EndedReadOnlyDays, p.EffectiveFrom, p.Reason, p.CreatedAt, p.CreatedByPrincipalID); err != nil {
			return err
		}
		out = p
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "entitlement_policy", AggregateID: "grace-policy",
			EventType: "entitlement_policy.published", Payload: p})
	})
	return out, err
}

func (s *PgStore) GetEntitlementPolicy(ctx context.Context, now time.Time) (*domain.EntitlementPolicyVersion, error) {
	var out *domain.EntitlementPolicyVersion
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		p, err := loadEntitlementPolicyAt(ctx, tx, now)
		out = p
		return err
	})
	return out, err
}
