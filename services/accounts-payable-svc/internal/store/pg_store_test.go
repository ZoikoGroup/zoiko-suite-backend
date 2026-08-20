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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-payable-svc/internal/middleware"
	"zoiko.io/accounts-payable-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
//
// WARNING, and the reason for requireThrowawayDatabase below: this DROPs
// vendor_invoices. Point TEST_DATABASE_URL at the `accounts_payable` database
// that a running accounts-payable-svc uses and it will silently delete that
// register — which is exactly what happened once during this service's console
// work, wiping a seeded demo set mid-session. The loss looks like a service bug
// afterwards, not like a test, so the database name is checked before anything
// is dropped.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS vendor_invoices CASCADE;`)

	// Every *.up.sql, sorted, rather than a list written out here.
	//
	// The list stopped at 000003, so this suite has been running against a
	// schema no deployment has: 000004 is the one that FORCEs row-level
	// security, which is the migration a store test most needs applied.
	// Globbing means the next migration lands here too.
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

// scoped runs a verification query with app.tenant_id installed, exactly as the
// store installs it for every statement it makes.
//
// The assertions here deliberately query the tables directly rather than
// through the store: a write must not be verified by the code that performed
// it. But a raw query carries no tenant scope, and once the row-level security
// policy actually applies — which it does the moment the service connects as
// something other than a superuser — an unscoped SELECT returns NO rows, and
// the assertion reports "this row was never written" about a row that was.
//
// Transaction-local, so no stale tenant is left on the pooled connection to
// silently scope a later query to the wrong tenant.
func scoped(t *testing.T, pool *pgxpool.Pool, tenantID string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("verification tx begin failed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		t.Fatalf("installing the verification tenant scope failed: %v", err)
	}
	fn(tx)
}

// requireThrowawayDatabase fails the test unless the DSN's database name marks
// it as disposable. Two names are legitimate: CI points TEST_DATABASE_URL at
// `testdb`, and the local convention is `accounts_payable_test`. Both contain
// "test"; the live `accounts_payable` database does not, which is the case this
// guard exists to catch.
//
// The name is taken from the parsed DSN rather than matched against the whole
// string, so a host or password that happens to contain "test" cannot vouch for
// a live database. Anything that does not parse as a URL is refused rather than
// waved through — the suite DROPs tables, so an unreadable target is not a
// target worth guessing at.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs vendor_invoices. Use accounts_payable_test (or CI's "+
			"testdb), not accounts_payable.", dbName)
	}
}

func newTestInvoice(tenantID string) *domain.VendorInvoice {
	return &domain.VendorInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		VendorID:             "vendor-1",
		InvoiceNumber:        "INV-" + uuid.New().String()[:8],
		Amount:               1000,
		CurrencyCode:         "USD",
		DueDate:              time.Now().Add(30 * 24 * time.Hour),
		Status:               domain.InvoiceStatusReceived,
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-" + uuid.New().String(),
	}
}

// The (tenant_id, vendor_id, invoice_number) UNIQUE constraint has existed since
// 000001, but nothing recognised its violation: SQLSTATE 23505 fell through to
// the generic branch and the API answered 503 `store_unavailable`. Booking the
// same vendor invoice twice is a caller mistake, not an outage.
//
// This is the test the fix hangs on — the mapping is by constraint NAME, so a
// renamed constraint in a future migration silently restores the old behaviour
// unless this fails.
func TestPgStore_CreateInvoice_DuplicateInvoiceNumber_IsDistinguishable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, first); err != nil {
		t.Fatalf("first CreateInvoice failed: %v", err)
	}

	// Same vendor and number, but a fresh correlation_id — so this is a genuine
	// second submission, not the idempotent replay of the first.
	second := newTestInvoice(tenantID)
	second.VendorID = first.VendorID
	second.InvoiceNumber = first.InvoiceNumber

	_, err := s.CreateInvoice(ctx, second)
	if err == nil {
		t.Fatal("expected a duplicate (vendor, invoice_number) to be refused")
	}
	if !errors.Is(err, domain.ErrDuplicateInvoiceNumber) {
		t.Fatalf("expected ErrDuplicateInvoiceNumber, got %v", err)
	}
}

// A different vendor may reuse a number: the constraint is per (tenant, vendor),
// and two vendors numbering their invoices "INV-001" is ordinary.
func TestPgStore_CreateInvoice_SameNumberDifferentVendor_Allowed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	first := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, first); err != nil {
		t.Fatalf("first CreateInvoice failed: %v", err)
	}

	second := newTestInvoice(tenantID)
	second.VendorID = "a-different-vendor"
	second.InvoiceNumber = first.InvoiceNumber

	created, err := s.CreateInvoice(ctx, second)
	if err != nil {
		t.Fatalf("a different vendor must be allowed to reuse a number: %v", err)
	}
	if !created {
		t.Fatal("expected a real insert, not a replay")
	}
}

