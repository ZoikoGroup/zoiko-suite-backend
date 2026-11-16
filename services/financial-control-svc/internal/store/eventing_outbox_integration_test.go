//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	"zoiko.io/financial-control-svc/internal/store"
)

const testEventRegion = "test-region"

// legacyEvents presents eventing_outbox the way the pre-migration tests read
// outbox_events: event_type is the legacy (§26) name and payload the event
// data, so their assertions about what a command announced are unchanged.
const legacyEvents = `(SELECT e.event_type AS canonical_type, b.body ->> 'event_type' AS event_type, e.aggregate_id,
	e.tenant_id, e.created_at, e.outbox_id, b.body -> 'data' AS payload
	FROM eventing_outbox e, LATERAL (SELECT convert_from(e.payload, 'UTF8')::jsonb AS body) b) AS legacy_events`

// Migration 000012 must carry the library's schema and hardening verbatim; the
// shared relay and VerifySchema depend on that exact shape.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	raw, err := os.ReadFile("../../deployments/migrations/000012_add_eventing_outbox.up.sql")
	require.NoError(t, err)
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	assert.Contains(t, migration, strings.TrimSpace(outbox.SchemaSQL), "000012 must contain outbox.SchemaSQL verbatim")
	assert.Contains(t, migration, strings.TrimSpace(outbox.HardeningSQL), "000012 must contain outbox.HardeningSQL verbatim")
}

// The canonical type of every §26 event, written out independently of the
// mapping so a change to either side is caught here.
func TestEventingOutbox_CanonicalTypes(t *testing.T) {
	want := map[string]string{
		store.RunCreated:              "com.zoikosuite.control.run.created",
		store.PopulationFrozen:        "com.zoikosuite.control.population.frozen",
		store.ExecutionCompleted:      "com.zoikosuite.control.execution.completed",
		store.ExceptionOpened:         "com.zoikosuite.control.exception.opened",
		store.ExceptionAssigned:       "com.zoikosuite.control.exception.assigned",
		store.RemediationRecorded:     "com.zoikosuite.control.exception.remediation-recorded",
		store.RunReperformed:          "com.zoikosuite.control.run.reperformed",
		store.RunReadyForCert:         "com.zoikosuite.control.run.ready-for-certification",
		store.RunCertified:            "com.zoikosuite.control.run.certified",
		store.RunCertificationReject:  "com.zoikosuite.control.run.certification-rejected",
		store.RunFailed:               "com.zoikosuite.control.run.failed",
		store.CloseGateBlocked:        "com.zoikosuite.control.close-gate.blocked",
		store.CloseGateSatisfied:      "com.zoikosuite.control.close-gate.satisfied",
		store.ExceptionSLABreached:    "com.zoikosuite.control.exception.sla-breached",
		store.RunCertificationRevoked: "com.zoikosuite.control.run.certification-revoked",
		store.ExceptionWaived:         "com.zoikosuite.control.exception.waived",
		store.ExceptionCarried:        "com.zoikosuite.control.exception.carried-forward",
		store.ExceptionReperformed:    "com.zoikosuite.control.exception.reperformed",
		store.ExceptionClosed:         "com.zoikosuite.control.exception.closed",
		store.RunSuperseded:           "com.zoikosuite.control.run.superseded",
		store.RunExpired:              "com.zoikosuite.control.run.expired",
	}
	for legacy, canonical := range want {
		assert.Equal(t, canonical, store.CanonicalType(legacy), legacy)
		// The library accepts it: a type it rejected would fail every write.
		_, err := envelope.New(envelope.Spec{Type: canonical, LegacyType: legacy, Service: "financial-control-svc", SchemaVersion: "1.0.0",
			OccurredAt: time.Now(), TenantID: uuid.NewString(), ResidencyRegion: testEventRegion, Classification: envelope.Confidential,
			Data: map[string]any{}})
		assert.NoError(t, err, canonical)
	}
}

func engineInput(tenant, runID, actor, corr string) engine.Input {
	return engine.Input{TenantID: tenant, RunID: runID, Actor: actor, CorrelationID: corr}
}

// expectedPartitionKey is envelope.PartitionKey's formula, written out
// independently so a change on either side is caught here.
func expectedPartitionKey(tenantID, aggregateType, aggregateID string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + aggregateType + "\x00" + aggregateID))
	return hex.EncodeToString(sum[:])
}

