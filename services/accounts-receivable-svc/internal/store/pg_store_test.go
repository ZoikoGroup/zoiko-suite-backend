//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from
// a clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set — same
// convention as every other service in this platform.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	base := filepath.Dir(filename)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS customer_invoices CASCADE;`)

	// Every *.up.sql, sorted, rather than a list written out here.
	//
	// The list used to be hardcoded and stopped at 000002, so these tests ran
	// against a schema no deployment has ever had, and the next migration would
	// have been skipped in silence — which is exactly how obligations-svc and
	// document-vault-svc went red in CI having been green locally. Globbing
	// means 000003 (FORCE row-level security) is picked up without anyone
	// remembering to come back here, and so is 000004.
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
// The assertions below deliberately query the table directly rather than through
// the store: a write must not be verified by the code that performed it. But a
// raw query carries no tenant scope, and now that this service connects as an
// ordinary NOSUPERUSER NOBYPASSRLS role, an unscoped SELECT returns NO rows —
// so the assertion reported "no invoice was written" about an invoice that was.
//
// Transaction-local, so nothing is left on the pooled connection. That matters
// here specifically: a custom GUC set non-locally persists on the connection,
// and TestPgStore_Policy_ConstrainsAnOrdinaryRole below sets one that way.
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

func newTestInvoice(tenantID string) *domain.CustomerInvoice {
	return &domain.CustomerInvoice{
		InvoiceID:            uuid.New().String(),
		TenantID:             tenantID,
		LegalEntityID:        uuid.New().String(),
		CustomerID:           "customer-1",
		InvoiceNumber:        "INV-" + uuid.New().String()[:8],
		Amount:               1500.50,
		CurrencyCode:         "USD",
		DueDate:              time.Now().Add(15 * 24 * time.Hour),
		Status:               domain.InvoiceStatusIssued,
		CreatedByPrincipalID: "test-admin",
		CorrelationID:        "corr-" + uuid.New().String(),
	}
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

	got, err := s.GetInvoice(ctx, tenantID, inv.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected invoice to be found")
	}
	if got.Status != domain.InvoiceStatusIssued {
		t.Fatalf("expected status ISSUED, got %s", got.Status)
	}
}

// TestPgStore_CreateInvoice_RetriedCorrelationID_IsIdempotent proves the
// idempotency guarantee against a REAL Postgres unique index — this is the
// exact scenario a network-timeout-triggered client retry produces, and it
// must resolve to the original invoice, never a duplicate receivable.
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

	inv2 := newTestInvoice(tenantID)
	inv2.CorrelationID = "corr-retry-1"
	created2, err := s.CreateInvoice(ctx, inv2)
	if err != nil {
		t.Fatalf("retried CreateInvoice failed: %v", err)
	}
	if created2 {
		t.Fatal("expected created=false on the retried call — this is a duplicate-receivable bug if it's true")
	}
	if inv2.InvoiceID != inv1.InvoiceID {
		t.Fatalf("retried call resolved to a different invoice_id (%s) than the original (%s)", inv2.InvoiceID, inv1.InvoiceID)
	}

	var count int
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM customer_invoices WHERE tenant_id = $1 AND correlation_id = $2`,
			tenantID, "corr-retry-1").Scan(&count); err != nil {
			t.Fatalf("count query failed: %v", err)
		}
	})
	if count != 1 {
		t.Fatalf("DUPLICATE RECEIVABLE: expected exactly 1 customer_invoices row for this correlation_id, got %d", count)
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

	// Invoice is ISSUED — attempting SENT -> OVERDUE (wrong fromStatus) must be rejected
	_, err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID, domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, "test-admin")
	if err == nil {
		t.Fatal("expected an error transitioning from the wrong fromStatus, got nil")
	}

	got, _ := s.GetInvoice(ctx, tenantID, inv.InvoiceID)
	if got.Status != domain.InvoiceStatusIssued {
		t.Fatalf("invoice status must remain unchanged after a rejected transition, got %s", got.Status)
	}
}

// TestPgStore_TenantPredicate_IsolatesTenants proves that the store's explicit
// `tenant_id = $n` predicate hides one tenant's invoices from another.
//
// This test was called TestPgStore_RLS_TenantIsolation, and it never tested RLS.
// It passed — and still passes — entirely because of the predicate in the SQL,
// which is why it went on passing while the row-level security policy it was
// named after was inert on every query this service has ever made (see
// migration 000003). A test whose name credits the wrong control is worse than
// no test: it is why nobody looked. The policy gets its own two tests below.
func TestPgStore_TenantPredicate_IsolatesTenants(t *testing.T) {
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

	got, err := s.GetInvoice(ctxB, tenantB, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice under tenant B returned an error: %v", err)
	}
	if got != nil {
		t.Fatal("tenant isolation failure: tenant B was able to read tenant A's invoice")
	}

	_, err = s.TransitionInvoice(ctxB, tenantB, invA.InvoiceID, domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "attacker")
	if err == nil {
		t.Fatal("tenant isolation failure: tenant B was able to transition tenant A's invoice")
	}

	gotA, err := s.GetInvoice(ctxA, tenantA, invA.InvoiceID)
	if err != nil {
		t.Fatalf("GetInvoice failed under the owning tenant: %v", err)
	}
	if gotA == nil {
		t.Fatal("expected the invoice to be found under its own tenant")
	}
}

// TestPgStore_ForceRowLevelSecurity_IsSet asserts the flag migration 000003
// sets, so that reverting it is a test failure rather than a silent return to a
// policy that never runs. ENABLE alone exempts the table's owner, which is the
// role this service connects as.
func TestPgStore_ForceRowLevelSecurity_IsSet(t *testing.T) {
	pool := openTestPool(t)

	var enabled, forced bool
	err := pool.QueryRow(context.Background(),
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'customer_invoices'`,
	).Scan(&enabled, &forced)
	if err != nil {
		t.Fatalf("failed to read pg_class flags: %v", err)
	}
	if !enabled {
		t.Fatal("row-level security is not enabled on customer_invoices")
	}
	if !forced {
		t.Fatal("row-level security is enabled but NOT forced, so it does not apply to the table owner — " +
			"which is the role the service connects as, so the policy applies to nothing")
	}
}

