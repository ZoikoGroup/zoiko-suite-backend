package store_test

// BNK-04 ListTransactions (GET /v1/banking/canonical-transactions) against the
// real handler, the cmd/server middleware stack and a real Postgres under
// row-level security: the store reads through a NOSUPERUSER NOBYPASSRLS role,
// so tenant isolation here is the database's, not the test's. Fixtures are
// written by the admin (superuser) pool, which is how a row can carry an
// amount whose float64 form is not exact — IngestStatement takes a float64.
// Skips (not fails) if TEST_DATABASE_URL isn't set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/banking-connector-svc/internal/authz"
	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/envelope"
	"zoiko.io/banking-connector-svc/internal/handler"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// listFixture seeds canonical transactions through the admin pool. Each call
// to txn creates its own statement line; connections and statements are one
// per (tenant, legal entity, bank account).
type listFixture struct {
	t     *testing.T
	admin *pgxpool.Pool
	conns map[string]string // tenant|entity|bankAccount -> statement_id
	seq   int
}

func (f *listFixture) statement(tenant, entity, bankAccount string) string {
	key := tenant + "|" + entity + "|" + bankAccount
	if id, ok := f.conns[key]; ok {
		return id
	}
	ctx := context.Background()
	connID, stmtID := uuid.NewString(), uuid.NewString()
	if _, err := f.admin.Exec(ctx, `INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, account_number, currency, bank_account_id)
		VALUES ($1, $2, $3, 'Test Bank', 'GB00TEST', 'USD', $4)`, connID, tenant, entity, bankAccount); err != nil {
		f.t.Fatalf("seed connection: %v", err)
	}
	if _, err := f.admin.Exec(ctx, `INSERT INTO bank_statements (statement_id, connection_id, tenant_id, statement_format, statement_date)
		VALUES ($1, $2, $3, 'BAI2', '2026-07-31')`, stmtID, connID, tenant); err != nil {
		f.t.Fatalf("seed statement: %v", err)
	}
	f.conns[key] = stmtID
	return stmtID
}

type seededTxn struct {
	tenant, entity, bankAccount, currency, amount, status string
	date                                                  time.Time
}

// txn seeds one canonical transaction and returns its id. A SUPERSEDED row is
// seeded with the NORMALIZED correction that replaced it, on the same line.
func (f *listFixture) txn(s seededTxn) string {
	ctx := context.Background()
	stmtID := f.statement(s.tenant, s.entity, s.bankAccount)
	f.seq++
	lineID, txnID := uuid.NewString(), uuid.NewString()
	if _, err := f.admin.Exec(ctx, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7)`, lineID, stmtID, s.tenant, f.seq, s.date, s.amount, s.currency); err != nil {
		f.t.Fatalf("seed line: %v", err)
	}
	insert := func(id, status string, supersededBy *string) {
		if _, err := f.admin.Exec(ctx, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, status, superseded_by)
			VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8)`, id, s.tenant, lineID, s.date, s.amount, s.currency, status, supersededBy); err != nil {
			f.t.Fatalf("seed canonical %s: %v", status, err)
		}
	}
	if s.status == domain.TxnSuperseded {
		correction := uuid.NewString()
		insert(correction, domain.TxnNormalized, nil)
		insert(txnID, domain.TxnSuperseded, &correction)
		return txnID
	}
	insert(txnID, s.status, nil)
	return txnID
}

// recordingAuthz stands in for authorization-svc: it records each decision
// asked for and denies (action, legal entity) pairs in deny.
type recordingAuthz struct {
	mu    sync.Mutex
	asked [][2]string
	deny  map[[2]string]bool
}

