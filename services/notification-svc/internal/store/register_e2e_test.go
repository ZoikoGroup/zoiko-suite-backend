package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/webhook"
)

func registerOrchestrator(t *testing.T, s *store.PgStore, del ledger.Deliverer) *ledger.Orchestrator {
	t.Helper()
	compiler := ledger.NewCompiler()
	for _, seed := range ledger.DefaultSeedDefinitions() {
		require.NoError(t, compiler.Register(seed))
	}
	return ledger.NewOrchestrator(s, compiler, ledger.NewKillSwitchManager(zap.NewNop()), del,
		&e2eResolver{email: "reg-user@example.com"}, zap.NewNop()).WithRegister(s)
}

func registerRequest(event string) ledger.EventIngestRequest {
	return ledger.EventIngestRequest{
		EventID: event, EventType: "identity.password_reset_requested", RecipientPrincipalID: "usr-reg",
		LegalEntityID: "entity-reg", TemplateKey: "ZS-IA-001", CorrelationID: "corr-" + event,
		Variables: map[string]string{
			"recipient.first_name": "Alice", "recipient.email_masked": "a***@example.com",
			"links.action_url": "https://auth.zoiko.com/verify?token=xyz", "security.link_expires_at_local": "15 minutes",
			"message.reference": "REF-" + event,
		},
	}
}

// Plan step 3 end to end against PostgreSQL: one ledger delivery leaves ONE
// communication, visible and linked from both sides, and everything the direct path
// guarantees about a register row holds for it.
func TestRegister_E2E_OneCommunicationOneIdentity(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-reg-e2e"
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	del := &e2eDeliverer{delivered: true, response: "smtp mail.example.com:587 accepted; message-id=<reg-e2e-1@example.com>"}
	orc := registerOrchestrator(t, s, del)

	res, err := orc.IngestEvent(ctx, registerRequest("evt-reg-e2e-1"), "caller-reg")
	require.NoError(t, err)
	require.Equal(t, ledger.IntentStatusDispatched, res.Status)

	// The same communication from both ends.
	notifID, err := s.NotificationIDForIntent(ctx, res.MessageIntentID)
	require.NoError(t, err)
	require.NotEmpty(t, notifID, "the intent must be linked to a register row")
	intentID, err := s.IntentIDForNotification(ctx, notifID)
	require.NoError(t, err)
	assert.Equal(t, res.MessageIntentID, intentID)
	assert.Equal(t, notifID, del.lastSent.NotificationID, "the transport was handed the register id")

	// The register row is concluded, evidenced and addressed with provenance.
	n, err := s.GetNotification(ctx, notifID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusSent, n.Status)
	assert.Equal(t, "reg-user@example.com", n.RecipientAddress)
	assert.Equal(t, domain.AddressSourceIdentityContext, n.RecipientAddressSource)
	assert.Nil(t, n.NextAttemptAt, "the ledger pipeline has no retry: nothing may be scheduled")
	attempts, err := s.ListAttempts(ctx, notifID)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, domain.AttemptOutcomeAccepted, attempts[0].Outcome)

	// Ledger evidence still exists, and a provider callback still resolves to it and
	// still suppresses.
	look, err := s.LookupAttemptByProviderMessageID(context.Background(), "<reg-e2e-1@example.com>")
	require.NoError(t, err)
	assert.Equal(t, res.MessageIntentID, look.MessageIntentID, "the callback resolves to the ledger attempt")
	payload := `{"event_id":"` + uuid.NewString() + `","event_type":"BOUNCE","bounce_type":"HARD","recipient_email":"reg-user@example.com",
		"provider_message_id":"<reg-e2e-1@example.com>","diagnostic_code":"550 5.1.1 User unknown"}`
	require.NoError(t, webhook.NewProcessor(s, zap.NewNop()).ProcessRawPayload(context.Background(), "generic", []byte(payload)))
	suppressed, _, err := s.IsEmailSuppressed(ctx, tenant, "reg-user@example.com", ledger.StreamTransactional, ledger.ClassT0)
	require.NoError(t, err)
	assert.True(t, suppressed)

	// It cannot be resent through the direct path (wrong sender identity).
	_, err = s.BeginResend(ctx, notifID, tenant, "agent-1", "customer asked", time.Now().UTC())
	assert.True(t, errors.Is(err, domain.ErrResendLedgerOwned), "got %v", err)

	// A replayed event neither delivers nor registers again.
	res2, err := orc.IngestEvent(ctx, registerRequest("evt-reg-e2e-1"), "caller-reg")
	require.NoError(t, err)
	assert.True(t, res2.IsReplay)
	assert.Equal(t, 1, del.sendCalls)
	var rows int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM notifications WHERE tenant_id=$1 AND idempotency_key LIKE 'ledger:%'`, tenant).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestRegister_E2E_FailedDeliveryIsConcludedAndNeverRetriedByTheDirectWorker(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-reg-fail"
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	del := &e2eDeliverer{delivered: false} // a retryable failure
	res, err := registerOrchestrator(t, s, del).IngestEvent(ctx, registerRequest("evt-reg-fail-1"), "caller-reg")
	require.NoError(t, err)
	assert.Equal(t, ledger.IntentStatusFailed, res.Status)

	notifID, err := s.NotificationIDForIntent(ctx, res.MessageIntentID)
	require.NoError(t, err)
	n, err := s.GetNotification(ctx, notifID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusFailed, n.Status)
	assert.Contains(t, n.FailureReason, "simulated deliverer failure")

	due, err := s.FindDueRetries(context.Background(), time.Now().Add(24*time.Hour), 100)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, notifID, d.NotificationID, "the direct retry worker must not pick up a ledger-owned row")
	}
}

// outboxFor reads every event (attempt events included) whose aggregate is the given
// communication, decoded.
func outboxFor(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, aggregate string) map[string]map[string]any {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)")
	require.NoError(t, err)
	rows, err := tx.Query(ctx, `SELECT event_type, payload FROM event_outbox WHERE aggregate_key = $1 ORDER BY outbox_id`, aggregate)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var typ string
		var raw []byte
		require.NoError(t, rows.Scan(&typ, &raw))
		var env struct {
			Payload map[string]any `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		if env.Payload == nil { // some deployments store the payload column as the bare payload
			require.NoError(t, json.Unmarshal(raw, &env.Payload))
		}
		out[typ] = env.Payload
	}
	require.NoError(t, rows.Err())
	return out
}

