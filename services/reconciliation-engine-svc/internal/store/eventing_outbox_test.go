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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
	"zoiko.io/eventing/outbox"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

const testEventRegion = "test-region"

// Migration 000012 must carry the library's schema and hardening verbatim; the
// shared relay and VerifySchema depend on that exact shape.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations", "000012_add_eventing_outbox.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(migration, strings.TrimSpace(outbox.SchemaSQL)) {
		t.Fatal("000012 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
	}
	if !strings.Contains(migration, strings.TrimSpace(outbox.HardeningSQL)) {
		t.Fatal("000012 must contain outbox.HardeningSQL verbatim; copy it again from services/eventing/outbox/outbox.go")
	}
}

// expectedPartitionKey is envelope.PartitionKey's formula, written out
// independently so a change on either side is caught here.
func expectedPartitionKey(tenantID, aggregateType, aggregateID string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + aggregateType + "\x00" + aggregateID))
	return hex.EncodeToString(sum[:])
}

type outboxRow struct {
	eventID, eventType, schemaVersion, tenantID, region, partitionKey, state string
	aggregateID                                                              *string
	payload                                                                  map[string]any
}

// outboxRows reads every eventing_outbox row as the admin (RLS-exempt) role,
// oldest first. outbox_id is a UUIDv7, so it orders by insertion.
func (f *fixture) outboxRows() []outboxRow {
	f.t.Helper()
	rows, err := f.admin.Query(f.ctx, `
		SELECT event_id, event_type, schema_version, tenant_id, region, partition_key, publish_state, aggregate_id, payload
		FROM eventing_outbox ORDER BY outbox_id`)
	if err != nil {
		f.t.Fatalf("read eventing_outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		if err := rows.Scan(&r.eventID, &r.eventType, &r.schemaVersion, &r.tenantID, &r.region, &r.partitionKey,
			&r.state, &r.aggregateID, &payload); err != nil {
			f.t.Fatalf("scan eventing_outbox: %v", err)
		}
		if err := json.Unmarshal(payload, &r.payload); err != nil {
			f.t.Fatalf("outbox payload is not JSON: %v (%s)", err, payload)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(f.ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// wantEvent is what one write must have enqueued.
type wantEvent struct {
	canonical, legacy, aggregate string
	aggregateID                  func() string // resolved after the write, when the id exists
	actor                        string
}

// Every one of the thirteen writes enqueues exactly one event in its own
// transaction: the canonical and legacy type for that write, keyed on the
// write's aggregate under the caller's tenant, stamped with the record's legal
// entity, the configured region, and the request's actor and correlation id.
func TestEventingOutbox_EveryWriteEnqueuesOneCorrectlyKeyedEvent(t *testing.T) {
	f := newFixture(t)
	const prefix = "com.zoikosuite.accounting."

	var (
		def                           *domain.ReconciliationDefinition
		failed, runA, runB, runC      *domain.ReconciliationRun
		ruleVersion                   int64
		breakA, breakB, certification string
	)
	mismatch := []testPop{
		{Side: "Bank", SourceSystem: "BANK", Items: []domain.PopulationItemInput{item("REF-1", 100)}},
		{Side: "Ledger", SourceSystem: "GL", Items: []domain.PopulationItemInput{item("REF-1", 90)}},
	}
	itemOf := func(runID string) string {
		snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, runID)
		if err != nil || len(snaps) == 0 {
			t.Fatalf("snapshots of %s: %v", runID, err)
		}
		var id string
		if err := f.admin.QueryRow(f.ctx, `SELECT item_id FROM population_items WHERE snapshot_id = $1 LIMIT 1`,
			snaps[0].SnapshotID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	refresh := func() {
		d, err := f.s.GetDefinition(f.ctx, orgA, def.DefinitionID)
		if err != nil {
			t.Fatal(err)
		}
		def = d
	}
	defID := func() string { return def.DefinitionID }

	steps := []struct {
		name  string
		write func(domain.IdempotencyClaim) error
		want  wantEvent
	}{
		{"DefineReconciliation", func(c domain.IdempotencyClaim) (err error) {
			def, err = f.s.DefineReconciliation(f.ctx, orgA, defineRequest("outbox-every-write", 0), "definer", c)
			return err
		}, wantEvent{"reconciliation-definition.rule-version-published", "CONTROL.RuleVersionPublished", "reconciliation-definition", defID, "definer"}},
		{"ApproveRuleVersion", func(c domain.IdempotencyClaim) error {
			_, err := f.s.ApproveRuleVersion(f.ctx, orgA, def.DefinitionID, 1, "rule-approver", c)
			refresh()
			return err
		}, wantEvent{"reconciliation-definition.rule-version-approved", "CONTROL.RuleVersionApproved", "reconciliation-definition", defID, "rule-approver"}},
		{"CreateRuleVersion", func(c domain.IdempotencyClaim) error {
			v, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, f.nextVersion(def, nil), "definer", c)
			if err == nil {
				ruleVersion = v.RuleVersion
			}
			return err
		}, wantEvent{"reconciliation-definition.rule-version-published", "CONTROL.RuleVersionPublished", "reconciliation-definition", defID, "definer"}},
		{"ApproveRuleVersion v2", func(c domain.IdempotencyClaim) error {
			_, err := f.s.ApproveRuleVersion(f.ctx, orgA, def.DefinitionID, ruleVersion, "rule-approver", c)
			refresh()
			return err
		}, wantEvent{"reconciliation-definition.rule-version-approved", "CONTROL.RuleVersionApproved", "reconciliation-definition", defID, "rule-approver"}},
		{"RecordFailedRun", func(c domain.IdempotencyClaim) (err error) {
			failed, err = f.s.RecordFailedRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
				domain.RunFailure{Code: domain.FailureSourceUnavailable, Reason: "source down"}, "runner", c)
			return err
		}, wantEvent{"reconciliation-run.failed", "CONTROL.ReconciliationFailed", "reconciliation-run", func() string { return failed.RunID }, "runner"}},
		{"StartRun", func(c domain.IdempotencyClaim) (err error) {
			runA, err = f.s.StartRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
				frozenAll(cleanPops()), "runner", c)
			return err
		}, wantEvent{"reconciliation-run.started", "CONTROL.ReconciliationStarted", "reconciliation-run", func() string { return runA.RunID }, "runner"}},
		{"Match (clean)", func(c domain.IdempotencyClaim) error {
			_, err := f.s.Match(f.ctx, orgA, runA.RunID, domain.MatchRequest{}, "matcher", c)
			return err
		}, wantEvent{"reconciliation-run.completed", "CONTROL.ReconciliationCompleted", "reconciliation-run", func() string { return runA.RunID }, "matcher"}},
		{"RaiseException", func(c domain.IdempotencyClaim) error {
			ex, err := f.s.RaiseException(f.ctx, orgA, runA.RunID, domain.RaiseExceptionRequest{ItemID: itemOf(runA.RunID), Reason: "looks wrong",
				Category: "MISMATCH", Assertion: "ACCURACY"}, "reviewer", c)
			if err == nil {
				breakA = ex.ExceptionID
			}
			return err
		}, wantEvent{"reconciliation-break.detected", "CONTROL.ExceptionRaised", "reconciliation-break", func() string { return breakA }, "reviewer"}},
		{"Reperform", func(c domain.IdempotencyClaim) error {
			_, err := f.s.Reperform(f.ctx, orgA, runA.RunID, frozenAll(cleanPops()), "reperformer", c)
			return err
		}, wantEvent{"reconciliation-run.reperformed", "CONTROL.ReconciliationCompleted", "reconciliation-run", func() string { return runA.RunID }, "reperformer"}},
		{"StartRun (mismatch)", func(c domain.IdempotencyClaim) (err error) {
			runB, err = f.s.StartRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
				frozenAll(mismatch), "runner", c)
			return err
		}, wantEvent{"reconciliation-run.started", "CONTROL.ReconciliationStarted", "reconciliation-run", func() string { return runB.RunID }, "runner"}},
		{"Match (variance)", func(c domain.IdempotencyClaim) error {
			_, err := f.s.Match(f.ctx, orgA, runB.RunID, domain.MatchRequest{}, "matcher", c)
			if err == nil {
				breakB = f.exceptions(runB.RunID)[0].ExceptionID
			}
			return err
		}, wantEvent{"reconciliation-run.variance-found", "CONTROL.ExceptionRaised", "reconciliation-run", func() string { return runB.RunID }, "matcher"}},
		{"AssignException", func(c domain.IdempotencyClaim) error {
			_, err := f.s.AssignException(f.ctx, orgA, breakB, domain.AssignExceptionRequest{OwnerPrincipal: "olga", Reason: "AR team"}, "lead", c)
			return err
		}, wantEvent{"reconciliation-break.assigned", "CONTROL.ExceptionUpdated", "reconciliation-break", func() string { return breakB }, "lead"}},
		{"TransitionException", func(c domain.IdempotencyClaim) error {
			_, err := f.s.TransitionException(f.ctx, orgA, breakB, domain.TransitionExceptionRequest{ToState: domain.ExceptionInvestigating,
				Reason: "checking the invoice"}, "olga", c)
			return err
		}, wantEvent{"reconciliation-break.status-changed", "CONTROL.ExceptionUpdated", "reconciliation-break", func() string { return breakB }, "olga"}},
		{"StartRun (to certify)", func(c domain.IdempotencyClaim) (err error) {
			runC, err = f.s.StartRun(f.ctx, orgA, domain.StartRunRequest{DefinitionID: def.DefinitionID, PeriodID: "2026-09"}, def.Version,
				frozenAll(cleanPops()), "runner", c)
			return err
		}, wantEvent{"reconciliation-run.started", "CONTROL.ReconciliationStarted", "reconciliation-run", func() string { return runC.RunID }, "runner"}},
		{"Match (to certify)", func(c domain.IdempotencyClaim) error {
			_, err := f.s.Match(f.ctx, orgA, runC.RunID, domain.MatchRequest{}, "matcher", c)
			return err
		}, wantEvent{"reconciliation-run.completed", "CONTROL.ReconciliationCompleted", "reconciliation-run", func() string { return runC.RunID }, "matcher"}},
		{"Certify", func(c domain.IdempotencyClaim) error {
			cert, err := f.s.Certify(f.ctx, orgA, runC.RunID, "certifier", grant("certifier"), c)
			if err == nil {
				certification = cert.CertificationID
			}
			return err
		}, wantEvent{"reconciliation-certification.issued", "CONTROL.Certified", "reconciliation-certification", func() string { return certification }, "certifier"}},
		{"SupersedeRun", func(c domain.IdempotencyClaim) error {
			_, err := f.s.SupersedeRun(f.ctx, orgA, failed.RunID, domain.SupersedeRunRequest{NewRunID: runC.RunID, Reason: "source back, rerun"},
				"controller", c)
			return err
		}, wantEvent{"reconciliation-run.superseded", "CONTROL.ReconciliationSuperseded", "reconciliation-run", func() string { return failed.RunID }, "controller"}},
		{"RevokeCertification", func(c domain.IdempotencyClaim) error {
			_, err := f.s.RevokeCertification(f.ctx, orgA, runC.RunID, revokeRequest(), "controller-erin", grant("controller-erin"), c)
			return err
		}, wantEvent{"reconciliation-certification.revoked", "CONTROL.CertificationRevoked", "reconciliation-certification", func() string { return certification }, "controller-erin"}},
	}

	for i, st := range steps {
		claim := f.claim(st.name, "every-write")
		if err := st.write(claim); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		rows := f.outboxRows()
		if len(rows) != i+1 {
			t.Fatalf("%s: eventing_outbox has %d rows, want %d — each write enqueues exactly one event", st.name, len(rows), i+1)
		}
		got, w := rows[i], st.want
		aggID := w.aggregateID()
		checks := []struct {
			ok   bool
			what string
		}{
			{got.eventType == prefix+w.canonical, "event_type " + got.eventType + ", want " + prefix + w.canonical},
			{got.payload["event_type"] == w.legacy, "legacy event_type, want " + w.legacy},
			{got.tenantID == orgA && got.payload["tenant_id"] == orgA, "tenant " + got.tenantID + ", want the caller's tenant"},
			{got.payload["legal_entity_id"] == entityA, "legal_entity_id, want the definition's entity " + entityA},
			{got.region == testEventRegion, "region " + got.region},
			{got.aggregateID != nil && *got.aggregateID == aggID, "aggregate_id, want " + aggID},
			{got.partitionKey == expectedPartitionKey(orgA, w.aggregate, aggID), "partition_key, want " + w.aggregate + " " + aggID},
			{got.payload["actor_id"] == w.actor, "actor_id, want " + w.actor},
			{claim.CorrelationID != "" && got.payload["correlation_id"] == claim.CorrelationID, "correlation_id, want " + claim.CorrelationID},
			{got.payload["event_id"] == got.eventID && got.schemaVersion == "1.0.0", "event id / schema version"},
			{got.state == outbox.StatePending, "publish_state " + got.state},
		}
		for _, c := range checks {
			if !c.ok {
				t.Errorf("%s: %s (row %+v)", st.name, c.what, got)
			}
		}
	}
}

// The event is enqueued inside the write's own transaction: a transaction
// that fails after the enqueue leaves no event, and a write whose enqueue
// fails leaves no business row and no idempotency claim either.
func TestEventingOutbox_RolledBackTransactionLeavesNoEvent(t *testing.T) {
	f := newFixture(t)
	claim := f.claim("StartRun", "rollback")

	// 1. Forced failure after the enqueue. The in-transaction count proves the
	// row really was written, so this does not pass merely because the insert
	// never ran.
	forced := errors.New("forced failure after enqueue")
	err := f.s.WithTenantTx(f.ctx, orgA, func(tx pgx.Tx) error {
		runID := "run-" + uuid.NewString()
		if err := f.s.EnqueueRunStarted(f.ctx, tx, orgA, entityA, runID, "runner", claim, map[string]string{"run_id": runID}); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Fatalf("inside the transaction eventing_outbox has %d rows, want 1", n)
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("transaction returned %v, want the forced failure", err)
	}
	if n := f.count("eventing_outbox"); n != 0 {
		t.Fatalf("rolled-back transaction left %d eventing_outbox rows, want 0", n)
	}

	// 2. The real write path, failing at the enqueue: a store with no region
	// cannot build the event, and the definition, its rule version and its
	// idempotency claim must roll back with it.
	noRegion := store.NewPgStore(f.app)
	if _, err := noRegion.DefineReconciliation(f.ctx, orgA, defineRequest("outbox-rollback", 0), "definer",
		f.claim("DefineReconciliation", "rollback")); err == nil {
		t.Fatal("a write whose event cannot be built must fail")
	}
	for _, table := range []string{"eventing_outbox", "reconciliation_definitions", "reconciliation_rule_versions", "idempotency_keys"} {
		if n := f.count(table); n != 0 {
			t.Errorf("failed enqueue left %d %s rows, want 0 — the write and its event are one transaction", n, table)
		}
	}
}

// An idempotent replay returns the original result and enqueues no second
// event for the same fact.
func TestEventingOutbox_ReplayEnqueuesNoSecondEvent(t *testing.T) {
	f := newFixture(t)
	claim := f.claim("DefineReconciliation", "replay")
	if _, err := f.s.DefineReconciliation(f.ctx, orgA, defineRequest("outbox-replay", 0), "definer", claim); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var replay *domain.IdempotentReplayError
	if _, err := f.s.DefineReconciliation(f.ctx, orgA, defineRequest("outbox-replay", 0), "definer", claim); !errors.As(err, &replay) {
		t.Fatalf("same-key retry should replay, got %v", err)
	}
	if n := f.count("eventing_outbox"); n != 1 {
		t.Fatalf("replay enqueued another event: eventing_outbox has %d rows, want 1", n)
	}
}

// eventing_outbox is tenant-isolated for request transactions (the app role is
// NOBYPASSRLS): a tenant neither sees another tenant's events nor can write an
// event under another tenant's id.
func TestEventingOutbox_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.defineReconciliation(orgA, "outbox-rls", 0)
	if f.count("eventing_outbox") == 0 {
		t.Fatal("setup enqueued no event")
	}
	err := f.s.WithTenantTx(f.ctx, orgB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(f.ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("tenant B sees %d of tenant A's events, want 0", n)
		}
		_, err := tx.Exec(f.ctx, `INSERT INTO eventing_outbox (outbox_id, event_id, event_type, schema_version, tenant_id,
				region, partition_key, payload, payload_hash)
			VALUES ($1, 'evt-cross-tenant', 'com.zoikosuite.accounting.reconciliation-run.started', '1.0.0',
				$2, 'test-region', repeat('a', 64), '\x7b7d', 'sha256:x')`, uuid.New(), orgA)
		return err
	})
	// withTenant's mapErr keeps the Postgres message but not the PgError.
	if err == nil || !strings.Contains(err.Error(), `violates row-level security policy for table "eventing_outbox"`) {
		t.Fatalf("tenant B writing a tenant A event: %v, want the eventing_outbox RLS policy to refuse it", err)
	}
}