// TestPgStore_Policy_ConstrainsAnOrdinaryRole proves the isolation policy
// genuinely filters rows, by running an unfiltered query as a role that is
// neither the table's owner nor a superuser.
//
// It has to be a separate role because the test pool — like the service —
// connects as `postgres`, a SUPERUSER, and superusers bypass the row security
// system altogether, FORCE or no FORCE. That is the honest limit of migration
// 000003 on a local run, and it is why the store carries its own tenant
// predicate rather than delegating isolation to the policy. This test shows the
// policy is correctly written and would bite the moment the services are given
// an ordinary role.
func TestPgStore_Policy_ConstrainsAnOrdinaryRole(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()

	invA := newTestInvoice(tenantA)
	if _, err := s.CreateInvoice(svcmiddleware.WithTenant(ctx, tenantA), invA); err != nil {
		t.Fatalf("CreateInvoice (tenant A) failed: %v", err)
	}
	invB := newTestInvoice(tenantB)
	if _, err := s.CreateInvoice(svcmiddleware.WithTenant(ctx, tenantB), invB); err != nil {
		t.Fatalf("CreateInvoice (tenant B) failed: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("failed to acquire a connection: %v", err)
	}
	defer conn.Release()

	// A role name unique to this run: these suites share one database and drop
	// only the table, so a fixed name would collide with a leftover role.
	role := "ar_rls_probe_" + uuid.New().String()[:8]
	// This test needs a PRIVILEGED connection — it has to mint a throwaway role
	// and SET ROLE into it, which is how it proves the policy filters for an
	// ordinary role. CI's TEST_DATABASE_URL is the superuser, so it runs there.
	//
	// Pointed at a DSN that IS one of the ordinary application roles
	// (deployments/scripts/create-app-roles.sh), it cannot do either, and used
	// to fail with "permission denied to create role" — which reads exactly
	// like a policy defect and is not one. Skipping says so out loud rather
	// than reporting a fault in the thing under test.
	if _, err := conn.Exec(ctx, `CREATE ROLE `+role+` NOSUPERUSER NOBYPASSRLS`); err != nil {
		t.Skipf("skipping: this probe needs a connection that may CREATE ROLE and SET ROLE, "+
			"and TEST_DATABASE_URL's user may not (%v). Point it at the superuser to run it.", err)
	}
	// The role outlives a table drop, so clean it up explicitly.
	defer func() {
		_, _ = conn.Exec(ctx, `REVOKE ALL ON customer_invoices FROM `+role)
		_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	}()
	if _, err := conn.Exec(ctx, `GRANT SELECT ON customer_invoices TO `+role); err != nil {
		t.Fatalf("failed to grant SELECT to the probe role: %v", err)
	}

	// Scope the connection to tenant A, then drop to the ordinary role. The
	// policy is now the only thing between this query and tenant B's
	// receivable — the query itself carries no tenant predicate at all.
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantA); err != nil {
		t.Fatalf("set_config failed: %v", err)
	}
	if _, err := conn.Exec(ctx, `SET ROLE `+role); err != nil {
		// Same reason as CREATE ROLE above: a CREATEROLE-but-not-superuser
		// connection can mint the role and still not be permitted to assume it.
		t.Skipf("skipping: TEST_DATABASE_URL's user may not SET ROLE (%v). "+
			"Point it at the superuser to run this probe.", err)
	}

	var visible int
	countErr := conn.QueryRow(ctx, `SELECT COUNT(*) FROM customer_invoices`).Scan(&visible)
	if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE failed: %v", err)
	}
	if countErr != nil {
		t.Fatalf("unfiltered count failed under the probe role: %v", countErr)
	}

	// Exactly tenant A's row: the policy filtered tenant B's out of a query
	// that asked for everything.
	if visible != 1 {
		t.Fatalf("the isolation policy did not filter by tenant: an unfiltered SELECT as an ordinary role "+
			"scoped to tenant A saw %d rows, expected exactly 1", visible)
	}
}

