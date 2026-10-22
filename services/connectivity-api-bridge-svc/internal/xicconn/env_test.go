package xicconn_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

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

	refOAuth  = "0a000000-0000-4000-8000-0000000000a1"
	refHMAC   = "0a000000-0000-4000-8000-0000000000a2"
	refOAuth2 = "0a000000-0000-4000-8000-0000000000a3"
	refHMAC2  = "0a000000-0000-4000-8000-0000000000a4"
	refOAuth3 = "0a000000-0000-4000-8000-0000000000a5"
	refOther  = "0a000000-0000-4000-8000-0000000000b1"
)

var (
	creator   = xicconn.Actor{PrincipalID: "creator", CorrelationID: "corr-1"}
	operator  = xicconn.Actor{PrincipalID: "operator"}
	approver  = xicconn.Actor{PrincipalID: "approver"}
	certifier = xicconn.Actor{PrincipalID: "certifier"}
)

func ctxFor(tenant string) context.Context {
	return middleware.WithTenant(context.Background(), tenant)
}

type fakeVault struct {
	mu   sync.Mutex
	refs map[string]error // ref -> nil (good) or the error
	down bool
	hits int
}

func (f *fakeVault) Verify(_ context.Context, ref string, _ vault.Caller) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.down {
		return vault.ErrUnavailable
	}
	if err, ok := f.refs[strings.ToLower(ref)]; ok {
		return err
	}
	return vault.ErrNotFound
}

func (f *fakeVault) set(ref string, err error) {
	f.mu.Lock()
	f.refs[strings.ToLower(ref)] = err
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

type env struct {
	t         *testing.T
	contracts *registry.MemRepo
	regSvc    *registry.Service
	repo      *xicconn.MemRepo
	svc       *xicconn.Service
	vault     *fakeVault
	dns       fakeDNS
	mu        sync.Mutex
	now       time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, contracts: registry.NewMemRepo(), repo: xicconn.NewMemRepo(), now: registrytest.T0.Add(48 * time.Hour)}
	clock := func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	e.regSvc = &registry.Service{Repo: e.contracts, Clock: func() time.Time { return registrytest.T0 }}
	e.vault = &fakeVault{refs: map[string]error{}}
	for _, r := range []string{refOAuth, refHMAC, refOAuth2, refHMAC2, refOAuth3} {
		e.vault.refs[strings.ToLower(r)] = nil
	}
	e.dns = fakeDNS{"api.bank-x.com": "93.184.216.34", "api.bank-y.com": "93.184.216.35"}
	e.svc = &xicconn.Service{
		Repo: e.repo, Contracts: e.contracts, Vault: e.vault, Egress: egress.Policy{Resolver: e.dns},
		AllowedRegions: []string{"uk"}, RedirectAllowlist: []string{"https://app.zoiko.example/oauth/callback"},
		Clock: clock,
	}
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *env) at() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

