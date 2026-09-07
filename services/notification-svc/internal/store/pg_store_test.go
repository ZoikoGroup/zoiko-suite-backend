package store_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"sort"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/store"
)

// openTestPool connects to a real Postgres and reapplies the migration from a
// clean slate. Skips (not fails) if TEST_DATABASE_URL isn't set â€” same
// convention as every other service in this platform.
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

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS notifications CASCADE;`)

	// Every migration, in order — discovered, not listed.
	//
	// This was a hardcoded list, with a comment warning that such a list is
	// easy to leave behind and citing authorization-svc's equivalent being
	// found short of 000005 on 31/08. It was then left behind in exactly the
	// same way: 000004 landed and the list did not, so the whole suite ran
	// against a schema with no delivery_attempts column and every test failed
	// at once. A warning about a footgun is not a guard against it.
	//
	// Globbing removes the failure mode rather than documenting it. Filenames
	// are zero-padded and fixed-width, so a lexical sort is the numeric order.
	migrationDir := filepath.Join(base, "../../deployments/migrations")
	names, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to list migrations: %v", err)
	}
	if len(names) == 0 {
		// An empty glob would otherwise leave every test failing against a
		// table that was just dropped, which reads as a store bug.
		t.Fatalf("no migrations found under %s", migrationDir)
	}
	sort.Strings(names)

	for _, path := range names {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", filepath.Base(path), err)
		}
		// A UTF-8 BOM survives ReadFile and is a syntax error to Postgres when
		// the file is sent as a query string — psql -f strips it, so a
		// migration can apply by hand and fail only here. 000003 shipped with
		// one and took this whole suite down with "syntax error at or near".
		sql = bytes.TrimPrefix(sql, []byte("\xef\xbb\xbf"))
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to apply migration %s: %v", filepath.Base(path), err)
		}
	}

	return pool
}

// requireThrowawayDatabase refuses to run against anything not recognisably
// disposable. This suite DROPs notifications, and those rows are the record of
// which notices were sent and which failed to go out. Only the database NAME
// vouches for it; a password that happens to contain "test" must not.
func requireThrowawayDatabase(t *testing.T, dsn string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("refusing to run: TEST_DATABASE_URL is not a parseable URL: %v", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run: TEST_DATABASE_URL names database %q, which is not recognisably "+
			"disposable, and this suite DROPs notifications. Use notification_test (or CI's testdb), "+
			"not notification.", dbName)
	}
}

func tenantCtx(tenantID string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenantID)
}

func newNotification(tenantID, legalEntityID, recipient, correlationID string) *domain.Notification {
	return &domain.Notification{
		NotificationID:       uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        legalEntityID,
		RecipientPrincipalID: recipient,
		Channel:              "EMAIL",
		Subject:              "Approval required",
		Body:                 "Invoice INV-100 needs your approval.",
		Status:               "PENDING",
		CorrelationID:        correlationID,
		CreatedByPrincipalID: "principal-1",
		CreatedAt:            time.Now().UTC(),
	}
}

// A retried send must resolve to the ORIGINAL notification, never a second
// delivery â€” proved against the real unique index rather than a map stub.
func TestPgStore_CreateNotification_Retried_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	first := newNotification("tenant-a", "le-us", "principal-2", "corr-retry")
	created, err := s.CreateNotification(ctx, first)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created {
		t.Fatal("expected the first send to create a notification")
	}

	// The retry carries a fresh notification_id, exactly as the handler would
	// generate on a second request â€” only the correlation id is the same.
	retry := newNotification("tenant-a", "le-us", "principal-2", "corr-retry")
	retryGeneratedID := retry.NotificationID
	created, err = s.CreateNotification(ctx, retry)
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if created {
		t.Fatal("a retry with the same correlation_id must not create a second notification")
	}
	if retry.NotificationID == retryGeneratedID {
		t.Fatal("the retry must be rewritten to the stored notification, not keep its own new id")
	}
	if retry.NotificationID != first.NotificationID {
		t.Fatalf("retry resolved to %s, want the original %s", retry.NotificationID, first.NotificationID)
	}
}

// The same correlation id in a DIFFERENT tenant is a different notification.
// The unique index is on (tenant_id, correlation_id), and a tenant must not be
// able to suppress another tenant's notification by guessing its key.
func TestPgStore_CreateNotification_CorrelationIDIsScopedPerTenant(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	a := newNotification("tenant-a", "le-us", "principal-2", "corr-shared")
	if created, err := s.CreateNotification(tenantCtx("tenant-a"), a); err != nil || !created {
		t.Fatalf("tenant-a create: created=%v err=%v", created, err)
	}
	b := newNotification("tenant-b", "le-uk", "principal-9", "corr-shared")
	created, err := s.CreateNotification(tenantCtx("tenant-b"), b)
	if err != nil {
		t.Fatalf("tenant-b create: %v", err)
	}
	if !created {
		t.Fatal("the same correlation_id in another tenant must create its own notification")
	}
	if b.NotificationID == a.NotificationID {
		t.Fatal("tenant-b's notification collided with tenant-a's")
	}
}

// A notification belonging to another tenant must read as not-found, not as
// someone else's subject line.
func TestPgStore_GetNotification_IsTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-scope")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got, err := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID); err != nil {
		t.Fatalf("own tenant read: %v", err)
	} else if got.Subject != n.Subject {
		t.Fatalf("own tenant read returned subject %q, want %q", got.Subject, n.Subject)
	}

	_, err := s.GetNotification(tenantCtx("tenant-b"), n.NotificationID)
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("cross-tenant read returned %v, want ErrNotificationNotFound", err)
	}
}

// notification_id is a uuid column, so a mistyped id dies inside the driver as
// SQLSTATE 22P02. That used to surface as 503 store_unavailable â€” an outage
// status for a typo in a URL.
func TestPgStore_GetNotification_MalformedID_IsNotFoundNotAnOutage(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	_, err := s.GetNotification(tenantCtx("tenant-a"), "not-a-uuid")
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("malformed id returned %v, want ErrNotificationNotFound", err)
	}
}

// The register read must not cross tenants, and must honour its paging bounds.
func TestPgStore_ListNotifications_IsTenantScopedAndPaged(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	for i := 0; i < 3; i++ {
		n := newNotification("tenant-a", "le-us", "principal-2", "corr-a-"+strconv.Itoa(i))
		if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
			t.Fatalf("seed tenant-a: %v", err)
		}
	}
	other := newNotification("tenant-b", "le-us", "principal-2", "corr-b-1")
	if _, err := s.CreateNotification(tenantCtx("tenant-b"), other); err != nil {
		t.Fatalf("seed tenant-b: %v", err)
	}

	all, err := s.ListNotifications(tenantCtx("tenant-a"), domain.ListFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("tenant-a sees %d notifications, want exactly its own 3", len(all))
	}
	for _, n := range all {
		if n.TenantID != "tenant-a" {
			t.Fatalf("tenant-a's list contained a %s row", n.TenantID)
		}
	}

	page, err := s.ListNotifications(tenantCtx("tenant-a"), domain.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("paged list: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(page))
	}

	rest, err := s.ListNotifications(tenantCtx("tenant-a"), domain.ListFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("offset list: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("offset=2 returned %d rows, want the remaining 1", len(rest))
	}
	// The paging must partition the register, not repeat it â€” a tie on
	// created_at with no tiebreaker would let the same row appear twice.
	for _, a := range page {
		for _, b := range rest {
			if a.NotificationID == b.NotificationID {
				t.Fatalf("notification %s appeared on both pages", a.NotificationID)
			}
		}
	}
}

// The recipient filter is what an unscoped list falls back to, so it has to
// actually filter â€” otherwise the handler's inbox scoping would be decorative.
func TestPgStore_ListNotifications_FiltersByRecipient(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	mine := newNotification("tenant-a", "le-us", "principal-me", "corr-mine")
	theirs := newNotification("tenant-a", "le-us", "principal-them", "corr-theirs")
	for _, n := range []*domain.Notification{mine, theirs} {
		if _, err := s.CreateNotification(ctx, n); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := s.ListNotifications(ctx, domain.ListFilter{RecipientPrincipalID: "principal-me", Limit: 100})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].NotificationID != mine.NotificationID {
		t.Fatalf("recipient filter returned %d rows, want only principal-me's notification", len(got))
	}
}

// CompleteDelivery is the second half of every send. It must not be able to
// conclude another tenant's notification.
func TestPgStore_CompleteDelivery_IsTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "principal-2", "corr-complete")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	sentAt := time.Now().UTC()
	if err := s.CompleteDelivery(tenantCtx("tenant-b"), n.NotificationID, "SENT", "", "stub receipt", &sentAt); !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("cross-tenant complete returned %v, want ErrNotificationNotFound", err)
	}

	if err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID, "SENT", "", "stub receipt", &sentAt); err != nil {
		t.Fatalf("own-tenant complete: %v", err)
	}
	got, err := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != "SENT" || got.SentAt == nil {
		t.Fatalf("delivery outcome not recorded: status=%q sent_at=%v", got.Status, got.SentAt)
	}
}

// A store call with no tenant in context must be refused rather than run
// against every tenant's rows.
func TestPgStore_WithoutTenant_IsRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	if _, err := s.ListNotifications(context.Background(), domain.ListFilter{Limit: 10}); !errors.Is(err, domain.ErrIdentityMissing) {
		t.Fatalf("list without tenant returned %v, want ErrIdentityMissing", err)
	}
	if _, err := s.GetNotification(context.Background(), uuid.NewString()); !errors.Is(err, domain.ErrIdentityMissing) {
		t.Fatalf("get without tenant returned %v, want ErrIdentityMissing", err)
	}
}
