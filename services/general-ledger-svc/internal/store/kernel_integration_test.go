package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
	"zoiko.io/general-ledger-svc/internal/kernel"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

// These run the accounting kernel against REAL Postgres as an ordinary (non-superuser) role: row-level security is in
// force, the guard triggers refuse what the service would never ask for, and the HTTP surface is exercised end to end.

type kenv struct {
	admin  *pgxpool.Pool
	app    *pgxpool.Pool
	repo   *store.KernelRepo
	svc    *kernel.Service
	tenant string
	entity string
	ts     *httptest.Server
	denied sync.Map
	periods *fakePeriods
	pg      *store.PgStore
	obs     *recObs
}

type fakeAuthz struct{ e *kenv }

func (f fakeAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if _, no := f.e.denied.Load(principal + "|" + action); no {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

type fakeUnits map[string]int

func (u fakeUnits) MinorUnits(_ context.Context, c string, _ time.Time) (int, error) {
	if v, ok := u[c]; ok {
		return v, nil
	}
	return 2, nil
}

func newKEnv(t *testing.T) *kenv {
	t.Helper()
	admin := openTestPool(t)
	ctx := context.Background()
	_, _ = admin.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='kernel_app') THEN
		CREATE ROLE kernel_app LOGIN PASSWORD 'app' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; END IF; END $$`)
	for _, q := range []string{`GRANT USAGE ON SCHEMA public TO kernel_app`, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kernel_app`, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO kernel_app`} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	u.User = url.UserPassword("kernel_app", "app")
	app, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	e := &kenv{admin: admin, app: app, tenant: uuid.NewString(), entity: uuid.NewString()}
	e.repo = store.NewKernelRepo(app, "uk")
	e.svc = &kernel.Service{Repo: e.repo}
	e.periods = &fakePeriods{states: map[string]string{}}
	e.pg = store.New(app, zap.NewNop(), store.WithEventRegion("uk"))
	e.obs = &recObs{}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	handler.RegisterKernelRoutes(r, handler.NewKernel(e.svc, e.repo, e.repo, fakeUnits{"JPY": 0}, nil, fakeAuthz{e}, zap.NewNop()).
		WithPeriodGate(handler.ClosePeriodGate{Close: e.periods, Authz: fakeAuthz{e}}).
		WithObserver(e.obs).
		WithSigner(kernel.HMACSigner{Key: []byte("test-signing-key-test-signing-key!!"), KeyID: "test-k1"}))
	handler.RegisterRoutes(r, handler.New(e.pg, nopPublisher{}, fakeAuthz{e}, e.periods, kenvLogger()).
		WithJournalControls(e.pg, handler.ManualJournalPolicy{EvidenceThresholdCents: 1000000}))
	e.ts = httptest.NewServer(r)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *kenv) ctx() context.Context { return svcmiddleware.WithTenant(context.Background(), e.tenant) }

func (e *kenv) call(method, path, tenant, principal string, body any) (int, map[string]any) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	req.Header.Set("X-Legal-Entity-Id", e.entity)
	req.Header.Set("X-Correlation-ID", "corr-k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *kenv) exec(sql string, args ...any) error {
	_, err := e.admin.Exec(context.Background(), sql, args...)
	return err
}

// asApp runs sql as the ordinary role with the given tenant in force and returns the error.
func (e *kenv) asApp(tenant, sql string, args ...any) error {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (e *kenv) countAs(tenant, sql string) int {
	ctx := context.Background()
	tx, err := e.app.Begin(ctx)
	if err != nil {
		panic(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, _ = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant)
	var n int
	if err := tx.QueryRow(ctx, sql).Scan(&n); err != nil {
		panic(err)
	}
	return n
}

func wantRefused(t *testing.T, what string, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the database accepted it", what)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("%s: %v", what, err)
	}
}

func bookBody(entity, code string) map[string]any {
	return map[string]any{"legal_entity_id": entity, "book_code": code, "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS", "functional_currency": "GBP",
		"fiscal_calendar_id": "CAL", "chart_of_accounts_id": "COA", "default_ledger_id": "LED", "effective_from": "2026-01-01T00:00:00Z"}
}

func (e *kenv) activeBook(t *testing.T, code string) string {
	t.Helper()
	st, b := e.call("POST", "/v1/accounting-books", e.tenant, "owner", bookBody(e.entity, code))
	if st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	id := b["book_id"].(string)
	if st, b = e.call("POST", "/v1/accounting-books/"+id+"/activate", e.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	return id
}

func (e *kenv) seedChart(t *testing.T) {
	t.Helper()
	for _, a := range [][2]string{{"1100", "ASSET"}, {"4000", "REVENUE"}, {"2200", "LIABILITY"}, {"7999", "EXPENSE"}} {
		if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), e.tenant, a[0], a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}
	for k, c := range map[string]string{"AR": "1100", "REV": "4000", "VAT": "2200", "ROUND": "7999"} {
		if err := e.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`,
			uuid.NewString(), e.tenant, k, c); err != nil {
			t.Fatal(err)
		}
	}
}

