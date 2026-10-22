package outbound_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/outbound"
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

	refAPIKey = "0a000000-0000-4000-8000-0000000000c1"
	refHMAC   = "0a000000-0000-4000-8000-0000000000c2"

	apiKey = "provider-api-key-value"
	payRef = `{"amount":"10.00","currency":"GBP"}`
)

var (
	creator  = xicconn.Actor{PrincipalID: "creator"}
	operator = xicconn.Actor{PrincipalID: "operator"}
	domain   = outbound.Actor{PrincipalID: "domain-svc", CorrelationID: "corr-1"}
	other    = outbound.Actor{PrincipalID: "ops-lead"}
)

func ctxFor(t string) context.Context { return middleware.WithTenant(context.Background(), t) }

type fakeVault struct{}

func (fakeVault) Verify(context.Context, string, vault.Caller) error { return nil }

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
		return nil, outbound.ErrSecretUnavailable
	}
	if s, ok := f.m[strings.ToLower(ref)]; ok {
		return []byte(s), nil
	}
	return nil, outbound.ErrBlocked
}

type fakeDNS struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ip, ok := f.m[host]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

func (f *fakeDNS) set(host, ip string) { f.mu.Lock(); f.m[host] = ip; f.mu.Unlock() }

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

// ---- the scripted provider ---------------------------------------------------------------

type call struct {
	method, url string
	header      http.Header
	body        []byte
}

