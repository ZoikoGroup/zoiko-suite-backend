package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
)

// These run the XIC-01 registry against REAL Postgres as an ordinary role (NOSUPERUSER, NOBYPASSRLS): the SQL,
// the guard triggers and constraints the database holds by itself, row-level security, the outbox, and concurrency.

type regEnv struct {
	admin *pgxpool.Pool
	app   *pgxpool.Pool
	svc   *registry.Service
}

var (
	rAuthor   = registry.Actor{PrincipalID: "author", CorrelationID: "corr-r1"}
	rSecurity = registry.Actor{PrincipalID: "security-reviewer"}
	rPrivacy  = registry.Actor{PrincipalID: "privacy-reviewer"}
	rQA       = registry.Actor{PrincipalID: "qa-lead"}
	rDomain   = registry.Actor{PrincipalID: "domain-owner"}
	rRelease  = registry.Actor{PrincipalID: "release-manager"}
)

func newRegEnv(t *testing.T) *regEnv {
	t.Helper()
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	return &regEnv{admin: admin, app: app, svc: &registry.Service{Repo: store.NewRegistryRepo(store.NewPgStore(app, store.WithEventRegion("uk")))}}
}

func platform() context.Context {
	return middleware.WithTenant(context.Background(), registry.PlatformScopeID)
}
func tenant(id string) context.Context { return middleware.WithTenant(context.Background(), id) }

func (e *regEnv) connector(t *testing.T, id string) *registry.Connector {
	t.Helper()
	c, _, err := e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor,
		registry.Connector{ConnectorID: id, Family: "BANK_FEED", ProviderClass: "Bank X", Owner: "integration-engineering"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *regEnv) draft(t *testing.T, connectorID, semver string, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	t.Helper()
	in := registrytest.Input(semver)
	in.EffectiveFrom = time.Now().Add(24 * time.Hour).UTC()
	if mutate != nil {
		mutate(&in)
	}
	v, _, err := e.svc.CreateVersion(platform(), registry.PlatformScopeID, rAuthor, connectorID, in, "")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *regEnv) validated(t *testing.T, v *registry.ContractVersion) *registry.ContractVersion {
	t.Helper()
	res, err := e.svc.Validate(platform(), registry.PlatformScopeID, rAuthor, v.ContractVersionID, v.RowVersion)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return res.Version
}

func (e *regEnv) submitted(t *testing.T, v *registry.ContractVersion) *registry.ContractVersion {
	t.Helper()
	v = e.validated(t, v)
	out, err := e.svc.SubmitReview(platform(), registry.PlatformScopeID, rAuthor, v.ContractVersionID, v.RowVersion)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *regEnv) attestAll(t *testing.T, v *registry.ContractVersion) {
	t.Helper()
	for _, a := range []struct {
		who    registry.Actor
		gate   string
		detail json.RawMessage
	}{{rSecurity, registry.GateSecurity, nil}, {rPrivacy, registry.GatePrivacy, nil}, {rQA, registry.GateNegativeTests, registrytest.PassedFor(v.Content)},
		{rDomain, registry.GateDomainAcceptance, nil}, {rRelease, registry.GateRollback, nil}} {
		if _, err := e.svc.Attest(platform(), registry.PlatformScopeID, a.who, v.ContractVersionID,
			registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-" + a.gate, Detail: a.detail}); err != nil {
			t.Fatalf("attest %s: %v", a.gate, err)
		}
	}
}

func (e *regEnv) reviewed(t *testing.T, v *registry.ContractVersion) *registry.ContractVersion {
	t.Helper()
	v = e.submitted(t, v)
	e.attestAll(t, v)
	cur, err := store.NewRegistryRepo(store.NewPgStore(e.app)).GetVersion(platform(), v.ContractVersionID)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

func (e *regEnv) publish(t *testing.T, v *registry.ContractVersion) *registry.ContractVersion {
	t.Helper()
	out, err := e.svc.Publish(platform(), registry.PlatformScopeID, rRelease, v.ContractVersionID, v.RowVersion)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return out
}

// execAs runs raw SQL as the application role under a tenant scope, in one transaction: how a caller (or a bug)
// that bypasses the service would reach the tables.
func (e *regEnv) execAs(tenantID, sql string, args ...any) error {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *regEnv) countAs(tenantID, sql string, args ...any) int {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		panic(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		panic(err)
	}
	var n int
	if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		panic(err)
	}
	return n
}

func wantDBError(t *testing.T, what string, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the database accepted it", what)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("%s: wrong refusal: %v (want %q)", what, err, contains)
	}
}

// wantRefused: the statement either raises, or (under row-level security with no policy for the verb) touches no row.
// Either way nothing changed, which is what the test is about; the count query proves it.
func (e *regEnv) wantRefused(t *testing.T, what, tenantID, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err == nil && tag.RowsAffected() != 0 {
		t.Fatalf("%s: the database changed %d row(s)", what, tag.RowsAffected())
	}
}

// ---- the schema --------------------------------------------------------------------------

func TestXIC_MigrationIsRerunnable(t *testing.T) {
	e := newRegEnv(t)
	dir, names := migrationFiles(t)
	var f string
	for _, n := range names {
		if strings.HasPrefix(n, "004") {
			f = n
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, f))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(context.Background(), string(b)); err != nil {
		t.Fatalf("004 applied a second time: %v", err)
	}
}

// ---- the whole lifecycle, through the database -----------------------------------------------

