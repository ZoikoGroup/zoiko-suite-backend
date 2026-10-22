package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

// These run the XIC-02 connection layer against REAL Postgres as an ordinary role: the SQL, the guard triggers and
// constraints the database holds itself, row-level security across tenants, the outbox and concurrency.

const (
	cRefOAuth  = "0a000000-0000-4000-8000-0000000000a1"
	cRefHMAC   = "0a000000-0000-4000-8000-0000000000a2"
	cRefOAuth2 = "0a000000-0000-4000-8000-0000000000a3"
	cRefHMAC2  = "0a000000-0000-4000-8000-0000000000a4"
	cRefOAuth3 = "0a000000-0000-4000-8000-0000000000a5"
	cTenantA   = "tenant-a"
	cTenantB   = "tenant-b"
)

type allVault struct{}

func (allVault) Verify(context.Context, string, vault.Caller) error { return nil }

type pubDNS struct{}

func (pubDNS) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type connEnv struct {
	*regEnv
	repo   *store.XICConnRepo
	svc    *xicconn.Service
	clock  func() time.Time
	nowAdd time.Duration
	mu     sync.Mutex
}

func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	r := newRegEnv(t)
	st := store.NewPgStore(r.app, store.WithEventRegion("uk"))
	e := &connEnv{regEnv: r, repo: store.NewXICConnRepo(st)}
	e.svc = &xicconn.Service{Repo: e.repo, Contracts: store.NewRegistryRepo(st), Vault: allVault{}, Egress: egress.Policy{Resolver: pubDNS{}},
		AllowedRegions: []string{"uk"}, RedirectAllowlist: []string{"https://app.zoiko.example/oauth/callback"},
		Clock: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return time.Now().UTC().Add(e.nowAdd) }}
	return e
}

func (e *connEnv) advance(d time.Duration) { e.mu.Lock(); e.nowAdd += d; e.mu.Unlock() }

// published returns a PUBLISHED contract version of connector id (platform scope), with an effective date in the past
// relative to the real clock so it can be activated.
func (e *connEnv) published(t *testing.T, connectorID, semver string, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	t.Helper()
	if _, err := store.NewRegistryRepo(store.NewPgStore(e.app)).GetConnector(platform(), connectorID); err != nil {
		e.connector(t, connectorID)
	}
	in := registrytest.Input(semver)
	in.EffectiveFrom = time.Now().Add(-time.Hour).UTC()
	if mutate != nil {
		mutate(&in)
	}
	v, _, err := e.regEnv.svc.CreateVersion(platform(), registry.PlatformScopeID, rAuthor, connectorID, in, "")
	if err != nil {
		t.Fatal(err)
	}
	return e.regEnv.publish(t, e.regEnv.reviewed(t, v))
}

