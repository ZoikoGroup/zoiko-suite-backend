package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/policy"
	"zoiko.io/notification-svc/internal/preference"
	"zoiko.io/notification-svc/internal/privacy"
	"zoiko.io/notification-svc/internal/quota"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

// The ZS-SVC-Y-001 negative-path matrix (NP-01 to NP-60) as an executable suite.
//
// Every scenario the service now handles has a subtest named after it that asserts the
// refusal. Every scenario it still cannot handle is a subtest that SKIPS with the reason, so
// the run itself lists what is not built and the matrix cannot claim more than the code does.
// Scenarios covered thoroughly by an existing test say which one, and a few re-prove the
// essential refusal here so the matrix is readable in one place.

type npInner struct{ calls int }

func (n *npInner) Deliver(context.Context, domain.Notification) domain.DeliveryOutcome {
	n.calls++
	return domain.DeliveryOutcome{Delivered: true}
}

type npPolicy struct{}

func (npPolicy) Evaluate(context.Context, *ledger.MessageIntent, ledger.SenderStream) (ledger.PolicyDecision, error) {
	return ledger.PolicyDecision{Allowed: true}, nil
}

type npKill struct{}

func (npKill) Check(context.Context, string, string) (bool, string) { return false, "" }

type npPrivacy struct{ out privacy.Outcome }

func (p npPrivacy) Check(context.Context, domain.Notification) privacy.Outcome { return p.out }

func skipNP(t *testing.T, name, why string) {
	t.Run(name, func(t *testing.T) { t.Skip("NOT BUILT: " + why) })
}

