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

// openTestPool connects to a real Postgres and reapplies the migration from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set.
//
// WARNING, and the reason for the guard below: this DROPs purchase_orders,
// purchase_order_amendments and the po_number sequence. Point TEST_DATABASE_URL
// at the `purchase_order` database a running service uses and it silently
// deletes that register — which happened once to accounts-payable during this
// platform's console work, and afterwards the loss looks like a service bug
// rather than a test.
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

	// Amendments first — it references the orders table.
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS purchase_order_amendments CASCADE;`)
	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS purchase_orders CASCADE;`)
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

func newTestOrder(tenantID string) *domain.PurchaseOrder {
	return &domain.PurchaseOrder{
		PurchaseOrderID:     uuid.New().String(),
		TenantID:            tenantID,
		LegalEntityID:       uuid.New().String(),
		TotalAmount:         12500,
		CurrencyCode:        "GBP",
		IssuedByPrincipalID: "test-buyer",
		CorrelationID:       "corr-" + uuid.New().String(),
	}
}

func TestPgStore_CreateOrder_And_GetOrder(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o := newTestOrder(tenantID)
	created, err := s.CreateOrder(ctx, o)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if !created {
		t.Fatal("expected created=true for a first insert")
	}
	if !strings.HasPrefix(o.PONumber, "PO-") {
		t.Errorf("po_number = %q, want a PO- prefixed number from the sequence", o.PONumber)
	}
	if o.Version != 1 {
		t.Errorf("version = %d, want 1 on issue", o.Version)
	}

	got, err := s.GetOrder(ctx, o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got == nil {
		t.Fatal("expected to read back the order just created")
	}
	if got.Status != domain.OrderStatusIssued {
		t.Errorf("status = %q, want ISSUED", got.Status)
	}
	if got.TotalAmount != o.TotalAmount {
		t.Errorf("total_amount = %v, want %v", got.TotalAmount, o.TotalAmount)
	}
	if got.ClosedByPrincipalID != nil || got.ClosedAt != nil {
		t.Error("a freshly ISSUED order carries closure fields")
	}
}

// Issue is idempotent on (tenant_id, correlation_id) — 201 real, 200 replay.
// A replay that created a SECOND order would double-commit the spend, so this
// also guards against re-publishing purchase.order.issued.
func TestPgStore_CreateOrder_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestOrder(tenantID)
	created, err := s.CreateOrder(ctx, first)
	if err != nil || !created {
		t.Fatalf("first CreateOrder: created=%v err=%v", created, err)
	}

	replay := newTestOrder(tenantID)
	replay.CorrelationID = first.CorrelationID
	replay.TotalAmount = 999999

	created, err = s.CreateOrder(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateOrder: %v", err)
	}
	if created {
		t.Fatal("expected created=false on a replayed correlation_id")
	}
	if replay.PurchaseOrderID != first.PurchaseOrderID {
		t.Errorf("replay resolved to %s, want the original %s",
			replay.PurchaseOrderID, first.PurchaseOrderID)
	}
	if replay.TotalAmount != first.TotalAmount {
		t.Errorf("replay returned total %v, want the original %v — a replay's body "+
			"must not restate the committed amount", replay.TotalAmount, first.TotalAmount)
	}
	if replay.PONumber != first.PONumber {
		t.Errorf("replay returned po_number %q, want %q", replay.PONumber, first.PONumber)
	}
}

func TestPgStore_GetOrder_OtherTenant_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()

	o := newTestOrder(owner)
	if _, err := s.CreateOrder(svcmiddleware.WithTenant(context.Background(), owner), o); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	intruder := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	got, err := s.GetOrder(intruder, o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("cross-tenant GetOrder returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("another tenant's order was readable")
	}
}

// purchase_order_id is a uuid column, so a mistyped id dies in the driver as
// 22P02. Unmapped it reached the caller as 503 store_unavailable, which reads as
// an outage rather than a typo — this service's known behaviour before the fix.
func TestPgStore_GetOrder_MalformedUUID_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), uuid.New().String())

	got, err := s.GetOrder(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("malformed purchase_order_id returned an error (%v) — it must read as "+
			"absent, because a store failure is indistinguishable from an outage", err)
	}
	if got != nil {
		t.Fatal("malformed purchase_order_id somehow matched a row")
	}
}

