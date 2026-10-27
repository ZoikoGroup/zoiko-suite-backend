package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
	"zoiko.io/policy-svc/internal/outbox"
	"zoiko.io/policy-svc/internal/resolve"
	"zoiko.io/policy-svc/internal/store"
)

// The suite needs a Postgres it may wipe: TEST_DATABASE_URL, as a superuser.
// Migrations run as that superuser; the store runs as policy_svc_test, a
// NOSUPERUSER NOBYPASSRLS role, because FORCE ROW LEVEL SECURITY is invisible
// to a superuser and a superuser-only suite once hid an RLS 503 elsewhere.

const appRole = "policy_svc_test"

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entityA = "33333333-3333-3333-3333-333333333333"
)

type testEnv struct {
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	s     *store.PgStore
	clock time.Time
}

func adminDSN(t *testing.T) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		// A silent skip turns the whole suite into nothing while `go test`
		// still prints ok: fail wherever the run claims to verify.
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS is")
		}
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	return dsn
}

func migrationFiles(t *testing.T) []string {
	files, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	return files
}

// applyMigrations runs every *.up.sql whose name sorts before stop (all when
// stop is "").
func applyMigrations(t *testing.T, admin *pgxpool.Pool, from, stop string) {
	t.Helper()
	for _, f := range migrationFiles(t) {
		base := filepath.Base(f)
		if (from != "" && base < from) || (stop != "" && base >= stop) {
			continue
		}
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("migration %s: %v", base, err)
		}
	}
}

func resetSchema(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS public CASCADE`,
		`CREATE SCHEMA public`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + appRole + `') THEN
				CREATE ROLE ` + appRole + ` LOGIN PASSWORD '` + appRole + `' NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$`,
		`GRANT USAGE ON SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("reset: %v", err)
		}
	}
}

