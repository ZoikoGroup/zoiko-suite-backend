// COM-03 Entitlement persistence (migration 000010).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/money"
)

// EntitlementStore is the COM-03 persistence contract. Every decision is
// computed fresh from immutable inputs; nothing here caches a grant.
type EntitlementStore interface {
	EvaluateCapability(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error)
	GetEffectiveEntitlements(ctx context.Context, now time.Time) ([]domain.CapabilityDecision, error)
	GetLimit(ctx context.Context, capabilityKey string, now time.Time) (*domain.CapabilityDecision, error)
	ExplainDecision(ctx context.Context, capabilityKey string, requestedQuantity *int64, now time.Time) (*domain.CapabilityDecision, error)
	RecomputeEntitlements(ctx context.Context, actor string, now time.Time) ([]domain.CapabilityDecision, error)
	CreateSnapshot(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim, snapshotBatchID string) ([]domain.CapabilityDecision, error)
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
func loadEntitlementBasis(ctx context.Context, tx pgx.Tx, org string, now time.Time) (domain.SubscriptionEntitlementBasis, error) {
	rows, err := tx.Query(ctx, `SELECT subscription_id FROM subscriptions
		WHERE organization_id = $1 AND starts_at <= $2 AND (ends_at IS NULL OR ends_at > $2)`, org, now)
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
		return loadMostRecentlyEnded(ctx, tx, org, now)
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
	consumption, err := currentPeriodConsumptions(ctx, tx, a.sub.SubscriptionID, b.Capabilities, now)
	if err != nil {
		return domain.SubscriptionEntitlementBasis{}, err
	}
	if len(consumption) > 0 {
		b.Consumption = consumption
	}
	return b, nil
}

// currentPeriodConsumptions is the batched form of the quota read: one term
// lookup, one statement read for every metered capability, one meter read,
// and one accepted-event read. The earlier per-capability implementation
// issued four queries per metered capability (an N+1 on the runtime decision
// path); this keeps the same live domain.Aggregate semantics without the
// loop. A CERTIFIED/ADJUSTED statement's total_quantity is authoritative; an
// OPEN/FROZEN statement is aggregated from its accepted rows, exactly as
// ExplainAggregation/certifyStatement do.
func currentPeriodConsumptions(ctx context.Context, tx pgx.Tx, subscriptionID string, caps map[string]domain.PlanCapability, now time.Time) (map[string]int64, error) {
	type meterCap struct {
		keys []string
	}
	meters := map[string]*meterCap{}
	for key, c := range caps {
		if c.MeterKey == nil || c.MeterVersion == nil {
			continue
		}
		mc := meters[*c.MeterKey]
		if mc == nil {
			mc = &meterCap{}
			meters[*c.MeterKey] = mc
		}
		mc.keys = append(mc.keys, key)
	}
	if len(meters) == 0 {
		return nil, nil
	}
	meterKeys := make([]string, 0, len(meters))
	for key := range meters {
		meterKeys = append(meterKeys, key)
	}

	termNo, err := resolveTerm(ctx, tx, subscriptionID, now)
	if err != nil {
		if errors.Is(err, domain.ErrStatementNotOpenForWindow) {
			return nil, nil // no term covers this instant: nothing to have consumed
		}
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+usageStatementColumns+` FROM usage_statements
		WHERE subscription_id = $1 AND term_no = $2 AND meter_key = ANY($3) AND status <> 'SUPERSEDED'`,
		subscriptionID, termNo, meterKeys)
	if err != nil {
		return nil, err
	}
	statements := map[string]*domain.UsageStatement{}
	for rows.Next() {
		st, err := scanStatement(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		statements[st.MeterKey] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(statements) == 0 {
		return nil, nil
	}

	needMeters := make([]string, 0, len(statements))
	needStatements := make([]string, 0, len(statements))
	for key, st := range statements {
		if st.Status == domain.StatementCertified || st.Status == domain.StatementAdjusted {
			continue
		}
		needMeters = append(needMeters, key)
		needStatements = append(needStatements, st.StatementID)
	}
	defs := map[string]*domain.MeterDefinition{}
	if len(needMeters) > 0 {
		rows, err := tx.Query(ctx, `SELECT `+meterDefinitionColumns+` FROM meter_definitions WHERE meter_key = ANY($1)`, needMeters)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMeterDefinition(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			defs[m.MeterKey+":"+strconv.Itoa(m.MeterVersion)] = m
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	events := map[string][]domain.AcceptedEvent{}
	if len(needStatements) > 0 {
		rows, err := tx.Query(ctx, `SELECT statement_id, usage_event_id, quantity::text, dimensions, occurred_at
			FROM usage_event_records WHERE statement_id = ANY($1) AND status = 'ACCEPTED'
			ORDER BY statement_id, usage_event_id`, needStatements)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var statementID string
			var e domain.AcceptedEvent
			var dimJSON []byte
			if err := rows.Scan(&statementID, &e.EventID, &e.Quantity, &dimJSON, &e.OccurredAt); err != nil {
				rows.Close()
				return nil, err
			}
			if len(dimJSON) > 0 {
				if err := json.Unmarshal(dimJSON, &e.Dimensions); err != nil {
					rows.Close()
					return nil, fmt.Errorf("decode usage dimensions: %w", err)
				}
			}
			events[statementID] = append(events[statementID], e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	out := map[string]int64{}
	for meterKey, mc := range meters {
		st := statements[meterKey]
		if st == nil {
			continue
		}
		var used int64
		if st.Status == domain.StatementCertified || st.Status == domain.StatementAdjusted {
			used, err = decimalToInt64Ceil(st.TotalQuantity)
		} else {
			m := defs[meterKey+":"+strconv.Itoa(st.MeterVersion)]
			if m == nil {
				return nil, domain.ErrMeterDefinitionNotFound
			}
			used, err = decimalToInt64Ceil(domain.Aggregate(*m, events[st.StatementID]))
		}
		if err != nil {
			return nil, err
		}
		for _, key := range mc.keys {
			out[key] = used
		}
	}
	return out, nil
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
func loadMostRecentlyEnded(ctx context.Context, tx pgx.Tx, org string, now time.Time) (domain.SubscriptionEntitlementBasis, error) {
	var subID string
	err := tx.QueryRow(ctx, `SELECT subscription_id FROM subscriptions
		WHERE organization_id = $1 AND ends_at IS NOT NULL AND ends_at <= $2
		ORDER BY ends_at DESC LIMIT 1`, org, now).Scan(&subID)
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

func loadActiveRestrictions(ctx context.Context, tx pgx.Tx, org string) ([]domain.CommercialRestriction, error) {
	rows, err := tx.Query(ctx, `
		SELECT restriction_id, organization_id::text, level, reason_code, policy_ref, basis_ref, applied_at,
		       applied_by_principal_id, lifted_at, lifted_by_principal_id, lift_reason
		FROM commercial_restrictions WHERE organization_id = $1 ORDER BY applied_at`, org)
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
	org := svcmiddleware.TenantFromContext(ctx)
	var out *domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		basis, err := loadEntitlementBasis(ctx, tx, org, now)
		if err != nil {
			return err
		}
		restrictions, err := loadActiveRestrictions(ctx, tx, org)
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
	org := svcmiddleware.TenantFromContext(ctx)
	var out []domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		basis, err := loadEntitlementBasis(ctx, tx, org, now)
		if err != nil {
			return err
		}
		restrictions, err := loadActiveRestrictions(ctx, tx, org)
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
func (s *PgStore) recomputeEntitlementsTx(ctx context.Context, tx pgx.Tx, org, actor string, now time.Time) ([]domain.CapabilityDecision, error) {
	return s.recomputeEntitlementsBatchTx(ctx, tx, org, actor, now, "")
}

// recomputeEntitlementsBatchTx is recomputeEntitlementsTx's body, with an
// optional snapshotBatchID: when non-empty (CreateSnapshot only), every
// entitlement_snapshots row this call writes is tagged with it, so a
// replay can look the exact original decisions back up by that id (A6)
// instead of recomputing fresh, which can legitimately differ from the
// original if state changed between the original call and the replay.
func (s *PgStore) recomputeEntitlementsBatchTx(ctx context.Context, tx pgx.Tx, org, actor string, now time.Time, snapshotBatchID string) ([]domain.CapabilityDecision, error) {
	basis, err := loadEntitlementBasis(ctx, tx, org, now)
	if err != nil {
		return nil, err
	}
	restrictions, err := loadActiveRestrictions(ctx, tx, org)
	if err != nil {
		return nil, err
	}
	policy, err := loadEntitlementPolicyAt(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	ds := domain.EffectiveEntitlements(basis, restrictions, policy, now)
	if err := s.emit(ctx, tx, "entitlement_snapshot.changed", "commercial_entitlement", org, org, actor, map[string]any{"organization_id": org, "decisions": ds, "actor_id": actor, "occurred_at": now.UTC()}); err != nil {
		return nil, err
	}
	for _, d := range ds {
		if err := insertEntitlementSnapshot(ctx, tx, org, actor, now, d, snapshotBatchID); err != nil {
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
		ds, err := s.recomputeEntitlementsTx(ctx, tx, org, actor, now)
		out = ds
		return err
	})
	return out, err
}

// CreateSnapshot is the explicit, delegated-authority "capture evidence of
// this organization's entitlement right now" command (§4.3) — the same
// recompute-and-record path RecomputeEntitlements uses, exposed under the
// doc's own name for an operator/support workflow.
//
// snapshotBatchID is generated by the handler (so it is stable across a
// retry under the same Idempotency-Key, carried as the claim's own resource
// id) and tags every entitlement_snapshots row this call writes — the
// faithful-replay lookup key for GetEntitlementSnapshotBatch (A6).
func (s *PgStore) CreateSnapshot(ctx context.Context, organizationID, actor string, now time.Time, claim domain.IdempotencyClaim, snapshotBatchID string) ([]domain.CapabilityDecision, error) {
	var out []domain.CapabilityDecision
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if err := declareOrgForSellerPlane(ctx, tx, organizationID); err != nil {
			return err
		}
		ds, err := s.recomputeEntitlementsBatchTx(ctx, tx, organizationID, actor, now, snapshotBatchID)
		out = ds
		return err
	})
	return out, err
}

// GetEntitlementSnapshotBatch is CreateSnapshot's replay reader (A6, B5/B6's
// shape): the exact decisions a specific original CreateSnapshot call wrote,
// looked up by the batch id tagged on each row — never recomputed, so a
// replay under the same idempotency key is faithful even if the
// organization's effective entitlements have genuinely changed since.
func (s *PgStore) GetEntitlementSnapshotBatch(ctx context.Context, organizationID, snapshotBatchID string) ([]domain.CapabilityDecision, error) {
	var out []domain.CapabilityDecision
	err := s.subscriptionTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+entitlementSnapshotColumns+` FROM entitlement_snapshots
			WHERE organization_id = $1 AND snapshot_batch_id = $2 ORDER BY capability_key`, organizationID, snapshotBatchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanEntitlementSnapshot(rows)
			if err != nil {
				return err
			}
			out = append(out, domain.CapabilityDecision{
				CapabilityKey: s.CapabilityKey, Outcome: s.Outcome, LimitValue: s.LimitValue, LimitUnit: s.LimitUnit,
				SubscriptionOutcome: s.SubscriptionOutcome, RestrictionOutcome: s.RestrictionOutcome,
				AppliedRestrictionID: s.AppliedRestrictionID, PolicyVersion: s.PolicyVersion, SubscriptionID: s.SubscriptionID,
				Reason: s.Reason, DecidedAt: s.DecidedAt,
			})
		}
		return rows.Err()
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
func insertEntitlementSnapshot(ctx context.Context, tx pgx.Tx, org, actor string, now time.Time, d domain.CapabilityDecision, snapshotBatchID string) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'seller', true)"); err != nil {
		return err
	}
	id := domain.NewCommercialID(domain.PrefixEntitlementSnapshot)
	var batchID *string
	if snapshotBatchID != "" {
		batchID = &snapshotBatchID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO entitlement_snapshots (snapshot_id, organization_id, capability_key, outcome, limit_value, limit_unit,
			subscription_outcome, restriction_outcome, applied_restriction_id, policy_version, subscription_id, reason,
			decided_at, created_at, created_by_principal_id, snapshot_batch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		id, org, d.CapabilityKey, d.Outcome, d.LimitValue, d.LimitUnit, d.SubscriptionOutcome, d.RestrictionOutcome,
		d.AppliedRestrictionID, d.PolicyVersion, d.SubscriptionID, d.Reason, d.DecidedAt, now, actor, batchID)
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

func (s *PgStore) restrictionEvent(ctx context.Context, tx pgx.Tx, eventType string, r *domain.CommercialRestriction) error {
	return s.emit(ctx, tx, eventType, "commercial_restriction", r.RestrictionID, r.OrganizationID, "", r)
}

// applyRestrictionTx is ApplyRestriction's body, extracted so a caller that
// has already opened its own seller-plane transaction (B3: a dunning case
// escalating to RESTRICTED/SUSPENDED) can apply a restriction atomically
// with whatever triggered it, instead of opening a second, separate
// transaction that could commit out of step with the first.
func (s *PgStore) applyRestrictionTx(ctx context.Context, tx pgx.Tx, r *domain.CommercialRestriction) (*domain.CommercialRestriction, error) {
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
	if err := s.restrictionEvent(ctx, tx, eventType, got); err != nil {
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
		got, err := s.applyRestrictionTx(ctx, tx, r)
		if err != nil {
			return err
		}
		// COM-03 §4.3: a restriction changes the snapshot. Record the new
		// decisions and emit EntitlementSnapshotChanged in the same
		// transaction, so a cache-fronted consumer cannot keep an Allow
		// decision after the restriction commits.
		if err := declareOrgForSellerPlane(ctx, tx, r.OrganizationID); err != nil {
			return err
		}
		if _, err := s.recomputeEntitlementsTx(ctx, tx, r.OrganizationID, r.AppliedByPrincipalID, r.AppliedAt); err != nil {
			return err
		}
		out = got
		return nil
	})
	return out, err
}

// removeRestrictionTx is RemoveRestriction's body, extracted for the same
// reason as applyRestrictionTx (B3: StopDunning lifting the restriction it
// applied, atomically with closing the case).
func (s *PgStore) removeRestrictionTx(ctx context.Context, tx pgx.Tx, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error) {
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
	if err := s.restrictionEvent(ctx, tx, eventType, got); err != nil {
		return nil, err
	}
	return got, nil
}

// RemoveRestriction lifts a restriction. Naturally idempotent in effect —
// lifted stays lifted — so a repeat is refused, not replayed.
func (s *PgStore) RemoveRestriction(ctx context.Context, restrictionID, actor, reason string, now time.Time) (*domain.CommercialRestriction, error) {
	var out *domain.CommercialRestriction
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		got, err := s.removeRestrictionTx(ctx, tx, restrictionID, actor, reason, now)
		if err != nil {
			return err
		}
		if err := declareOrgForSellerPlane(ctx, tx, got.OrganizationID); err != nil {
			return err
		}
		if _, err := s.recomputeEntitlementsTx(ctx, tx, got.OrganizationID, actor, now); err != nil {
			return err
		}
		out = got
		return nil
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

// PublishEntitlementPolicy claims the idempotency key under the resource id
// of the policy_version this call actually reserves, not a static "there is
// only one policy resource" constant — so a replay (A6) can look up the
// exact version the ORIGINAL call created via GetEntitlementPolicyByVersion,
// rather than GetEntitlementPolicy(now), which answers "what's effective
// right now" and silently differs from the original result once a
// future-dated policy actually becomes effective, or once a later version is
// published.
//
// This only works because claimIdempotency's ON CONFLICT DO NOTHING means a
// retry's locally (re)computed `next` is never persisted — on conflict it
// always returns the ORIGINAL call's stored resource_id untouched, so a
// retry safely recomputing a different (now-stale) `next` cannot corrupt the
// replay target.
func (s *PgStore) PublishEntitlementPolicy(ctx context.Context, p *domain.EntitlementPolicyVersion, claim domain.IdempotencyClaim) (*domain.EntitlementPolicyVersion, error) {
	var out *domain.EntitlementPolicyVersion
	err := s.withSellerPlane(ctx, func(tx pgx.Tx) error {
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(policy_version), 0) + 1 FROM entitlement_policy_versions`).Scan(&next); err != nil {
			return err
		}
		claim.ResourceID = strconv.Itoa(next)
		if err := claimIdempotency(ctx, tx, claim); err != nil {
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
		// A5: enqueue the real per-tenant invalidation signal for exactly the
		// organizations this policy version can actually change the answer
		// for — those with no live subscription whose most recent one has
		// already ended at or before this policy's own effective_from. See
		// migration 000024 for why ended_subscription_index (never
		// `subscriptions` itself) is the safe read here.
		if err := enqueueEntitlementInvalidations(ctx, tx, p.PolicyVersion, p.EffectiveFrom); err != nil {
			return err
		}
		out = p
		return s.emit(ctx, tx, "entitlement_policy.published", "entitlement_policy", "grace-policy", PlatformScopeID, claim.PrincipalID, p)
	})
	return out, err
}

// GetEntitlementPolicyByVersion is PublishEntitlementPolicy's replay reader
// (A6): the exact version a specific original call created, looked up by
// its own policy_version — never "whichever version happens to be
// effective now", which is what GetEntitlementPolicy(now) answers and what
// a stale cache-fronted replay previously got back instead of its own
// original result.
func (s *PgStore) GetEntitlementPolicyByVersion(ctx context.Context, policyVersion int) (*domain.EntitlementPolicyVersion, error) {
	var out *domain.EntitlementPolicyVersion
	err := s.catalogTx(ctx, false, func(tx pgx.Tx) error {
		var p domain.EntitlementPolicyVersion
		err := tx.QueryRow(ctx, `SELECT policy_version, ended_outcome, ended_read_only_days, effective_from, reason,
			created_at, created_by_principal_id FROM entitlement_policy_versions WHERE policy_version = $1`, policyVersion).
			Scan(&p.PolicyVersion, &p.EndedOutcome, &p.EndedReadOnlyDays, &p.EffectiveFrom, &p.Reason, &p.CreatedAt, &p.CreatedByPrincipalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &p
		return nil
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

// ── Entitlement invalidation queue (A5) ──────────────────────────────────────
//
// PublishEntitlementPolicy enqueues one row per organization its new policy
// version can actually change the answer for (migration 000024); a worker
// shaped exactly like subscription_boundary_queue's ProcessNextBoundary
// drains the queue and turns each row into the real per-tenant
// entitlement_snapshot.changed event through the existing
// recomputeEntitlementsBatchTx path — the same event CreateSnapshot,
// RecomputeEntitlements and the boundary worker's own B2 remediation already
// produce, not a new one-off event shape invented for this gap.

// enqueueEntitlementInvalidations reads ended_subscription_index — never
// `subscriptions` itself, which has no seller-plane bypass — for every
// organization whose most recently ended subscription's ends_at is at or
// before this policy version's own effective_from, and queues one
// invalidation per organization. Called inside PublishEntitlementPolicy's own
// transaction, so the enqueue has the same durability as the policy write
// and the same idempotency guard (a replay never reaches this code: it
// returns via claimIdempotency before the policy row, and so this call, is
// ever reached a second time).
func enqueueEntitlementInvalidations(ctx context.Context, tx pgx.Tx, policyVersion int, effectiveFrom time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO entitlement_invalidation_queue (organization_id, policy_version, due_at, next_attempt_at)
		SELECT organization_id, $1, $2, $2
		  FROM ended_subscription_index
		 WHERE ends_at <= $2
		ON CONFLICT (organization_id, policy_version) DO NOTHING`,
		policyVersion, effectiveFrom)
	return err
}

const (
	entitlementInvalidationWorkerActor = "system:entitlement-invalidation-worker"
	entitlementInvalidationMaxAttempts = 10
)

type entitlementInvalidationItem struct {
	id            string
	org           string
	policyVersion int
	attempts      int
}

// entitlementInvalidationBackoff doubles from one minute up to an hour, the
// same shape as boundaryBackoff.
func entitlementInvalidationBackoff(attempts int) time.Duration {
	d := time.Minute << attempts
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

// ProcessNextEntitlementInvalidation claims one due entitlement_invalidation_queue
// row and processes it, all in one transaction: SKIP LOCKED lets any number
// of workers run side by side without two ever taking the same item, the
// same shape as ProcessNextBoundary. It reports whether an item was found; a
// failure is recorded against the item with a backoff (and parked as FAILED
// after entitlementInvalidationMaxAttempts) so one bad item never blocks the
// queue behind it.
func (s *PgStore) ProcessNextEntitlementInvalidation(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return false, err
	}
	var it entitlementInvalidationItem
	err = tx.QueryRow(ctx, `
		SELECT invalidation_id::text, organization_id::text, policy_version, attempts
		FROM entitlement_invalidation_queue
		WHERE status = 'PENDING' AND next_attempt_at <= $1
		ORDER BY next_attempt_at, invalidation_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&it.id, &it.org, &it.policyVersion, &it.attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// declareOrgForSellerPlane's own mechanism: this organization is the only
	// one this transaction now needs, and RLS throughout (subscriptions,
	// commercial_restrictions, eventing_outbox) admits a boundary_worker-plane
	// transaction that has also declared this tenant.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", it.org); err != nil {
		return false, err
	}

	// The decision is always computed fresh (never cached), so recomputing
	// here is correct even if the organization's state moved on between
	// enqueue and processing (e.g. it started a new subscription since) —
	// recomputeEntitlementsBatchTx reloads the basis, restrictions and
	// effective policy at `now` itself; this call's only job is to make that
	// fresh decision durable and emit the real per-tenant signal.
	_, perr := s.recomputeEntitlementsTx(ctx, tx, it.org, entitlementInvalidationWorkerActor, now)
	if perr == nil {
		_, perr = tx.Exec(ctx, `UPDATE entitlement_invalidation_queue SET status = 'DONE', processed_at = $2, outcome = $3
			WHERE invalidation_id = $1::uuid`, it.id, now, "EVENT_PUBLISHED")
	}
	if perr == nil {
		if perr = tx.Commit(ctx); perr == nil {
			return true, nil
		}
	}
	_ = tx.Rollback(ctx)
	if rerr := s.recordEntitlementInvalidationFailure(ctx, it, perr, now); rerr != nil {
		return true, fmt.Errorf("entitlement invalidation %s failed (%v) and the failure could not be recorded: %w", it.id, perr, rerr)
	}
	return true, fmt.Errorf("entitlement invalidation %s: %w", it.id, perr)
}

func (s *PgStore) recordEntitlementInvalidationFailure(ctx context.Context, it entitlementInvalidationItem, cause error, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.commercial_plane', 'boundary_worker', true)"); err != nil {
		return err
	}
	attempts := it.attempts + 1
	if attempts >= entitlementInvalidationMaxAttempts {
		_, err = tx.Exec(ctx, `UPDATE entitlement_invalidation_queue SET attempts = $2, last_error = $3, status = 'FAILED',
			processed_at = $4, outcome = 'FAILED' WHERE invalidation_id = $1::uuid`, it.id, attempts, cause.Error(), now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE entitlement_invalidation_queue SET attempts = $2, last_error = $3, next_attempt_at = $4
			WHERE invalidation_id = $1::uuid`, it.id, attempts, cause.Error(), now.Add(entitlementInvalidationBackoff(attempts)))
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