// TestPgStore_ListInvoices_ReturnsOnlyTheGivenTenant replaces a test called
// TestPgStore_ListInvoices_TenantScoped, which asserted that listing with
// filter.TenantID = tenantB returned nothing while scoped to tenant A — and so
// documented the cross-tenant read as correct behaviour. The store faithfully
// returns whichever tenant the filter names; that is all it can do. What makes
// the register safe is that the HANDLER now builds filter.TenantID from the
// verified X-Tenant-Id and refuses a ?tenant_id= that disagrees, which has its
// own handler test.
func TestPgStore_ListInvoices_ReturnsOnlyTheGivenTenant(t *testing.T) {
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
	invB := newTestInvoice(tenantB)
	if _, err := s.CreateInvoice(ctxB, invB); err != nil {
		t.Fatalf("CreateInvoice (tenant B) failed: %v", err)
	}

	listA, err := s.ListInvoices(ctxA, domain.ListInvoicesFilter{TenantID: tenantA, Limit: 100})
	if err != nil {
		t.Fatalf("ListInvoices (tenant A) failed: %v", err)
	}
	if len(listA) != 1 {
		t.Fatalf("expected 1 invoice for tenant A, got %d", len(listA))
	}
	if listA[0].InvoiceID != invA.InvoiceID {
		t.Fatalf("tenant A's register returned another tenant's invoice: %s", listA[0].InvoiceID)
	}

	// Both tenants hold one invoice each, so a register that leaked would show
	// two. The previous version created only ONE invoice in total, which meant
	// its "0 rows for tenant B" assertion held whether filtering worked or not.
	listB, err := s.ListInvoices(ctxB, domain.ListInvoicesFilter{TenantID: tenantB, Limit: 100})
	if err != nil {
		t.Fatalf("ListInvoices (tenant B) failed: %v", err)
	}
	if len(listB) != 1 || listB[0].InvoiceID != invB.InvoiceID {
		t.Fatalf("expected tenant B's register to hold exactly its own invoice, got %d rows", len(listB))
	}
}