// A malformed id cannot name a row, so it is absent. It used to die inside the
// pgx driver as SQLSTATE 22P02 and surface as 503 — a typo in a URL reading as an
// outage.
func TestPgStore_GetInvoice_MalformedUUID_ReadsAsAbsent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	ctx := svcmiddleware.WithTenant(context.Background(), uuid.New().String())

	got, err := s.GetInvoice(ctx, "not-a-uuid")
	if err != nil {
		t.Fatalf("a malformed id must not be an error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil invoice, got %+v", got)
	}
}

// Same for a transition: not found, not unavailable. Kept apart from
// invalid_transition, since "no such invoice" and "wrong state" differ.
func TestPgStore_TransitionInvoice_MalformedUUID_IsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	err := s.TransitionInvoice(ctx, tenantID, "not-a-uuid",
		domain.InvoiceStatusReceived, domain.InvoiceStatusValidated, "principal-1")

	if !errors.Is(err, domain.ErrInvoiceNotFound) {
		t.Fatalf("expected ErrInvoiceNotFound, got %v", err)
	}
}

// The scan targets and the column list are derived from one slice, and a read of
// every column is what proves they are still in step. This is the policy-svc
// defect shape: there, one query had drifted to 10 of 12 columns, the two it
// dropped stayed nil and serialised as null, and every test still passed.
func TestPgStore_ReadShape_EveryColumnIsPopulated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}
	// Walk the whole lifecycle so the three actor and three timestamp columns are
	// all non-null by the end — a dropped column reads as nil, which is exactly
	// what an un-reached stage also looks like.
	for _, hop := range []struct{ from, to domain.InvoiceStatus }{
		{domain.InvoiceStatusReceived, domain.InvoiceStatusValidated},
		{domain.InvoiceStatusValidated, domain.InvoiceStatusApproved},
		{domain.InvoiceStatusApproved, domain.InvoiceStatusPaymentRequested},
	} {
		if err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, hop.from, hop.to, "principal-"+string(hop.to)); err != nil {
			t.Fatalf("transition %s -> %s failed: %v", hop.from, hop.to, err)
		}
	}

	assertFullyPopulated := func(t *testing.T, label string, got *domain.VendorInvoice) {
		t.Helper()
		if got.InvoiceID == "" || got.TenantID == "" || got.LegalEntityID == "" {
			t.Fatalf("%s: identity columns not populated: %+v", label, got)
		}
		if got.VendorID == "" || got.InvoiceNumber == "" || got.CurrencyCode == "" {
			t.Fatalf("%s: descriptive columns not populated: %+v", label, got)
		}
		if got.Amount == 0 || got.DueDate.IsZero() || got.CreatedAt.IsZero() {
			t.Fatalf("%s: amount/date columns not populated: %+v", label, got)
		}
		if got.CorrelationID == "" || got.CreatedByPrincipalID == "" {
			t.Fatalf("%s: provenance columns not populated: %+v", label, got)
		}
		if got.ValidatedByPrincipalID == nil || got.ApprovedByPrincipalID == nil || got.PaymentRequestedByPrincipalID == nil {
			t.Fatalf("%s: actor columns not populated after a full lifecycle: %+v", label, got)
		}
		if got.ValidatedAt == nil || got.ApprovedAt == nil || got.PaymentRequestedAt == nil {
			t.Fatalf("%s: transition timestamps not populated after a full lifecycle: %+v", label, got)
		}
	}

	// Both read paths, because they are separate SELECTs that must agree.
	got, err := s.GetInvoice(ctx, inv.InvoiceID)
	if err != nil || got == nil {
		t.Fatalf("GetInvoice failed: %v", err)
	}
	assertFullyPopulated(t, "GetInvoice", got)

	list, err := s.ListInvoices(ctx, domain.ListInvoicesFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("ListInvoices failed: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 invoice, got %d", len(list))
	}
	assertFullyPopulated(t, "ListInvoices", &list[0])
}

func TestPgStore_CreateInvoice_And_GetInvoice(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	got, err := s.GetInvoice(ctx, inv.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected invoice to be found")
	}
	if got.Status != domain.InvoiceStatusReceived {
		t.Fatalf("expected status RECEIVED, got %s", got.Status)
	}
}