// Step 6 end to end: a ledger delivery's events all name the SAME communication and
// the intent it came from, so a consumer sees one identity.
func TestRegister_E2E_EventsCarryOneCommunicationIdentity(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	tenant := "tenant-events-identity"
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	del := &e2eDeliverer{delivered: true, response: "smtp h accepted; message-id=<events-identity@example.com>"}
	res, err := registerOrchestrator(t, s, del).IngestEvent(ctx, registerRequest("evt-events-identity"), "caller")
	require.NoError(t, err)
	notifID, err := s.NotificationIDForIntent(ctx, res.MessageIntentID)
	require.NoError(t, err)

	evs := outboxFor(t, pool, notifID)
	require.Contains(t, evs, "notification.sent")
	require.Contains(t, evs, "delivery.attempt.created")
	for typ, p := range evs {
		assert.Equal(t, notifID, p["communication_id"], typ)
		assert.Equal(t, notifID, p["notification_id"], "%s: the earlier field is still there", typ)
		assert.Equal(t, res.MessageIntentID, p["message_intent_id"], "%s: names the ledger intent it came from", typ)
		assert.NotEmpty(t, p["communication_class"], typ)
	}
}

// A direct send has no intent: its events say so by omitting the field.
func TestRegister_E2E_DirectSendEventsOmitTheIntent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	n := classed(t, s, "tenant-events-direct", "corr-events-direct", "T0")
	require.NoError(t, complete(t, s, "tenant-events-direct", n.NotificationID, "SENT", "", receiptFor("<events-direct@example.com>")))
	evs := outboxFor(t, pool, n.NotificationID)
	require.Contains(t, evs, "notification.sent")
	for typ, p := range evs {
		assert.Equal(t, n.NotificationID, p["communication_id"], typ)
		assert.NotContains(t, p, "message_intent_id", typ)
		assert.Equal(t, "T0", p["communication_class"], typ)
	}
}