func grantApp(t *testing.T, admin *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO ` + appRole,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + appRole,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
}

func connectApp(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = appRole
	cfg.ConnConfig.Password = appRole
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := adminDSN(t)
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	resetSchema(t, admin)
	applyMigrations(t, admin, "", "")
	grantApp(t, admin)
	app := connectApp(t, dsn)
	// Microsecond precision: Postgres truncates timestamps to it.
	e := &testEnv{admin: admin, app: app, s: store.New(app, zap.NewNop()), clock: time.Now().UTC().Truncate(time.Microsecond)}
	e.s.SetClock(func() time.Time { return e.clock })
	t.Cleanup(func() { app.Close(); admin.Close() })
	return e
}

func (e *testEnv) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func as(tenant string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenant)
}

func sp(s string) *string { return &s }

func (e *testEnv) family(t *testing.T, tenant string, owner *string, code string) *domain.Policy {
	t.Helper()
	p, created, err := e.s.CreatePolicy(as(tenant), domain.CreatePolicyParams{
		PolicyCode: code, PolicyName: code, PolicyType: "APPROVAL_THRESHOLD", TenantID: owner, CreatedByPrincipalID: "author",
	})
	if err != nil || !created {
		t.Fatalf("create family %s: created=%v err=%v", code, created, err)
	}
	return p
}

func (e *testEnv) draft(t *testing.T, tenant string, p *domain.Policy, scopeTenant, entity *string, effective time.Time, payload string) *domain.PolicyVersion {
	t.Helper()
	v, created, err := e.s.CreatePolicyVersion(as(tenant), domain.CreatePolicyVersionParams{
		PolicyID: p.PolicyID, TenantID: scopeTenant, LegalEntityID: entity,
		RulePayload: []byte(payload), EffectiveFrom: effective, CreatedByPrincipalID: "author",
		RequestKey: uuid.NewString(),
	})
	if err != nil || !created {
		t.Fatalf("create version: created=%v err=%v", created, err)
	}
	return v
}

func (e *testEnv) cmd(t *testing.T, tenant string, v *domain.PolicyVersion, command, actor string) (*domain.TransitionResult, error) {
	t.Helper()
	return e.s.Transition(as(tenant), domain.TransitionCommand{
		PolicyVersionID: v.PolicyVersionID, Command: command, ActorID: actor, Reason: "test",
		Meta: domain.EventMeta{CorrelationID: "corr", IdempotencyKey: uuid.NewString()},
	})
}

// release takes a draft through review, approval and activation.
func (e *testEnv) release(t *testing.T, tenant string, v *domain.PolicyVersion) *domain.PolicyVersion {
	t.Helper()
	for _, step := range []struct{ cmd, actor string }{
		{domain.CommandSubmitReview, "author"},
		{domain.CommandApprove, "approver"},
		{domain.CommandActivate, "activator"},
	} {
		res, err := e.cmd(t, tenant, v, step.cmd, step.actor)
		if err != nil {
			t.Fatalf("%s: %v", step.cmd, err)
		}
		v = res.Version
	}
	return v
}

func (e *testEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// ── migration 000007 on existing data ────────────────────────────────────────

func TestMigration000007_BackfillsLegacyRows(t *testing.T) {
	dsn := adminDSN(t)
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	resetSchema(t, admin)
	applyMigrations(t, admin, "", "000007")

	ctx := context.Background()
	pid := uuid.NewString()
	if _, err := admin.Exec(ctx, `INSERT INTO policies (policy_id, policy_code, policy_name, policy_type, created_by_principal_id)
		VALUES ($1, 'LEGACY', 'legacy', 'APPROVAL_THRESHOLD', 'author')`, pid); err != nil {
		t.Fatal(err)
	}
	// Two legacy versions, both numbered 1 by 000006's backfill, rationale NULL.
	for i, status := range []string{"SUPERSEDED", "ACTIVE"} {
		if _, err := admin.Exec(ctx, `INSERT INTO policy_versions (policy_id, tenant_id, rule_payload, effective_from,
				version_status, version_number, created_by_principal_id, scope_type, activated_at, created_at)
			VALUES ($1, $2, '{"threshold_amount": 5000}', $3, $4, 1, 'author', 'TENANT', $3, $3)`,
			pid, tenantA, time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC), status); err != nil {
			t.Fatal(err)
		}
	}

	applyMigrations(t, admin, "000007", "")
	grantApp(t, admin)
	app := connectApp(t, dsn)
	defer app.Close()
	s := store.New(app, zap.NewNop())

	history, err := s.ListVersionHistory(as(tenantA), pid)
	if err != nil {
		t.Fatalf("legacy rows (NULL rationale) must stay readable: %v", err)
	}
	if len(history) != 2 || history[0].VersionNumber != 2 || history[1].VersionNumber != 1 {
		t.Fatalf("expected versions renumbered 2,1 newest first, got %+v", history)
	}
	for _, v := range history {
		sum := sha256.Sum256(v.RulePayload)
		if v.ArtifactDigest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("backfilled digest must match the bytes read back: %s vs %x", v.ArtifactDigest, sum)
		}
	}
	// The legacy ACTIVE version still resolves.
	cands, err := s.ResolutionCandidates(as(tenantA), "APPROVAL_THRESHOLD", sp(tenantA), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := resolve.Resolve(cands, time.Now(), time.Now()); r.Status != resolve.Resolved || r.Selected.VersionStatus != "ACTIVE" {
		t.Fatalf("expected the legacy ACTIVE version to govern, got %s", r.Status)
	}

	// And the migrations reverse (newest first) while no GOV-05-only state
	// exists, then apply again.
	downs, err := filepath.Glob("../../deployments/migrations/*.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	for _, f := range downs {
		if filepath.Base(f) < "000007" {
			continue
		}
		down, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, string(down)); err != nil {
			t.Fatalf("down %s: %v", filepath.Base(f), err)
		}
	}
	applyMigrations(t, admin, "000007", "")
}

// ── families ─────────────────────────────────────────────────────────────────

// Audit Q5 (decided 14 Oct): families are tenant-owned; policy_code is unique
// per owner and invisible across tenants.
func TestPolicies_TenantOwnedAndIsolated(t *testing.T) {
	e := newEnv(t)
	a := e.family(t, tenantA, sp(tenantA), "AP-5K")

	b, created, err := e.s.CreatePolicy(as(tenantB), domain.CreatePolicyParams{
		PolicyCode: "AP-5K", PolicyName: "AP-5K", PolicyType: "APPROVAL_THRESHOLD", TenantID: sp(tenantB), CreatedByPrincipalID: "x",
	})
	if err != nil || !created || b.PolicyID == a.PolicyID {
		t.Fatalf("tenant B must get its own family, got created=%v err=%v", created, err)
	}
	if _, err := e.s.FindPolicyByID(as(tenantB), a.PolicyID); !errors.Is(err, domain.ErrPolicyNotFound) {
		t.Fatalf("tenant B must not see tenant A's family, got %v", err)
	}

	replay, created, err := e.s.CreatePolicy(as(tenantA), domain.CreatePolicyParams{
		PolicyCode: "AP-5K", PolicyName: "AP-5K", PolicyType: "APPROVAL_THRESHOLD", TenantID: sp(tenantA), CreatedByPrincipalID: "x",
	})
	if err != nil || created || replay.PolicyID != a.PolicyID {
		t.Fatalf("identical repeat must replay, got created=%v err=%v", created, err)
	}
	if _, _, err := e.s.CreatePolicy(as(tenantA), domain.CreatePolicyParams{
		PolicyCode: "AP-5K", PolicyName: "different", PolicyType: "APPROVAL_THRESHOLD", TenantID: sp(tenantA), CreatedByPrincipalID: "x",
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("same code, different name must conflict, got %v", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.created'`); n != 2 {
		t.Fatalf("expected one policy.created per real create, got %d", n)
	}
}

