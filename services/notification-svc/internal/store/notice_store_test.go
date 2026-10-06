package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/store"
)

const noticeRecipient = "recipient-1" // the principal seedNotification addresses

// regulatedIntentVersion publishes an E3 intent version that allows email.
func regulatedIntentVersion(t *testing.T, s *store.PgStore, tenant, key string) *domain.IntentVersion {
	t.Helper()
	i := newIntent(t, s, tenant, key)
	return publishVersion(t, s, tenant, draftVersion(t, s, tenant, i.IntentID, func(p *domain.CreateIntentVersionParams) {
		p.EvidenceClass, p.AllowedChannels = "E3", []string{"EMAIL"}
	}), nil)
}

func noticeParams(iv *domain.IntentVersion, ack string, deadline time.Time) domain.CreateNoticeParams {
	p := domain.CreateNoticeParams{LegalEntityID: iv.LegalEntityID, IntentVersionID: iv.VersionID, RecipientPrincipalID: noticeRecipient,
		Locale: "en", Subject: "Notice of change", Body: "Your terms change on 1 January.", PolicyRef: "PDC-RULE-9",
		AckRequirement: ack, CreatedByPrincipalID: "sender"}
	if ack != domain.AckNone {
		p.DeadlineAt = &deadline
	}
	return p
}

// deliverNotice links a fresh direct notification to the notice and makes the provider
// accept it, exactly as a dispatch followed by a send would.
func deliverNotice(t *testing.T, s *store.PgStore, tenant string, n *domain.Notice, msgID string) (*domain.Notification, string) {
	t.Helper()
	notif := seedNotification(t, s, tenant, "corr-"+n.NoticeID)
	_, started, err := s.BeginNoticeDispatch(tenantCtx(tenant), n.NoticeID, notif.NotificationID, "sender")
	require.NoError(t, err)
	require.True(t, started)
	require.NoError(t, complete(t, s, tenant, notif.NotificationID, "SENT", "", receiptFor(msgID)))
	attempts, err := s.ListAttempts(tenantCtx(tenant), notif.NotificationID)
	require.NoError(t, err)
	return notif, attempts[0].AttemptID
}

func mailboxAccepted(t *testing.T, s *store.PgStore, tenant, attemptID, fact string) {
	t.Helper()
	ok, err := s.RecordDeliveryEvidence(context.Background(), &domain.DeliveryEvidence{TenantID: tenant, AttemptID: attemptID,
		SourceEventID: uuid.NewString(), Provider: "generic", Fact: fact, Strength: domain.StrengthMailboxLevel, OccurredAt: time.Now().UTC()})
	require.NoError(t, err)
	require.True(t, ok)
}

