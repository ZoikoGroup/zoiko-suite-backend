package inbound_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

const (
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	platform = registry.PlatformScopeID

	refHMAC  = "0a000000-0000-4000-8000-0000000000a2"
	refOAuth = "0a000000-0000-4000-8000-0000000000a1"
	refHMAC2 = "0a000000-0000-4000-8000-0000000000a4"
	refNew   = "0a000000-0000-4000-8000-0000000000a9"
	refOther = "0a000000-0000-4000-8000-0000000000b1"

	secretHMAC = "shared-secret-for-tenant-a"
)

var (
	creator  = xicconn.Actor{PrincipalID: "creator"}
	operator = xicconn.Actor{PrincipalID: "operator"}
)

func ctxFor(t string) context.Context { return middleware.WithTenant(context.Background(), t) }

type fakeVault struct{}

func (fakeVault) Verify(context.Context, string, vault.Caller) error { return nil }

// fakeSecrets releases the material a test registers. down simulates a vault outage.
type fakeSecrets struct {
	mu   sync.Mutex
	m    map[string]string
	down bool
	hits int
}

func (f *fakeSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.down {
		return nil, inbound.ErrSecretUnavailable
	}
	if s, ok := f.m[strings.ToLower(ref)]; ok {
		return []byte(s), nil
	}
	return nil, inbound.ErrAuthFailed
}

func (f *fakeSecrets) set(ref, secret string) {
	f.mu.Lock()
	f.m[strings.ToLower(ref)] = secret
	f.mu.Unlock()
}

type fakeDNS map[string]string

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ip, ok := f[host]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

// connAdapter exposes the connection service to the gateway.
type connAdapter struct{ svc *xicconn.Service }

func (c connAdapter) Connection(ctx context.Context, id string) (*xicconn.Connection, error) {
	return c.svc.Connection(ctx, id)
}
func (c connAdapter) PinnedContract(ctx context.Context, conn *xicconn.Connection) (*registry.ContractVersion, error) {
	return c.svc.PinnedContract(ctx, conn)
}
func (c connAdapter) Operable(ctx context.Context, tenant, id, op string) (*xicconn.Decision, error) {
	return c.svc.Operable(ctx, tenant, id, op)
}

type env struct {
	t         *testing.T
	contracts *registry.MemRepo
	regSvc    *registry.Service
	conns     *xicconn.MemRepo
	connSvc   *xicconn.Service
	repo      *inbound.MemRepo
	secrets   *fakeSecrets
	gw        *inbound.Service
	mu        sync.Mutex
	now       time.Time
	seq       int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, contracts: registry.NewMemRepo(), conns: xicconn.NewMemRepo(), repo: inbound.NewMemRepo(), now: registrytest.T0.Add(72 * time.Hour)}
	clock := func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	e.regSvc = &registry.Service{Repo: e.contracts, Clock: func() time.Time { return registrytest.T0 }}
	e.connSvc = &xicconn.Service{Repo: e.conns, Contracts: e.contracts, Vault: fakeVault{},
		Egress: egress.Policy{Resolver: fakeDNS{"api.bank-x.com": "93.184.216.34"}}, AllowedRegions: []string{"uk"}, Clock: clock}
	e.secrets = &fakeSecrets{m: map[string]string{refHMAC: secretHMAC, refOAuth: "oauth-token", refHMAC2: "secret-for-tenant-b", refNew: "rotated-secret"}}
	e.gw = &inbound.Service{Repo: e.repo, Conns: connAdapter{e.connSvc}, Secrets: e.secrets, Clock: clock,
		Cfg: inbound.DefaultConfig(), Handoff: inbound.HandoffDeps{AllowedRegions: []string{"uk"}}}
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }
func (e *env) at() time.Time           { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

// publish publishes the reference contract with the inbound conveniences a gateway test needs.
func (e *env) publish(mutate func(*registry.VersionInput)) *registry.ContractVersion {
	e.t.Helper()
	pctx := ctxFor(platform)
	if _, err := e.contracts.GetConnector(pctx, "bank-x"); err != nil {
		if _, _, err := e.regSvc.CreateConnector(pctx, platform, registry.Actor{PrincipalID: "author"},
			registry.Connector{ConnectorID: "bank-x", Family: "BANK_FEED", ProviderClass: "Bank X", Owner: "eng"}, ""); err != nil {
			e.t.Fatal(err)
		}
	}
	e.seq++
	in := registrytest.Input("1." + strconv.Itoa(e.seq) + ".0")
	in.EffectiveFrom = registrytest.T0.Add(time.Hour)
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
	// the schema tolerates the optional claim fields
	in.Content.Schemas[0].ValidationProfile = json.RawMessage(`{"type":"object","required":["event_id","account","balance","booked_at"],
	 "properties":{"event_id":{"type":"string"},"account":{"type":"string"},"balance":{"type":"string"},"currency":{"type":"string","enum":["GBP","EUR","USD"]},
	 "booked_at":{"type":"string"},"tenant":{"type":"string"},"holder_account":{"type":"string"},"holder":{"type":"object"},"note":{"type":"string"}}}`)
	if mutate != nil {
		mutate(&in)
	}
	v, _, err := e.regSvc.CreateVersion(pctx, platform, registry.Actor{PrincipalID: "author"}, "bank-x", in, "")
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.regSvc.Validate(pctx, platform, registry.Actor{PrincipalID: "author"}, v.ContractVersionID, v.RowVersion)
	if err != nil {
		e.t.Fatalf("validate: %v", err)
	}
	v, err = e.regSvc.SubmitReview(pctx, platform, registry.Actor{PrincipalID: "author"}, v.ContractVersionID, res.Version.RowVersion)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range []struct {
		who, gate string
		detail    json.RawMessage
	}{{"s", registry.GateSecurity, nil}, {"p", registry.GatePrivacy, nil}, {"q", registry.GateNegativeTests, registrytest.Passed()}, {"d", registry.GateDomainAcceptance, nil}, {"r", registry.GateRollback, nil}} {
		if _, err := e.regSvc.Attest(pctx, platform, registry.Actor{PrincipalID: a.who}, v.ContractVersionID, registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-" + a.gate, Detail: a.detail}); err != nil {
			e.t.Fatal(err)
		}
	}
	cur, _ := e.contracts.GetVersion(pctx, v.ContractVersionID)
	pub, err := e.regSvc.Publish(pctx, platform, registry.Actor{PrincipalID: "release"}, cur.ContractVersionID, cur.RowVersion)
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
	return pub
}

// activeConn returns an ACTIVE sandbox connection of the tenant pinned to v, whose HMAC secret is hmacRef.
func (e *env) activeConn(tenant string, v *registry.ContractVersion, account, hmacRef string) *xicconn.Connection {
	e.t.Helper()
	ctx := ctxFor(tenant)
	c, _, err := e.connSvc.Create(ctx, creator, xicconn.CreateInput{Environment: "SANDBOX", ConnectorID: "bank-x", ContractVersionID: v.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: account}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.connSvc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil {
		e.t.Fatal(err)
	}
	cur := res.Connection
	exp := e.at().Add(60 * 24 * time.Hour)
	for _, b := range []xicconn.BindInput{
		{AuthProfile: "oauth", CredentialRef: uniqueRef(tenant+account, "o"), GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp},
		{AuthProfile: "hmac-in", CredentialRef: hmacRef, ConsentActor: "x"},
	} {
		if cur, err = e.connSvc.Bind(ctx, creator, c.ConnectionID, cur.Version, b); err != nil {
			e.t.Fatal(err)
		}
	}
	vres, err := e.connSvc.Validate(ctx, operator, c.ConnectionID, cur.Version, "")
	if err != nil {
		e.t.Fatal(err)
	}
	act, err := e.connSvc.Activate(ctx, operator, c.ConnectionID, vres.Connection.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	return act
}

func uniqueRef(tenant, kind string) string {
	h := sha256.Sum256([]byte(tenant + kind))
	x := hex.EncodeToString(h[:])
	return x[0:8] + "-" + x[8:12] + "-4" + x[13:16] + "-8" + x[17:20] + "-" + x[20:32]
}

// ---- building deliveries ---------------------------------------------------------------

func goodBody(eventID string) string {
	return `{"event_id":"` + eventID + `","account":"A1","balance":"10.5","currency":"GBP","booked_at":"2026-10-20T10:00:00+01:00","tenant":"` + tenantA + `","holder_account":"acc-a"}`
}

func sign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + body))
	return hex.EncodeToString(m.Sum(nil))
}

