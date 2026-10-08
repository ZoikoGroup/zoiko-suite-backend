package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

func seedIntent(t *testing.T, s *store.PgStore, tenant string) string {
	t.Helper()
	id := uuid.NewString()
	in := &ledger.MessageIntent{
		MessageIntentID: id, TenantID: tenant, LegalEntityID: "entity-1", RecipientPrincipalID: "usr-1",
		RecipientEmail: "u@example.com", Channel: "EMAIL", CommunicationClass: ledger.ClassT0, TemplateKey: "ZS-IA-001",
		EventID: id, SourceEventType: "t.test", DeduplicationKey: "dk-" + id, CorrelationID: "c-" + id,
		Status: ledger.IntentStatusPending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if created, _, err := s.CreateMessageIntent(tenantCtx(tenant), in); err != nil || !created {
		t.Fatalf("seed intent: created=%v err=%v", created, err)
	}
	return id
}

// Migration 000016 / INV-02: the two halves of a communication are joined on both
// sides in one step, and can be read from either end.
func TestLink_JoinsIntentAndNotificationBothWays(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx("tenant-link")
	intent := seedIntent(t, s, "tenant-link")
	n := seedNotification(t, s, "tenant-link", "corr-link-1")

	if got, err := s.NotificationIDForIntent(ctx, intent); err != nil || got != "" {
		t.Fatalf("an unlinked intent has no notification: %q %v", got, err)
	}
	if err := s.LinkIntentToNotification(ctx, intent, n.NotificationID); err != nil {
		t.Fatalf("link: %v", err)
	}
	if got, _ := s.NotificationIDForIntent(ctx, intent); got != n.NotificationID {
		t.Errorf("intent side = %q, want %q", got, n.NotificationID)
	}
	if got, _ := s.IntentIDForNotification(ctx, n.NotificationID); got != intent {
		t.Errorf("notification side = %q, want %q", got, intent)
	}
	// Idempotent: the same pair again changes nothing and is not an error.
	if err := s.LinkIntentToNotification(ctx, intent, n.NotificationID); err != nil {
		t.Errorf("relinking the same pair must be a no-op: %v", err)
	}
}

func TestLink_IsOneToOneAndNeverRepointed(t *testing.T) {
	s := store.New(openTestPool(t))
	ctx := tenantCtx("tenant-link")
	i1, i2 := seedIntent(t, s, "tenant-link"), seedIntent(t, s, "tenant-link")
	n1, n2 := seedNotification(t, s, "tenant-link", "corr-link-2a"), seedNotification(t, s, "tenant-link", "corr-link-2b")
	if err := s.LinkIntentToNotification(ctx, i1, n1.NotificationID); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"intent already linked":       {i1, n2.NotificationID},
		"notification already linked": {i2, n1.NotificationID},
	} {
		if err := s.LinkIntentToNotification(ctx, pair[0], pair[1]); !errors.Is(err, domain.ErrAlreadyLinked) {
			t.Errorf("%s: got %v, want ErrAlreadyLinked", name, err)
		}
	}
	// The refusals changed nothing.
	if got, _ := s.IntentIDForNotification(ctx, n2.NotificationID); got != "" {
		t.Errorf("a refused link half-applied: %q", got)
	}
	if got, _ := s.NotificationIDForIntent(ctx, i2); got != "" {
		t.Errorf("a refused link half-applied: %q", got)
	}
}

func TestLink_RefusesMissingAndCrossTenantTargets(t *testing.T) {
	s := store.New(openTestPool(t))
	intent := seedIntent(t, s, "tenant-link-a")
	n := seedNotification(t, s, "tenant-link-b", "corr-link-3")

	// Tenant A cannot link its intent to a notification of tenant B, and that
	// notification id looks exactly like one that does not exist.
	if err := s.LinkIntentToNotification(tenantCtx("tenant-link-a"), intent, n.NotificationID); !errors.Is(err, domain.ErrLinkTargetNotFound) {
		t.Errorf("cross-tenant: got %v, want ErrLinkTargetNotFound", err)
	}
	for _, bad := range []string{uuid.NewString(), "not-a-uuid", ""} {
		if err := s.LinkIntentToNotification(tenantCtx("tenant-link-a"), intent, bad); !errors.Is(err, domain.ErrLinkTargetNotFound) {
			t.Errorf("notification %q: got %v, want ErrLinkTargetNotFound", bad, err)
		}
	}
	if err := s.LinkIntentToNotification(context.Background(), intent, n.NotificationID); !errors.Is(err, domain.ErrIdentityMissing) {
		t.Errorf("no tenant on the context: got %v, want ErrIdentityMissing", err)
	}
}

// The database enforces the same rules without the store.
func TestLink_DatabaseRefusesWhatTheStoreWouldRefuse(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()
	intentA := seedIntent(t, s, "tenant-db-a")
	nB := seedNotification(t, s, "tenant-db-b", "corr-link-4")
	nA := seedNotification(t, s, "tenant-db-a", "corr-link-5")
	nA2 := seedNotification(t, s, "tenant-db-a", "corr-link-6")

	if _, err := pool.Exec(ctx, `UPDATE message_intents SET notification_id=$2::uuid WHERE message_intent_id=$1::uuid`, intentA, nB.NotificationID); err == nil {
		t.Error("a cross-tenant link was accepted by the database")
	}
	if _, err := pool.Exec(ctx, `UPDATE message_intents SET notification_id=$2::uuid WHERE message_intent_id=$1::uuid`, intentA, nA.NotificationID); err != nil {
		t.Fatalf("a same-tenant link must be accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_intents SET notification_id=$2::uuid WHERE message_intent_id=$1::uuid`, intentA, nA2.NotificationID); err == nil {
		t.Error("a set link was repointed")
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET message_intent_id=$2::uuid WHERE notification_id=$1::uuid`, nA2.NotificationID, intentA); err == nil {
		t.Error("two notifications were linked to one intent (one-to-one)")
	}
}

// Housekeeping purges stale intents; that must not be blocked by, or damage, the
// notification that points at the intent.
func TestLink_PurgingTheIntentLeavesTheNotification(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-link-purge")
	intent := seedIntent(t, s, "tenant-link-purge")
	n := seedNotification(t, s, "tenant-link-purge", "corr-link-7")
	if err := s.LinkIntentToNotification(ctx, intent, n.NotificationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM message_intents WHERE message_intent_id=$1::uuid`, intent); err != nil {
		t.Fatalf("the purge was blocked by the link: %v", err)
	}
	got, err := s.GetNotification(ctx, n.NotificationID)
	if err != nil || got == nil {
		t.Fatalf("the notification must survive: %v", err)
	}
	if id, _ := s.IntentIDForNotification(ctx, n.NotificationID); id != "" {
		t.Errorf("the dangling link should have been cleared, got %q", id)
	}
}

func TestMigration000016_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000016_intent_notification_link.down.sql", "000016_intent_notification_link.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE (table_name='message_intents' AND column_name='notification_id') OR (table_name='notifications' AND column_name='message_intent_id')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("link columns: n=%d err=%v", n, err)
	}
}