type outboxRow struct {
	outboxID, eventID, canonical, legacy, tenant, region, partitionKey, state string
	aggregateID                                                               string
	aggregateVersion                                                          *int64
	body                                                                      map[string]any
}

// tenantEvents is every eventing_outbox row of one tenant, oldest first
// (outbox_id is a UUIDv7). Read as the superuser test pool.
func tenantEvents(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, tenant string) []outboxRow {
	t.Helper()
	rows, err := q.Query(ctx, `SELECT outbox_id::text, event_id, event_type, tenant_id, region, partition_key, publish_state,
			COALESCE(aggregate_id, ''), aggregate_version, payload
		FROM eventing_outbox WHERE tenant_id = $1 ORDER BY outbox_id`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		require.NoError(t, rows.Scan(&r.outboxID, &r.eventID, &r.canonical, &r.tenant, &r.region, &r.partitionKey, &r.state,
			&r.aggregateID, &r.aggregateVersion, &payload))
		require.NoError(t, json.Unmarshal(payload, &r.body))
		r.legacy, _ = r.body["event_type"].(string)
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// Every call site that announces a control fact enqueues exactly the events
// its command stands for — no more, no fewer — inside its own transaction,
// each canonically typed, keyed on its aggregate under the run's tenant, and
// stamped with the run's legal entity, the configured region, and the
// command's actor and correlation id. The steps reach all seven call sites:
// CreateRun, the advanceTx event loop, RevokeCertification, openException,
// AssignException, ResolveException and the SLA sweep.
func TestEventingOutbox_EveryCallSiteEnqueuesItsEventsCorrectlyKeyed(t *testing.T) {
	w := newWave5(t)
	seen := 0
	var runs []string
	exceptionRun := map[string]string{}

	// step runs one command and checks the events it added, in order.
	step := func(name, actor, corr string, do func(), want ...string) []outboxRow {
		t.Helper()
		do()
		all := tenantEvents(t, testPool, w.tenant)
		added := all[seen:]
		seen = len(all)
		var got []string
		for _, r := range added {
			got = append(got, r.legacy)
		}
		require.Equal(t, want, got, "%s: events added", name)
		for _, r := range added {
			assert.Equal(t, store.CanonicalType(r.legacy), r.canonical, "%s: canonical type", name)
			assert.Equal(t, w.tenant, r.tenant, "%s: tenant", name)
			assert.Equal(t, w.entity, r.body["legalentityid"], "%s: legal entity", name)
			assert.Equal(t, w.entity, r.body["legal_entity_id"], "%s: legacy legal entity", name)
			assert.Equal(t, testEventRegion, r.region, "%s: region", name)
			assert.Equal(t, actor, r.body["actor_id"], "%s: actor", name)
			assert.Equal(t, corr, r.body["correlation_id"], "%s: correlation", name)
			assert.Equal(t, r.eventID, r.body["event_id"], "%s: event id", name)
			assert.Equal(t, outbox.StatePending, r.state, "%s: state", name)
			data, _ := r.body["data"].(map[string]any)
			if strings.HasPrefix(r.legacy, "control.exception.") {
				_, known := exceptionRun[r.aggregateID]
				assert.True(t, known, "%s: aggregate %s is not one of the run's exceptions", name, r.aggregateID)
				assert.Equal(t, expectedPartitionKey(w.tenant, "control-exception", r.aggregateID), r.partitionKey, "%s: exception key", name)
				assert.Nil(t, r.aggregateVersion, "%s: exception events carry no aggregate version", name)
			} else {
				assert.Contains(t, runs, r.aggregateID, "%s: aggregate is a run", name)
				assert.Equal(t, expectedPartitionKey(w.tenant, "control-run", r.aggregateID), r.partitionKey, "%s: run key", name)
				run, err := testStore.GetRun(ctx, w.tenant, r.aggregateID)
				require.NoError(t, err)
				if assert.NotNil(t, r.aggregateVersion, "%s: run events carry the run version", name) {
					if v, ok := data["version"].(float64); ok {
						assert.Equal(t, int64(v), *r.aggregateVersion, "%s: version matches the payload's", name)
					}
					assert.LessOrEqual(t, *r.aggregateVersion, int64(run.Version), "%s: version is a committed one", name)
				}
			}
		}
		return added
	}

	// A passing run: create, execute, certify, revoke.
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	var run *domain.ControlRun
	step("CreateRun", "prep", "corr-create", func() {
		r, _, err := testStore.CreateRun(ctx, w.tenant, "prep", "corr-create", "create-"+uuid.NewString(),
			domain.CreateRunRequest{ControlDefinitionID: w.defs["FIN-CTRL-020"], LegalEntityID: w.entity, TriggerType: "PERIOD_END",
				PeriodID: "2026-09", Scope: json.RawMessage(`{"fiscal_period":"2026-09"}`)})
		require.NoError(t, err)
		run = r
		runs = append(runs, r.RunID)
	}, store.RunCreated)
	step("BeginExecution + Execute", "prep", "corr-exec", func() {
		_, _, err := testStore.BeginExecution(ctx, w.tenant, run.RunID, "exec-"+uuid.NewString(), "prep", "corr-exec", store.SkipVersionCheck)
		require.NoError(t, err)
		run, err = w.exec.Execute(ctx, engineInput(w.tenant, run.RunID, "prep", "corr-exec"))
		require.NoError(t, err)
	}, store.PopulationFrozen, store.ExecutionCompleted, store.RunReadyForCert)
	step("CertifyRun", "checker", "corr-certify", func() {
		var err error
		run, err = certifyRun(ctx, w.tenant, run.RunID, "checker", "corr-certify", run.Version,
			domain.CertifyRequest{Decision: "CERTIFY", Reason: "reviewed"}, testAuthority("checker", w.tenant))
		require.NoError(t, err)
	}, store.RunCertified)
	step("RevokeCertification", "auditor", "corr-revoke", func() {
		_, _, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "corr-revoke", "revoke-"+uuid.NewString(), run.Version, revokeReq)
		require.NoError(t, err)
	}, store.RunCertificationRevoked)

	// A failing run: its exceptions are opened, assigned, worked, waived, and
	// the one left open breaches its SLA.
	w.src.set(catalogue.SysGL, "control-account-postings", "wm", r("e1", "J-1", "250.00"), r("e2", "J-2", "40.00"))
	var failing *domain.ControlRun
	step("CreateRun (failing)", "prep", "corr-create-2", func() {
		f, _, err := testStore.CreateRun(ctx, w.tenant, "prep", "corr-create-2", "create-"+uuid.NewString(),
			domain.CreateRunRequest{ControlDefinitionID: w.defs["FIN-CTRL-021"], LegalEntityID: w.entity, TriggerType: "PERIOD_END",
				PeriodID: "2026-09", Scope: json.RawMessage(`{"fiscal_period":"2026-09"}`)})
		require.NoError(t, err)
		failing = f
		runs = append(runs, f.RunID)
	}, store.RunCreated)
	added := step("BeginExecution + Execute (failing)", "prep", "corr-exec-2", func() {
		_, _, err := testStore.BeginExecution(ctx, w.tenant, failing.RunID, "exec-"+uuid.NewString(), "prep", "corr-exec-2", store.SkipVersionCheck)
		require.NoError(t, err)
		list, err := w.exec.Execute(ctx, engineInput(w.tenant, failing.RunID, "prep", "corr-exec-2"))
		require.NoError(t, err)
		failing = list
		exs, err := testStore.ListExceptions(ctx, w.tenant, failing.RunID, store.ListExceptionsFilter{})
		require.NoError(t, err)
		for _, e := range exs {
			exceptionRun[e.ExceptionID] = failing.RunID
		}
	}, store.PopulationFrozen, store.ExceptionOpened, store.ExceptionOpened, store.ExecutionCompleted)
	require.Len(t, exceptionRun, 2)
	var opened []string
	for _, a := range added {
		if a.legacy == store.ExceptionOpened {
			opened = append(opened, a.aggregateID)
		}
	}
	sort.Strings(opened)
	require.Len(t, opened, 2)
	assert.NotEqual(t, opened[0], opened[1], "each opened event is keyed on its own exception")

	exs, err := testStore.ListExceptions(ctx, w.tenant, failing.RunID, store.ListExceptionsFilter{})
	require.NoError(t, err)
	e := exs[0]
	step("AssignException", "lead", "corr-assign", func() {
		a, err := testStore.AssignException(ctx, w.tenant, e.ExceptionID, "lead", "corr-assign", e.Version,
			domain.AssignExceptionRequest{OwnerPrincipalID: "olivia", Reason: "owner"})
		require.NoError(t, err)
		e = *a
	}, store.ExceptionAssigned)
	step("ResolveException INVESTIGATING", "olivia", "corr-investigate", func() {
		out, err := resolveException(ctx, w.tenant, e.ExceptionID, "olivia", "corr-investigate", e.Version,
			domain.ResolveExceptionRequest{ToState: "INVESTIGATING", Reason: "looking"})
		require.NoError(t, err)
		e = *out
	}) // working the exception is not a §26 fact: no event
	step("ResolveException WAIVED", "sam", "corr-waive", func() {
		out, err := resolveException(ctx, w.tenant, e.ExceptionID, "sam", "corr-waive", e.Version,
			domain.ResolveExceptionRequest{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "immaterial", AuthorityRef: "waiver:memo-7"})
		require.NoError(t, err)
		e = *out
	}, store.ExceptionWaived)
	step("Sweep (SLA breach)", "system:sweeper", "sweep", func() {
		_, err := testStore.Sweep(ctx, time.Now().UTC().Add(60*24*time.Hour), 24*time.Hour, 500)
		require.NoError(t, err)
	}, store.ExceptionSLABreached) // only the exception still open; the waived one is final
}

// The event is enqueued inside the write's own transaction: a transaction that
// fails after the enqueue leaves no event, and a command whose event cannot be
// built leaves no run, no idempotency record and no event.
func TestEventingOutbox_RolledBackTransactionLeavesNoEvent(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")

	// 1. Forced failure after the enqueue; the in-transaction count proves the
	// row really was written first.
	forced := errors.New("forced failure after enqueue")
	err := testStore.WithRLSTx(ctx, tenant, func(tx pgx.Tx) error {
		runID := uuid.NewString()
		require.NoError(t, testStore.EmitRunEvent(ctx, tx, domain.ControlRun{RunID: runID, TenantID: tenant, LegalEntityID: entity, Version: 1},
			store.RunCreated, "prep", "corr-rollback", map[string]any{"run_id": runID}))
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenant).Scan(&n))
		require.Equal(t, 1, n, "inside the transaction the event is there")
		return forced
	})
	require.ErrorIs(t, err, forced)
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenant))

	// 2. The real command path, failing at the enqueue: a store with no event
	// profile cannot build the event, and the run and its idempotency record
	// must roll back with it.
	unprofiled := store.New(testPool, zap.NewNop())
	key := "create-" + uuid.NewString()
	_, _, err = unprofiled.CreateRun(ctx, tenant, "prep", "corr", key, runReq(d.ControlDefinitionID, entity))
	require.Error(t, err, "a command whose event cannot be built must fail")
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM control_runs WHERE tenant_id = $1`, tenant))
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM control_run_transitions WHERE tenant_id = $1`, tenant))
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenant))
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM control_runs WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key))

	// The same command through the profiled store succeeds: the failure was the event.
	_, _, err = testStore.CreateRun(ctx, tenant, "prep", "corr", key, runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenant))
}