func (e *connEnv) create(t *testing.T, tenant, environment string, v *registry.ContractVersion, account string) *xicconn.Connection {
	t.Helper()
	c, _, err := e.svc.Create(tenantCtx(tenant), cCreator, xicconn.CreateInput{Environment: environment, ConnectorID: v.ConnectorID,
		ContractVersionID: v.ContractVersionID, Region: "uk", Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: account}, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return c
}

var (
	cCreator   = xicconn.Actor{PrincipalID: "creator", CorrelationID: "corr-c1"}
	cOperator  = xicconn.Actor{PrincipalID: "operator"}
	cApprover  = xicconn.Actor{PrincipalID: "approver"}
	cCertifier = xicconn.Actor{PrincipalID: "certifier"}
)

func tenantCtx(id string) context.Context { return tenant(id) }

func (e *connEnv) bound(t *testing.T, tn string, c *xicconn.Connection, oauthRef, hmacRef string) *xicconn.Connection {
	t.Helper()
	ctx := tenantCtx(tn)
	res, err := e.svc.Authorize(ctx, cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil {
		t.Fatal(err)
	}
	cur := res.Connection
	exp := time.Now().Add(60 * 24 * time.Hour)
	if cur, err = e.svc.Bind(ctx, cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: oauthRef,
		GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "tenant-admin", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if cur, err = e.svc.Bind(ctx, cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: hmacRef, ConsentActor: "tenant-admin"}); err != nil {
		t.Fatal(err)
	}
	return cur
}

func (e *connEnv) validatedC(t *testing.T, tn string, c *xicconn.Connection) *xicconn.Connection {
	t.Helper()
	res, err := e.svc.Validate(tenantCtx(tn), cOperator, c.ConnectionID, c.Version, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return res.Connection
}

func (e *connEnv) sandbox(t *testing.T, tn string, v *registry.ContractVersion, account, oauthRef, hmacRef string) *xicconn.Connection {
	t.Helper()
	c := e.validatedC(t, tn, e.bound(t, tn, e.create(t, tn, xicconn.EnvSandbox, v, account), oauthRef, hmacRef))
	c, err := e.svc.Activate(tenantCtx(tn), cOperator, c.ConnectionID, c.Version)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	return c
}

func contractOps(v *registry.ContractVersion) []string {
	var out []string
	for _, o := range v.Content.Operations {
		out = append(out, o.ID)
	}
	return out
}

func (e *connEnv) probe(t *testing.T, tn string, c *xicconn.Connection, account string, v *registry.ContractVersion) {
	t.Helper()
	ck, err := e.svc.ReportProbe(tenantCtx(tn), cOperator, c.ConnectionID, 0, xicconn.ProbeInput{AccountRef: account, Capabilities: contractOps(v)})
	if err != nil || ck.Outcome != xicconn.Passed {
		t.Fatalf("probe: %v %+v", err, ck)
	}
}

func (e *connEnv) certify(t *testing.T, tn string, c *xicconn.Connection) {
	t.Helper()
	var d struct {
		Passed []string `json:"passed"`
	}
	_ = json.Unmarshal(registrytest.Passed(), &d)
	if _, err := e.svc.Certify(tenantCtx(tn), cCertifier, c.ConnectionID, xicconn.CertifyInput{TestPackVersion: "tp-2026.10", Passed: d.Passed}); err != nil {
		t.Fatalf("certify: %v", err)
	}
}

func (e *connEnv) production(t *testing.T, tn string, v *registry.ContractVersion, account, oauthRef, hmacRef string) *xicconn.Connection {
	t.Helper()
	c := e.validatedC(t, tn, e.bound(t, tn, e.create(t, tn, xicconn.EnvProduction, v, account), oauthRef, hmacRef))
	e.probe(t, tn, c, account, v)
	e.certify(t, tn, c)
	cur, err := e.repo.Get(tenantCtx(tn), c.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	act, err := e.svc.Activate(tenantCtx(tn), cOperator, c.ConnectionID, cur.Version)
	if err != nil {
		t.Fatalf("activate production: %v", err)
	}
	return act
}

func (e *connEnv) refusal(t *testing.T, err error, code xicconn.RefusalCode) {
	t.Helper()
	var r *xicconn.Refusal
	if !errors.As(err, &r) || r.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

// ---- schema -----------------------------------------------------------------------------

func TestXICConn_MigrationIsRerunnable(t *testing.T) {
	e := newConnEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "005") {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.admin.Exec(context.Background(), string(b)); err != nil {
				t.Fatalf("005 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 005 migration")
}

// ---- lifecycle through the database -----------------------------------------------------------

func TestXICConn_SandboxLifecycleAndEvidence(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	c := e.sandbox(t, cTenantA, v, "acc-1", cRefOAuth, cRefHMAC)
	if c.Status != xicconn.StatusActive || c.ActivatedBy != "operator" || len(c.Bindings) != 2 {
		t.Fatalf("%+v", c)
	}
	ctx := tenantCtx(cTenantA)
	evs, _ := e.repo.ListEvents(ctx, c.ConnectionID)
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	if got := strings.Join(kinds, ","); got != "CREATE,AUTHORIZE,BIND,BIND,VALIDATE,ACTIVATE" {
		t.Fatalf("evidence trail %s", got)
	}
	checks, _ := e.repo.ListChecks(ctx, c.ConnectionID)
	if len(checks) != 1 || checks[0].Outcome != xicconn.Passed || checks[0].ConfigFingerprint != xicconn.ConfigFingerprint(c) {
		t.Fatalf("%+v", checks)
	}
	// the raw provider account is nowhere in the row
	var n int
	if err := e.admin.QueryRow(context.Background(), `SELECT count(*) FROM xic_connections WHERE to_jsonb(xic_connections)::text LIKE '%acc-1%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the raw account reference is stored: %d %v", n, err)
	}
	d, err := e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	if err != nil || d.Host != "api.bank-x.com" || d.CredentialRefs[0].Ref != cRefOAuth {
		t.Fatalf("%v %+v", err, d)
	}
	// the §14.1 events carry their anchors
	var payload []byte
	if err := e.admin.QueryRow(context.Background(), `SELECT payload FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connection.activated'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(payload, &env)
	for _, k := range []string{"tenant_id", "connection_id", "connector_id", "contract_version_id", "region_profile"} {
		if _, ok := env.Data[k]; !ok {
			t.Errorf("activated event lacks %s: %s", k, payload)
		}
	}
}

func TestXICConn_ProductionNeedsProbeAndCertificationAndOperabilityFollowsThem(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	ctx := tenantCtx(cTenantA)
	c := e.validatedC(t, cTenantA, e.bound(t, cTenantA, e.create(t, cTenantA, xicconn.EnvProduction, v, "acc"), cRefOAuth, cRefHMAC))
	if _, err := e.svc.Activate(ctx, cOperator, c.ConnectionID, c.Version); !errors.Is(err, xicconn.ErrNotValidated) {
		t.Fatalf("no probe: %v", err)
	}
	e.probe(t, cTenantA, c, "acc", v)
	cur, _ := e.repo.Get(ctx, c.ConnectionID)
	if _, err := e.svc.Activate(ctx, cOperator, c.ConnectionID, cur.Version); !errors.Is(err, xicconn.ErrNotCertified) {
		t.Fatalf("no certification: %v", err)
	}
	e.certify(t, cTenantA, c)
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	act, err := e.svc.Activate(ctx, cOperator, c.ConnectionID, cur.Version)
	if err != nil || act.Status != xicconn.StatusActive {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit"); err != nil {
		t.Fatalf("%v", err)
	}
	// the certification expires: XIC-017 (credentials extended so only the certification is at stake)
	exp := time.Now().Add(400 * 24 * time.Hour)
	if _, err := e.svc.Rotate(ctx, cOperator, c.ConnectionID, act.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: cRefOAuth3, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	e.advance(181 * 24 * time.Hour)
	_, err = e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	e.refusal(t, err, xicconn.CodeCertStale)
}

func TestXICConn_MigrationMovesThePinThroughTheDatabaseGuards(t *testing.T) {
	e := newConnEnv(t)
	v1 := e.published(t, "bank-x", "1.0.0", nil)
	c := e.production(t, cTenantA, v1, "acc", cRefOAuth, cRefHMAC)
	ctx := tenantCtx(cTenantA)
	v2 := e.published(t, "bank-x", "1.1.0", func(in *registry.VersionInput) { in.Content.Operations[0].RateLimitPerMinute = 321 })
	req, err := e.svc.RequestMigration(ctx, cCreator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt 1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.DecideMigration(ctx, cCreator, c.ConnectionID, req.RequestID, true); !errors.Is(err, xicconn.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.DecideMigration(ctx, cApprover, c.ConnectionID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.Get(ctx, c.ConnectionID)
	if cur.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("pin %s", cur.ContractVersionID)
	}
	_, err = e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	e.refusal(t, err, xicconn.CodeCertStale) // I-29: the certification covered v1
	reqs, _ := e.repo.ListRequests(ctx, c.ConnectionID)
	if len(reqs) != 1 || reqs[0].Status != xicconn.ReqUsed || reqs[0].DecidedBy != "approver" {
		t.Fatalf("%+v", reqs)
	}
}

func TestXICConn_FutureDatedMigrationAppliesWhenDue(t *testing.T) {
	e := newConnEnv(t)
	v1 := e.published(t, "bank-x", "1.0.0", nil)
	c := e.sandbox(t, cTenantA, v1, "acc", cRefOAuth, cRefHMAC)
	ctx := tenantCtx(cTenantA)
	v2 := e.published(t, "bank-x", "1.1.0", nil)
	when := time.Now().Add(48 * time.Hour)
	req, err := e.svc.RequestMigration(ctx, cCreator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt", EffectiveAt: &when})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.DecideMigration(ctx, cApprover, c.ConnectionID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	if cur, _ := e.repo.Get(ctx, c.ConnectionID); cur.ContractVersionID != v1.ContractVersionID {
		t.Fatal("pin moved early")
	}
	e.advance(49 * time.Hour)
	d, err := e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	if err != nil || d.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("%v %+v", err, d)
	}
	// the worker finds it, and Evaluate persists it
	due, err := e.repo.Due(context.Background(), time.Now().Add(49*time.Hour), 14*24*time.Hour, 50)
	if err != nil || len(due) == 0 {
		t.Fatalf("%v %+v", err, due)
	}
	res, err := e.svc.Evaluate(ctx, cOperator, c.ConnectionID)
	if err != nil || res.Connection.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("%v %+v", err, res)
	}
}

// ---- tenants ----------------------------------------------------------------------------------

func TestXICConn_TenantIsolation(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	c := e.sandbox(t, cTenantA, v, "acc", cRefOAuth, cRefHMAC)
	b := tenantCtx(cTenantB)
	if _, err := e.repo.Get(b, c.ConnectionID); !errors.Is(err, xicconn.ErrNotFound) {
		t.Fatalf("tenant B read tenant A's connection: %v", err)
	}
	if list, _ := e.repo.List(b, xicconn.Filter{}); len(list) != 0 {
		t.Fatalf("%d", len(list))
	}
	for _, tbl := range []string{"xic_connections", "xic_credential_bindings", "xic_connection_events", "xic_connection_checks", "xic_authorization_sessions", "xic_connection_requests", "xic_conn_idempotency"} {
		if n := e.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d rows of %s", n, tbl)
		}
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM xic_credential_bindings`); n != 2 {
		t.Fatalf("tenant A sees %d bindings", n)
	}
	for name, fn := range map[string]func() error{
		"suspend":  func() error { _, err := e.svc.Suspend(b, cOperator, c.ConnectionID, c.Version, "x"); return err },
		"revoke":   func() error { _, err := e.svc.Revoke(b, cOperator, c.ConnectionID, c.Version, "x"); return err },
		"validate": func() error { _, err := e.svc.Validate(b, cOperator, c.ConnectionID, c.Version, ""); return err },
		"activate": func() error { _, err := e.svc.Activate(b, cOperator, c.ConnectionID, c.Version); return err },
	} {
		if err := fn(); !errors.Is(err, xicconn.ErrNotFound) {
			t.Errorf("%s from tenant B: %v", name, err)
		}
	}
	// raw writes under another tenant's scope change nothing, and forged rows are refused
	e.wantRefused(t, "update", cTenantB, `UPDATE xic_connections SET status_reason='x', version=version+1`)
	e.wantRefused(t, "delete", cTenantB, `DELETE FROM xic_connections`)
	wantDBError(t, "forged connection", e.execAs(cTenantB, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
		SELECT gen_random_uuid(), 'tenant-a', 'SANDBOX', connector_id, contract_version_id, 'uk', repeat('a',64), 'evil' FROM xic_contract_versions WHERE status='PUBLISHED' LIMIT 1`), "")
	// the cross-tenant resolver reads, and only reads
	got, err := e.repo.GetAcrossTenants(context.Background(), c.ConnectionID)
	if err != nil || got.TenantID != cTenantA {
		t.Fatalf("%v", err)
	}
	if err := e.execAsWorker(`UPDATE xic_connections SET status_reason='x', version=version+1`); err == nil {
		// zero rows is also a refusal; confirm nothing changed
		var r string
		_ = e.admin.QueryRow(context.Background(), `SELECT status_reason FROM xic_connections`).Scan(&r)
		if r == "x" {
			t.Fatal("the cross-tenant reader changed a connection")
		}
	}
	if err := e.execAsWorker(`INSERT INTO xic_connection_events (connection_id, tenant_id, kind, actor) SELECT connection_id, tenant_id, 'FORGED', 'w' FROM xic_connections LIMIT 1`); err == nil {
		t.Fatal("the cross-tenant reader inserted a row")
	}
	// operability across tenants: XIC-008 and a security signal
	_, err = e.svc.Operable(b, cTenantB, c.ConnectionID, "payment-submit")
	e.refusal(t, err, xicconn.CodeTenantScope)
	if n := e.countAs(cTenantB, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.tenant-scope-mismatch'`); n != 1 {
		t.Fatalf("%d tenant-scope signals", n)
	}
}

func (e *connEnv) execAsWorker(sql string) error {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true), set_config('app.xic_worker','true',true)`); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, sql)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("no rows")
	}
	return tx.Commit(ctx)
}

// ---- credential exclusivity --------------------------------------------------------------------

func TestXICConn_ACredentialReferenceIsBoundOnceAcrossTenants(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	a := e.create(t, cTenantA, xicconn.EnvSandbox, v, "acc-a")
	b := e.create(t, cTenantB, xicconn.EnvSandbox, v, "acc-b")
	for _, c := range []*xicconn.Connection{a, b} {
		tn := c.TenantID
		if _, err := e.svc.Authorize(tenantCtx(tn), cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"}); err != nil {
			t.Fatal(err)
		}
	}
	exp := time.Now().Add(24 * time.Hour)
	in := xicconn.BindInput{AuthProfile: "oauth", CredentialRef: cRefOAuth, GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp}
	curA, _ := e.repo.Get(tenantCtx(cTenantA), a.ConnectionID)
	if _, err := e.svc.Bind(tenantCtx(cTenantA), cCreator, a.ConnectionID, curA.Version, in); err != nil {
		t.Fatal(err)
	}
	curB, _ := e.repo.Get(tenantCtx(cTenantB), b.ConnectionID)
	_, err := e.svc.Bind(tenantCtx(cTenantB), cCreator, b.ConnectionID, curB.Version, in)
	if !errors.Is(err, xicconn.ErrCrossTenantCredential) {
		t.Fatalf("the database accepted one reference in two tenants: %v", err)
	}
	if n := e.countAs(cTenantB, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.cross-tenant-credential'`); n != 1 {
		t.Fatalf("%d security signals survived the failed bind", n)
	}
	if got, _ := e.repo.Get(tenantCtx(cTenantB), b.ConnectionID); len(got.Bindings) != 0 || got.Version != curB.Version {
		t.Fatalf("the failed bind left residue: %+v", got)
	}
	// raw SQL cannot sidestep it either, in the same or a different case
	wantDBError(t, "raw bind by B", e.execAs(cTenantB, `INSERT INTO xic_credential_bindings (binding_id, connection_id, tenant_id, auth_profile, current_ref, scope_fingerprint, bound_by)
		VALUES (gen_random_uuid(), $1::uuid, 'tenant-b', 'oauth', upper($2), repeat('a',64), 'x')`, b.ConnectionID, cRefOAuth), "xic_bind_ref_exclusive")
	// a revoked connection releases it
	if _, err = e.svc.Revoke(tenantCtx(cTenantA), cOperator, a.ConnectionID, curA.Version+1, "gone"); err != nil {
		t.Fatal(err)
	}
	curB, _ = e.repo.Get(tenantCtx(cTenantB), b.ConnectionID)
	if _, err = e.svc.Bind(tenantCtx(cTenantB), cCreator, b.ConnectionID, curB.Version, in); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
}

// ---- guards the database holds by itself ------------------------------------------------------

func TestXICConn_DatabaseGuards(t *testing.T) {
	e := newConnEnv(t)
	v1 := e.published(t, "bank-x", "1.0.0", nil)
	act := e.sandbox(t, cTenantA, v1, "acc-act", cRefOAuth, cRefHMAC)
	v2 := e.published(t, "bank-x", "1.1.0", nil)
	draft := e.create(t, cTenantA, xicconn.EnvSandbox, v2, "acc-draft")
	A := cTenantA
	bump := `, version = version + 1`
	id := act.ConnectionID

	t.Run("identity is immutable", func(t *testing.T) {
		for col, set := range map[string]string{
			"tenant":      `tenant_id='tenant-b'`,
			"environment": `environment='PRODUCTION'`,
			"connector":   `connector_id='other-one'`,
			"account":     `provider_account_hash=repeat('b',64)`,
			"created_by":  `created_by='someone'`,
		} {
			wantDBError(t, col, e.execAs(A, `UPDATE xic_connections SET `+set+bump+` WHERE connection_id=$1::uuid`, id), "")
		}
	})
	t.Run("version advances by exactly one", func(t *testing.T) {
		wantDBError(t, "no bump", e.execAs(A, `UPDATE xic_connections SET status_reason='x' WHERE connection_id=$1::uuid`, id), "version")
		wantDBError(t, "jump", e.execAs(A, `UPDATE xic_connections SET status_reason='x', version=version+2 WHERE connection_id=$1::uuid`, id), "version")
	})
	t.Run("the pin moves only through an approved migration", func(t *testing.T) {
		wantDBError(t, "raw re-pin", e.execAs(A, `UPDATE xic_connections SET contract_version_id=$2::uuid`+bump+` WHERE connection_id=$1::uuid`, id, v2.ContractVersionID), "pinned")
		// even with the flag, no approved request means no move
		wantDBError(t, "flag alone", e.execSeq(A, "SELECT set_config('app.xic_migration','true',true)", "UPDATE xic_connections SET contract_version_id='"+v2.ContractVersionID+"'::uuid"+bump+" WHERE connection_id='"+id+"'::uuid"), "pinned")
	})
	t.Run("status only moves along the graph", func(t *testing.T) {
		for _, c := range []struct{ name, id, to string }{
			{"draft to active", draft.ConnectionID, "ACTIVE"},
			{"draft to validating", draft.ConnectionID, "VALIDATING"},
			{"draft to suspended", draft.ConnectionID, "SUSPENDED"},
			{"active to draft", id, "DRAFT"},
			{"active to authorizing", id, "AUTHORIZING"},
			{"active to validating", id, "VALIDATING"},
		} {
			wantDBError(t, c.name, e.execAs(A, `UPDATE xic_connections SET status=$2, status_reason='x'`+bump+` WHERE connection_id=$1::uuid`, c.id, c.to), "")
		}
	})
	t.Run("a live connection was activated by someone else", func(t *testing.T) {
		wantDBError(t, "self-activated", e.execAs(A, `UPDATE xic_connections SET activated_by=created_by`+bump+` WHERE connection_id=$1::uuid`, id), "")
	})
	t.Run("reasons and dates are required", func(t *testing.T) {
		wantDBError(t, "suspend without reason", e.execAs(A, `UPDATE xic_connections SET status='SUSPENDED', status_reason=''`+bump+` WHERE connection_id=$1::uuid`, id), "")
		wantDBError(t, "revoke without date", e.execAs(A, `UPDATE xic_connections SET status='REVOKED', status_reason='x', revoked_at=NULL`+bump+` WHERE connection_id=$1::uuid`, id), "")
	})
	t.Run("nothing is deleted", func(t *testing.T) {
		e.wantRefused(t, "connection", A, `DELETE FROM xic_connections WHERE connection_id=$1::uuid`, id)
		e.wantRefused(t, "binding", A, `DELETE FROM xic_credential_bindings`)
		for _, tbl := range []string{"xic_connection_events", "xic_connection_checks"} {
			e.wantRefused(t, "update "+tbl, A, `UPDATE `+tbl+` SET tenant_id=tenant_id`)
			e.wantRefused(t, "delete "+tbl, A, `DELETE FROM `+tbl)
		}
	})
	t.Run("bindings", func(t *testing.T) {
		wantDBError(t, "malformed ref", e.execAs(A, `UPDATE xic_credential_bindings SET current_ref='not-a-uuid' WHERE connection_id=$1::uuid`, id), "")
		wantDBError(t, "previous equals current", e.execAs(A, `UPDATE xic_credential_bindings SET previous_ref=current_ref, previous_grace_until=now() WHERE connection_id=$1::uuid AND auth_profile='oauth'`, id), "")
		wantDBError(t, "previous without a grace end", e.execAs(A, `UPDATE xic_credential_bindings SET previous_ref=$2 WHERE connection_id=$1::uuid AND auth_profile='oauth'`, id, "0a000000-0000-4000-8000-0000000000f1"), "")
		wantDBError(t, "status outside the set", e.execAs(A, `UPDATE xic_credential_bindings SET status='GREAT' WHERE connection_id=$1::uuid`, id), "")
		wantDBError(t, "profile re-keyed", e.execAs(A, `UPDATE xic_credential_bindings SET auth_profile='other' WHERE connection_id=$1::uuid AND auth_profile='oauth'`, id), "immutable")
	})
	t.Run("checks and certifications are well formed", func(t *testing.T) {
		wantDBError(t, "passing check with issues", e.execAs(A, `INSERT INTO xic_connection_checks (check_id, connection_id, tenant_id, kind, outcome, config_fingerprint, issues, run_by)
			VALUES (gen_random_uuid(), $1::uuid, 'tenant-a', 'VALIDATION', 'PASSED', repeat('a',64), '[{"path":"x","rule":"y"}]', 'x')`, id), "")
		wantDBError(t, "unknown check kind", e.execAs(A, `INSERT INTO xic_connection_checks (check_id, connection_id, tenant_id, kind, outcome, config_fingerprint, run_by)
			VALUES (gen_random_uuid(), $1::uuid, 'tenant-a', 'MAGIC', 'PASSED', repeat('a',64), 'x')`, id), "")
		wantDBError(t, "certification that expires before it starts", e.execAs(A, `INSERT INTO xic_certifications (certification_id, connection_id, tenant_id, environment, contract_version_id, scope_fingerprint, test_pack_version, outcome, certified_by, at, expires_at)
			VALUES (gen_random_uuid(), $1::uuid, 'tenant-a', 'SANDBOX', $2::uuid, repeat('a',64), 'tp', 'PASSED', 'x', now(), now() - interval '1 day')`, id, v1.ContractVersionID), "")
	})
	t.Run("creation pins a published contract of the same connector", func(t *testing.T) {
		other := e.published(t, "bank-y", "1.0.0", nil)
		wantDBError(t, "wrong connector", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
			VALUES (gen_random_uuid(), 'tenant-a', 'SANDBOX', 'bank-x', $1::uuid, 'uk', repeat('c',64), 'x')`, other.ContractVersionID), "does not belong")
		wantDBError(t, "superseded contract", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
			VALUES (gen_random_uuid(), 'tenant-a', 'SANDBOX', 'bank-x', $1::uuid, 'uk', repeat('c',64), 'x')`, v1.ContractVersionID), "")
		wantDBError(t, "created active", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, status, region, provider_account_hash, created_by)
			VALUES (gen_random_uuid(), 'tenant-a', 'SANDBOX', 'bank-x', $1::uuid, 'ACTIVE', 'uk', repeat('c',64), 'x')`, v2.ContractVersionID), "")
		wantDBError(t, "bad environment", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
			VALUES (gen_random_uuid(), 'tenant-a', 'DEV', 'bank-x', $1::uuid, 'uk', repeat('c',64), 'x')`, v2.ContractVersionID), "")
		wantDBError(t, "bad hash", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
			VALUES (gen_random_uuid(), 'tenant-a', 'SANDBOX', 'bank-x', $1::uuid, 'uk', 'nothex', 'x')`, v2.ContractVersionID), "")
	})
	t.Run("one live connection per account", func(t *testing.T) {
		wantDBError(t, "duplicate", e.execAs(A, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, region, provider_account_hash, created_by)
			SELECT gen_random_uuid(), tenant_id, environment, connector_id, $2::uuid, region, provider_account_hash, 'x' FROM xic_connections WHERE connection_id=$1::uuid`, id, v2.ContractVersionID), "xic_conn_one_per_account")
	})
}