// TestPgStore_CreateInvoice_RetriedCorrelationID_IsIdempotent proves the
// idempotency guarantee against a REAL Postgres unique index — this is the
// exact scenario a network-timeout-triggered client retry produces, and it
// must resolve to the original invoice, never a duplicate liability.
func TestPgStore_CreateInvoice_RetriedCorrelationID_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv1 := newTestInvoice(tenantID)
	inv1.CorrelationID = "corr-retry-1"
	created1, err := s.CreateInvoice(ctx, inv1)
	if err != nil {
		t.Fatalf("first CreateInvoice failed: %v", err)
	}
	if !created1 {
		t.Fatal("expected created=true on the first call")
	}

	// Simulate a client retry: a fresh invoice (new InvoiceID, as a real
	// client would generate) but the SAME correlation_id.
	inv2 := newTestInvoice(tenantID)
	inv2.CorrelationID = "corr-retry-1"
	created2, err := s.CreateInvoice(ctx, inv2)
	if err != nil {
		t.Fatalf("retried CreateInvoice failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-liability bug if it's true")
	}
	if inv2.InvoiceID != inv1.InvoiceID {
		t.Fatalf("retried call resolved to a different invoice_id (%s) than the original (%s)", inv2.InvoiceID, inv1.InvoiceID)
	}

	var count int
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM vendor_invoices WHERE tenant_id = $1 AND correlation_id = $2`,
			tenantID, "corr-retry-1").Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
	})
	if count != 1 {
		t.Fatalf("DUPLICATE LIABILITY: expected exactly 1 vendor_invoices row for this correlation_id, got %d", count)
	}
}

func TestPgStore_TransitionInvoice_WrongFromStatus_Rejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	// Invoice is RECEIVED — attempting VALIDATED -> APPROVED (wrong
	// fromStatus) must be rejected as a no-op (0 rows affected), never
	// silently succeed.
	err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, domain.InvoiceStatusValidated, domain.InvoiceStatusApproved, "test-admin")
	if err == nil {
		t.Fatal("expected an error transitioning from the wrong fromStatus, got nil")
	}

	got, _ := s.GetInvoice(ctx, inv.InvoiceID)
	if got.Status != domain.InvoiceStatusReceived {
		t.Fatalf("invoice status must remain unchanged after a rejected transition, got %s", got.Status)
	}
}

func TestPgStore_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	invA := newTestInvoice(tenantA)
	if _, err := s.CreateInvoice(ctxA, invA); err != nil {
		t.Fatalf("CreateInvoice (tenant A) failed: %v", err)
	}

	// Query tenant A's invoice while scoped to tenant B's context — this must
	// be hidden entirely, proving tenant isolation actually holds (not just
	// that the column exists). This is the exact class of check that caught
	// a real cross-tenant leak in general-ledger-svc via CI — every method
	// here filters explicitly by tenant_id for that reason, RLS alone is
	// insufficient given this pool connects as a Postgres superuser.
	got, err := s.GetInvoice(ctxB, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under tenant B's context returned an error rather than a clean not-found: %v", err)
	}
	if got != nil {
		t.Fatal("tenant isolation failure: tenant B's session was able to read tenant A's invoice")
	}

	// Also verify TransitionInvoice can't be used to mutate another tenant's
	// row by supplying a mismatched tenantID explicitly.
	err = s.TransitionInvoice(ctxB, tenantB, invA.InvoiceID, domain.InvoiceStatusReceived, domain.InvoiceStatusValidated, "attacker")
	if err == nil {
		t.Fatal("tenant isolation failure: TransitionInvoice allowed tenant B to transition tenant A's invoice")
	}

	// Sanity: the same lookup under the correct tenant's context succeeds.
	gotA, err := s.GetInvoice(ctxA, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under the correct tenant context failed: %v", err)
	}
	if gotA == nil {
		t.Fatal("expected invoice to be found under its own tenant's context")
	}
}

func TestPgStore_ListInvoices_TenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)

	invA := newTestInvoice(tenantA)
	if _, err := s.CreateInvoice(ctxA, invA); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	listA, err := s.ListInvoices(ctxA, domain.ListInvoicesFilter{TenantID: tenantA})
	if err != nil {
		t.Fatalf("ListInvoices (tenant A) failed: %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("expected 1 invoice for tenant A, got %d", len(listA))
	}

	listB, err := s.ListInvoices(ctxA, domain.ListInvoicesFilter{TenantID: tenantB})
	if err != nil {
		t.Fatalf("ListInvoices (tenant B) failed: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("expected 0 invoices for tenant B, got %d", len(listB))
	}
}
