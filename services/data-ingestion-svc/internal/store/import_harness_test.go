package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/data-ingestion-svc/internal/domain"
	"zoiko.io/data-ingestion-svc/internal/imports"
	"zoiko.io/data-ingestion-svc/internal/store"
	"zoiko.io/eventing/scan"
)

var ictx = context.Background()

// ── a fake owning domain ────────────────────────────────────────────────────

type domainCall struct {
	Key    string
	Header http.Header
	Body   map[string]any
}

// fakeDomain behaves like an owning service: it honours Idempotency-Key (a replay answers with
// the original result and creates nothing), refuses a duplicate natural key with 409, and can be
// told to fail or to drop the connection after it has committed.
type fakeDomain struct {
	srv   *httptest.Server
	mu    sync.Mutex
	byKey map[string]struct {
		code int
		body string
	}
	records map[string]string // natural key -> record id
	calls   []domainCall
	hits    map[string]int
	cur     int32
	maxConc int32
	delay   time.Duration
	// failFirst makes the first N requests for a number answer with a status before it is created.
	failFirst map[string]struct {
		n    int
		code int
	}
	// dropAfterCommit processes the request fully, then closes the connection without answering.
	dropAfterCommit map[string]int
	rejectAll       int // when non-zero, every request is answered with this status
}

func newFakeDomain(t *testing.T) *fakeDomain {
	d := &fakeDomain{
		byKey: map[string]struct {
			code int
			body string
		}{}, records: map[string]string{}, hits: map[string]int{},
		failFirst: map[string]struct {
			n    int
			code int
		}{}, dropAfterCommit: map[string]int{},
	}
	d.srv = httptest.NewServer(http.HandlerFunc(d.handle))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDomain) handle(w http.ResponseWriter, r *http.Request) {
	c := atomic.AddInt32(&d.cur, 1)
	for {
		m := atomic.LoadInt32(&d.maxConc)
		if c <= m || atomic.CompareAndSwapInt32(&d.maxConc, m, c) {
			break
		}
	}
	defer atomic.AddInt32(&d.cur, -1)
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := r.Header.Get("Idempotency-Key")
	number, _ := body["number"].(string)

	d.mu.Lock()
	d.calls = append(d.calls, domainCall{Key: key, Header: r.Header.Clone(), Body: body})
	d.hits[key]++
	reply := func(code int, b string) {
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(b))
	}
	if d.rejectAll != 0 {
		reply(d.rejectAll, `{"error":"refused"}`)
		return
	}
	if prev, ok := d.byKey[key]; ok { // a replay of the same command: the original answer, nothing created
		reply(prev.code, prev.body)
		return
	}
	if f, ok := d.failFirst[number]; ok && f.n > 0 {
		f.n--
		d.failFirst[number] = f
		reply(f.code, `{"error":"try later"}`)
		return
	}
	if _, dup := d.records[number]; dup {
		d.byKey[key] = struct {
			code int
			body string
		}{409, `{"error":"duplicate invoice number"}`}
		reply(409, `{"error":"duplicate invoice number"}`)
		return
	}
	id := fmt.Sprintf("dom-%d", len(d.records)+1)
	d.records[number] = id
	resp := `{"id":"` + id + `"}`
	d.byKey[key] = struct {
		code int
		body string
	}{201, resp}
	if n := d.dropAfterCommit[number]; n > 0 {
		d.dropAfterCommit[number] = n - 1
		d.mu.Unlock()
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
		return
	}
	reply(201, resp)
}

func (d *fakeDomain) recordCount() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.records) }
func (d *fakeDomain) callCount() int   { d.mu.Lock(); defer d.mu.Unlock(); return len(d.calls) }
func (d *fakeDomain) hitsFor(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hits[key]
}
func (d *fakeDomain) allCalls() []domainCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]domainCall(nil), d.calls...)
}

// ── flaky scanner ───────────────────────────────────────────────────────────

// flakyScanner wraps a real scanner and can be told to be unreachable.
type flakyScanner struct {
	inner imports.ContentScanner
	down  atomic.Bool
}

func (f *flakyScanner) Name() string { return "flaky" }
func (f *flakyScanner) ScanOpen(ctx context.Context, open func() (io.ReadCloser, error), t string) (scan.Verdict, error) {
	if f.down.Load() {
		return scan.Verdict{}, fmt.Errorf("%w: clamd is down", scan.ErrUnavailable)
	}
	return f.inner.ScanOpen(ctx, open, t)
}

// ── the environment ─────────────────────────────────────────────────────────

const (
	uploader  = "uploader-1"
	approver  = "approver-1"
	executor  = "executor-1"
	platAuth  = "platform-author"
	platCert  = "platform-certifier"
	entityOne = "le-1"
)

type impEnv struct {
	t      *testing.T
	admin  *pgxpool.Pool
	app    *pgxpool.Pool
	st     *store.PgStore
	dom    *fakeDomain
	svc    *imports.Service
	exec   *imports.Executor
	val    *imports.ValidationRunner
	tenant string
	cfg    imports.Config
	chain  scan.Chain
}