func TestXICConn_ActivationEvidenceIsCheckedByTheDatabase(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	A := cTenantA
	bump := `, version = version + 1`

	// VALIDATING with a passed validation: ACTIVE is allowed only for exactly that configuration
	c := e.validatedC(t, A, e.bound(t, A, e.create(t, A, xicconn.EnvSandbox, v, "acc"), cRefOAuth, cRefHMAC))
	wantDBError(t, "fingerprint differs from the validated one", e.execAs(A, `UPDATE xic_connections SET status='ACTIVE', activated_by='operator', activated_at=now(), config_fingerprint=repeat('e',64)`+bump+` WHERE connection_id=$1::uuid`, c.ConnectionID), "passed validation")
	// no validation at all
	d := e.bound(t, A, e.create(t, A, xicconn.EnvSandbox, v, "acc-2"), cRefOAuth2, cRefHMAC2)
	wantDBError(t, "no validation", e.execAs(A, `UPDATE xic_connections SET status='VALIDATING'`+bump+` WHERE connection_id=$1::uuid`, d.ConnectionID), "")
	// a production connection with a passed validation but no certification
	p := e.validatedC(t, A, e.bound(t, A, e.create(t, A, xicconn.EnvProduction, v, "acc-p"), "0a000000-0000-4000-8000-0000000000e1", "0a000000-0000-4000-8000-0000000000e2"))
	wantDBError(t, "production without certification", e.execAs(A, `UPDATE xic_connections SET status='ACTIVE', activated_by='operator', activated_at=now()`+bump+` WHERE connection_id=$1::uuid`, p.ConnectionID), "certified")
	// a stale (older) validation does not count: add a FAILED validation as the latest
	if err := e.execAs(A, `INSERT INTO xic_connection_checks (check_id, connection_id, tenant_id, kind, outcome, config_fingerprint, issues, run_by)
		VALUES (gen_random_uuid(), $1::uuid, 'tenant-a', 'VALIDATION', 'FAILED', $2, '[{"path":"x","rule":"y"}]', 'x')`, c.ConnectionID, c.ConfigFingerprint); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "latest validation failed", e.execAs(A, `UPDATE xic_connections SET status='ACTIVE', activated_by='operator', activated_at=now()`+bump+` WHERE connection_id=$1::uuid`, c.ConnectionID), "passed validation")
}

