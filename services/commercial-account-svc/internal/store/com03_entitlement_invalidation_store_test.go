package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// A5 (SPEC_DEVIATIONS.md), real Postgres as the NOSUPERUSER NOBYPASSRLS
// role: PublishEntitlementPolicy must enqueue — and a drain of
// entitlement_invalidation_queue must emit — the real per-tenant
// entitlement_snapshot.changed signal for exactly the organizations a given
// policy version can actually change the answer for (those with no live
// subscription whose most recent one has already ended at or before the
// policy's own effective_from), never every organization in the system and
// never an organization whose subscription is still active.
func TestEntitlement_PublishPolicyInvalidatesOnlyGenuinelyEndedOrgs(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})

	// Org A (f.org): started, then genuinely ended (CancelNow) well before
	// the policy below publishes.
	subA := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	cancelAt := domain.AddBillingIntervals(day(11), "MONTH", 1, 1).Add(time.Hour)
	subA = f.sv(f.s.CancelNow(f.ctxOrg, f.cmd(subA, "customer-admin", cancelAt),
		f.tclaim(f.org, "customer-admin", "cancel-now", subA.SubscriptionID)))
	if subA.Status == nil || !subA.Status.Terminal() {
		t.Fatalf("org A subscription did not reach a terminal state: %+v", subA.Status)
	}

	// Org B: a second, genuinely unrelated organization with its own
	// commercial account and a subscription that is still active when the
	// policy publishes below — it must NOT be invalidated.
	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)
	acctB := &domain.CommercialAccount{CommercialAccountID: uuid.NewString(), OrganizationID: orgB, LegalCustomerName: "Acme B",
		BillingCurrencyCode: "USD", Status: domain.CommercialAccountStatusActive, CreatedAt: t0, CreatedByPrincipalID: "seed"}
	if err := f.s.CreateCommercialAccount(ctxB, acctB); err != nil {
		t.Fatalf("create org B account: %v", err)
	}
	if err := f.s.SetAccountMarket(ctxB, acctB.CommercialAccountID, "GB", operator, t0,
		f.tclaim(orgB, operator, "SetAccountMarket", acctB.CommercialAccountID)); err != nil {
		t.Fatalf("set org B market: %v", err)
	}
	startB := domain.StartSubscriptionParams{
		SubscriptionID: domain.NewCommercialID(domain.PrefixSubscription), ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange),
		CommercialAccountID: acctB.CommercialAccountID, PlanProductCode: "business", Quantities: map[string]string{},
		AcceptedTermsSHA256: termsHash, StartsAt: day(11), Channel: domain.ChannelSelfService, Actor: "customer-admin", Now: day(11),
	}
	subB, err := f.s.StartSubscription(ctxB, startB, f.tclaim(orgB, "customer-admin", "StartSubscription", startB.SubscriptionID))
	if err != nil {
		t.Fatalf("start org B subscription: %v", err)
	}
	basis := "first invoice paid"
	cmdB := domain.SubscriptionCommand{SubscriptionID: subB.SubscriptionID, ExpectedVersion: subB.RowVersion,
		ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange), Actor: operator, Channel: domain.ChannelAssisted,
		CustomerBasisRef: &basis, Now: day(11)}
	if _, err := f.s.ActivateSubscription(ctxB, cmdB, false, f.tclaim(orgB, operator, "activate", subB.SubscriptionID)); err != nil {
		t.Fatalf("activate org B subscription: %v", err)
	}

	// Org B's own activation already emits its own entitlement_snapshot.changed
	// (B2's lifecycle recompute) — an expected, unrelated event. Baseline it
	// here so the later assertion is "this A5 invalidation pass emitted
	// nothing new for org B", not "org B never saw this event type".
	var orgBBaseline int
	if err := f.admin.QueryRow(f.ctx,
		`SELECT count(*) FROM eventing_outbox WHERE convert_from(payload, 'UTF8')::jsonb ->> 'event_type' = 'entitlement_snapshot.changed' AND tenant_id = $1`,
		orgB).Scan(&orgBBaseline); err != nil {
		t.Fatalf("baseline org B events: %v", err)
	}

	// Publish a policy effective after org A's cancellation but while org B
	// is still active (and will remain so).
	policyEffective := cancelAt.Add(24 * time.Hour)
	p := &domain.EntitlementPolicyVersion{EndedOutcome: "READ_ONLY", EndedReadOnlyDays: 14, EffectiveFrom: policyEffective,
		Reason: "A5 proof", CreatedAt: policyEffective, CreatedByPrincipalID: publisher}
	published, err := f.s.PublishEntitlementPolicy(f.ctx, p, f.claim(publisher, "PublishEntitlementPolicy", "grace-policy"))
	if err != nil {
		t.Fatalf("publish policy: %v", err)
	}

	// Exactly one organization was queued: org A, not org B, and not every
	// organization in the system.
	rows, err := f.admin.Query(f.ctx, `SELECT organization_id::text, policy_version FROM entitlement_invalidation_queue`)
	if err != nil {
		t.Fatalf("query invalidation queue: %v", err)
	}
	var queued []string
	for rows.Next() {
		var org string
		var pv int
		if err := rows.Scan(&org, &pv); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		if pv != published.PolicyVersion {
			rows.Close()
			t.Fatalf("queued row carries policy_version %d, want %d", pv, published.PolicyVersion)
		}
		queued = append(queued, org)
	}
	rows.Close()
	if len(queued) != 1 || queued[0] != f.org {
		t.Fatalf("entitlement_invalidation_queue = %v, want exactly [%s] (org A only, not org B %s)", queued, f.org, orgB)
	}

	// Draining the queue emits the real per-tenant entitlement_snapshot.changed
	// event for org A, and finds nothing else to process.
	found, err := f.s.ProcessNextEntitlementInvalidation(f.ctx, policyEffective)
	if err != nil || !found {
		t.Fatalf("process invalidation: found=%v err=%v", found, err)
	}
	if found2, err := f.s.ProcessNextEntitlementInvalidation(f.ctx, policyEffective); err != nil || found2 {
		t.Fatalf("a second drain found another item: found=%v err=%v", found2, err)
	}

	var status, processedOrg string
	if err := f.admin.QueryRow(f.ctx, `SELECT status, organization_id::text FROM entitlement_invalidation_queue WHERE policy_version = $1`,
		published.PolicyVersion).Scan(&status, &processedOrg); err != nil {
		t.Fatalf("read processed row: %v", err)
	}
	if status != "DONE" || processedOrg != f.org {
		t.Fatalf("processed row = status=%s org=%s, want DONE org=%s", status, processedOrg, f.org)
	}

	// The real per-tenant signal landed on the outbox for org A (aggregate id
	// = organization id, same shape recomputeEntitlementsBatchTx always uses
	// for this event), carrying this policy publish's own organization as
	// its tenant.
	evt := f.latestEvent("entitlement_snapshot.changed", f.org)
	if evt.tenant != f.org {
		t.Fatalf("entitlement_snapshot.changed tenant = %s, want org A %s", evt.tenant, f.org)
	}

	var orgBEvents int
	if err := f.admin.QueryRow(f.ctx,
		`SELECT count(*) FROM eventing_outbox WHERE convert_from(payload, 'UTF8')::jsonb ->> 'event_type' = 'entitlement_snapshot.changed' AND tenant_id = $1`,
		orgB).Scan(&orgBEvents); err != nil {
		t.Fatalf("count org B events: %v", err)
	}
	if orgBEvents != orgBBaseline {
		t.Fatalf("org B (still-active subscription) gained %d new entitlement_snapshot.changed events from this A5 invalidation pass, want 0 (baseline %d, now %d)",
			orgBEvents-orgBBaseline, orgBBaseline, orgBEvents)
	}
}

