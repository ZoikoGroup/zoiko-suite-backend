package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

// These run the XIC-04 engine against REAL Postgres as an ordinary role: the SQL, the guard triggers and constraints
// the database holds itself (no blind resubmission, one logical operation, final states, governed manual resolution),
// row-level security, the outbox, body retention and concurrency.

const (
	obRefKey = "0a000000-0000-4000-8000-0000000000d1"
	obKey    = "pg-provider-api-key"
	obPay    = `{"amount":"10.00","currency":"GBP"}`
)

type obSecrets struct{ down atomic.Bool }

func (s *obSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	if s.down.Load() {
		return nil, outbound.ErrSecretUnavailable
	}
	if strings.EqualFold(ref, obRefKey) {
		return []byte(obKey), nil
	}
	return nil, outbound.ErrBlocked
}

type obProvider struct {
	mu    sync.Mutex
	calls []obCall
	fn    func(c obCall) (int, string, error)
}

type obCall struct {
	method, url string
	header      http.Header
	body        []byte
}

type obTimeout struct{}

func (obTimeout) Error() string   { return "i/o timeout" }
func (obTimeout) Timeout() bool   { return true }
func (obTimeout) Temporary() bool { return true }

func (p *obProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	c := obCall{method: r.Method, url: r.URL.String(), header: r.Header.Clone(), body: b}
	p.mu.Lock()
	p.calls = append(p.calls, c)
	fn := p.fn
	p.mu.Unlock()
	status, body, err := 200, `{"payment_id":"P-1"}`, error(nil)
	if fn != nil {
		status, body, err = fn(c)
	}
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader([]byte(body))), Request: r}, nil
}

func (p *obProvider) set(fn func(c obCall) (int, string, error)) {
	p.mu.Lock()
	p.fn = fn
	p.mu.Unlock()
}
func (p *obProvider) posts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.method == http.MethodPost {
			n++
		}
	}
	return n
}

type obEnv struct {
	*connEnv
	repo    *store.OutboundRepo
	svc     *outbound.Service
	secrets *obSecrets
	prov    *obProvider
}

func newObEnv(t *testing.T) *obEnv {
	t.Helper()
	c := newConnEnv(t)
	o := &obEnv{connEnv: c, secrets: &obSecrets{}, prov: &obProvider{}}
	st := store.NewPgStore(c.app, store.WithEventRegion("uk"))
	o.repo = store.NewOutboundRepo(st)
	o.svc = &outbound.Service{Repo: o.repo, Conns: pgConns{c.svc}, Secrets: o.secrets, Egress: egress.Policy{Resolver: pubDNS{}}, Cfg: outbound.DefaultConfig(),
		Clock: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return time.Now().UTC().Add(c.nowAdd) },
		HTTP:  &http.Client{Transport: o.prov}}
	return o
}

// contract publishes the reference contract with outbound calls carried by an API key.
func (o *obEnv) contract(t *testing.T, resubmit bool) *registry.ContractVersion {
	t.Helper()
	return o.published(t, "bank-x", "1.0.0", func(in *registry.VersionInput) {
		in.Content.Profiles = append(in.Content.Profiles, registry.Profile{Kind: registry.ProfileAuth, Name: "api-out",
			Spec: json.RawMessage(`{"mechanism":"API_KEY_HEADER","credential_class":"PROVIDER_CREDENTIAL","api_key_header":"X-Api-Key"}`)})
		for i := range in.Content.Operations {
			op := &in.Content.Operations[i]
			switch op.ID {
			case "payment-submit":
				op.AuthProfile, op.CallbackProfile, op.DataClasses, op.ResubmitAfterUnknown = "api-out", "", []string{"FINANCIAL"}, resubmit
				op.UnknownResolution = registry.UnknownResolution{StatusQueryOperation: "payment-status"}
			case "payment-status":
				op.AuthProfile = "api-out"
			}
		}
	})
}