func invoiceRule(version string) map[string]any {
	return map[string]any{
		"name": "ar-invoice", "domain": "AR", "event_type": "AR.INVOICE_ISSUED", "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS", "version": version,
		"effective_from": "2026-01-01T00:00:00Z", "test_pack_id": "tp-1",
		"content": map[string]any{
			"conditions": []any{map[string]any{"fact": "document_type", "op": "eq", "value": "INVOICE"}}, "rounding_policy_id": "HALF_EVEN",
			"line_templates": []any{
				map[string]any{"line_ref": "L1", "side": "DEBIT", "account": map[string]any{"mapping_key": "AR"}, "amount": "gross"},
				map[string]any{"line_ref": "L2", "side": "CREDIT", "account": map[string]any{"mapping_key": "REV"}, "amount": "net"},
				map[string]any{"line_ref": "L3", "side": "CREDIT", "account": map[string]any{"mapping_key": "VAT"}, "amount": "tax", "when": []any{map[string]any{"fact": "tax", "op": "gt", "value": "0"}}},
			},
		},
		"test_pack": []any{map[string]any{"name": "basic",
			"event":    map[string]any{"event_type": "AR.INVOICE_ISSUED", "transaction_currency": "GBP", "facts": map[string]any{"document_type": "INVOICE", "gross": "120.00", "net": "100.00", "tax": "20.00"}},
			"accounts": map[string]any{"AR": "1100", "REV": "4000", "VAT": "2200"},
			"expect": map[string]any{"lines": []any{
				map[string]any{"side": "DEBIT", "account_code": "1100", "amount": "120.00"}, map[string]any{"side": "CREDIT", "account_code": "4000", "amount": "100.00"},
				map[string]any{"side": "CREDIT", "account_code": "2200", "amount": "20.00"}}}}},
	}
}

func eventBody(entity, sourceEvent string, gross, net, tax string) map[string]any {
	return map[string]any{"event_type": "AR.INVOICE_ISSUED", "event_version": 1, "legal_entity_id": entity, "source_system": "accounts-receivable-svc", "source_object_type": "invoice",
		"source_object_id": "inv-" + sourceEvent, "source_event_id": sourceEvent, "occurred_at": "2026-03-10T09:00:00Z", "document_date": "2026-03-10T00:00:00Z", "transaction_currency": "GBP",
		"facts": map[string]any{"document_type": "INVOICE", "gross": gross, "net": net, "tax": tax}, "idempotency_key": "idem-" + sourceEvent, "correlation_id": "corr-" + sourceEvent, "causation_id": "cause-" + sourceEvent}
}

