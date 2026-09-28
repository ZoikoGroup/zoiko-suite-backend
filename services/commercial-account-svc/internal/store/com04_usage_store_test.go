package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// COM-04 Usage Metering against real Postgres as the NOBYPASSRLS role.

func (f *subFixture) registerMeter(key string, version int, aggMethod string, uniqueDim *string) *domain.MeterDefinition {
	f.t.Helper()
	m := &domain.MeterDefinition{MeterKey: key, MeterVersion: version, DisplayName: key, Unit: "unit",
		AggregationMethod: aggMethod, UniqueDimension: uniqueDim, CreatedAt: t0, CreatedByPrincipalID: publisher}
	got, err := f.s.RegisterMeterDefinition(f.ctx, m, f.claim(publisher, "RegisterMeterDefinition", domain.RegisteredMeterKey(key, version)))
	if err != nil {
		f.t.Fatalf("register meter %s/%d: %v", key, version, err)
	}
	return got
}

func (f *subFixture) ingest(org, sub, meterKey string, version int, eventID, quantity string, occurredAt time.Time) (*domain.UsageEventRecord, error) {
	return f.ingestAt(org, sub, meterKey, version, eventID, quantity, occurredAt, occurredAt)
}

// ingestAt lets a test separate occurred_at (when the usage happened) from
// observed_at (when COM-04 saw it) — needed to simulate a genuinely late
// arrival, where observed_at is well after occurred_at's window closed.
func (f *subFixture) ingestAt(org, sub, meterKey string, version int, eventID, quantity string, occurredAt, observedAt time.Time) (*domain.UsageEventRecord, error) {
	return f.s.RegisterUsageEvent(f.ctx, org, sub, meterKey, version, eventID,
		domain.EventInput{Quantity: quantity, OccurredAt: occurredAt}, "api-gateway-svc", observedAt,
		f.claim("api-gateway-svc", "RegisterUsageEvent", meterKey+"/"+eventID))
}

func (f *subFixture) mustIngest(org, sub, meterKey string, version int, eventID, quantity string, occurredAt time.Time) *domain.UsageEventRecord {
	f.t.Helper()
	e, err := f.ingest(org, sub, meterKey, version, eventID, quantity, occurredAt)
	if err != nil {
		f.t.Fatalf("ingest %s: %v", eventID, err)
	}
	return e
}

func (f *subFixture) closeAndCertify(subID string, termNo int, meterKey string, at time.Time) *domain.UsageStatement {
	f.t.Helper()
	if _, err := f.s.CloseUsageWindow(f.ctx, subID, termNo, meterKey, publisher, at, f.claim(publisher, "close", subID+meterKey)); err != nil {
		f.t.Fatalf("close window: %v", err)
	}
	st, err := f.s.GetUsage(f.ctxOrg, subID, termNo, meterKey)
	if err != nil {
		f.t.Fatalf("get usage: %v", err)
	}
	got, err := f.s.CertifyUsageStatement(f.ctx, st.StatementID, publisher, at, f.claim(publisher, "certify", st.StatementID))
	if err != nil {
		f.t.Fatalf("certify: %v", err)
	}
	return got
}

// Uniqueness (COM-CTRL-014; negative path #15): the same usage_event_id
// under the same meter is refused, never double-counted.
func TestUsage_DuplicateEventIsRefusedNotDoubleCounted(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))
	if _, err := f.ingest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12)); !errors.Is(err, domain.ErrUsageEventExists) {
		t.Fatalf("a replayed usage_event_id was accepted twice: %v", err)
	}
	st, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls")
	if err != nil || st.EventCount != 1 {
		t.Fatalf("statement after a duplicate attempt: %+v (err=%v)", st, err)
	}
}

// Validity (negative path #16): negative or malformed quantities quarantine,
// never counted toward the total.
func TestUsage_NegativeQuantityIsQuarantined(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	e, err := f.ingest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-bad", "-3", day(12))
	if err != nil || e.Status != domain.UsageQuarantined || e.QuarantineReason == nil {
		t.Fatalf("a negative quantity was not quarantined: %+v (err=%v)", e, err)
	}
	// Linked to the window it would have belonged to (still OPEN here), so
	// the window's quarantined_count is real completeness evidence.
	if e.StatementID == nil {
		t.Fatal("a quarantined event in an open window was not linked to its statement")
	}
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-good", "5", day(12))
	st, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls")
	if err != nil || st.EventCount != 1 || st.QuarantinedCount != 1 {
		t.Fatalf("statement counts: %+v (err=%v)", st, err)
	}
}

