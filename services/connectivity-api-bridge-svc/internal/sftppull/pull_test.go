package sftppull_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"zoiko.io/connectivity-api-bridge-svc/internal/batch"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/sftppull"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
	"zoiko.io/eventing/egress"
)

const (
	tenant = "tenant-a"
	connID = "11111111-1111-1111-1111-111111111111"
	host   = "files.bank.example.com"
)

// ---- an in-process SFTP server ------------------------------------------------------------------------------

type server struct {
	ln      net.Listener
	hostKey ssh.Signer
	handler sftp.Handlers
	user    string
	pass    string
}

func newServer(t *testing.T) *server {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ln: ln, hostKey: signer, handler: sftp.InMemHandler(), user: "bank", pass: "s3cret"}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == s.user && string(pw) == s.pass {
			return nil, nil
		}
		return nil, errAuth
	}}
	cfg.AddHostKey(signer)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(nc, cfg)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

type authErr struct{}

func (authErr) Error() string { return "denied" }

var errAuth = authErr{}

func (s *server) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				ok := r.Type == "subsystem" && len(r.Payload) >= 4 && string(r.Payload[4:]) == "sftp"
				_ = r.Reply(ok, nil)
				if ok {
					rs := sftp.NewRequestServer(ch, s.handler)
					_ = rs.Serve()
					_ = ch.Close()
					return
				}
			}
		}()
	}
}

func (s *server) fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

// put seeds a file on the server through a client of its own.
func (s *server) client(t *testing.T) *sftp.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", s.ln.Addr().String(), &ssh.ClientConfig{User: s.user, Auth: []ssh.AuthMethod{ssh.Password(s.pass)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close(); _ = c.Close() })
	return cl
}