// ── versions ─────────────────────────────────────────────────────────────────

func TestVersions_NumberingReplayAndScope(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	key := "idem-1"
	params := domain.CreatePolicyVersionParams{
		PolicyID: p.PolicyID, TenantID: sp(tenantA), RulePayload: []byte(`{"threshold_amount": 100}`),
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), CreatedByPrincipalID: "author", RequestKey: key,
	}
	v1, created, err := e.s.CreatePolicyVersion(as(tenantA), params)
	if err != nil || !created || v1.VersionNumber != 1 || v1.VersionStatus != domain.StatusDraft {
		t.Fatalf("first version: %+v created=%v err=%v", v1, created, err)
	}
	replay, created, err := e.s.CreatePolicyVersion(as(tenantA), params)
	if err != nil || created || replay.PolicyVersionID != v1.PolicyVersionID {
		t.Fatalf("same key and content must replay, got created=%v err=%v", created, err)
	}
	params.RulePayload = []byte(`{"threshold_amount": 999}`)
	if _, _, err := e.s.CreatePolicyVersion(as(tenantA), params); !errors.Is(err, domain.ErrIdempotencyMismatch) {
		t.Fatalf("same key, different content must be refused, got %v", err)
	}
	// A corrected version with the same effective date is now possible.
	params.RequestKey = "idem-2"
	v2, created, err := e.s.CreatePolicyVersion(as(tenantA), params)
	if err != nil || !created || v2.VersionNumber != 2 {
		t.Fatalf("second version: created=%v err=%v %+v", created, err, v2)
	}
	params.TenantID, params.RequestKey = nil, "idem-3"
	if _, _, err := e.s.CreatePolicyVersion(as(tenantA), params); !errors.Is(err, domain.ErrInvalidScope) {
		t.Fatalf("a tenant-owned family cannot take a global version, got %v", err)
	}
	sum := sha256.Sum256(v1.RulePayload)
	if v1.ArtifactDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("the server-derived digest must match the bytes read back")
	}
}

// Audit row "Released versions are immutable": enforced by the database, not
// only the API — even the service's own role cannot rewrite or delete one.
func TestVersions_ImmutableInTheDatabase(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	v := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1}`))

	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE policy_versions SET rule_payload = '{"threshold_amount": 999999}' WHERE policy_version_id = '` + v.PolicyVersionID + `'`,
		`UPDATE policy_versions SET effective_from = now() WHERE policy_version_id = '` + v.PolicyVersionID + `'`,
		`UPDATE policy_versions SET artifact_digest = 'x' WHERE policy_version_id = '` + v.PolicyVersionID + `'`,
		`DELETE FROM policy_versions WHERE policy_version_id = '` + v.PolicyVersionID + `'`,
		`UPDATE policies SET policy_type = 'OTHER' WHERE policy_id = '` + p.PolicyID + `'`,
	} {
		_, err := tx.Exec(ctx, `SAVEPOINT sp`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%q must be refused by the database, got %v", q, err)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT sp`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.app.Exec(ctx, `TRUNCATE policy_versions CASCADE`); err == nil {
		t.Fatal("TRUNCATE must be refused")
	}
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// GOV-05 SoD (audit top gap #2): the author cannot approve or activate, and
// nothing goes live without approval.
func TestLifecycle_MakerCheckerAndApprovalRequired(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	v := e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1}`)

	if _, err := e.cmd(t, tenantA, v, domain.CommandActivate, "activator"); !errors.Is(err, domain.ErrApprovalRequired) {
		t.Fatalf("activating a DRAFT must need approval, got %v", err)
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandSubmitReview, "author"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandActivate, "activator"); !errors.Is(err, domain.ErrApprovalRequired) {
		t.Fatalf("activating a version in REVIEW must need approval, got %v", err)
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandApprove, "author"); !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("the author approving must be an SoD conflict, got %v", err)
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandApprove, "approver"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandActivate, "author"); !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("the author activating must be an SoD conflict, got %v", err)
	}
	res, err := e.cmd(t, tenantA, v, domain.CommandActivate, "activator")
	if err != nil || res.Version.VersionStatus != domain.StatusActive || *res.Version.ApprovedByPrincipalID != "approver" {
		t.Fatalf("expected ACTIVE approved by approver, got %+v err=%v", res, err)
	}
	again, err := e.cmd(t, tenantA, v, domain.CommandActivate, "activator")
	if err != nil || again.Transitioned {
		t.Fatalf("re-activating is an idempotent no-op, got %+v err=%v", again, err)
	}

	ts, err := e.s.ListTransitions(as(tenantA), v.PolicyVersionID)
	if err != nil || len(ts) != 4 {
		t.Fatalf("expected DRAFT, REVIEW, APPROVED, ACTIVE in the log, got %d err=%v", len(ts), err)
	}
	if ts[2].ToStatus != domain.StatusApproved || ts[2].ActorID != "approver" {
		t.Fatalf("approval must be evidenced with its actor, got %+v", ts[2])
	}
	for _, et := range []string{"policy.updated", "policy.version.submitted", "policy.version.approved", "policy.version.activated"} {
		if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = $1`, et); n != 1 {
			t.Fatalf("expected exactly one %s, got %d", et, n)
		}
	}
}