func TestXICConn_AuthorizationSessionAndRequestGuards(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "oauth" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
	A := cTenantA
	c := e.create(t, A, xicconn.EnvSandbox, v, "acc")
	res, err := e.svc.Authorize(tenantCtx(A), cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "OAUTH_AUTH_CODE", RedirectURI: "https://app.zoiko.example/oauth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	// the state is only ever stored as a hash
	if n := e.countAs(A, `SELECT count(*) FROM xic_authorization_sessions WHERE to_jsonb(xic_authorization_sessions)::text LIKE '%'||$1||'%'`, res.State); n != 0 {
		t.Fatal("the raw authorization state is stored")
	}
	exp := time.Now().Add(24 * time.Hour)
	bind := func(state string) error {
		cur, _ := e.repo.Get(tenantCtx(A), c.ConnectionID)
		_, err := e.svc.Bind(tenantCtx(A), cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: cRefOAuth,
			GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp, State: state})
		return err
	}
	if err := bind("wrong-state-wrong-state-wrong-state-wrong-state-xxxx"); !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("%v", err)
	}
	if err := bind(res.State); err != nil {
		t.Fatal(err)
	}
	if err := bind(res.State); !errors.Is(err, xicconn.ErrAuthState) {
		t.Fatalf("replayed state: %v", err)
	}
	// single-use and immutable at the database
	wantDBError(t, "consume twice", e.execAs(A, `UPDATE xic_authorization_sessions SET consumed_at=now() WHERE connection_id=$1::uuid`, c.ConnectionID), "single-use")
	wantDBError(t, "rewrite the state hash", e.execAs(A, `UPDATE xic_authorization_sessions SET state_hash=repeat('a',64) WHERE connection_id=$1::uuid`, c.ConnectionID), "")
	e.wantRefused(t, "delete a session", A, `DELETE FROM xic_authorization_sessions`)

	// requests: decided by someone else, payload fixed
	cur, _ := e.repo.Get(tenantCtx(A), c.ConnectionID)
	if _, err := e.svc.Bind(tenantCtx(A), cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: cRefHMAC, ConsentActor: "x"}); err != nil {
		t.Fatal(err)
	}
	v2 := e.published(t, "bank-x", "1.1.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "oauth" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
	req, err := e.svc.RequestMigration(tenantCtx(A), cCreator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt"})
	if err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "self decision", e.execAs(A, `UPDATE xic_connection_requests SET status='APPROVED', decided_by=requested_by WHERE request_id=$1::uuid`, req.RequestID), "")
	wantDBError(t, "payload rewrite", e.execAs(A, `UPDATE xic_connection_requests SET payload='{}' WHERE request_id=$1::uuid`, req.RequestID), "")
	wantDBError(t, "reason rewrite", e.execAs(A, `UPDATE xic_connection_requests SET reason='x' WHERE request_id=$1::uuid`, req.RequestID), "")
	e.wantRefused(t, "delete a request", A, `DELETE FROM xic_connection_requests`)
	if _, err := e.svc.DecideMigration(tenantCtx(A), cApprover, c.ConnectionID, req.RequestID, false); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "a decided request changes again", e.execAs(A, `UPDATE xic_connection_requests SET status='APPROVED', decided_by='someone' WHERE request_id=$1::uuid`, req.RequestID), "")
}