// A replayed command returns the original result and enqueues nothing.
func TestEventingOutbox_ReplayEnqueuesNoSecondEvent(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	d := approvedDef(t, tenant, "FIN-CTRL-001")
	key := "create-" + uuid.NewString()
	first, created, err := testStore.CreateRun(ctx, tenant, "prep", "corr", key, runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.True(t, created)
	again, created, err := testStore.CreateRun(ctx, tenant, "prep", "corr", key, runReq(d.ControlDefinitionID, entity))
	require.NoError(t, err)
	require.False(t, created, "same key, same request replays the original run")
	assert.Equal(t, first.RunID, again.RunID)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1`, tenant))
}

// ── Fresh databases, for tests that need the outbox to themselves ──────────

var appRoleOnce sync.Once

// freshDB creates an empty database on the test server, applies the up
// migrations include accepts, and returns a superuser pool on it.
func freshDB(t *testing.T, include func(name string) bool) *pgxpool.Pool {
	t.Helper()
	name := "fc_evt_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := testPool.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	cfg := testPool.Config().Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, _ = testPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	applyMigrations(t, pool, include)
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool, include func(name string) bool) {
	t.Helper()
	files, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	require.NoError(t, err)
	sort.Strings(files)
	for _, f := range files {
		if !include(filepath.Base(f)) {
			continue
		}
		sqlBytes, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sqlBytes))
		require.NoError(t, err, filepath.Base(f))
	}
}

// appRolePool connects to admin's database as an ordinary role — NOSUPERUSER
// NOBYPASSRLS, not the owner, DML only — the shape
// deployments/scripts/create-app-roles.sh gives the service in production, so
// row-level security applies to it.
func appRolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	const role, password = "fc_app_test", "fc_app_test_pw"
	appRoleOnce.Do(func() {
		_, err := testPool.Exec(ctx, `DO $do$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+role+`') THEN
				CREATE ROLE `+role+` LOGIN PASSWORD '`+password+`' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
			END IF;
		END $do$`)
		require.NoError(t, err)
	})
	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + role,
	} {
		_, err := admin.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, password
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var bypass bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass))
	require.False(t, bypass, "the app role must not bypass RLS")
	return pool
}