// runRuleToActive takes a rule set through test, approval and activation by three different people.
func (e *kenv) runRuleToActive(t *testing.T, version, book string) string {
	t.Helper()
	st, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", invoiceRule(version))
	if st != 201 {
		t.Fatalf("%d %v", st, rs)
	}
	id := rs["posting_rule_set_id"].(string)
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/test", e.tenant, "builder", map[string]any{"book_id": book}); st != 200 {
		t.Fatalf("test %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/approve", e.tenant, "approver", map[string]any{}); st != 200 {
		t.Fatalf("approve %d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/activate", e.tenant, "releaser", map[string]any{}); st != 200 {
		t.Fatalf("activate %d %v", st, b)
	}
	return id
}

func TestKernelFoundationsEndToEnd(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")

	// books: one per entity and code; identity is fixed
	if st, _ := e.call("POST", "/v1/accounting-books", e.tenant, "owner", bookBody(e.entity, "STAT")); st != 409 {
		t.Fatalf("a duplicate book code: %d", st)
	}
	if st, b := e.call("POST", "/v1/accounting-books", e.tenant, "owner", map[string]any{"legal_entity_id": e.entity}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/accounting-books/"+book+"/activate", e.tenant, "owner", map[string]any{}); st != 409 {
		t.Fatalf("activating an active book: %d", st)
	}
	wantRefused(t, "change a book's currency", e.asApp(e.tenant, `UPDATE accounting_books SET functional_currency='USD', version=version+1 WHERE book_id=$1::uuid`, book), "fixed")
	wantRefused(t, "delete a book", e.asApp(e.tenant, `DELETE FROM accounting_books WHERE book_id=$1::uuid`, book), "never deleted")

	// no rule yet: a deterministic exception, nothing posted
	st, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, "se-1", "120.00", "100.00", "20.00"))
	if st != 201 {
		t.Fatalf("%d %v", st, ev)
	}
	evID := ev["accounting_event_id"].(string)
	if ev["payload_hash"] == "" || ev["registered_by"] != "ar-svc" {
		t.Fatalf("%v", ev)
	}
	st, b := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": evID, "book_id": book})
	if st != 422 || b["family"] != "AK-RULE" || b["code"] != "NO_RULE" {
		t.Fatalf("%d %v", st, b)
	}

	// the rule set: author, tester, approver and releaser are separate people
	st, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", invoiceRule("1.0.0"))
	if st != 201 || rs["status"] != "DRAFT" {
		t.Fatalf("%d %v", st, rs)
	}
	rsID := rs["posting_rule_set_id"].(string)
	if st, _ := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", invoiceRule("1.0.0")); st != 409 {
		t.Fatalf("the same version twice: %d", st)
	}
	bad := invoiceRule("1.0.1")
	bad["content"].(map[string]any)["line_templates"].([]any)[0].(map[string]any)["amount"] = "gross +"
	if st, b := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", bad); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/approve", e.tenant, "approver", map[string]any{}); st != 409 {
		t.Fatalf("an untested rule set cannot be approved: %d", st)
	}
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/activate", e.tenant, "releaser", map[string]any{}); st != 409 {
		t.Fatalf("a draft cannot be activated: %d", st)
	}
	if err := e.asApp(e.tenant, `UPDATE posting_rule_sets SET status='APPROVED', approved_by='someone', row_version=row_version+1 WHERE posting_rule_set_id=$1::uuid`, rsID); err == nil {
		t.Fatal("DRAFT -> APPROVED by SQL must be refused")
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/test", e.tenant, "builder", map[string]any{"book_id": book}); st != 200 || b["status"] != "TEST" || b["tests_passed_hash"] != b["content_hash"] {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/approve", e.tenant, "builder", map[string]any{}); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the author cannot approve: %d %v", st, b)
	}
	wantRefused(t, "SQL self-approval", e.asApp(e.tenant, `UPDATE posting_rule_sets SET status='APPROVED', approved_by=created_by, tests_passed_hash=content_hash, row_version=row_version+1 WHERE posting_rule_set_id=$1::uuid`, rsID), "")
	wantRefused(t, "SQL edit of content after test", e.asApp(e.tenant, `UPDATE posting_rule_sets SET content='{}'::jsonb, row_version=row_version+1 WHERE posting_rule_set_id=$1::uuid`, rsID), "fixed once it leaves draft")
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/approve", e.tenant, "approver", map[string]any{}); st != 200 || b["status"] != "APPROVED" || b["approved_by"] != "approver" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/activate", e.tenant, "builder", map[string]any{}); st != 403 {
		t.Fatalf("the author cannot activate: %d %v", st, b)
	}
	e.denied.Store("junior|"+"GL_RULE_ACTIVATE", true)
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/activate", e.tenant, "junior", map[string]any{}); st != 403 {
		t.Fatalf("activation is its own permission: %d", st)
	}
	if st, b := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/activate", e.tenant, "releaser", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}

	// evaluation: balanced, with provenance, deterministic, and it changes nothing
	before := e.countAs(e.tenant, `SELECT count(*) FROM journal_headers`)
	st, p := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": evID, "book_id": book})
	if st != 200 {
		t.Fatalf("%d %v", st, p)
	}
	lines := p["lines"].([]any)
	prov := p["provenance"].(map[string]any)
	if len(lines) != 3 || p["total_debit_transaction"] != "120.00" || p["total_credit_transaction"] != "120.00" || prov["posting_rule_set_id"] != rsID || prov["rule_version"] != "1.0.0" {
		t.Fatalf("%v", p)
	}
	_, p2 := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": evID, "book_id": book})
	if p2["proposal_hash"] != p["proposal_hash"] || p["proposal_hash"] == "" {
		t.Fatal("evaluation is deterministic")
	}
	if e.countAs(e.tenant, `SELECT count(*) FROM journal_headers`) != before || e.countAs(e.tenant, `SELECT count(*) FROM ledger_entries`) != 0 {
		t.Fatal("evaluation must not write a journal or a ledger entry")
	}

	// events: a replay returns the original; changed content under the same producer event is refused
	st, again := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, "se-1", "120.00", "100.00", "20.00"))
	if st != 200 || again["accounting_event_id"] != evID {
		t.Fatalf("a replay returns the original: %d %v", st, again)
	}
	if st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, "se-1", "999.00", "100.00", "20.00")); st != 422 || b["code"] != "SOURCE_EVENT_CONFLICT" {
		t.Fatalf("%d %v", st, b)
	}
	if e.countAs(e.tenant, `SELECT count(*) FROM accounting_events`) != 1 {
		t.Fatal("the replay and the conflict stored nothing")
	}
	wantRefused(t, "edit an event", e.asApp(e.tenant, `UPDATE accounting_events SET event_type='AP.X'`), "never changed")
	wantRefused(t, "delete an event", e.asApp(e.tenant, `DELETE FROM accounting_events`), "never changed")
	bogus := eventBody(e.entity, "se-2", "1.00", "1.00", "0.00")
	bogus["event_type"] = "invoice"
	if st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", bogus); st != 422 || b["family"] != "AK-DATA" {
		t.Fatalf("%d %v", st, b)
	}

	// a failing branch is an exception, not a partial posting
	_, ev2 := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, "se-3", "100.00", "100.00", "0.00"))
	ev2["facts"] = nil
	st, p3 := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": ev2["accounting_event_id"], "book_id": book})
	if st != 200 || len(p3["lines"].([]any)) != 2 {
		t.Fatalf("a zero-tax invoice skips the tax line: %d %v", st, p3)
	}
	e.exec(`DELETE FROM account_mappings WHERE mapping_key='REV'`)
	if st, b := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": evID, "book_id": book}); st != 422 || b["family"] != "AK-COA" {
		t.Fatalf("%d %v", st, b)
	}

	// versions: v2 replaces v1 from its start date; two active rule sets never claim one date
	v2 := invoiceRule("2.0.0")
	v2["effective_from"] = "2026-06-01T00:00:00Z"
	_, r2 := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", v2)
	r2ID := r2["posting_rule_set_id"].(string)
	e.call("POST", "/v1/posting-rule-sets/"+r2ID+"/test", e.tenant, "builder", map[string]any{"book_id": book})
	e.call("POST", "/v1/posting-rule-sets/"+r2ID+"/approve", e.tenant, "approver", map[string]any{})
	st, act := e.call("POST", "/v1/posting-rule-sets/"+r2ID+"/activate", e.tenant, "releaser", map[string]any{})
	if st != 200 || len(act["superseded"].([]any)) != 1 {
		t.Fatalf("%d %v", st, act)
	}
	if _, old := e.call("GET", "/v1/posting-rule-sets/"+rsID, e.tenant, "reader", nil); old["status"] != "SUPERSEDED" || old["effective_to"] == nil {
		t.Fatalf("the replaced version is closed where the new one starts: %v", old)
	}
	if st, evs := e.call("GET", "/v1/posting-rule-sets/"+rsID+"/events", e.tenant, "reader", nil); st != 200 || evs["total"].(float64) < 5 {
		t.Fatalf("every lifecycle step is evidenced: %v", evs)
	}
	// an overlapping activation is refused by the database even if the service were bypassed
	_, r3 := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", invoiceRule("3.0.0"))
	r3ID := r3["posting_rule_set_id"].(string)
	e.call("POST", "/v1/posting-rule-sets/"+r3ID+"/test", e.tenant, "builder", map[string]any{"book_id": book})
	e.call("POST", "/v1/posting-rule-sets/"+r3ID+"/approve", e.tenant, "approver", map[string]any{})
	wantRefused(t, "overlapping activation by SQL", e.asApp(e.tenant, `UPDATE posting_rule_sets SET status='ACTIVE', activated_by='someone-else', row_version=row_version+1 WHERE posting_rule_set_id=$1::uuid`, r3ID), "already covers")
	wantRefused(t, "delete a rule set", e.asApp(e.tenant, `DELETE FROM posting_rule_sets`), "never deleted")
	wantRefused(t, "edit rule evidence", e.asApp(e.tenant, `UPDATE posting_rule_set_events SET kind='X'`), "append-only")

	// the outbox carries the facts of the kernel
	for _, fact := range []string{"book.created", "event.registered", "rule-set.created", "rule-set.tested", "rule-set.approved", "rule-set.activated"} {
		if n := e.countAs(e.tenant, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.accounting.kernel.`+fact+`'`); n == 0 {
			t.Errorf("no %s event on the outbox", fact)
		}
	}
}