func TestPgStore_AmendAndClose_MalformedUUID_IsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	if _, err := s.AmendOrder(ctx, tenantID, "not-a-uuid", 100, "reason", "actor"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Errorf("AmendOrder err = %v, want ErrOrderNotFound — invalid_transition would "+
			"assert the order exists in the wrong state", err)
	}
	if _, err := s.CloseOrder(ctx, tenantID, "not-a-uuid", "actor"); !errors.Is(err, domain.ErrOrderNotFound) {
		t.Errorf("CloseOrder err = %v, want ErrOrderNotFound", err)
	}
}

// Amending restates the total and appends an immutable amendment row. It must
// NOT change status, and the ledger must record the before and after values —
// before this ledger was readable, `version` was the only trace an amendment
// had ever happened.
func TestPgStore_AmendOrder_RecordsLedgerAndKeepsStatus(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o := newTestOrder(tenantID)
	if _, err := s.CreateOrder(ctx, o); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	amended, err := s.AmendOrder(ctx, tenantID, o.PurchaseOrderID, 20000, "scope increased", "amender-1")
	if err != nil {
		t.Fatalf("AmendOrder: %v", err)
	}
	if amended.Version != 2 {
		t.Errorf("version = %d, want 2 after one amendment", amended.Version)
	}
	if amended.TotalAmount != 20000 {
		t.Errorf("total_amount = %v, want 20000", amended.TotalAmount)
	}
	if amended.Status != domain.OrderStatusIssued {
		t.Errorf("status = %q after amending, want ISSUED — amending is not a transition", amended.Status)
	}

	ledger, err := s.ListAmendments(ctx, o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("ListAmendments: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("ledger has %d rows, want 1", len(ledger))
	}
	a := ledger[0]
	if a.FromVersion != 1 || a.ToVersion != 2 {
		t.Errorf("ledger versions = %d->%d, want 1->2", a.FromVersion, a.ToVersion)
	}
	if a.PreviousTotalAmount != 12500 || a.NewTotalAmount != 20000 {
		t.Errorf("ledger amounts = %v->%v, want 12500->20000", a.PreviousTotalAmount, a.NewTotalAmount)
	}
	// The reason the operator gave is the audit record for the restatement.
	if a.Reason != "scope increased" {
		t.Errorf("ledger reason = %q, want %q", a.Reason, "scope increased")
	}

	// A second amendment appends rather than overwriting: the ledger is the
	// history, so losing the first row would erase how the order got here.
	if _, err := s.AmendOrder(ctx, tenantID, o.PurchaseOrderID, 21000, "freight added", "amender-2"); err != nil {
		t.Fatalf("second AmendOrder: %v", err)
	}
	ledger, err = s.ListAmendments(ctx, o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("ListAmendments after second amendment: %v", err)
	}
	if len(ledger) != 2 {
		t.Fatalf("ledger has %d rows after two amendments, want 2", len(ledger))
	}
	if ledger[0].FromVersion != 1 || ledger[1].FromVersion != 2 {
		t.Errorf("ledger not ordered oldest-first: %d then %d",
			ledger[0].FromVersion, ledger[1].FromVersion)
	}
}

// Closing is terminal. Re-closing and amending a CLOSED order are both refused,
// and the real code is 422 invalid_transition rather than 409.
func TestPgStore_CloseOrder_IsTerminal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	o := newTestOrder(tenantID)
	if _, err := s.CreateOrder(ctx, o); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	closed, err := s.CloseOrder(ctx, tenantID, o.PurchaseOrderID, "closer-1")
	if err != nil {
		t.Fatalf("CloseOrder: %v", err)
	}
	if closed.Status != domain.OrderStatusClosed {
		t.Errorf("status = %q, want CLOSED", closed.Status)
	}
	if closed.ClosedByPrincipalID == nil || *closed.ClosedByPrincipalID != "closer-1" {
		t.Error("closer was not recorded")
	}
	if closed.ClosedAt == nil {
		t.Error("closed_at was not recorded")
	}

	if _, err := s.CloseOrder(ctx, tenantID, o.PurchaseOrderID, "closer-2"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("re-close err = %v, want ErrInvalidTransition", err)
	}
	if _, err := s.AmendOrder(ctx, tenantID, o.PurchaseOrderID, 500, "too late", "amender"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("amend-after-close err = %v, want ErrInvalidTransition", err)
	}

	// The refused calls must not have altered the record.
	after, err := s.GetOrder(ctx, o.PurchaseOrderID)
	if err != nil || after == nil {
		t.Fatalf("GetOrder: got=%v err=%v", after, err)
	}
	if after.TotalAmount != 12500 || after.Version != 1 {
		t.Errorf("refused calls mutated the order: total=%v version=%d",
			after.TotalAmount, after.Version)
	}
}