// recordingWriter is an outbox.Writer that records what the relay sends.
type recordingWriter struct {
	mu        sync.Mutex
	failCalls int // the first failCalls writes are rejected
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

// The shared relay, running as the NOBYPASSRLS app role, delivers every
// tenant's events (the app.outbox_relay disjunct of the policy): keyed on the
// stored partition key, carrying the stored event id as X-Event-ID, then
// marked published. A failed publish keeps the event for retry, id unchanged.
func TestEventingOutbox_SharedRelayDeliversServiceEvents(t *testing.T) {
	f := newFixture(t)
	f.defineReconciliation(orgA, "outbox-relay-a", 0)
	f.defineReconciliation(orgB, "outbox-relay-b", 0)
	stored := f.outboxRows()
	if len(stored) != 4 {
		t.Fatalf("setup enqueued %d events, want 4 (define + approve per tenant)", len(stored))
	}

	// Two failures: the relay first writes the whole batch, and when that fails
	// with the broker still up (Probe ok) it retries each message alone; the
	// second rejection fails the first of those, so it alone is retried.
	writer := &recordingWriter{failCalls: 2}
	relay, err := outbox.NewRelay(f.app, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	res, err := relay.DrainOnce(f.ctx)
	if err != nil || res.Claimed != 4 || res.Published != 3 || res.Retrying != 1 {
		t.Fatalf("first pass: res=%+v err=%v, want 4 claimed across both tenants, 3 published, 1 retrying", res, err)
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE eventing_outbox SET next_attempt_at = now() WHERE publish_state = 'failed'`); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if res, err := relay.DrainOnce(f.ctx); err != nil || res.Published != 1 {
		t.Fatalf("retry pass: res=%+v err=%v, want the failed event published", res, err)
	}

	byID := map[string]outboxRow{}
	for _, r := range stored {
		byID[r.eventID] = r
	}
	if len(writer.sent) != 4 {
		t.Fatalf("writer received %d messages, want 4", len(writer.sent))
	}
	for _, m := range writer.sent {
		r, ok := byID[headerEventID(m)]
		if !ok {
			t.Fatalf("delivered X-Event-ID %q is not a stored event id", headerEventID(m))
		}
		if string(m.Key) != r.partitionKey {
			t.Errorf("event %s: Kafka key %s, want stored partition key %s", r.eventID, m.Key, r.partitionKey)
		}
		delete(byID, r.eventID)
	}
	for _, r := range f.outboxRows() {
		if r.state != outbox.StatePublished {
			t.Errorf("event %s: publish_state %s, want published", r.eventID, r.state)
		}
	}
	if res, err := relay.DrainOnce(f.ctx); err != nil || res.Claimed != 0 {
		t.Fatalf("a published event was claimed again: res=%+v err=%v", res, err)
	}
}

// Migration 000012 carries every undelivered outbox_events row over, rebuilt
// as the body the old publisher sent, keeps outbox_events (evidence), and
// leaves already-published rows behind. A carried-over row lands on the same
// partition as new events for its aggregate, and the shared relay delivers it.
func TestEventingOutbox_MigrationCarriesOverUndeliveredLegacyRows(t *testing.T) {
	admin := openAdminPoolThrough(t, func(name string) bool { return name < "000012" })
	ctx := context.Background()

	type legacyRow struct {
		id                     uuid.UUID
		tenant, aggType, aggID string
		eventType              string
		published              bool
		wantAggregate          string
	}
	legacy := []legacyRow{
		{uuid.New(), orgA, "reconciliation_run", "run-1", "CONTROL.ReconciliationStarted", false, "reconciliation-run"},
		{uuid.New(), orgB, "reconciliation_exception", "rex-1", "CONTROL.ExceptionRaised", false, "reconciliation-break"},
		{uuid.New(), orgA, "certification", "rct-1", "CONTROL.Certified", false, "reconciliation-certification"},
		{uuid.New(), orgA, "reconciliation_definition", "rdf-1", "CONTROL.RuleVersionPublished", false, "reconciliation-definition"},
		{uuid.New(), orgA, "reconciliation_run", "run-2", "CONTROL.ReconciliationStarted", true, ""},
	}
	for _, l := range legacy {
		var publishedAt any
		if l.published {
			publishedAt = "2026-10-01T00:00:00Z"
		}
		if _, err := admin.Exec(ctx, `INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type, payload,
				tenant_id, published_at, publish_attempts, last_error)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 3, 'broker down')`,
			l.id, l.aggType, l.aggID, l.eventType, `{"id":"`+l.aggID+`","amount":100}`, l.tenant, publishedAt); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}

	applyMigrations(t, admin, func(name string) bool { return strings.HasPrefix(name, "000012") })

	var kept, carried int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&kept)
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&carried)
	if kept != 5 || carried != 4 {
		t.Fatalf("outbox_events=%d eventing_outbox=%d, want 5 kept as evidence and the 4 undelivered carried over", kept, carried)
	}
	for _, l := range legacy[:4] {
		eventID := "evt-" + l.id.String()
		var tenant, region, key, state, eventType, lastErr string
		var attempts int
		var payload []byte
		if err := admin.QueryRow(ctx, `SELECT tenant_id, region, partition_key, publish_state, event_type, attempt_count, last_error, payload
			FROM eventing_outbox WHERE event_id = $1`, eventID).
			Scan(&tenant, &region, &key, &state, &eventType, &attempts, &lastErr, &payload); err != nil {
			t.Fatalf("carried-over row %s: %v", eventID, err)
		}
		if tenant != l.tenant || region != "legacy" || state != outbox.StatePending || attempts != 0 ||
			eventType != l.eventType || lastErr != "broker down" {
			t.Errorf("carried-over row %s: tenant=%s region=%s state=%s attempts=%d type=%s last_error=%s",
				eventID, tenant, region, state, attempts, eventType, lastErr)
		}
		if want := expectedPartitionKey(l.tenant, l.wantAggregate, l.aggID); key != want {
			t.Errorf("carried-over row %s: partition key %s, want the %s key %s new events use", eventID, key, l.wantAggregate, want)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatalf("carried-over payload is not JSON: %v", err)
		}
		inner, _ := body["payload"].(map[string]any)
		if body["event_id"] != eventID || body["event_type"] != l.eventType || body["subject_id"] != l.aggID ||
			body["tenant_id"] != l.tenant || body["source_service"] != "reconciliation-engine-svc" || body["event_version"] != "1.0" ||
			body["occurred_at"] == nil || inner["id"] != l.aggID {
			t.Errorf("carried-over body %s is not the old publisher's shape: %v", eventID, body)
		}
	}

	// Deliver through the shared relay as the app role, as production does.
	app := appRolePool(t, admin)
	writer := &recordingWriter{}
	relay, err := outbox.NewRelay(app, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if res, err := relay.DrainOnce(ctx); err != nil || res.Published != 4 {
		t.Fatalf("relay over carried-over rows: res=%+v err=%v, want 4 published", res, err)
	}
	for _, m := range writer.sent {
		var body map[string]any
		if err := json.Unmarshal(m.Value, &body); err != nil {
			t.Fatalf("delivered body is not JSON: %v", err)
		}
		if id := headerEventID(m); id == "" || id != body["event_id"] {
			t.Errorf("carried-over event delivered with X-Event-ID %q but body event_id %v — consumer dedupe would break", id, body["event_id"])
		}
	}
}

// A legacy row whose aggregate type the carry-over does not map fails the
// migration (NOT NULL partition key) instead of being keyed on a guess, and
// the failed migration leaves nothing behind.
func TestEventingOutbox_MigrationRefusesUnmappedLegacyAggregate(t *testing.T) {
	admin := openAdminPoolThrough(t, func(name string) bool { return name < "000012" })
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload, tenant_id)
		VALUES ('mystery_aggregate', 'x-1', 'CONTROL.Unknown', '{}', $1)`, orgA); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	sql, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations/000012_add_eventing_outbox.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Exec(ctx, string(sql))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Fatalf("migration over an unmapped aggregate: %v, want a NOT NULL violation (23502)", err)
	}
	var exists bool
	_ = admin.QueryRow(ctx, `SELECT to_regclass('eventing_outbox') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("the failed migration left eventing_outbox behind; it must apply atomically")
	}
}