func TestNotice_LifecycleFromPreparedToAcknowledged(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-notice"
	ctx := tenantCtx(tenant)
	iv := regulatedIntentVersion(t, s, tenant, "legal.notice_of_change")

	n, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(72*time.Hour)))
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeReady, n.Status, "validated into READY")
	assert.Equal(t, 1, n.VersionNumber)
	assert.Equal(t, domain.NoticeContentHash(n.Subject, n.Body, n.Locale), n.ContentHash)

	notif, attemptID := deliverNotice(t, s, tenant, n, "<notice-1@example.com>")

	// Provider acceptance alone is NOT delivery evidence.
	n, err = s.RefreshNotice(ctx, n.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeDeliveryInProgess, n.Status, "SENT with no mailbox acceptance must not advance")

	// The receiving mail server's acceptance is.
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceMailboxAccepted)
	n, err = s.RefreshNotice(ctx, n.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeAckPending, n.Status, "evidenced, and a response is required")

	// Someone other than the recipient cannot respond, whatever their role.
	_, _, err = s.RecordNoticeAck(ctx, n.NoticeID, "operator-1", domain.ActionAcknowledge, "on their behalf", time.Now())
	assert.ErrorIs(t, err, domain.ErrNotNoticeRecipient)
	// The wrong kind of action for this requirement is refused.
	_, _, err = s.RecordNoticeAck(ctx, n.NoticeID, noticeRecipient, domain.ActionDecline, "", time.Now())
	assert.ErrorIs(t, err, domain.ErrNoticeInvalid)

	n, ack, err := s.RecordNoticeAck(ctx, n.NoticeID, noticeRecipient, domain.ActionAcknowledge, "got it", time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeAcknowledged, n.Status)
	assert.Equal(t, noticeRecipient, ack.ActorPrincipalID)
	assert.Equal(t, domain.AckMethodAuthAction, ack.Method)

	// One response per notice version.
	_, _, err = s.RecordNoticeAck(ctx, n.NoticeID, noticeRecipient, domain.ActionDispute, "", time.Now())
	assert.ErrorIs(t, err, domain.ErrNoticeAlreadyAnswered)

	history, err := s.ListNoticeTransitions(ctx, n.NoticeID)
	require.NoError(t, err)
	var path []string
	for _, h := range history {
		path = append(path, h.ToStatus)
	}
	assert.Equal(t, []string{"PREPARED", "READY", "DELIVERY_IN_PROGRESS", "DELIVERY_EVIDENCED", "ACK_PENDING", "ACKNOWLEDGED"}, path)

	got, err := s.GetNoticeAck(ctx, n.NoticeID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "got it", got.Comment)

	// Each move announced itself, without content.
	evs := outboxFor(t, pool, n.NoticeID)
	for _, typ := range []string{"notice.dispatched", "notice.delivery_evidenced", "notice.acknowledged"} {
		require.Contains(t, evs, typ)
	}
	assert.Equal(t, n.ContentHash, evs["notice.acknowledged"]["content_hash"])
	assert.Equal(t, notif.NotificationID, evs["notice.dispatched"]["communication_id"])
	assert.NotContains(t, evs["notice.acknowledged"], "body", "events never carry the content")
}

func TestNotice_NoAcknowledgementRequiredIsSatisfiedByPolicyNotLegallyServed(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-notice-none"
	iv := regulatedIntentVersion(t, s, tenant, "legal.fyi")
	n, err := s.CreateNotice(tenantCtx(tenant), noticeParams(iv, domain.AckNone, time.Time{}))
	require.NoError(t, err)
	_, attemptID := deliverNotice(t, s, tenant, n, "<notice-none@example.com>")
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceMailboxAccepted)

	n, err = s.RefreshNotice(tenantCtx(tenant), n.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeSatisfiedByPolicy, n.Status)

	_, _, err = s.RecordNoticeAck(tenantCtx(tenant), n.NoticeID, noticeRecipient, domain.ActionAcknowledge, "", time.Now())
	assert.ErrorIs(t, err, domain.ErrAckNotRequired)
}

func TestNotice_ABounceOrAFailureIsAnExceptionNeverEvidence(t *testing.T) {
	s := store.New(openTestPool(t))
	tenant := "tenant-notice-bounce"
	ctx := tenantCtx(tenant)
	iv := regulatedIntentVersion(t, s, tenant, "legal.bounce")

	n, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
	require.NoError(t, err)
	_, attemptID := deliverNotice(t, s, tenant, n, "<notice-bounce@example.com>")
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceMailboxAccepted)
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceBounced)
	n, err = s.RefreshNotice(ctx, n.NoticeID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeException, n.Status, "a bounce beats an acceptance")

	// A response to a notice that never reached anyone is refused.
	_, _, err = s.RecordNoticeAck(ctx, n.NoticeID, noticeRecipient, domain.ActionAcknowledge, "", time.Now())
	assert.ErrorIs(t, err, domain.ErrNoticeState)
}