func TestXIC_FullLifecycleAndEvidence(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	v := e.draft(t, "bank-x", "1.0.0", nil)
	if len(v.MappingVersions) != 1 {
		t.Fatalf("%d mappings", len(v.MappingVersions))
	}
	pub := e.publish(t, e.reviewed(t, v))
	if pub.Status != registry.VersionPublished || pub.PublishedBy != "release-manager" || pub.ContentHash == "" {
		t.Fatalf("%+v", pub)
	}
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	got, err := repo.GetVersion(platform(), v.ContractVersionID)
	if err != nil || got.Status != registry.VersionPublished || len(got.MappingVersions) != 1 || got.MappingVersions[0].Status != registry.MappingPublished {
		t.Fatalf("%v %+v", err, got)
	}
	if got.CompatReport == nil || got.CompatClass != registry.ClassNone {
		t.Fatalf("compat not persisted: %+v", got)
	}
	// the content round-trips exactly (the hash is of this content)
	h, _ := registry.ContentHash(got)
	if h != got.ContentHash {
		t.Fatalf("stored content hashes to %s, stored hash %s", h, got.ContentHash)
	}
	trs, _ := repo.ListTransitions(platform(), v.ContractVersionID)
	var path []string
	for _, tr := range trs {
		path = append(path, tr.From+">"+tr.To)
	}
	if strings.Join(path, ",") != "DRAFT>VALIDATED,VALIDATED>REVIEW,REVIEW>PUBLISHED" {
		t.Fatalf("%v", path)
	}
	gates, _ := repo.ListGates(platform(), v.ContractVersionID)
	if len(gates) != 6 {
		t.Fatalf("%d gate attestations", len(gates))
	}
	runs, _ := repo.ListValidationRuns(platform(), v.ContractVersionID)
	if len(runs) != 1 || runs[0].Outcome != registry.GatePassed || runs[0].Report == nil {
		t.Fatalf("%+v", runs)
	}
	conn, _ := repo.GetConnector(platform(), "bank-x")
	if conn.Lifecycle != registry.ConnectorPublished {
		t.Fatalf("connector %s", conn.Lifecycle)
	}

	// every change wrote its outbox event in the same transaction, as a valid envelope in the integration domain
	rows, err := e.admin.Query(context.Background(), `SELECT event_type, tenant_id, region, payload FROM eventing_outbox ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	types := map[string]int{}
	for rows.Next() {
		var typ, tn, region string
		var payload []byte
		if err := rows.Scan(&typ, &tn, &region, &payload); err != nil {
			t.Fatal(err)
		}
		types[typ]++
		if tn != registry.PlatformScopeID || region != "uk" {
			t.Fatalf("event scoped to %s/%s", tn, region)
		}
		if typ == "com.zoikosuite.integration.contract.published" {
			var env struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(payload, &env); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"connector_id", "contract_version_id", "capabilities", "effective_from", "owner"} {
				if _, ok := env.Data[k]; !ok {
					t.Fatalf("published event lacks %s: %s", k, payload)
				}
			}
		}
	}
	for _, want := range []string{"com.zoikosuite.integration.connector.created", "com.zoikosuite.integration.contract.draft-created",
		"com.zoikosuite.integration.contract.validated", "com.zoikosuite.integration.contract.submitted",
		"com.zoikosuite.integration.contract.gate-attested", "com.zoikosuite.integration.contract.published"} {
		if types[want] == 0 {
			t.Errorf("no %s event; have %v", want, types)
		}
	}
	if types["com.zoikosuite.integration.contract.gate-attested"] != 5 {
		t.Errorf("%d gate events, want 5", types["com.zoikosuite.integration.contract.gate-attested"])
	}
}

func TestXIC_FailedValidationIsPersistedAndDoesNotAdvance(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	v := e.draft(t, "bank-x", "1.0.0", func(in *registry.VersionInput) { in.Content.Operations[1].MaxPayloadBytes = 0 })
	res, err := e.svc.Validate(platform(), registry.PlatformScopeID, rAuthor, v.ContractVersionID, v.RowVersion)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || res.Version.Status != registry.VersionDraft {
		t.Fatalf("%v", err)
	}
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	runs, _ := repo.ListValidationRuns(platform(), v.ContractVersionID)
	if len(runs) != 1 || runs[0].Outcome != registry.GateFailed || len(runs[0].Issues) == 0 {
		t.Fatalf("%+v", runs)
	}
	cur, _ := repo.GetVersion(platform(), v.ContractVersionID)
	if cur.RowVersion != v.RowVersion || cur.Status != registry.VersionDraft {
		t.Fatalf("a failed validation moved the row: %+v", cur)
	}
}

// ---- visibility and the platform scope (RLS) -----------------------------------------------

func TestXIC_TenantsSeeOnlyWhatIsPublished(t *testing.T) {
	e := newRegEnv(t)
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	e.connector(t, "bank-x")
	v := e.draft(t, "bank-x", "1.0.0", nil)
	tn := tenant("tenant-a")
	if _, err := repo.GetConnector(tn, "bank-x"); !errors.Is(err, registry.ErrConnectorNotFound) {
		t.Fatalf("tenant read an unpublished connector: %v", err)
	}
	if _, err := repo.GetVersion(tn, v.ContractVersionID); !errors.Is(err, registry.ErrVersionNotFound) {
		t.Fatalf("tenant read a draft: %v", err)
	}
	if _, err := repo.GetMapping(tn, v.MappingVersions[0].MappingVersionID); !errors.Is(err, registry.ErrMappingNotFound) {
		t.Fatalf("tenant read a draft mapping: %v", err)
	}
	if _, err := repo.ListGates(tn, v.ContractVersionID); !errors.Is(err, registry.ErrVersionNotFound) {
		t.Fatalf("tenant listed a draft's gates: %v", err)
	}
	if n := e.countAs("tenant-a", `SELECT count(*) FROM xic_connectors`); n != 0 {
		t.Fatalf("tenant counts %d unpublished connectors", n)
	}
	// through review the version is still platform-only
	rv := e.reviewed(t, v)
	if n := e.countAs("tenant-a", `SELECT count(*) FROM xic_contract_versions`); n != 0 {
		t.Fatalf("tenant sees %d versions in review", n)
	}
	if n := e.countAs("tenant-a", `SELECT count(*) FROM xic_contract_gates`); n != 0 {
		t.Fatalf("tenant sees %d gates of a version in review", n)
	}
	pub := e.publish(t, rv)
	for _, who := range []string{"tenant-a", "tenant-b"} {
		got, err := repo.GetVersion(tenant(who), pub.ContractVersionID)
		if err != nil || got.Status != registry.VersionPublished {
			t.Fatalf("%s cannot read the published contract: %v", who, err)
		}
		if m, err := repo.GetMapping(tenant(who), pub.MappingVersions[0].MappingVersionID); err != nil || m.Status != registry.MappingPublished {
			t.Fatalf("%s cannot read the published mapping: %v", who, err)
		}
		if n := e.countAs(who, `SELECT count(*) FROM xic_contract_transitions`); n != 3 {
			t.Fatalf("%s sees %d transitions of a published contract", who, n)
		}
	}
	// the published contract's content is the same for everyone: one platform truth
	a, _ := repo.GetVersion(tenant("tenant-a"), pub.ContractVersionID)
	b, _ := repo.GetVersion(tenant("tenant-b"), pub.ContractVersionID)
	if a.ContentHash != b.ContentHash {
		t.Fatal("tenants disagree about a published contract")
	}
}

func TestXIC_TenantCannotWriteAnythingEvenByRawSQL(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	pub := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.0.0", nil)))
	tn := "tenant-a"
	for name, sql := range map[string]string{
		"insert connector": `INSERT INTO xic_connectors (connector_id, family, provider_class, owner, created_by) VALUES ('evil-conn','API','x','y','evil')`,
		"update connector": `UPDATE xic_connectors SET owner='evil', version=version+1 WHERE connector_id='bank-x'`,
		"insert version": `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, effective_from, content, created_by)
			VALUES (gen_random_uuid(), 'bank-x', '9.9.9', now(), '{}', 'evil')`,
		"update version": `UPDATE xic_contract_versions SET blocked_reason='x', row_version=row_version+1`,
		"insert gate":    `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(), '` + pub.ContractVersionID + `', 'SECURITY', 'PASSED', 'x', 'evil')`,
		"idempotency":    `INSERT INTO xic_idempotency (scope, idem_key, request_hash, resource_id) VALUES ('connector','k','h','r')`,
	} {
		err := e.execAs(tn, sql)
		// either refused outright, or it matched no row (UPDATE under RLS): in both cases nothing changed
		if err == nil && !strings.HasPrefix(name, "update") {
			t.Errorf("%s: a tenant wrote registry content", name)
		}
	}
	var owner string
	_ = e.admin.QueryRow(context.Background(), `SELECT owner FROM xic_connectors WHERE connector_id='bank-x'`).Scan(&owner)
	if owner != "integration-engineering" {
		t.Fatalf("a tenant's UPDATE changed the connector: %s", owner)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_connectors`); n != 1 {
		t.Fatalf("%d connectors", n)
	}
	// and through the service/repo
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	if _, _, err := e.svc.CreateConnector(tenant(tn), tn, rAuthor, registry.Connector{ConnectorID: "evil-conn", Family: "API", ProviderClass: "x", Owner: "y"}, ""); !errors.Is(err, registry.ErrNotPlatformScope) {
		t.Fatalf("%v", err)
	}
	if _, err := repo.MutateVersion(tenant(tn), pub.ContractVersionID, func(*registry.VersionTx) error { return nil }); !errors.Is(err, registry.ErrNotPlatformScope) {
		t.Fatalf("%v", err)
	}
	// and an empty tenant context sees and writes nothing
	e.connectorOnly(t, "draft-only")
	if n := e.countAs("", `SELECT count(*) FROM xic_connectors WHERE lifecycle IN ('DRAFT','REVIEW')`); n != 0 {
		t.Fatalf("an empty tenant scope sees %d unpublished connectors", n)
	}
}

