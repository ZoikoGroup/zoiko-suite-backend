package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies every migration from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
//
// WARNING, and the reason for the guard below: this DROPs purchase_orders and
// every table that hangs off it. Point TEST_DATABASE_URL at the `purchase_order`
// database a running service uses and it silently deletes that register — which
// happened once to accounts-payable during this platform's console work, and
// afterwards the loss looks like a service bug rather than a test.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	requireThrowawayDatabase(t, dsn)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	// Children first; CASCADE for the FKs and the triggers that hang off them.
	for _, tbl := range []string{
		"idempotency_keys", "outbox_events", "purchase_order_events", "purchase_order_revisions",
		"purchase_order_progress", "purchase_order_lines", "purchase_order_amendments", "purchase_orders",
	} {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE;`)
	}
	_, _ = pool.Exec(ctx, `DROP SEQUENCE IF EXISTS purchase_order_number_seq CASCADE;`)

	// Every *.up.sql, sorted, rather than a list written out here.
	//
	// The list here was hardcoded and had fallen behind the directory, so this
	// suite was applying a schema no deployment has -- in particular without the
	// FORCE row-level security migration, which is the one a store test most
	// needs in place. Globbing means the next migration is picked up without
	// anyone remembering to come back to this file. Same shape as
	// accounts-receivable-svc.
	migrationDir := filepath.Join(base, "../../deployments/migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to glob migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatalf("no *.up.sql migrations found under %s", migrationDir)
	}
	sort.Strings(migrations)

	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(migration), err)
		}
	}

	return pool
}

// openStore returns the store under test. When TEST_APP_DATABASE_URL names an
// ordinary NOSUPERUSER NOBYPASSRLS role (as docker-compose runs this service),
// the store runs as THAT role, so row-level security, the grants and the
// triggers are exercised exactly as in a deployment; migrations always run as the
// owner. Without it, the store runs as the owner, as the older suites did.
func openStore(t *testing.T) (*store.PgStore, *pgxpool.Pool) {
	t.Helper()
	owner := openTestPool(t)
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		requireThrowawayDatabase(t, appDSN)
		app, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatalf("failed to connect as the app role: %v", err)
		}
		t.Cleanup(app.Close)
		return store.New(app, zap.NewNop()), owner
	}
	return store.New(owner, zap.NewNop()), owner
}

// requireThrowawayDatabase fails the test unless the DSN's database name marks
// it as disposable. Two names are legitimate: CI points TEST_DATABASE_URL at
// `testdb`, and the local convention is `purchase_order_test`. Both contain
// "test"; the live `purchase_order` database does not, which is the case this
// guard exists to catch.
//
// The name is taken from the parsed DSN rather than matched against the whole
// string, so a host or password that happens to contain "test" cannot vouch for
// a live database. Anything that does not parse as a URL is refused rather than
// waved through — the suite DROPs tables, so an unreadable target is not a
// target worth guessing at. Same helper as the other store suites here.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs purchase_orders. Use purchase_order_test "+
			"(or CI's testdb), not purchase_order.", dbName)
	}
}

// legacyInput is a header-only direct-issue request with a verified approval
// basis: the shape of the original POST /v1/purchase-orders.
func legacyInput(tenantID string) store.IssuedInput {
	return store.IssuedInput{
		CreateDraftInput: store.CreateDraftInput{
			TenantID: tenantID, LegalEntityID: uuid.New().String(), CurrencyCode: "GBP",
			CorrelationID: "corr-" + uuid.New().String(), PreparedBy: "test-buyer", TotalAmount: 12500,
		},
		ApprovalBasis: domain.ApprovalBasisProcurementCase, ApprovalRef: "case-1", ApprovedBy: "case-approver",
	}
}

func tctx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

func TestPgStore_CreateIssued_And_GetOrder(t *testing.T) {
	s, _ := openStore(t)
	tenantID := uuid.New().String()
	ctx := tctx(tenantID)

	d, created, err := s.CreateIssued(ctx, legacyInput(tenantID))
	if err != nil {
		t.Fatalf("CreateIssued: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a first insert")
	}
	if !strings.HasPrefix(d.PONumber, "PO-") {
		t.Errorf("po_number = %q, want a PO- prefixed number from the sequence", d.PONumber)
	}
	if d.Version != 1 || d.Revision != 1 {
		t.Errorf("version/revision = %d/%d, want 1/1 on issue", d.Version, d.Revision)
	}
	if d.Status != domain.OrderStatusIssued {
		t.Errorf("status = %q, want ISSUED", d.Status)
	}
	if d.ApprovalBasis != domain.ApprovalBasisProcurementCase || d.ApprovedByPrincipalID == nil || *d.ApprovedByPrincipalID != "case-approver" {
		t.Errorf("approval evidence not recorded: basis=%q approver=%v", d.ApprovalBasis, d.ApprovedByPrincipalID)
	}
	if d.IssuedAt == nil || d.IssuedByPrincipalID != "test-buyer" {
		t.Errorf("issue evidence not recorded: at=%v by=%q", d.IssuedAt, d.IssuedByPrincipalID)
	}

	got, err := s.GetOrder(ctx, d.PurchaseOrderID)
	if err != nil || got == nil {
		t.Fatalf("GetOrder: got=%v err=%v", got, err)
	}
	if got.TotalAmount != 12500 {
		t.Errorf("total_amount = %v, want 12500", got.TotalAmount)
	}
	if got.ClosedByPrincipalID != nil || got.ClosedAt != nil {
		t.Error("a freshly ISSUED order carries closure fields")
	}
}

// Issue is idempotent on (tenant_id, correlation_id) — 201 real, 200 replay.
// A replay that created a SECOND order would double-commit the spend.
func TestPgStore_CreateIssued_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	s, owner := openStore(t)
	tenantID := uuid.New().String()
	ctx := tctx(tenantID)

	in := legacyInput(tenantID)
	first, created, err := s.CreateIssued(ctx, in)
	if err != nil || !created {
		t.Fatalf("first CreateIssued: created=%v err=%v", created, err)
	}

	replay := legacyInput(tenantID)
	replay.CorrelationID = in.CorrelationID
	replay.TotalAmount = 999999
	again, created, err := s.CreateIssued(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateIssued: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a replayed correlation_id")
	}
	if again.PurchaseOrderID != first.PurchaseOrderID {
		t.Errorf("replay resolved to %s, want the original %s", again.PurchaseOrderID, first.PurchaseOrderID)
	}
	if again.TotalAmount != first.TotalAmount || again.PONumber != first.PONumber {
		t.Errorf("replay restated the committed order: total %v po %q", again.TotalAmount, again.PONumber)
	}

	// ...and published nothing a second time: Created, Submitted, Approved and
	// Issued (+ its legacy alias) from the first call only.
	var n int
	if err := owner.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, first.PurchaseOrderID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("outbox has %d events for the order, want 5 (Created, Submitted, Approved, Issued, purchase.order.issued)", n)
	}
}

func TestPgStore_GetOrder_OtherTenant_ReadsAsAbsent(t *testing.T) {
	s, _ := openStore(t)
	owner := uuid.New().String()

	d, _, err := s.CreateIssued(tctx(owner), legacyInput(owner))
	if err != nil {
		t.Fatalf("CreateIssued: %v", err)
	}

	got, err := s.GetOrder(tctx(uuid.New().String()), d.PurchaseOrderID)
	if err != nil {
		t.Fatalf("cross-tenant GetOrder returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("another tenant's order was readable")
	}
}

// purchase_order_id is a uuid column, so a mistyped id dies in the driver as
// 22P02. Unmapped it reached the caller as 503 store_unavailable, which reads as
// an outage rather than a typo.
func TestPgStore_GetOrder_MalformedUUID_ReadsAsAbsent(t *testing.T) {
	s, _ := openStore(t)
	ctx := tctx(uuid.New().String())

	got, err := s.GetOrder(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("malformed purchase_order_id returned an error (%v) — it must read as "+
			"absent, because a store failure is indistinguishable from an outage", err)
	}
	if got != nil {
		t.Fatal("malformed purchase_order_id somehow matched a row")
	}
}

func TestPgStore_AmendAndTransition_MalformedUUID_IsNotFound(t *testing.T) {
	s, _ := openStore(t)
	tenantID := uuid.New().String()
	ctx := tctx(tenantID)

	if _, err := s.AmendOrder(ctx, tenantID, "not-a-uuid", "actor", domain.AmendOrderRequest{NewTotalAmount: 100, Reason: "reason"}); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Errorf("AmendOrder err = %v, want ErrOrderNotFound — invalid_transition would "+
			"assert the order exists in the wrong state", err)
	}
	if _, err := s.Transition(ctx, store.TransitionInput{TenantID: tenantID, OrderID: "not-a-uuid", Actor: "actor", Command: store.CmdClose}); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Errorf("Transition err = %v, want ErrOrderNotFound", err)
	}
}

// Closing is terminal. Re-closing and amending a CLOSED order are both refused,
// and the refused calls must not alter the record.
func TestPgStore_Close_IsTerminal(t *testing.T) {
	s, _ := openStore(t)
	tenantID := uuid.New().String()
	ctx := tctx(tenantID)

	o, _, err := s.CreateIssued(ctx, legacyInput(tenantID))
	if err != nil {
		t.Fatalf("CreateIssued: %v", err)
	}

	closed, err := s.Transition(ctx, store.TransitionInput{TenantID: tenantID, OrderID: o.PurchaseOrderID, Actor: "closer-1", Command: store.CmdClose})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != domain.OrderStatusClosed {
		t.Errorf("status = %q, want CLOSED", closed.Status)
	}
	if closed.ClosedByPrincipalID == nil || *closed.ClosedByPrincipalID != "closer-1" || closed.ClosedAt == nil {
		t.Error("closer was not recorded")
	}

	if _, err := s.Transition(ctx, store.TransitionInput{TenantID: tenantID, OrderID: o.PurchaseOrderID, Actor: "closer-2", Command: store.CmdClose}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("re-close err = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.AmendOrder(ctx, tenantID, o.PurchaseOrderID, "amender", domain.AmendOrderRequest{NewTotalAmount: 500, Reason: "too late"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("amend-after-close err = %v, want ErrInvalidTransition", err)
	}

	after, err := s.GetOrder(ctx, o.PurchaseOrderID)
	if err != nil || after == nil {
		t.Fatalf("GetOrder: got=%v err=%v", after, err)
	}
	if after.TotalAmount != 12500 || after.Status != domain.OrderStatusClosed {
		t.Errorf("refused calls mutated the order: total=%v status=%s", after.TotalAmount, after.Status)
	}
}

// An unknown order and an order with no amendments are DIFFERENT facts, and the
// store must not collapse them: ListAmendments for another tenant's order
// returns nothing rather than that order's ledger.
func TestPgStore_ListAmendments_TenantScoped(t *testing.T) {
	s, _ := openStore(t)
	owner := uuid.New().String()
	ownerCtx := tctx(owner)

	o, _, err := s.CreateIssued(ownerCtx, legacyInput(owner))
	if err != nil {
		t.Fatalf("CreateIssued: %v", err)
	}
	if _, err := s.AmendOrder(ownerCtx, owner, o.PurchaseOrderID, "amender", domain.AmendOrderRequest{NewTotalAmount: 30000, Reason: "restated"}); err != nil {
		t.Fatalf("AmendOrder: %v", err)
	}

	ledger, err := s.ListAmendments(tctx(uuid.New().String()), o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("cross-tenant ListAmendments returned an error: %v", err)
	}
	if len(ledger) != 0 {
		t.Fatalf("another tenant read %d amendment rows", len(ledger))
	}
}

func TestPgStore_ListOrders_FiltersAndTenantScope(t *testing.T) {
	s, _ := openStore(t)
	tenantID := uuid.New().String()
	ctx := tctx(tenantID)
	entity := uuid.New().String()

	issue := func(entityID string) *domain.OrderDetail {
		in := legacyInput(tenantID)
		in.LegalEntityID = entityID
		d, _, err := s.CreateIssued(ctx, in)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return d
	}
	issue(entity)
	toClose := issue(entity)
	if _, err := s.Transition(ctx, store.TransitionInput{TenantID: tenantID, OrderID: toClose.PurchaseOrderID, Actor: "closer", Command: store.CmdClose}); err != nil {
		t.Fatalf("close: %v", err)
	}
	issue(uuid.New().String())

	all, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: tenantID})
	if err != nil || len(all) != 3 {
		t.Fatalf("unfiltered list: n=%d err=%v, want 3", len(all), err)
	}
	closedOnly, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: tenantID, Status: string(domain.OrderStatusClosed)})
	if err != nil || len(closedOnly) != 1 || closedOnly[0].PurchaseOrderID != toClose.PurchaseOrderID {
		t.Fatalf("status filter: n=%d err=%v, want exactly the closed one", len(closedOnly), err)
	}
	byEntity, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: tenantID, LegalEntityID: entity})
	if err != nil || len(byEntity) != 2 {
		t.Fatalf("entity filter: n=%d err=%v, want 2", len(byEntity), err)
	}
	empty, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: uuid.New().String()})
	if err != nil || len(empty) != 0 {
		t.Fatalf("a foreign tenant read %d orders (err=%v)", len(empty), err)
	}
}