func TestNotice_NoResponseByTheDeadlineExpiresAndNeverAcknowledges(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-notice-expire"
	ctx := tenantCtx(tenant)
	iv := regulatedIntentVersion(t, s, tenant, "legal.expire")
	deadline := time.Now().Add(2 * time.Hour)
	n, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckAcceptDecline, deadline))
	require.NoError(t, err)
	_, attemptID := deliverNotice(t, s, tenant, n, "<notice-expire@example.com>")
	mailboxAccepted(t, s, tenant, attemptID, domain.EvidenceMailboxAccepted)
	n, err = s.RefreshNotice(ctx, n.NoticeID, time.Now())
	require.NoError(t, err)
	require.Equal(t, domain.NoticeAckPending, n.Status)

	n, err = s.RefreshNotice(ctx, n.NoticeID, deadline.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, domain.NoticeExpired, n.Status)
	assert.Contains(t, outboxFor(t, pool, n.NoticeID), "notice.expired", "the exception is announced")

	// A late response is refused, and the expiry it ran into is still recorded.
	late, _, err := s.RecordNoticeAck(ctx, n.NoticeID, noticeRecipient, domain.ActionAccept, "", deadline.Add(time.Hour))
	assert.ErrorIs(t, err, domain.ErrNoticeState)
	assert.Equal(t, domain.NoticeExpired, late.Status)
	ack, err := s.GetNoticeAck(ctx, n.NoticeID)
	require.NoError(t, err)
	assert.Nil(t, ack, "no acknowledgement was fabricated")

	// A notice that expires without ever being answered is found by the sweeper before that.
	open, err := s.FindOpenNotices(context.Background(), 10)
	require.NoError(t, err)
	assert.Empty(t, open, "an expired notice is no longer open")
}