// ---- idempotency and concurrency ------------------------------------------------------------------

func TestXICConn_CreateIsIdempotent(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	in := xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "acc"}
	a, created, err := e.svc.Create(tenantCtx(cTenantA), cCreator, in, "k1")
	if err != nil || !created {
		t.Fatal(err)
	}
	b, created, err := e.svc.Create(tenantCtx(cTenantA), cCreator, in, "k1")
	if err != nil || created || b.ConnectionID != a.ConnectionID {
		t.Fatalf("%v %v", err, created)
	}
	in2 := in
	in2.ProviderAccountRef = "other"
	if _, _, err = e.svc.Create(tenantCtx(cTenantA), cCreator, in2, "k1"); !errors.Is(err, xicconn.ErrIdempotency) {
		t.Fatalf("%v", err)
	}
	// the same key under another tenant is that tenant's own
	if _, created, err = e.svc.Create(tenantCtx(cTenantB), cCreator, in, "k1"); err != nil || !created {
		t.Fatalf("%v %v", err, created)
	}
	if _, _, err = e.svc.Create(tenantCtx(cTenantA), cCreator, in, "k2"); !errors.Is(err, xicconn.ErrExists) {
		t.Fatalf("%v", err)
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connection.created'`); n != 1 {
		t.Fatalf("%d created events for tenant A", n)
	}
}