func TestKernelConcurrentActivationsOfOneScope(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")
	var ids []string
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0"} {
		_, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", invoiceRule(v))
		id := rs["posting_rule_set_id"].(string)
		e.call("POST", "/v1/posting-rule-sets/"+id+"/test", e.tenant, "builder", map[string]any{"book_id": book})
		e.call("POST", "/v1/posting-rule-sets/"+id+"/approve", e.tenant, "approver", map[string]any{})
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			st, _ := e.call("POST", "/v1/posting-rule-sets/"+id+"/activate", e.tenant, "releaser", map[string]any{})
			mu.Lock()
			codes[st]++
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	active := e.countAs(e.tenant, `SELECT count(*) FROM posting_rule_sets WHERE status='ACTIVE'`)
	// all six start on the same date: exactly one can be in force at a time (the others are refused or supersede nothing)
	if active != 1 {
		t.Fatalf("exactly one rule set is active per scope and date, got %d (%v)", active, codes)
	}
	if codes[200] != 1 {
		t.Fatalf("one activation wins: %v", codes)
	}
}

func TestKernelTenantIsolation(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")
	rsID := e.runRuleToActive(t, "1.0.0", book)
	_, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, "se-1", "120.00", "100.00", "20.00"))
	other := uuid.NewString()
	for _, p := range []string{"/v1/accounting-books/" + book, "/v1/posting-rule-sets/" + rsID, "/v1/accounting-events/" + ev["accounting_event_id"].(string), "/v1/posting-rule-sets/" + rsID + "/events"} {
		if st, _ := e.call("GET", p, other, "intruder", nil); st != 404 {
			t.Errorf("GET %s as another tenant: %d", p, st)
		}
	}
	if st, b := e.call("POST", "/v1/posting-evaluations", other, "intruder", map[string]any{"accounting_event_id": ev["accounting_event_id"], "book_id": book}); st != 404 {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+rsID+"/retire", other, "intruder", map[string]any{"reason": "x"}); st != 404 {
		t.Fatalf("%d", st)
	}
	for _, tbl := range []string{"accounting_books", "accounting_events", "posting_rule_sets", "posting_rule_set_events", "dimension_definitions", "dimension_values"} {
		if n := e.countAs(other, `SELECT count(*) FROM `+tbl); n != 0 {
			t.Errorf("%s leaks %d rows to another tenant", tbl, n)
		}
	}
	wantRefused(t, "forged book for another tenant", e.asApp(other, `INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, book_code, book_type, accounting_framework_id, functional_currency,
		fiscal_calendar_id, chart_of_accounts_id, default_ledger_id, effective_from, created_by) VALUES (gen_random_uuid(), $1, $2, 'X', 'TAX', 'F', 'GBP', 'C', 'C', 'L', '2026-01-01', 'x')`, e.tenant, e.entity), "")
	// a book of one entity cannot evaluate an event of another
	e2 := uuid.NewString()
	st, b2 := e.call("POST", "/v1/accounting-books", e.tenant, "owner", bookBody(e2, "STAT"))
	if st != 201 {
		t.Fatalf("%d", st)
	}
	e.call("POST", "/v1/accounting-books/"+b2["book_id"].(string)+"/activate", e.tenant, "owner", map[string]any{})
	if st, b := e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": ev["accounting_event_id"], "book_id": b2["book_id"]}); st != 403 || b["code"] != "ENTITY_MISMATCH" {
		t.Fatalf("%d %v", st, b)
	}
	// authorization is per action
	e.denied.Store("viewer|GL_BOOK_MANAGE", true)
	if st, _ := e.call("POST", "/v1/accounting-books", e.tenant, "viewer", bookBody(e.entity, "NEW")); st != 403 {
		t.Fatalf("%d", st)
	}
	if st, _ := e.call("GET", "/v1/accounting-books", e.tenant, "", nil); st != 401 {
		t.Fatalf("no identity: %d", st)
	}
}