type deliv struct {
	conn    *xicconn.Connection
	body    string
	secret  string
	tsOff   time.Duration
	key     string
	headers map[string]string
	op      string
	method  string
	ip      string
	noSig   bool
}

func (e *env) request(d deliv) inbound.Request {
	if d.secret == "" {
		d.secret = secretHMAC
	}
	if d.op == "" {
		d.op = "statement-push"
	}
	if d.method == "" {
		d.method = "POST"
	}
	if d.ip == "" {
		d.ip = "198.51.100.7:4433"
	}
	ts := strconv.FormatInt(e.at().Add(d.tsOff).Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Timestamp", ts)
	if !d.noSig {
		h.Set("X-Signature", sign(d.secret, ts, d.body))
	}
	if d.key != "" {
		h.Set("Idempotency-Key", d.key)
	}
	for k, v := range d.headers {
		if v == "" {
			h.Del(k)
		} else {
			h.Set(k, v)
		}
	}
	return inbound.Request{ConnectorKey: d.conn.ConnectorID, ConnectionID: d.conn.ConnectionID, OperationID: d.op, Method: d.method, Header: h,
		Body: []byte(d.body), RemoteAddr: d.ip, CorrelationID: "corr-1"}
}

func (e *env) deliver(d deliv) *inbound.Outcome {
	return e.gw.Receive(context.Background(), e.request(d))
}

// ok returns a deliv that should be accepted.
func ok(c *xicconn.Connection, event string) deliv {
	return deliv{conn: c, body: goodBody(event), key: "key-" + event}
}

// target registers and activates the handoff target for the reference canonical contract.
func (e *env) target(base string) *inbound.Target {
	e.t.Helper()
	e.gw.Handoff.Check = func(context.Context, string) (string, error) { return "test", nil }
	t, err := e.gw.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "platform-a"}, inbound.CreateTargetInput{
		CanonicalContract: "bank.statement.v1", BaseURL: base, Path: "/v1/statements", Region: "uk"})
	if err != nil {
		e.t.Fatal(err)
	}
	t, err = e.gw.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "platform-b"}, t.TargetID, t.Version)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

func hasType(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func countType(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}
	return n
}

func xicconnRotate(profile, newRef string, graceSeconds int64) xicconn.RotateInput {
	return xicconn.RotateInput{AuthProfile: profile, NewRef: newRef, GraceSeconds: graceSeconds}
}

var (
	creatorX  = xicconn.Actor{PrincipalID: "mig-requester"}
	approverX = xicconn.Actor{PrincipalID: "mig-approver"}
)

func migInput(to string) xicconn.MigrationInput {
	return xicconn.MigrationInput{ToContractVersionID: to, Reason: "adopt the corrected contract"}
}