func (s *server) put(t *testing.T, name string, data []byte) {
	t.Helper()
	cl := s.client(t)
	_ = cl.MkdirAll(name[:strings.LastIndex(name, "/")])
	f, err := cl.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func (s *server) list(t *testing.T, dir string) []string {
	t.Helper()
	es, err := s.client(t).ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// ---- fakes for what the puller depends on -----------------------------------------------------------------

type resolver struct{ ip string }

func (r resolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
}

type fakeConns struct {
	v        *registry.ContractVersion
	operable error
}

func (f *fakeConns) Connection(context.Context, string) (*xicconn.Connection, error) {
	return &xicconn.Connection{ConnectionID: connID, TenantID: tenant, ConnectorID: "bank-x", Status: xicconn.StatusActive}, nil
}
func (f *fakeConns) PinnedContract(context.Context, *xicconn.Connection) (*registry.ContractVersion, error) {
	return f.v, nil
}
func (f *fakeConns) Operable(_ context.Context, _, _, op string) (*xicconn.Decision, error) {
	if f.operable != nil {
		return nil, f.operable
	}
	return &xicconn.Decision{ConnectionID: connID, TenantID: tenant, Host: host, CredentialRefs: []xicconn.CredentialUse{{Ref: "vault://cred/1"}}}, nil
}

type fakeSource struct{}

func (fakeSource) LiveRefs(context.Context, int) ([]xicconn.DueRef, error) {
	return []xicconn.DueRef{{TenantID: tenant, ConnectionID: connID}}, nil
}

type fakeSecrets struct {
	mu    sync.Mutex
	calls int
	body  string
}

func (f *fakeSecrets) Secret(context.Context, string, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return []byte(f.body), nil
}

type taken struct {
	manifest string
	files    map[string][]byte
}

type fakeIngest struct {
	mu     sync.Mutex
	got    []taken
	status int
	dup    bool
}

func (f *fakeIngest) IngestPulled(_ context.Context, _ *xicconn.Connection, _ *registry.ContractVersion, _ *registry.Operation, m []byte, files []batch.DeliveredFile) *batch.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := taken{manifest: string(m), files: map[string][]byte{}}
	for _, x := range files {
		t.files[x.Name] = x.Data
	}
	f.got = append(f.got, t)
	st := f.status
	if st == 0 {
		st = 202
	}
	return &batch.Outcome{Status: st, Duplicate: f.dup}
}

type env struct {
	srv    *server
	conns  *fakeConns
	sec    *fakeSecrets
	ing    *fakeIngest
	p      *sftppull.Puller
	seen   []string
	seenMu sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := newServer(t)
	spec, _ := json.Marshal(map[string]any{"schemes": []string{"sftp"}, "hosts": []string{host}, "regions": []string{"uk"}, "redirects": "NONE", "host_keys": []string{srv.fingerprint()}})
	v := &registry.ContractVersion{Content: registry.Content{
		Profiles:   []registry.Profile{{Kind: registry.ProfileEndpoint, Name: "sftp-ep", Spec: spec}},
		Operations: []registry.Operation{{ID: "statement-file", Direction: registry.Inbound, Channel: "SFTP", EndpointProfile: "sftp-ep"}, {ID: "other", Direction: registry.Inbound, Channel: "WEBHOOK"}}}}
	e := &env{srv: srv, conns: &fakeConns{v: v}, sec: &fakeSecrets{body: `{"username":"bank","password":"s3cret"}`}, ing: &fakeIngest{}}
	e.p = &sftppull.Puller{Conns: e.conns, Source: fakeSource{}, Batches: e.ing, Secrets: e.sec,
		Egress: egress.Policy{AllowedPorts: []int{22}, Resolver: resolver{ip: "93.184.216.34"}},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != net.JoinHostPort(host, "22") {
				t.Errorf("dialed %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, srv.ln.Addr().String())
		},
		Observe: func(kind, reason string) {
			e.seenMu.Lock()
			e.seen = append(e.seen, kind+":"+reason)
			e.seenMu.Unlock()
		},
		Clock: func() time.Time { return time.Unix(1_800_000_000, 0) }}
	return e
}

func (e *env) reasons() string {
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	return strings.Join(e.seen, ",")
}

func (e *env) drop(t *testing.T, dir string, files map[string]string, withManifest bool) {
	t.Helper()
	var names []map[string]any
	for n, c := range files {
		e.srv.put(t, "inbox/"+dir+"/"+n, []byte(c))
		names = append(names, map[string]any{"name": n})
	}
	if withManifest {
		m, _ := json.Marshal(map[string]any{"batch_id": dir, "files": names})
		e.srv.put(t, "inbox/"+dir+"/manifest.json", m)
	}
}

// ---- tests --------------------------------------------------------------------------------------------------

func TestPull_TakesACompleteBatchAndMovesItAside(t *testing.T) {
	e := newEnv(t)
	e.drop(t, "B-1", map[string]string{"stmt-0001.csv": "a,b\n1,2\n"}, true)
	e.drop(t, "B-2", map[string]string{"stmt-0001.csv": "still being written"}, false) // no manifest yet
	n, err := e.p.PollOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if len(e.ing.got) != 1 || string(e.ing.got[0].files["stmt-0001.csv"]) != "a,b\n1,2\n" || !strings.Contains(e.ing.got[0].manifest, `"B-1"`) {
		t.Fatalf("%+v", e.ing.got)
	}
	in := e.srv.list(t, "inbox")
	if len(in) != 1 || in[0] != "B-2" {
		t.Fatalf("inbox: %v", in)
	}
	done := e.srv.list(t, "processed")
	if len(done) != 1 || !strings.HasPrefix(done[0], "B-1.taken.") {
		t.Fatalf("processed: %v", done)
	}
	// a second poll takes nothing more: the batch is not read twice
	if n, _ := e.p.PollOnce(context.Background()); n != 0 || len(e.ing.got) != 1 {
		t.Fatalf("%d %d", n, len(e.ing.got))
	}
	// and once the provider completes the other batch it is taken
	e.srv.put(t, "inbox/B-2/manifest.json", []byte(`{"batch_id":"B-2","files":[{"name":"stmt-0001.csv"}]}`))
	if n, _ := e.p.PollOnce(context.Background()); n != 1 || len(e.ing.got) != 2 {
		t.Fatalf("%d %d", n, len(e.ing.got))
	}
}

func TestPull_AHostKeyThePinDoesNotListIsRefusedAndNoCredentialIsOfferedAfterIt(t *testing.T) {
	e := newEnv(t)
	other := newServer(t) // a different host key than the contract pinned
	e.p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, other.ln.Addr().String())
	}
	other.put(t, "inbox/B-1/stmt-0001.csv", []byte("x"))
	other.put(t, "inbox/B-1/manifest.json", []byte(`{"files":[{"name":"stmt-0001.csv"}]}`))
	n, err := e.p.PollOnce(context.Background())
	if err == nil || n != 0 || len(e.ing.got) != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if !strings.Contains(e.reasons(), "pull_failed:CONNECT") {
		t.Fatal(e.reasons())
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatal("the credential leaked into the error")
	}
}