func (o *obEnv) active(t *testing.T, tn string, v *registry.ContractVersion, account string, hmacRef string) *xicconn.Connection {
	t.Helper()
	ctx := tenantCtx(tn)
	c := o.create(t, tn, xicconn.EnvSandbox, v, account)
	res, err := o.connEnv.svc.Authorize(ctx, cCreator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil {
		t.Fatal(err)
	}
	cur := res.Connection
	exp := time.Now().Add(60 * 24 * time.Hour)
	for _, b := range []xicconn.BindInput{
		{AuthProfile: "api-out", CredentialRef: obRefKey, ConsentActor: "tenant-admin", ExpiresAt: &exp},
		{AuthProfile: "hmac-in", CredentialRef: hmacRef, ConsentActor: "tenant-admin"},
	} {
		if cur, err = o.connEnv.svc.Bind(ctx, cCreator, c.ConnectionID, cur.Version, b); err != nil {
			t.Fatal(err)
		}
	}
	cur = o.validatedC(t, tn, cur)
	act, err := o.connEnv.svc.Activate(ctx, cOperator, c.ConnectionID, cur.Version)
	if err != nil {
		t.Fatal(err)
	}
	return act
}

func (o *obEnv) ready(t *testing.T, resubmit bool) *xicconn.Connection {
	t.Helper()
	return o.active(t, cTenantA, o.contract(t, resubmit), "acc-out", cRefHMAC)
}

var (
	obDomain = outbound.Actor{PrincipalID: "domain-svc", CorrelationID: "corr-pg-ob"}
	obOps    = outbound.Actor{PrincipalID: "ops-lead"}
)

func (o *obEnv) reserve(t *testing.T, c *xicconn.Connection, ref string) *outbound.Attempt {
	t.Helper()
	a, created, err := o.svc.Reserve(tenantCtx(c.TenantID), obDomain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: ref, Payload: json.RawMessage(obPay)})
	if err != nil || !created {
		t.Fatalf("reserve: %v created=%v", err, created)
	}
	return a
}

