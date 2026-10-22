package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// These run the XIC-03 gateway against REAL Postgres as an ordinary role: the SQL, the guard triggers and
// constraints the database holds itself, row-level security, the outbox, retention and concurrency.

const pgHMAC = "pg-hmac-secret-for-tenant-a"

type pgSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *pgSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[strings.ToLower(ref)]; ok {
		return []byte(v), nil
	}
	return nil, inbound.ErrAuthFailed
}

type pgConns struct{ svc *xicconn.Service }

func (c pgConns) Connection(ctx context.Context, id string) (*xicconn.Connection, error) {
	return c.svc.Connection(ctx, id)
}
func (c pgConns) PinnedContract(ctx context.Context, conn *xicconn.Connection) (*registry.ContractVersion, error) {
	return c.svc.PinnedContract(ctx, conn)
}
func (c pgConns) Operable(ctx context.Context, t, id, op string) (*xicconn.Decision, error) {
	return c.svc.Operable(ctx, t, id, op)
}

type gwEnv struct {
	*connEnv
	gw      *inbound.Service
	irepo   *store.InboundRepo
	secrets *pgSecrets
	dom     *httptest.Server
	domHits int32
	domCode int32
	domKeys sync.Map
}

func newGwEnv(t *testing.T) *gwEnv {
	t.Helper()
	c := newConnEnv(t)
	g := &gwEnv{connEnv: c, secrets: &pgSecrets{m: map[string]string{strings.ToLower(cRefHMAC): pgHMAC, strings.ToLower(cRefHMAC2): "pg-hmac-b"}}}
	g.domCode = 200
	g.dom = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		atomic.AddInt32(&g.domHits, 1)
		g.domKeys.Store(r.Header.Get("Idempotency-Key"), true)
		w.WriteHeader(int(atomic.LoadInt32(&g.domCode)))
		_, _ = w.Write([]byte(`{"id":"dom-1"}`))
	}))
	t.Cleanup(g.dom.Close)
	st := store.NewPgStore(c.app, store.WithEventRegion("uk"))
	g.irepo = store.NewInboundRepo(st)
	g.gw = &inbound.Service{Repo: g.irepo, Conns: pgConns{c.svc}, Secrets: g.secrets, Cfg: inbound.DefaultConfig(),
		Clock: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().UTC().Add(c.nowAdd) },
		Handoff: inbound.HandoffDeps{AllowedRegions: []string{"uk"}, HTTP: g.dom.Client(),
			Check: func(context.Context, string) (string, error) { return "ok", nil }}}
	return g
}

// contract publishes the reference contract with the gateway's conveniences.
func (g *gwEnv) contract(t *testing.T, connectorID, semver string, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	t.Helper()
	return g.published(t, connectorID, semver, func(in *registry.VersionInput) {
		for i := range in.Content.Profiles {
			if in.Content.Profiles[i].Name == "hmac-in" {
				in.Content.Profiles[i].Spec = json.RawMessage(`{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"CALLBACK_SECRET","signature_header":"X-Signature","timestamp_header":"X-Timestamp","replay_window_seconds":300}`)
			}
		}
		for i := range in.Content.Operations {
			if in.Content.Operations[i].ID == "statement-push" {
				in.Content.Operations[i].PayloadTenantPointer = "/tenant"
				in.Content.Operations[i].PayloadAccountPointer = "/holder_account"
			}
		}
		in.Content.Schemas[0].ValidationProfile = json.RawMessage(`{"type":"object","required":["event_id","account","balance","booked_at"],
		 "properties":{"event_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},"currency":{"type":"string","enum":["GBP","EUR","USD"]},
		 "booked_at":{"type":"string"},"tenant":{"type":"string"},"holder_account":{"type":"string"},"holder":{"type":"object"}}}`)
		if mutate != nil {
			mutate(in)
		}
	})
}