func TestXICConn_ConcurrentActivation(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	c := e.validatedC(t, cTenantA, e.bound(t, cTenantA, e.create(t, cTenantA, xicconn.EnvSandbox, v, "acc"), cRefOAuth, cRefHMAC))
	const n = 12
	var ok, lost, other int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Activate(tenantCtx(cTenantA), cOperator, c.ConnectionID, c.Version)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, xicconn.ErrVersionConflict), errors.Is(err, xicconn.ErrTransition):
				atomic.AddInt32(&lost, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || lost != n-1 || other != 0 {
		t.Fatalf("ok=%d lost=%d other=%d", ok, lost, other)
	}
	if got := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connection.activated'`); got != 1 {
		t.Fatalf("%d activated events", got)
	}
}

func TestXICConn_ConcurrentCredentialClaimsAcrossTenants(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	const n = 8
	conns := make([]*xicconn.Connection, n)
	tenants := make([]string, n)
	for i := range conns {
		tenants[i] = "tenant-" + string(rune('a'+i))
		c := e.create(t, tenants[i], xicconn.EnvSandbox, v, "acc")
		res, err := e.svc.Authorize(tenantCtx(tenants[i]), cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = res.Connection
	}
	exp := time.Now().Add(24 * time.Hour)
	var won, lost int32
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.svc.Bind(tenantCtx(tenants[i]), cCreator, conns[i].ConnectionID, conns[i].Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: cRefOAuth,
				GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp})
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, xicconn.ErrCrossTenantCredential):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || lost != n-1 {
		t.Fatalf("won=%d lost=%d: a credential reference was bound in more than one tenant", won, lost)
	}
	var bound int
	_ = e.admin.QueryRow(context.Background(), `SELECT count(*) FROM xic_credential_bindings WHERE lower(current_ref)=lower($1)`, cRefOAuth).Scan(&bound)
	if bound != 1 {
		t.Fatalf("%d bindings hold the reference", bound)
	}
}