// ---- immutability and the state machine, enforced by the database itself ------------------------

func TestXIC_DatabaseHoldsTheRulesTheServiceChecks(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	draft := e.draft(t, "bank-x", "1.0.0", nil)
	validated := e.validated(t, e.draft(t, "bank-x", "1.1.0", nil))
	reviewed := e.reviewed(t, e.draft(t, "bank-x", "1.2.0", nil))
	pub := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.3.0", nil)))
	bump := `, row_version = row_version + 1`

	t.Run("a draft is editable", func(t *testing.T) {
		if err := e.execAs(P, `UPDATE xic_contract_versions SET rollback_ref='NEW-REF-123'`+bump+` WHERE contract_version_id=$1::uuid`, draft.ContractVersionID); err != nil {
			t.Fatalf("a draft could not be edited: %v", err)
		}
	})
	for name, id := range map[string]string{"validated": validated.ContractVersionID, "review": reviewed.ContractVersionID, "published": pub.ContractVersionID} {
		id := id
		t.Run(name+" definition is immutable", func(t *testing.T) {
			for col, set := range map[string]string{
				"semver":          `semver='9.0.0'`,
				"effective_from":  `effective_from=effective_from + interval '1 day'`,
				"content":         `content='{"operations":[]}'::jsonb`,
				"rollback_ref":    `rollback_ref='SOMETHING-ELSE'`,
				"migration_plan":  `migration_plan_ref='MIG-1 something'`,
				"replay":          `replay_analysis_ref='REPLAY-1 something'`,
				"supported_until": `supported_until=effective_from + interval '400 days'`,
			} {
				wantDBError(t, name+" "+col, e.execAs(P, `UPDATE xic_contract_versions SET `+set+bump+` WHERE contract_version_id=$1::uuid`, id), "immutable")
			}
			wantDBError(t, name+" content_hash", e.execAs(P, `UPDATE xic_contract_versions SET content_hash=repeat('a',64)`+bump+` WHERE contract_version_id=$1::uuid`, id), "fixed")
			wantDBError(t, name+" compat", e.execAs(P, `UPDATE xic_contract_versions SET compat_class='BREAKING'`+bump+` WHERE contract_version_id=$1::uuid`, id), "fixed")
			e.wantRefused(t, name+" delete", P, `DELETE FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, id)
		})
	}
	t.Run("identity is immutable", func(t *testing.T) {
		wantDBError(t, "connector_id", e.execAs(P, `UPDATE xic_contract_versions SET connector_id='other-conn'`+bump+` WHERE contract_version_id=$1::uuid`, draft.ContractVersionID), "")
		wantDBError(t, "created_by", e.execAs(P, `UPDATE xic_contract_versions SET created_by='someone'`+bump+` WHERE contract_version_id=$1::uuid`, draft.ContractVersionID), "immutable")
	})
	t.Run("published facts are fixed", func(t *testing.T) {
		wantDBError(t, "published_by", e.execAs(P, `UPDATE xic_contract_versions SET published_by='someone-else'`+bump+` WHERE contract_version_id=$1::uuid`, pub.ContractVersionID), "")
	})
	t.Run("row_version advances by exactly one", func(t *testing.T) {
		wantDBError(t, "no bump", e.execAs(P, `UPDATE xic_contract_versions SET rollback_ref='NEW-REF-124' WHERE contract_version_id=$1::uuid`, draft.ContractVersionID), "row_version")
		wantDBError(t, "jump", e.execAs(P, `UPDATE xic_contract_versions SET rollback_ref='NEW-REF-124', row_version=row_version+2 WHERE contract_version_id=$1::uuid`, draft.ContractVersionID), "row_version")
	})
	t.Run("status only moves along the graph", func(t *testing.T) {
		for _, c := range []struct{ name, id, to string }{
			{"draft to published", draft.ContractVersionID, "PUBLISHED"},
			{"draft to review", draft.ContractVersionID, "REVIEW"},
			{"draft to blocked", draft.ContractVersionID, "BLOCKED"},
			{"validated to published", validated.ContractVersionID, "PUBLISHED"},
			{"validated to draft", validated.ContractVersionID, "DRAFT"},
			{"review to draft", reviewed.ContractVersionID, "DRAFT"},
			{"review to validated", reviewed.ContractVersionID, "VALIDATED"},
			{"published to draft", pub.ContractVersionID, "DRAFT"},
			{"published to review", pub.ContractVersionID, "REVIEW"},
		} {
			err := e.execAs(P, `UPDATE xic_contract_versions SET status=$2, blocked_reason='x'`+bump+` WHERE contract_version_id=$1::uuid`, c.id, c.to)
			wantDBError(t, c.name, err, "")
		}
	})
	t.Run("a connector identity is never deleted, reused or re-keyed", func(t *testing.T) {
		e.wantRefused(t, "delete", P, `DELETE FROM xic_connectors WHERE connector_id='bank-x'`)
		wantDBError(t, "rename", e.execAs(P, `UPDATE xic_connectors SET connector_id='bank-y', version=version+1 WHERE connector_id='bank-x'`), "immutable")
		wantDBError(t, "family", e.execAs(P, `UPDATE xic_connectors SET family='HRIS', version=version+1 WHERE connector_id='bank-x'`), "immutable")
		wantDBError(t, "duplicate", e.execAs(P, `INSERT INTO xic_connectors (connector_id, family, provider_class, owner, created_by) VALUES ('bank-x','API','x','y','z')`), "")
		wantDBError(t, "lifecycle skip", e.execAs(P, `UPDATE xic_connectors SET lifecycle='DRAFT', version=version+1 WHERE connector_id='bank-x'`), "lifecycle")
		wantDBError(t, "no version bump", e.execAs(P, `UPDATE xic_connectors SET owner='someone' WHERE connector_id='bank-x'`), "version")
		wantDBError(t, "deprecate without a date", e.execAs(P, `UPDATE xic_connectors SET lifecycle='DEPRECATED', version=version+1 WHERE connector_id='bank-x'`), "")
	})
	t.Run("bad identifiers and enumerations are refused", func(t *testing.T) {
		wantDBError(t, "bad id", e.execAs(P, `INSERT INTO xic_connectors (connector_id, family, provider_class, owner, created_by) VALUES ('Bad_Id','API','x','y','z')`), "")
		wantDBError(t, "bad family", e.execAs(P, `INSERT INTO xic_connectors (connector_id, family, provider_class, owner, created_by) VALUES ('good-id','MAGIC','x','y','z')`), "")
		wantDBError(t, "bad semver", e.execAs(P, `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, effective_from, content, created_by)
			VALUES (gen_random_uuid(),'bank-x','1.0','2030-01-01','{}','z')`), "")
		wantDBError(t, "content not an object", e.execAs(P, `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, effective_from, content, created_by)
			VALUES (gen_random_uuid(),'bank-x','5.0.0','2030-01-01','[]','z')`), "")
		wantDBError(t, "duplicate semver", e.execAs(P, `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, effective_from, content, created_by)
			VALUES (gen_random_uuid(),'bank-x','1.0.0','2030-01-01','{}','z')`), "")
	})
}

func TestXIC_PublicationIsGuardedByTheDatabase(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	bump := `, row_version = row_version + 1`

	// a version in REVIEW with NO attestations but the system's
	v := e.submitted(t, e.draft(t, "bank-x", "1.0.0", nil))
	publishSQL := `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='release-manager', published_at=now()` + bump + ` WHERE contract_version_id=$1::uuid`
	wantDBError(t, "publish with no human gates", e.execAs(P, publishSQL, v.ContractVersionID), "six gates")

	// four of five human gates
	for _, a := range []struct {
		who  string
		gate string
		det  string
	}{{"security-reviewer", "SECURITY", ``}, {"privacy-reviewer", "PRIVACY", ``}, {"qa-lead", "NEGATIVE_TESTS", ``}, {"domain-owner", "DOMAIN_ACCEPTANCE", ``}} {
		if err := e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(),$1::uuid,$2,'PASSED','REVIEW-1',$3)`,
			v.ContractVersionID, a.gate, a.who); err != nil {
			t.Fatal(err)
		}
	}
	wantDBError(t, "publish with one gate missing", e.execAs(P, publishSQL, v.ContractVersionID), "six gates")
	// the missing gate arrives FAILED: the latest attestation is what counts
	if err := e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(),$1::uuid,'ROLLBACK','FAILED','REVIEW-1','release-manager')`, v.ContractVersionID); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "publish with a failed gate", e.execAs(P, publishSQL, v.ContractVersionID), "has not passed")
	if err := e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(),$1::uuid,'ROLLBACK','PASSED','REVIEW-2','release-manager')`, v.ContractVersionID); err != nil {
		t.Fatal(err)
	}
	// published by the author is refused by the constraint
	wantDBError(t, "publisher is the author", e.execAs(P, `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='author', published_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, v.ContractVersionID), "")
	// not published at all by nobody
	wantDBError(t, "no publisher", e.execAs(P, `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='', published_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, v.ContractVersionID), "")
	// and with everything in order it goes through
	if err := e.execAs(P, publishSQL, v.ContractVersionID); err != nil {
		t.Fatalf("a properly gated publication was refused: %v", err)
	}
	// its mapping rows stay DRAFT when the SQL path forgot them — the service path flips them; a mapping can never
	// be PUBLISHED while the contract is not (status only moves forward from DRAFT)
}