// Negative control, exercised the same way rls_test.go's own CP001/immutable
// probes are: raw SQL as the admin/owner role proves the database itself
// enforces this, not merely that the Go code declines to ask. A plain
// connection with no app.commercial_plane / app.tenant_id declared (what any
// connection looks like before a store transaction declares one) sees zero
// rows in either new table — there is no organization-scoped branch to fall
// back to, unlike subscription_boundary_queue.
func TestEntitlement_InvalidationQueueHasNoDefaultVisibility(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	cancelAt := domain.AddBillingIntervals(day(11), "MONTH", 1, 1).Add(time.Hour)
	f.sv(f.s.CancelNow(f.ctxOrg, f.cmd(sub, "customer-admin", cancelAt), f.tclaim(f.org, "customer-admin", "cancel-now", sub.SubscriptionID)))
	f.policy("READ_ONLY", 14, publisher)

	var n int
	if err := f.app.QueryRow(f.ctx, `SELECT count(*) FROM entitlement_invalidation_queue`).Scan(&n); err != nil {
		t.Fatalf("plain connection read of invalidation queue: %v", err)
	}
	if n != 0 {
		t.Fatalf("a plain connection (no commercial_plane/tenant_id declared) saw %d entitlement_invalidation_queue rows, want 0", n)
	}
	if err := f.app.QueryRow(f.ctx, `SELECT count(*) FROM ended_subscription_index`).Scan(&n); err != nil {
		t.Fatalf("plain connection read of ended_subscription_index: %v", err)
	}
	if n != 0 {
		t.Fatalf("a plain connection (no tenant_id declared) saw %d ended_subscription_index rows, want 0", n)
	}
}
