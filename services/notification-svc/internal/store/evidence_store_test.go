package store_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

// sentDirect seeds a direct notification that a provider accepted under the given Message-ID.
func sentDirect(t *testing.T, s *store.PgStore, tenant, corr, msgID string) *domain.Notification {
	t.Helper()
	n := seedNotification(t, s, tenant, corr)
	require.NoError(t, complete(t, s, tenant, n.NotificationID, "SENT", "", receiptFor(msgID)))
	return n
}

func TestEvidence_ADirectSendsCallbacksBecomeNormalizedFacts(t *testing.T) {
	s := store.New(openTestPool(t))
	n := sentDirect(t, s, "tenant-ev", "corr-ev-1", "<ev-1@example.com>")
	proc := webhook.NewProcessor(s, zap.NewNop())

	deliver := `{"event_id":"` + uuid.NewString() + `","event_type":"DELIVERED","recipient_email":"recipient@example.com","provider_message_id":"<ev-1@example.com>"}`
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(deliver)))
	bounceID := uuid.NewString()
	bounce := `{"event_id":"` + bounceID + `","event_type":"BOUNCE","bounce_type":"HARD","recipient_email":"recipient@example.com","provider_message_id":"<ev-1@example.com>","diagnostic_code":"550 5.1.1 User unknown"}`
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(bounce)))
	// The provider delivers the same callback again: nothing is added.
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(bounce)))

	got, err := s.ListDeliveryEvidence(tenantCtx("tenant-ev"), n.NotificationID)
	require.NoError(t, err)
	require.Len(t, got, 2, "a replayed callback adds no fact")
	byFact := map[string]domain.DeliveryEvidence{}
	for _, e := range got {
		byFact[e.Fact] = e
	}
	assert.Equal(t, domain.StrengthMailboxLevel, byFact["MAILBOX_ACCEPTED"].Strength)
	assert.Contains(t, byFact["MAILBOX_ACCEPTED"].Limits, "does not prove")
	assert.Equal(t, "550 5.1.1 User unknown", byFact["BOUNCED"].Diagnostic)
	assert.Equal(t, bounceID, byFact["BOUNCED"].SourceEventID)

	attempts, err := s.ListAttempts(tenantCtx("tenant-ev"), n.NotificationID)
	require.NoError(t, err)
	assert.Equal(t, attempts[0].AttemptID, byFact["BOUNCED"].AttemptID, "tied to the exact attempt")

	// The hard bounce still suppresses, as before.
	suppressed, _, err := s.IsEmailSuppressed(tenantCtx("tenant-ev"), "tenant-ev", "recipient@example.com", ledger.StreamTransactional, ledger.ClassT0)
	require.NoError(t, err)
	assert.True(t, suppressed)
}

func TestEvidence_IsTenantIsolatedAndAppendOnly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := sentDirect(t, s, "tenant-ev-a", "corr-ev-2", "<ev-2@example.com>")
	proc := webhook.NewProcessor(s, zap.NewNop())
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(
		`{"event_id":"`+uuid.NewString()+`","event_type":"DELIVERED","recipient_email":"recipient@example.com","provider_message_id":"<ev-2@example.com>"}`)))

	other, err := s.ListDeliveryEvidence(tenantCtx("tenant-ev-b"), n.NotificationID)
	require.NoError(t, err)
	assert.Empty(t, other, "another tenant sees none of it")

	for _, q := range []string{
		`UPDATE notification_delivery_evidence SET fact = 'BOUNCED'`,
		`DELETE FROM notification_delivery_evidence`,
	} {
		conn, err := pool.Acquire(context.Background())
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id','tenant-ev-a',false)`)
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), q)
		conn.Release()
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), "append-only", q)
	}
}

func TestEvidence_RefusesWhatItCannotAttach(t *testing.T) {
	s := store.New(openTestPool(t))
	base := domain.DeliveryEvidence{TenantID: "tenant-ev-c", AttemptID: uuid.NewString(), SourceEventID: "e", Provider: "generic",
		Fact: domain.EvidenceBounced, Strength: domain.StrengthMailboxLevel, OccurredAt: time.Now().UTC()}

	_, err := s.RecordDeliveryEvidence(context.Background(), &base)
	assert.ErrorIs(t, err, webhook.ErrAttemptNotFound, "an attempt that does not exist in the tenant has no evidence")

	n := sentDirect(t, s, "tenant-ev-c", "corr-ev-3", "<ev-3@example.com>")
	attempts, err := s.ListAttempts(tenantCtx("tenant-ev-c"), n.NotificationID)
	require.NoError(t, err)
	wrongTenant := base
	wrongTenant.TenantID, wrongTenant.AttemptID = "tenant-ev-d", attempts[0].AttemptID
	_, err = s.RecordDeliveryEvidence(context.Background(), &wrongTenant)
	assert.ErrorIs(t, err, webhook.ErrAttemptNotFound, "another tenant cannot file a fact against this attempt")

	bad := base
	bad.AttemptID, bad.Fact = attempts[0].AttemptID, "ACKNOWLEDGED"
	_, err = s.RecordDeliveryEvidence(context.Background(), &bad)
	require.Error(t, err, "a provider callback can never record that a person acknowledged")
	assert.Contains(t, err.Error(), "ck_nde_fact")

	long := base
	long.AttemptID, long.Diagnostic = attempts[0].AttemptID, strings.Repeat("x", 500)
	ok, err := s.RecordDeliveryEvidence(context.Background(), &long)
	require.NoError(t, err)
	assert.True(t, ok)
	got, err := s.ListDeliveryEvidence(tenantCtx("tenant-ev-c"), n.NotificationID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Len(t, got[0].Diagnostic, domain.MaxDiagnosticLength, "provider free text is minimized")
}

func TestMigration000024_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000024_delivery_evidence.down.sql", "000024_delivery_evidence.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'notification_delivery_evidence'`).Scan(&n))
	assert.Equal(t, 1, n)
}