func TestLifecycle_ExpectedVersion(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	v := e.draft(t, tenantA, p, sp(tenantA), nil, e.clock, `{"threshold_amount": 1}`)
	stale := v.RowVersion + 7
	_, err := e.s.Transition(as(tenantA), domain.TransitionCommand{
		PolicyVersionID: v.PolicyVersionID, Command: domain.CommandSubmitReview, ActorID: "author", ExpectedVersion: &stale,
	})
	if !errors.Is(err, domain.ErrConcurrencyConflict) {
		t.Fatalf("a stale expected_version must be CONCURRENCY_CONFLICT, got %v", err)
	}
	current := v.RowVersion
	res, err := e.s.Transition(as(tenantA), domain.TransitionCommand{
		PolicyVersionID: v.PolicyVersionID, Command: domain.CommandSubmitReview, ActorID: "author", ExpectedVersion: &current,
	})
	if err != nil || res.Version.RowVersion != current+1 {
		t.Fatalf("the matching expected_version must pass and bump row_version, got %+v err=%v", res, err)
	}
}

// Audit top gaps #6/#9 and GOV-05 negative paths #1 and #4: a successor takes
// over at its effective time; history still resolves the original version.
func TestLifecycle_SupersessionAndHistoricalResolution(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	t0 := e.clock
	v1 := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, t0.Add(-48*time.Hour), `{"threshold_amount": 100}`))
	e.advance(time.Hour)
	v2draft := e.draft(t, tenantA, p, sp(tenantA), nil, t0.Add(-time.Hour), `{"threshold_amount": 200}`)
	e.advance(time.Hour)
	v2 := e.release(t, tenantA, v2draft)

	old, err := e.s.FindPolicyVersionByID(as(tenantA), v1.PolicyVersionID)
	if err != nil || old.VersionStatus != domain.StatusSuperseded || *old.SupersededByVersionID != v2.PolicyVersionID {
		t.Fatalf("v1 must be SUPERSEDED by v2, got %+v err=%v", old, err)
	}
	cands, err := e.s.ResolutionCandidates(as(tenantA), "APPROVAL_THRESHOLD", sp(tenantA), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := resolve.Resolve(cands, e.clock, e.clock); r.Selected == nil || r.Selected.PolicyVersionID != v2.PolicyVersionID {
		t.Fatalf("now: expected v2, got %+v", r)
	}
	// As of a time before v2 existed, as known then: v1.
	a, k, _ := resolve.Times(&t0, nil, e.clock)
	if r := resolve.Resolve(cands, a, k); r.Selected == nil || r.Selected.PolicyVersionID != v1.PolicyVersionID {
		t.Fatalf("historical: expected v1, got %+v", r)
	}
}