// An unknown order and an order with no amendments are DIFFERENT facts, and the
// store must not collapse them: ListAmendments for another tenant's order
// returns nothing rather than that order's ledger.
func TestPgStore_ListAmendments_TenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	owner := uuid.New().String()
	ownerCtx := svcmiddleware.WithTenant(context.Background(), owner)

	o := newTestOrder(owner)
	if _, err := s.CreateOrder(ownerCtx, o); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if _, err := s.AmendOrder(ownerCtx, owner, o.PurchaseOrderID, 30000, "restated", "amender"); err != nil {
		t.Fatalf("AmendOrder: %v", err)
	}

	intruder := svcmiddleware.WithTenant(context.Background(), uuid.New().String())
	ledger, err := s.ListAmendments(intruder, o.PurchaseOrderID)
	if err != nil {
		t.Fatalf("cross-tenant ListAmendments returned an error: %v", err)
	}
	if len(ledger) != 0 {
		t.Fatalf("another tenant read %d amendment rows", len(ledger))
	}
}

func TestPgStore_ListOrders_FiltersAndTenantScope(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entity := uuid.New().String()

	issued := newTestOrder(tenantID)
	issued.LegalEntityID = entity
	if _, err := s.CreateOrder(ctx, issued); err != nil {
		t.Fatalf("create issued: %v", err)
	}

	toClose := newTestOrder(tenantID)
	toClose.LegalEntityID = entity
	if _, err := s.CreateOrder(ctx, toClose); err != nil {
		t.Fatalf("create to-close: %v", err)
	}
	if _, err := s.CloseOrder(ctx, tenantID, toClose.PurchaseOrderID, "closer"); err != nil {
		t.Fatalf("close: %v", err)
	}

	elsewhere := newTestOrder(tenantID)
	if _, err := s.CreateOrder(ctx, elsewhere); err != nil {
		t.Fatalf("create other-entity: %v", err)
	}

	all, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("ListOrders unfiltered: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list returned %d orders, want 3", len(all))
	}

	closedOnly, err := s.ListOrders(ctx, domain.ListOrdersFilter{
		TenantID: tenantID, Status: string(domain.OrderStatusClosed),
	})
	if err != nil {
		t.Fatalf("ListOrders by status: %v", err)
	}
	if len(closedOnly) != 1 || closedOnly[0].PurchaseOrderID != toClose.PurchaseOrderID {
		t.Fatalf("status filter returned %d orders, want exactly the closed one", len(closedOnly))
	}

	byEntity, err := s.ListOrders(ctx, domain.ListOrdersFilter{
		TenantID: tenantID, LegalEntityID: entity,
	})
	if err != nil {
		t.Fatalf("ListOrders by entity: %v", err)
	}
	if len(byEntity) != 2 {
		t.Fatalf("entity filter returned %d orders, want 2", len(byEntity))
	}

	empty, err := s.ListOrders(ctx, domain.ListOrdersFilter{TenantID: uuid.New().String()})
	if err != nil {
		t.Fatalf("ListOrders for a foreign tenant: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("a foreign tenant read %d orders", len(empty))
	}
}
