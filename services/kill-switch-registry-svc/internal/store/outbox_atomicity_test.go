package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/kill-switch-registry-svc/internal/domain"
	"zoiko.io/kill-switch-registry-svc/internal/events"
	"zoiko.io/kill-switch-registry-svc/internal/outbox"
	"zoiko.io/kill-switch-registry-svc/internal/store"
)

type mockOutboxPublisher struct {
	published []string
	fail      bool
}

func (m *mockOutboxPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	if m.fail {
		return errors.New("simulated publish failure")
	}
	m.published = append(m.published, outboxEventID)
	return nil
}

func TestOutbox_AtomicityAndRelay(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool)

	ctx := context.Background()
	eventID := uuid.NewString()
	killEvent := &domain.KillSwitchEvent{
		KillSwitchEventID:          eventID,
		Domain:                     strp("AUTOMATION_ACTION"),
		Action:                     domain.KillSwitchActionEngage,
		Reason:                     "testing outbox atomicity",
		ReconciliationProcedureRef: strp("runbook:atomicity"),
		ApprovedByPrincipalID:      "lead-approver-1",
		CreatedAt:                  time.Now().UTC(),
		CreatedByPrincipalID:       "incident-commander-1",
	}

	outboxEvt := outbox.Event{
		AggregateType: "KillSwitch",
		AggregateID:   eventID,
		EventType:     events.EventTypeKillSwitchEngaged,
		Payload:       killEvent,
		CorrelationID: "test-corr-atomicity",
	}

	// 1. Atomically insert kill switch event + outbox event
	if err := s.AppendEvent(ctx, killEvent, outboxEvt); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}

	// Verify kill switch event was persisted
	latest, err := s.LatestEventForScope(ctx, nil, strp("AUTOMATION_ACTION"), nil, nil)
	if err != nil {
		t.Fatalf("LatestEventForScope failed: %v", err)
	}
	if latest == nil || latest.KillSwitchEventID != eventID {
		t.Fatalf("expected event %s to be persisted", eventID)
	}

	// Verify outbox event row exists and is unpublished
	var outboxCount int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", eventID).Scan(&outboxCount); err != nil {
		t.Fatalf("query outbox count: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected 1 unpublished outbox event, got %d", outboxCount)
	}

	// 2. Test relay with failure
	mockPub := &mockOutboxPublisher{fail: true}
	logger, _ := zap.NewDevelopment()
	relay := outbox.NewRelay(appPool, mockPub, time.Millisecond*100, 10, logger)

	count, err := relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("RelayOnce error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected RelayOnce to process 1 event, got %d", count)
	}

	// Verify attempts incremented and last_error recorded
	var attempts int
	var lastErr *string
	if err := appPool.QueryRow(ctx, "SELECT publish_attempts, last_error FROM outbox_events WHERE aggregate_id = $1", eventID).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("query outbox retry state: %v", err)
	}
	if attempts != 1 || lastErr == nil || *lastErr != "simulated publish failure" {
		t.Fatalf("expected attempts=1 and last_error='simulated publish failure', got attempts=%d, err=%v", attempts, lastErr)
	}

	// 3. Test relay success
	mockPub.fail = false
	count, err = relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("RelayOnce success attempt error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected RelayOnce to reprocess and succeed, got count %d", count)
	}
	if len(mockPub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(mockPub.published))
	}

	// Verify published_at is set now
	var remainingUnpublished int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", eventID).Scan(&remainingUnpublished); err != nil {
		t.Fatalf("query remaining unpublished: %v", err)
	}
	if remainingUnpublished != 0 {
		t.Fatalf("expected 0 unpublished events remaining, got %d", remainingUnpublished)
	}

	// 4. Next relay run finds 0 events
	count, err = relay.RelayOnce(ctx)
	if err != nil {
		t.Fatalf("subsequent RelayOnce error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 events in subsequent run, got %d", count)
	}
}