func (a *recordingAuthz) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d struct {
			LegalEntityID string `json:"legal_entity_id"`
			ActionType    string `json:"action_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&d)
		key := [2]string{d.ActionType, d.LegalEntityID}
		a.mu.Lock()
		a.asked = append(a.asked, key)
		denied := a.deny[key]
		a.mu.Unlock()
		outcome := "GRANTED"
		if denied {
			outcome = "DENIED"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": outcome})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type listEnv struct {
	f      *listFixture
	router chi.Router
	authz  *recordingAuthz
}

func newListEnv(t *testing.T) *listEnv {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	s := store.NewPgStore(app, store.WithEventRegion("uk"))
	az := &recordingAuthz{deny: map[[2]string]bool{}}
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.TenantContext)
	r.Use(envelope.MiddlewareWithMode(envelope.ServicePolicy(), envelope.ModeWriteStrict, nil))
	h := handler.New(s, authz.NewClient(az.server(t)), zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterBNK0304Routes(r, h, s)
	return &listEnv{f: &listFixture{t: t, admin: admin, conns: map[string]string{}}, router: r, authz: az}
}

type listResponse struct {
	Transactions []map[string]any `json:"transactions"`
	NextCursor   string           `json:"next_cursor"`
}

func (e *listEnv) list(t *testing.T, tenant string, params url.Values) (int, listResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/banking/canonical-transactions?"+params.Encode(), nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "recon-intel-svc")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out listResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func listedIDs(r listResponse) map[string]bool {
	out := map[string]bool{}
	for _, tx := range r.Transactions {
		out[tx["transaction_id"].(string)] = true
	}
	return out
}

func day(d int, hh, mm, ss int) time.Time { return time.Date(2026, 7, d, hh, mm, ss, 0, time.UTC) }

func TestListTransactions_DateWindowIsInclusiveUTCDays(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	base := seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: "10", status: domain.TxnNormalized}
	at := func(d time.Time) string { s := base; s.date = d; return e.f.txn(s) }
	before := at(day(9, 23, 59, 59))
	first := at(day(10, 0, 0, 0))
	last := at(day(20, 23, 59, 59))
	after := at(day(21, 0, 0, 0))

	code, resp, body := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"},
		"transaction_date_from": {"2026-07-10"}, "transaction_date_to": {"2026-07-20"}})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", code, body)
	}
	got := listedIDs(resp)
	if !got[first] || !got[last] || got[before] || got[after] || len(got) != 2 {
		t.Fatalf("window must include 07-10 00:00:00 and 07-20 23:59:59 only; got %v", got)
	}
	if code, _, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"},
		"transaction_date_from": {"2026-07-21"}, "transaction_date_to": {"2026-07-10"}}); code != http.StatusBadRequest {
		t.Fatalf("an inverted window must be 400, got %d", code)
	}
}

func TestListTransactions_CurrencyAndBankAccountFilters(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	seed := func(bankAccount, currency string) string {
		return e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: bankAccount, currency: currency, amount: "10",
			status: domain.TxnNormalized, date: day(15, 12, 0, 0)})
	}
	usd1, eur1 := seed("ba-1", "USD"), seed("ba-1", "EUR")
	usd2 := seed("ba-2", "USD")
	legacy := seed("", "USD") // a connection created before BNK-02 recorded a bank account

	_, resp, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "currency": {"USD"}})
	if got := listedIDs(resp); !got[usd1] || !got[usd2] || !got[legacy] || got[eur1] {
		t.Fatalf("currency=USD: got %v", got)
	}

	_, resp, _ = e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "bank_account_id": {"ba-1"}})
	if got := listedIDs(resp); len(got) != 2 || !got[usd1] || !got[eur1] {
		t.Fatalf("bank_account_id=ba-1 must return only ba-1's transactions; got %v", got)
	}
	for _, tx := range resp.Transactions {
		if tx["bank_account_id"] != "ba-1" || tx["connection_id"] == "" {
			t.Fatalf("every row names its bank account and connection: %v", tx)
		}
	}

	_, resp, _ = e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "bank_account_id": {"ba-2"}, "currency": {"USD"}})
	if got := listedIDs(resp); len(got) != 1 || !got[usd2] {
		t.Fatalf("ba-2 + USD: got %v", got)
	}

	_, resp, _ = e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}})
	for _, tx := range resp.Transactions {
		if tx["transaction_id"] == legacy && tx["bank_account_id"] != nil {
			t.Fatalf("a connection with no bank account must serialize bank_account_id as null: %v", tx)
		}
	}
	if code, _, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "currency": {"usd"}}); code != http.StatusBadRequest {
		t.Fatalf("lower-case currency must be 400, got %d", code)
	}
}

// QUARANTINED and SUPERSEDED rows are not current truth and never appear;
// the NORMALIZED correction that superseded a row does.
func TestListTransactions_OnlyNormalizedRows(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	seed := func(status string) string {
		return e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: "10",
			status: status, date: day(15, 12, 0, 0)})
	}
	normalized := seed(domain.TxnNormalized)
	quarantined := seed(domain.TxnQuarantined)
	superseded := seed(domain.TxnSuperseded)

	_, resp, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}})
	got := listedIDs(resp)
	if got[quarantined] || got[superseded] {
		t.Fatalf("QUARANTINED / SUPERSEDED rows leaked: %v", got)
	}
	if !got[normalized] || len(got) != 2 {
		t.Fatalf("expected the NORMALIZED row and the superseded row's NORMALIZED correction; got %v", got)
	}
	for _, tx := range resp.Transactions {
		if tx["status"] != domain.TxnNormalized {
			t.Fatalf("non-NORMALIZED row returned: %v", tx)
		}
	}
	if code, _, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "status": {"SUPERSEDED"}}); code != http.StatusBadRequest {
		t.Fatalf("there is no status parameter to opt into stale rows; expected 400, got %d", code)
	}
}

// Another tenant's rows never appear — not even with the same legal entity
// and bank account ids — because row-level security scopes the read.
func TestListTransactions_CrossTenantIsolation(t *testing.T) {
	e := newListEnv(t)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	seed := func(tenant string) string {
		return e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: "10",
			status: domain.TxnNormalized, date: day(15, 12, 0, 0)})
	}
	a, b := seed(tenantA), seed(tenantB)
	params := url.Values{"legal_entity_id": {"le-1"}, "bank_account_id": {"ba-1"}}

	_, respA, _ := e.list(t, tenantA, params)
	if got := listedIDs(respA); len(got) != 1 || !got[a] {
		t.Fatalf("tenant A must see only its own row; got %v", got)
	}
	_, respB, _ := e.list(t, tenantB, params)
	if got := listedIDs(respB); len(got) != 1 || !got[b] {
		t.Fatalf("tenant B must see only its own row; got %v", got)
	}
	for _, tx := range respA.Transactions {
		if tx["tenant_id"] != tenantA {
			t.Fatalf("row of another tenant returned: %v", tx)
		}
	}
}

// The amount is the exact NUMERIC(18,4) value as a four-place decimal string.
// 98765432109876.5432 has no exact float64: the nearest one prints as
// 98765432109876.55, so a route that passed the amount through float64 would
// return a different number than the bank recorded.
func TestListTransactions_AmountIsExactDecimalString(t *testing.T) {
	const exact = "98765432109876.5432"
	f, _ := strconv.ParseFloat(exact, 64)
	if rendered := strconv.FormatFloat(f, 'f', -1, 64); rendered == exact {
		t.Fatalf("test premise broken: %s round-trips through float64", exact)
	} else if fmt.Sprintf("%.4f", f) == exact {
		t.Fatalf("test premise broken: %s survives %%.4f formatting", exact)
	}

	e := newListEnv(t)
	tenant := uuid.NewString()
	big := e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: exact,
		status: domain.TxnNormalized, date: day(15, 12, 0, 0)})
	small := e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: "125.5",
		status: domain.TxnNormalized, date: day(15, 12, 0, 0)})

	req := httptest.NewRequest(http.MethodGet, "/v1/banking/canonical-transactions?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "recon-intel-svc")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// Decoded with UseNumber so that a bare JSON number could not be mistaken
	// for the string: the amount must be a JSON string.
	dec := json.NewDecoder(rec.Body)
	dec.UseNumber()
	var resp struct {
		Transactions []map[string]any `json:"transactions"`
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{big: exact, small: "125.5000"}
	for _, tx := range resp.Transactions {
		amount, isString := tx["amount"].(string)
		if !isString {
			t.Fatalf("amount must be a JSON string, got %T %v", tx["amount"], tx["amount"])
		}
		if w := want[tx["transaction_id"].(string)]; amount != w {
			t.Fatalf("amount = %q, want exactly %q", amount, w)
		}
	}
	if len(resp.Transactions) != 2 {
		t.Fatalf("expected both rows, got %d", len(resp.Transactions))
	}
}

// A full page always carries next_cursor, the last page never does, and
// following the cursor returns every row exactly once. Checked at the
// default page size (1000) with 1001 rows, and with an explicit limit.
func TestListTransactions_PaginationSignalsTruncation(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	stmtID := e.f.statement(tenant, "le-1", "ba-1")
	ctx := context.Background()
	if _, err := e.f.admin.Exec(ctx, `
		WITH l AS (
			INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency)
			SELECT gen_random_uuid()::text, $1, $2, g, '2026-07-15T12:00:00Z', 1, 'USD' FROM generate_series(1, 1001) g
			RETURNING line_id)
		INSERT INTO bank_transactions_canonical (tenant_id, statement_line_id, transaction_date, amount, currency, status)
		SELECT $2, line_id, '2026-07-15T12:00:00Z', 1, 'USD', 'NORMALIZED' FROM l`, stmtID, tenant); err != nil {
		t.Fatalf("seed 1001 rows: %v", err)
	}

	code, page, body := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}})
	if code != http.StatusOK || len(page.Transactions) != 1000 || page.NextCursor == "" {
		t.Fatalf("default page of 1001 rows: expected 1000 rows and a next_cursor, got %d rows, cursor %q (%d) %s",
			len(page.Transactions), page.NextCursor, code, body)
	}
	seen := listedIDs(page)
	_, last, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "cursor": {page.NextCursor}})
	if len(last.Transactions) != 1 || last.NextCursor != "" {
		t.Fatalf("the last page must hold the remaining row and no cursor; got %d rows, cursor %q", len(last.Transactions), last.NextCursor)
	}
	for id := range listedIDs(last) {
		if seen[id] {
			t.Fatalf("row %s returned on two pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 1001 {
		t.Fatalf("following the cursor must return every row once; got %d", len(seen))
	}

	// An exact-fit page (limit == rows remaining) ends with no cursor only
	// when nothing is left: 1001 rows at limit 1001 is one complete page.
	_, whole, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "limit": {"1001"}})
	if len(whole.Transactions) != 1001 || whole.NextCursor != "" {
		t.Fatalf("limit=1001 over 1001 rows: expected one complete page, got %d rows, cursor %q", len(whole.Transactions), whole.NextCursor)
	}
	_, small, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "limit": {"1000"}})
	if len(small.Transactions) != 1000 || small.NextCursor == "" {
		t.Fatalf("limit=1000 over 1001 rows must signal truncation; got %d rows, cursor %q", len(small.Transactions), small.NextCursor)
	}
	if code, _, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}, "cursor": {"!!!"}}); code != http.StatusBadRequest {
		t.Fatalf("a malformed cursor must be 400, got %d", code)
	}
}

// The route asks authorization-svc for bank.transaction.read on the legal
// entity — a real decision, not just a principal header — and a denial is
// 403 with nothing read.
func TestListTransactions_Authorization(t *testing.T) {
	e := newListEnv(t)
	tenant := uuid.NewString()
	e.f.txn(seededTxn{tenant: tenant, entity: "le-1", bankAccount: "ba-1", currency: "USD", amount: "10",
		status: domain.TxnNormalized, date: day(15, 12, 0, 0)})

	if code, _, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-1"}}); code != http.StatusOK {
		t.Fatalf("granted: expected 200, got %d", code)
	}
	e.authz.mu.Lock()
	asked := append([][2]string(nil), e.authz.asked...)
	e.authz.mu.Unlock()
	if len(asked) == 0 || asked[len(asked)-1] != [2]string{handler.BANK_TRANSACTION_READ, "le-1"} {
		t.Fatalf("expected a bank.transaction.read decision on le-1, got %v", asked)
	}

	e.authz.deny[[2]string{handler.BANK_TRANSACTION_READ, "le-2"}] = true
	if code, resp, _ := e.list(t, tenant, url.Values{"legal_entity_id": {"le-2"}}); code != http.StatusForbidden || len(resp.Transactions) != 0 {
		t.Fatalf("denied: expected 403 and nothing returned, got %d", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/banking/canonical-transactions?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-Id", tenant)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", rec.Code)
	}
	for _, bad := range []url.Values{{}, {"legal_entity_id": {"le-1"}, "account": {"ba-1"}}} {
		if code, _, _ := e.list(t, tenant, bad); code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, code)
		}
	}
}