// runAs creates and approves a definition (as the superuser), then creates a
// run through appStore under tenantArg — the tenant exactly as a request
// supplied it.
func runAs(t *testing.T, admin *pgxpool.Pool, appStore *store.PgStore, tenant, tenantArg string) *domain.ControlRun {
	t.Helper()
	adminStore := store.New(admin, zap.NewNop(), store.WithEventProfile(testEventRegion, envelope.Confidential))
	d, err := adminStore.CreateDefinition(ctx, tenant, "maker", "c", newDef("FIN-CTRL-001"))
	require.NoError(t, err)
	_, err = adminStore.ApproveRuleVersion(ctx, tenant, d.ControlDefinitionID, 1, "checker")
	require.NoError(t, err)
	run, _, err := appStore.CreateRun(ctx, tenantArg, "prep", "corr", "create-"+uuid.NewString(), runReq(d.ControlDefinitionID, uuid.NewString()))
	require.NoError(t, err)
	return run
}

// eventing_outbox carries tenant isolation (decision #1). As the app role:
//   - a command writes its own tenant's event, including when the request
//     spelled the tenant UUID in upper case (the policy compares as UUID, like
//     every other policy here);
//   - another tenant can neither see that event nor write one under it;
//   - only the relay's app.outbox_relay marker sees every tenant.
func TestEventingOutbox_TenantIsolationAsAppRole(t *testing.T) {
	admin := freshDB(t, func(string) bool { return true })
	app := appRolePool(t, admin)
	appStore := store.New(app, zap.NewNop(), store.WithEventProfile(testEventRegion, envelope.Confidential))

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	runA := runAs(t, admin, appStore, tenantA, strings.ToUpper(tenantA))
	runAs(t, admin, appStore, tenantB, tenantB)
	require.Len(t, tenantEvents(t, admin, tenantA), 1)
	assert.Equal(t, runA.RunID, tenantEvents(t, admin, tenantA)[0].aggregateID)

	err := appStore.WithRLSTx(ctx, tenantB, func(tx pgx.Tx) error {
		var visible int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&visible))
		assert.Equal(t, 1, visible, "tenant B sees only its own event")
		_, err := tx.Exec(ctx, `INSERT INTO eventing_outbox (outbox_id, event_id, event_type, schema_version, tenant_id,
				region, partition_key, payload, payload_hash)
			VALUES ($1, 'evt-cross-tenant', 'com.zoikosuite.control.run.created', '1.0.0', $2, 'test-region',
				repeat('a', 64), '\x7b7d', 'sha256:x')`, uuid.New(), tenantA)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `row-level security policy for table "eventing_outbox"`)

	var all int
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&all))
	assert.Equal(t, 0, all, "no tenant scope and no relay marker: nothing visible")
	_, err = tx.Exec(ctx, `SELECT set_config('app.outbox_relay', 'true', true)`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&all))
	assert.Equal(t, 2, all, "the relay marker sees every tenant")
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