func (o *obEnv) get(t *testing.T, id string) *outbound.Attempt {
	t.Helper()
	a, err := o.repo.Get(tenantCtx(cTenantA), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (o *obEnv) unknown(t *testing.T, c *xicconn.Connection, ref string) *outbound.Attempt {
	t.Helper()
	a := o.reserve(t, c, ref)
	o.prov.set(func(obCall) (int, string, error) { return 0, "", obTimeout{} })
	got, err := o.svc.Submit(tenantCtx(c.TenantID), obDomain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateUnknown {
		t.Fatalf("%v %+v", err, got)
	}
	return got
}

// ---- schema -------------------------------------------------------------------------------------------------

func TestOutboundPG_MigrationIsRerunnable(t *testing.T) {
	o := newObEnv(t)
	dir, names := migrationFiles(t)
	for _, n := range names {
		if strings.HasPrefix(n, "007") {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.admin.Exec(context.Background(), string(b)); err != nil {
				t.Fatalf("007 applied a second time: %v", err)
			}
			return
		}
	}
	t.Fatal("no 007 migration")
}

// ---- the flow through the database -------------------------------------------------------------------------

func TestOutboundPG_ReserveSubmitSettleAndDropBody(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.reserve(t, c, "PAY-1")
	if o.prov.posts() != 0 {
		t.Fatal("reserving called the provider")
	}
	got := o.get(t, a.AttemptID)
	if got.State != outbound.StateReserved || !got.BodyRetained || string(got.Body) != obPay || got.PayloadHash != a.PayloadHash || got.ProviderIdemRefHash == "" {
		t.Fatalf("%+v", got)
	}
	// reserving again returns the original; a different payload conflicts; neither adds a row
	again, created, err := o.svc.Reserve(tenantCtx(cTenantA), obDomain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1", Payload: json.RawMessage(obPay)})
	if err != nil || created || again.AttemptID != a.AttemptID {
		t.Fatalf("%v %v %+v", err, created, again)
	}
	if _, _, err := o.svc.Reserve(tenantCtx(cTenantA), obDomain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-1", Payload: json.RawMessage(`{"amount":"1.00","currency":"GBP"}`)}); !errors.Is(err, outbound.ErrPayloadConflict) {
		t.Fatalf("%v", err)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_attempts`); n != 1 {
		t.Fatalf("%d attempts", n)
	}

	done, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil)
	if err != nil || done.State != outbound.StateSettled || done.ProviderReference != "P-1" || done.SubmitCount != 1 || done.ResolutionMethod != outbound.MethodSubmit || len(done.FinalOutcomeRef) == 0 {
		t.Fatalf("%v %+v", err, done)
	}
	if got := o.get(t, a.AttemptID); got.BodyRetained || len(got.Body) != 0 {
		t.Fatal("a final attempt kept its body")
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_attempts WHERE body IS NOT NULL`); n != 0 {
		t.Fatalf("%d bodies", n)
	}
	// evidence and outbox, as valid integration envelopes, with no secret and no value
	events, _ := o.repo.Events(tenantCtx(cTenantA), a.AttemptID)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "RESERVED,SUBMIT_STARTED,RESOLVED" {
		t.Fatalf("%v", kinds)
	}
	for _, want := range []string{"reserved", "submitted", "resolved"} {
		if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type=$1`, "com.zoikosuite.integration.outbound."+want); n != 1 {
			t.Errorf("%d %s events", n, want)
		}
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%outbound%' AND (payload::text LIKE '%'||$1||'%' OR payload::text LIKE '%10.00%')`, obKey); n != 0 {
		t.Fatalf("%d outbound events leak a secret or a value", n)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_events WHERE detail::text LIKE '%'||$1||'%' OR detail::text LIKE '%10.00%'`, obKey); n != 0 {
		t.Fatalf("%d evidence rows leak a secret or a value", n)
	}
	// a final attempt is never sent again
	if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil); err != nil || o.prov.posts() != 1 {
		t.Fatalf("%v posts=%d", err, o.prov.posts())
	}
}

func TestOutboundPG_UnknownIsResolvedByQueryAndNeverResubmitted(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.unknown(t, c, "PAY-1")
	if a.AmbiguityReason != outbound.ReasonTimeout || a.NextResolutionAt == nil || !a.BodyRetained {
		t.Fatalf("%+v", a)
	}
	for i := 0; i < 3; i++ {
		if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil); !errors.Is(err, outbound.ErrUnknownMustBeResolved) {
			t.Fatalf("%v", err)
		}
	}
	if o.prov.posts() != 1 {
		t.Fatalf("%d submissions of one logical operation", o.prov.posts())
	}
	// the database refuses it too, even for a buggy engine
	err := o.execAs(cTenantA, `UPDATE xic_outbound_attempts SET state='SUBMITTING', submit_count=submit_count+1, last_submit_at=now(), version=version+1 WHERE attempt_id=$1::uuid`, a.AttemptID)
	wantDBError(t, "blind resubmission", err, "resolve the original")

	o.advance(time.Hour)
	o.prov.set(func(c obCall) (int, string, error) {
		if c.method == http.MethodGet {
			return 200, `{"status":"SETTLED","payment_id":"P-3"}`, nil
		}
		return 200, `{}`, nil
	})
	got, err := o.svc.Resolve(tenantCtx(cTenantA), obOps, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateSettled || got.ResolutionMethod != outbound.MethodStatusQuery || got.ProviderReference != "P-3" || got.BodyRetained {
		t.Fatalf("%v %+v", err, got)
	}
	if o.prov.posts() != 1 {
		t.Fatal("resolving submitted again")
	}
}

func TestOutboundPG_ResubmissionOnlyWhereTheContractAllowsIt(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, true)
	a := o.unknown(t, c, "PAY-1")
	if !a.ResubmitAllowed {
		t.Fatal("not allowed")
	}
	o.prov.set(nil)
	got, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil)
	if err != nil || got.State != outbound.StateSettled || got.SubmitCount != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM xic_outbound_attempts`); n != 1 {
		t.Fatalf("%d logical attempts", n)
	}
}

func TestOutboundPG_ConcurrentSubmittersSendOnce(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.reserve(t, c, "PAY-1")
	o.prov.set(func(obCall) (int, string, error) {
		time.Sleep(30 * time.Millisecond)
		return 200, `{"payment_id":"P-1"}`, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = o.svc.Submit(tenantCtx(cTenantA), obDomain, a.AttemptID, nil)
		}()
	}
	wg.Wait()
	if n := o.prov.posts(); n != 1 {
		t.Fatalf("the provider saw %d submissions", n)
	}
	if got := o.get(t, a.AttemptID); got.State != outbound.StateSettled || got.SubmitCount != 1 {
		t.Fatalf("%+v", got)
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.outbound.submitted'`); n != 1 {
		t.Fatalf("%d submitted events", n)
	}
}

func TestOutboundPG_ConcurrentReservesMakeOneOperation(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	var created, dup int32
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, made, err := o.svc.Reserve(tenantCtx(cTenantA), obDomain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-RACE", Payload: json.RawMessage(obPay)})
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			if made {
				atomic.AddInt32(&created, 1)
			} else {
				atomic.AddInt32(&dup, 1)
			}
			ids <- a.AttemptID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if created != 1 || dup != 15 || len(seen) != 1 {
		t.Fatalf("created=%d dup=%d refs=%d", created, dup, len(seen))
	}
	if n := o.countAs(cTenantA, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.integration.outbound.reserved'`); n != 1 {
		t.Fatalf("%d reserved events", n)
	}
}

// ---- what the database enforces on its own ---------------------------------------------------------------

func TestOutboundPG_DatabaseRulesHoldWithoutTheEngine(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	open := o.reserve(t, c, "PAY-OPEN")
	done := o.reserve(t, c, "PAY-DONE")
	if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, done.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	set := func(id, sql string, args ...any) error {
		return o.execAs(cTenantA, `UPDATE xic_outbound_attempts SET `+sql+`, version=version+1 WHERE attempt_id=$1::uuid`, append([]any{id}, args...)...)
	}
	wantDBError(t, "identity is immutable", set(open.AttemptID, `domain_request_ref='OTHER'`), "immutable")
	wantDBError(t, "payload hash is immutable", set(open.AttemptID, `payload_hash=repeat('a', 64)`), "immutable")
	wantDBError(t, "provider key hash is immutable", set(open.AttemptID, `provider_idem_ref_hash=repeat('b', 64)`), "immutable")
	wantDBError(t, "resubmit flag is immutable", set(open.AttemptID, `resubmit_allowed=true`), "immutable")
	wantDBError(t, "final is final", set(done.AttemptID, `provider_reference='X'`), "final")
	wantDBError(t, "no jump to settled without an outcome", set(open.AttemptID, `state='SETTLED'`), "")
	wantDBError(t, "no jump over the graph", set(open.AttemptID, `state='UNKNOWN', ambiguity_reason='X'`), "cannot go")
	wantDBError(t, "submitting without counting", set(open.AttemptID, `state='SUBMITTING', last_submit_at=now()`), "submit_count")
	wantDBError(t, "counting without submitting", set(open.AttemptID, `submit_count=submit_count+1`), "submit_count")
	wantDBError(t, "version must advance by one", o.execAs(cTenantA, `UPDATE xic_outbound_attempts SET provider_reference='X', version=version+5 WHERE attempt_id=$1::uuid`, open.AttemptID), "version")
	wantDBError(t, "delete", o.execAs(cTenantA, `DELETE FROM xic_outbound_attempts WHERE attempt_id=$1::uuid`, open.AttemptID), "never deleted")
	wantDBError(t, "evidence rewrite", o.execAs(cTenantA, `UPDATE xic_outbound_events SET kind='X'`), "")
	wantDBError(t, "evidence delete", o.execAs(cTenantA, `DELETE FROM xic_outbound_events`), "")
	wantDBError(t, "manual resolution by the requester", set(open.AttemptID,
		`state='SETTLED', resolution_method='MANUAL', resolved_by=requested_by, evidence_ref='E', final_outcome='{}'::jsonb`), "")
	// a manual resolution needs evidence, another person, and a state that is unresolved
	if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, open.AttemptID, nil); err != nil {
		t.Fatal(err)
	}
	// a body cannot be rewritten or attached to a final attempt
	wantDBError(t, "body on a final attempt", o.execAs(cTenantA, `UPDATE xic_outbound_attempts SET body='\x7b7d'::bytea, body_expires_at=now(), version=version+1 WHERE attempt_id=$1::uuid`, done.AttemptID), "")
	// the worker flag cannot be used by the app role to read without it
	if n := o.countAs("", `SELECT count(*) FROM xic_outbound_attempts`); n != 0 {
		t.Fatalf("%d rows visible with no tenant", n)
	}
}

func TestOutboundPG_ManualResolutionIsGoverned(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.unknown(t, c, "PAY-1")
	m := func(who outbound.Actor, in outbound.ResolveInput) error {
		in.Method = outbound.MethodManual
		_, err := o.svc.Resolve(tenantCtx(cTenantA), who, a.AttemptID, in)
		return err
	}
	if err := m(obOps, outbound.ResolveInput{Outcome: "SETTLED", Reason: "statement"}); !errors.Is(err, outbound.ErrBadRequest) {
		t.Fatalf("%v", err)
	}
	if err := m(obDomain, outbound.ResolveInput{Outcome: "SETTLED", EvidenceRef: "BS-1", Reason: "statement"}); !errors.Is(err, outbound.ErrSelfAction) {
		t.Fatalf("%v", err)
	}
	if err := m(obOps, outbound.ResolveInput{Outcome: "FAILED", EvidenceRef: "BS-1", Reason: "statement shows no debit"}); err != nil {
		t.Fatal(err)
	}
	got := o.get(t, a.AttemptID)
	if got.State != outbound.StateFailed || got.ResolutionMethod != outbound.MethodManual || got.ResolvedBy != "ops-lead" || got.EvidenceRef != "BS-1" || got.BodyRetained {
		t.Fatalf("%+v", got)
	}
	if o.prov.posts() != 1 {
		t.Fatal("a manual resolution submitted")
	}
}

func TestOutboundPG_CrashDuringSubmitIsRecoveredByQuery(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.reserve(t, c, "PAY-1")
	// the worker claimed the attempt (SUBMITTING is committed before any call) and died
	if err := o.execAs(cTenantA, `UPDATE xic_outbound_attempts SET state='SUBMITTING', submit_count=1, last_submit_at=now(), next_resolution_at=now() + interval '2 minutes', version=version+1 WHERE attempt_id=$1::uuid`, a.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.svc.Resolve(tenantCtx(cTenantA), obOps, a.AttemptID, outbound.ResolveInput{}); !errors.Is(err, outbound.ErrTransition) {
		t.Fatalf("inside its lease: %v", err)
	}
	o.advance(10 * time.Minute)
	o.prov.set(func(c obCall) (int, string, error) { return 200, `{"status":"COMPLETED","payment_id":"P-8"}`, nil })
	// the real database clock has not moved, so make the lease expire there too
	if err := o.admin.QueryRow(context.Background(), `SELECT 1`).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if _, err := o.admin.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica; UPDATE xic_outbound_attempts SET next_resolution_at = now() - interval '1 minute'; COMMIT`); err != nil {
		t.Fatal(err)
	}
	got, err := o.svc.Resolve(tenantCtx(cTenantA), obOps, a.AttemptID, outbound.ResolveInput{})
	if err != nil || got.State != outbound.StateSettled || got.ProviderReference != "P-8" {
		t.Fatalf("%v %+v", err, got)
	}
	if o.prov.posts() != 0 {
		t.Fatal("the crashed attempt was submitted again")
	}
	events, _ := o.repo.Events(tenantCtx(cTenantA), a.AttemptID)
	var seen bool
	for _, e := range events {
		if e.Kind == "UNKNOWN" && strings.Contains(string(e.Detail), outbound.ReasonCrashRecovery) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("%+v", events)
	}
}

// ---- tenant isolation and the worker -----------------------------------------------------------------------

func TestOutboundPG_TenantIsolation(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.reserve(t, c, "PAY-1")
	b := tenantCtx(cTenantB)
	if _, err := o.repo.Get(b, a.AttemptID); !errors.Is(err, outbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := o.repo.Events(b, a.AttemptID); !errors.Is(err, outbound.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if l, _ := o.repo.List(b, outbound.Filter{}); len(l) != 0 {
		t.Fatal("listed another tenant's attempts")
	}
	for name, f := range map[string]func() error{
		"submit":  func() error { _, err := o.svc.Submit(b, obDomain, a.AttemptID, nil); return err },
		"resolve": func() error { _, err := o.svc.Resolve(b, obOps, a.AttemptID, outbound.ResolveInput{}); return err },
		"cancel":  func() error { _, err := o.svc.Cancel(b, obOps, a.AttemptID, "x"); return err },
	} {
		if err := f(); !errors.Is(err, outbound.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var ref *xicconn.Refusal
	if _, _, err := o.svc.Reserve(b, obDomain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: "PAY-X", Payload: json.RawMessage(obPay)}); !errors.As(err, &ref) || ref.Code != xicconn.CodeTenantScope {
		t.Fatalf("%v", err)
	}
	for _, tbl := range []string{"xic_outbound_attempts", "xic_outbound_events"} {
		if n := o.countAs(cTenantB, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("tenant B sees %d %s rows", n, tbl)
		}
	}
	o.wantRefused(t, "update", cTenantB, `UPDATE xic_outbound_attempts SET provider_reference='X', version=version+1`)
	o.wantRefused(t, "delete", cTenantB, `DELETE FROM xic_outbound_attempts`)
	// as tenant B, writing a row that claims tenant A (real ids, so only row-level security can refuse it)
	wantDBError(t, "forged attempt", o.execAs(cTenantB, `INSERT INTO xic_outbound_attempts (attempt_id, tenant_id, connection_id, connector_id, operation_id, contract_version_id,
		domain_request_ref, payload_hash, payload_size, requested_by) VALUES (gen_random_uuid(), $1, $2::uuid, 'bank-x', 'payment-submit', $3::uuid, 'FORGED', repeat('a', 64), 1, 'x')`,
		cTenantA, c.ConnectionID, c.ContractVersionID), "row-level security")
	if got := o.get(t, a.AttemptID); got.State != outbound.StateReserved {
		t.Fatalf("%+v", got)
	}
	if o.prov.posts() != 0 {
		t.Fatal("another tenant caused a call")
	}
}

func TestOutboundPG_WorkerSeesDueAttemptsAcrossTenantsAndResolvesOnlyDueOnes(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	due := o.unknown(t, c, "PAY-DUE")
	// the app role without the worker flag sees nothing across tenants
	if n := o.countAs("", `SELECT count(*) FROM xic_outbound_attempts`); n != 0 {
		t.Fatalf("%d", n)
	}
	now := time.Now().UTC()
	refs, err := o.repo.DueForResolution(context.Background(), now, now.Add(-24*time.Hour), 100)
	if err != nil || len(refs) != 0 {
		t.Fatalf("not due yet: %v %+v", err, refs)
	}
	refs, err = o.repo.DueForResolution(context.Background(), now.Add(2*time.Hour), now.Add(-24*time.Hour), 100)
	if err != nil || len(refs) != 1 || refs[0].AttemptID != due.AttemptID || refs[0].TenantID != cTenantA {
		t.Fatalf("%v %+v", err, refs)
	}
	// escalation candidates: open longer than the maximum age even if no resolution is scheduled
	refs, _ = o.repo.DueForResolution(context.Background(), now, now.Add(time.Hour), 100)
	if len(refs) != 1 {
		t.Fatalf("an attempt open past the escalation age must be returned: %+v", refs)
	}
	// the worker's read flag grants SELECT only
	if err := o.execAsWorker(`UPDATE xic_outbound_attempts SET provider_reference='X', version=version+1`); err == nil {
		t.Fatal("the worker role changed an attempt")
	}
}

func TestOutboundPG_BodiesArePurgedWhenNoLongerNeeded(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	open := o.unknown(t, c, "PAY-OPEN")
	if n, _ := o.repo.PurgeBodies(context.Background(), time.Now().UTC(), 100); n != 0 {
		t.Fatalf("purged %d early", n)
	}
	// back-date the retention, as the passage of time would (triggers are disabled for this one superuser statement)
	if _, err := o.admin.Exec(context.Background(), `BEGIN; SET LOCAL session_replication_role = replica; UPDATE xic_outbound_attempts SET body_expires_at = now() - interval '1 minute'; COMMIT`); err != nil {
		t.Fatal(err)
	}
	n, err := o.repo.PurgeBodies(context.Background(), time.Now().UTC(), 100)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got := o.get(t, open.AttemptID)
	if got.BodyRetained || got.PayloadHash != open.PayloadHash || got.State != outbound.StateUnknown {
		t.Fatalf("%+v", got)
	}
	// without a stored body the attempt can still be resolved by query, and a submission needs the body again
	if _, err := o.svc.Submit(tenantCtx(cTenantA), obDomain, open.AttemptID, nil); !errors.Is(err, outbound.ErrUnknownMustBeResolved) {
		t.Fatalf("%v", err)
	}
}

func TestOutboundPG_RunResolverOnATicker(t *testing.T) {
	o := newObEnv(t)
	c := o.ready(t, false)
	a := o.unknown(t, c, "PAY-1")
	o.advance(time.Hour)
	o.prov.set(func(c obCall) (int, string, error) {
		if c.method == http.MethodGet {
			return 200, `{"status":"SETTLED"}`, nil
		}
		return 200, `{}`, nil
	})
	// the store compares against the engine's clock (an hour ahead), so the attempt is due
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.svc.RunResolver(ctx, 20*time.Millisecond, 10, nil); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && o.get(t, a.AttemptID).State != outbound.StateSettled {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if got := o.get(t, a.AttemptID); got.State != outbound.StateSettled {
		t.Fatalf("%+v", got)
	}
}