// Audit rows "State model", "Emergency suspension", scheduled activation.
func TestLifecycle_ScheduleSuspendRetire(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	current := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 100}`))

	future := e.clock.Add(48 * time.Hour)
	next := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, future, `{"threshold_amount": 200}`))
	if next.VersionStatus != domain.StatusScheduled {
		t.Fatalf("activating a future version must schedule it, got %s", next.VersionStatus)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_activation_schedule`); n != 1 {
		t.Fatalf("expected one pending schedule, got %d", n)
	}
	// The future version does not decide before its time.
	cands, _ := e.s.ResolutionCandidates(as(tenantA), "APPROVAL_THRESHOLD", sp(tenantA), nil)
	if r := resolve.Resolve(cands, e.clock, e.clock); r.Selected.PolicyVersionID != current.PolicyVersionID {
		t.Fatalf("before its effective time the scheduled version must not govern")
	}
	if n, _ := e.s.PromoteDueActivations(context.Background(), 10); n != 0 {
		t.Fatalf("nothing is due yet, promoted %d", n)
	}
	e.advance(49 * time.Hour)
	if n, err := e.s.PromoteDueActivations(context.Background(), 10); n != 1 || err != nil {
		t.Fatalf("expected one promotion, got %d err=%v", n, err)
	}
	live, _ := e.s.FindPolicyVersionByID(as(tenantA), next.PolicyVersionID)
	prev, _ := e.s.FindPolicyVersionByID(as(tenantA), current.PolicyVersionID)
	if live.VersionStatus != domain.StatusActive || prev.VersionStatus != domain.StatusSuperseded || !prev.SupersededAt.Equal(future) {
		t.Fatalf("promotion must activate the successor and supersede the predecessor at the effective time; got %s / %s at %v",
			live.VersionStatus, prev.VersionStatus, prev.SupersededAt)
	}
	ts, _ := e.s.ListTransitions(as(tenantA), next.PolicyVersionID)
	if last := ts[len(ts)-1]; last.ActorID != store.SchedulerActor || last.ToStatus != domain.StatusActive {
		t.Fatalf("the promotion must be evidenced as the scheduler's, got %+v", last)
	}

	// Emergency suspension (GOV-05 negative path #3): evidenced, and the
	// predecessor does not come back.
	res, err := e.s.Transition(as(tenantA), domain.TransitionCommand{
		PolicyVersionID: live.PolicyVersionID, Command: domain.CommandSuspend, ActorID: "security", Reason: "DEFECT: wrong threshold",
	})
	if err != nil || res.Version.VersionStatus != domain.StatusSuspended || res.Version.WithdrawnAt == nil {
		t.Fatalf("expected SUSPENDED with withdrawn_at, got %+v err=%v", res, err)
	}
	cands, _ = e.s.ResolutionCandidates(as(tenantA), "APPROVAL_THRESHOLD", sp(tenantA), nil)
	if r := resolve.Resolve(cands, e.clock, e.clock); r.Status != resolve.NotApplicable {
		t.Fatalf("after suspension nothing may govern, got %s", r.Status)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_version_transitions WHERE to_status = 'SUSPENDED' AND reason = 'DEFECT: wrong threshold'`); n != 1 {
		t.Fatal("the suspension and its reason must be in the transition log")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.version.suspended'`); n != 1 {
		t.Fatal("expected policy.version.suspended")
	}

	res, err = e.s.Transition(as(tenantA), domain.TransitionCommand{
		PolicyVersionID: live.PolicyVersionID, Command: domain.CommandRetire, ActorID: "owner", Reason: "REPLACED",
	})
	if err != nil || res.Version.VersionStatus != domain.StatusRetired {
		t.Fatalf("a suspended version can be retired, got %+v err=%v", res, err)
	}
}

