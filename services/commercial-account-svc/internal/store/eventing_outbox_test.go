package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcenvelope "zoiko.io/commercial-account-svc/internal/envelope"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

const testEventRegion = "test-region"

// testEventProfile is the event profile every test store is built with.
var testEventProfile = store.WithEventProfile(testEventRegion, envelope.Confidential)

func migrationFile(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// Migration 000021 must carry the library's schema and hardening verbatim; the
// shared relay and VerifySchema depend on that exact shape.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	m := strings.ReplaceAll(migrationFile(t, "000021_add_eventing_outbox.up.sql"), "\r\n", "\n")
	if !strings.Contains(m, strings.TrimSpace(outbox.SchemaSQL)) {
		t.Fatal("000021 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
	}
	if !strings.Contains(m, strings.TrimSpace(outbox.HardeningSQL)) {
		t.Fatal("000021 must contain outbox.HardeningSQL verbatim; copy it again from services/eventing/outbox/outbox.go")
	}
}

// Canonical types, written out independently of the mapping: the catalogue's
// own Commercial Platform names come out exactly, the one catalogue spelling
// (cancelled) wins, and every other name keeps the service's words kebab-cased.
func TestEventingOutbox_CanonicalTypes(t *testing.T) {
	want := map[string]string{
		"subscription.started":                   "com.zoikosuite.commercial.subscription.started",
		"subscription.changed":                   "com.zoikosuite.commercial.subscription.changed",
		"subscription.canceled":                  "com.zoikosuite.commercial.subscription.cancelled",
		"platform_invoice.issued":                "com.zoikosuite.commercial.platform-invoice.issued",
		"platform_payment.settled":               "com.zoikosuite.commercial.platform-payment.settled",
		"usage.accepted":                         "com.zoikosuite.commercial.usage.accepted",
		"usage.window_frozen":                    "com.zoikosuite.commercial.usage.window-frozen",
		"commercial_subscription.created":        "com.zoikosuite.commercial.commercial-subscription.created",
		"entitlement.grace_period_started":       "com.zoikosuite.commercial.entitlement.grace-period-started",
		"price_version.commercial_terms_changed": "com.zoikosuite.commercial.price-version.commercial-terms-changed",
	}
	for legacy, canonical := range want {
		if got := store.CanonicalType(legacy); got != canonical {
			t.Errorf("%s: canonical %q, want %q", legacy, got, canonical)
		}
	}
}

func expectedPartitionKey(tenantID, aggregateType, aggregateID string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + aggregateType + "\x00" + aggregateID))
	return hex.EncodeToString(sum[:])
}

type outboxRow struct {
	eventID, canonical, legacy, tenant, region, partitionKey, aggregateID, state string
	body                                                                         map[string]any
}

// latestEvent is the newest eventing_outbox row with the given legacy type
// and aggregate id, read as the superuser.
func (f *pbFixture) latestEvent(legacy, aggregateID string) outboxRow {
	f.t.Helper()
	var r outboxRow
	var payload []byte
	err := f.admin.QueryRow(f.ctx, `SELECT event_id, event_type, tenant_id, region, partition_key, COALESCE(aggregate_id, ''),
			publish_state, payload
		FROM eventing_outbox
		WHERE convert_from(payload, 'UTF8')::jsonb ->> 'event_type' = $1 AND aggregate_id = $2
		ORDER BY outbox_id DESC LIMIT 1`, legacy, aggregateID).
		Scan(&r.eventID, &r.canonical, &r.tenant, &r.region, &r.partitionKey, &r.aggregateID, &r.state, &payload)
	if err != nil {
		f.t.Fatalf("no %s event for %s: %v", legacy, aggregateID, err)
	}
	if err := json.Unmarshal(payload, &r.body); err != nil {
		f.t.Fatal(err)
	}
	r.legacy, _ = r.body["event_type"].(string)
	return r
}