// The shared relay, running as the app role, delivers both tenants' events
// keyed on their stored partition keys with the stored event id as X-Event-ID,
// and a failed publish is retried under the same id.
func TestEventingOutbox_SharedRelayDeliversServiceEvents(t *testing.T) {
	admin := freshDB(t, func(string) bool { return true })
	app := appRolePool(t, admin)
	appStore := store.New(app, zap.NewNop(), store.WithEventProfile(testEventRegion, envelope.Confidential))
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	runAs(t, admin, appStore, tenantA, tenantA)
	runAs(t, admin, appStore, tenantB, tenantB)
	stored := map[string]outboxRow{}
	for _, tn := range []string{tenantA, tenantB} {
		for _, r := range tenantEvents(t, admin, tn) {
			stored[r.eventID] = r
		}
	}
	require.Len(t, stored, 2)

	// Two failures: the batch write, then the first single-message retry the
	// relay makes when the broker probe still succeeds.
	writer := &recordingWriter{failCalls: 2}
	relay, err := outbox.NewRelay(app, writer, outbox.DefaultConfig(), zap.NewNop())
	require.NoError(t, err)
	res, err := relay.DrainOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, res.Claimed, "the relay claims both tenants' events")
	require.Equal(t, 1, res.Published)
	require.Equal(t, 1, res.Retrying)
	_, err = admin.Exec(ctx, `UPDATE eventing_outbox SET next_attempt_at = now() WHERE publish_state = 'failed'`)
	require.NoError(t, err)
	res, err = relay.DrainOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.Published)

	require.Len(t, writer.sent, 2)
	for _, m := range writer.sent {
		r, ok := stored[headerEventID(m)]
		require.True(t, ok, "X-Event-ID %q is a stored event id", headerEventID(m))
		assert.Equal(t, r.partitionKey, string(m.Key))
	}
	for _, tn := range []string{tenantA, tenantB} {
		for _, r := range tenantEvents(t, admin, tn) {
			assert.Equal(t, outbox.StatePublished, r.state)
		}
	}
	res, err = relay.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Claimed, "a published event is never claimed again")
}