func TestXICConn_ConcurrentCreatesOfOneAccount(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	var won, lost int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.Create(tenantCtx(cTenantA), cCreator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID,
				Region: "uk", Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: "same"}, "")
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, xicconn.ErrExists):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 9 {
		t.Fatalf("won=%d lost=%d", won, lost)
	}
}

func TestXICConn_RotationAndEvaluationThroughPostgres(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	c := e.sandbox(t, cTenantA, v, "acc", cRefOAuth, cRefHMAC)
	ctx := tenantCtx(cTenantA)
	exp := time.Now().Add(120 * 24 * time.Hour)
	rot, err := e.svc.Rotate(ctx, cOperator, c.ConnectionID, c.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: cRefOAuth3, GraceSeconds: 3600, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	b := rot.BindingFor("oauth")
	if b.CurrentRef != cRefOAuth3 || b.PreviousRef != cRefOAuth || b.PreviousGraceUntil == nil {
		t.Fatalf("%+v", b)
	}
	d, err := e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	if err != nil || len(d.CredentialRefs) != 2 || !d.CredentialRefs[1].Grace {
		t.Fatalf("%v %+v", err, d)
	}
	// the old reference is free for another tenant once the grace window ends: while it is held as the previous
	// reference it is not in the exclusivity index, which only covers current references (documented limit)
	e.advance(2 * time.Hour)
	res, err := e.svc.Evaluate(ctx, cOperator, c.ConnectionID)
	if err != nil || res.Connection.BindingFor("oauth").PreviousRef != "" {
		t.Fatalf("%v %+v", err, res)
	}
	e.advance(110 * 24 * time.Hour) // 12 days left: inside the warning window
	res, err = e.svc.Evaluate(ctx, cOperator, c.ConnectionID)
	if err != nil || res.Connection.Status != xicconn.StatusDegraded || res.Connection.StatusReason != "CREDENTIAL_EXPIRING" {
		t.Fatalf("%v %+v", err, res)
	}
	due, _ := e.repo.Due(context.Background(), time.Now().Add(110*24*time.Hour+2*time.Hour), 14*24*time.Hour, 50)
	found := false
	for _, x := range due {
		found = found || x.ConnectionID == c.ConnectionID
	}
	if !found {
		t.Fatalf("the worker's due list misses an expiring connection: %+v", due)
	}
	e.advance(15 * 24 * time.Hour)
	res, _ = e.svc.Evaluate(ctx, cOperator, c.ConnectionID)
	if res.Connection.StatusReason != "CREDENTIAL_EXPIRED" {
		t.Fatalf("%+v", res.Connection)
	}
	_, err = e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit")
	e.refusal(t, err, xicconn.CodeNotActive)
}

func TestXICConn_BreakGlassAndSuspensionThroughPostgres(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", nil)
	c := e.sandbox(t, cTenantA, v, "acc", cRefOAuth, cRefHMAC)
	ctx := tenantCtx(cTenantA)
	if _, _, err := e.svc.RequestBreakGlass(ctx, cOperator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "REVEAL_CREDENTIAL", Reason: "debugging"}); !errors.Is(err, xicconn.ErrBreakGlassDenied) {
		t.Fatalf("%v", err)
	}
	if n := e.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connection.break-glass-denied'`); n != 1 {
		t.Fatalf("a denied reveal was not evidenced: %d", n)
	}
	_, paused, err := e.svc.RequestBreakGlass(ctx, cOperator, c.ConnectionID, c.Version, xicconn.BreakGlassInput{Action: "PAUSE", Reason: "suspected leak"})
	if err != nil || paused.Status != xicconn.StatusSuspended {
		t.Fatalf("%v", err)
	}
	if _, err = e.svc.Operable(ctx, cTenantA, c.ConnectionID, "payment-submit"); err == nil {
		t.Fatal("a paused connection was operable")
	}
	req, _, err := e.svc.RequestBreakGlass(ctx, cOperator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "RESUME", Reason: "contained", DurationSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.DecideBreakGlass(ctx, cApprover, c.ConnectionID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.Get(ctx, c.ConnectionID)
	if cur.Status != xicconn.StatusValidating {
		t.Fatalf("break-glass resumed to %s", cur.Status)
	}
	e.advance(time.Minute)
}

// Every event type XIC-02 can emit is produced by a real flow and accepted by the outbox as a valid envelope.
func TestXICConn_EveryEventTypeIsEmitted(t *testing.T) {
	e := newConnEnv(t)
	v := e.published(t, "bank-x", "1.0.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "oauth" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
	A := cTenantA
	ctx := tenantCtx(A)
	c := e.create(t, A, xicconn.EnvProduction, v, "acc")
	res, err := e.svc.Authorize(ctx, cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "OAUTH_AUTH_CODE", RedirectURI: "https://app.zoiko.example/oauth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(60 * 24 * time.Hour)
	cur := res.Connection
	if cur, err = e.svc.Bind(ctx, cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: cRefOAuth,
		GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp, State: res.State}); err != nil {
		t.Fatal(err)
	}
	if cur, err = e.svc.Bind(ctx, cCreator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: cRefHMAC, ConsentActor: "x"}); err != nil {
		t.Fatal(err)
	}
	// a failed validation, then a passing one
	e.advance(61 * 24 * time.Hour)
	if _, err = e.svc.Validate(ctx, cOperator, c.ConnectionID, cur.Version, ""); err == nil {
		t.Fatal("expected a failed validation")
	}
	e.advance(-61 * 24 * time.Hour)
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	cur = e.validatedC(t, A, cur)
	e.probe(t, A, cur, "acc", v)
	e.certify(t, A, cur)
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	if cur, err = e.svc.Activate(ctx, cOperator, c.ConnectionID, cur.Version); err != nil {
		t.Fatal(err)
	}
	// scopes, rotation, expiry
	if cur, err = e.svc.ReportScopes(ctx, cOperator, c.ConnectionID, cur.Version, "oauth", []string{"payments.write", "payments.read", "x.admin"}); err != nil || cur.Status != xicconn.StatusSuspended {
		t.Fatalf("%v", err)
	}
	if cur, err = e.svc.Resume(ctx, cOperator, c.ConnectionID, cur.Version); err != nil {
		t.Fatal(err)
	}
	if cur, err = e.svc.ReportScopes(ctx, cOperator, c.ConnectionID, cur.Version, "oauth", []string{"payments.write", "payments.read"}); err != nil {
		t.Fatal(err)
	}
	cur = e.validatedC(t, A, cur)
	e.probe(t, A, cur, "acc", v)
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	if cur, err = e.svc.Activate(ctx, cOperator, c.ConnectionID, cur.Version); err != nil {
		t.Fatal(err)
	}
	exp2 := time.Now().Add(20 * 24 * time.Hour)
	if cur, err = e.svc.Rotate(ctx, cOperator, c.ConnectionID, cur.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: cRefOAuth3, ExpiresAt: &exp2}); err != nil {
		t.Fatal(err)
	}
	e.advance(10 * 24 * time.Hour) // inside the warning window
	if _, err = e.svc.Evaluate(ctx, cOperator, c.ConnectionID); err != nil {
		t.Fatal(err)
	}
	e.advance(11 * 24 * time.Hour) // expired
	if _, err = e.svc.Evaluate(ctx, cOperator, c.ConnectionID); err != nil {
		t.Fatal(err)
	}
	// migration, break-glass, security signals
	v2 := e.published(t, "bank-x", "1.1.0", func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "oauth" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"OAUTH2_AUTH_CODE","audience":"https://api.bank-x.com","scopes_required":["payments.write","payments.read"],"credential_class":"PROVIDER_CREDENTIAL"}`)
			}
		}
	})
	req, err := e.svc.RequestMigration(ctx, cCreator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.DecideMigration(ctx, cApprover, c.ConnectionID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	if cur.Status == xicconn.StatusActive || cur.Status == xicconn.StatusDegraded {
		if _, err = e.svc.Suspend(ctx, cOperator, c.ConnectionID, cur.Version, "INVESTIGATION"); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ = e.repo.Get(ctx, c.ConnectionID)
	bg, _, err := e.svc.RequestBreakGlass(ctx, cOperator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "RESUME", Reason: "ok", DurationSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	_ = bg
	_, _, _ = e.svc.RequestBreakGlass(ctx, cOperator, c.ConnectionID, 0, xicconn.BreakGlassInput{Action: "REVEAL", Reason: "x"})
	_, _ = e.svc.Operable(tenantCtx(cTenantB), cTenantB, c.ConnectionID, "payment-submit")
	// a cross-tenant credential attempt
	other := e.create(t, cTenantB, xicconn.EnvSandbox, v2, "acc-b")
	ores, _ := e.svc.Authorize(tenantCtx(cTenantB), cCreator, other.ConnectionID, other.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	exp3 := time.Now().Add(e.nowAdd + 24*time.Hour)
	if _, berr := e.svc.Bind(tenantCtx(cTenantB), cCreator, other.ConnectionID, ores.Connection.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: cRefHMAC, ConsentActor: "x", ExpiresAt: &exp3}); !errors.Is(berr, xicconn.ErrCrossTenantCredential) {
		t.Fatalf("cross-tenant bind: %v", berr)
	}
	if _, err = e.svc.Revoke(ctx, cOperator, c.ConnectionID, e.mustVersion(t, A, c.ConnectionID), "done"); err != nil {
		t.Fatal(err)
	}

	rows, err := e.admin.Query(context.Background(), `SELECT DISTINCT event_type FROM eventing_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		have[s] = true
	}
	for _, want := range xicconn.EventTypes {
		if !have["com.zoikosuite."+want] {
			t.Errorf("event %s was never emitted by a real flow", want)
		}
	}
}

func (e *connEnv) mustVersion(t *testing.T, tn, id string) int64 {
	t.Helper()
	c, err := e.repo.Get(tenantCtx(tn), id)
	if err != nil {
		t.Fatal(err)
	}
	return c.Version
}

// execSeq runs several argument-free statements in one transaction under a tenant scope.
func (e *connEnv) execSeq(tenantID string, stmts ...string) error {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