// Negative path #14: usage against an unregistered meter is refused, not
// silently accepted.
func TestUsage_UnregisteredMeterIsRefused(t *testing.T) {
	f := newSub(t)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	if _, err := f.ingest(f.org, sub.SubscriptionID, "no.such.meter", 1, "evt-1", "1", day(12)); !errors.Is(err, domain.ErrMeterDefinitionNotFound) {
		t.Fatalf("usage against an unregistered meter was accepted: %v", err)
	}
}

// Completeness/Aggregation (§4.4): CloseUsageWindow freezes the population;
// CertifyUsageStatement seals a total that reproduces domain.Aggregate over
// exactly the accepted events (proven via ExplainAggregation).
func TestUsage_CloseAndCertify_TotalMatchesExplainedEvents(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5.5", day(12))
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-2", "2.25", day(13))
	_, _ = f.ingest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-bad", "-1", day(13))

	st := f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", day(20))
	if st.Status != domain.StatementCertified || st.TotalQuantity != "7.7500" || st.EventCount != 2 || st.QuarantinedCount != 1 {
		t.Fatalf("certified statement: %+v", st)
	}
	if st.WatermarkAt == nil || !st.WatermarkAt.Equal(day(13)) {
		t.Fatalf("watermark = %v, want the latest accepted occurred_at", st.WatermarkAt)
	}
	// ExplainAggregation shows everything linked to the window — accepted
	// and quarantined alike (Reconciliation: accepted+quarantined =
	// everything ingested) — not only what contributed to the total.
	events, err := f.s.ExplainAggregation(f.ctxOrg, st.StatementID)
	if err != nil || len(events) != 3 {
		t.Fatalf("ExplainAggregation: %d events, want 2 accepted + 1 quarantined (err=%v)", len(events), err)
	}
	var accepted int
	for _, e := range events {
		if e.Status == domain.UsageAccepted {
			accepted++
		}
	}
	if accepted != 2 {
		t.Fatalf("%d accepted events among those explained, want 2", accepted)
	}
	if n := f.outboxCount(st.StatementID, "usage.statement_certified"); n != 1 {
		t.Fatalf("certification events: %d", n)
	}

	// Reconciliation: nothing further can change a certified statement.
	if _, err := f.s.CloseUsageWindow(f.ctx, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", publisher, day(21),
		f.claim(publisher, "close-again", "x")); err == nil {
		t.Log("closing an already-certified window is idempotent by design; verifying it did not change state")
	}
	after, _ := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls")
	if after.TotalQuantity != "7.7500" || after.Status != domain.StatementCertified {
		t.Fatalf("a certified statement's total changed: %+v", after)
	}
}

// Negative path #18: usage arriving after its window is certified never
// mutates that statement; it becomes an adjustment on the next open window.
func TestUsage_LateUsageAfterCertificationCreatesAnAdjustment(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "10", day(12))
	origin := f.closeAndCertify(sub.SubscriptionID, term1, "api.calls", day(20))
	if origin.TotalQuantity != "10.0000" {
		t.Fatalf("origin total before the late event: %s", origin.TotalQuantity)
	}

	renewAt := sub.CurrentTerm.EndsAt.Add(time.Hour)
	sub2 := f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, renewAt), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	term2 := sub2.CurrentTerm.TermNo

	// This event's occurred_at falls inside term1's window, but it is only
	// observed (reported) after term2 has already started — term1's
	// statement is already CERTIFIED and must not reopen or change.
	observedInTerm2 := renewAt.Add(time.Hour)
	late, err := f.ingestAt(f.org, sub.SubscriptionID, "api.calls", 1, "evt-late", "3", day(15), observedInTerm2)
	if err != nil {
		t.Fatalf("ingest late event: %v", err)
	}
	if !late.Late || late.StatementID != nil {
		t.Fatalf("a late event was not marked late, or was linked to a statement: %+v", late)
	}
	stillOrigin, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || stillOrigin.TotalQuantity != "10.0000" || stillOrigin.Status != domain.StatementCertified {
		t.Fatalf("the certified statement changed after a late event: %+v (err=%v)", stillOrigin, err)
	}

	target, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term2, "api.calls")
	if err != nil || target.EventCount != 1 {
		t.Fatalf("the late event's quantity was not carried into the next open window: %+v (err=%v)", target, err)
	}
	target2 := f.closeAndCertify(sub.SubscriptionID, term2, "api.calls", sub2.CurrentTerm.EndsAt.Add(time.Hour))
	if target2.Status != domain.StatementAdjusted {
		t.Fatalf("a statement carrying a late adjustment must certify as ADJUSTED, got %s", target2.Status)
	}

	lateEvents, err := f.s.GetLateEvents(f.ctxOrg, sub.SubscriptionID)
	if err != nil || len(lateEvents) != 1 || lateEvents[0].UsageEventID != "evt-late" {
		t.Fatalf("GetLateEvents: %+v (err=%v)", lateEvents, err)
	}
}