func TestXIC_ValidationMustCoverThePublishedContent(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	bump := `, row_version = row_version + 1`
	d := e.draft(t, "bank-x", "1.0.0", nil)
	id := d.ContractVersionID
	// the registry's own gate is recorded for hash A, then the version is moved to VALIDATED carrying hash B
	hashA, hashB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, detail, attested_by)
		VALUES (gen_random_uuid(),$1::uuid,'SCHEMA_COMPAT','PASSED','validation-run:x', jsonb_build_object('content_hash',$2::text), 'system:xic-registry')`, id, hashA); err != nil {
		t.Fatal(err)
	}
	if err := e.execAs(P, `UPDATE xic_contract_versions SET status='VALIDATED', content_hash=$2, compat_class='NONE', validated_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, id, hashB); err != nil {
		t.Fatal(err)
	}
	if err := e.execAs(P, `UPDATE xic_contract_versions SET status='REVIEW'`+bump+` WHERE contract_version_id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	for _, g := range []struct{ who, gate string }{{"s", "SECURITY"}, {"p", "PRIVACY"}, {"q", "NEGATIVE_TESTS"}, {"d", "DOMAIN_ACCEPTANCE"}, {"r", "ROLLBACK"}} {
		if err := e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(),$1::uuid,$2,'PASSED','REVIEW-1',$3)`, id, g.gate, g.who); err != nil {
			t.Fatal(err)
		}
	}
	wantDBError(t, "publish content the validation did not cover",
		e.execAs(P, `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='release', published_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, id), "different content")
}

func TestXIC_GateAttestationRules(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	ins := `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, detail, attested_by) VALUES (gen_random_uuid(),$1::uuid,$2,$3,'REVIEW-1',$4::jsonb,$5)`
	draft := e.draft(t, "bank-x", "1.0.0", nil)
	review := e.submitted(t, e.draft(t, "bank-x", "1.1.0", nil))

	wantDBError(t, "human gate while a draft", e.execAs(P, ins, draft.ContractVersionID, "SECURITY", "PASSED", nil, "security-reviewer"), "during review")
	wantDBError(t, "author attests", e.execAs(P, ins, review.ContractVersionID, "SECURITY", "PASSED", nil, "author"), "other than the author")
	wantDBError(t, "system attests a human gate", e.execAs(P, ins, review.ContractVersionID, "SECURITY", "PASSED", nil, "system:xic-registry"), "system")
	wantDBError(t, "human attests the system gate", e.execAs(P, ins, review.ContractVersionID, "SCHEMA_COMPAT", "PASSED", `{"content_hash":"x"}`, "security-reviewer"), "registry itself")
	wantDBError(t, "system gate after validation", e.execAs(P, ins, review.ContractVersionID, "SCHEMA_COMPAT", "PASSED", `{"content_hash":"x"}`, "system:xic-registry"), "draft")
	wantDBError(t, "system gate without a hash", e.execAs(P, ins, draft.ContractVersionID, "SCHEMA_COMPAT", "PASSED", `{}`, "system:xic-registry"), "content hash")
	wantDBError(t, "unknown gate", e.execAs(P, ins, review.ContractVersionID, "MAGIC", "PASSED", nil, "x"), "")
	wantDBError(t, "unknown outcome", e.execAs(P, ins, review.ContractVersionID, "SECURITY", "MAYBE", nil, "x"), "")
	wantDBError(t, "no evidence", e.execAs(P, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, attested_by) VALUES (gen_random_uuid(),$1::uuid,'SECURITY','PASSED','','x')`, review.ContractVersionID), "")
	if err := e.execAs(P, ins, review.ContractVersionID, "SECURITY", "PASSED", nil, "security-reviewer"); err != nil {
		t.Fatalf("a legitimate attestation was refused: %v", err)
	}
	// append-only
	e.wantRefused(t, "update gate", P, `UPDATE xic_contract_gates SET outcome='FAILED'`)
	e.wantRefused(t, "delete gate", P, `DELETE FROM xic_contract_gates`)
	e.wantRefused(t, "update run", P, `UPDATE xic_validation_runs SET outcome='PASSED'`)
	e.wantRefused(t, "delete run", P, `DELETE FROM xic_validation_runs`)
	e.wantRefused(t, "update transition", P, `UPDATE xic_contract_transitions SET actor='x'`)
	e.wantRefused(t, "delete transition", P, `DELETE FROM xic_contract_transitions`)
	e.wantRefused(t, "update idempotency", P, `UPDATE xic_idempotency SET request_hash='x'`)
	// a run that "passed" with issues is refused
	wantDBError(t, "passing run with issues", e.execAs(P, `INSERT INTO xic_validation_runs (run_id, contract_version_id, outcome, content_hash, issues, run_by)
		VALUES (gen_random_uuid(),$1::uuid,'PASSED',repeat('a',64),'[{"path":"x","rule":"y"}]','x')`, draft.ContractVersionID), "")
}

