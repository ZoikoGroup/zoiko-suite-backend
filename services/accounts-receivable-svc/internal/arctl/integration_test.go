//go:build integration

// Real-Postgres tests for the AR-to-GL control module. Run (serially):
//
//	go test -tags=integration -count=1 -p 1 -timeout=600s ./internal/arctl/...
//
// The embedded superuser bypasses RLS, so isolation tests prove the explicit tenant_id / legal_entity_id predicates.
package arctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/kit/kittest"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/store"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	var stop func()
	pool, stop = kittest.Start("arctl_test", 18200)
	code := m.Run()
	stop()
	os.Exit(code)
}

const base = "/internal/v1/arctl/"

// fakeGL is a controllable GLBalanceSource.
type fakeGL struct {
	mu     sync.Mutex
	totals map[string]kit.Amount
	err    error
	down   bool
	calls  int
}

func (f *fakeGL) set(t map[string]kit.Amount) {
	f.mu.Lock()
	f.totals, f.down = t, false
	f.mu.Unlock()
}

func (f *fakeGL) ControlAccountBalance(_ context.Context, _ svcclient.Caller, _, period string, keys []string) (*GLRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.down {
		return &GLRead{Available: false, Error: "general ledger unreachable", MappingKeys: keys, PeriodID: period, Totals: map[string]kit.Amount{}}, nil
	}
	cp := map[string]kit.Amount{}
	for k, v := range f.totals {
		cp[k] = v
	}
	return &GLRead{Available: true, MappingKeys: keys, AccountCodes: []string{"1200"}, PeriodID: period, Totals: cp, Watermark: "gl-wm-1"}, nil
}

type authz struct{ allow map[string]bool }

func (a authz) CheckAllowed(_ context.Context, p, _, action string) error {
	if a.allow[p+"|"+action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type entities struct{}

func (entities) VerifyInTenant(context.Context, string, string) error { return nil }

var clockNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	m      *Module
	h      http.Handler
	ar07m  *ar07.Module
	gl     *fakeGL
	tenant string
	entity string
	party  string
	seq    int
	db     *kit.DB
}

func emptyPop(context.Context, domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	return &domain.ControlPopulationPage{Records: []domain.ControlRecord{}, Watermark: "empty:0", DeclaredTotals: domain.DeclaredTotals{Totals: map[string]string{}}}, nil
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, tenant: uuid.NewString(), entity: uuid.NewString(), party: uuid.NewString(), gl: &fakeGL{totals: map[string]kit.Amount{}}}
	e.db = kit.NewDB(pool)
	deps := kit.Deps{DB: e.db, Log: zap.NewNop(), Entities: entities{}, Clock: func() time.Time { return clockNow }, Authz: authz{allow: map[string]bool{
		"runner|" + ActionRunReconciliation: true, "runner2|" + ActionRunReconciliation: true,
		"acker|" + ActionAcknowledgeFinding: true, "runner|" + ActionAcknowledgeFinding: true,
	}}}
	e.ar07m = ar07.New(deps)
	pg := PgSources{DB: e.db}
	legacy := store.New(pool, zap.NewNop()).ControlPopulation
	e.m = New(deps, Sources{Documents: pg, Completeness: e.ar07m, Postings: pg, GL: e.gl,
		Subledger:  PopulationSubledger{Provider: Union(legacy, NewPathInvoices(e.db))},
		Exceptions: pg.Rollup(func() time.Time { return clockNow }, DefaultPEPendingEInvoice)})
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	e.m.Register(r)
	e.h = r
	return e
}

func (e *env) call(method, path, tenant, principal, key string, body any) (int, []byte) {
	e.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if method == http.MethodPost && key == "" {
		key = uuid.NewString()
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (e *env) run(principal, asOf string, extra map[string]any) (int, map[string]any) {
	e.t.Helper()
	body := map[string]any{"legal_entity_id": e.entity, "as_of_date": asOf}
	for k, v := range extra {
		body[k] = v
	}
	code, b := e.call(http.MethodPost, base+"commands/runARGLReconciliation", e.tenant, principal, "", body)
	var out map[string]any
	require.NoError(e.t, json.Unmarshal(b, &out), string(b))
	return code, out
}

func (e *env) mustRun(asOf string, extra map[string]any) map[string]any {
	e.t.Helper()
	code, out := e.run("runner", asOf, extra)
	require.Equal(e.t, http.StatusCreated, code, "%v", out)
	return out
}

func (e *env) get(path string) (int, map[string]any) {
	e.t.Helper()
	code, b := e.call(http.MethodGet, base+path, e.tenant, "reader", "", nil)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return code, out
}

func (e *env) findings(runID string) []map[string]any {
	e.t.Helper()
	code, out := e.get("getFindings?run_id=" + runID)
	require.Equal(e.t, 200, code, "%v", out)
	var fs []map[string]any
	for _, f := range out["findings"].([]any) {
		fs = append(fs, f.(map[string]any))
	}
	return fs
}

func kindsOf(fs []map[string]any) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f["kind"].(string))
	}
	return out
}

// ---- seeding ----

// invoice seeds an ISSUED AR-04 invoice (gross = net, no tax) and, when withItem, its AR-07 open item through the port.
func (e *env) invoice(gross, currency string, withItem bool) string {
	e.t.Helper()
	e.seq++
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `INSERT INTO ar04_invoices (invoice_id, tenant_id, legal_entity_id, customer_party_id, currency, lifecycle_state, accounting_state, invoice_number, numbering_series,
			sequence_no, supply_date, issue_date, due_date, net_total, tax_total, gross_total, content_hash, approved_content_hash, approved_by, approved_at, created_by, submitted_by,
			submitted_at, issued_by, issued_at, issued_canonical, issued_content_hash, open_item_id)
		VALUES ($1,$2,$3,$4,$5,'ISSUED','QUEUED',$6,'INV',$7,'2026-09-01','2026-09-10','2026-10-10',$8,0,$8,repeat('a',64),repeat('a',64),'appr',now(),'maker','sub',now(),
			'iss','2026-09-10T09:00:00Z','{}',repeat('b',64),'OI')`, id, e.tenant, e.entity, e.party, currency, fmt.Sprintf("INV-%s-%d", id[:4], e.seq), e.seq, gross)
	require.NoError(e.t, err)
	if withItem {
		e.openItem(id, gross, currency)
	}
	return id
}