// A correction while the window is still OPEN replaces the quantity
// directly; once the window has closed, a "correction" is just another late
// fact and is routed the same way (never mutates closed history).
func TestUsage_CorrectUsageEvent(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))
	corrected, err := f.s.CorrectUsageEvent(f.ctx, "api.calls", "evt-1", "evt-1-fix",
		domain.EventInput{Quantity: "8", OccurredAt: day(12)}, "undercounted", publisher, day(13),
		f.claim(publisher, "correct", "api.calls/evt-1-fix"))
	if err != nil || corrected.Status != domain.UsageAccepted {
		t.Fatalf("correction: %+v (err=%v)", corrected, err)
	}
	st, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || st.EventCount != 1 {
		t.Fatalf("after correction, event_count must reflect one live event, not two: %+v (err=%v)", st, err)
	}
	orig, err := f.s.GetDedupStatus(f.ctx, "api.calls", "evt-1")
	if err != nil || orig.Status != domain.UsageCorrected || orig.SupersededByEventID == nil || *orig.SupersededByEventID != "evt-1-fix" {
		t.Fatalf("original event after correction: %+v (err=%v)", orig, err)
	}
	if _, err := f.s.CorrectUsageEvent(f.ctx, "api.calls", "evt-1", "evt-1-fix-2",
		domain.EventInput{Quantity: "9", OccurredAt: day(12)}, "again", publisher, day(13),
		f.claim(publisher, "correct-2", "api.calls/evt-1-fix-2")); !errors.Is(err, domain.ErrUsageEventNotCorrectable) {
		t.Fatalf("an already-corrected event was corrected again: %v", err)
	}

	f.closeAndCertify(sub.SubscriptionID, term1, "api.calls", day(20))
	if _, err := f.s.CorrectUsageEvent(f.ctx, "api.calls", "evt-1-fix", "evt-1-fix-late",
		domain.EventInput{Quantity: "1", OccurredAt: day(12)}, "post-certify", publisher, day(21),
		f.claim(publisher, "correct-late", "api.calls/evt-1-fix-late")); !errors.Is(err, domain.ErrUsageEventNotCorrectable) {
		t.Fatalf("an accepted event in a certified window was corrected in place: %v", err)
	}
}

// ReopenWindow is the controlled exception, maker-checker: the certifier
// cannot reopen their own certification, and every accepted event re-homes
// onto the replacement so it aggregates again on recertify.
func TestUsage_ReopenWindow(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))
	orig := f.closeAndCertify(sub.SubscriptionID, term1, "api.calls", day(20))

	if _, err := f.s.ReopenWindow(f.ctx, orig.StatementID, publisher, "dispute", day(21),
		f.claim(publisher, "reopen", orig.StatementID)); !errors.Is(err, domain.ErrReopenNeedsIndependentActor) {
		t.Fatalf("the certifier reopened their own statement: %v", err)
	}
	reopened, err := f.s.ReopenWindow(f.ctx, orig.StatementID, checker, "dispute resolved in customer's favor", day(21),
		f.claim(checker, "reopen", orig.StatementID))
	if err != nil || reopened.Status != domain.StatementOpen || reopened.StatementID == orig.StatementID {
		t.Fatalf("reopen: %+v (err=%v)", reopened, err)
	}
	stale, err := f.s.GetUsageStatement(f.ctxOrg, orig.StatementID)
	if err != nil || stale.Status != domain.StatementSuperseded || stale.SupersededByStatementID == nil || *stale.SupersededByStatementID != reopened.StatementID {
		t.Fatalf("superseded original: %+v (err=%v)", stale, err)
	}
	events, err := f.s.ExplainAggregation(f.ctxOrg, reopened.StatementID)
	if err != nil || len(events) != 1 {
		t.Fatalf("the reopened statement must inherit the original's accepted events: %d (err=%v)", len(events), err)
	}
	recert := f.closeAndCertify(sub.SubscriptionID, term1, "api.calls", day(22))
	if recert.TotalQuantity != "5.0000" || recert.StatementID != reopened.StatementID {
		t.Fatalf("recertification after reopen: %+v", recert)
	}
}

