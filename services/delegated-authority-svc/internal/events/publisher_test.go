package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/events"
)

type fakeWriter struct {
	batches [][]kafka.Message
	err     error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.batches = append(f.batches, msgs)
	return nil
}

func grant() domain.DelegationGrant {
	revoker := "revoker-9"
	now := time.Now().UTC()
	return domain.DelegationGrant{
		DelegationID:         "del-1",
		TenantID:             "tenant-abc",
		LegalEntityID:        "le-us",
		DelegatorPrincipalID: "delegator-1",
		DelegatePrincipalID:  "delegate-2",
		ActionType:           "PO_ISSUE",
		EffectiveFrom:        now,
		EffectiveTo:          now.Add(24 * time.Hour),
		Status:               domain.DelegationStatusActive,
		CreatedByPrincipalID: "creator-7",
		CorrelationID:        "corr-1",
		RevokedByPrincipalID: &revoker,
	}
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
	return env
}

// Doc 03 §19 lists the fields every published event must carry. Asserted as a
// set rather than one at a time: a missing field is only discovered by the
// consumer that needed it, long after the event was emitted.
func TestBuild_EnvelopeCarriesTheContractFields(t *testing.T) {
	_, body, err := events.Build(events.EventDelegated, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	env := decode(t, body)
	for _, f := range []string{
		"event_id", "event_type", "event_version", "emitted_at",
		"schema_version", "source_service", "tenant_id", "legal_entity_id",
		"actor_id", "correlation_id", "payload",
	} {
		if _, ok := env[f]; !ok {
			t.Errorf("envelope is missing the contract field %q", f)
		}
	}
	if env["source_service"] != "delegated-authority-svc" {
		t.Errorf("source_service = %v", env["source_service"])
	}
	if env["tenant_id"] != "tenant-abc" {
		t.Errorf("tenant_id = %v", env["tenant_id"])
	}
}

func TestBuild_DelegatedActorIsTheCreator(t *testing.T) {
	_, body, err := events.Build(events.EventDelegated, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := decode(t, body)["actor_id"]; got != "creator-7" {
		t.Errorf("actor_id = %v, want the principal who created the grant", got)
	}
}

func TestBuild_RevokedActorIsTheRevoker(t *testing.T) {
	_, body, err := events.Build(events.EventRevoked, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := decode(t, body)["actor_id"]; got != "revoker-9" {
		t.Errorf("actor_id = %v, want the principal who revoked", got)
	}
}

// Nobody expires a delegation — its window closes. Naming the principal whose
// read happened to observe the lapse would record an act that was not
// performed, on a register that exists to say who did what.
func TestBuild_ExpiredHasNoActor(t *testing.T) {
	_, body, err := events.Build(events.EventExpired, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got, present := decode(t, body)["actor_id"]; present {
		t.Errorf("actor_id = %v; expiry is not performed by anyone and must omit it", got)
	}
}

// The consumer of authority.revoked has to know WHOSE authority ended and
// which action it covered. Reading that back used to mean a lookup against
// this service, which defeats the point of an event.
func TestBuild_RevokedPayloadNamesBothPartiesAndTheAction(t *testing.T) {
	_, body, err := events.Build(events.EventRevoked, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var env struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, want := range map[string]string{
		"delegation_id":          "del-1",
		"delegator_principal_id": "delegator-1",
		"delegate_principal_id":  "delegate-2",
		"action_type":            "PO_ISSUE",
	} {
		if got := env.Payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %q", k, got, want)
		}
	}
}

// Keyed by delegation so every event about one grant lands on the same
// partition. Keyed by tenant they could be reordered across partitions and a
// consumer could act on a revocation before it knows the grant exists.
func TestBuild_KeyIsTheDelegationNotTheTenant(t *testing.T) {
	key, _, err := events.Build(events.EventRevoked, grant())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if key != "del-1" {
		t.Errorf("key = %q, want the delegation id", key)
	}
}

func TestBuild_RepeatEventsGetDistinctEventIDs(t *testing.T) {
	_, a, _ := events.Build(events.EventDelegated, grant())
	_, b, _ := events.Build(events.EventDelegated, grant())
	if decode(t, a)["event_id"] == decode(t, b)["event_id"] {
		t.Error("two events on the same delegation share an event_id; a deduplicating consumer would drop the second")
	}
}

func TestBuild_UnknownEventTypeIsRefused(t *testing.T) {
	if _, _, err := events.Build("authority.invented", grant()); err == nil {
		t.Fatal("expected an error for an event type outside the contract")
	}
}

// One WriteMessages call for the whole batch, not one per record. kafka-go
// does not flush a batch of one until BatchTimeout elapses, so a per-record
// loop turns a drain of 200 into 200 sequential timer waits — identity-
// context-svc's outbox ran at 1.03 events/second for exactly that reason.
func TestPublish_SendsTheWholeBatchInOneCall(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "t", w)
	msgs := make([]kafka.Message, 50)
	if err := p.Publish(context.Background(), msgs); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(w.batches) != 1 {
		t.Fatalf("WriteMessages called %d times, want 1", len(w.batches))
	}
	if len(w.batches[0]) != 50 {
		t.Errorf("batch carried %d messages, want 50", len(w.batches[0]))
	}
}

// The relay decides whether to mark rows published from this error, so
// swallowing it here would lose every event in the batch while recording them
// as delivered — the exact failure the outbox was built to prevent.
func TestPublish_ReturnsTheWriteError(t *testing.T) {
	boom := errors.New("broker unreachable")
	p := events.NewPublisherWithWriter(zap.NewNop(), "t", &fakeWriter{err: boom})
	err := p.Publish(context.Background(), []kafka.Message{{}})
	if err == nil {
		t.Fatal("publish returned nil on a failed write")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the cause: %v", err)
	}
}

func TestPublish_EmptyBatchIsANoOp(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "t", w)
	if err := p.Publish(context.Background(), nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(w.batches) != 0 {
		t.Errorf("empty batch still called the writer %d time(s)", len(w.batches))
	}
}

// A nil *kafka.Writer stored straight into the interface field would make the
// interface non-nil and panic on the first publish.
func TestNewPublisher_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "t", nil)
	if err := p.Publish(context.Background(), []kafka.Message{{}}); err != nil {
		t.Fatalf("dry-run publish returned %v", err)
	}
}