func (e *env) caller() kit.Caller {
	return kit.Caller{TenantID: e.tenant, PrincipalID: "seed", CorrelationID: "c", LegalEntityID: e.entity}
}

func (e *env) openItem(srcID, amount, currency string) kit.OpenItemRef {
	e.t.Helper()
	var ref kit.OpenItemRef
	require.NoError(e.t, e.db.WithTx(context.Background(), e.tenant, func(tx pgx.Tx) (err error) {
		ref, err = e.ar07m.CreateFromDocument(context.Background(), tx, e.caller(), kit.OpenItemFromDocument{DocumentKind: kit.DocInvoice, SourceDocumentID: srcID,
			DocumentNumber: "N-" + srcID[:6], CustomerPartyID: e.party, Currency: currency, OriginalAmount: kit.MustAmount(amount),
			IssueDate: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)})
		return
	}))
	return ref
}

func (e *env) adjust(itemID, kind, amount string) {
	e.t.Helper()
	require.NoError(e.t, e.db.WithTx(context.Background(), e.tenant, func(tx pgx.Tx) error {
		_, err := e.ar07m.ApplyAdjustment(context.Background(), tx, e.caller(), kit.OpenItemAdjustment{OpenItemID: itemID, Kind: kind, Amount: kit.MustAmount(amount),
			SourceRefType: kind, SourceRefID: "ref-" + uuid.NewString(), EventKey: uuid.NewString(), Reason: "test"})
		return err
	}))
}

func (e *env) queue(sourceType, eventID, status string, age time.Duration) {
	e.t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO ar_accounting_posting_requests (request_id, tenant_id, legal_entity_id, source_type, source_id, source_event_id,
		request_payload, principal_id, status, created_at) VALUES (gen_random_uuid(),$1,$2,$3,$4,$4,'{}','p',$5,$6)`, e.tenant, e.entity, sourceType, eventID, status, clockNow.Add(-age))
	require.NoError(e.t, err)
}

func (e *env) note(kind, gross, currency string) string {
	e.t.Helper()
	e.seq++
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `INSERT INTO ar06_corrections (correction_id, tenant_id, legal_entity_id, customer_party_id, kind, source_invoice_id, currency, state, accounting_state,
			reason_code, supply_date, net_total, tax_total, gross_total, content_hash, approved_content_hash, approved_by, approved_at, created_by, submitted_by, submitted_at,
			document_number, numbering_series, sequence_no, issue_date, due_date, issued_by, issued_at, issued_canonical, issued_content_hash, open_item_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'ISSUED','QUEUED','R','2026-09-01',$8,0,$8,repeat('a',64),repeat('a',64),'appr',now(),'maker','sub',now(),
			$9,'CN',$10,'2026-09-12','2026-10-12','iss','2026-09-12T09:00:00Z','{}',repeat('b',64),'OI')`, id, e.tenant, e.entity, e.party, kind, uuid.NewString(), currency, gross,
		fmt.Sprintf("%s-%d", kind[:2], e.seq), e.seq)
	require.NoError(e.t, err)
	return id
}

func (e *env) receipt(amount string, applied string) string {
	e.t.Helper()
	id := uuid.NewString()
	state := "UNAPPLIED"
	if applied != "0" {
		state = "PARTIALLY_APPLIED"
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO ar08_receipts (receipt_id, tenant_id, legal_entity_id, bank_transaction_id, settlement_ref, currency, amount, applied_amount, unapplied_amount,
		value_date, state, registered_by) VALUES ($1,$2,$3,gen_random_uuid(),$4,'USD',$5,$6,$5::numeric-$6::numeric,'2026-09-15',$7,'reg')`, id, e.tenant, e.entity, "SET-"+id[:6], amount, applied, state)
	require.NoError(e.t, err)
	return id
}

func (e *env) legacy(amount, status string) string {
	e.t.Helper()
	e.seq++
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `INSERT INTO customer_invoices (invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number, amount, currency_code, due_date, status,
		created_by_principal_id, sent_by_principal_id, correlation_id, created_at, sent_at, invoice_date, supply_date, net_amount, tax_amount)
		VALUES ($1,$2,$3,'cust-1',$4,$5::numeric,'USD','2026-12-31',$6,'creator',$7,$8,'2026-09-05T12:00:00Z',$9,'2026-09-05','2026-09-05',$5::numeric,0)`,
		id, e.tenant, e.entity, fmt.Sprintf("L-%d-%s", e.seq, id[:4]), amount, status, nilIf(status == "ISSUED", "sender"), "corr-"+id, nilTime(status == "ISSUED"))
	require.NoError(e.t, err)
	return id
}

func nilIf(isNil bool, v string) any {
	if isNil {
		return nil
	}
	return v
}
func nilTime(isNil bool) any {
	if isNil {
		return nil
	}
	return time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
}

// postAll queues a POSTED row for every issued fact of the entity so only the check under test can fail.
func (e *env) postAll(ids ...string) {
	for _, id := range ids {
		e.queue("INVOICE", id, kit.PostingPosted, time.Hour)
	}
}

func rowCount(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}