func TestXIC_OnlyOneVersionIsPublishedAndPublishedVersionsOnlyIncrease(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	bump := `, row_version = row_version + 1`
	// both are reviewed against an empty baseline; the higher one is published first
	low := e.reviewed(t, e.draft(t, "bank-x", "1.0.0", nil))
	v1 := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.1.0", nil)))
	// the lower version, fully gated, cannot be published afterwards: the service refuses it (the baseline moved)
	// and the database refuses it independently
	_, err := e.svc.Publish(platform(), registry.PlatformScopeID, rRelease, low.ContractVersionID, low.RowVersion)
	if !errors.Is(err, registry.ErrVersionOrder) && !errors.Is(err, registry.ErrBaselineMoved) {
		// the baseline differs (low was validated before v1 was published), which is also a refusal
		t.Fatalf("published a lower version: %v", err)
	}
	// directly, demoting the current one first so only the ordering rule is at stake
	if err := e.execAs(P, `UPDATE xic_contract_versions SET status='SUPERSEDED'`+bump+` WHERE contract_version_id=$1::uuid`, v1.ContractVersionID); err != nil {
		t.Fatal(err)
	}
	wantDBError(t, "lower semver published", e.execAs(P, `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='release', published_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, low.ContractVersionID), "greater than")
	// two published at once
	if err := e.execAs(P, `UPDATE xic_contract_versions SET status='RETIRED'`+bump+` WHERE contract_version_id=$1::uuid`, v1.ContractVersionID); err != nil {
		t.Fatal(err)
	}
	e2 := e.reviewed(t, e.draft(t, "bank-x", "1.5.0", nil))
	e.publish(t, e2)
	e3 := e.reviewed(t, e.draft(t, "bank-x", "1.6.0", nil))
	wantDBError(t, "second PUBLISHED", e.execAs(P, `UPDATE xic_contract_versions SET status='PUBLISHED', published_by='release', published_at=now()`+bump+` WHERE contract_version_id=$1::uuid`, e3.ContractVersionID), "")
}

func TestXIC_MappingVersionsFollowTheirContract(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	P := registry.PlatformScopeID
	d := e.draft(t, "bank-x", "1.0.0", nil)
	validated := e.validated(t, e.draft(t, "bank-x", "1.1.0", nil))
	pub := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.2.0", nil)))
	insert := func(id string) error {
		return e.execAs(P, `INSERT INTO xic_mapping_versions (mapping_version_id, contract_version_id, mapping_key, version, source_schema, canonical_contract_ref, spec, transform_hash)
			VALUES (gen_random_uuid(), $1::uuid, 'extra', 1, 'statement@1', 'x', '{}', repeat('a',64))`, id)
	}
	if err := insert(d.ContractVersionID); err != nil {
		t.Fatalf("a draft contract could not take a mapping: %v", err)
	}
	wantDBError(t, "mapping on a validated contract", insert(validated.ContractVersionID), "draft contract only")
	wantDBError(t, "mapping on a published contract", insert(pub.ContractVersionID), "draft contract only")
	mid := validated.MappingVersions[0].MappingVersionID
	for col, set := range map[string]string{"spec": `spec='{"x":1}'`, "hash": `transform_hash=repeat('b',64)`, "key": `mapping_key='other'`, "schema": `source_schema='x@1'`} {
		wantDBError(t, "edit mapping "+col, e.execAs(P, `UPDATE xic_mapping_versions SET `+set+` WHERE mapping_version_id=$1::uuid`, mid), "immutable")
	}
	wantDBError(t, "delete a validated contract's mapping", e.execAs(P, `DELETE FROM xic_mapping_versions WHERE mapping_version_id=$1::uuid`, mid), "never deleted")
	wantDBError(t, "delete a published mapping", e.execAs(P, `DELETE FROM xic_mapping_versions WHERE contract_version_id=$1::uuid`, pub.ContractVersionID), "never deleted")
	wantDBError(t, "published mapping to draft", e.execAs(P, `UPDATE xic_mapping_versions SET status='DRAFT' WHERE contract_version_id=$1::uuid`, pub.ContractVersionID), "status")
	wantDBError(t, "bad hash", e.execAs(P, `INSERT INTO xic_mapping_versions (mapping_version_id, contract_version_id, mapping_key, version, source_schema, canonical_contract_ref, spec, transform_hash)
		VALUES (gen_random_uuid(), $1::uuid, 'k', 1, 's@1', 'x', '{}', 'nothex')`, d.ContractVersionID), "")
	wantDBError(t, "duplicate mapping", e.execAs(P, `INSERT INTO xic_mapping_versions (mapping_version_id, contract_version_id, mapping_key, version, source_schema, canonical_contract_ref, spec, transform_hash)
		VALUES (gen_random_uuid(), $1::uuid, 'extra', 1, 's@1', 'x', '{}', repeat('a',64))`, d.ContractVersionID), "")
}

func TestXIC_SupersessionCarriesMappingsAndKeepsPinnedContractReadable(t *testing.T) {
	e := newRegEnv(t)
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	e.connector(t, "bank-x")
	v1 := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.0.0", nil)))
	v2 := e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.1.0", nil)))
	old, err := repo.GetVersion(tenant("tenant-a"), v1.ContractVersionID)
	if err != nil || old.Status != registry.VersionSuperseded || old.ContentHash != v1.ContentHash || old.MappingVersions[0].Status != registry.MappingSuperseded {
		t.Fatalf("%v %+v", err, old)
	}
	cur, _ := repo.Baseline(platform(), "bank-x")
	if cur == nil || cur.ContractVersionID != v2.ContractVersionID {
		t.Fatalf("baseline %+v", cur)
	}
	cp, err := e.svc.Compatibility(tenant("tenant-b"), "bank-x")
	if err != nil || cp.Current != v2.ContractVersionID || len(cp.Versions) != 2 || !cp.Versions[0].Supported {
		t.Fatalf("%v %+v", err, cp)
	}
	if cp.Versions[0].Semver != "1.0.0" || cp.Versions[1].Semver != "1.1.0" {
		t.Fatalf("not in semver order: %+v", cp.Versions)
	}
	// a published contract is in tenant view with its supersession event
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_contract_versions WHERE status='SUPERSEDED'`); n != 1 {
		t.Fatalf("%d superseded", n)
	}
}

func TestXIC_SemverOrderIsNumericNotLexical(t *testing.T) {
	e := newRegEnv(t)
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	e.connector(t, "bank-x")
	for _, s := range []string{"1.10.0", "1.2.0", "1.9.0", "2.0.0", "1.9.10"} {
		e.draft(t, "bank-x", s, nil)
	}
	vs, err := repo.ListVersions(platform(), "bank-x")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vs {
		got = append(got, v.Semver)
	}
	if strings.Join(got, " ") != "1.2.0 1.9.0 1.9.10 1.10.0 2.0.0" {
		t.Fatalf("order %v", got)
	}
}

// ---- idempotency and concurrency -----------------------------------------------------------

func TestXIC_Idempotency(t *testing.T) {
	e := newRegEnv(t)
	c := registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED", ProviderClass: "Bank X", Owner: "eng"}
	_, created, err := e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor, c, "key-1")
	if err != nil || !created {
		t.Fatalf("%v %v", err, created)
	}
	got, created, err := e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor, c, "key-1")
	if err != nil || created || got.ConnectorID != "bank-x" {
		t.Fatalf("replay: %v %v", err, created)
	}
	c2 := c
	c2.Owner = "other"
	if _, _, err = e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor, c2, "key-1"); !errors.Is(err, registry.ErrIdempotency) {
		t.Fatalf("%v", err)
	}
	if _, _, err = e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor, c, "key-2"); !errors.Is(err, registry.ErrConnectorExists) {
		t.Fatalf("%v", err)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connector.created'`); n != 1 {
		t.Fatalf("%d created events", n)
	}
	// a version: replay returns the original draft, the same id, one row, one event
	in := registrytest.Input("1.0.0")
	a, created, err := e.svc.CreateVersion(platform(), registry.PlatformScopeID, rAuthor, "bank-x", in, "vkey")
	if err != nil || !created {
		t.Fatal(err)
	}
	b, created, err := e.svc.CreateVersion(platform(), registry.PlatformScopeID, rAuthor, "bank-x", in, "vkey")
	if err != nil || created || b.ContractVersionID != a.ContractVersionID {
		t.Fatalf("%v %v", err, created)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_contract_versions`); n != 1 {
		t.Fatalf("%d versions", n)
	}
}

func TestXIC_ConcurrentIdempotentCreates(t *testing.T) {
	e := newRegEnv(t)
	c := registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED", ProviderClass: "Bank X", Owner: "eng"}
	const n = 16
	var created, replayed, other int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, was, err := e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor, c, "same-key")
			switch {
			case err == nil && was:
				atomic.AddInt32(&created, 1)
			case err == nil:
				atomic.AddInt32(&replayed, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if created != 1 || replayed != n-1 || other != 0 {
		t.Fatalf("created=%d replayed=%d other=%d", created, replayed, other)
	}
	if got := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.connector.created'`); got != 1 {
		t.Fatalf("%d events", got)
	}
}