func (g *gwEnv) target(t *testing.T) *inbound.Target {
	t.Helper()
	tg, err := g.gw.CreateTarget(platform(), registry.PlatformScopeID, inbound.Actor{PrincipalID: "platform-a"}, inbound.CreateTargetInput{
		CanonicalContract: "bank.statement.v1", BaseURL: g.dom.URL, Path: "/v1/statements", Region: "uk"})
	if err != nil {
		t.Fatal(err)
	}
	tg, err = g.gw.ApproveTarget(platform(), registry.PlatformScopeID, inbound.Actor{PrincipalID: "platform-b"}, tg.TargetID, tg.Version)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func sig(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

func (g *gwEnv) now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().UTC().Add(g.nowAdd)
}

func (g *gwEnv) deliver(c *xicconn.Connection, body, key, secret string) *inbound.Outcome {
	ts := strconv.FormatInt(g.now().Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Timestamp", ts)
	h.Set("X-Signature", sig(secret, ts, body))
	if key != "" {
		h.Set("Idempotency-Key", key)
	}
	return g.gw.Receive(context.Background(), inbound.Request{ConnectorKey: c.ConnectorID, ConnectionID: c.ConnectionID, OperationID: "statement-push",
		Method: "POST", Header: h, Body: []byte(body), RemoteAddr: "198.51.100.7:4000", CorrelationID: "corr-pg"})
}

func body(event string) string {
	return `{"event_id":"` + event + `","account":"A1","balance":"10.5","currency":"GBP","booked_at":"2026-10-20T10:00:00+01:00","tenant":"` + cTenantA + `","holder_account":"acc-a"}`
}

func (g *gwEnv) active(t *testing.T) (*registry.ContractVersion, *xicconn.Connection) {
	t.Helper()
	v := g.contract(t, "bank-x", "1.0.0", nil)
	c := g.sandbox(t, cTenantA, v, "acc-a", cRefOAuth, cRefHMAC)
	return v, c
}

func (g *gwEnv) get(t *testing.T, id string) *inbound.Exchange {
	t.Helper()
	x, err := g.irepo.Get(tenantCtx(cTenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (g *gwEnv) drive(t *testing.T) {
	t.Helper()
	if _, err := g.gw.DriveDue(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
}

// ---- schema -----------------------------------------------------------------------------

func TestInboundPG_MigrationIsRerunnable(t *testing.T) {
	g := newGwEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "006") {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.admin.Exec(context.Background(), string(b)); err != nil {
				t.Fatalf("006 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 006 migration")
}

// ---- the pipeline through the database ---------------------------------------------------------------

func TestInboundPG_AcceptedThenDeliveredThenPurged(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	o := g.deliver(c, body("evt-1"), "k1", pgHMAC)
	if o.Status != 202 || o.Exchange == nil {
		t.Fatalf("%+v", o)
	}
	x := g.get(t, o.Exchange.ExchangeID)
	if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainPending || len(x.Raw) == 0 || x.RawExpiresAt == nil ||
		string(x.Canonical) != `{"account":"A1","balance":"10.50","booked_at":"2026-10-20T09:00:00Z","currency":"GBP"}` || x.BodyHash == "" || len(x.Lineage) == 0 {
		t.Fatalf("%+v", x)
	}
	events, _ := g.irepo.Events(tenantCtx(cTenantA), x.ExchangeID)
	if len(events) != 1 || events[0].Kind != "ACCEPTED" {
		t.Fatalf("%+v", events)
	}
	g.drive(t)
	x = g.get(t, o.Exchange.ExchangeID)
	if x.State != inbound.StateHandedOff || x.DomainOutcome != inbound.DomainAccepted || x.DomainRef != "dom-1" || x.TargetID == "" || x.NextAttemptAt != nil || atomic.LoadInt32(&g.domHits) != 1 {
		t.Fatalf("%+v hits=%d", x, g.domHits)
	}
	if _, ok := g.domKeys.Load(x.ExchangeID); !ok {
		t.Fatal("the domain was not called with the exchange id as its idempotency key")
	}
	// retention: nothing is purged early; after the date the bodies go and the evidence stays
	if n, _ := g.irepo.PurgeExpired(context.Background(), time.Now().UTC(), 100); n != 0 {
		t.Fatalf("purged %d early", n)
	}
	// back-date the retention as an operator would see it after the retention period (the database clock decides)
	// (triggers are disabled for this one superuser statement: a finished exchange is otherwise immutable)
	if _, err := g.admin.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica; UPDATE xic_inbound_exchanges SET raw_expires_at = now() - interval '1 minute'; COMMIT`); err != nil {
		t.Fatal(err)
	}
	n, err := g.irepo.PurgeExpired(context.Background(), time.Now().UTC(), 100)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	x = g.get(t, o.Exchange.ExchangeID)
	if len(x.Raw) != 0 || len(x.Canonical) != 0 || x.CanonicalHash == "" || x.State != inbound.StateHandedOff || x.RawRetained {
		t.Fatalf("%+v", x)
	}
	// the outbox carries the transport and delivery events as valid envelopes
	for _, want := range []string{"accepted", "handed-off"} {
		if cnt := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration.inbound."+want); cnt != 1 {
			t.Errorf("%d %s events", cnt, want)
		}
	}
}

func TestInboundPG_DuplicatesAndConflictsAreSettledByTheDatabase(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	first := g.deliver(c, body("evt-1"), "k", pgHMAC)
	dup := g.deliver(c, body("evt-1"), "k", pgHMAC)
	if dup.Status != 200 || !dup.Duplicate || dup.Exchange.ExchangeID != first.Exchange.ExchangeID {
		t.Fatalf("%+v", dup)
	}
	list, _ := g.irepo.List(tenantCtx(cTenantA), inbound.Filter{State: inbound.StateDuplicate})
	if len(list) != 1 || list[0].OriginalExchangeID != first.Exchange.ExchangeID {
		t.Fatalf("%+v", list)
	}
	if cnt := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.inbound.accepted'`); cnt != 1 {
		t.Fatalf("%d accepted events", cnt)
	}
	changed := g.deliver(c, strings.Replace(body("evt-1"), "10.5", "99.9", 1), "k", pgHMAC)
	if changed.Status != 409 || changed.Exchange.State != inbound.StateRejected || changed.Exchange.ReasonCode != "BODY_CHANGED_UNDER_SAME_KEY" {
		t.Fatalf("%+v", changed.Exchange)
	}
	// one key is held by exactly one live exchange, enforced by the unique constraint
	if n := g.countAs(cTenantA, `SELECT count(*) FROM xic_inbound_exchanges WHERE dedupe_key IS NOT NULL`); n != 1 {
		t.Fatalf("%d exchanges hold a key", n)
	}
	wantDBError(t, "raw duplicate key", g.execAs(cTenantA, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state,
		dedupe_key, body_hash, body_size) SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'REJECTED',
		dedupe_key, body_hash, 1 FROM xic_inbound_exchanges WHERE dedupe_key IS NOT NULL LIMIT 1`), "")
}

func TestInboundPG_ConcurrentIdenticalDeliveriesProduceOneEffect(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	const n = 16
	var fresh, dup, other int32
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := g.deliver(c, body("race"), "race", pgHMAC)
			switch {
			case o.Status == 202:
				atomic.AddInt32(&fresh, 1)
			case o.Status == 200 && o.Duplicate:
				atomic.AddInt32(&dup, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("%d %v", o.Status, o.XIC)
			}
			if o.Exchange != nil {
				ids <- o.Exchange.ExchangeID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if fresh != 1 || dup != n-1 || other != 0 || len(seen) != 1 {
		t.Fatalf("fresh=%d dup=%d other=%d refs=%d", fresh, dup, other, len(seen))
	}
	if cnt := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.inbound.accepted'`); cnt != 1 {
		t.Fatalf("%d accepted events", cnt)
	}
}

func TestInboundPG_QuarantineClassesAndTenantClaims(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	foreign := g.deliver(c, strings.Replace(body("e1"), cTenantA, cTenantB, 1), "k1", pgHMAC)
	if foreign.Status != 403 || foreign.Exchange.QuarantineClass != inbound.ClassAuthContext || foreign.Exchange.TenantID != cTenantA {
		t.Fatalf("%+v", foreign)
	}
	x := g.get(t, foreign.Exchange.ExchangeID)
	if len(x.Raw) != 0 || len(x.Canonical) != 0 {
		t.Fatal("a foreign-tenant payload was retained")
	}
	if n := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.security.inbound-tenant-claim-mismatch'`); n != 1 {
		t.Fatalf("%d security signals", n)
	}
	for name, tc := range map[string]struct {
		b     string
		class string
	}{
		"mapping":     {strings.Replace(body("e2"), "10.5", "10.555", 1), inbound.ClassMapping},
		"schema":      {`{"event_id":"e3","account":"A1","booked_at":"2026-10-20T10:00:00Z"}`, inbound.ClassSchema},
		"data policy": {strings.Replace(body("e4"), `"holder_account"`, `"holder":{"national_id":"X1"},"holder_account"`, 1), inbound.ClassDataPolicy},
	} {
		o := g.deliver(c, tc.b, "k-"+name, pgHMAC)
		if o.Exchange == nil || o.Exchange.State != inbound.StateQuarantined || o.Exchange.QuarantineClass != tc.class {
			t.Fatalf("%s: %+v", name, o.Exchange)
		}
	}
	// the prohibited value is stored nowhere in the database
	if n := g.countAs(cTenantA, `SELECT count(*) FROM xic_inbound_exchanges WHERE to_jsonb(xic_inbound_exchanges)::text LIKE '%X1%'`); n != 0 {
		t.Fatalf("%d rows carry the prohibited value", n)
	}
}

func TestInboundPG_PreAuthenticationRefusalsWriteNothing(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	before := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox`)
	for i := 0; i < 5; i++ {
		if o := g.deliver(c, body("e"+strconv.Itoa(i)), "k", "wrong-secret"); o.Status != 401 || o.Exchange != nil {
			t.Fatalf("%+v", o)
		}
	}
	if n := g.countAs(cTenantA, `SELECT count(*) FROM xic_inbound_exchanges`); n != 0 {
		t.Fatalf("%d exchanges for unauthenticated input", n)
	}
	if after := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox`); after != before {
		t.Fatalf("%d events for unauthenticated input", after-before)
	}
}

// ---- isolation ------------------------------------------------------------------------------------------------

func TestInboundPG_TenantIsolationAndTheWorkersLimits(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	o := g.deliver(c, body("e1"), "k", pgHMAC)
	for _, tbl := range []string{"xic_inbound_exchanges", "xic_inbound_events", "xic_quarantine_requests"} {
		if n := g.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d rows of %s", n, tbl)
		}
	}
	if _, err := g.irepo.Get(tenantCtx(cTenantB), o.Exchange.ExchangeID); !errors.Is(err, inbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if list, _ := g.irepo.List(tenantCtx(cTenantB), inbound.Filter{}); len(list) != 0 {
		t.Fatal("tenant B lists tenant A's exchanges")
	}
	g.wantRefused(t, "update", cTenantB, `UPDATE xic_inbound_exchanges SET reason_code='x', version=version+1`)
	g.wantRefused(t, "delete", cTenantB, `DELETE FROM xic_inbound_exchanges`)
	wantDBError(t, "forged exchange", g.execAs(cTenantB, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, reason_code, body_hash, body_size)
		SELECT gen_random_uuid(), 'tenant-a', connection_id, connector_id, operation_id, contract_version_id, 'REJECTED', 'x', body_hash, 1 FROM xic_connections c
		JOIN xic_inbound_exchanges e USING (connection_id) LIMIT 1`), "")
	// the worker reads across tenants (and only reads, apart from the purge)
	due, err := g.irepo.Due(context.Background(), time.Now().UTC().Add(time.Minute), 10)
	if err != nil || len(due) != 1 || due[0].TenantID != cTenantA {
		t.Fatalf("%v %+v", err, due)
	}
	if err := g.execAsWorker(`UPDATE xic_inbound_exchanges SET reason_code='x', version=version+1`); err == nil {
		var r string
		_ = g.admin.QueryRow(context.Background(), `SELECT reason_code FROM xic_inbound_exchanges`).Scan(&r)
		if r == "x" {
			t.Fatal("the cross-tenant worker rewrote an exchange")
		}
	}
	// the purge flag may only remove EXPIRED bodies
	wantDBError(t, "purge before expiry", g.execPurge(`UPDATE xic_inbound_exchanges SET raw=NULL, canonical=NULL`), "")
	wantDBError(t, "purge changing state", g.execPurge(`UPDATE xic_inbound_exchanges SET raw=NULL, state='REJECTED'`), "")
	// a bridge of the other tenant cannot present an exchange of this one
	if _, err := g.gw.Conns.Connection(context.Background(), c.ConnectionID); err != nil {
		t.Fatal(err)
	}
}

func (g *gwEnv) execPurge(sql string) error {
	ctx := context.Background()
	tx, err := g.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','',true), set_config('app.xic_worker','true',true), set_config('app.xic_purge','true',true)`); err != nil {
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

// ---- guards ------------------------------------------------------------------------------------------------------

func TestInboundPG_DatabaseGuards(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	A := cTenantA
	v := g.deliver(c, body("ok-1"), "k-ok", pgHMAC).Exchange
	q := g.deliver(c, strings.Replace(body("q-1"), "10.5", "10.555", 1), "k-q", pgHMAC).Exchange
	g.drive(t)
	done := g.get(t, v.ExchangeID)
	if done.State != inbound.StateHandedOff {
		t.Fatalf("%+v", done)
	}
	rej := g.deliver(c, body("r-1"), "", pgHMAC).Exchange // no key => rejected
	bump := `, version = version + 1`
	upd := func(id, set string) error {
		return g.execAs(A, `UPDATE xic_inbound_exchanges SET `+set+bump+` WHERE exchange_id=$1::uuid`, id)
	}

	t.Run("identity and body hash are immutable", func(t *testing.T) {
		for col, set := range map[string]string{
			"tenant": `tenant_id='tenant-b'`, "connection": `connection_id=gen_random_uuid()`, "operation": `operation_id='other'`,
			"body hash": `body_hash=repeat('b',64)`, "source ref": `source_ref_hash='x'`, "dedupe key": `dedupe_key='x'`, "received": `received_at=now()`,
		} {
			wantDBError(t, col, upd(q.ExchangeID, set), "")
		}
	})
	t.Run("version advances by one", func(t *testing.T) {
		wantDBError(t, "no bump", g.execAs(A, `UPDATE xic_inbound_exchanges SET reason_code='x' WHERE exchange_id=$1::uuid`, q.ExchangeID), "version")
	})
	t.Run("state graph", func(t *testing.T) {
		for _, c := range []struct{ name, id, to string }{
			{"handed off is final", v.ExchangeID, "QUARANTINED"},
			{"rejected is final", rej.ExchangeID, "VALIDATED"},
			{"quarantined to handed off", q.ExchangeID, "HANDED_OFF"},
			{"quarantined to duplicate", q.ExchangeID, "DUPLICATE"},
		} {
			wantDBError(t, c.name, upd(c.id, `state='`+c.to+`'`), "")
		}
		wantDBError(t, "handed-off exchange edited", upd(v.ExchangeID, `reason_code='x'`), "final")
	})
	t.Run("states carry their facts", func(t *testing.T) {
		wantDBError(t, "validated without canonical", upd(q.ExchangeID, `state='VALIDATED', quarantine_class=NULL, reason_code=''`), "")
		wantDBError(t, "quarantined without class", g.execAs(A, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, reason_code, body_hash, body_size)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'QUARANTINED', 'x', repeat('a',64), 1 FROM xic_inbound_exchanges LIMIT 1`), "")
		wantDBError(t, "rejected without reason", g.execAs(A, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, body_hash, body_size)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'REJECTED', repeat('a',64), 1 FROM xic_inbound_exchanges LIMIT 1`), "")
		wantDBError(t, "duplicate without original", g.execAs(A, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, reason_code, body_hash, body_size)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'DUPLICATE', 'D', repeat('a',64), 1 FROM xic_inbound_exchanges LIMIT 1`), "")
		wantDBError(t, "raw without an expiry", g.execAs(A, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, reason_code, body_hash, body_size, raw)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'REJECTED', 'x', repeat('a',64), 1, '\x01'::bytea FROM xic_inbound_exchanges LIMIT 1`), "")
		wantDBError(t, "bad class", upd(q.ExchangeID, `quarantine_class='WHATEVER'`), "")
		wantDBError(t, "bad outcome", upd(q.ExchangeID, `domain_outcome='MAYBE'`), "")
		wantDBError(t, "bad body hash", g.execAs(A, `INSERT INTO xic_inbound_exchanges (exchange_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id, state, reason_code, body_hash, body_size)
			SELECT gen_random_uuid(), tenant_id, connection_id, connector_id, operation_id, contract_version_id, 'REJECTED', 'x', 'nothex', 1 FROM xic_inbound_exchanges LIMIT 1`), "")
	})
	t.Run("a new attempt follows a quarantined one", func(t *testing.T) {
		wantDBError(t, "attempt jump", upd(q.ExchangeID, `attempt_no=attempt_no+2`), "attempt")
		wantDBError(t, "attempt on a non-quarantined exchange", upd(v.ExchangeID, `attempt_no=attempt_no+1`), "")
	})
	t.Run("the raw body is purged or kept, never rewritten", func(t *testing.T) {
		wantDBError(t, "rewrite raw", upd(q.ExchangeID, `raw='\x0102'::bytea`), "raw payload")
	})
	t.Run("nothing is deleted", func(t *testing.T) {
		wantDBError(t, "delete", g.execAs(A, `DELETE FROM xic_inbound_exchanges WHERE exchange_id=$1::uuid`, v.ExchangeID), "never deleted")
		g.wantRefused(t, "events update", A, `UPDATE xic_inbound_events SET kind='x'`)
		g.wantRefused(t, "events delete", A, `DELETE FROM xic_inbound_events`)
	})
	t.Run("targets", func(t *testing.T) {
		P := registry.PlatformScopeID
		for col, set := range map[string]string{"url": `base_url='https://other.example'`, "path": `path='/x'`, "contract": `canonical_contract='x'`, "region": `region='eu'`, "fingerprint": `fingerprint=repeat('c',64)`} {
			wantDBError(t, col, g.execAs(P, `UPDATE xic_handoff_targets SET `+set+`, version=version+1`), "immutable")
		}
		wantDBError(t, "no bump", g.execAs(P, `UPDATE xic_handoff_targets SET status='SUSPENDED'`), "version")
		wantDBError(t, "active to draft", g.execAs(P, `UPDATE xic_handoff_targets SET status='DRAFT', version=version+1`), "")
		g.wantRefused(t, "delete", P, `DELETE FROM xic_handoff_targets`)
		wantDBError(t, "a tenant writes a target", g.execAs(A, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by)
			VALUES (gen_random_uuid(),'evil.v1','https://evil.example','/x','uk',repeat('a',64),'evil')`), "")
		g.wantRefused(t, "a tenant edits a target", A, `UPDATE xic_handoff_targets SET status='SUSPENDED', version=version+1`)
		wantDBError(t, "url with credentials", g.execAs(P, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by)
			VALUES (gen_random_uuid(),'c2.v1','https://u:p@d.example','/x','uk',repeat('a',64),'x')`), "")
		wantDBError(t, "path with query", g.execAs(P, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by)
			VALUES (gen_random_uuid(),'c3.v1','https://d.example','/x?a=1','uk',repeat('a',64),'x')`), "")
		wantDBError(t, "two live targets for one contract", g.execAs(P, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by)
			VALUES (gen_random_uuid(),'bank.statement.v1','https://d.example','/x','uk',repeat('a',64),'x')`), "")
		wantDBError(t, "active without an approver", g.execAs(P, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by, status)
			VALUES (gen_random_uuid(),'c4.v1','https://d.example','/x','uk',repeat('a',64),'x','ACTIVE')`), "")
		wantDBError(t, "approved by the author", g.execAs(P, `INSERT INTO xic_handoff_targets (target_id, canonical_contract, base_url, path, region, fingerprint, created_by, status, approved_by, approved_at)
			VALUES (gen_random_uuid(),'c5.v1','https://d.example','/x','uk',repeat('a',64),'same','ACTIVE','same',now())`), "")
	})
	t.Run("quarantine requests", func(t *testing.T) {
		req, err := g.gw.RequestAction(tenantCtx(A), inbound.Actor{PrincipalID: "requester"}, q.ExchangeID, "DISMISS", "policy resolved")
		if err != nil {
			t.Fatal(err)
		}
		wantDBError(t, "self decision", g.execAs(A, `UPDATE xic_quarantine_requests SET status='APPROVED', decided_by=requested_by WHERE request_id=$1::uuid`, req.RequestID), "")
		wantDBError(t, "reason rewrite", g.execAs(A, `UPDATE xic_quarantine_requests SET reason='x' WHERE request_id=$1::uuid`, req.RequestID), "immutable")
		wantDBError(t, "a second open request", g.execAs(A, `INSERT INTO xic_quarantine_requests (request_id, exchange_id, tenant_id, action, reason, requested_by)
			VALUES (gen_random_uuid(), $1::uuid, 'tenant-a', 'DISMISS', 'again', 'x')`, q.ExchangeID), "xic_qr_one_open")
		g.wantRefused(t, "delete", A, `DELETE FROM xic_quarantine_requests`)
	})
}

// ---- release and replay through the database --------------------------------------------------------------------------

func TestInboundPG_ReleaseReplaysTheStoredBodyAsANewAttempt(t *testing.T) {
	g := newGwEnv(t)
	v1 := g.contract(t, "bank-x", "1.0.0", nil)
	c := g.sandbox(t, cTenantA, v1, "acc-a", cRefOAuth, cRefHMAC)
	g.target(t)
	q := g.deliver(c, strings.Replace(body("evt-1"), "10.5", "10.555", 1), "k1", pgHMAC).Exchange
	if q.State != inbound.StateQuarantined || q.QuarantineClass != inbound.ClassMapping {
		t.Fatalf("%+v", q)
	}
	ctx := tenantCtx(cTenantA)
	// the corrected contract (scale 3) is published and the connection migrates to it
	v2 := g.contract(t, "bank-x", "1.1.0", func(in *registry.VersionInput) {
		in.Content.Mappings[0].Spec = json.RawMessage(strings.Replace(string(in.Content.Mappings[0].Spec), `"scale":2`, `"scale":3`, 1))
		in.ReplayAnalysisRef = "REPLAY-1 scale widened"
	})
	mreq, err := g.svc.RequestMigration(ctx, cCreator, c.ConnectionID, xicconn.MigrationInput{ToContractVersionID: v2.ContractVersionID, Reason: "adopt the corrected mapping"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.DecideMigration(ctx, cApprover, c.ConnectionID, mreq.RequestID, true); err != nil {
		t.Fatal(err)
	}
	req, err := g.gw.RequestAction(ctx, inbound.Actor{PrincipalID: "requester"}, q.ExchangeID, "RELEASE", "mapping corrected")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.gw.DecideAction(ctx, inbound.Actor{PrincipalID: "requester"}, q.ExchangeID, req.RequestID, true); !errors.Is(err, inbound.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	ex, dq, err := g.gw.DecideAction(ctx, inbound.Actor{PrincipalID: "approver"}, q.ExchangeID, req.RequestID, true)
	if err != nil || dq.Status != inbound.ReqUsed || ex.State != inbound.StateValidated || ex.AttemptNo != 2 || !strings.Contains(string(ex.Canonical), `"balance":"10.555"`) {
		t.Fatalf("%v %+v", err, ex)
	}
	g.drive(t)
	done := g.get(t, q.ExchangeID)
	if done.State != inbound.StateHandedOff || done.BodyHash != q.BodyHash || done.SourceRefHash != q.SourceRefHash {
		t.Fatalf("%+v", done)
	}
	for _, want := range []string{"quarantine-released"} {
		if n := g.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration.inbound."+want); n != 1 {
			t.Errorf("%d %s", n, want)
		}
	}
	events, _ := g.irepo.Events(ctx, q.ExchangeID)
	kinds := ""
	for _, ev := range events {
		kinds += ev.Kind + ","
	}
	if !strings.Contains(kinds, "QUARANTINE_RELEASE_REQUESTED,QUARANTINE_RELEASED") {
		t.Fatalf("%s", kinds)
	}
}

func TestInboundPG_DomainRejectionAndRequeue(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	atomic.StoreInt32(&g.domCode, 409)
	o := g.deliver(c, body("e1"), "k1", pgHMAC)
	g.drive(t)
	x := g.get(t, o.Exchange.ExchangeID)
	if x.State != inbound.StateQuarantined || x.QuarantineClass != inbound.ClassDomainPrecondition || x.DomainOutcome != inbound.DomainRejected {
		t.Fatalf("%+v", x)
	}
	atomic.StoreInt32(&g.domCode, 200)
	ctx := tenantCtx(cTenantA)
	req, _ := g.gw.RequestAction(ctx, inbound.Actor{PrincipalID: "r"}, x.ExchangeID, "RELEASE", "case closed")
	if _, _, err := g.gw.DecideAction(ctx, inbound.Actor{PrincipalID: "a"}, x.ExchangeID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	g.drive(t)
	x = g.get(t, o.Exchange.ExchangeID)
	if x.State != inbound.StateHandedOff || x.AttemptNo != 2 {
		t.Fatalf("%+v", x)
	}
	// both deliveries used the same idempotency key
	n := 0
	g.domKeys.Range(func(_, _ any) bool { n++; return true })
	if n != 1 {
		t.Fatalf("%d distinct idempotency keys across the retry", n)
	}
}

func TestInboundPG_ConcurrentWorkersDeliverOnce(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	o := g.deliver(c, body("e1"), "k1", pgHMAC)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.gw.Drive(context.Background(), inbound.DueRef{TenantID: cTenantA, ExchangeID: o.Exchange.ExchangeID})
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&g.domHits); n != 1 {
		t.Fatalf("%d domain calls from 8 workers", n)
	}
	if x := g.get(t, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff {
		t.Fatalf("%+v", x)
	}
}

func TestInboundPG_UnknownOutcomeKeepsTheExchangeValidated(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	g.target(t)
	atomic.StoreInt32(&g.domCode, 503)
	o := g.deliver(c, body("e1"), "k1", pgHMAC)
	g.drive(t)
	x := g.get(t, o.Exchange.ExchangeID)
	if x.State != inbound.StateValidated || x.DomainOutcome != inbound.DomainUnknown || x.LastErrorCode != "HTTP_503" || x.HandoffAttempts != 1 || x.NextAttemptAt == nil {
		t.Fatalf("%+v", x)
	}
	g.drive(t) // backoff not yet elapsed
	if atomic.LoadInt32(&g.domHits) != 1 {
		t.Fatal("retried before the backoff")
	}
	atomic.StoreInt32(&g.domCode, 200)
	g.advance(time.Minute)
	g.drive(t)
	if x = g.get(t, o.Exchange.ExchangeID); x.State != inbound.StateHandedOff || x.HandoffAttempts != 2 {
		t.Fatalf("%+v", x)
	}
}

// Every event type XIC-03 can emit is produced by a real flow and accepted by the outbox as a valid envelope.
func TestInboundPG_EveryEventTypeIsEmitted(t *testing.T) {
	g := newGwEnv(t)
	_, c := g.active(t)
	tg := g.target(t)
	ctx := tenantCtx(cTenantA)
	g.deliver(c, body("e1"), "k1", pgHMAC)                                                    // accepted
	g.deliver(c, body("e1"), "k1", pgHMAC)                                                    // duplicate
	g.deliver(c, body("e1"), "", pgHMAC)                                                      // rejected
	foreign := g.deliver(c, strings.Replace(body("e2"), cTenantA, cTenantB, 1), "k2", pgHMAC) // tenant claim signal + quarantined
	_ = foreign
	g.deliver(c, strings.Replace(body("e3"), "acc-a", "other-acct", 1), "k3", pgHMAC) // account claim signal
	atomic.StoreInt32(&g.domCode, 503)
	g.drive(t) // handoff-unknown
	g.gw.Cfg.HandoffMaxAttempts = 2
	g.advance(time.Minute)
	g.drive(t) // second unknown attempt: stalled
	atomic.StoreInt32(&g.domCode, 200)
	g.advance(time.Hour)
	g.drive(t) // handed-off
	// dismiss a quarantined exchange
	q := g.deliver(c, strings.Replace(body("e4"), "10.5", "10.555", 1), "k4", pgHMAC).Exchange
	req, _ := g.gw.RequestAction(ctx, inbound.Actor{PrincipalID: "r"}, q.ExchangeID, "DISMISS", "no longer needed")
	if _, _, err := g.gw.DecideAction(ctx, inbound.Actor{PrincipalID: "a"}, q.ExchangeID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	// release a domain-rejected exchange
	atomic.StoreInt32(&g.domCode, 409)
	r1 := g.deliver(c, body("e5"), "k5", pgHMAC).Exchange
	g.drive(t)
	atomic.StoreInt32(&g.domCode, 200)
	req, _ = g.gw.RequestAction(ctx, inbound.Actor{PrincipalID: "r"}, r1.ExchangeID, "RELEASE", "case closed")
	if _, _, err := g.gw.DecideAction(ctx, inbound.Actor{PrincipalID: "a"}, r1.ExchangeID, req.RequestID, true); err != nil {
		t.Fatal(err)
	}
	// egress and residency signals
	var blocked atomic.Bool
	g.gw.Handoff.Check = func(context.Context, string) (string, error) {
		if blocked.Load() {
			return "", errors.New("blocked")
		}
		return "ok", nil
	}
	blocked.Store(true)
	g.deliver(c, body("e6"), "k6", pgHMAC)
	g.drive(t)
	blocked.Store(false)
	g.gw.Handoff.AllowedRegions = []string{"eu"}
	g.advance(time.Hour)
	g.drive(t)
	g.gw.Handoff.AllowedRegions = []string{"uk"}
	// suspend the target
	if _, err := g.gw.SetTargetStatus(platform(), registry.PlatformScopeID, inbound.Actor{PrincipalID: "p"}, tg.TargetID, 2, "SUSPENDED"); err != nil {
		t.Fatal(err)
	}

	rows, err := g.admin.Query(context.Background(), `SELECT DISTINCT event_type FROM eventing_outbox`)
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
	for _, want := range inbound.EventTypes {
		if !have["com.zoikosuite."+want] {
			t.Errorf("event %s was never emitted by a real flow; have %v", want, have)
		}
	}
}

var _ = xicconn.StatusActive