func (f *pbFixture) countEvents(where string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox WHERE `+where, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// checkEvent asserts one stored event's key fields.
func (f *pbFixture) checkEvent(r outboxRow, tenant, aggregateType, actor, correlation string) {
	f.t.Helper()
	switch {
	case r.canonical != store.CanonicalType(r.legacy):
		f.t.Errorf("%s: canonical type %q", r.legacy, r.canonical)
	case r.tenant != tenant || r.body["tenant_id"] != tenant:
		f.t.Errorf("%s: tenant %q / %v, want %q", r.legacy, r.tenant, r.body["tenant_id"], tenant)
	case r.region != testEventRegion:
		f.t.Errorf("%s: region %q", r.legacy, r.region)
	case r.partitionKey != expectedPartitionKey(tenant, aggregateType, r.aggregateID):
		f.t.Errorf("%s: partition key is not sha256(%s, %s, %s)", r.legacy, tenant, aggregateType, r.aggregateID)
	case r.body["event_id"] != r.eventID:
		f.t.Errorf("%s: body event_id %v, column %s", r.legacy, r.body["event_id"], r.eventID)
	case r.state != outbox.StatePending:
		f.t.Errorf("%s: state %s", r.legacy, r.state)
	}
	if actor != "" && r.body["actor_id"] != actor {
		f.t.Errorf("%s: actor %v, want %s", r.legacy, r.body["actor_id"], actor)
	}
	if correlation != "" && r.body["correlation_id"] != correlation {
		f.t.Errorf("%s: correlation %v, want %s", r.legacy, r.body["correlation_id"], correlation)
	}
}

// requestCtx is ctx carrying the request envelope the middleware would have
// placed: the actor and correlation id every write path reads.
func requestCtx(ctx context.Context, actor, correlation string) context.Context {
	return svcenvelope.WithEnvelope(ctx, svcenvelope.Envelope{ActorSubjectID: actor, CorrelationID: correlation})
}

// One representative flow per COM group, against real Postgres as the
// NOBYPASSRLS app role: each group's events carry the right tenant — the
// organization, or the platform scope for the seller's catalogue — keyed on
// their aggregate, with the actor and the request's correlation id.
func TestEventingOutbox_EachCOMGroupKeysItsEvents(t *testing.T) {
	f := newSub(t)

	t.Run("platform scope: COM-01 catalogue and COM-04 meter definitions", func(t *testing.T) {
		ctx := requestCtx(f.ctx, publisher, "corr-meter")
		m := &domain.MeterDefinition{MeterKey: "api.calls", MeterVersion: 1, DisplayName: "api.calls", Unit: "unit",
			AggregationMethod: "SUM", CreatedAt: t0, CreatedByPrincipalID: publisher}
		if _, err := f.s.RegisterMeterDefinition(ctx, m, f.claim(publisher, "RegisterMeterDefinition", "api.calls/1")); err != nil {
			t.Fatal(err)
		}
		f.checkEvent(f.latestEvent("meter_definition.registered", "api.calls/1"), store.PlatformScopeID, "meter-definition", publisher, "corr-meter")
		v := f.publishPlan("business", planOpts{autoRenew: true})
		f.checkEvent(f.latestEvent("price_version.published", v.PriceVersionID), store.PlatformScopeID, "product-price-version", "", "")
		f.checkEvent(f.latestEvent("product.created", v.ProductID), store.PlatformScopeID, "commercial-product", "", "")
	})

	var sub *domain.SubscriptionView
	t.Run("COM-02 subscriptions", func(t *testing.T) {
		sub = activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
		f.checkEvent(f.latestEvent("subscription.started", sub.SubscriptionID), f.org, "subscription", "", "")
	})

	t.Run("COM-02 legacy pilot: tenant is the organization, not the commercial account", func(t *testing.T) {
		org := uuid.NewString()
		account, legacySub := seedAccountAndSubscription(t, f.s, f.admin, org)
		r := f.latestEvent("commercial_subscription.created", legacySub)
		if r.tenant == account {
			t.Fatalf("the pilot's bug is back: tenant_id is the commercial account %s", account)
		}
		f.checkEvent(r, org, "commercial-subscription", "", "")
	})

	t.Run("COM-03 entitlements", func(t *testing.T) {
		r := f.restrict(domain.RestrictionLevel("READ_ONLY"), publisher, day(12))
		// A READ_ONLY restriction is announced as the start of a grace period.
		f.checkEvent(f.latestEvent("entitlement.grace_period_started", r.RestrictionID), f.org, "commercial-restriction", "", "")
	})

	t.Run("COM-04 usage: ingested events and seller-plane statement events", func(t *testing.T) {
		e := f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "5", day(12))
		f.checkEvent(f.latestEvent("usage.accepted", "api.calls/"+e.UsageEventID), f.org, "usage-event", "api-gateway-svc", "")
		// Close and certify run on the seller plane with no organization in
		// the request; the tenant is derived from the statement's usage events.
		ctx := requestCtx(f.ctx, publisher, "corr-close")
		st, err := f.s.CloseUsageWindow(ctx, sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", publisher, day(20),
			f.claim(publisher, "close", sub.SubscriptionID))
		if err != nil {
			t.Fatal(err)
		}
		f.checkEvent(f.latestEvent("usage.window_frozen", st.StatementID), f.org, "usage-statement", publisher, "corr-close")
		cert, err := f.s.CertifyUsageStatement(ctx, st.StatementID, checker, day(20), f.claim(checker, "certify", st.StatementID))
		if err != nil {
			t.Fatal(err)
		}
		f.checkEvent(f.latestEvent("usage.statement_certified", cert.StatementID), f.org, "usage-statement", checker, "corr-close")
	})

	t.Run("COM-05 billing", func(t *testing.T) {
		b := f.openBillingAccount(f.org)
		f.checkEvent(f.latestEvent("billing_account.opened", b.BillingAccountID), f.org, "billing-account", publisher, "")
		inv := f.issueSimpleInvoice(f.org, sub.SubscriptionID, sub.CurrentTerm.TermNo)
		f.checkEvent(f.latestEvent("platform_invoice.issued", inv.InvoiceID), f.org, "platform-commercial-invoice", publisher, "")
		if inv := f.latestEvent("platform_invoice.issued", inv.InvoiceID); inv.canonical != "com.zoikosuite.commercial.platform-invoice.issued" {
			t.Errorf("platform invoice canonical type %q is not the catalogue's", inv.canonical)
		}
	})

	if n := f.countEvents(`tenant_id IS NULL OR tenant_id = ''`); n != 0 {
		t.Fatalf("%d events without a tenant", n)
	}
}

// The event is enqueued inside the write's own transaction: a transaction that
// fails after the enqueue leaves no event, and a write whose event cannot be
// built leaves no business row and no idempotency claim either.
func TestEventingOutbox_RolledBackTransactionLeavesNoEvent(t *testing.T) {
	f := newSub(t)
	before := f.countEvents(`true`)

	forced := errors.New("forced failure after enqueue")
	err := f.s.WithScopeTx(f.ctx, f.org, func(tx pgx.Tx) error {
		if err := f.s.Emit(f.ctx, tx, "membership.created", "membership", uuid.NewString(), f.org, "seed", map[string]string{"k": "v"}); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&n); err != nil {
			return err
		}
		if n < 1 {
			t.Fatalf("inside the transaction the event is not visible to its own tenant")
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("transaction returned %v, want the forced failure", err)
	}
	if n := f.countEvents(`true`); n != before {
		t.Fatalf("rolled-back transaction left an event: %d -> %d", before, n)
	}

	// The real write path, failing at the enqueue: a store with no event
	// profile cannot build the event; the billing account and its
	// idempotency claim roll back with it.
	unprofiled := store.NewPgStore(f.app)
	b := &domain.BillingAccount{BillingAccountID: domain.NewCommercialID(domain.PrefixBillingAccount), OrganizationID: f.org,
		SellingEntity: "ZOIKOSUITE_INC_US", BillingCurrencyCode: "USD", InvoiceNumberingProfile: "US-STD",
		PaymentProviderRef: "provider:stripe:acct_test", AccountingMappingKey: "gl:us-inc:default", CreatedAt: t0, CreatedByPrincipalID: publisher}
	claim := f.claim(publisher, "OpenBillingAccount", f.org)
	if _, err := unprofiled.OpenBillingAccount(f.ctx, b, claim); err == nil {
		t.Fatal("a write whose event cannot be built must fail")
	}
	var accounts, claims int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM billing_accounts WHERE billing_account_id = $1`, b.BillingAccountID).Scan(&accounts)
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM commercial_idempotency_keys WHERE idempotency_key = $1`, claim.Key).Scan(&claims)
	if accounts != 0 || claims != 0 || f.countEvents(`true`) != before {
		t.Fatalf("failed enqueue left billing_accounts=%d idempotency=%d events=%d", accounts, claims, f.countEvents(`true`)-before)
	}
}

// An idempotent replay enqueues no second event.
func TestEventingOutbox_ReplayEnqueuesNoSecondEvent(t *testing.T) {
	f := newSub(t)
	b := &domain.BillingAccount{BillingAccountID: domain.NewCommercialID(domain.PrefixBillingAccount), OrganizationID: f.org,
		SellingEntity: "ZOIKOSUITE_INC_US", BillingCurrencyCode: "USD", InvoiceNumberingProfile: "US-STD",
		PaymentProviderRef: "provider:stripe:acct_test", AccountingMappingKey: "gl:us-inc:default", CreatedAt: t0, CreatedByPrincipalID: publisher}
	claim := f.claim(publisher, "OpenBillingAccount", f.org)
	if _, err := f.s.OpenBillingAccount(f.ctx, b, claim); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.OpenBillingAccount(f.ctx, b, claim)
	var replay *domain.IdempotentReplayError
	if err != nil && !errors.As(err, &replay) {
		t.Fatalf("same-key retry: %v, want the original result or a replay", err)
	}
	if n := f.countEvents(`aggregate_id = $1`, b.BillingAccountID); n != 1 {
		t.Fatalf("replay enqueued another event: %d", n)
	}
}

// eventing_outbox isolation, as the NOBYPASSRLS app role (decision: the house
// pattern, two writers):
//   - a tenant transaction sees only its own organization's events and cannot
//     write one under another organization — nor a platform-scope one;
//   - a seller-plane transaction can write for any organization and for the
//     platform scope, but reads nothing;
//   - only the relay marker sees every row.
func TestEventingOutbox_IsolationAsAppRole(t *testing.T) {
	f := newSub(t)
	orgB := uuid.NewString()
	f.newAccount(orgB, "USD", "")
	insert := func(tx pgx.Tx, tenant string) error {
		_, err := tx.Exec(f.ctx, `INSERT INTO eventing_outbox (outbox_id, event_id, event_type, schema_version, tenant_id,
				region, partition_key, payload, payload_hash)
			VALUES ($1, $2, 'com.zoikosuite.commercial.membership.created', '1.0.0', $3, 'test-region',
				repeat('a', 64), '\x7b7d', 'sha256:x')`, uuid.New(), "evt-"+uuid.NewString(), tenant)
		return err
	}
	refused := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "42501" ||
			err != nil && strings.Contains(err.Error(), `row-level security policy for table "eventing_outbox"`)
	}

	ownA := f.countEvents(`tenant_id = $1`, f.org)
	if ownA == 0 {
		t.Fatal("setup enqueued no event for org A")
	}
	err := f.s.WithScopeTx(f.ctx, orgB, func(tx pgx.Tx) error {
		var visible int
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox WHERE tenant_id <> $1`, orgB).Scan(&visible); err != nil {
			return err
		}
		if visible != 0 {
			t.Errorf("org B sees %d events of other tenants", visible)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{f.org, store.PlatformScopeID} {
		err := f.s.WithScopeTx(f.ctx, orgB, func(tx pgx.Tx) error { return insert(tx, tenant) })
		if !refused(err) {
			t.Errorf("tenant transaction for org B writing an event for %s: %v, want an RLS refusal", tenant, err)
		}
	}
	err = f.s.WithSellerPlaneTx(f.ctx, func(tx pgx.Tx) error {
		if err := insert(tx, f.org); err != nil {
			return err
		}
		if err := insert(tx, store.PlatformScopeID); err != nil {
			return err
		}
		var visible int
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&visible); err != nil {
			return err
		}
		if visible != 0 {
			t.Errorf("the seller plane reads %d outbox rows; it only needs to write", visible)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seller plane writing tenant and platform events: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	var all, total int
	_ = tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&all)
	if all != 0 {
		t.Errorf("no tenant, no plane, no relay marker: %d rows visible", all)
	}
	if _, err := tx.Exec(f.ctx, `SELECT set_config('app.outbox_relay', 'true', true)`); err != nil {
		t.Fatal(err)
	}
	_ = tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&all)
	total = f.countEvents(`true`)
	if all != total {
		t.Errorf("the relay marker sees %d of %d rows", all, total)
	}
}