func TestUsage_UniqueCountAggregation(t *testing.T) {
	f := newSub(t)
	dim := "user_id"
	f.registerMeter("active.users", 1, "UNIQUE_COUNT", &dim)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))

	for i, ev := range []struct{ id, user string }{{"e1", "u1"}, {"e2", "u2"}, {"e3", "u1"}} {
		_, err := f.s.RegisterUsageEvent(f.ctx, f.org, sub.SubscriptionID, "active.users", 1, ev.id,
			domain.EventInput{Quantity: "1", Dimensions: map[string]string{"user_id": ev.user}, OccurredAt: day(12)},
			"app-svc", day(12), f.claim("app-svc", "ingest", "active.users/"+ev.id))
		if err != nil {
			f.t.Fatalf("ingest %d: %v", i, err)
		}
	}
	st := f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "active.users", day(20))
	if st.EventCount != 3 {
		t.Fatalf("3 occurrences were accepted (u1, u2, u1), got event_count=%d", st.EventCount)
	}
	if st.TotalQuantity != "2" {
		t.Fatalf("UNIQUE_COUNT total = %s, want 2 (u1 and u2 are the only distinct users across 3 occurrences)", st.TotalQuantity)
	}
}

// Boundary integration: the worker freezes and certifies usage at term end,
// whether or not the subscription itself renews — the usage window is the
// term, not the subscription's continuation.
func TestUsage_BoundaryWorkerCertifiesAtTermEnd(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true, notice: 0})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	term1 := sub.CurrentTerm.TermNo
	termEnd := sub.CurrentTerm.EndsAt

	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "42", day(15))
	f.drain(termEnd)

	st, err := f.s.GetUsage(f.ctxOrg, sub.SubscriptionID, term1, "api.calls")
	if err != nil || st.Status != domain.StatementCertified || st.TotalQuantity != "42.0000" {
		t.Fatalf("the boundary worker did not certify the term's usage: %+v (err=%v)", st, err)
	}

	// A subscription that cancels (does not renew) still has its final
	// term's usage certified — usage certification is not conditional on
	// the subscription continuing.
	org2, acct2 := f.newAccount(orgB, "USD", "GB")
	ctx2 := svcmiddleware.WithTenant(context.Background(), org2)
	p := f.startParams(acct2, "business", day(11))
	sub2 := f.sv(f.s.StartSubscription(ctx2, p, f.tclaim(org2, "customer-admin", "StartSubscription", p.SubscriptionID)))
	sub2 = f.sv(f.s.ActivateSubscription(ctx2, f.seller(sub2, day(11)), false, f.tclaim(org2, operator, "activate", sub2.SubscriptionID)))
	f.mustIngest(org2, sub2.SubscriptionID, "api.calls", 1, "evt-cancel-1", "7", day(15))
	// With a 1-interval minimum term, the minimum is exactly one term long,
	// so the earliest CancelNow can take effect is at that term's own end.
	term1No, term1End := sub2.CurrentTerm.TermNo, sub2.CurrentTerm.EndsAt
	sub2 = f.sv(f.s.CancelNow(ctx2, domain.SubscriptionCommand{SubscriptionID: sub2.SubscriptionID, ExpectedVersion: sub2.RowVersion,
		ChangeID: domain.NewCommercialID(domain.PrefixCommercialChange), Actor: "customer-admin", Channel: domain.ChannelSelfService, Now: term1End},
		f.tclaim(org2, "customer-admin", "cancel-now", sub2.SubscriptionID)))
	f.drain(term1End)
	st2, err := f.s.GetUsage(ctx2, sub2.SubscriptionID, term1No, "api.calls")
	if err != nil || st2.Status != domain.StatementCertified || st2.TotalQuantity != "7.0000" {
		t.Fatalf("a canceled subscription's final usage was not certified: %+v (err=%v)", st2, err)
	}
}

// ── RLS ──────────────────────────────────────────────────────────────────────