// publish takes a contract through the registry to PUBLISHED.
func (e *env) publish(connectorID, semver string, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	e.t.Helper()
	pctx := ctxFor(platform)
	if _, err := e.contracts.GetConnector(pctx, connectorID); err != nil {
		if _, _, err := e.regSvc.CreateConnector(pctx, platform, registry.Actor{PrincipalID: "author"},
			registry.Connector{ConnectorID: connectorID, Family: "BANK_FEED", ProviderClass: "Bank X", Owner: "eng"}, ""); err != nil {
			e.t.Fatal(err)
		}
	}
	in := registrytest.Input(semver)
	in.EffectiveFrom = registrytest.T0.Add(time.Hour)
	if mutate != nil {
		mutate(&in)
	}
	v, _, err := e.regSvc.CreateVersion(pctx, platform, registry.Actor{PrincipalID: "author"}, connectorID, in, "")
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
		who    string
		gate   string
		detail json.RawMessage
	}{{"s", registry.GateSecurity, nil}, {"p", registry.GatePrivacy, nil}, {"q", registry.GateNegativeTests, registrytest.Passed()},
		{"d", registry.GateDomainAcceptance, nil}, {"r", registry.GateRollback, nil}} {
		if _, err := e.regSvc.Attest(pctx, platform, registry.Actor{PrincipalID: a.who}, v.ContractVersionID,
			registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-" + a.gate, Detail: a.detail}); err != nil {
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

func (e *env) create(tenant, environment string, v *registry.ContractVersion, account string) *xicconn.Connection {
	e.t.Helper()
	c, _, err := e.svc.Create(ctxFor(tenant), creator, xicconn.CreateInput{
		Environment: environment, ConnectorID: v.ConnectorID, ContractVersionID: v.ContractVersionID, Region: "uk",
		Endpoints: map[string]string{"bank-api": "api.bank-x.com"}, ProviderAccountRef: account}, "")
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return c
}

var oauthScopes = []string{"payments.write", "payments.read"}

// bound drives a connection through authorize and both bindings (pre-provisioned).
func (e *env) bound(tenant string, c *xicconn.Connection, oauthRef, hmacRef string) *xicconn.Connection {
	e.t.Helper()
	ctx := ctxFor(tenant)
	res, err := e.svc.Authorize(ctx, creator, c.ConnectionID, c.Version, xicconn.AuthorizeInput{Mode: "PRE_PROVISIONED"})
	if err != nil {
		e.t.Fatal(err)
	}
	cur := res.Connection
	exp := e.at().Add(60 * 24 * time.Hour)
	if cur, err = e.svc.Bind(ctx, creator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "oauth", CredentialRef: oauthRef,
		GrantedScopes: oauthScopes, ConsentActor: "tenant-admin", ExpiresAt: &exp}); err != nil {
		e.t.Fatal(err)
	}
	if cur, err = e.svc.Bind(ctx, creator, c.ConnectionID, cur.Version, xicconn.BindInput{AuthProfile: "hmac-in", CredentialRef: hmacRef,
		ConsentActor: "tenant-admin"}); err != nil {
		e.t.Fatal(err)
	}
	return cur
}

func (e *env) validated(tenant string, c *xicconn.Connection) *xicconn.Connection {
	e.t.Helper()
	res, err := e.svc.Validate(ctxFor(tenant), operator, c.ConnectionID, c.Version, "")
	if err != nil {
		e.t.Fatalf("validate: %v", err)
	}
	return res.Connection
}

// sandbox returns an ACTIVE sandbox connection.
func (e *env) sandbox(tenant string, v *registry.ContractVersion, account, oauthRef, hmacRef string) *xicconn.Connection {
	e.t.Helper()
	c := e.create(tenant, xicconn.EnvSandbox, v, account)
	c = e.bound(tenant, c, oauthRef, hmacRef)
	c = e.validated(tenant, c)
	c, err := e.svc.Activate(ctxFor(tenant), operator, c.ConnectionID, c.Version)
	if err != nil {
		e.t.Fatalf("activate: %v", err)
	}
	return c
}

func (e *env) probeOK(tenant string, c *xicconn.Connection, account string, v *registry.ContractVersion) {
	e.t.Helper()
	var caps []string
	for _, o := range v.Content.Operations {
		caps = append(caps, o.ID)
	}
	ck, err := e.svc.ReportProbe(ctxFor(tenant), operator, c.ConnectionID, 0, xicconn.ProbeInput{AccountRef: account, Capabilities: caps})
	if err != nil || ck.Outcome != xicconn.Passed {
		e.t.Fatalf("probe: %v %+v", err, ck)
	}
}

func (e *env) certify(tenant string, c *xicconn.Connection) *xicconn.Certification {
	e.t.Helper()
	cert, err := e.svc.Certify(ctxFor(tenant), certifier, c.ConnectionID, xicconn.CertifyInput{
		TestPackVersion: "tp-2026.10", Passed: passedTests()})
	if err != nil {
		e.t.Fatalf("certify: %v", err)
	}
	return cert
}

func passedTests() []string {
	var d struct {
		Passed []string `json:"passed"`
	}
	_ = json.Unmarshal(registrytest.Passed(), &d)
	return d.Passed
}

// production returns an ACTIVE, certified production connection.
func (e *env) production(tenant string, v *registry.ContractVersion, account, oauthRef, hmacRef string) *xicconn.Connection {
	e.t.Helper()
	c := e.create(tenant, xicconn.EnvProduction, v, account)
	c = e.bound(tenant, c, oauthRef, hmacRef)
	c = e.validated(tenant, c)
	e.probeOK(tenant, c, account, v)
	e.certify(tenant, c)
	cur, _ := e.repo.Get(ctxFor(tenant), c.ConnectionID)
	c, err := e.svc.Activate(ctxFor(tenant), operator, c.ConnectionID, cur.Version)
	if err != nil {
		e.t.Fatalf("activate production: %v", err)
	}
	return c
}

func (e *env) get(tenant string, c *xicconn.Connection) *xicconn.Connection {
	e.t.Helper()
	cur, err := e.repo.Get(ctxFor(tenant), c.ConnectionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return cur
}

func hasEvent(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func issuePaths(err error) string {
	var v *xicconn.Validation
	if e, ok := err.(*xicconn.Validation); ok {
		v = e
	}
	if v == nil {
		return ""
	}
	var ps []string
	for _, i := range v.Issues {
		ps = append(ps, i.Path)
	}
	return strings.Join(ps, ",")
}

func registrytest_input(semver string) registry.VersionInput { return registrytest.Input(semver) }
func registrytest_t0() time.Time                             { return registrytest.T0 }
