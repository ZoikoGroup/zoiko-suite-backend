package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/connectivity-api-bridge-svc/internal/connector"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/eventing/egress"
)

// These tests run the governed connector against REAL Postgres: the SQL, the
// constraints and triggers the database holds itself, the outbox, row-level
// security as an ordinary role, and concurrency.

const credRef = "0a000000-0000-4000-8000-000000000001"

type pgVault struct{}

func (pgVault) Verify(_ context.Context, ref string, _ vault.Caller) error {
	if strings.EqualFold(ref, credRef) {
		return nil
	}
	return vault.ErrNotFound
}

type pgDNS struct{}

func (pgDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

func migrationFiles(t *testing.T) (dir string, names []string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	dir = filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") && !strings.Contains(e.Name(), ".down.") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return dir, names
}

func applyOnly(t *testing.T, admin *pgxpool.Pool, prefixes ...string) {
	t.Helper()
	dir, names := migrationFiles(t)
	resetGovernanceObjects(context.Background(), admin)
	for _, n := range names {
		ok := false
		for _, p := range prefixes {
			ok = ok || strings.HasPrefix(n, p)
		}
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(context.Background(), string(b)); err != nil {
			t.Fatalf("apply %s: %v", n, err)
		}
	}
}

type pgEnv struct {
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	svc   *connector.Service
	st    *store.PgStore
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	admin := openAdminPool(t) // applies every migration from scratch
	app := appRolePool(t, admin)
	st := store.NewPgStore(app, store.WithEventRegion("uk"))
	svc := &connector.Service{
		Store: st, Vault: pgVault{}, Egress: egress.Policy{Resolver: pgDNS{}}, EgressAllowedRegions: []string{"uk"},
	}
	e := &pgEnv{admin: admin, app: app, svc: svc, st: st}
	pctx := middleware.WithTenant(context.Background(), store.PlatformScopeID)
	a := connector.Actor{PrincipalID: "platform-admin"}
	if err := svc.PublishProfile(pctx, store.PlatformScopeID, a, &domain.Profile{ProviderKey: "bank-x", ProviderType: "BANK", Version: 2,
		AllowedRegions: []string{"uk"}, AllowedHostSuffixes: []string{"bank-x.example"},
		SupportedScopes: []string{"ingest", "statements.read", "payments.write"}}); err != nil {
		t.Fatalf("publish profile: %v", err)
	}
	if _, err := svc.SetProfileStatus(pctx, store.PlatformScopeID, a, "bank-x", 2, domain.ProfileCertified); err != nil {
		t.Fatalf("certify: %v", err)
	}
	return e
}

func tctx(tenant string) context.Context { return middleware.WithTenant(context.Background(), tenant) }

var (
	maker   = connector.Actor{PrincipalID: "maker", CorrelationID: "corr-1"}
	checker = connector.Actor{PrincipalID: "checker"}
)

func goodInput() connector.CreateInput {
	return connector.CreateInput{CreateBridgeRequest: domain.CreateBridgeRequest{
		LegalEntityID: "le-1", BridgeName: "Bank X", Protocol: "REST", EndpointURL: "https://api.bank-x.example/v2/feed",
		AuthType: "OAUTH2", Purpose: "bank-statement-import", ProviderKey: "bank-x", ProviderProfileVersion: 2,
		CredentialRef: credRef, DestinationRegion: "uk", SyncMode: "PULL", Scopes: []string{"ingest", "statements.read"},
	}}
}

func (e *pgEnv) active(t *testing.T, tenant string) *domain.ApiBridge {
	t.Helper()
	ctx := tctx(tenant)
	b, _, err := e.svc.Create(ctx, maker, goodInput())
	if err != nil {
		t.Fatal(err)
	}
	if b, err = e.svc.Submit(ctx, tenant, maker, b.BridgeID, b.Version); err != nil {
		t.Fatal(err)
	}
	if b, err = e.svc.Approve(ctx, tenant, checker, b.BridgeID, b.Version, "ok"); err != nil {
		t.Fatal(err)
	}
	if b, err = e.svc.Activate(ctx, tenant, checker, b.BridgeID, b.Version); err != nil {
		t.Fatal(err)
	}
	return b
}

func count(t *testing.T, e *pgEnv, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- the migration ----------------------------------------------------------------

func TestMigration_GrandfathersLegacyRowsAndStripsStoredSecrets(t *testing.T) {
	admin := openAdminPool(t)
	ctx := context.Background()
	applyOnly(t, admin, "001", "002") // the schema as it was before governance

	for id, url := range map[string]string{
		"clean":    "https://erp.example.internal/api",
		"userinfo": "https://svc:s3cretpass@api.partner.example/feed?page=2",
		"query":    "https://api.partner.example/feed?api_key=sk_live_XYZ&page=2",
	} {
		if _, err := admin.Exec(ctx, `INSERT INTO api_bridges (bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status)
			VALUES ($1, 'tenant-a', 'le-1', $1, 'REST', $2, 'API_KEY', 'ACTIVE')`, id, url); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(func() string { d, _ := migrationFiles(t); return d }(), "003_connector_management.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(b)); err != nil {
		t.Fatalf("003 over existing data: %v", err)
	}

	type row struct {
		status, url, host, reason, by string
		legacy                        bool
		version                       int64
		scopes                        []string
	}
	read := func(id string) row {
		var r row
		if err := admin.QueryRow(ctx, `SELECT status, endpoint_url, destination_host, status_reason, created_by, legacy_unapproved, version, scopes
			FROM api_bridges WHERE bridge_id = $1`, id).Scan(&r.status, &r.url, &r.host, &r.reason, &r.by, &r.legacy, &r.version, &r.scopes); err != nil {
			t.Fatal(err)
		}
		return r
	}
	clean := read("clean")
	if clean.status != "ACTIVE" || !clean.legacy || clean.version != 1 || len(clean.scopes) != 1 || clean.scopes[0] != "ingest" ||
		clean.host != "erp.example.internal" || clean.by != "legacy" {
		t.Fatalf("an existing connector must keep working, flagged as never approved: %+v", clean)
	}
	for _, id := range []string{"userinfo", "query"} {
		r := read(id)
		if r.status != "SUSPENDED" || r.reason == "" {
			t.Errorf("%s: a connector that stored a credential must be stopped: %+v", id, r)
		}
		for _, leak := range []string{"s3cretpass", "sk_live", "api_key", "svc:"} {
			if strings.Contains(r.url, leak) {
				t.Errorf("%s: the stored URL still carries %q: %s", id, leak, r.url)
			}
		}
		if !strings.HasPrefix(r.url, "https://api.partner.example/feed") {
			t.Errorf("%s: the destination itself must survive: %s", id, r.url)
		}
	}
	// the new constraint now holds for any later write
	if _, err := admin.Exec(ctx, `UPDATE api_bridges SET endpoint_url = 'https://u:p@x.example/' WHERE bridge_id = 'clean'`); err == nil {
		t.Fatal("a credential in the URL must be refused by the database")
	}
}

// ---- guarantees the database holds by itself --------------------------------------------

func TestDB_ConstraintsAndTriggers(t *testing.T) {
	e := newPgEnv(t)
	ctx := context.Background()
	insert := func(extra string) error {
		_, err := e.admin.Exec(ctx, `INSERT INTO api_bridges (bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status,
			purpose, provider_key, provider_profile_version, destination_region, destination_host, scopes, config_fingerprint, approved_fingerprint,
			approved_by, last_changed_by, legacy_unapproved) VALUES `+extra)
		return err
	}
	ok := func(id, status, cfg, appr, approver, changer string, legacy bool) string {
		b := "false"
		if legacy {
			b = "true"
		}
		return "('" + id + "','t','le','n','REST','https://api.bank-x.example/x','NONE','" + status + "','p','bank-x',2,'uk','api.bank-x.example',ARRAY['ingest'],'" +
			cfg + "','" + appr + "','" + approver + "','" + changer + "'," + b + ")"
	}

	if err := insert(ok("good", "ACTIVE", "sha256:a", "sha256:a", "checker", "maker", false)); err != nil {
		t.Fatalf("a properly approved live connector must be accepted: %v", err)
	}
	for name, sql := range map[string]string{
		"live without any approval":                ok("b1", "ACTIVE", "sha256:a", "", "", "maker", false),
		"live whose config differs from approved":  ok("b2", "ACTIVE", "sha256:NEW", "sha256:a", "checker", "maker", false),
		"approver is the last person to change it": ok("b3", "PENDING_APPROVAL", "sha256:a", "sha256:a", "maker", "maker", false),
		"unknown status":                           ok("b4", "RUNNING", "", "", "", "maker", true),
	} {
		if err := insert(sql); err == nil {
			t.Errorf("%s was accepted by the database", name)
		}
	}
	// a pending connector with nothing specified is refused too
	if _, err := e.admin.Exec(ctx, `INSERT INTO api_bridges (bridge_id, tenant_id, legal_entity_id, bridge_name, protocol, endpoint_url, auth_type, status)
		VALUES ('b5','t','le','n','REST','https://x.example/','NONE','PENDING_APPROVAL')`); err == nil {
		t.Error("an unspecified connector reached PENDING_APPROVAL")
	}

	// append-only evidence
	if _, err := e.admin.Exec(ctx, `INSERT INTO connector_changes (change_id, connector_id, tenant_id, from_version, to_version, change_kind, changed_by)
		VALUES (gen_random_uuid(), 'good', 't', 1, 2, 'UPDATE', 'x')`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE connector_changes SET changed_by = 'someone-else'`, `DELETE FROM connector_changes`,
	} {
		if _, err := e.admin.Exec(ctx, stmt); err == nil {
			t.Errorf("%q must be refused", stmt)
		}
	}
	if _, err := e.admin.Exec(ctx, `INSERT INTO connector_approvals (approval_id, connector_id, tenant_id, connector_version, fingerprint, decision, requested_by, decided_by)
		VALUES (gen_random_uuid(), 'good', 't', 1, 'f', 'APPROVED', 'same', 'same')`); err == nil {
		t.Error("an approval by the person who requested it was accepted")
	}
	if _, err := e.admin.Exec(ctx, `INSERT INTO connector_approvals (approval_id, connector_id, tenant_id, connector_version, fingerprint, decision, requested_by, decided_by)
		VALUES (gen_random_uuid(), 'good', 't', 1, 'f', 'MAYBE', 'a', 'b')`); err == nil {
		t.Error("an unknown decision was accepted")
	}

	// checkpoints only move forward, one version at a time
	if _, err := e.admin.Exec(ctx, `INSERT INTO sync_checkpoints (connector_id, tenant_id, stream, cursor, watermark, checkpoint_version, updated_by)
		VALUES ('good', 't', 's', 'c1', now(), 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"skip a version":      `UPDATE sync_checkpoints SET cursor='c', watermark=now()+interval '1 hour', checkpoint_version=3`,
		"repeat a version":    `UPDATE sync_checkpoints SET cursor='c', watermark=now()+interval '1 hour', checkpoint_version=1`,
		"watermark backwards": `UPDATE sync_checkpoints SET cursor='c', watermark=now()-interval '5 hours', checkpoint_version=2`,
		"drop the watermark":  `UPDATE sync_checkpoints SET cursor='c', watermark=NULL, checkpoint_version=2`,
		"change its identity": `UPDATE sync_checkpoints SET stream='other', checkpoint_version=2`,
		"delete":              `DELETE FROM sync_checkpoints`,
	} {
		if _, err := e.admin.Exec(ctx, stmt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	tx, _ := e.admin.Begin(ctx)
	_, _ = tx.Exec(ctx, `SELECT set_config('app.checkpoint_reset', 'true', true)`)
	if _, err := tx.Exec(ctx, `UPDATE sync_checkpoints SET cursor='c0', watermark=now()-interval '5 hours', checkpoint_version=2`); err != nil {
		t.Errorf("an approved reset (the sanctioned flag) must be able to move it back: %v", err)
	}
	_ = tx.Commit(ctx)

	// provider profiles are fixed once published
	for name, stmt := range map[string]string{
		"edit the regions": `UPDATE external_profiles SET allowed_regions = ARRAY['us']`,
		"edit the hosts":   `UPDATE external_profiles SET allowed_host_suffixes = ARRAY['evil.example']`,
		"edit the scopes":  `UPDATE external_profiles SET supported_scopes = ARRAY['admin']`,
		"delete":           `DELETE FROM external_profiles`,
	} {
		if _, err := e.admin.Exec(ctx, stmt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := e.admin.Exec(ctx, `UPDATE external_profiles SET status = 'DRAFT'`); err == nil {
		t.Error("a profile status moved backwards")
	}

	// event payloads cannot be edited by repair tooling
	b := e.active(t, "tenant-a")
	_ = b
	if _, err := e.admin.Exec(ctx, `UPDATE eventing_outbox SET payload = '\x7b7d'::bytea`); err == nil {
		t.Error("an outbox event's payload was editable")
	}
}

// ---- the store, end to end ---------------------------------------------------------------

func TestPg_FullLifecycleWritesEvidenceAndEventsAtomically(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")

	if b.Status != domain.StatusActive || b.Version != 4 || b.ApprovedBy != "checker" || b.CredentialVerifiedAt == nil {
		t.Fatalf("%+v", b)
	}
	got, err := e.st.GetBridgeByID(ctx, b.BridgeID)
	if err != nil || got.ApprovedFingerprint != b.ApprovedFingerprint || got.ConfigFingerprint != got.ApprovedFingerprint ||
		len(got.Scopes) != 2 || got.DestinationHost != "api.bank-x.example" {
		t.Fatalf("a connector reads back exactly as written: %v %+v", err, got)
	}

	changes, _ := e.st.ListChanges(ctx, b.BridgeID)
	kinds := make([]string, len(changes))
	for i, c := range changes {
		kinds[i] = c.Kind
	}
	if strings.Join(kinds, ",") != "CREATE,SUBMIT,APPROVE,ACTIVATE" {
		t.Fatalf("changes %v", kinds)
	}
	approvals, _ := e.st.ListApprovals(ctx, b.BridgeID)
	if len(approvals) != 1 || approvals[0].DecidedBy != "checker" || approvals[0].RequestedBy != "maker" {
		t.Fatalf("%+v", approvals)
	}
	h, _ := e.st.GetHealth(ctx, b.BridgeID)
	if h.State != domain.HealthHealthy || h.LastSuccessAt == nil {
		t.Fatalf("%+v", h)
	}

	// the events are in the outbox, in the same transactions
	rows, err := e.admin.Query(context.Background(),
		`SELECT event_type, tenant_id, region, aggregate_id, convert_from(payload, 'UTF8') FROM eventing_outbox WHERE tenant_id = 'tenant-a' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ, tenant, region, agg, payload string
		if err := rows.Scan(&typ, &tenant, &region, &agg, &payload); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
		if region != "uk" || agg != b.BridgeID {
			t.Errorf("%s: region=%s aggregate=%s", typ, region, agg)
		}
		for _, secret := range []string{"api.bank-x.example", credRef, "oauth", "OAUTH2"} {
			if strings.Contains(payload, secret) {
				t.Errorf("%s: the event payload carries %q (events carry identifiers, not configuration or credentials)", typ, secret)
			}
		}
	}
	want := []string{"com.zoikosuite.connectivity.bridge.created", "com.zoikosuite.connectivity.connector.approval-requested",
		"com.zoikosuite.connectivity.connector.approved", "com.zoikosuite.connectivity.connector.activated"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("outbox = %v", types)
	}
	var legacy string
	if err := e.admin.QueryRow(context.Background(), `SELECT convert_from(payload,'UTF8')::jsonb->>'event_type' FROM eventing_outbox
		WHERE event_type LIKE '%bridge.created' AND tenant_id = 'tenant-a'`).Scan(&legacy); err != nil || legacy != "connectivity.bridge.created" {
		t.Fatalf("the pre-standard event type must be kept for existing consumers: %q %v", legacy, err)
	}
}

func TestPg_IngestWritesLogAndEventAndEnforcesState(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	log, err := e.svc.Ingest(ctx, maker, b.BridgeID, connector.IngestInput{PayloadSummary: "batch 1", Scope: "statements.read"})
	if err != nil || log.LogID == "" {
		t.Fatal(err)
	}
	if n := count(t, e, `SELECT count(*) FROM ingestion_logs WHERE bridge_id = $1`, b.BridgeID); n != 1 {
		t.Fatalf("%d logs", n)
	}
	if n := count(t, e, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%bridge.ingested'`); n != 1 {
		t.Fatalf("%d ingested events", n)
	}
	if _, err := e.svc.Ingest(ctx, maker, b.BridgeID, connector.IngestInput{Scope: "payments.write"}); !errors.Is(err, connector.ErrScopeDenied) {
		t.Fatalf("%v", err)
	}
	if n := count(t, e, `SELECT count(*) FROM ingestion_logs WHERE bridge_id = $1`, b.BridgeID); n != 1 {
		t.Fatal("a refused ingestion must not be logged")
	}
}

func TestPg_ProtectedChangeWithdrawsApprovalInTheDatabase(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	wider := []string{"ingest", "statements.read", "payments.write"}
	u, err := e.svc.Update(ctx, connector.Actor{PrincipalID: "editor"}, b.BridgeID, b.Version, connector.Patch{Scopes: &wider})
	if err != nil {
		t.Fatal(err)
	}
	if u.Live() || u.ApprovedFingerprint != "" {
		t.Fatalf("%+v", u)
	}
	var status, approved string
	if err := e.admin.QueryRow(context.Background(), `SELECT status, approved_fingerprint FROM api_bridges WHERE bridge_id = $1`, b.BridgeID).Scan(&status, &approved); err != nil {
		t.Fatal(err)
	}
	if status != "DRAFT" || approved != "" {
		t.Fatalf("stored: %s %q", status, approved)
	}
	if _, err := e.svc.Ingest(ctx, maker, b.BridgeID, connector.IngestInput{}); !errors.Is(err, connector.ErrNotLive) {
		t.Fatalf("%v", err)
	}
	if n := count(t, e, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%connector.scope-changed'`); n != 1 {
		t.Fatalf("scope_changed events: %d", n)
	}
	changes, _ := e.st.ListChanges(ctx, b.BridgeID)
	last := changes[len(changes)-1]
	if len(last.ScopesAdded) != 1 || last.ScopesAdded[0] != "payments.write" || last.ChangedBy != "editor" {
		t.Fatalf("%+v", last)
	}
}

// A failure while writing the effects must undo the connector change too.
func TestPg_FailedEffectRollsBackTheConnectorAndItsEvents(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	before := count(t, e, `SELECT count(*) FROM eventing_outbox`)

	_, err := e.st.MutateConnector(ctx, b.BridgeID, func(c *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		c.StatusReason, c.Version = "changed", c.Version+1
		return &domain.Effects{
			Events: []domain.OutboxEvent{{Type: "connector.suspended", Data: map[string]any{"x": 1}}},
			Health: &domain.HealthUpdate{State: "BOGUS"}, // the CHECK constraint refuses this
		}, nil
	})
	if err == nil {
		t.Fatal("the invalid health state must fail the whole transaction")
	}
	var reason string
	var version int64
	_ = e.admin.QueryRow(context.Background(), `SELECT status_reason, version FROM api_bridges WHERE bridge_id = $1`, b.BridgeID).Scan(&reason, &version)
	if reason == "changed" || version != b.Version {
		t.Fatalf("the connector change survived a failed effect: %q v%d", reason, version)
	}
	if after := count(t, e, `SELECT count(*) FROM eventing_outbox`); after != before {
		t.Fatalf("an event was left behind by a rolled-back transaction: %d -> %d", before, after)
	}
}

// ---- tenant isolation under row-level security ---------------------------------------------

func TestPg_TenantIsolationAcrossEveryTable(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctxA, ctxB := tctx("tenant-a"), tctx("tenant-b")
	w := time.Now().UTC().Add(-time.Hour)
	if _, err := e.svc.AdvanceCheckpoint(ctxA, maker, b.BridgeID, "s", "c1", &w, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReportHealth(ctxA, maker, b.BridgeID, connector.HealthReport{Reason: domain.ReasonCredentialExpired}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.st.GetBridgeByID(ctxB, b.BridgeID); !errors.Is(err, domain.ErrBridgeNotFound) {
		t.Fatalf("%v", err)
	}
	for name, f := range map[string]func() error{
		"changes":     func() error { _, err := e.st.ListChanges(ctxB, b.BridgeID); return err },
		"approvals":   func() error { _, err := e.st.ListApprovals(ctxB, b.BridgeID); return err },
		"health":      func() error { _, err := e.st.GetHealth(ctxB, b.BridgeID); return err },
		"transitions": func() error { _, err := e.st.ListHealthTransitions(ctxB, b.BridgeID, 10); return err },
		"checkpoints": func() error { _, err := e.st.ListCheckpoints(ctxB, b.BridgeID); return err },
		"mutate":      func() error { _, err := e.svc.Suspend(ctxB, maker, b.BridgeID, b.Version, "x"); return err },
		"advance": func() error {
			_, err := e.svc.AdvanceCheckpoint(ctxB, maker, b.BridgeID, "s", "c2", nil, 1)
			return err
		},
	} {
		if err := f(); !errors.Is(err, domain.ErrBridgeNotFound) {
			t.Errorf("%s as another tenant: %v", name, err)
		}
	}
	// a direct read as the application role sees only the tenant it set
	var n int
	conn, _ := e.app.Acquire(context.Background())
	defer conn.Release()
	_, _ = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', 'tenant-b', false)`)
	for _, table := range []string{"api_bridges", "connector_changes", "connector_approvals", "connector_health",
		"connector_health_transitions", "sync_checkpoints", "eventing_outbox"} {
		if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: tenant-b sees %d rows (err %v)", table, n, err)
		}
	}
	// and may not write a row attributed to someone else
	if _, err := conn.Exec(context.Background(), `INSERT INTO connector_health (connector_id, tenant_id, state) VALUES ($1, 'tenant-a', 'HEALTHY')`, b.BridgeID); err == nil {
		t.Error("WITH CHECK must refuse a foreign-tenant write")
	}
}

func TestPg_ProfilesAreReadableByAllAndWritableOnlyByThePlatform(t *testing.T) {
	e := newPgEnv(t)
	p, err := e.st.GetProfile(tctx("tenant-a"), "bank-x", 2)
	if err != nil || p.Status != domain.ProfileCertified {
		t.Fatalf("every tenant can read a profile: %v %+v", err, p)
	}
	prof := &domain.Profile{ProviderKey: "bank-q", ProviderType: "BANK", Version: 1, AllowedRegions: []string{"uk"},
		AllowedHostSuffixes: []string{"bank-q.example"}, SupportedScopes: []string{"ingest"}, PublishedBy: "x"}
	if err := e.st.PublishProfile(tctx("tenant-a"), prof, domain.OutboxEvent{Type: "external-profile.published"}); !errors.Is(err, domain.ErrPlatformScopeRequired) {
		t.Fatalf("a tenant wrote platform reference data (row-level security must refuse): %v", err)
	}
	if _, err := e.st.SetProfileStatus(tctx("tenant-a"), "bank-x", 2, domain.ProfileUnsupported, time.Now(), domain.OutboxEvent{Type: "external-profile.unsupported"}); !errors.Is(err, domain.ErrProfileNotFound) {
		t.Fatalf("a tenant changed a platform profile's status: %v", err)
	}
	if p, _ := e.st.GetProfile(tctx("tenant-a"), "bank-x", 2); p.Status != domain.ProfileCertified {
		t.Fatalf("the profile was changed: %s", p.Status)
	}
	if err := e.st.PublishProfile(tctx(store.PlatformScopeID), prof, domain.OutboxEvent{Type: "external-profile.published"}); err != nil {
		t.Fatalf("the platform can: %v", err)
	}
	if err := e.st.PublishProfile(tctx(store.PlatformScopeID), &domain.Profile{ProviderKey: "bank-q", ProviderType: "BANK", Version: 1, AllowedRegions: []string{"uk"},
		AllowedHostSuffixes: []string{"bank-q.example"}, SupportedScopes: []string{"ingest"}, PublishedBy: "x"}, domain.OutboxEvent{Type: "x"}); !errors.Is(err, domain.ErrProfileExists) {
		t.Fatalf("%v", err)
	}
}

// ---- concurrency ---------------------------------------------------------------------------

func TestPg_ConcurrentUpdatesExactlyOneWins(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	var won, conflict int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := "purpose-" + strings.Repeat("x", i+1)
			_, err := e.svc.Update(ctx, connector.Actor{PrincipalID: "editor"}, b.BridgeID, b.Version, connector.Patch{Purpose: &p})
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, domain.ErrVersionConflict):
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || conflict != 15 {
		t.Fatalf("%d won, %d conflicted", won, conflict)
	}
	if n := count(t, e, `SELECT count(*) FROM connector_changes WHERE connector_id = $1 AND change_kind = 'UPDATE'`, b.BridgeID); n != 1 {
		t.Fatalf("one update, one change record: %d", n)
	}
}

func TestPg_ConcurrentApprovalsRecordExactlyOne(t *testing.T) {
	e := newPgEnv(t)
	ctx := tctx("tenant-a")
	b, _, _ := e.svc.Create(ctx, maker, goodInput())
	b, err := e.svc.Submit(ctx, "tenant-a", maker, b.BridgeID, b.Version)
	if err != nil {
		t.Fatal(err)
	}
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := connector.Actor{PrincipalID: "checker-" + strings.Repeat("x", i+1)}
			if _, err := e.svc.Approve(ctx, "tenant-a", who, b.BridgeID, b.Version, ""); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d approvals succeeded", ok)
	}
	if n := count(t, e, `SELECT count(*) FROM connector_approvals WHERE connector_id = $1`, b.BridgeID); n != 1 {
		t.Fatalf("%d approval rows", n)
	}
	if n := count(t, e, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%connector.approved'`); n != 1 {
		t.Fatalf("%d approved events", n)
	}
}

func TestPg_ConcurrentCreatesWithOneKeyMakeOneConnector(t *testing.T) {
	e := newPgEnv(t)
	ctx := tctx("tenant-a")
	in := goodInput()
	in.IdempotencyKey = "same-key"
	var mu sync.Mutex
	ids := map[string]bool{}
	var created int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, c, err := e.svc.Create(ctx, maker, in)
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			if c {
				atomic.AddInt32(&created, 1)
			}
			mu.Lock()
			ids[b.BridgeID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Fatalf("created=%d distinct ids=%d", created, len(ids))
	}
	if n := count(t, e, `SELECT count(*) FROM api_bridges WHERE tenant_id = 'tenant-a'`); n != 1 {
		t.Fatalf("%d rows", n)
	}
	if n := count(t, e, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%bridge.created'`); n != 1 {
		t.Fatalf("%d created events", n)
	}
}

func TestPg_ConcurrentCheckpointAdvancesExactlyOneWins(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	w0 := time.Now().UTC().Add(-3 * time.Hour)
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "c0", &w0, 0); err != nil {
		t.Fatal(err)
	}
	var won, conflict int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := w0.Add(time.Duration(i+1) * time.Minute)
			_, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "c"+strings.Repeat("x", i+1), &w, 1)
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, domain.ErrCheckpointConflict):
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || conflict != 11 {
		t.Fatalf("%d won, %d conflicted", won, conflict)
	}
	cps, _ := e.st.ListCheckpoints(ctx, b.BridgeID)
	if cps[0].Version != 2 {
		t.Fatalf("%+v", cps[0])
	}
}

func TestPg_CheckpointResetThroughTheStore(t *testing.T) {
	e := newPgEnv(t)
	b := e.active(t, "tenant-a")
	ctx := tctx("tenant-a")
	w1, w2 := time.Now().UTC().Add(-3*time.Hour), time.Now().UTC().Add(-2*time.Hour)
	_, _ = e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "c1", &w1, 0)
	_, _ = e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "c2", &w2, 1)

	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "old", &w1, 2); !errors.Is(err, domain.ErrCheckpointRegress) {
		t.Fatalf("rollback through the normal path: %v", err)
	}
	req, err := e.svc.RequestCheckpointReset(ctx, maker, b.BridgeID, "s", "c1", &w1, "provider re-sent a batch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DecideCheckpointReset(ctx, maker, req.RequestID, true); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("%v", err)
	}
	done, err := e.svc.DecideCheckpointReset(ctx, checker, req.RequestID, true)
	if err != nil || done.Status != domain.ResetApplied {
		t.Fatalf("%v", err)
	}
	cps, _ := e.st.ListCheckpoints(ctx, b.BridgeID)
	if cps[0].Cursor != "c1" || cps[0].Version != 3 || cps[0].UpdatedBy != "checker" {
		t.Fatalf("%+v", cps[0])
	}
	// the sanctioned flag does not leak: the next normal advance is checked again
	if _, err := e.svc.AdvanceCheckpoint(ctx, maker, b.BridgeID, "s", "back", ptrT(w1.Add(-time.Hour)), 3); !errors.Is(err, domain.ErrCheckpointRegress) {
		t.Fatalf("after a reset the no-rollback rule must still apply: %v", err)
	}
	if _, err := e.svc.DecideCheckpointReset(ctx, checker, req.RequestID, true); !errors.Is(err, domain.ErrResetNotPending) {
		t.Fatalf("%v", err)
	}
	if n := count(t, e, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%sync.checkpoint-reset-applied'`); n != 1 {
		t.Fatalf("reset events: %d", n)
	}
}

func ptrT(t time.Time) *time.Time { return &t }

func TestPg_OutboxSchemaVerifies(t *testing.T) {
	e := newPgEnv(t)
	var payload []byte
	_ = e.admin.QueryRow(context.Background(), `SELECT payload FROM eventing_outbox LIMIT 1`).Scan(&payload)
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("the stored event is a valid envelope: %v", err)
	}
	for _, k := range []string{"id", "type", "tenantid", "residencyregion", "payloadhash", "aggregateid"} {
		if _, ok := env[k]; !ok {
			t.Errorf("event envelope lacks %q", k)
		}
	}
}
