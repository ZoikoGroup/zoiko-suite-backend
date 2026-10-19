package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/inbox"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/whout"
)

// ── fakes for the two outside dependencies ──────────────────────────────────

// whKeys fakes the vault's key-reference confirmation.
type whKeys struct {
	mu       sync.Mutex
	known    map[string]bool
	err      error
	verified []string
}

func (k *whKeys) Verify(_ context.Context, path string, c whout.VaultCaller) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.verified = append(k.verified, c.PrincipalID+"|"+path)
	if k.err != nil {
		return k.err
	}
	if !k.known[path] {
		return whout.ErrKeyUnavailable
	}
	return nil
}

// whVault fakes the vault's sign operation with real HMACs, so the destination can
// verify them for real.
type whVault struct {
	mu    sync.Mutex
	keys  map[string][]byte
	fail  error
	calls []string
}

func (v *whVault) Sign(_ context.Context, tenantID, path, requestID, _ string, msg []byte) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, tenantID+"|"+path+"|"+requestID)
	if v.fail != nil {
		return "", v.fail
	}
	key, ok := v.keys[path]
	if !ok {
		return "", whout.ErrSignerUnavailable
	}
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil)), nil
}

func (v *whVault) callCount() int { v.mu.Lock(); defer v.mu.Unlock(); return len(v.calls) }

// whDNS is a resolver that can be flipped mid-test (DNS rebinding).
type whDNS struct {
	mu sync.Mutex
	m  map[string][]string
}

func (d *whDNS) set(host string, ips ...string) { d.mu.Lock(); d.m[host] = ips; d.mu.Unlock() }
func (d *whDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ips, ok := d.m[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

// ── the destination ─────────────────────────────────────────────────────────

type whRequest struct {
	Header http.Header
	Body   []byte
	At     time.Time
}

// whDest is a real TLS HTTP server standing in for a tenant's endpoint.
type whDest struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []whRequest
	status  func(n int, r whRequest) (code int, hdr map[string]string)
	delay   time.Duration
	cur     int32
	maxConc int32
}

func newWhDest(t *testing.T) *whDest {
	d := &whDest{}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&d.cur, 1)
		for {
			m := atomic.LoadInt32(&d.maxConc)
			if c <= m || atomic.CompareAndSwapInt32(&d.maxConc, m, c) {
				break
			}
		}
		defer atomic.AddInt32(&d.cur, -1)
		body := make([]byte, 0, 1024)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		req := whRequest{Header: r.Header.Clone(), Body: body, At: time.Now()}
		d.mu.Lock()
		d.reqs = append(d.reqs, req)
		n := len(d.reqs)
		status := d.status
		delay := d.delay
		d.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		code, hdr := 200, map[string]string(nil)
		if status != nil {
			code, hdr = status(n, req)
		}
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *whDest) setStatus(f func(n int, r whRequest) (int, map[string]string)) {
	d.mu.Lock()
	d.status = f
	d.mu.Unlock()
}

func (d *whDest) requests() []whRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]whRequest(nil), d.reqs...)
}

func (d *whDest) count() int { return len(d.requests()) }