// GOV-05 negative path #2 / GCP #7 (audit rows "Overlapping conflicting
// rules are blocked" and "latest-by-timestamp"): a second policy of the same
// type cannot go live in the same scope over an overlapping window, while a
// more specific override can.
func TestLifecycle_OverlapBlockedAtSameScopeOnly(t *testing.T) {
	e := newEnv(t)
	start := e.clock.Add(-time.Hour)
	p1 := e.family(t, tenantA, sp(tenantA), "AP-1")
	e.release(t, tenantA, e.draft(t, tenantA, p1, sp(tenantA), nil, start, `{"threshold_amount": 100}`))

	p2 := e.family(t, tenantA, sp(tenantA), "AP-2")
	v := e.draft(t, tenantA, p2, sp(tenantA), nil, start, `{"threshold_amount": 200}`)
	for _, step := range []struct{ cmd, actor string }{{domain.CommandSubmitReview, "author"}, {domain.CommandApprove, "approver"}} {
		if _, err := e.cmd(t, tenantA, v, step.cmd, step.actor); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.cmd(t, tenantA, v, domain.CommandActivate, "activator"); !errors.Is(err, domain.ErrPolicyConflicted) {
		t.Fatalf("an overlapping same-scope policy must be blocked, got %v", err)
	}

	// An entity-level override of the same type is a different scope.
	p3 := e.family(t, tenantA, sp(tenantA), "AP-3")
	e.release(t, tenantA, e.draft(t, tenantA, p3, sp(tenantA), sp(entityA), start, `{"threshold_amount": 50}`))
}

func TestResolution_WatermarkMovesOnTransitions(t *testing.T) {
	e := newEnv(t)
	before, err := e.s.ResolutionWatermark(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := e.family(t, tenantA, sp(tenantA), "AP")
	e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock, `{"threshold_amount": 1}`))
	after, _ := e.s.ResolutionWatermark(context.Background())
	if after <= before {
		t.Fatalf("lifecycle changes must move the watermark: %d → %d", before, after)
	}
}

// ── evaluations ──────────────────────────────────────────────────────────────

func evaluation(tenant, decision string) *domain.Evaluation {
	return &domain.Evaluation{
		TenantID: tenant, DecisionID: decision, PolicyType: "APPROVAL_THRESHOLD", RequestedByPrincipalID: "user",
		InputFingerprint: "sha256:x", ActionContext: []byte(`{"amount": 1}`), AsOf: time.Now(), KnownAt: time.Now(),
		Result: "WITHIN_THRESHOLD", Outcome: "GRANTED", PolicyID: uuid.NewString(), PolicyVersionID: uuid.NewString(),
		PolicyCode: "AP", ArtifactDigest: "sha256:y", RuleBasis: "AP:v", SelectedRuleIDs: []string{"v#threshold_amount"},
		EvaluatorVersion: "test@1", CorrelationID: "corr",
	}
}