// recordingWriter is an outbox.Writer that records what the relay sends.
type recordingWriter struct {
	mu        sync.Mutex
	failCalls int
	calls     int
	sent      []outbox.Message
}

func (w *recordingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.failCalls {
		return errors.New("simulated broker rejection")
	}
	w.sent = append(w.sent, msgs...)
	return nil
}

func (w *recordingWriter) Probe(context.Context) error { return nil }

func headerEventID(m outbox.Message) string {
	for _, h := range m.Headers {
		if h.Key == outbox.EventIDHeader {
			return string(h.Value)
		}
	}
	return ""
}

// The shared relay, as the app role, delivers every event — tenant and
// platform scope alike — keyed on its stored partition key with its stored
// event id, retries a failure under the same id, and never sends one twice.
func TestEventingOutbox_SharedRelayDeliversServiceEvents(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.openBillingAccount(f.org)
	stored := map[string]string{} // event id -> partition key
	rows, err := f.admin.Query(f.ctx, `SELECT event_id, partition_key FROM eventing_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, key string
		_ = rows.Scan(&id, &key)
		stored[id] = key
	}
	rows.Close()
	if len(stored) < 3 {
		t.Fatalf("setup enqueued only %d events", len(stored))
	}

	writer := &recordingWriter{failCalls: 2}
	relay, err := outbox.NewRelay(f.app, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 10 && len(writer.sent) < len(stored); pass++ {
		if _, err := relay.DrainOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
		_, _ = f.admin.Exec(f.ctx, `UPDATE eventing_outbox SET next_attempt_at = now() WHERE publish_state = 'failed'`)
	}
	if len(writer.sent) != len(stored) {
		t.Fatalf("relay delivered %d of %d events", len(writer.sent), len(stored))
	}
	seen := map[string]bool{}
	for _, m := range writer.sent {
		id := headerEventID(m)
		key, ok := stored[id]
		if !ok || seen[id] {
			t.Fatalf("X-Event-ID %q is unknown or delivered twice", id)
		}
		seen[id] = true
		if string(m.Key) != key {
			t.Errorf("event %s: Kafka key %s, want %s", id, m.Key, key)
		}
	}
	if n := f.countEvents(`publish_state <> 'published'`); n != 0 {
		t.Fatalf("%d events not marked published", n)
	}
	if res, err := relay.DrainOnce(f.ctx); err != nil || res.Claimed != 0 {
		t.Fatalf("a published event was claimed again: %+v %v", res, err)
	}
}

// reapply000021 rolls 000021 back with its own down file, lets seed write
// legacy outbox_events rows, then applies 000021 again — so the carry-over
// runs against real commercial data.
func (f *pbFixture) reapply000021(seed func()) error {
	f.t.Helper()
	if _, err := f.admin.Exec(f.ctx, migrationFile(f.t, "000021_add_eventing_outbox.down.sql")); err != nil {
		f.t.Fatalf("000021 down: %v", err)
	}
	seed()
	_, err := f.admin.Exec(f.ctx, migrationFile(f.t, "000021_add_eventing_outbox.up.sql"))
	return err
}

// Migration 000021 carries every undelivered outbox_events row over with the
// tenant each one actually belongs to — resolved four ways — keeps
// outbox_events, leaves delivered rows behind, and the shared relay delivers
// the carried-over rows.
func TestEventingOutbox_MigrationCarriesOverUndeliveredLegacyRows(t *testing.T) {
	f := newSub(t)
	f.registerMeter("api.calls", 1, "SUM", nil)
	f.publishPlan("business", planOpts{autoRenew: true})
	sub := activateNow(f, f.sv(f.start(f.startParams(f.account, "business", day(11)))), day(11))
	f.mustIngest(f.org, sub.SubscriptionID, "api.calls", 1, "evt-1", "10", day(12))
	origin := f.closeAndCertify(sub.SubscriptionID, sub.CurrentTerm.TermNo, "api.calls", day(20))
	renewAt := sub.CurrentTerm.EndsAt.Add(time.Hour)
	f.sv(f.s.Renew(f.ctxOrg, f.seller(sub, renewAt), f.tclaim(f.org, operator, "renew", sub.SubscriptionID)))
	if _, err := f.ingestAt(f.org, sub.SubscriptionID, "api.calls", 1, "evt-late", "3", day(15), renewAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var adjustmentID string
	if err := f.admin.QueryRow(f.ctx, `SELECT adjustment_id FROM usage_adjustments WHERE origin_statement_id = $1`, origin.StatementID).
		Scan(&adjustmentID); err != nil {
		t.Fatalf("late usage made no adjustment: %v", err)
	}
	legacyOrg := uuid.NewString()
	legacyAccount, legacySub := seedAccountAndSubscription(t, f.s, f.admin, legacyOrg)

	type legacyRow struct {
		id                     uuid.UUID
		aggType, aggID, evType string
		tenant                 any // stored tenant_id: org, account id, or NULL
		published              bool
		wantTenant, wantAgg    string
	}
	legacy := []legacyRow{
		{uuid.New(), "billing_account", "cba_x", "billing_account.opened", f.org, false, f.org, "billing-account"},
		{uuid.New(), "commercial_subscription", legacySub, "commercial_subscription.created", legacyOrg, false, "", "commercial-subscription"},
		{uuid.New(), "usage_statement", origin.StatementID, "usage.statement_certified", nil, false, f.org, "usage-statement"},
		{uuid.New(), "usage_adjustment", adjustmentID, "usage_adjustment.created", nil, false, f.org, "usage-adjustment"},
		{uuid.New(), "meter_definition", "api.calls/1", "meter_definition.registered", nil, false, store.PlatformScopeID, "meter-definition"},
		{uuid.New(), "billing_account", "cba_y", "billing_account.opened", f.org, true, "", ""},
	}
	// The pilot stored the commercial ACCOUNT id as tenant; the carry-over must
	// map it back to the account's organization.
	legacy[1].tenant, legacy[1].wantTenant = legacyAccount, legacyOrg

	err := f.reapply000021(func() {
		for _, l := range legacy {
			var publishedAt any
			if l.published {
				publishedAt = time.Now().UTC()
			}
			if _, err := f.admin.Exec(f.ctx, `INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type,
					payload, tenant_id, published_at, publish_attempts, last_error)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 4, 'broker down')`,
				l.id, l.aggType, l.aggID, l.evType, `{"id":"`+l.aggID+`"}`, l.tenant, publishedAt); err != nil {
				t.Fatalf("seed %s: %v", l.evType, err)
			}
		}
	})
	if err != nil {
		t.Fatalf("000021 over legacy rows: %v", err)
	}

	if n := f.countEvents(`true`); n != 5 {
		t.Fatalf("eventing_outbox has %d rows, want the 5 undelivered legacy rows", n)
	}
	var kept int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events`).Scan(&kept)
	if kept < len(legacy) {
		t.Fatalf("outbox_events has %d rows: it must be kept as evidence", kept)
	}
	for _, l := range legacy[:5] {
		eventID := "evt-" + l.id.String()
		var tenant, region, key, state, lastErr string
		var attempts int
		var payload []byte
		if err := f.admin.QueryRow(f.ctx, `SELECT tenant_id, region, partition_key, publish_state, attempt_count, last_error, payload
			FROM eventing_outbox WHERE event_id = $1`, eventID).Scan(&tenant, &region, &key, &state, &attempts, &lastErr, &payload); err != nil {
			t.Fatalf("%s not carried over: %v", l.evType, err)
		}
		var body map[string]any
		_ = json.Unmarshal(payload, &body)
		inner, _ := body["payload"].(map[string]any)
		switch {
		case tenant != l.wantTenant || body["tenant_id"] != l.wantTenant:
			t.Errorf("%s: tenant %q, want %q", l.evType, tenant, l.wantTenant)
		case key != expectedPartitionKey(l.wantTenant, l.wantAgg, l.aggID):
			t.Errorf("%s: not keyed like new %s events", l.evType, l.wantAgg)
		case region != "legacy" || state != outbox.StatePending || attempts != 0 || lastErr != "broker down":
			t.Errorf("%s: region=%s state=%s attempts=%d last_error=%s", l.evType, region, state, attempts, lastErr)
		case body["event_id"] != eventID || body["event_type"] != l.evType || body["entity_id"] != l.aggID ||
			body["source_service"] != "commercial-account-svc" || inner["id"] != l.aggID:
			t.Errorf("%s: body is not the old publisher's shape: %v", l.evType, body)
		}
	}

	// Re-applying 000021 re-created the table, so grant the app role again —
	// in production ALTER DEFAULT PRIVILEGES does this for a new table.
	app := appRolePool(t, f.admin)
	writer := &recordingWriter{}
	relay, err := outbox.NewRelay(app, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if res, err := relay.DrainOnce(f.ctx); err != nil || res.Published != 5 {
		t.Fatalf("relay over carried-over rows: %+v %v", res, err)
	}
	for _, m := range writer.sent {
		var body map[string]any
		_ = json.Unmarshal(m.Value, &body)
		if headerEventID(m) != body["event_id"] {
			t.Errorf("X-Event-ID %q, body event_id %v", headerEventID(m), body["event_id"])
		}
	}
}

// A legacy row whose aggregate type the carry-over does not know — or whose
// tenant cannot be resolved — fails the migration instead of being keyed on a
// guess, and nothing is left behind.
func TestEventingOutbox_MigrationRefusesUnresolvableLegacyRows(t *testing.T) {
	for name, row := range map[string][2]string{
		"unmapped aggregate":          {"mystery_aggregate", uuid.NewString()},
		"account that does not exist": {"commercial_subscription", uuid.NewString()},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSub(t)
			err := f.reapply000021(func() {
				if _, err := f.admin.Exec(f.ctx, `INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload, tenant_id)
					VALUES ($1, 'x-1', 'x.happened', '{}', $2)`, row[0], row[1]); err != nil {
					t.Fatal(err)
				}
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
				t.Fatalf("migration: %v, want a NOT NULL violation (23502)", err)
			}
			var exists bool
			_ = f.admin.QueryRow(f.ctx, `SELECT to_regclass('eventing_outbox') IS NOT NULL`).Scan(&exists)
			if exists {
				t.Fatal("the failed migration left eventing_outbox behind; it must apply atomically")
			}
		})
	}
}

var _ = svcmiddleware.WithTenant