// Migration 000012 carries every undelivered outbox_events row over, rebuilt
// as the body the old publisher sent, with its UUID tenant and entity cast to
// text; it keeps outbox_events, leaves delivered rows behind, quarantines the
// old relay's dead letters, and the shared relay delivers the rest under their
// original event ids.
func TestEventingOutbox_MigrationCarriesOverUndeliveredLegacyRows(t *testing.T) {
	admin := freshDB(t, func(name string) bool { return name < "000012" })

	type legacyRow struct {
		id                     uuid.UUID
		tenant, entity         string
		aggType, aggID, evType string
		payload                string
		published              bool
		attempts               int
		wantAggregate          string
	}
	// Upper-case UUID text on purpose: the UUID columns normalise it, and the
	// ::text cast must carry the canonical lower-case form.
	tenant, entity := strings.ToUpper(uuid.NewString()), strings.ToUpper(uuid.NewString())
	runID, exID := uuid.NewString(), uuid.NewString()
	legacy := []legacyRow{
		{uuid.New(), tenant, entity, "control_run", runID, "control.run.certified", `{"run_id":"` + runID + `","version":7}`, false, 2, "control-run"},
		{uuid.New(), tenant, entity, "control_exception", exID, "control.exception.opened", `{"exception_id":"` + exID + `"}`, false, 0, "control-exception"},
		{uuid.New(), tenant, entity, "control_run", runID, "control.run.failed", `{"run_id":"` + runID + `"}`, false, 10, "control-run"},
		{uuid.New(), tenant, entity, "control_run", runID, "control.run.created", `{"run_id":"` + runID + `"}`, true, 1, ""},
	}
	for _, l := range legacy {
		var publishedAt any
		if l.published {
			publishedAt = time.Now().UTC()
		}
		_, err := admin.Exec(ctx, `INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type, tenant_id,
				legal_entity_id, actor_id, correlation_id, payload, published_at, publish_attempts, last_error)
			VALUES ($1, $2, $3, $4, $5, $6, 'prep', 'corr-legacy', $7, $8, $9, 'broker down')`,
			l.id, l.aggType, l.aggID, l.evType, l.tenant, l.entity, l.payload, publishedAt, l.attempts)
		require.NoError(t, err)
	}

	applyMigrations(t, admin, func(name string) bool { return strings.HasPrefix(name, "000012") })

	var kept, carried int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&kept))
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM eventing_outbox`).Scan(&carried))
	require.Equal(t, 4, kept, "outbox_events is kept as evidence")
	require.Equal(t, 3, carried, "every undelivered row is carried over, and only those")

	lowerTenant, lowerEntity := strings.ToLower(tenant), strings.ToLower(entity)
	for _, l := range legacy[:3] {
		var tn, region, key, state, evType, lastErr string
		var errCode *string
		var attempts int
		var version *int64
		var payload []byte
		require.NoError(t, admin.QueryRow(ctx, `SELECT tenant_id, region, partition_key, publish_state, event_type, attempt_count,
				last_error_code, last_error, aggregate_version, payload
			FROM eventing_outbox WHERE event_id = $1`, l.id.String()).
			Scan(&tn, &region, &key, &state, &evType, &attempts, &errCode, &lastErr, &version, &payload), l.evType)
		assert.Equal(t, lowerTenant, tn, "tenant cast to canonical text")
		assert.Equal(t, "legacy", region)
		assert.Equal(t, l.evType, evType)
		assert.Equal(t, "broker down", lastErr)
		assert.Equal(t, expectedPartitionKey(lowerTenant, l.wantAggregate, l.aggID), key, "keyed like new events for %s", l.wantAggregate)
		if l.attempts >= 10 {
			assert.Equal(t, outbox.StateQuarantined, state, "a dead letter needs an owner, not an eleventh try")
			assert.Equal(t, 10, attempts)
			if assert.NotNil(t, errCode) {
				assert.Equal(t, "MIGRATED_DEAD_LETTER", *errCode)
			}
		} else {
			assert.Equal(t, outbox.StatePending, state)
			assert.Zero(t, attempts)
		}
		var body map[string]any
		require.NoError(t, json.Unmarshal(payload, &body))
		assert.Equal(t, l.id.String(), body["event_id"])
		assert.Equal(t, l.evType, body["event_type"])
		assert.Equal(t, "financial-control-svc", body["source_service"])
		assert.Equal(t, lowerTenant, body["tenant_id"])
		assert.Equal(t, lowerEntity, body["legal_entity_id"], "entity cast to canonical text")
		assert.Equal(t, "prep", body["actor_id"])
		assert.Equal(t, "corr-legacy", body["correlation_id"])
		assert.NotEmpty(t, body["emitted_at"])
		assert.Equal(t, l.aggID, body["aggregate_id"])
		inner, _ := body["payload"].(map[string]any)
		assert.NotEmpty(t, inner, "the old data rides as payload")
		if strings.Contains(l.payload, `"version"`) {
			if assert.NotNil(t, version) {
				assert.Equal(t, int64(7), *version)
			}
			assert.Equal(t, float64(7), body["aggregate_version"])
		} else {
			assert.Nil(t, version)
		}
	}

	app := appRolePool(t, admin)
	writer := &recordingWriter{}
	relay, err := outbox.NewRelay(app, writer, outbox.DefaultConfig(), zap.NewNop())
	require.NoError(t, err)
	res, err := relay.DrainOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, res.Published, "the two pending rows are delivered; the quarantined one waits for an owner")
	for _, m := range writer.sent {
		var body map[string]any
		require.NoError(t, json.Unmarshal(m.Value, &body))
		assert.Equal(t, body["event_id"], headerEventID(m), "X-Event-ID is the old outbox id, so consumers dedupe redeliveries")
	}
}

// An old row whose aggregate type the carry-over does not map fails the
// migration instead of being keyed on a guess, and nothing is left behind.
func TestEventingOutbox_MigrationRefusesUnmappedLegacyAggregate(t *testing.T) {
	admin := freshDB(t, func(name string) bool { return name < "000012" })
	_, err := admin.Exec(ctx, `INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, payload)
		VALUES ('control_gate', 'g-1', 'control.close_gate.blocked', $1, $2, '{}')`, uuid.New(), uuid.New())
	require.NoError(t, err)
	sqlBytes, err := os.ReadFile("../../deployments/migrations/000012_add_eventing_outbox.up.sql")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, string(sqlBytes))
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "migration error: %v", err)
	assert.Equal(t, "23502", pgErr.Code, "NOT NULL partition key: %s", fmt.Sprint(err))
	var exists bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT to_regclass('eventing_outbox') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "the failed migration applies atomically: no eventing_outbox left behind")
}