// Decision C6 / audit top gap #5: evidence is stored with its ledger outbox
// row in one transaction, delivered with retries, and tenant-scoped.
func TestEvaluations_DurableReconcilableAndIsolated(t *testing.T) {
	e := newEnv(t)
	saved, created, err := e.s.SaveEvaluation(context.Background(), evaluation(tenantA, "dec-1"))
	if err != nil || !created || saved.LedgerStatus != domain.LedgerPending {
		t.Fatalf("save: %+v created=%v err=%v", saved, created, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_evidence_outbox WHERE evaluation_id = $1`, saved.EvaluationID); n != 1 {
		t.Fatal("the ledger outbox row must be written with the evaluation")
	}
	again, created, err := e.s.SaveEvaluation(context.Background(), evaluation(tenantA, "dec-1"))
	if err != nil || created || again.EvaluationID != saved.EvaluationID {
		t.Fatalf("the same decision_id must return the stored evaluation, got created=%v err=%v", created, err)
	}
	if _, err := e.s.FindEvaluationByID(context.Background(), tenantB, saved.EvaluationID); !errors.Is(err, domain.ErrEvaluationNotFound) {
		t.Fatalf("tenant B must not read tenant A's evidence, got %v", err)
	}
	if _, created, _ := e.s.SaveEvaluation(context.Background(), evaluation(tenantB, "dec-1")); !created {
		t.Fatal("decision ids are per tenant")
	}

	claimed, err := e.s.ClaimEvidence(context.Background(), 10, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("expected both pending, got %v err=%v", claimed, err)
	}
	if again, _ := e.s.ClaimEvidence(context.Background(), 10, time.Minute); len(again) != 0 {
		t.Fatal("leased rows must not be claimed twice")
	}
	if err := e.s.MarkEvidenceDelivered(context.Background(), saved.EvaluationID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.s.FindEvaluationByID(context.Background(), tenantA, saved.EvaluationID)
	if got.LedgerStatus != domain.LedgerRecorded {
		t.Fatalf("expected RECORDED, got %s", got.LedgerStatus)
	}
	other := claimed[0].EvaluationID
	if other == saved.EvaluationID {
		other = claimed[1].EvaluationID
	}
	if err := e.s.MarkEvidenceFailed(context.Background(), other, "409"); err != nil {
		t.Fatal(err)
	}
	pending, failed, err := e.s.EvidenceBacklog(context.Background())
	if err != nil || pending != 0 || failed != 1 {
		t.Fatalf("expected 0 pending / 1 parked, got %d / %d err=%v", pending, failed, err)
	}

	// As superuser, so RLS cannot hide the rows and the trigger must refuse.
	ctx := context.Background()
	if _, err := e.admin.Exec(ctx, `UPDATE policy_evaluations SET result = 'X'`); err == nil {
		t.Fatal("evaluation evidence must be append-only")
	}
}

// ── outbox relay ─────────────────────────────────────────────────────────────

type fakeWriter struct {
	err  error
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return f.err
}

// Audit row "Outbox after commit; at-least-once delivery".
func TestOutboxRelay_PublishesRetriesAndKeepsEventID(t *testing.T) {
	e := newEnv(t)
	e.family(t, tenantA, sp(tenantA), "AP")

	broken := &fakeWriter{err: errors.New("broker down")}
	if n := outbox.NewWorker(e.app, broken, zap.NewNop()).RunOnce(context.Background()); n != 0 {
		t.Fatalf("nothing is published while the broker is down, got %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL AND last_error IS NOT NULL AND attempts = 1`); n != 1 {
		t.Fatal("a failed publish must be kept with its error for retry")
	}
	if _, err := e.admin.Exec(context.Background(), `UPDATE outbox SET next_attempt_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	ok := &fakeWriter{}
	if n := outbox.NewWorker(e.app, ok, zap.NewNop()).RunOnce(context.Background()); n != 1 {
		t.Fatalf("expected the retry to publish 1, got %d", n)
	}
	if string(broken.msgs[0].Value) == "" || headerValue(broken.msgs[0], "event_id") != headerValue(ok.msgs[0], "event_id") {
		t.Fatal("a redelivered event must carry the same event_id")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE published_at IS NOT NULL`); n != 1 {
		t.Fatal("a published event must be marked")
	}
}

func headerValue(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// ── control tests and attestations ───────────────────────────────────────────

// Audit row "Control-test / attestation reads are scoped".
func TestControlTests_TenantIsolation(t *testing.T) {
	e := newEnv(t)
	def, created, err := e.s.CreateControlTestDefinition(as(tenantA), domain.CreateControlTestDefinitionParams{
		TenantID: tenantA, ControlRef: "CTRL-1", TestCode: "T-1", Title: "t", Methodology: "m", CreatedByPrincipalID: "tester",
	})
	if err != nil || !created {
		t.Fatalf("create definition: %v", err)
	}
	if _, err := e.s.FindControlTestDefinitionByID(as(tenantB), def.ControlTestDefinitionID); !errors.Is(err, domain.ErrControlTestDefinitionNotFound) {
		t.Fatalf("tenant B must not see tenant A's definition, got %v", err)
	}
	if _, err := e.s.CreateControlTestExecution(as(tenantB), domain.CreateControlTestExecutionParams{
		TenantID: tenantB, ControlTestDefinitionID: def.ControlTestDefinitionID, PeriodStart: time.Now().Add(-time.Hour),
		PeriodEnd: time.Now(), Result: "EFFECTIVE", TesterPrincipalID: "x", CreatedByPrincipalID: "x",
	}); !errors.Is(err, domain.ErrControlTestDefinitionNotFound) {
		t.Fatalf("tenant B must not test tenant A's definition, got %v", err)
	}
	if _, err := e.s.CreateControlTestExecution(as(tenantA), domain.CreateControlTestExecutionParams{
		TenantID: tenantA, ControlTestDefinitionID: def.ControlTestDefinitionID, PeriodStart: time.Now().Add(-time.Hour),
		PeriodEnd: time.Now(), Result: "INEFFECTIVE", TesterPrincipalID: "x", CreatedByPrincipalID: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if eff, _ := e.s.ResolveControlEffectiveness(as(tenantB), "CTRL-1"); eff.OperatingEffectiveness != "NO_EXECUTIONS_RECORDED" || eff.DesignStatus != "NOT_TESTED" {
		t.Fatalf("tenant B must see nothing of tenant A's control, got %+v", eff)
	}
	if eff, _ := e.s.ResolveControlEffectiveness(as(tenantA), "CTRL-1"); eff.OperatingEffectiveness != "INEFFECTIVE" {
		t.Fatalf("tenant A must see its own result, got %+v", eff)
	}

	att, err := e.s.CreateAttestation(as(tenantA), domain.CreateAttestationParams{
		TenantID: tenantA, Statement: "s", StatementVersion: "v1", SubjectRef: "CTRL-1", PeriodStart: time.Now().Add(-time.Hour),
		PeriodEnd: time.Now(), SignerPrincipalID: "cfo", SignerRole: "CFO", CreatedByPrincipalID: "cfo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FindAttestationByID(as(tenantB), att.AttestationID); !errors.Is(err, domain.ErrAttestationNotFound) {
		t.Fatalf("tenant B must not read tenant A's attestation, got %v", err)
	}
	if _, err := e.s.RevokeAttestation(as(tenantB), att.AttestationID, "x"); !errors.Is(err, domain.ErrAttestationNotFound) {
		t.Fatalf("tenant B must not revoke tenant A's attestation, got %v", err)
	}
	if _, err := e.s.RevokeAttestation(as(tenantA), att.AttestationID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RevokeAttestation(as(tenantA), att.AttestationID, ""); !errors.Is(err, domain.ErrInvalidAttestationTransition) {
		t.Fatalf("a second revocation is an invalid transition, got %v", err)
	}
}

// GOV-05 negative path #3: an emergency suspension is evidenced AND reviewed,
// and the review is independent of the suspender.
func TestLifecycle_SuspensionReviewIsIndependent(t *testing.T) {
	e := newEnv(t)
	p := e.family(t, tenantA, sp(tenantA), "AP")
	v := e.release(t, tenantA, e.draft(t, tenantA, p, sp(tenantA), nil, e.clock.Add(-time.Hour), `{"threshold_amount": 1}`))
	review := func(actor string) (*domain.TransitionResult, error) {
		return e.s.Transition(as(tenantA), domain.TransitionCommand{
			PolicyVersionID: v.PolicyVersionID, Command: domain.CommandReviewSuspension, ActorID: actor,
			Reason: "checked the threshold", Outcome: domain.SuspensionUpheld,
		})
	}
	if _, err := review("auditor"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("only a suspended version can be reviewed, got %v", err)
	}
	if _, err := e.s.Transition(as(tenantA), domain.TransitionCommand{
		PolicyVersionID: v.PolicyVersionID, Command: domain.CommandSuspend, ActorID: "security", Reason: "DEFECT",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := review("security"); !errors.Is(err, domain.ErrSoDConflict) {
		t.Fatalf("the suspender cannot review their own suspension, got %v", err)
	}
	res, err := review("auditor")
	if err != nil || res.Version.VersionStatus != domain.StatusSuspended ||
		*res.Version.SuspensionReviewedByPrincipalID != "auditor" || *res.Version.SuspensionReviewOutcome != domain.SuspensionUpheld ||
		*res.Version.SuspendedByPrincipalID != "security" {
		t.Fatalf("expected a recorded review of a still-suspended version, got %+v err=%v", res, err)
	}
	if again, err := review("auditor2"); err != nil || again.Transitioned {
		t.Fatalf("a second review is an idempotent no-op, got %+v err=%v", again, err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM policy_version_transitions WHERE reason = 'suspension review UPHELD: checked the threshold' AND actor_id = 'auditor'`); n != 1 {
		t.Fatal("the review must be in the transition log")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE event_type = 'policy.version.suspension_reviewed'`); n != 1 {
		t.Fatal("expected policy.version.suspension_reviewed")
	}
}

// Doc 03 §19: the request's jurisdiction context travels on its events.
func TestOutbox_CarriesJurisdictionContext(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.s.CreatePolicy(as(tenantA), domain.CreatePolicyParams{
		PolicyCode: "AP", PolicyName: "AP", PolicyType: "APPROVAL_THRESHOLD", TenantID: sp(tenantA), CreatedByPrincipalID: "a",
		Meta: domain.EventMeta{CorrelationID: "c", JurisdictionContext: "IN-KA"},
	}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM outbox WHERE jurisdiction_context = 'IN-KA'`); n != 1 {
		t.Fatal("the jurisdiction context must be stored with the event")
	}
	ok := &fakeWriter{}
	outbox.NewWorker(e.app, ok, zap.NewNop()).WithResidency("asia-south1").RunOnce(context.Background())
	if len(ok.msgs) != 1 || !strings.Contains(string(ok.msgs[0].Value), `"jurisdiction_context":"IN-KA"`) ||
		!strings.Contains(string(ok.msgs[0].Value), `"residency_region":"asia-south1"`) {
		t.Fatalf("the envelope must carry jurisdiction and residency, got %s", ok.msgs[0].Value)
	}
}
