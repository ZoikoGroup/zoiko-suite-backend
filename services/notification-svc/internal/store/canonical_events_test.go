package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

// countEvents counts outbox rows of one type for one aggregate.
func countEvents(t *testing.T, pool *pgxpool.Pool, aggregate, eventType string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)")
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT COUNT(*) FROM event_outbox WHERE aggregate_key = $1 AND event_type = $2`, aggregate, eventType).Scan(&n))
	return n
}

// communication.prepared: a communication announces itself when it is created, pinned to its
// content, before any delivery is attempted.
func TestEvents_CommunicationPreparedAtCreation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := newNotification("tenant-ev27", "entity-1", "recipient-1", "corr-prep")
	n.RenderedContentHash = "abc123"
	n.IntentVersionID = ""
	created, err := s.CreateNotification(tenantCtx("tenant-ev27"), n)
	require.NoError(t, err)
	require.True(t, created)

	evs := outboxFor(t, pool, n.NotificationID)
	p, ok := evs["communication.prepared"]
	require.True(t, ok, "creation must announce the communication")
	assert.Equal(t, n.NotificationID, p["communication_id"])
	assert.Equal(t, n.NotificationID, p["notification_id"])
	assert.Equal(t, "abc123", p["rendered_content_hash"])
	assert.NotContains(t, p, "body", "events never carry the content")
	assert.NotContains(t, p, "recipient_address")

	// A replay of the same source event creates no second announcement.
	again := newNotification("tenant-ev27", "entity-1", "recipient-1", "corr-prep")
	created, err = s.CreateNotification(tenantCtx("tenant-ev27"), again)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, 1, countEvents(t, pool, n.NotificationID, "communication.prepared"))
}

// communication.blocked: a withheld attempt announces itself with the stable reason code.
func TestEvents_CommunicationBlockedCarriesTheStableCode(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := seedNotification(t, s, "tenant-ev27-b", "corr-blocked")
	require.NoError(t, s.ScheduleRetry(tenantCtx("tenant-ev27-b"), n.NotificationID, "tenant-ev27-b", "NCD-012 QUIET_HOUR_DEFERRED: held",
		time.Now().UTC(), time.Now().Add(time.Hour).UTC(),
		domain.AttemptMeta{Origin: domain.AttemptOriginRequest, BlockCode: "NCD-012"}))

	p, ok := outboxFor(t, pool, n.NotificationID)["communication.blocked"]
	require.True(t, ok)
	assert.Equal(t, "NCD-012", p["reason_code"])
	assert.Equal(t, "QUIET_HOUR_DEFERRED", p["reason"])
	assert.Equal(t, true, p["retryable"])

	// An attempt that was not withheld announces nothing of the kind.
	m := seedNotification(t, s, "tenant-ev27-b", "corr-not-blocked")
	done := time.Now().UTC()
	require.NoError(t, s.CompleteDelivery(tenantCtx("tenant-ev27-b"), m.NotificationID, "SENT", "", "250 ok", &done, "c", domain.AttemptMeta{Origin: domain.AttemptOriginRequest}))
	assert.Zero(t, countEvents(t, pool, m.NotificationID, "communication.blocked"))
}

// delivery.evidence.recorded: each new fact announces itself once.
func TestEvents_EvidenceRecordedOncePerFact(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := sentDirect(t, s, "tenant-ev27-c", "corr-evrec", "<evrec@example.com>")
	proc := webhook.NewProcessor(s, zap.NewNop())
	body := `{"event_id":"evrec-1","event_type":"DELIVERED","recipient_email":"recipient@example.com","provider_message_id":"<evrec@example.com>"}`
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(body)))
	require.NoError(t, proc.ProcessRawPayload(context.Background(), "generic", []byte(body))) // replay

	assert.Equal(t, 1, countEvents(t, pool, n.NotificationID, "delivery.evidence.recorded"), "a replayed callback is not new evidence")
	p := outboxFor(t, pool, n.NotificationID)["delivery.evidence.recorded"]
	assert.Equal(t, "MAILBOX_ACCEPTED", p["normalized_state"])
	assert.Equal(t, "MAILBOX_LEVEL", p["strength"])
	assert.Equal(t, n.NotificationID, p["communication_id"])
	assert.NotEmpty(t, p["attempt_id"])
}

// endpoint.suppressed: announced when a suppression is new or changes, never as the raw
// address, and not again for an unchanged repeat.
func TestEvents_EndpointSuppressedIsHashedAndNotRepeated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-ev27-d"
	const address = "Someone.Private@Example.com"
	sum := sha256.Sum256([]byte("someone.private@example.com"))
	ref := hex.EncodeToString(sum[:])
	add := func(reason ledger.SuppressionReason) {
		require.NoError(t, s.AddSuppression(tenantCtx(tenant), &ledger.EmailSuppression{SuppressionID: uuid.NewString(), TenantID: tenant,
			RecipientEmail: address, Reason: reason, SourceStream: "ALL", CreatedAt: time.Now().UTC()}))
	}

	add(ledger.SuppressionReasonHardBounce)
	add(ledger.SuppressionReasonHardBounce) // the provider repeats itself
	assert.Equal(t, 1, countEvents(t, pool, ref, "endpoint.suppressed"), "an unchanged repeat is not news")
	add(ledger.SuppressionReasonComplaint) // the reason changed
	assert.Equal(t, 2, countEvents(t, pool, ref, "endpoint.suppressed"))

	p := outboxFor(t, pool, ref)["endpoint.suppressed"]
	assert.Equal(t, ref, p["endpoint_ref"])
	assert.Equal(t, "COMPLAINT", p["reason"])
	assert.Equal(t, "ALL", p["scope"])
	for _, v := range p {
		assert.NotContains(t, toString(v), "example.com", "the topic must not list suppressed people's addresses")
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// notice.deadline.at_risk: raised once, only while delivery or a response is still missing and
// the deadline is near.
func TestEvents_NoticeDeadlineAtRiskIsRaisedOnce(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-ev27-e"
	ctx := tenantCtx(tenant)
	iv := regulatedIntentVersion(t, s, tenant, "legal.atrisk")

	near, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(12*time.Hour)))
	require.NoError(t, err)
	far, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(5*24*time.Hour)))
	require.NoError(t, err)
	none, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckNone, time.Time{}))
	require.NoError(t, err)
	for i, n := range []*domain.Notice{near, far, none} {
		deliverNotice(t, s, tenant, n, "<atrisk-"+string(rune('a'+i))+"@example.com>")
	}

	for i := 0; i < 3; i++ { // every sweep, one warning
		_, err = s.RefreshNotice(ctx, near.NoticeID, time.Now())
		require.NoError(t, err)
	}
	assert.Equal(t, 1, countEvents(t, pool, near.NoticeID, "notice.deadline.at_risk"))
	p := outboxFor(t, pool, near.NoticeID)["notice.deadline.at_risk"]
	assert.Equal(t, "DELIVERY_NOT_EVIDENCED", p["deficiency"], "it says what is still missing")

	_, err = s.RefreshNotice(ctx, far.NoticeID, time.Now())
	require.NoError(t, err)
	_, err = s.RefreshNotice(ctx, none.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Zero(t, countEvents(t, pool, far.NoticeID, "notice.deadline.at_risk"), "a distant deadline is not at risk")
	assert.Zero(t, countEvents(t, pool, none.NoticeID, "notice.deadline.at_risk"), "nothing is due")

	// A deadline that has already passed is an exception or an expiry, not "at risk".
	late, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
	require.NoError(t, err)
	deliverNotice(t, s, tenant, late, "<atrisk-late@example.com>")
	_, err = s.RefreshNotice(ctx, late.NoticeID, time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, countEvents(t, pool, late.NoticeID, "notice.deadline.at_risk"))

	// Once delivery is evidenced the deficiency changes, but the warning was already given.
	_, attemptID := func() (*domain.Notification, string) {
		attempts, _ := s.ListAttempts(ctx, *near2(t, s, ctx, near.NoticeID).NotificationID)
		return nil, attempts[0].AttemptID
	}()
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceMailboxAccepted)
	_, err = s.RefreshNotice(ctx, near.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, countEvents(t, pool, near.NoticeID, "notice.deadline.at_risk"), "still raised once")
}

func near2(t *testing.T, s *store.PgStore, ctx context.Context, id string) *domain.Notice {
	t.Helper()
	n, err := s.GetNotice(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, n.NotificationID)
	return n
}

func TestMigration000027_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000027_more_canonical_events.down.sql", "000027_more_canonical_events.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'regulated_notices' AND column_name = 'at_risk_notified_at'`).Scan(&n))
	assert.Equal(t, 1, n)
}
