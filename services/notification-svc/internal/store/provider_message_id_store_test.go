package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

func receiptFor(id string) string { return "smtp mail.example.com:587 accepted; message-id=" + id }

func complete(t *testing.T, s *store.PgStore, tenant, id, status, failure, receipt string) error {
	t.Helper()
	at := time.Now().UTC()
	return s.CompleteDelivery(tenantCtx(tenant), id, status, failure, receipt, &at, "c", domain.AttemptMeta{})
}

// F-12 (migration 000015): a callback quoting a direct send's Message-ID, with or
// without angle brackets, or its attempt id, finds that attempt.
func TestDirectAttempt_IsFoundByItsProviderMessageID(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := seedNotification(t, s, "tenant-pmid", "corr-pmid-1")
	if err := complete(t, s, "tenant-pmid", n.NotificationID, "SENT", "", receiptFor("<abc-123@example.com>")); err != nil {
		t.Fatal(err)
	}

	for _, quoted := range []string{"<abc-123@example.com>", "abc-123@example.com"} {
		got, err := s.LookupAttemptByProviderMessageID(context.Background(), quoted)
		if err != nil {
			t.Fatalf("lookup %q: %v", quoted, err)
		}
		if got.TenantID != "tenant-pmid" || got.ProviderAttemptID == "" {
			t.Errorf("lookup %q = %+v", quoted, got)
		}
		if got.MessageIntentID != "" {
			t.Errorf("a direct attempt has no ledger intent: %q", got.MessageIntentID)
		}
	}

	attempts, err := s.ListAttempts(tenantCtx("tenant-pmid"), n.NotificationID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %v err=%v", attempts, err)
	}
	if got, err := s.LookupAttemptByProviderMessageID(context.Background(), attempts[0].AttemptID); err != nil || got.ProviderAttemptID != attempts[0].AttemptID {
		t.Errorf("lookup by attempt id = %+v err=%v", got, err)
	}

	if _, err := s.LookupAttemptByProviderMessageID(context.Background(), "<nobody@example.com>"); !errors.Is(err, store.ErrAttemptNotFound) {
		t.Errorf("an unknown id must be ErrAttemptNotFound, got %v", err)
	}
}

// A failed attempt has no message to point at; recording an id would let a stray
// callback attach itself to it.
func TestDirectAttempt_FailedAttemptRecordsNoProviderMessageID(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := seedNotification(t, s, "tenant-pmid", "corr-pmid-2")
	if err := complete(t, s, "tenant-pmid", n.NotificationID, "FAILED", "550 no such user", receiptFor("<should-not-be-stored@example.com>")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupAttemptByProviderMessageID(context.Background(), "<should-not-be-stored@example.com>"); !errors.Is(err, store.ErrAttemptNotFound) {
		t.Fatalf("a failed attempt must not be findable by a message id, got %v", err)
	}
}

// NP-27: one provider message id maps to exactly one attempt; a second claim is
// refused, not merged.
func TestDirectAttempt_ProviderMessageIDIsUnique(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	a := seedNotification(t, s, "tenant-pmid", "corr-pmid-3a")
	b := seedNotification(t, s, "tenant-pmid", "corr-pmid-3b")
	if err := complete(t, s, "tenant-pmid", a.NotificationID, "SENT", "", receiptFor("<dup@example.com>")); err != nil {
		t.Fatal(err)
	}
	if err := complete(t, s, "tenant-pmid", b.NotificationID, "SENT", "", receiptFor("<dup@example.com>")); err == nil {
		t.Fatal("a second attempt claimed an existing provider message id")
	}
	got, _ := s.GetNotification(tenantCtx("tenant-pmid"), b.NotificationID)
	if got.Status != domain.StatusPending {
		t.Fatalf("the refused attempt half-applied: %s", got.Status)
	}
}

// The point of F-12, end to end: a hard bounce for a DIRECT send now becomes a
// suppression, which the direct-send guard then enforces.
func TestDirectAttempt_HardBounceCallbackBecomesASuppression(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := newNotification("tenant-bounce", "entity-1", "recipient-1", "corr-pmid-4")
	n.RecipientAddress, n.RecipientAddressSource = "gone@example.com", domain.AddressSourceRequest
	if created, err := s.CreateNotification(tenantCtx("tenant-bounce"), n); err != nil || !created {
		t.Fatalf("seed: %v %v", created, err)
	}
	if err := complete(t, s, "tenant-bounce", n.NotificationID, "SENT", "", receiptFor("<bounce-me@example.com>")); err != nil {
		t.Fatal(err)
	}

	payload := `{"event_id":"evt-pmid-1","event_type":"BOUNCE","bounce_type":"HARD","recipient_email":"gone@example.com",
		"provider_message_id":"<bounce-me@example.com>","diagnostic_code":"550 5.1.1 User unknown"}`
	if err := webhook.NewProcessor(s, zap.NewNop()).ProcessRawPayload(context.Background(), "generic", []byte(payload)); err != nil {
		t.Fatalf("callback: %v", err)
	}

	suppressed, why, err := s.IsEmailSuppressed(tenantCtx("tenant-bounce"), "tenant-bounce", "gone@example.com", ledger.StreamTransactional, ledger.ClassT0)
	if err != nil || !suppressed {
		t.Fatalf("the hard bounce must suppress the address: suppressed=%v why=%q err=%v", suppressed, why, err)
	}
}

// The migration reverses cleanly and can be re-applied, and the column is nullable
// so existing rows are untouched.
func TestMigration000015_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	for _, f := range []string{"000015_direct_attempt_provider_message_id.down.sql", "000015_direct_attempt_provider_message_id.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='notification_delivery_attempts' AND column_name='provider_message_id' AND is_nullable='YES'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("provider_message_id column: n=%d err=%v", n, err)
	}
}