func TestUsage_RLS_IngestionAndCertificationAreSellerPlaneOnly(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))

	if _, err := f.tenantExec(f.org, `INSERT INTO usage_event_records (meter_key, usage_event_id, meter_version,
		organization_id, subscription_id, term_no, quantity, occurred_at, observed_at, source_service, status, created_at)
		VALUES ('api.calls', 'forged', 1, $1, $2, 1, 99, now(), now(), 'x', 'ACCEPTED', now())`,
		f.org, sub.SubscriptionID); pgCode(err) != "42501" {
		t.Fatalf("a tenant wrote its own usage event: %v", err)
	}
	// OPEN -> FROZEN is otherwise a structurally legal transition, so this
	// specifically proves RLS refuses it, not just the lifecycle trigger.
	if _, err := f.tenantExec(f.org, `UPDATE usage_statements SET status = 'FROZEN', frozen_at = now(),
		frozen_by_principal_id = 'tenant-user' WHERE subscription_id = $1`, sub.SubscriptionID); pgCode(err) != "42501" {
		t.Fatalf("a tenant froze its own statement: %v", err)
	}

	ctxB := svcmiddleware.WithTenant(context.Background(), orgB)
	if _, err := f.s.GetUsage(ctxB, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls"); !errors.Is(err, domain.ErrStatementNotFound) {
		t.Fatalf("ISOLATION FAILURE: org B read org A's usage statement: %v", err)
	}
	if events, err := f.s.GetLateEvents(ctxB, sub.SubscriptionID); err != nil || len(events) != 0 {
		t.Fatalf("ISOLATION FAILURE: org B read org A's usage events: %v (err=%v)", events, err)
	}
}

func TestUsage_MeterDefinitionsAreImmutableAndSellerOnly(t *testing.T) {
	f := newSub(t)
	m := f.registerMeter("api.calls", 1, "SUM", nil)
	if _, err := f.tenantExec(orgA, `INSERT INTO meter_definitions (meter_key, meter_version, display_name, unit,
		aggregation_method, created_at, created_by_principal_id) VALUES ('tenant.meter', 1, 'x', 'x', 'SUM', now(), 'tenant-user')`); pgCode(err) != "42501" {
		t.Fatalf("a tenant registered a meter: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE meter_definitions SET aggregation_method = 'MAX' WHERE meter_key = $1 AND meter_version = $2`,
		m.MeterKey, m.MeterVersion); pgCode(err) != "CP001" {
		t.Fatalf("a meter definition was edited in place: %v", err)
	}
	retired, err := f.s.RetireMeterDefinition(f.ctx, m.MeterKey, m.MeterVersion, publisher, "superseded by v2", day(1))
	if err != nil || retired.RetiredAt == nil {
		t.Fatalf("retire: %+v (err=%v)", retired, err)
	}
	if _, err := f.ingest(f.org, "csub_00000000-0000-4000-8000-000000000001", m.MeterKey, m.MeterVersion, "evt-x", "1", day(2)); !errors.Is(err, domain.ErrMeterDefinitionRetired) {
		t.Fatalf("usage was ingested against a retired meter: %v", err)
	}
}

// Integration with COM-01: a price version's METERED component cannot
// submit until the meter it names is registered.
func TestUsage_UnblocksMeteredPriceComponentSubmission(t *testing.T) {
	f := newSub(t)
	p := f.product("automation")
	v := f.draft(p.ProductID, day(40), false, maker)
	v = f.put(v, domain.PriceComponent{ComponentKey: "runs", ComponentType: domain.ComponentMetered,
		MeterKey: strp("automation.runs"), MeterVersion: intp(1), AggregationMethod: strp("SUM"),
		IncludedQuantity: strp("0"), BillingTiming: strp("IN_ARREARS"), Amount: strp("0.02")})
	v = f.ok(f.s.SetCommercialTerms(f.ctx, v.PriceVersionID, v.RowVersion, &domain.CommercialTerms{
		TermsDocumentRef: "legal/terms/automation", TermsDocumentSHA256: termsHash, AutoRenew: true,
		RenewalNoticeDays: 0, MinimumTermIntervals: 1, SetAt: t0, SetByPrincipalID: maker,
	}, f.claim(maker, "SetCommercialTerms", v.PriceVersionID)))

	if _, err := f.submit(v, maker, t0); err == nil {
		t.Fatal("a METERED component submitted with no registered meter")
	}
	f.registerMeter("automation.runs", 1, "SUM", nil)
	if _, err := f.submit(v, maker, t0); err != nil {
		t.Fatalf("submission still blocked after the meter was registered: %v", err)
	}
}
