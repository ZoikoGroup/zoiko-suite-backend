// Package events_test asserts the event envelope actually carries the fields
// Doc 03 §19 requires, and that the build/publish split holds.
//
// domain.Notification carries real TenantID/LegalEntityID and a
// CreatedByPrincipalID actor — the principal who initiated the send, distinct
// from RecipientPrincipalID (who it goes to). No jurisdiction field exists on
// the domain object.
//
// The tests are split the way the package is. Sent/Failed SEAL an envelope and
// are what the store calls inside the delivery transaction; Publish carries
// already-sealed envelopes to Kafka and is what the relay calls. Before the
// outbox existed these were one act — a post-commit Kafka write whose error was
// logged and discarded — so there was nothing to assert in between, and a
// broker outage during a conclusion lost the event with no test able to see it.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
)

type fakeWriter struct {
	msgs  []kafka.Message
	calls int
	err   error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	EmittedAt     time.Time       `json:"emitted_at"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func decodeBody(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env
}

func payloadOf(t *testing.T, env envelope) map[string]any {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &p))
	return p
}

// ── sealing ──────────────────────────────────────────────────────────────────

func TestSent_ActorIsSender_NotRecipient(t *testing.T) {
	sentAt := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	out, err := events.Sent("corr-1", domain.Notification{
		NotificationID: "notif-1", TenantID: "tenant-1", LegalEntityID: "entity-1",
		RecipientPrincipalID: "recipient-1", CreatedByPrincipalID: "sender-1",
		Channel: "EMAIL", SentAt: &sentAt, DeliveryAttempts: 2,
		ProviderResponse: "250 2.0.0 Ok: queued as ABC",
	})
	require.NoError(t, err)

	assert.Equal(t, events.TypeSent, out.EventType)
	// The partition key is the AGGREGATE — the notification — not the
	// correlation id. Keying on the correlation id would put two events about
	// the same notification on different partitions if a second ever existed.
	assert.Equal(t, "notif-1", out.Key)

	env := decodeBody(t, out.Body)
	assert.Equal(t, "notification.sent", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "notification-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "sender-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
	assert.False(t, env.EmittedAt.IsZero())
}

// The content of a notice is the most sensitive thing this service holds, and
// the topic is readable by every consumer on the bus. A consumer that needs the
// subject or the body reads the register under its own authorization.
func TestSent_PayloadCarriesNoMessageContent(t *testing.T) {
	out, err := events.Sent("corr-1", domain.Notification{
		NotificationID: "notif-1", TenantID: "tenant-1", Channel: "EMAIL",
		Subject:          "Your payslip for August",
		Body:             "Net pay 2,431.09 to account ending 4417.",
		RecipientAddress: "someone@example.com",
	})
	require.NoError(t, err)

	p := payloadOf(t, decodeBody(t, out.Body))
	for _, forbidden := range []string{"subject", "body", "recipient_address"} {
		_, present := p[forbidden]
		assert.Falsef(t, present, "payload must not carry %q", forbidden)
	}
	assert.NotContains(t, string(out.Body), "2,431.09")
	assert.NotContains(t, string(out.Body), "someone@example.com")
}

// failed_at comes off the row, not off the clock. time.Now() at seal time
// drifts from sent_at by however long the transaction takes, so a consumer
// correlating the event against the register would find two different answers
// to when the notice failed.
func TestFailed_FailedAtIsTheRecordedConclusion(t *testing.T) {
	concluded := time.Date(2026, 9, 22, 11, 30, 0, 0, time.UTC)
	out, err := events.Failed("corr-x", domain.Notification{
		NotificationID: "notif-1", TenantID: "tenant-1", Channel: "EMAIL",
		SentAt: &concluded, DeliveryAttempts: 5,
	}, "550 no such mailbox")
	require.NoError(t, err)

	p := payloadOf(t, decodeBody(t, out.Body))
	assert.Equal(t, "550 no such mailbox", p["failure_reason"])
	assert.Equal(t, float64(5), p["delivery_attempts"])
	assert.Equal(t, concluded.Format(time.RFC3339Nano), p["failed_at"])
}

// The reason is passed separately rather than read off the row because the
// worker appends the exhaustion note to it, and the event must carry what was
// actually recorded.
func TestFailed_ReasonIsTheArgument_NotTheStruct(t *testing.T) {
	out, err := events.Failed("corr-x", domain.Notification{
		NotificationID: "notif-1", FailureReason: "stale value from the struct",
	}, "smtp timeout (no further attempts: exhausted after 5 of 5)")
	require.NoError(t, err)

	p := payloadOf(t, decodeBody(t, out.Body))
	assert.Equal(t, "smtp timeout (no further attempts: exhausted after 5 of 5)", p["failure_reason"])
}

func TestSeal_RepeatEventsOnSameNotification_GetDistinctEventIDs(t *testing.T) {
	first, err := events.Failed("corr-x", domain.Notification{NotificationID: "notif-1"}, "smtp timeout")
	require.NoError(t, err)
	second, err := events.Failed("corr-x", domain.Notification{NotificationID: "notif-1"}, "smtp timeout")
	require.NoError(t, err)

	assert.NotEqual(t, decodeBody(t, first.Body).EventID, decodeBody(t, second.Body).EventID)
}

// ── publishing ───────────────────────────────────────────────────────────────

// One WriteMessages call for the whole batch, not one per record. kafka-go
// waits out BatchTimeout per invocation, so a per-record loop makes a backlog
// of 200 drain in 200 round trips instead of one.
func TestPublish_WritesTheWholeBatchInOneCall(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.notification.events", w)

	msgs := []kafka.Message{
		{Key: []byte("n-1"), Value: []byte(`{"event_type":"notification.sent"}`)},
		{Key: []byte("n-2"), Value: []byte(`{"event_type":"notification.failed"}`)},
		{Key: []byte("n-3"), Value: []byte(`{"event_type":"notification.sent"}`)},
	}
	require.NoError(t, p.Publish(context.Background(), msgs))

	assert.Equal(t, 1, w.calls)
	assert.Len(t, w.msgs, 3)
}

// A broker refusal must be RETURNED, not logged and swallowed. This is the
// whole difference the outbox buys: the relay keeps the rows unpublished and
// tries again, where the old publisher logged and dropped the event forever.
func TestPublish_BrokerFailureIsReturned(t *testing.T) {
	w := &fakeWriter{err: errors.New("broker unreachable")}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.notification.events", w)

	err := p.Publish(context.Background(), []kafka.Message{{Key: []byte("n-1"), Value: []byte("{}")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker unreachable")
	assert.Contains(t, err.Error(), "zoiko.notification.events")
}

func TestPublish_EmptyBatchIsANoOp(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.notification.events", w)
	require.NoError(t, p.Publish(context.Background(), nil))
	assert.Zero(t, w.calls)
}

// KAFKA_BROKERS empty is a deployment saying it has no bus, which is different
// from a bus that is down: the first must succeed so the relay marks the batch
// published, the second must fail so it retries. NewPublisher must keep the
// interface field genuinely nil for that check to work — storing a nil
// *kafka.Writer into it would make the interface itself non-nil.
func TestNewPublisher_NilProducer_IsASuccessfulDryRun(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.notification.events", nil)
	require.NoError(t, p.Publish(context.Background(), []kafka.Message{{Key: []byte("n-1"), Value: []byte("{}")}}))
}

// ── BIZ-10 / BIZ-03 events, which main published directly and now seal for the
// outbox like the two conclusion events ────────────────────────────────────────

func TestOutcomeUnknown_CarriesReasonAndUnknownAt(t *testing.T) {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	out, err := events.OutcomeUnknown("corr-u", domain.Notification{
		NotificationID: "notif-u", TenantID: "tenant-1", LegalEntityID: "entity-1",
		CreatedByPrincipalID: "sender-1", UnknownAt: &at,
	}, "connection reset after DATA")
	require.NoError(t, err)
	assert.Equal(t, events.TypeOutcomeUnknown, out.EventType)
	assert.Equal(t, "notif-u", out.Key, "keyed on the notification, like sent/failed")

	env := decodeBody(t, out.Body)
	assert.Equal(t, "sender-1", env.ActorID)
	p := payloadOf(t, env)
	assert.Equal(t, "connection reset after DATA", p["reason"])
	assert.Equal(t, at.Format(time.RFC3339), p["unknown_at"])
}

// Every event about one template — created, a version approved, published,
// retired — is keyed on the template id, so they share a partition and a
// consumer sees them in the order they happened.
func TestTemplateEvents_KeyedOnTemplateWithTheRightActor(t *testing.T) {
	approver, retirer := "approver-1", "retirer-1"
	def := domain.TemplateDefinition{TemplateID: "tpl-1", TenantID: "t", LegalEntityID: "le", Name: "Payslip", OwnerPrincipalID: "owner-1"}
	ver := domain.TemplateVersion{VersionID: "ver-1", TemplateID: "tpl-1", TenantID: "t", LegalEntityID: "le", Locale: "en-GB", VersionNumber: 2, ApprovedByPrincipalID: &approver, ContentHash: "abc"}

	created, err := events.TemplateCreated("c", def)
	require.NoError(t, err)
	approved, err := events.TemplateVersionApproved("c", ver)
	require.NoError(t, err)
	published, err := events.TemplatePublished("c", "publisher-1", ver)
	require.NoError(t, err)
	def.RetiredByPrincipalID = &retirer
	retired, err := events.TemplateRetired("c", def)
	require.NoError(t, err)

	for _, tc := range []struct {
		out       events.Outbound
		eventType string
		actor     string
	}{
		{created, events.TypeTemplateCreated, "owner-1"},
		{approved, events.TypeTemplateVersionApproved, "approver-1"},
		{published, events.TypeTemplatePublished, "publisher-1"},
		{retired, events.TypeTemplateRetired, "retirer-1"},
	} {
		assert.Equal(t, tc.eventType, tc.out.EventType)
		assert.Equal(t, "tpl-1", tc.out.Key, tc.eventType)
		assert.Equal(t, tc.actor, decodeBody(t, tc.out.Body).ActorID, tc.eventType)
	}
	assert.Equal(t, "ver-1", payloadOf(t, decodeBody(t, published.Body))["version_id"])
}