func TestNP_Matrix(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// ── NCD-01 intent, template and content ────────────────────────────────────────────
	skipNP(t, "NP-01 domain calls provider SDK directly", "no architecture test or secret policy")

	t.Run("NP-02 unknown intent id", func(t *testing.T) {
		_, err := s.EffectiveIntentVersion(tenantCtx("np"), uuid.NewString(), time.Now(), time.Now())
		assert.ErrorIs(t, err, domain.ErrIntentNotFound)
		_, err = s.EffectiveIntentVersion(tenantCtx("np"), "not-a-uuid", time.Now(), time.Now())
		assert.ErrorIs(t, err, domain.ErrIntentNotFound)
	})

	t.Run("NP-03 retired or not-yet-effective intent", func(t *testing.T) {
		i := newIntent(t, s, "np-03", "np.retired")
		publishVersion(t, s, "np-03", draftVersion(t, s, "np-03", i.IntentID, nil), nil)
		_, err := s.EffectiveIntentVersion(tenantCtx("np-03"), i.IntentID, time.Now().Add(-24*time.Hour), time.Now())
		assert.ErrorIs(t, err, domain.ErrIntentNotEffective, "before it took effect")
		_, err = s.EffectiveIntentVersion(tenantCtx("np-03"), i.IntentID, time.Now(), time.Now())
		require.NoError(t, err)
		_, err = s.RetireIntent(tenantCtx("np-03"), i.IntentID, "owner")
		require.NoError(t, err)
		_, err = s.EffectiveIntentVersion(tenantCtx("np-03"), i.IntentID, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
		assert.ErrorIs(t, err, domain.ErrIntentNotEffective, "after it was retired")
	})

	t.Run("NP-04 draft template referenced", func(t *testing.T) {
		tmpl := newTestTemplate(t, s, tenantCtx("np-04"), "owner")
		_, err := s.CreateVersion(tenantCtx("np-04"), domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>x</p>", CreatedByPrincipalID: "owner"})
		require.NoError(t, err)
		_, err = s.GetPublishedVersion(tenantCtx("np-04"), tmpl.TemplateID, "en-US")
		assert.Error(t, err, "only a published version can be used")
	})

	t.Run("NP-05 published template edited in place", func(t *testing.T) {
		tmpl := newTestTemplate(t, s, tenantCtx("np-05"), "owner")
		v, err := s.CreateVersion(tenantCtx("np-05"), domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>x</p>", CreatedByPrincipalID: "owner"})
		require.NoError(t, err)
		_, err = s.ValidateTemplate(tenantCtx("np-05"), v.VersionID)
		require.NoError(t, err)
		_, err = s.ApproveTemplate(tenantCtx("np-05"), domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "approver"})
		require.NoError(t, err)
		_, err = s.PublishTemplate(tenantCtx("np-05"), domain.PublishVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "approver"})
		require.NoError(t, err)
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer conn.Release()
		_, err = conn.Exec(ctx, `SELECT set_config('app.tenant_id','np-05',false)`)
		require.NoError(t, err)
		_, err = conn.Exec(ctx, `UPDATE template_versions SET content = '<p>changed</p>' WHERE version_id = $1`, v.VersionID)
		assert.Error(t, err, "the database refuses the edit")
	})

	t.Run("NP-06 and NP-07 required variable missing, unexpected variable", func(t *testing.T) {
		c := map[string]domain.VariableSpec{"first_name": {Type: "STRING", Required: true, Sensitivity: "S1", MaxLength: 40}}
		assert.Error(t, domain.CheckVariables(c, map[string]string{}), "NP-06")
		assert.Error(t, domain.CheckVariables(c, map[string]string{"first_name": "Ann", "salary": "90000"}), "NP-07")
		assert.NoError(t, domain.CheckVariables(c, map[string]string{"first_name": "Ann"}))
	})

	t.Run("NP-08 CRLF in a subject or address", func(t *testing.T) {
		c := map[string]domain.VariableSpec{"name": {Type: "STRING", Required: true, Sensitivity: "S1", MaxLength: 80}}
		assert.Error(t, domain.CheckVariables(c, map[string]string{"name": "Ann\r\nBcc: attacker@example.com"}), "a control character or line break is refused")
	})

	t.Run("NP-09 unapproved locale, no silent fallback", func(t *testing.T) {
		tmpl := newTestTemplate(t, s, tenantCtx("np-09"), "owner")
		v, err := s.CreateVersion(tenantCtx("np-09"), domain.CreateVersionParams{TemplateID: tmpl.TemplateID, Locale: "en-US", Content: "<p>x</p>", CreatedByPrincipalID: "owner"})
		require.NoError(t, err)
		_, _ = s.ValidateTemplate(tenantCtx("np-09"), v.VersionID)
		_, _ = s.ApproveTemplate(tenantCtx("np-09"), domain.ApproveVersionParams{VersionID: v.VersionID, ApprovedByPrincipalID: "approver"})
		_, err = s.PublishTemplate(tenantCtx("np-09"), domain.PublishVersionParams{VersionID: v.VersionID, PublishedByPrincipalID: "approver"})
		require.NoError(t, err)
		_, err = s.GetPublishedVersion(tenantCtx("np-09"), tmpl.TemplateID, "fr-FR")
		assert.Error(t, err, "a locale with no published version is not replaced by another")
	})

	skipNP(t, "NP-10 attachment 'latest' pointer", "no attachment slots or DRC versions")
	skipNP(t, "NP-11 free-text legal recipient", "F-06: no regulated-endpoint control for free-text addresses on the direct path")

	// ── NCD-02 recipient, preference, privacy ──────────────────────────────────────────
	t.Run("NP-12 recipient in another tenant", func(t *testing.T) {
		n := seedNotification(t, s, "np-12-a", "corr-np-12")
		_, err := s.GetNotification(tenantCtx("np-12-b"), n.NotificationID)
		assert.ErrorIs(t, err, domain.ErrNotificationNotFound)
	})

	t.Run("NP-13 endpoint previously hard-bounced", func(t *testing.T) {
		seedNotification(t, s, "np-13", "corr-np-13")
		require.NoError(t, s.AddSuppression(tenantCtx("np-13"), &ledger.EmailSuppression{SuppressionID: uuid.NewString(), TenantID: "np-13",
			RecipientEmail: "bounced@example.com", Reason: ledger.SuppressionReasonHardBounce, SourceStream: "ALL", CreatedAt: time.Now()}))
		for _, class := range []ledger.CommunicationClass{ledger.ClassS0, ledger.ClassT0, ledger.ClassA1} {
			stream, _ := ledger.StreamForDirectClass(class)
			suppressed, _, err := s.IsEmailSuppressed(tenantCtx("np-13"), "np-13", "bounced@example.com", stream, class)
			require.NoError(t, err)
			assert.True(t, suppressed, "even a security or transactional message does not go to a bounced endpoint (%s)", class)
		}
	})

	t.Run("NP-14 marketing is not a direct send", func(t *testing.T) {
		for _, c := range []ledger.CommunicationClass{ledger.ClassM1, ledger.ClassL1} {
			_, ok := ledger.StreamForDirectClass(c)
			assert.False(t, ok, "%s must use the ledger pipeline, which has the opt-out and one-click unsubscribe", c)
		}
	})

	night := time.Date(2026, 1, 10, 23, 30, 0, 0, time.UTC)
	prefs := &domain.RecipientPreferences{TimeZone: "Europe/London", MutedChannels: []string{"SMS", "EMAIL"}, QuietStart: ptr("22:00"), QuietEnd: ptr("07:00")}

	t.Run("NP-15 muted channel, urgent security policy", func(t *testing.T) {
		assert.Equal(t, preference.Proceed, preference.Evaluate(prefs, "S0", "EMAIL", night).Effect, "a security notice ignores a mute")
	})
	t.Run("NP-16 muted channel, routine reminder", func(t *testing.T) {
		assert.Equal(t, preference.Mute, preference.Evaluate(prefs, "A1", "EMAIL", night).Effect)
	})
	t.Run("NP-19 quiet hours, routine message", func(t *testing.T) {
		d := preference.Evaluate(&domain.RecipientPreferences{TimeZone: "Europe/London", QuietStart: ptr("22:00"), QuietEnd: ptr("07:00")}, "A1", "EMAIL", night)
		assert.Equal(t, preference.Defer, d.Effect)
		assert.True(t, d.Until.After(night))
	})
	t.Run("NP-20 recipient time zone missing", func(t *testing.T) {
		assert.ErrorIs(t, domain.SetPreferencesParams{TimeZone: "", UpdatedBy: "x"}.Validate(), domain.ErrPreferencesInvalid, "a profile without a zone is refused, never guessed")
		assert.Equal(t, preference.Unusable, preference.Evaluate(&domain.RecipientPreferences{TimeZone: "Nowhere/Land", QuietStart: ptr("22:00"), QuietEnd: ptr("07:00")}, "A1", "EMAIL", night).Effect)
	})

	t.Run("NP-17 privacy decision INDETERMINATE fails closed", func(t *testing.T) {
		v := privacy.Judge(&privacy.Decision{DecisionID: "d", Result: privacy.ResultIndeterminate}, nil)
		assert.False(t, v.Allow)
		assert.True(t, v.Retryable, "ask again later; never assume permission")
		assert.False(t, privacy.Judge(nil, privacy.ErrUnavailable).Allow, "an unreachable authority is not permission")
	})

	t.Run("NP-18 mandatory notice does not override a privacy block", func(t *testing.T) {
		inner := &npInner{}
		g, err := policy.NewDirectSendGuard(inner, npPolicy{}, npKill{}, zap.NewNop())
		require.NoError(t, err)
		g.WithPrivacyGate(npPrivacy{out: privacy.Outcome{Applies: true, Verdict: privacy.Verdict{Result: privacy.ResultBlock, DecisionID: "d", Reason: "NCD-008 blocked"}}})
		out := g.Deliver(ctx, domain.Notification{NotificationID: "n", TenantID: "t", Channel: domain.ChannelEmail, CommunicationClass: "S0", IntentVersionID: "iv"})
		assert.False(t, out.Delivered, "a security notice is still not sent when privacy blocks it")
		assert.Zero(t, inner.calls)
		assert.Equal(t, privacy.ResultBlock, out.PrivacyResult)
	})

	// ── NCD-03 delivery ────────────────────────────────────────────────────────────────
	t.Run("NP-21 duplicate source event", func(t *testing.T) {
		n := newNotification("np-21", "entity-1", "recipient-1", "corr-np-21")
		created, err := s.CreateNotification(tenantCtx("np-21"), n)
		require.NoError(t, err)
		require.True(t, created)
		again := newNotification("np-21", "entity-1", "recipient-1", "corr-np-21")
		created, err = s.CreateNotification(tenantCtx("np-21"), again)
		require.NoError(t, err)
		assert.False(t, created, "the same source event creates one communication")
	})

	t.Run("NP-22 timeout after submit is UNKNOWN, never resent blindly", func(t *testing.T) {
		n := seedNotification(t, s, "np-22", "corr-np-22")
		require.NoError(t, s.BeginSubmission(tenantCtx("np-22"), n.NotificationID, "np-22", time.Now()))
		require.NoError(t, s.MarkOutcomeUnknown(tenantCtx("np-22"), n.NotificationID, "np-22", "timeout after submit", time.Now(), "corr-np-22", domain.AttemptMeta{}))
		got, err := s.GetNotification(tenantCtx("np-22"), n.NotificationID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusPendingUnknown, got.Status)
		_, err = s.BeginResend(tenantCtx("np-22"), n.NotificationID, "np-22", "agent", "customer asked", time.Now())
		assert.Error(t, err, "an ambiguous attempt must be resolved before any resend")
	})

	t.Run("NP-51 a rate limit never silently drops work and a backlog keeps its priority", func(t *testing.T) {
		qs := store.New(pool).WithQuota(quota.Limits{TenantPerMinute: 1})
		require.NoError(t, send(qs, "np-51", "corr-np-51-a", "p1", "A1"))
		err := send(qs, "np-51", "corr-np-51-b", "p2", "A1")
		ex := exceeded(t, err)
		assert.GreaterOrEqual(t, int(ex.RetryAfter.Seconds()), 1, "the caller is told when to retry")
		assert.Equal(t, 1, countRows(t, qs, "np-51"), "an overflow is refused outright, never half-accepted or quietly lost")
		// The backlog that does exist is served security first (see TestPriority_DueSecurityMessagesAreFoundBeforeRoutineOnes).
	})

	t.Run("NP-53 a security alert is not starved by a routine blast", func(t *testing.T) {
		qs := store.New(pool).WithQuota(quota.Limits{TenantPerMinute: 2, TenantS0PerMinute: 3})
		for i := 0; i < 2; i++ {
			require.NoError(t, send(qs, "np-53", fmt.Sprintf("blast-%d", i), fmt.Sprintf("b%d", i), "A1"))
		}
		exceeded(t, send(qs, "np-53", "blast-over", "b9", "A1"))
		for i := 0; i < 3; i++ { // the whole of the security allowance
			require.NoError(t, send(qs, "np-53", fmt.Sprintf("alert-%d", i), fmt.Sprintf("victim%d", i), "S0"), "the protected pool is untouched by the blast")
		}
	})

	t.Run("NP-52 a job that expires before it is submitted is never submitted", func(t *testing.T) {
		n := queuedNotification(t, s, "np-52", "corr-np-52", nil, tp(time.Hour))
		ok, err := s.ExpireNotification(tenantCtx("np-52"), n.NotificationID, "np-52", time.Now().Add(2*time.Hour))
		require.NoError(t, err)
		assert.True(t, ok)
		got, err := s.GetNotification(tenantCtx("np-52"), n.NotificationID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusExpired, got.Status)
		assert.Contains(t, got.FailureReason, "NCD-015 DELIVERY_EXPIRED")
		_, err = s.BeginResend(tenantCtx("np-52"), n.NotificationID, "np-52", "agent", "send anyway", time.Now())
		assert.Error(t, err, "an expired communication cannot be revived by a resend")
	})

	// ── NCD-04 evidence ────────────────────────────────────────────────────────────────
	t.Run("NP-24 callback duplicated", func(t *testing.T) {
		n := sentDirect(t, s, "np-24", "corr-np-24", "<np-24@example.com>")
		proc := webhook.NewProcessor(s, zap.NewNop())
		body := `{"event_id":"dup-1","event_type":"DELIVERED","recipient_email":"recipient@example.com","provider_message_id":"<np-24@example.com>"}`
		require.NoError(t, proc.ProcessRawPayload(ctx, "generic", []byte(body)))
		require.NoError(t, proc.ProcessRawPayload(ctx, "generic", []byte(body)))
		facts, err := s.ListDeliveryEvidence(tenantCtx("np-24"), n.NotificationID)
		require.NoError(t, err)
		assert.Len(t, facts, 1)
	})

	t.Run("NP-26 callback signature invalid", func(t *testing.T) {
		v := webhook.NewVerifier(map[string][]string{"ses": {"0123456789abcdef0123"}}, 5*time.Minute)
		assert.Error(t, v.Verify("ses", "t=1,v1=deadbeef", []byte(`{}`)))
		assert.Error(t, v.Verify("ses", "", []byte(`{}`)))
		assert.Error(t, v.Verify("unknown-provider", "t=1,v1=aa", []byte(`{}`)), "a provider with no secret is closed")
	})

	t.Run("NP-27 provider message id maps to two attempts", func(t *testing.T) {
		const id = "<np-27-shared@zoikosuite.com>"
		seedLedgerAttempt(t, s, "np-27-a", "dedup-np-27-a", id)
		seedLedgerAttempt(t, s, "np-27-b", "dedup-np-27-b", id)
		_, err := s.LookupAttemptByProviderMessageID(ctx, id)
		assert.ErrorIs(t, err, webhook.ErrAmbiguousAttempt)
	})

	t.Run("NP-35 and NP-36 provider accepted is not mailbox accepted", func(t *testing.T) {
		n := sentDirect(t, s, "np-35", "corr-np-35", "<np-35@example.com>")
		got, err := s.GetNotification(tenantCtx("np-35"), n.NotificationID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusSent, got.Status)
		facts, err := s.ListDeliveryEvidence(tenantCtx("np-35"), n.NotificationID)
		require.NoError(t, err)
		assert.Empty(t, facts, "SENT carries no delivery fact; MAILBOX_ACCEPTED must come from the receiving server")
	})

	t.Run("NP-37 and NP-38 open tracking is never evidence", func(t *testing.T) {
		for _, ev := range []string{"OPENED", "OPEN", "CLICK", "CLICKED"} {
			_, _, ok := domain.NormalizeEvidence(ev, "")
			assert.False(t, ok, ev)
		}
	})

	t.Run("NP-45 complaint, then a provider switch, keeps the suppression", func(t *testing.T) {
		provider := "ses"
		require.NoError(t, s.AddSuppression(tenantCtx("np-45"), &ledger.EmailSuppression{SuppressionID: uuid.NewString(), TenantID: "np-45",
			RecipientEmail: "complainer@example.com", Reason: ledger.SuppressionReasonComplaint, SourceStream: "ALL", ProviderName: &provider, CreatedAt: time.Now()}))
		// The question carries no provider, so changing provider cannot forget the complaint.
		suppressed, _, err := s.IsEmailSuppressed(tenantCtx("np-45"), "np-45", "complainer@example.com", ledger.StreamTransactional, ledger.ClassT0)
		require.NoError(t, err)
		assert.True(t, suppressed)
	})

	// ── NCD-05 regulated notices ───────────────────────────────────────────────────────
	t.Run("NP-39 click without authentication is not an acknowledgement", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-39", "legal.np39")
		n, err := s.CreateNotice(tenantCtx("np-39"), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
		require.NoError(t, err)
		_, attemptID := deliverNotice(t, s, "np-39", n, "<np-39@example.com>")
		mailboxAccepted(t, s, "np-39", attemptID, domain.EvidenceMailboxAccepted)
		// The only way to respond is an authenticated action by the recipient: a stranger fails,
		// and no action name for an email click or open exists.
		_, _, err = s.RecordNoticeAck(tenantCtx("np-39"), n.NoticeID, "anonymous", domain.ActionAcknowledge, "", time.Now())
		assert.ErrorIs(t, err, domain.ErrNotNoticeRecipient)
		_, _, err = s.RecordNoticeAck(tenantCtx("np-39"), n.NoticeID, noticeRecipient, "LINK_CLICKED", "", time.Now())
		assert.ErrorIs(t, err, domain.ErrNoticeInvalid)
	})

	t.Run("NP-40 acknowledging a superseded version does not satisfy the new one", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-40", "legal.np40")
		v1, err := s.CreateNotice(tenantCtx("np-40"), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
		require.NoError(t, err)
		_, attemptID := deliverNotice(t, s, "np-40", v1, "<np-40@example.com>")
		mailboxAccepted(t, s, "np-40", attemptID, domain.EvidenceMailboxAccepted)
		_, err = s.RefreshNotice(tenantCtx("np-40"), v1.NoticeID, time.Now())
		require.NoError(t, err)

		fix := noticeParams(iv, domain.AckReceipt, time.Now().Add(2*time.Hour))
		fix.Body, fix.SupersedesNoticeID, fix.CorrectionReason = "Corrected terms.", v1.NoticeID, "fix"
		v2, err := s.CreateNotice(tenantCtx("np-40"), fix)
		require.NoError(t, err)

		_, _, err = s.RecordNoticeAck(tenantCtx("np-40"), v1.NoticeID, noticeRecipient, domain.ActionAcknowledge, "", time.Now())
		require.NoError(t, err, "the response binds to the exact version it was made on")
		got, err := s.GetNotice(tenantCtx("np-40"), v2.NoticeID)
		require.NoError(t, err)
		assert.Equal(t, domain.NoticeReady, got.Status, "the correction is not acknowledged by an answer to the original")
		ack, err := s.GetNoticeAck(tenantCtx("np-40"), v2.NoticeID)
		require.NoError(t, err)
		assert.Nil(t, ack)
	})

	t.Run("NP-41 operator marks a notice served", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-41", "legal.np41")
		n, err := s.CreateNotice(tenantCtx("np-41"), noticeParams(iv, domain.AckReceipt, time.Now().Add(time.Hour)))
		require.NoError(t, err)
		_, attemptID := deliverNotice(t, s, "np-41", n, "<np-41@example.com>")
		mailboxAccepted(t, s, "np-41", attemptID, domain.EvidenceMailboxAccepted)
		_, _, err = s.RecordNoticeAck(tenantCtx("np-41"), n.NoticeID, "operator-1", domain.ActionAcknowledge, "marked served", time.Now())
		assert.ErrorIs(t, err, domain.ErrNotNoticeRecipient)
	})

	t.Run("NP-42 acknowledgement deadline expires", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-42", "legal.np42")
		deadline := time.Now().Add(time.Hour)
		n, err := s.CreateNotice(tenantCtx("np-42"), noticeParams(iv, domain.AckReceipt, deadline))
		require.NoError(t, err)
		_, attemptID := deliverNotice(t, s, "np-42", n, "<np-42@example.com>")
		mailboxAccepted(t, s, "np-42", attemptID, domain.EvidenceMailboxAccepted)
		_, err = s.RefreshNotice(tenantCtx("np-42"), n.NoticeID, time.Now())
		require.NoError(t, err)
		got, err := s.RefreshNotice(tenantCtx("np-42"), n.NoticeID, deadline.Add(time.Second))
		require.NoError(t, err)
		assert.Equal(t, domain.NoticeExpired, got.Status)
	})

	t.Run("NP-43 correction after the original", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-43", "legal.np43")
		v1, err := s.CreateNotice(tenantCtx("np-43"), noticeParams(iv, domain.AckNone, time.Time{}))
		require.NoError(t, err)
		fix := noticeParams(iv, domain.AckNone, time.Time{})
		fix.Body, fix.SupersedesNoticeID, fix.CorrectionReason = "Corrected.", v1.NoticeID, "typo"
		_, err = s.CreateNotice(tenantCtx("np-43"), fix)
		require.NoError(t, err)
		old, err := s.GetNotice(tenantCtx("np-43"), v1.NoticeID)
		require.NoError(t, err)
		assert.Equal(t, "Your terms change on 1 January.", old.Body, "the original is unchanged")
		assert.NotNil(t, old.SupersededByNoticeID)
	})

	t.Run("NP-44 mistaken recipient is not repaired by a correction", func(t *testing.T) {
		iv := regulatedIntentVersion(t, s, "np-44", "legal.np44")
		v1, err := s.CreateNotice(tenantCtx("np-44"), noticeParams(iv, domain.AckNone, time.Time{}))
		require.NoError(t, err)
		fix := noticeParams(iv, domain.AckNone, time.Time{})
		fix.RecipientPrincipalID, fix.SupersedesNoticeID, fix.CorrectionReason = "someone-else", v1.NoticeID, "wrong person"
		_, err = s.CreateNotice(tenantCtx("np-44"), fix)
		assert.ErrorIs(t, err, domain.ErrNoticeInvalid, "re-addressing is a new notice; the original delivery evidence stays")
		// NOT BUILT: the privacy and security incident assessment this scenario also calls for.
	})

	// ── Scenarios this service cannot yet handle: listed here so the run shows them. ───
	skipNP(t, "NP-23 provider 500 before a known submit", "retries exist but there is no provider idempotency token (NCD-03)")
	skipNP(t, "NP-25 callback before the API response", "ordering rule for an early callback is unverified and untested")
	skipNP(t, "NP-28 primary provider outage", "SMTP secondary failover is covered in package deliver; certified-equivalent routing is not modelled")
	skipNP(t, "NP-29 fallback lowers evidence class", "no governed multi-channel fallback")
	skipNP(t, "NP-30 fallback violates residency", "no residency-aware routing or provider bindings")
	skipNP(t, "NP-31 SMS exposes an S3 payload", "no SMS channel; sensitivity governs subjects only, not other channels or logs")
	skipNP(t, "NP-33 forwarded secure-link token", "action tokens are signed and scanner-safe, but audience binding is unverified")
	skipNP(t, "NP-34 attachment hash differs", "no attachments")
	skipNP(t, "NP-47 purchased list, no consent", "needs an audience-level consent check beyond the per-message privacy gate")
	skipNP(t, "NP-48 promotional block in a transactional template", "the class is explicit and judged, but there is no content check for promotional blocks")
	skipNP(t, "NP-49 and NP-50 bulk across tenants, audience drift preview to send", "no bulk API or audience preview")
	skipNP(t, "NP-55 DMARC or DKIM break", "no sender-authentication monitoring (operational)")
	skipNP(t, "NP-58 DRC declaration fails", "no DRC integration")
	skipNP(t, "NP-59 provider corrects a status later", "later corrections append facts but do not reopen a notice or re-evaluate status")
	skipNP(t, "NP-60 tenant disables unsubscribe", "no such setting exists; there is no explicit guard test")
}

func ptr(s string) *string { return &s }
