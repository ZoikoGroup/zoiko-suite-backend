package batch_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/inbound"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/scan"
)

const (
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	platform = registry.PlatformScopeID

	refHMAC   = "0a000000-0000-4000-8000-0000000000a2"
	refOAuth  = "0a000000-0000-4000-8000-0000000000a1"
	refHMACB  = "0a000000-0000-4000-8000-0000000000b2"
	refOAuthB = "0a000000-0000-4000-8000-0000000000b1"

	secretA = "shared-secret-for-tenant-a"
	secretB = "shared-secret-for-tenant-b"
)

var (
	creator  = xicconn.Actor{PrincipalID: "creator"}
	operator = xicconn.Actor{PrincipalID: "operator"}
	requestr = batch.Actor{PrincipalID: "requester", CorrelationID: "corr-r"}
	approver = batch.Actor{PrincipalID: "approver"}
)

func ctxFor(t string) context.Context { return middleware.WithTenant(context.Background(), t) }

type fakeVault struct{}

func (fakeVault) Verify(context.Context, string, vault.Caller) error { return nil }

type fakeSecrets struct {
	mu   sync.Mutex
	m    map[string]string
	down bool
}

func (f *fakeSecrets) Secret(_ context.Context, _ string, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, inbound.ErrSecretUnavailable
	}
	if s, ok := f.m[strings.ToLower(ref)]; ok {
		return []byte(s), nil
	}
	return nil, inbound.ErrAuthFailed
}

type fakeDNS map[string]string