func TestNotice_ASweeperFindsOpenNoticesAcrossTenants(t *testing.T) {
	s := store.New(openTestPool(t))
	for _, tenant := range []string{"tenant-sweep-a", "tenant-sweep-b"} {
		iv := regulatedIntentVersion(t, s, tenant, "legal.sweep")
		n, err := s.CreateNotice(tenantCtx(tenant), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
		require.NoError(t, err)
		deliverNotice(t, s, tenant, n, "<sweep-"+tenant+"@example.com>")
	}
	open, err := s.FindOpenNotices(context.Background(), 10)
	require.NoError(t, err)
	tenants := map[string]bool{}
	for _, o := range open {
		tenants[o.TenantID] = true
	}
	assert.True(t, tenants["tenant-sweep-a"] && tenants["tenant-sweep-b"])
}

func TestNotice_ACorrectionIsANewVersionAndTheOriginalStaysVisible(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-notice-correct"
	ctx := tenantCtx(tenant)
	iv := regulatedIntentVersion(t, s, tenant, "legal.correct")
	v1, err := s.CreateNotice(ctx, noticeParams(iv, domain.AckReceipt, time.Now().Add(48*time.Hour)))
	require.NoError(t, err)

	fix := noticeParams(iv, domain.AckReceipt, time.Now().Add(96*time.Hour))
	fix.Body, fix.SupersedesNoticeID, fix.CorrectionReason = "Your terms change on 1 February.", v1.NoticeID, "the date was wrong"
	v2, err := s.CreateNotice(ctx, fix)
	require.NoError(t, err)
	assert.Equal(t, 2, v2.VersionNumber)
	assert.Equal(t, v1.LineageID, v2.LineageID)
	require.NotNil(t, v2.SupersedesNoticeID)
	assert.Equal(t, v1.NoticeID, *v2.SupersedesNoticeID)
	assert.NotEqual(t, v1.ContentHash, v2.ContentHash)

	old, err := s.GetNotice(ctx, v1.NoticeID)
	require.NoError(t, err)
	assert.Equal(t, "Your terms change on 1 January.", old.Body, "the version already written is untouched")
	require.NotNil(t, old.SupersededByNoticeID)
	assert.Equal(t, v2.NoticeID, *old.SupersededByNoticeID, "and it says it was superseded")
	assert.Contains(t, outboxFor(t, pool, v2.NoticeID), "notice.corrected")

	// A superseded version that was never sent cannot now be sent.
	_, _, err = s.BeginNoticeDispatch(ctx, v1.NoticeID, seedNotification(t, s, tenant, "corr-sup").NotificationID, "sender")
	assert.ErrorIs(t, err, domain.ErrNoticeState)

	// Each version is corrected at most once, and a correction needs its reason.
	again := fix
	again.CorrectionReason = ""
	_, err = s.CreateNotice(ctx, again)
	assert.ErrorIs(t, err, domain.ErrNoticeState, "v1 already has a correction")
	other := fix
	other.SupersedesNoticeID, other.RecipientPrincipalID = v2.NoticeID, "someone-else"
	_, err = s.CreateNotice(ctx, other)
	assert.ErrorIs(t, err, domain.ErrNoticeInvalid, "a different recipient is a new notice, not a correction")
}

func TestNotice_TheDatabaseHoldsTheLineEvenIfTheServiceDoesNot(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-notice-db"
	iv := regulatedIntentVersion(t, s, tenant, "legal.db")
	n, err := s.CreateNotice(tenantCtx(tenant), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
	require.NoError(t, err)
	_, err = s.CreateNotice(tenantCtx(tenant), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
	require.NoError(t, err)

	exec := func(q string, args ...any) error {
		conn, err := pool.Acquire(context.Background())
		require.NoError(t, err)
		defer conn.Release()
		_, err = conn.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, false)`, tenant)
		require.NoError(t, err)
		_, err = conn.Exec(context.Background(), q, args...)
		return err
	}
	for name, c := range map[string][2]string{
		"content is immutable":             {`UPDATE regulated_notices SET body = 'changed' WHERE notice_id = $1`, "immutable"},
		"the hash is immutable":            {`UPDATE regulated_notices SET content_hash = repeat('a', 64) WHERE notice_id = $1`, "immutable"},
		"a ready notice cannot skip ahead": {`UPDATE regulated_notices SET status = 'ACKNOWLEDGED' WHERE notice_id = $1`, "cannot move"},
		"nor be sent without a delivery":   {`UPDATE regulated_notices SET status = 'DELIVERY_IN_PROGRESS' WHERE notice_id = $1`, "cannot move"},
		"notices are never deleted":        {`DELETE FROM regulated_notices WHERE notice_id = $1`, "never deleted"},
	} {
		err := exec(c[0], n.NoticeID)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), c[1], name)
	}
	assert.Error(t, exec(`UPDATE regulated_notice_events SET reason = 'x'`), "history is append-only")
	assert.Error(t, exec(`DELETE FROM regulated_notice_events`), "history is append-only")

	// Operator acknowledgement is not a method that can even be stored.
	_, _, err = s.RecordNoticeAck(tenantCtx(tenant), n.NoticeID, noticeRecipient, domain.ActionAcknowledge, "", time.Now())
	assert.ErrorIs(t, err, domain.ErrNoticeState, "not yet delivered, so nothing to respond to")
	assert.Error(t, exec(`INSERT INTO notice_acknowledgements (tenant_id, notice_id, action, actor_principal_id, method) VALUES ($1, $2::uuid, 'ACKNOWLEDGE', 'op', 'OPERATOR_MARKED')`, tenant, n.NoticeID))
	assert.Error(t, exec(`INSERT INTO notice_acknowledgements (tenant_id, notice_id, action, actor_principal_id, method) VALUES ($1, $2::uuid, 'EMAIL_OPENED', 'x', 'AUTHENTICATED_ACTION')`, tenant, n.NoticeID))
}

func TestNotice_RowLevelSecurityKeepsTenantsApart(t *testing.T) {
	s := store.New(openTestPool(t))
	iv := regulatedIntentVersion(t, s, "tenant-notice-a", "legal.rls")
	n, err := s.CreateNotice(tenantCtx("tenant-notice-a"), noticeParams(iv, domain.AckNone, time.Time{}))
	require.NoError(t, err)
	_, err = s.GetNotice(tenantCtx("tenant-notice-b"), n.NoticeID)
	assert.ErrorIs(t, err, domain.ErrNoticeNotFound)
}

func TestMigration000025_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	for _, f := range []string{"000025_regulated_notices.down.sql", "000025_regulated_notices.up.sql"} {
		b, err := os.ReadFile("../../deployments/migrations/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(context.Background(), string(b))
		require.NoError(t, err, f)
	}
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_name IN ('regulated_notices','regulated_notice_events','notice_acknowledgements')`).Scan(&n))
	assert.Equal(t, 3, n)
}
