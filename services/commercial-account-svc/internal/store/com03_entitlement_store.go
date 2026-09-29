// COM-03 Entitlement persistence (migration 000010).
package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/money"
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
	CreateSnapshot(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim) ([]domain.CapabilityDecision, error)
	GetEntitlementHistory(ctx context.Context, organizationID, capabilityKey string, since, until time.Time) ([]domain.EntitlementSnapshot, error)

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
	for key, c := range b.Capabilities {
		if c.MeterKey == nil || c.MeterVersion == nil {
			continue
		}
		used, err := currentPeriodConsumption(ctx, tx, a.sub.SubscriptionID, *c.MeterKey, *c.MeterVersion, now)
		if err != nil {
			return domain.SubscriptionEntitlementBasis{}, err
		}
		if b.Consumption == nil {
			b.Consumption = map[string]int64{}
		}
		b.Consumption[key] = used
	}
	return b, nil
}

// currentPeriodConsumption is B1's gap fix: how much of a metered
// capability's quota has been consumed in the subscription's current term,
// as of now. Reuses COM-04's own term-resolution and live-aggregation
// machinery rather than inventing a second running total: a CERTIFIED/
// ADJUSTED statement's total_quantity is authoritative; an OPEN/FROZEN
// statement (the common case, since a quota check happens mid-term) has no
// live total_quantity by COM-04's own design ("no dashboard counter"), so
// this sums its accepted events live with the same domain.Aggregate
// ExplainAggregation/certifyStatement already use — a genuine on-demand
// read of the real accepted rows, not a second cached counter that could
// drift from them.
func currentPeriodConsumption(ctx context.Context, tx pgx.Tx, subscriptionID, meterKey string, meterVersion int, now time.Time) (int64, error) {
	termNo, err := resolveTerm(ctx, tx, subscriptionID, now)
	if err != nil {
		if errors.Is(err, domain.ErrStatementNotOpenForWindow) {
			return 0, nil // no term covers this instant: nothing to have consumed
		}
		return 0, err
	}
	st, err := scanStatement(tx.QueryRow(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND meter_key = $3 AND status <> 'SUPERSEDED'`, subscriptionID, termNo, meterKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if st.Status == domain.StatementCertified || st.Status == domain.StatementAdjusted {
		return decimalToInt64Ceil(st.TotalQuantity)
	}
	m, err := loadMeterDefinition(ctx, tx, meterKey, meterVersion)
	if err != nil {
		return 0, err
	}
	events, err := acceptedEvents(ctx, tx, st.StatementID)
	if err != nil {
		return 0, err
	}
	return decimalToInt64Ceil(domain.Aggregate(*m, events))
}

// decimalToInt64Ceil converts an exact decimal quantity to a whole-unit
// quota count, rounding up (in the platform's favor: never undercount what
// has been consumed).
func decimalToInt64Ceil(s string) (int64, error) {
	d, err := money.Parse(s)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(d.Ceil().String(), 10, 64)
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

// recomputeEntitlementsTx computes every capability decision for org from
// tx (which the caller must already have scoped to org via app.tenant_id),
// records an evidence snapshot of each one (B4), and emits
// entitlement_snapshot.changed — all in the caller's own transaction, so a
// boundary-driven subscription change or a dunning-driven restriction can
// never partially apply without its entitlement recompute, or vice versa.
// Every automatic invalidation touchpoint and the explicit
// RecomputeEntitlements command all go through this one function so they
// can never disagree about what "recompute" does.
//
// Previously RecomputeEntitlements computed the decisions in one
// transaction and wrote the outbox event in a second, separate one whose
// error was silently discarded (`_ = s.subscriptionTx(...)`) — a failure to
// record the event was invisible to the caller. Doing everything in the
// one transaction the public method already opens fixes that as a side
// effect of the extraction, not a separate change.
func recomputeEntitlementsTx(ctx context.Context, tx pgx.Tx, org, actor string, now time.Time) ([]domain.CapabilityDecision, error) {
	basis, err := loadEntitlementBasis(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	restrictions, err := loadActiveRestrictions(ctx, tx)
	if err != nil {
		return nil, err
	}
	policy, err := loadEntitlementPolicyAt(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	ds := domain.EffectiveEntitlements(basis, restrictions, policy, now)
	if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "commercial_entitlement", AggregateID: org,
		EventType: "entitlement_snapshot.changed", TenantID: &org,
		Payload: map[string]any{"organization_id": org, "decisions": ds, "actor_id": actor, "occurred_at": now.UTC()}}); err != nil {
		return nil, err
	}
	for _, d := range ds {
		if err := insertEntitlementSnapshot(ctx, tx, org, actor, now, d); err != nil {
			return nil, err
		}
	}
	return ds, nil
}

// RecomputeEntitlements is the explicit, operator-facing cache-invalidation
// signal (§4.3). The decision itself is unaffected by calling this — it is
// always fresh; this only makes the freshly computed result durable
// evidence and gives a cache-fronted caller something concrete to
// invalidate against.
func (s *PgStore) RecomputeEntitlements(ctx context.Context, actor string, now time.Time) ([]domain.CapabilityDecision, error) {
	org := svcmiddleware.TenantFromContext(ctx)
	var out []domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		ds, err := recomputeEntitlementsTx(ctx, tx, org, actor, now)
		out = ds
		return err
	})
	return out, err
}

// CreateSnapshot is the explicit, delegated-authority "capture evidence of
// this organization's entitlement right now" command (§4.3) — the same
// recompute-and-record path RecomputeEntitlements uses, exposed under the
// doc's own name for an operator/support workflow.
func (s *PgStore) CreateSnapshot(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim) ([]domain.CapabilityDecision, error) {
	var out []domain.CapabilityDecision
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := declareOrgForSellerPlane(ctx, tx, organizationID); err != nil {
			return err
		}
		ds, err := recomputeEntitlementsTx(ctx, tx, organizationID, actor, now)
		out = ds
		return err
	})
	return out, err
}

// GetEntitlementHistory answers "what was this organization's entitlement
// at each point it was recorded" from the append-only snapshot evidence —
// never from the live decision path, which has no history of its own.
func (s *PgStore) GetEntitlementHistory(ctx context.Context, organizationID, capabilityKey string, since, until time.Time) ([]domain.EntitlementSnapshot, error) {
	var out []domain.EntitlementSnapshot
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+entitlementSnapshotColumns+` FROM entitlement_snapshots
			WHERE organization_id = $1 AND capability_key = $2 AND created_at >= $3 AND created_at <= $4
			ORDER BY created_at`, organizationID, capabilityKey, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanEntitlementSnapshot(rows)
			if err != nil {
				return err
			}
			out = append(out, *s)
		}
		return rows.Err()
	})
	return out, err
}

// ── Entitlement snapshots (append-only evidence) ────────────────────────────

const entitlementSnapshotColumns = `snapshot_id, organization_id::text, capability_key, outcome, limit_value, limit_unit,
	subscription_outcome, restriction_outcome, applied_restriction_id, policy_version, subscription_id, reason,
	decided_at, created_at, created_by_principal_id`

func scanEntitlementSnapshot(row pgx.Row) (*domain.EntitlementSnapshot, error) {
	var s domain.EntitlementSnapshot
	if err := row.Scan(&s.SnapshotID, &s.OrganizationID, &s.CapabilityKey, &s.Outcome, &s.LimitValue, &s.LimitUnit,
		&s.SubscriptionOutcome, &s.RestrictionOutcome, &s.AppliedRestrictionID, &s.PolicyVersion, &s.SubscriptionID,
		&s.Reason, &s.DecidedAt, &s.CreatedAt, &s.CreatedByPrincipalID); err != nil {
		return nil, err
	}
	return &s, nil
}

// insertEntitlementSnapshot always writes under the seller plane
// regardless of the caller's own transaction scope (a tenant-scoped
// RecomputeEntitlements, a boundary-worker recompute, a dunning-driven
// one): the row is entirely server-computed evidence, never
// tenant-suppliable content, and entitlement_snapshots' own INSERT policy
// is platform/system-only (never a plain tenant fabricating its own
// evidence) — see migration 000016.
func insertEntitlementSnapshot(ctx context.Context, tx pgx.Tx, org, actor string, now time.Time, d domain.CapabilityDecision) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		return err
	}
	id := domain.NewCommercialID(domain.PrefixEntitlementSnapshot)
	_, err := tx.Exec(ctx, `
		INSERT INTO entitlement_snapshots (snapshot_id, organization_id, capability_key, outcome, limit_value, limit_unit,
			subscription_outcome, restriction_outcome, applied_restriction_id, policy_version, subscription_id, reason,
			decided_at, created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, org, d.CapabilityKey, d.Outcome, d.LimitValue, d.LimitUnit, d.SubscriptionOutcome, d.RestrictionOutcome,
		d.AppliedRestrictionID, d.PolicyVersion, d.SubscriptionID, d.Reason, d.DecidedAt, now, actor)
	return err
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

// applyRestrictionTx is ApplyRestriction's body, extracted so a caller that
// has already opened its own seller-plane transaction (B3: a dunning case
// escalating to RESTRICTED/SUSPENDED) can apply a restriction atomically
// with whatever triggered it, instead of opening a second, separate
// transaction that could commit out of step with the first.
func applyRestrictionTx(ctx context.Context, tx pgx.Tx, r *domain.CommercialRestriction) (*domain.CommercialRestriction, error) {
	got, err := scanRestriction(tx.QueryRow(ctx, `
		INSERT INTO commercial_restrictions (restriction_id, organization_id, level, reason_code, policy_ref, basis_ref,
			applied_at, applied_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+restrictionColumns,
		r.RestrictionID, r.OrganizationID, r.Level, r.ReasonCode, r.PolicyRef, r.BasisRef, r.AppliedAt, r.AppliedByPrincipalID))
	if err != nil {
		return nil, err
	}
	eventType := "entitlement.capability_restricted"
	if got.Level == domain.RestrictionReadOnly {
		eventType = "entitlement.grace_period_started"
	}
	if err := restrictionEvent(ctx, tx, eventType, got); err != nil {
		return nil, err
	}
	return got, nil
}

// ApplyRestriction lowers an organization's access under a named policy.
func (s *PgStore) ApplyRestriction(ctx context.Context, r *domain.CommercialRestriction, claim domain.IdempotencyClaim) (*domain.CommercialRestriction, error) {
	var out *domain.CommercialRestriction
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := applyRestrictionTx(ctx, tx, r)
		out = got
		return err
	})
	return out, err
}

// removeRestrictionTx is RemoveRestriction's body, extracted for the same
// reason as applyRestrictionTx (B3: StopDunning lifting the restriction it
// applied, atomically with closing the case).
func removeRestrictionTx(ctx context.Context, tx pgx.Tx, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error) {
	cur, err := scanRestriction(tx.QueryRow(ctx, `SELECT `+restrictionColumns+` FROM commercial_restrictions
		WHERE restriction_id = $1 FOR UPDATE`, restrictionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRestrictionNotFound
	}
	if err != nil {
		return nil, err
	}
	if cur.LiftedAt != nil {
		return nil, domain.ErrRestrictionAlreadyLifted
	}
	if _, err := tx.Exec(ctx, `UPDATE commercial_restrictions SET lifted_at = $2, lifted_by_principal_id = $3, lift_reason = $4
		WHERE restriction_id = $1`, restrictionID, now, actor, reason); err != nil {
		return nil, err
	}
	got, err := scanRestriction(tx.QueryRow(ctx, `SELECT `+restrictionColumns+` FROM commercial_restrictions WHERE restriction_id = $1`, restrictionID))
	if err != nil {
		return nil, err
	}
	eventType := "entitlement.capability_revoked"
	if got.Level == domain.RestrictionReadOnly {
		eventType = "entitlement.grace_period_ended"
	}
	if err := restrictionEvent(ctx, tx, eventType, got); err != nil {
		return nil, err
	}
	return got, nil
}

// RemoveRestriction lifts a restriction. Naturally idempotent in effect —
// lifted stays lifted — so a repeat is refused, not replayed.
func (s *PgStore) RemoveRestriction(ctx context.Context, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error) {
	var out *domain.CommercialRestriction
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		got, err := removeRestrictionTx(ctx, tx, restrictionID, actor, reason, now)
		out = got
		return err
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