func (f fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ip, ok := f[host]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

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
	in        *inbound.Service
	inRepo    *inbound.MemRepo
	repo      *batch.MemRepo
	svc       *batch.Service
	secrets   *fakeSecrets
	dom       *httptest.Server
	domHits   int32
	mu        sync.Mutex
	now       time.Time
	seq       int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, contracts: registry.NewMemRepo(), conns: xicconn.NewMemRepo(), inRepo: inbound.NewMemRepo(), repo: batch.NewMemRepo(), now: registrytest.T0.Add(72 * time.Hour)}
	clock := func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
	e.regSvc = &registry.Service{Repo: e.contracts, Clock: func() time.Time { return registrytest.T0 }}
	e.connSvc = &xicconn.Service{Repo: e.conns, Contracts: e.contracts, Vault: fakeVault{},
		Egress: egress.Policy{Resolver: fakeDNS{"api.bank-x.com": "93.184.216.34"}}, AllowedRegions: []string{"uk"}, Clock: clock}
	e.secrets = &fakeSecrets{m: map[string]string{refHMAC: secretA, refOAuth: "oauth-token", refHMACB: secretB, refOAuthB: "oauth-token-b"}}
	e.dom = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		atomic.AddInt32(&e.domHits, 1)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"dom-1"}`))
	}))
	t.Cleanup(e.dom.Close)
	icfg := inbound.DefaultConfig()
	e.in = &inbound.Service{Repo: e.inRepo, Conns: connAdapter{e.connSvc}, Secrets: e.secrets, Clock: clock, Cfg: icfg, Versions: e.contracts,
		Handoff: inbound.HandoffDeps{AllowedRegions: []string{"uk"}, HTTP: e.dom.Client(), Check: func(context.Context, string) (string, error) { return "ok", nil }}}
	chain, err := scan.Require("", true, 64<<20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = &batch.Service{Repo: e.repo, Conns: connAdapter{e.connSvc}, In: e.in, Secrets: e.secrets, Scanner: chain, Cfg: batch.DefaultConfig(), Clock: clock}
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }
func (e *env) at() time.Time           { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

// publish publishes the reference contract with the file operation (and any further change) and returns it.
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
	in.Content = registrytest.WithFileOperation(in.Content)
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
	}{{"s", registry.GateSecurity, nil}, {"p", registry.GatePrivacy, nil}, {"q", registry.GateNegativeTests, registrytest.PassedFor(in.Content)}, {"d", registry.GateDomainAcceptance, nil}, {"r", registry.GateRollback, nil}} {
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

// activeConn returns an ACTIVE sandbox connection of the tenant pinned to v.
func (e *env) activeConn(tenant string, v *registry.ContractVersion, account string) *xicconn.Connection {
	e.t.Helper()
	ctx := ctxFor(tenant)
	hm, oa := refHMAC, refOAuth
	if tenant == tenantB {
		hm, oa = refHMACB, refOAuthB
	}
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
		{AuthProfile: "oauth", CredentialRef: oa, GrantedScopes: []string{"payments.write", "payments.read"}, ConsentActor: "x", ExpiresAt: &exp},
		{AuthProfile: "hmac-in", CredentialRef: hm, ConsentActor: "x"},
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

// target registers and activates the handoff target of the canonical contract the rows map to.
func (e *env) target() {
	e.t.Helper()
	t, err := e.in.CreateTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "platform-a"}, inbound.CreateTargetInput{
		CanonicalContract: "bank.statement.v1", BaseURL: e.dom.URL, Path: "/v1/statements", Region: "uk"})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.in.ApproveTarget(ctxFor(platform), platform, inbound.Actor{PrincipalID: "platform-b"}, t.TargetID, t.Version); err != nil {
		e.t.Fatal(err)
	}
}

// ---- building deliveries -------------------------------------------------------------------------------------

func csvFile(rows ...string) string {
	return "txn_id,account,balance,currency,booked_at\n" + strings.Join(rows, "\n") + "\n"
}

func row(id string) string { return id + ",A1,10.5,GBP,2026-10-20T10:00:00Z" }

func rows(n int, prefix string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = row(fmt.Sprintf("%s%04d", prefix, i+1))
	}
	return out
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.Bytes()
}

type fileSpec struct {
	name        string
	data        []byte
	compression string
	declared    *string // override the declared sha
	size        *int64  // override the declared size
	records     *int
}

type bspec struct {
	conn     *xicconn.Connection
	id       string
	files    []fileSpec
	records  *int
	schema   string
	secret   string
	tsOff    time.Duration
	omitRec  bool
	extra    []batch.DeliveredFile // delivered but not declared
	skip     []string              // declared but not delivered
	manifest []byte                // override the manifest bytes
	op       string
	sigOff   bool
}

func intp(n int) *int { return &n }

func (e *env) delivery(s bspec) batch.Delivery {
	if s.schema == "" {
		s.schema = "stmt-csv@1"
	}
	if s.secret == "" {
		s.secret = secretA
	}
	if s.op == "" {
		s.op = "statement-file"
	}
	m := map[string]any{"batch_id": s.id, "source_system": "bank-x", "schema": s.schema, "file_count": len(s.files), "checksum_algorithm": "SHA-256"}
	if s.records != nil && !s.omitRec {
		m["record_count"] = *s.records
	}
	var mf []map[string]any
	var delivered []batch.DeliveredFile
	for _, f := range s.files {
		sh := sha(f.data)
		if f.declared != nil {
			sh = *f.declared
		}
		size := int64(len(f.data))
		if f.size != nil {
			size = *f.size
		}
		ent := map[string]any{"name": f.name, "size": size, "sha256": sh}
		if f.compression != "" {
			ent["compression"] = f.compression
		}
		if f.records != nil {
			ent["records"] = *f.records
		}
		mf = append(mf, ent)
		skip := false
		for _, k := range s.skip {
			if k == f.name {
				skip = true
			}
		}
		if !skip {
			delivered = append(delivered, batch.DeliveredFile{Name: f.name, Data: f.data})
		}
	}
	m["files"] = mf
	manifest, _ := json.Marshal(m)
	if s.manifest != nil {
		manifest = s.manifest
	}
	delivered = append(delivered, s.extra...)
	ts := strconv.FormatInt(e.at().Add(s.tsOff).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write([]byte(ts + "." + string(manifest)))
	h := http.Header{}
	h.Set("X-Timestamp", ts)
	if !s.sigOff {
		h.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	return batch.Delivery{ConnectorKey: s.conn.ConnectorID, ConnectionID: s.conn.ConnectionID, OperationID: s.op, Channel: batch.ChannelHTTPS, Header: h,
		Manifest: manifest, Files: delivered, RemoteAddr: "198.51.100.7:4433", CorrelationID: "corr-1"}
}

func (e *env) deliver(s bspec) *batch.Outcome {
	return e.svc.Receive(context.Background(), e.delivery(s))
}

// simple returns a one-file spec of n good rows.
func simple(c *xicconn.Connection, id string, n int) bspec {
	data := []byte(csvFile(rows(n, id+"-")...))
	return bspec{conn: c, id: id, records: intp(n), files: []fileSpec{{name: "stmt-0001.csv", data: data}}}
}

// process runs the batch to the point it reports on its rows.
func (e *env) process(b *batch.Batch) *batch.Batch {
	e.t.Helper()
	got, err := e.svc.Process(context.Background(), b.TenantID, b.BatchID)
	if err != nil {
		e.t.Fatalf("process: %v", err)
	}
	return got
}

// settle delivers every handed-off row to the domain and refreshes the batch.
func (e *env) settle(b *batch.Batch) *batch.Batch {
	e.t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := e.in.DriveDue(context.Background(), 1000); err != nil {
			e.t.Fatal(err)
		}
	}
	got, err := e.svc.Refresh(context.Background(), b.TenantID, b.BatchID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *env) get(tenant, id string) *batch.Batch {
	e.t.Helper()
	b, err := e.repo.Get(ctxFor(tenant), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) rowsOf(tenant, id string, states ...string) []batch.Row {
	e.t.Helper()
	r, err := e.repo.Rows(ctxFor(tenant), id, batch.RowFilter{States: states})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
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

type registryInput = registry.VersionInput