// provider is the external system. fn decides each answer; a nil fn answers 200 with {"payment_id":"P-1"}.
type provider struct {
	mu    sync.Mutex
	calls []call
	fn    func(c call) (int, string, error)
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func (p *provider) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	c := call{method: r.Method, url: r.URL.String(), header: r.Header.Clone(), body: b}
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

func (p *provider) set(fn func(c call) (int, string, error)) { p.mu.Lock(); p.fn = fn; p.mu.Unlock() }

func (p *provider) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

func (p *provider) posts() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []call
	for _, c := range p.calls {
		if c.method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

func (p *provider) gets() []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []call
	for _, c := range p.calls {
		if c.method == http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

func answer(status int, body string) func(call) (int, string, error) {
	return func(call) (int, string, error) { return status, body, nil }
}

func failWith(err error) func(call) (int, string, error) {
	return func(call) (int, string, error) { return 0, "", err }
}

// ---- the environment -------------------------------------------------------------------

type variant struct {
	callback      bool // payment-submit declares a callback profile (a 2xx is then PENDING)
	resubmit      bool // the provider's idempotency key makes a resubmission safe
	personal      bool // the payload class forbids storing the request body
	hmacOut       bool // the outbound auth profile is HMAC signing instead of an API key
	noStatusQuery bool
}

type env struct {
	t         *testing.T
	contracts *registry.MemRepo
	regSvc    *registry.Service
	conns     *xicconn.MemRepo
	connSvc   *xicconn.Service
	repo      *outbound.MemRepo
	secrets   *fakeSecrets
	dns       *fakeDNS
	prov      *provider
	svc       *outbound.Service
	mu        sync.Mutex
	now       time.Time
	seq       int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, contracts: registry.NewMemRepo(), conns: xicconn.NewMemRepo(), repo: outbound.NewMemRepo(), prov: &provider{},
		dns: &fakeDNS{m: map[string]string{"api.bank-x.com": "93.184.216.34"}}, now: registrytest.T0.Add(72 * time.Hour)}
	clock := func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	pol := egress.Policy{Resolver: e.dns}
	e.regSvc = &registry.Service{Repo: e.contracts, Clock: func() time.Time { return registrytest.T0 }}
	e.connSvc = &xicconn.Service{Repo: e.conns, Contracts: e.contracts, Vault: fakeVault{}, Egress: pol, AllowedRegions: []string{"uk"}, Clock: clock}
	e.secrets = &fakeSecrets{m: map[string]string{refAPIKey: apiKey, refHMAC: "hmac-secret-material"}}
	e.svc = &outbound.Service{Repo: e.repo, Conns: connAdapter{e.connSvc}, Secrets: e.secrets, Egress: pol, Cfg: outbound.DefaultConfig(), Clock: clock,
		HTTP: &http.Client{Transport: e.prov}}
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }
func (e *env) at() time.Time           { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

// publish publishes the reference contract with outbound calls carried by an API key (or HMAC signing).
func (e *env) publish(v variant, mutate func(*registry.VersionInput)) *registry.ContractVersion {
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
	spec := `{"mechanism":"API_KEY_HEADER","credential_class":"PROVIDER_CREDENTIAL","api_key_header":"X-Api-Key"}`
	if v.hmacOut {
		spec = `{"mechanism":"HMAC","signature_algorithm":"HMAC-SHA256","credential_class":"PROVIDER_CREDENTIAL","signature_header":"X-Signature","timestamp_header":"X-Timestamp"}`
	}
	in.Content.Profiles = append(in.Content.Profiles, registry.Profile{Kind: registry.ProfileAuth, Name: "api-out", Spec: json.RawMessage(spec)})
	for i := range in.Content.Operations {
		o := &in.Content.Operations[i]
		switch o.ID {
		case "payment-submit":
			o.AuthProfile = "api-out"
			o.ResubmitAfterUnknown = v.resubmit
			o.DataClasses = []string{"FINANCIAL"}
			if v.personal {
				o.DataClasses = []string{"FINANCIAL", "PERSONAL"}
			}
			if !v.callback {
				o.CallbackProfile = ""
			}
			o.UnknownResolution = registry.UnknownResolution{ViaCallback: v.callback, ViaReconciliation: v.noStatusQuery}
			if !v.noStatusQuery {
				o.UnknownResolution.StatusQueryOperation = "payment-status"
			}
		case "payment-status":
			o.AuthProfile = "api-out"
		}
	}
	if mutate != nil {
		mutate(&in)
	}
	cv, _, err := e.regSvc.CreateVersion(pctx, platform, registry.Actor{PrincipalID: "author"}, "bank-x", in, "")
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.regSvc.Validate(pctx, platform, registry.Actor{PrincipalID: "author"}, cv.ContractVersionID, cv.RowVersion)
	if err != nil {
		e.t.Fatalf("validate: %v", err)
	}
	cv, err = e.regSvc.SubmitReview(pctx, platform, registry.Actor{PrincipalID: "author"}, cv.ContractVersionID, res.Version.RowVersion)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range []struct {
		who, gate string
		detail    json.RawMessage
	}{{"s", registry.GateSecurity, nil}, {"p", registry.GatePrivacy, nil}, {"q", registry.GateNegativeTests, registrytest.Passed()}, {"d", registry.GateDomainAcceptance, nil}, {"r", registry.GateRollback, nil}} {
		if _, err := e.regSvc.Attest(pctx, platform, registry.Actor{PrincipalID: a.who}, cv.ContractVersionID, registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-" + a.gate, Detail: a.detail}); err != nil {
			e.t.Fatal(err)
		}
	}
	cur, _ := e.contracts.GetVersion(pctx, cv.ContractVersionID)
	pub, err := e.regSvc.Publish(pctx, platform, registry.Actor{PrincipalID: "release"}, cur.ContractVersionID, cur.RowVersion)
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
	return pub
}

func uniqueRef(tenant, kind string) string {
	h := sha256.Sum256([]byte(tenant + kind))
	x := hex.EncodeToString(h[:])
	return x[0:8] + "-" + x[8:12] + "-4" + x[13:16] + "-8" + x[17:20] + "-" + x[20:32]
}

// activeConn returns an ACTIVE sandbox connection of the tenant pinned to v, whose outbound credential is outRef.
func (e *env) activeConn(tenant string, v *registry.ContractVersion, account, outRef string) *xicconn.Connection {
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
		{AuthProfile: "api-out", CredentialRef: outRef, ConsentActor: "x", ExpiresAt: &exp},
		{AuthProfile: "hmac-in", CredentialRef: uniqueRef(tenant+account, "h"), ConsentActor: "x"},
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

// setup publishes the variant and returns an active connection of tenant A.
func (e *env) setup(v variant) *xicconn.Connection {
	e.t.Helper()
	cv := e.publish(v, nil)
	return e.activeConn(tenantA, cv, "acc-a", refAPIKey)
}

func (e *env) reserve(c *xicconn.Connection, ref string) *outbound.Attempt {
	e.t.Helper()
	a, created, err := e.svc.Reserve(ctxFor(c.TenantID), domain, outbound.ReserveInput{ConnectionID: c.ConnectionID, OperationID: "payment-submit", DomainRequestRef: ref, Payload: json.RawMessage(payRef)})
	if err != nil {
		e.t.Fatalf("reserve: %v", err)
	}
	if !created {
		e.t.Fatalf("reserve %s was not created", ref)
	}
	return a
}

func (e *env) get(tenant, id string) *outbound.Attempt {
	e.t.Helper()
	a, err := e.repo.Get(ctxFor(tenant), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
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

var _ = errors.New

func newHMAC(secret string) hash.Hash { return hmac.New(sha256.New, []byte(secret)) }
func hexOf(b []byte) string           { return hex.EncodeToString(b) }
