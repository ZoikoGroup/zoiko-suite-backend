package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
)

// Identity plan step 6 (ZS-SVC-Y-001 INV-02): every notification.* and
// delivery.attempt.* event names the communication the same way, so a consumer sees
// ONE identity whichever send path produced the message.

func sealAll(t *testing.T, n domain.Notification) map[string]events.Outbound {
	t.Helper()
	at := time.Now().UTC()
	n.SentAt, n.UnknownAt = &at, &at
	a := domain.DeliveryAttempt{AttemptID: "att-1", AttemptNumber: 1, Origin: "request", Channel: n.Channel, Outcome: "ACCEPTED", AttemptedAt: at}
	out := map[string]events.Outbound{}
	var err error
	out["sent"], err = events.Sent("corr", n)
	require.NoError(t, err)
	out["failed"], err = events.Failed("corr", n, "550 no")
	require.NoError(t, err)
	out["unknown"], err = events.OutcomeUnknown("corr", n, "timeout")
	require.NoError(t, err)
	out["attempt.created"], err = events.AttemptCreated("corr", n, a)
	require.NoError(t, err)
	out["attempt.unknown"], err = events.AttemptUnknown("corr", n, a, at.Add(time.Hour))
	require.NoError(t, err)
	return out
}

func notificationFor(intent, class string) domain.Notification {
	return domain.Notification{NotificationID: "n-1", TenantID: "t-1", LegalEntityID: "le-1", RecipientPrincipalID: "p-1",
		Channel: "EMAIL", CreatedByPrincipalID: "sender", MessageIntentID: intent, CommunicationClass: class}
}

func TestEvents_EveryNotificationEventNamesTheCommunication(t *testing.T) {
	for name, ob := range sealAll(t, notificationFor("", "")) {
		p := payloadOf(t, decodeBody(t, ob.Body))
		assert.Equal(t, "n-1", p["communication_id"], name)
		assert.Equal(t, "n-1", ob.Key, "%s: the partition key stays the communication, so one communication stays in order", name)
		// An unlinked, unclassified (direct) send omits the optional fields rather than sending blanks.
		assert.NotContains(t, p, "message_intent_id", name)
		assert.NotContains(t, p, "communication_class", name)
	}
}

func TestEvents_LinkedCommunicationCarriesBothIdentitiesAndTheClass(t *testing.T) {
	for name, ob := range sealAll(t, notificationFor("intent-9", "S0")) {
		p := payloadOf(t, decodeBody(t, ob.Body))
		assert.Equal(t, "n-1", p["communication_id"], name)
		assert.Equal(t, "intent-9", p["message_intent_id"], name)
		assert.Equal(t, "S0", p["communication_class"], name)
	}
}

// Additive only: a consumer written against the earlier shape keeps working.
func TestEvents_EarlierFieldsAreUnchanged(t *testing.T) {
	sealed := sealAll(t, notificationFor("intent-9", "T0"))
	for name, ob := range sealed {
		p := payloadOf(t, decodeBody(t, ob.Body))
		assert.Equal(t, "n-1", p["notification_id"], "%s: notification_id must still be present and equal", name)
	}
	sent := payloadOf(t, decodeBody(t, sealed["sent"].Body))
	for _, k := range []string{"tenant_id", "legal_entity_id", "recipient_principal_id", "channel", "sent_at", "provider_response", "delivery_attempts"} {
		assert.Contains(t, sent, k)
	}
	assert.NotContains(t, sent, "subject")
	assert.NotContains(t, sent, "body")
	assert.NotContains(t, sent, "recipient_address", "the payload still carries no content or address")
	att := payloadOf(t, decodeBody(t, sealed["attempt.created"].Body))
	for _, k := range []string{"attempt_id", "attempt_number", "origin", "outcome", "payload_hash"} {
		assert.Contains(t, att, k)
	}
}