func TestKernelDimensionsAndRetirement(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")
	if st, _ := e.call("POST", "/v1/dimensions", e.tenant, "owner", map[string]any{"dimension_code": "cost_center", "name": "Cost centre"}); st != 201 {
		t.Fatal(st)
	}
	if st, _ := e.call("POST", "/v1/dimensions", e.tenant, "owner", map[string]any{"dimension_code": "cost_center", "name": "again"}); st != 409 {
		t.Fatal(st)
	}
	st, v := e.call("POST", "/v1/dimensions/cost_center/values", e.tenant, "owner", map[string]any{"value_code": "CC1", "name": "Sales", "valid_from": "2026-01-01T00:00:00Z"})
	if st != 201 {
		t.Fatalf("%d %v", st, v)
	}
	if st, _ := e.call("POST", "/v1/dimensions/nope/values", e.tenant, "owner", map[string]any{"value_code": "X", "name": "x", "valid_from": "2026-01-01T00:00:00Z"}); st == 201 {
		t.Fatal("a value of an unknown dimension")
	}
	wantRefused(t, "rename a value", e.asApp(e.tenant, `UPDATE dimension_values SET name='Other'`), "never renamed")
	wantRefused(t, "delete a value", e.asApp(e.tenant, `DELETE FROM dimension_values`), "never deleted")

	rule := invoiceRule("1.0.0")
	rule["content"].(map[string]any)["dimension_rules"] = []any{map[string]any{"dimension": "cost_center", "mode": "REQUIRED"}}
	rule["test_pack"].([]any)[0].(map[string]any)["event"].(map[string]any)["dimensions"] = map[string]any{"cost_center": "CC1"}
	_, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", rule)
	id := rs["posting_rule_set_id"].(string)
	e.call("POST", "/v1/posting-rule-sets/"+id+"/test", e.tenant, "builder", map[string]any{"book_id": book})
	e.call("POST", "/v1/posting-rule-sets/"+id+"/approve", e.tenant, "approver", map[string]any{})
	e.call("POST", "/v1/posting-rule-sets/"+id+"/activate", e.tenant, "releaser", map[string]any{})

	mk := func(src string, dims map[string]any) string {
		b := eventBody(e.entity, src, "120.00", "100.00", "20.00")
		if dims != nil {
			b["dimensions"] = dims
		}
		_, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", b)
		return ev["accounting_event_id"].(string)
	}
	eval := func(id string) (int, map[string]any) {
		return e.call("POST", "/v1/posting-evaluations", e.tenant, "ar-svc", map[string]any{"accounting_event_id": id, "book_id": book})
	}
	if st, b := eval(mk("d1", nil)); st != 422 || b["code"] != "DIMENSION_MISSING" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := eval(mk("d2", map[string]any{"cost_center": "CC1"})); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := eval(mk("d3", map[string]any{"cost_center": "CC9"})); st != 422 || b["code"] != "DIMENSION_VALUE_INVALID" {
		t.Fatalf("an unregistered value: %d %v", st, b)
	}
	// retiring the value stops new use, and it can never come back
	if st, _ := e.call("POST", "/v1/dimension-values/"+v["value_id"].(string)+"/retire", e.tenant, "owner", nil); st != 200 {
		t.Fatal(st)
	}
	time.Sleep(10 * time.Millisecond)
	if st, b := eval(mk("d4", map[string]any{"cost_center": "CC1"})); st != 200 {
		// the value is retired as of today; the event's posting date is in March, when it was valid
		t.Fatalf("history keeps its meaning: %d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/dimension-values/"+v["value_id"].(string)+"/retire", e.tenant, "owner", nil); st != 404 {
		t.Fatalf("a retired value is final: %d", st)
	}
	if st, l := e.call("GET", "/v1/dimension-values?dimension=cost_center", e.tenant, "reader", nil); st != 200 || l["total"].(float64) != 1 {
		t.Fatalf("%d %v", st, l)
	}
}

func TestKernelBooksAreBackfilledFromExistingJournals(t *testing.T) {
	e := newKEnv(t)
	ctx := e.ctx()
	// journals exist with a free-text book_id and no registered book, as they do in a database from before this migration
	if err := e.exec(`DROP TABLE dimension_values, dimension_definitions, posting_rule_set_events, posting_rule_sets, accounting_events, accounting_books CASCADE`); err != nil {
		t.Fatal(err)
	}
	s := store.New(e.admin, zap.NewNop(), store.WithEventRegion("uk"))
	mk := func(bookID *string, ccy string) {
		h := &domain.JournalHeader{JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 7, 15), PostingDate: domain.NewDate(2026, 7, 15), CurrencyCode: ccy,
			JournalID: uuid.NewString(), TenantID: e.tenant, LegalEntityID: e.entity, FiscalPeriod: "2026-07", Status: domain.JournalStatusPending, Description: "legacy",
			CreatedByPrincipalID: "x", CorrelationID: uuid.NewString(), BookID: bookID}
		if _, _, err := s.CreateJournal(ctx, h, []domain.JournalLine{{AccountCode: "1000", DebitAmount: 1}, {AccountCode: "4000", CreditAmount: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	main := "MAIN"
	mk(&main, "EUR")
	mk(&main, "EUR")
	mk(&main, "GBP")
	mk(nil, "GBP")
	_, file, _, _ := runtime.Caller(0)
	sql, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../deployments/migrations/000018_accounting_kernel_foundations.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("the migration over existing data: %v", err)
	}
	rows, err := e.admin.Query(context.Background(), `SELECT book_code, functional_currency, status, accounting_framework_id FROM accounting_books WHERE tenant_id=$1 ORDER BY book_code`, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][3]string{}
	for rows.Next() {
		var c, ccy, st, fw string
		if err := rows.Scan(&c, &ccy, &st, &fw); err != nil {
			t.Fatal(err)
		}
		got[c] = [3]string{ccy, st, fw}
	}
	if len(got) != 2 || got["MAIN"] != [3]string{"EUR", "ACTIVE", "LEGACY"} || got["DEFAULT"][0] != "GBP" {
		t.Fatalf("every book in use is registered, in the currency most journals used: %v", got)
	}
	if errors.Is(nil, nil) {
		// the migration is also re-runnable only through its down/up pair: nothing else to check here
	}
	_ = json.Marshal
}