// TestPgStore_CreateInvoice_StoresTheVerifiedTenant guards the write half of the
// cross-tenant defect. inv.TenantID is now always the caller's verified scope —
// the handler refuses a body tenant_id that disagrees with it — so the row must
// land in that tenant, and the RLS session variable must be set from the same
// value rather than from a second, different one.
func TestPgStore_CreateInvoice_StoresTheVerifiedTenant(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	inv := newTestInvoice(tenantID)

	// Deliberately an EMPTY context: the store must take the tenant from
	// inv.TenantID, which the handler has verified, and must not reach into the
	// context for it. It used to try the context first and fall back to
	// whatever the request had supplied, which is how a request carrying no
	// X-Tenant-Id at all got to choose its own scope.
	if _, err := s.CreateInvoice(context.Background(), inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	var stored string
	scoped(t, pool, tenantID, func(tx pgx.Tx) {
		if err := tx.QueryRow(context.Background(),
			`SELECT tenant_id::text FROM customer_invoices WHERE invoice_id = $1`, inv.InvoiceID,
		).Scan(&stored); err != nil {
			t.Fatalf("failed to read the stored tenant_id: %v", err)
		}
	})
	if stored != tenantID {
		t.Fatalf("invoice stored under tenant %s, expected %s", stored, tenantID)
	}
}

// TestPgStore_TransitionInvoice_ReturnsTheStampedRow proves the UPDATE's RETURNING
// carries the attribution, against a real Postgres. TransitionInvoice used to
// return only an error, so the handlers reported a transition with sent_at and
// sent_by_principal_id still null — the database had them and the API denied it.
func TestPgStore_TransitionInvoice_ReturnsTheStampedRow(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	inv := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(ctx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	sent, err := s.TransitionInvoice(ctx, tenantID, inv.InvoiceID,
		domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "principal-sender")
	if err != nil {
		t.Fatalf("TransitionInvoice failed: %v", err)
	}
	if sent == nil {
		t.Fatal("TransitionInvoice returned no invoice")
	}
	if sent.Status != domain.InvoiceStatusSent {
		t.Fatalf("expected SENT, got %s", sent.Status)
	}
	if sent.SentByPrincipalID == nil || *sent.SentByPrincipalID != "principal-sender" {
		t.Fatalf("the returned row omits who sent it: %#v", sent.SentByPrincipalID)
	}
	if sent.SentAt == nil {
		t.Fatal("the returned row omits when it was sent")
	}
	// The rest of the record must come back intact, not just the changed columns.
	if sent.InvoiceNumber != inv.InvoiceNumber || sent.Amount != inv.Amount {
		t.Fatalf("the returned row lost fields: %s / %v", sent.InvoiceNumber, sent.Amount)
	}
}

// TestPgStore_TransitionInvoice_WrongTenant_IsNotFound — no rows matched means
// "you cannot make that move from here", whether because of the status, the tenant,
// or the id. All three must read as ErrInvalidTransition rather than a store fault.
func TestPgStore_TransitionInvoice_WrongTenant_IsNotFound(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)

	invA := newTestInvoice(tenantA)
	if _, err := s.CreateInvoice(ctxA, invA); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}

	got, err := s.TransitionInvoice(ctxB, tenantB, invA.InvoiceID,
		domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "attacker")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}
	if got != nil {
		t.Fatal("a refused transition returned a row")
	}
}

// ── migration 000004's invariants ────────────────────────────────────────────
//
// These assert the CHECK constraints bite against a real Postgres. The service is
// the only writer today, so they are defence in depth — but the rules lived only in
// Go, which means anything reaching this database by another route could leave a
// receivable the domain considers impossible.

func TestPgStore_Invariants_RejectImpossibleInvoices(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	insert := func(t *testing.T, column, value string) error {
		t.Helper()
		tenantID := uuid.New().String()
		// RLS is FORCEd, so the session needs a tenant even for a direct insert.
		if _, err := pool.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantID); err != nil {
			t.Fatalf("set_config failed: %v", err)
		}
		sql := fmt.Sprintf(`
			INSERT INTO customer_invoices (
				invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
				amount, currency_code, due_date, status, created_by_principal_id,
				correlation_id
			) VALUES ($1, $2, $3, 'c1', $4, %s, %s, DATE '2026-09-01', %s, 'p1', $5)`,
			pick(column, "amount", value, "100.00"),
			pick(column, "currency_code", value, "'GBP'"),
			pick(column, "status", value, "'ISSUED'"))
		_, err := pool.Exec(ctx, sql,
			uuid.New().String(), tenantID, uuid.New().String(),
			"INV-"+uuid.New().String()[:8], "corr-"+uuid.New().String())
		return err
	}

	for _, tc := range []struct{ name, column, value string }{
		{"a status the domain cannot name", "status", "'OUTSTANDING'"},
		{"an invoice for nothing", "amount", "0"},
		{"an invoice for a negative sum", "amount", "-100.00"},
		{"a lowercase currency", "currency_code", "'gbp'"},
		{"a currency that is not three letters", "currency_code", "'G1'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := insert(t, tc.column, tc.value); err == nil {
				t.Fatalf("the database accepted %s = %s", tc.column, tc.value)
			}
		})
	}

	// The control: the same insert with all-valid values must succeed, or the test
	// above would pass for the wrong reason.
	t.Run("a valid invoice is still accepted", func(t *testing.T) {
		if err := insert(t, "", ""); err != nil {
			t.Fatalf("a valid invoice was rejected: %v", err)
		}
	})
}

