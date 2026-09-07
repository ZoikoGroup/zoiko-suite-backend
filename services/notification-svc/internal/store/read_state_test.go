package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

func newInApp(tenantID, recipient, correlationID string) *domain.Notification {
	n := newNotification(tenantID, "le-us", recipient, correlationID)
	n.Channel = domain.ChannelInApp
	return n
}

// ── recipient address and provenance ─────────────────────────────────────────

func TestPgStore_RecipientAddressAndProvenanceRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "employee-9", "corr-addr")
	n.RecipientAddress = "employee@example.com"
	n.RecipientAddressSource = domain.AddressSourceIdentityContext

	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.RecipientAddress != "employee@example.com" {
		t.Errorf("recipient_address = %q", got.RecipientAddress)
	}
	if got.RecipientAddressSource != domain.AddressSourceIdentityContext {
		t.Errorf("recipient_address_source = %q", got.RecipientAddressSource)
	}
}

// The CHECK from 000003. An address with no provenance is the state
// ZS-SVC-Y-001 §0.4 forbids, and it is reachable by a bug rather than by a
// caller — the two columns are written by one code path, so one without the
// other means that path is wrong.
func TestPgStore_AddressWithoutProvenanceIsRejected(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "employee-9", "corr-noprov")
	n.RecipientAddress = "employee@example.com"
	n.RecipientAddressSource = "" // stored as NULL

	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err == nil {
		t.Fatal("an address with no provenance was accepted")
	}
}

// ── delivery evidence, and the one-way conclusion ────────────────────────────

func TestPgStore_CompleteDelivery_RecordsProviderEvidence(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "employee-9", "corr-evidence")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	sentAt := time.Now().UTC()
	receipt := "smtp mail.example.com:587 accepted; message-id=<abc@zoiko.test>"
	if err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID, "SENT", "", receipt, &sentAt); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.ProviderResponse != receipt {
		t.Errorf("provider_response = %q, want %q", got.ProviderResponse, receipt)
	}
}

// A notification concludes once. Without the status guard, a second attempt —
// or a replay that slipped past the idempotency index — would rewrite sent_at
// and the evidence with a later attempt's, so the record of what actually
// happened would be whatever ran last.
func TestPgStore_CompleteDelivery_DoesNotReopenAConcludedNotification(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "employee-9", "corr-once")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	first := time.Now().UTC().Add(-time.Hour)
	if err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID,
		"FAILED", "550 no such user", "", &first); err != nil {
		t.Fatalf("first conclusion: %v", err)
	}

	second := time.Now().UTC()
	err := s.CompleteDelivery(tenantCtx("tenant-a"), n.NotificationID, "SENT", "", "accepted", &second)
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("a concluded notification was re-opened: %v", err)
	}

	got, _ := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if got.Status != "FAILED" || got.FailureReason != "550 no such user" {
		t.Errorf("the original conclusion was overwritten: status=%q reason=%q",
			got.Status, got.FailureReason)
	}
}

// ── read state ───────────────────────────────────────────────────────────────

func TestPgStore_MarkRead_SetsAndKeepsTheFirstRead(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newInApp("tenant-a", "employee-9", "corr-read")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	first := time.Now().UTC()
	if err := s.MarkRead(tenantCtx("tenant-a"), n.NotificationID, "employee-9", first); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	got, _ := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if got.ReadAt == nil {
		t.Fatal("read_at was not set")
	}
	stored := *got.ReadAt

	// Re-opening the inbox re-issues the mark. The first read must survive.
	if err := s.MarkRead(tenantCtx("tenant-a"), n.NotificationID, "employee-9",
		first.Add(2*time.Hour)); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	got, _ = s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if !got.ReadAt.Equal(stored) {
		t.Errorf("a repeat mark moved read_at from %v to %v — "+
			"\"first seen\" decayed into \"last looked\"", stored, *got.ReadAt)
	}
}

func TestPgStore_MarkRead_IsScopedToTheRecipientAndTenant(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newInApp("tenant-a", "employee-9", "corr-scope")
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	if err := s.MarkRead(tenantCtx("tenant-a"), n.NotificationID, "somebody-else", now); !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Errorf("another principal marked the notice read: %v", err)
	}
	if err := s.MarkRead(tenantCtx("tenant-b"), n.NotificationID, "employee-9", now); !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Errorf("another tenant marked the notice read: %v", err)
	}

	got, _ := s.GetNotification(tenantCtx("tenant-a"), n.NotificationID)
	if got.ReadAt != nil {
		t.Error("read_at was set by a refused caller")
	}
}

// Read state is an IN_APP concept: this service cannot observe whether an
// email was opened.
func TestPgStore_MarkRead_RefusesNonInAppChannels(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	n := newNotification("tenant-a", "le-us", "employee-9", "corr-email-read") // EMAIL
	if _, err := s.CreateNotification(tenantCtx("tenant-a"), n); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := s.MarkRead(tenantCtx("tenant-a"), n.NotificationID, "employee-9", time.Now().UTC())
	if !errors.Is(err, domain.ErrNotificationNotFound) {
		t.Fatalf("an EMAIL notification accepted a read mark: %v", err)
	}
}

func TestPgStore_CountUnread(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	ctx := tenantCtx("tenant-a")
	mine := newInApp("tenant-a", "employee-9", "corr-u1")
	if _, err := s.CreateNotification(ctx, mine); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateNotification(ctx, newInApp("tenant-a", "employee-9", "corr-u2")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateNotification(ctx, newInApp("tenant-a", "other-principal", "corr-u3")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// An EMAIL to the same principal must not be counted: nothing can clear it.
	if _, err := s.CreateNotification(ctx, newNotification("tenant-a", "le-us", "employee-9", "corr-u4")); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Another tenant's notice for a principal with the same id.
	if _, err := s.CreateNotification(tenantCtx("tenant-b"), newInApp("tenant-b", "employee-9", "corr-u5")); err != nil {
		t.Fatalf("create: %v", err)
	}

	count, err := s.CountUnread(ctx, "employee-9")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("unread = %d, want 2 (in-app, this principal, this tenant)", count)
	}

	if err := s.MarkRead(ctx, mine.NotificationID, "employee-9", time.Now().UTC()); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if count, _ = s.CountUnread(ctx, "employee-9"); count != 1 {
		t.Fatalf("after one read, unread = %d, want 1", count)
	}
}

func TestPgStore_ListNotifications_UnreadOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	ctx := tenantCtx("tenant-a")
	read := newInApp("tenant-a", "employee-9", "corr-l1")
	unread := newInApp("tenant-a", "employee-9", "corr-l2")
	for _, n := range []*domain.Notification{read, unread} {
		if _, err := s.CreateNotification(ctx, n); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := s.MarkRead(ctx, read.NotificationID, "employee-9", time.Now().UTC()); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	list, err := s.ListNotifications(ctx, domain.ListFilter{
		RecipientPrincipalID: "employee-9", UnreadOnly: true, Limit: 100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].NotificationID != unread.NotificationID {
		t.Fatalf("unread_only returned %d rows, want just the unread one", len(list))
	}
}