type impOpts struct {
	cfg     func(*imports.Config)
	scanner imports.ContentScanner
	noScan  bool
}

func newImpEnv(t *testing.T, o ...impOpts) *impEnv {
	t.Helper()
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	if _, err := admin.Exec(ictx, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app_test`); err != nil {
		t.Fatal(err)
	}
	var opt impOpts
	if len(o) > 0 {
		opt = o[0]
	}
	e := &impEnv{t: t, admin: admin, app: app, st: store.NewPgStore(app), dom: newFakeDomain(t), tenant: uuid.NewString(),
		chain: scan.Chain{Scanners: []scan.Scanner{scan.Builtin{MaxBytes: 64 << 20}}}}
	e.cfg = imports.DefaultConfig()
	e.cfg.ItemRetryBase, e.cfg.ItemRetryCap, e.cfg.ItemMaxAttempts = 5*time.Millisecond, 20*time.Millisecond, 3
	e.cfg.MaxChunkBytes, e.cfg.MaxBatchBytes = 1<<20, 8<<20
	if opt.cfg != nil {
		opt.cfg(&e.cfg)
	}
	targets, err := imports.NewHTTPTargets(map[string]string{"ap": e.dom.srv.URL}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var sc imports.ContentScanner = e.chain
	if opt.scanner != nil {
		sc = opt.scanner
	}
	if opt.noScan {
		sc = nil
	}
	e.svc, err = imports.NewService(imports.Deps{Repo: e.st, Scanner: sc, Targets: targets, Config: e.cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.exec = imports.NewExecutor(e.svc, zap.NewNop(), nil)
	e.val = imports.NewValidationRunner(e.svc, zap.NewNop())
	return e
}

func (e *impEnv) actor(p string) imports.Actor {
	return imports.Actor{TenantID: e.tenant, PrincipalID: p, CorrelationID: "corr-" + p}
}

func (e *impEnv) platform(p string) imports.Actor {
	return imports.Actor{TenantID: imports.PlatformScopeID, PrincipalID: p, CorrelationID: "corr-" + p}
}

func (e *impEnv) claim(op string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{OwnerScope: domain.SellerScope, PrincipalID: uploader, Key: uuid.NewString(), Operation: op, RequestSHA256: strings.Repeat("a", 64), ResourceID: "x"}
}

func plainSpec() imports.Spec {
	return imports.Spec{
		ImportType: "supplier-invoices", Format: "csv",
		Columns: []imports.Column{
			{Name: "invoice_no", Type: imports.TypeString, Required: true, MaxLen: 20, Pattern: `^INV-[0-9]+$`},
			{Name: "entity", Type: imports.TypeString, Required: true},
			{Name: "amount", Type: imports.TypeDecimal, Required: true, NonNegative: true},
		},
		KeyColumns: []string{"invoice_no"}, EntityColumn: "entity", PartialPolicy: imports.PartialSkipInvalid,
		Control: &imports.Control{Column: "amount", Required: false},
		MaxRows: 5000,
		Target:  imports.Target{Key: "ap", Method: "POST", Path: "/v1/invoices", ResultRefField: "id"},
		Mapping: []imports.MapRule{
			{Field: "number", From: "column", Column: "invoice_no"}, {Field: "amount", From: "column", Column: "amount"},
			{Field: "legal_entity_id", From: "legal_entity_id"}, {Field: "origin", From: "const", Const: "import"},
		},
	}.Normalize()
}

func materialSpec() imports.Spec {
	s := plainSpec()
	s.Material, s.PartialPolicy, s.RequireResultRef = true, imports.PartialRejectBatch, true
	s.Control = &imports.Control{Column: "amount", Required: true}
	return s.Normalize()
}

// profile publishes and certifies a profile as two different platform people.
func (e *impEnv) profile(spec imports.Spec) *imports.Profile {
	e.t.Helper()
	key := "p-" + strings.ToLower(uuid.NewString()[:8])
	p, err := e.svc.PublishProfile(ictx, e.platform(platAuth), key, spec)
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
	c, err := e.svc.CertifyProfile(ictx, e.platform(platCert), key, p.Version)
	if err != nil {
		e.t.Fatalf("certify: %v", err)
	}
	return c
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func csvOf(rows ...string) string {
	return "invoice_no,entity,amount\n" + strings.Join(rows, "\n") + "\n"
}

// goodRows builds n valid rows for the entity; the amounts sum to n*10.
func goodRows(n int, entity string) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("INV-%d,%s,10.00", i+1, entity)
	}
	return rows
}

type declared struct {
	rows    *int
	control string
	sha     string // override the content hash the sender declares
	entity  string
}

// create declares a batch for content under a profile.
func (e *impEnv) create(p *imports.Profile, content string, d declared) (*imports.Batch, error) {
	e.t.Helper()
	if d.sha == "" {
		d.sha = sha([]byte(content))
	}
	if d.entity == "" {
		d.entity = entityOne
	}
	return e.svc.CreateBatch(ictx, e.actor(uploader), imports.CreateRequest{
		LegalEntityID: d.entity, ProfileKey: p.Key, ProfileVersion: p.Version, SourceSystem: "erp-x", FileName: "invoices.csv",
		ContentType: "text/csv", SHA256: d.sha, DeclaredRows: d.rows, DeclaredControl: d.control,
	}, e.claim("CreateBatch"))
}

func ip(n int) *int { return &n }

// upload sends content in chunks of the given size, in the given order of chunk indexes.
func (e *impEnv) upload(b *imports.Batch, content string, chunk int, reverse bool) int {
	e.t.Helper()
	var parts [][]byte
	raw := []byte(content)
	for len(raw) > 0 {
		n := chunk
		if n > len(raw) {
			n = len(raw)
		}
		parts = append(parts, raw[:n])
		raw = raw[n:]
	}
	order := make([]int, len(parts))
	for i := range order {
		order[i] = i
		if reverse {
			order[i] = len(parts) - 1 - i
		}
	}
	for _, i := range order {
		if _, err := e.svc.UploadChunk(ictx, e.actor(uploader), b.BatchID, i, parts[i], sha(parts[i])); err != nil {
			e.t.Fatalf("chunk %d: %v", i, err)
		}
	}
	return len(parts)
}

// validated takes content all the way to VALIDATED (or wherever the pipeline stops).
func (e *impEnv) validated(p *imports.Profile, content string, d declared) *imports.Batch {
	e.t.Helper()
	b, err := e.create(p, content, d)
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	n := e.upload(b, content, 64, false)
	if _, err := e.svc.Seal(ictx, e.actor(uploader), b.BatchID, n); err != nil {
		e.t.Fatalf("seal: %v", err)
	}
	if _, err := e.svc.RequestValidation(ictx, e.actor(uploader), b.BatchID); err != nil {
		e.t.Fatalf("request validation: %v", err)
	}
	out, err := e.svc.RunPipeline(ictx, e.tenant, b.BatchID)
	if err != nil {
		e.t.Fatalf("pipeline: %v", err)
	}
	return out
}

func (e *impEnv) previewed(p *imports.Profile, content string, d declared) *imports.Batch {
	e.t.Helper()
	b := e.validated(p, content, d)
	if b.Status != imports.StatusValidated {
		e.t.Fatalf("batch is %s (%s), want VALIDATED", b.Status, b.StatusReason)
	}
	out, err := e.svc.Preview(ictx, e.actor(uploader), b.BatchID)
	if err != nil {
		e.t.Fatalf("preview: %v", err)
	}
	return out
}

func (e *impEnv) approved(p *imports.Profile, content string, d declared) *imports.Batch {
	e.t.Helper()
	b := e.previewed(p, content, d)
	out, err := e.svc.Approve(ictx, e.actor(approver), b.BatchID, b.PreviewFingerprint, "checked against the ledger")
	if err != nil {
		e.t.Fatalf("approve: %v", err)
	}
	return out
}

func (e *impEnv) executing(p *imports.Profile, content string, d declared) *imports.Batch {
	e.t.Helper()
	b := e.approved(p, content, d)
	out, err := e.svc.Execute(ictx, e.actor(executor), b.BatchID)
	if err != nil {
		e.t.Fatalf("execute: %v", err)
	}
	return out
}

// drain runs the executor until the batch has nothing left to do.
func (e *impEnv) drain(b *imports.Batch) *imports.Batch {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		got, err := e.svc.Get(ictx, e.tenant, b.BatchID)
		if err != nil {
			e.t.Fatal(err)
		}
		if got.Status != imports.StatusImporting {
			return got
		}
		if _, err := e.exec.ProcessBatch(ictx, imports.Ref{TenantID: e.tenant, BatchID: b.BatchID}); err != nil {
			e.t.Fatalf("executor: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("the batch did not finish")
	return nil
}

func (e *impEnv) completed(p *imports.Profile, content string, d declared) *imports.Batch {
	e.t.Helper()
	return e.drain(e.executing(p, content, d))
}

func (e *impEnv) batch(id string) *imports.Batch {
	e.t.Helper()
	b, err := e.svc.Get(ictx, e.tenant, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *impEnv) counts(id string) map[imports.ItemStatus]int {
	e.t.Helper()
	c, err := e.st.ItemCounts(ictx, e.tenant, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// outboxTypes counts the events this tenant's batches enqueued, by type.
func (e *impEnv) outboxTypes() map[string]int {
	e.t.Helper()
	rows, err := e.admin.Query(ictx, `SELECT event_type, count(*) FROM outbox_events WHERE tenant_id = $1 GROUP BY event_type`, e.tenant)
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

func scanChain() imports.ContentScanner {
	return scan.Chain{Scanners: []scan.Scanner{scan.Builtin{MaxBytes: 64 << 20}}}
}

// profileFor reads a certified profile back by key.
func (e *impEnv) profileFor(key string) *imports.Profile {
	e.t.Helper()
	p, err := e.svc.GetProfile(ictx, key, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}