// pick returns value when column is the one under test, otherwise the default —
// so each case varies exactly one column.
func pick(column, name, value, def string) string {
	if column == name {
		return value
	}
	return def
}

// TestPgStore_Invariants_RejectUnattributedStateChange — a PAID invoice with no
// payment_received_by_principal_id is money recorded as received on nobody's
// authority, which is the shape an audit exists to catch.
func TestPgStore_Invariants_RejectUnattributedStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New().String()
	inv := newTestInvoice(tenantID)
	if _, err := s.CreateInvoice(svcmiddleware.WithTenant(ctx, tenantID), inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantID); err != nil {
		t.Fatalf("set_config failed: %v", err)
	}

	// Straight to PAID with no stamp at all.
	if _, err := pool.Exec(ctx,
		`UPDATE customer_invoices SET status = 'PAID' WHERE invoice_id = $1`, inv.InvoiceID,
	); err == nil {
		t.Fatal("the database accepted a PAID invoice with no payment attribution")
	}

	// An actor with no timestamp: the pair is meaningless apart.
	if _, err := pool.Exec(ctx,
		`UPDATE customer_invoices SET sent_by_principal_id = 'p1' WHERE invoice_id = $1`, inv.InvoiceID,
	); err == nil {
		t.Fatal("the database accepted an actor with no timestamp")
	}
}

// TestPgStore_Invariants_RejectOverdueBeforeDueDate — the same rule the handler
// enforces, written where it cannot be bypassed.
func TestPgStore_Invariants_RejectOverdueBeforeDueDate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New().String()
	inv := newTestInvoice(tenantID) // due in 15 days
	tenantCtx := svcmiddleware.WithTenant(ctx, tenantID)
	if _, err := s.CreateInvoice(tenantCtx, inv); err != nil {
		t.Fatalf("CreateInvoice failed: %v", err)
	}
	if _, err := s.TransitionInvoice(tenantCtx, tenantID, inv.InvoiceID,
		domain.InvoiceStatusIssued, domain.InvoiceStatusSent, "p1"); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	// The handler refuses this too (422 not_yet_due); this proves the schema does
	// as well, so a direct writer cannot age a receivable early.
	if _, err := s.TransitionInvoice(tenantCtx, tenantID, inv.InvoiceID,
		domain.InvoiceStatusSent, domain.InvoiceStatusOverdue, "p1"); err == nil {
		t.Fatal("the database accepted an invoice declared overdue before its due date")
	}
}

// ── paging ───────────────────────────────────────────────────────────────────

// TestPgStore_ListInvoices_PagesStably — created_at is not unique, so ordering by
// it alone leaves ties in an arbitrary order, and an arbitrary order under
// LIMIT/OFFSET can show one row on two pages while never showing another. The
// invoice_id tiebreaker is what makes paging total.
func TestPgStore_ListInvoices_PagesStably(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	const total = 7
	for i := 0; i < total; i++ {
		if _, err := s.CreateInvoice(ctx, newTestInvoice(tenantID)); err != nil {
			t.Fatalf("CreateInvoice %d failed: %v", i, err)
		}
	}

	// Walk the register two at a time and prove every invoice is seen exactly once.
	seen := map[string]int{}
	for offset := 0; offset < total+2; offset += 2 {
		page, err := s.ListInvoices(ctx, domain.ListInvoicesFilter{
			TenantID: tenantID, Limit: 2, Offset: offset,
		})
		if err != nil {
			t.Fatalf("page at offset %d failed: %v", offset, err)
		}
		for _, inv := range page {
			seen[inv.InvoiceID]++
		}
	}
	if len(seen) != total {
		t.Fatalf("paging saw %d distinct invoices, expected %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("invoice %s appeared on %d pages — paging is not stable", id, n)
		}
	}

	// And the limit is honoured rather than ignored.
	one, err := s.ListInvoices(ctx, domain.ListInvoicesFilter{TenantID: tenantID, Limit: 1})
	if err != nil {
		t.Fatalf("limit=1 failed: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("expected 1 row for limit=1, got %d", len(one))
	}
}
