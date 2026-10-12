package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
	"zoiko.io/authorization-svc/internal/outbox"
	"zoiko.io/authorization-svc/internal/store"
)

// Runs only with AUTHZ_IT_DSN pointing at a database migrated to 000020.
// A separate variable from TEST_DATABASE_URL on purpose: the other store tests
// wipe whatever that names. These only add rows.

const itTenant = "11111111-1111-4111-8111-111111111111"
const itEntity = "22222222-2222-4222-8222-222222222222"

func outboxPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AUTHZ_IT_DSN")
	if dsn == "" {
		t.Skip("AUTHZ_IT_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pendingFor(t *testing.T, pool *pgxpool.Pool, decisionID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT event_type FROM outbox_events WHERE message_key = $1 AND published_at IS NULL ORDER BY event_type", decisionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		types = append(types, s)
	}
	return types
}

func countDecisions(t *testing.T, pool *pgxpool.Pool, correlationID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM access_decision_log WHERE correlation_id = $1", correlationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The decision and its events commit together.
func TestOutboxIT_DecisionAndEventsCommitTogether(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	d, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID: "p-it", LegalEntityID: itEntity, ActionType: "PAYMENT_RELEASE",
		Outcome: "DENIED", Basis: "sod:conflict_with=PAYMENT_APPROVE", CorrelationID: "it-commit", TenantID: itTenant,
		Events: events.DecisionEvents,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	got := pendingFor(t, pool, d.AccessDecisionID)
	if len(got) != 2 || got[0] != "authorization.denied" || got[1] != "sod.violation.detected" {
		t.Fatalf("outbox rows for the decision = %v, want denied + sod.violation.detected", got)
	}
}

// If the events cannot be written the decision is not recorded either, and the
// handler answers 503 — no decision exists without its events.
func TestOutboxIT_EventFailureRollsBackDecision(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	before := countDecisions(t, pool, "it-rollback")
	_, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID: "p-it", LegalEntityID: itEntity, ActionType: "X",
		Outcome: "DENIED", Basis: "no_grant", CorrelationID: "it-rollback", TenantID: itTenant,
		Events: func(domain.AccessDecisionLog) ([]domain.OutboxMessage, error) { return nil, errors.New("boom") },
	})
	if err == nil {
		t.Fatal("record succeeded although its events could not be written")
	}
	if after := countDecisions(t, pool, "it-rollback"); after != before {
		t.Fatalf("decision row committed without its events (%d -> %d)", before, after)
	}
}

type flakyWriter struct {
	fail bool
	sent []kafka.Message
}

func (f *flakyWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.fail {
		return errors.New("kafka down")
	}
	f.sent = append(f.sent, msgs...)
	return nil
}

// The audit's scenario: Kafka is down when the decision is made. The event
// stays pending through the outage and is published once Kafka is back.
func TestOutboxIT_KafkaOutageDelaysNotLoses(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	d, err := s.RecordAccessDecision(context.Background(), domain.RecordAccessDecisionParams{
		PrincipalID: "p-it", LegalEntityID: itEntity, ActionType: "Y",
		Outcome: "DENIED", Basis: "no_grant", CorrelationID: "it-outage", TenantID: itTenant,
		Events: events.DecisionEvents,
	})
	if err != nil {
		t.Fatal(err)
	}

	w := &flakyWriter{fail: true}
	relay := outbox.NewRelay(pool, w, 0, 1000, zap.NewNop())
	if _, err := relay.RelayOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pendingFor(t, pool, d.AccessDecisionID); len(got) != 1 {
		t.Fatalf("after a failed publish the event is no longer pending: %v", got)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(),
		"SELECT publish_attempts FROM outbox_events WHERE message_key = $1", d.AccessDecisionID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts < 1 {
		t.Errorf("publish_attempts = %d, want the failure counted", attempts)
	}

	w.fail = false
	for i := 0; i < 20 && len(pendingFor(t, pool, d.AccessDecisionID)) > 0; i++ {
		if _, err := relay.RelayOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := pendingFor(t, pool, d.AccessDecisionID); len(got) != 0 {
		t.Fatalf("event still pending after Kafka recovered: %v", got)
	}
	found := false
	for _, m := range w.sent {
		if string(m.Key) == d.AccessDecisionID {
			found = true
		}
	}
	if !found {
		t.Fatal("the decision's event was marked published but never written to Kafka")
	}
}