// client trusts the test server's certificate and always connects to it, whatever the
// URL's host says (the URL host is what egress policy and TLS verification judge).
func (d *whDest) client(trust bool) *http.Client {
	base := d.srv.Client().Transport.(*http.Transport).Clone()
	if !trust {
		base.TLSClientConfig = nil
	}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, d.srv.Listener.Addr().String())
	}
	return &http.Client{Transport: base, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ── observer ────────────────────────────────────────────────────────────────

type whObs struct {
	mu       sync.Mutex
	attempts map[whout.AttemptOutcome]int
	dead     map[string]int
	control  int
	breaker  int
}

func (o *whObs) Attempt(oc whout.AttemptOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.attempts == nil {
		o.attempts = map[whout.AttemptOutcome]int{}
	}
	o.attempts[oc]++
}
func (o *whObs) DeadLetter(r string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dead == nil {
		o.dead = map[string]int{}
	}
	o.dead[r]++
}
func (o *whObs) ControlFailure()  { o.mu.Lock(); o.control++; o.mu.Unlock() }
func (o *whObs) BreakerDeferred() { o.mu.Lock(); o.breaker++; o.mu.Unlock() }

// ── the environment ─────────────────────────────────────────────────────────

const (
	whMaker   = "maker-1"
	whChecker = "checker-1"
	whHost    = "example.com" // the host in the httptest certificate
	whURL     = "https://example.com/hooks/zoiko"
)

var whKey1, whKey2 = []byte("whkey-one-whkey-one-whkey-one-1!"), []byte("whkey-two-whkey-two-whkey-two-2!")

type whEnv struct {
	t      *testing.T
	app    *pgxpool.Pool
	admin  *pgxpool.Pool
	st     *store.PgStore
	keys   *whKeys
	vault  *whVault
	dns    *whDNS
	dest   *whDest
	clk    *clock
	obs    *whObs
	svc    *whout.Service
	disp   *whout.Dispatcher
	cons   *whout.Consumer
	tenant string
	cfg    whout.DispatcherConfig
}

type whOpts struct {
	cfg          func(*whout.DispatcherConfig)
	trustTLS     *bool
	allowRegions []string
}

func newWhEnv(t *testing.T, o ...whOpts) *whEnv {
	t.Helper()
	app, admin := ncdPools(t)
	// A previous test's queue must not be claimed by this test's dispatcher.
	if _, err := admin.Exec(context.Background(), `UPDATE webhook_deliveries SET status = 'DEAD_LETTERED', dead_letter_reason = 'TEST_CLEANUP',
		dead_lettered_at = now(), locked_until = NULL WHERE status IN ('PENDING', 'RETRYING', 'SENT')`); err != nil {
		t.Fatal(err)
	}
	// Other tests in this package drop and recreate tables (event_outbox among them) through
	// their own harness, which removes this role's grants on the recreated tables. Assert
	// them again so this suite does not depend on test order.
	if _, err := admin.Exec(context.Background(), `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+ncdAppRole+`;
		GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO `+ncdAppRole); err != nil {
		t.Fatal(err)
	}
	var opt whOpts
	if len(o) > 0 {
		opt = o[0]
	}
	e := &whEnv{
		t: t, app: app, admin: admin, st: store.New(app),
		keys:   &whKeys{known: map[string]bool{"kv/webhooks/erp": true, "kv/webhooks/erp-2": true}},
		vault:  &whVault{keys: map[string][]byte{"kv/webhooks/erp": whKey1, "kv/webhooks/erp-2": whKey2}},
		dns:    &whDNS{m: map[string][]string{whHost: {"93.184.216.34"}}},
		dest:   newWhDest(t),
		clk:    &clock{t: time.Now().UTC().Add(time.Hour)}, // ahead of any database clock skew
		obs:    &whObs{},
		tenant: "tenant-wh-" + uuid.NewString()[:8],
	}
	regions := opt.allowRegions
	if regions == nil {
		regions = []string{"uk", "eu"}
	}
	policy := egress.Policy{Resolver: e.dns}
	svc, err := whout.NewService(whout.ServiceConfig{Store: e.st, Keys: e.keys, Egress: policy, AllowedRegions: regions, Now: e.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.cfg = whout.DefaultDispatcherConfig()
	e.cfg.RequestTimeout = 3 * time.Second
	e.cfg.Retry = whout.RetryPolicy{Base: time.Minute, Cap: time.Hour}
	if opt.cfg != nil {
		opt.cfg(&e.cfg)
	}
	trust := opt.trustTLS == nil || *opt.trustTLS
	disp, err := whout.NewDispatcher(whout.DispatcherDeps{
		Store: e.st, Signing: e.vault, Egress: policy, Client: e.dest.client(trust), AllowedRegions: regions,
		Log: zap.NewNop(), Observer: e.obs, Now: e.clk.now, Jitter: func() float64 { return 0.999 },
	}, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.disp = disp
	in, err := inbox.New(inbox.PoolRunner{Pool: app, TenantSetting: "app.tenant_id"}, whout.ConsumerName, inbox.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	e.cons = whout.NewConsumer(zap.NewNop(), e.st, in)
	return e
}

func (e *whEnv) actor(p string) whout.Actor {
	return whout.Actor{TenantID: e.tenant, PrincipalID: p, CorrelationID: "corr-" + p}
}

func (e *whEnv) spec(mut ...func(*whout.Spec)) whout.Spec {
	s := whout.Spec{
		LegalEntityID: "le-1", Name: "ERP feed", DestinationURL: whURL, EventTypes: []string{"invoice.paid"},
		AllowedFields: []string{"invoice_id", "amount"}, SigningKeyPath: "kv/webhooks/erp", DestinationRegion: "uk", MaxAttempts: 3,
	}
	for _, m := range mut {
		m(&s)
	}
	return s
}

// draft creates a DRAFT; pending submits it; active approves it as a different person.
func (e *whEnv) draft(mut ...func(*whout.Spec)) *whout.Subscription {
	e.t.Helper()
	s, err := e.svc.Create(context.Background(), e.actor(whMaker), e.spec(mut...))
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return s
}

func (e *whEnv) pending(mut ...func(*whout.Spec)) *whout.Subscription {
	e.t.Helper()
	d := e.draft(mut...)
	s, err := e.svc.Submit(context.Background(), e.actor(whMaker), d.SubscriptionID, d.Version)
	if err != nil {
		e.t.Fatalf("submit: %v", err)
	}
	return s
}

func (e *whEnv) active(mut ...func(*whout.Spec)) *whout.Subscription {
	e.t.Helper()
	p := e.pending(mut...)
	s, err := e.svc.Approve(context.Background(), e.actor(whChecker), p.SubscriptionID, p.Version, "verified with the customer")
	if err != nil {
		e.t.Fatalf("approve: %v", err)
	}
	return s
}

func whEventJSON(id, typ, tenant string, at time.Time, payload map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"event_id": id, "event_type": typ, "tenant_id": tenant, "emitted_at": at.UTC().Format(time.RFC3339Nano),
		"schema_version": "1", "source_service": "invoicing", "payload": payload,
	})
	return b
}

func (e *whEnv) emit(id, typ string, payload map[string]any) {
	e.t.Helper()
	if err := e.cons.Handle(context.Background(), whEventJSON(id, typ, e.tenant, e.clk.now(), payload)); err != nil {
		e.t.Fatalf("fan-out %s: %v", id, err)
	}
}

func (e *whEnv) invoice(id string) map[string]any {
	return map[string]any{"invoice_id": id, "amount": "10.00", "internal_note": "never leaves", "bank": map[string]any{"account_number": "12345678"}}
}

// drain delivers everything that is due right now.
func (e *whEnv) drain() int {
	e.t.Helper()
	total := 0
	for i := 0; i < 20; i++ {
		n, err := e.disp.RunOnce(context.Background())
		if err != nil {
			e.t.Fatalf("dispatch: %v", err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
	return total
}

func (e *whEnv) delivery(sub *whout.Subscription, eventID string) *whout.Delivery {
	e.t.Helper()
	d, err := e.st.GetDelivery(context.Background(), e.tenant, whout.DeliveryID(sub.SubscriptionID, eventID))
	if err != nil {
		e.t.Fatalf("delivery %s: %v", eventID, err)
	}
	return d
}

func (e *whEnv) attempts(d *whout.Delivery) []whout.Attempt {
	e.t.Helper()
	a, err := e.st.ListDeliveryAttempts(context.Background(), e.tenant, d.DeliveryID)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *whEnv) sub(id string) *whout.Subscription {
	e.t.Helper()
	s, err := e.svc.Get(context.Background(), e.tenant, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// outboxTypes counts the events this tenant's transactions enqueued, by type.
func (e *whEnv) outboxTypes() map[string]int {
	e.t.Helper()
	rows, err := e.admin.Query(context.Background(), `SELECT event_type, count(*) FROM event_outbox WHERE tenant_id = $1 GROUP BY event_type`, e.tenant)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			e.t.Fatal(err)
		}
		out[t] = n
	}
	return out
}

func (e *whEnv) deliveryCount(sub *whout.Subscription) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), `SELECT count(*) FROM webhook_deliveries WHERE subscription_id = $1`, sub.SubscriptionID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// forceDue makes a delivery due now regardless of its backoff (a test of what the
// dispatcher does, not of waiting).
func (e *whEnv) advance(d time.Duration) { e.clk.advance(d) }

// newDispatcherForRegions builds a dispatcher for the same environment whose deployment
// allows a different set of regions than the one the subscription was approved under.
func newDispatcherForRegions(e *whEnv, regions []string) (*whout.Dispatcher, error) {
	return whout.NewDispatcher(whout.DispatcherDeps{
		Store: e.st, Signing: e.vault, Egress: egress.Policy{Resolver: e.dns}, Client: e.dest.client(true), AllowedRegions: regions,
		Log: zap.NewNop(), Observer: e.obs, Now: e.clk.now, Jitter: func() float64 { return 0.999 },
	}, e.cfg)
}
