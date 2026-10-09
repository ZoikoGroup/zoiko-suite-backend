package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/policy"
	"zoiko.io/notification-svc/internal/store"
)

func classed(t *testing.T, s *store.PgStore, tenant, corr, class string) *domain.Notification {
	t.Helper()
	n := newNotification(tenant, "entity-1", "recipient-1", corr)
	n.CommunicationClass = class
	created, err := s.CreateNotification(tenantCtx(tenant), n)
	require.NoError(t, err)
	require.True(t, created)
	return n
}

// The class is stated at creation, stored, and read back.
func TestCommunicationClass_IsStoredAndReadBack(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	n := classed(t, s, "tenant-class", "corr-class-1", "A1")
	got, err := s.GetNotification(tenantCtx("tenant-class"), n.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, "A1", got.CommunicationClass)

	none := classed(t, s, "tenant-class", "corr-class-2", "")
	got, err = s.GetNotification(tenantCtx("tenant-class"), none.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, "", got.CommunicationClass, "a notification that stated no class stays unclassified (and is judged as T0)")
}

// The class decides what can block a message, so it cannot be changed afterwards:
// not by a retry, not by a resend, not by SQL.
func TestCommunicationClass_IsFrozenAndConstrainedByTheDatabase(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	n := classed(t, s, "tenant-class", "corr-class-3", "M1")
	bg := context.Background()

	for name, set := range map[string]string{
		"M1 to T0 (slip past an opt-out)": `communication_class='T0'`,
		"M1 to NULL":                      `communication_class=NULL`,
		"to an unknown class":             `communication_class='Z9'`,
	} {
		if _, err := pool.Exec(bg, `UPDATE notifications SET `+set+` WHERE notification_id=$1`, n.NotificationID); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	bad := newNotification("tenant-class", "entity-1", "recipient-1", "corr-class-4")
	bad.CommunicationClass = "Z9"
	if _, err := s.CreateNotification(tenantCtx("tenant-class"), bad); err == nil {
		t.Error("a notification with an unknown class was created")
	}
}

// The point of step 5: the direct path is judged by the SAME engine as the ledger
// path, with the real suppression store. An unsubscribe must not stop a security or
// transactional notice, and must stop an operational one.
func TestCommunicationClass_SharedGateWithTheRealSuppressionStore(t *testing.T) {
	pool := openAdminTestPool(t)
	s := store.New(pool)
	tenant := "tenant-gate"
	ctx := tenantCtx(tenant)
	engine := policy.NewPrecedenceEngine(s, zap.NewNop())

	suppress := func(email string, reason ledger.SuppressionReason) {
		require.NoError(t, s.AddSuppression(ctx, &ledger.EmailSuppression{
			SuppressionID: uuid.NewString(), TenantID: tenant, RecipientEmail: email, Reason: reason, SourceStream: "ALL", CreatedAt: time.Now().UTC()}))
	}
	suppress("unsub@example.com", ledger.SuppressionReasonUnsubscribe)
	suppress("bounced@example.com", ledger.SuppressionReasonHardBounce)

	del := &countingDeliverer{}
	guard, err := policy.NewDirectSendGuard(del, engine, ledger.NewKillSwitchManager(zap.NewNop()), zap.NewNop())
	require.NoError(t, err)

	send := func(class, email string) domain.DeliveryOutcome {
		return guard.Deliver(ctx, domain.Notification{NotificationID: uuid.NewString(), TenantID: tenant, LegalEntityID: "le",
			RecipientPrincipalID: "p", Channel: domain.ChannelEmail, RecipientAddress: email, CommunicationClass: class})
	}

	// Unsubscribed: security and transactional still go, operational does not.
	assert.True(t, send("S0", "unsub@example.com").Delivered, "an unsubscribe must not stop a security notice")
	assert.True(t, send("T0", "unsub@example.com").Delivered, "an unsubscribe must not stop a transactional notice")
	assert.True(t, send("", "unsub@example.com").Delivered, "no class is judged as T0")
	op := send("A1", "unsub@example.com")
	assert.False(t, op.Delivered, "an unsubscribe does stop an operational notice")
	assert.False(t, op.Retryable)

	// Hard bounce: a physical barrier for every class.
	for _, class := range []string{"S0", "T0", "A1", ""} {
		assert.False(t, send(class, "bounced@example.com").Delivered, "class %q to a hard-bounced address", class)
	}

	// Not suppressed: delivered.
	assert.True(t, send("A1", "fine@example.com").Delivered)

	// Marketing is not a direct send, whoever asks.
	assert.False(t, send("M1", "fine@example.com").Delivered)
	assert.Equal(t, 4, del.calls, "S0, T0, no-class to the unsubscribed address and A1 to the clean one reached the provider; nothing else did")
}

type countingDeliverer struct{ calls int }

func (c *countingDeliverer) Deliver(_ context.Context, _ domain.Notification) domain.DeliveryOutcome {
	c.calls++
	return domain.DeliveryOutcome{Delivered: true, ProviderResponse: "ok"}
}

// The ledger pipeline records the template's class on its register row.
func TestCommunicationClass_LedgerRegisterRowCarriesTheTemplateClass(t *testing.T) {
	s := store.New(openAdminTestPool(t))
	tenant := "tenant-class-ledger"
	ctx := tenantCtx(tenant)
	del := &e2eDeliverer{delivered: true, response: "smtp h accepted; message-id=<class-ledger@example.com>"}
	res, err := registerOrchestrator(t, s, del).IngestEvent(ctx, registerRequest("evt-class-ledger"), "caller")
	require.NoError(t, err)
	id, err := s.NotificationIDForIntent(ctx, res.MessageIntentID)
	require.NoError(t, err)
	n, err := s.GetNotification(ctx, id)
	require.NoError(t, err)
	want := ""
	for _, d := range ledger.DefaultSeedDefinitions() {
		if d.TemplateKey == "ZS-IA-001" {
			want = string(d.CommunicationClass)
		}
	}
	require.NotEmpty(t, want)
	assert.Equal(t, want, n.CommunicationClass, "the register row carries the template class")
}

func TestMigration000019_DownThenUp(t *testing.T) {
	pool := openAdminTestPool(t)
	for _, f := range []string{"000019_notification_communication_class.down.sql", "000019_notification_communication_class.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='notifications' AND column_name='communication_class'`).Scan(&n))
	assert.Equal(t, 1, n)
}