func TestXIC_ConcurrentCreatesOfOneConnectorIDWithoutAKey(t *testing.T) {
	e := newRegEnv(t)
	const n = 12
	var won, lost int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := e.svc.CreateConnector(platform(), registry.PlatformScopeID, rAuthor,
				registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED", ProviderClass: "Bank X", Owner: fmt.Sprintf("owner-%d", i)}, "")
			switch {
			case err == nil:
				atomic.AddInt32(&won, 1)
			case errors.Is(err, registry.ErrConnectorExists):
				atomic.AddInt32(&lost, 1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || lost != n-1 {
		t.Fatalf("won=%d lost=%d", won, lost)
	}
}

func TestXIC_ConcurrentPublishersOfOneConnector(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.0.0", nil)))
	const n = 6
	cands := make([]*registry.ContractVersion, n)
	for i := range cands {
		cands[i] = e.reviewed(t, e.draft(t, "bank-x", fmt.Sprintf("1.%d.0", i+1), nil))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.Publish(platform(), registry.PlatformScopeID, rRelease, cands[i].ContractVersionID, cands[i].RowVersion)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, registry.ErrBaselineMoved):
		default:
			t.Errorf("unexpected loser error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d publishers won", won)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_contract_versions WHERE status='PUBLISHED'`); n != 1 {
		t.Fatalf("%d PUBLISHED at once", n)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_contract_versions WHERE status='SUPERSEDED'`); n != 1 {
		t.Fatalf("%d SUPERSEDED", n)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.contract.published'`); n != 2 {
		t.Fatalf("%d published events for two publications", n)
	}
}

func TestXIC_ConcurrentCommandsOnOneVersion(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	v := e.draft(t, "bank-x", "1.0.0", nil)
	const n = 8
	var ok, conflict int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Validate(platform(), registry.PlatformScopeID, rAuthor, v.ContractVersionID, v.RowVersion)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, registry.ErrVersionConflict) || errors.Is(err, registry.ErrNotDraft):
				atomic.AddInt32(&conflict, 1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflict != n-1 {
		t.Fatalf("ok=%d conflict=%d", ok, conflict)
	}
	if got := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_validation_runs`); got != 1 {
		t.Fatalf("%d validation runs", got)
	}
}

func TestXIC_FailedMutationWritesNothing(t *testing.T) {
	e := newRegEnv(t)
	e.connector(t, "bank-x")
	v := e.submitted(t, e.draft(t, "bank-x", "1.0.0", nil))
	before := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM eventing_outbox`)
	// the author attesting their own contract is refused by the service; nothing is left behind
	_, err := e.svc.Attest(platform(), registry.PlatformScopeID, rAuthor, v.ContractVersionID,
		registry.AttestInput{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1"})
	if !errors.Is(err, registry.ErrSelfAttest) {
		t.Fatalf("%v", err)
	}
	// a publication without gates is refused and leaves no event, no transition, no status change
	if _, err = e.svc.Publish(platform(), registry.PlatformScopeID, rRelease, v.ContractVersionID, v.RowVersion); !errors.Is(err, registry.ErrGatesIncomplete) {
		t.Fatalf("%v", err)
	}
	if after := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM eventing_outbox`); after != before {
		t.Fatalf("a refused command left %d events behind", after-before)
	}
	if n := e.countAs(registry.PlatformScopeID, `SELECT count(*) FROM xic_contract_gates WHERE gate_key<>'SCHEMA_COMPAT'`); n != 0 {
		t.Fatalf("%d human gates recorded by refused commands", n)
	}
}

// ---- foreign keys and tx hygiene ----------------------------------------------------------

func TestXIC_ChildRowsNeedARealParent(t *testing.T) {
	e := newRegEnv(t)
	P := registry.PlatformScopeID
	wantDBError(t, "version without connector", e.execAs(P, `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, effective_from, content, created_by)
		VALUES (gen_random_uuid(),'ghost-conn','1.0.0','2030-01-01','{}','z')`), "")
	if _, err := store.NewRegistryRepo(store.NewPgStore(e.app)).GetVersion(platform(), "not-a-uuid"); !errors.Is(err, registry.ErrVersionNotFound) {
		t.Fatalf("a malformed id must read as not found, not as a server error: %v", err)
	}
	var _ pgx.Tx
}

func (e *regEnv) connectorOnly(t *testing.T, id string) {
	t.Helper()
	e.connector(t, id)
}

// Every event type the registry can emit is a valid envelope that Postgres accepts, in the integration domain.
func TestXIC_EveryEventTypeIsEmittedAsAValidEnvelope(t *testing.T) {
	e := newRegEnv(t)
	repo := store.NewRegistryRepo(store.NewPgStore(e.app))
	e.connector(t, "bank-x")

	// draft-created, draft-updated, validation-failed, validated, submitted, gate-attested(+failed), blocked
	bad := e.draft(t, "bank-x", "0.9.0", func(in *registry.VersionInput) { in.Content.Operations[1].MaxPayloadBytes = 0 })
	in := registrytest.Input("0.9.0")
	in.EffectiveFrom = time.Now().Add(24 * time.Hour).UTC()
	fixed, err := e.svc.ReplaceDraft(platform(), registry.PlatformScopeID, rAuthor, bad.ContractVersionID, bad.RowVersion, in)
	if err != nil {
		t.Fatal(err)
	}
	_ = fixed
	if _, err := e.svc.Validate(platform(), registry.PlatformScopeID, rAuthor, bad.ContractVersionID, fixed.RowVersion); err != nil {
		t.Fatal(err)
	}
	wrongDraft := e.draft(t, "bank-x", "0.8.0", func(in *registry.VersionInput) { in.Content.Operations[1].RateLimitPerMinute = 0 })
	_, _ = e.svc.Validate(platform(), registry.PlatformScopeID, rAuthor, wrongDraft.ContractVersionID, wrongDraft.RowVersion) // validation-failed

	toBlock := e.submitted(t, e.draft(t, "bank-x", "0.7.0", nil))
	if _, err := e.svc.Attest(platform(), registry.PlatformScopeID, rSecurity, toBlock.ContractVersionID,
		registry.AttestInput{Gate: registry.GateSecurity, Outcome: registry.GateFailed, EvidenceRef: "REVIEW-9"}); err != nil {
		t.Fatal(err)
	}
	manual := e.validated(t, e.draft(t, "bank-x", "0.6.0", nil))
	if _, err := e.svc.Block(platform(), registry.PlatformScopeID, rSecurity, manual.ContractVersionID, manual.RowVersion, "provider withdrew the API"); err != nil {
		t.Fatal(err)
	}

	// published, superseded, deprecated
	e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.0.0", nil)))
	e.publish(t, e.reviewed(t, e.draft(t, "bank-x", "1.1.0", nil)))
	c, _ := repo.GetConnector(platform(), "bank-x")
	if _, err := e.svc.DeprecateConnector(platform(), registry.PlatformScopeID, rAuthor, "bank-x", c.Version, time.Now().Add(90*24*time.Hour), "provider sunset"); err != nil {
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
	for _, want := range registry.EventTypes {
		if !have["com.zoikosuite."+want] {
			t.Errorf("event %s was never emitted by a real flow; have %v", want, have)
		}
	}
	for typ := range have {
		if !strings.HasPrefix(typ, "com.zoikosuite.integration.") {
			t.Errorf("event %s is outside the integration domain", typ)
		}
	}
}