func TestPull_ADestinationEgressPolicyRefusesIsNeverDialedAndNoCredentialIsFetched(t *testing.T) {
	e := newEnv(t)
	e.p.Egress = egress.Policy{AllowedPorts: []int{22}, Resolver: resolver{ip: "10.0.0.5"}}
	e.p.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialed a refused destination")
		return nil, nil
	}
	n, err := e.p.PollOnce(context.Background())
	if err == nil || n != 0 || e.sec.calls != 0 {
		t.Fatalf("%d %v calls=%d", n, err, e.sec.calls)
	}
	// port 22 is not allowed by default policy either
	e.p.Egress = egress.Policy{Resolver: resolver{ip: "93.184.216.34"}}
	if _, err := e.p.PollOnce(context.Background()); err == nil || e.sec.calls != 0 {
		t.Fatalf("%v calls=%d", err, e.sec.calls)
	}
	if !strings.Contains(e.reasons(), "pull_refused:EGRESS_DENIED") {
		t.Fatal(e.reasons())
	}
}

func TestPull_AWrongPasswordOrABadCredentialShapeTakesNothing(t *testing.T) {
	e := newEnv(t)
	e.drop(t, "B-1", map[string]string{"stmt-0001.csv": "x"}, true)
	e.sec.body = `{"username":"bank","password":"wrong"}`
	if n, err := e.p.PollOnce(context.Background()); err == nil || n != 0 || len(e.ing.got) != 0 {
		t.Fatalf("%d %v", n, err)
	}
	e.sec.body = `not json`
	if n, err := e.p.PollOnce(context.Background()); err == nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	e.sec.body = `{"username":"bank"}`
	if n, err := e.p.PollOnce(context.Background()); err == nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if !strings.Contains(e.reasons(), "CREDENTIAL_SHAPE") {
		t.Fatal(e.reasons())
	}
}

func TestPull_ATransientRefusalLeavesTheBatchAndAFinalOneMovesItWithItsOutcome(t *testing.T) {
	e := newEnv(t)
	e.drop(t, "B-1", map[string]string{"stmt-0001.csv": "x"}, true)
	e.ing.status = 503
	if n, _ := e.p.PollOnce(context.Background()); n != 0 {
		t.Fatalf("%d", n)
	}
	if in := e.srv.list(t, "inbox"); len(in) != 1 {
		t.Fatalf("%v", in)
	}
	e.ing.status = 422 // refused with evidence: not read again
	if n, _ := e.p.PollOnce(context.Background()); n != 1 {
		t.Fatalf("%d", n)
	}
	if d := e.srv.list(t, "processed"); len(d) != 1 || !strings.HasPrefix(d[0], "B-1.rejected.") {
		t.Fatalf("%v", d)
	}
	e.drop(t, "B-2", map[string]string{"stmt-0001.csv": "y"}, true)
	e.ing.status, e.ing.dup = 200, true
	if n, _ := e.p.PollOnce(context.Background()); n != 1 {
		t.Fatalf("%d", n)
	}
}

func TestPull_OnlyFilesTheManifestNamesAreOpenedAndAnOversizeFileIsSeenAsOne(t *testing.T) {
	e := newEnv(t)
	e.p.Cfg = sftppull.Config{MaxFileBytes: 16}
	e.srv.put(t, "inbox/B-1/stmt-0001.csv", []byte(strings.Repeat("z", 100)))
	e.srv.put(t, "inbox/B-1/unlisted.csv", []byte("secret"))
	e.srv.put(t, "inbox/B-1/manifest.json", []byte(`{"files":[{"name":"stmt-0001.csv"},{"name":"../../etc/passwd"},{"name":".hidden"},{"name":"sub/x.csv"}]}`))
	if n, err := e.p.PollOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	g := e.ing.got[0]
	if len(g.files) != 1 {
		t.Fatalf("%v", g.files)
	}
	if l := len(g.files["stmt-0001.csv"]); l != 17 { // the cap plus one byte: enough for the engine to see it exceeds its limit
		t.Fatalf("read %d bytes", l)
	}
}

func TestPull_ConnectionsThatMayNotRunTheOperationAreNotPulled(t *testing.T) {
	e := newEnv(t)
	e.drop(t, "B-1", map[string]string{"stmt-0001.csv": "x"}, true)
	e.conns.operable = context.DeadlineExceeded
	if n, err := e.p.PollOnce(context.Background()); err != nil || n != 0 || e.sec.calls != 0 {
		t.Fatalf("%d %v", n, err)
	}
	e.conns.operable = nil
	// no pinned host key: not configured, never trusted on first use
	e.conns.v.Content.Profiles[0].Spec = json.RawMessage(`{"schemes":["sftp"],"hosts":["` + host + `"]}`)
	if n, err := e.p.PollOnce(context.Background()); err != nil || n != 0 || e.sec.calls != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if !strings.Contains(e.reasons(), "pull_refused:NOT_CONFIGURED") {
		t.Fatal(e.reasons())
	}
}
