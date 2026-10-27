package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/retention-registry-svc/internal/domain"
	"zoiko.io/retention-registry-svc/internal/outbox"
	"zoiko.io/retention-registry-svc/internal/store"
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
	policyID := uuid.NewString()
	policy := &domain.RetentionPolicy{
		RetentionPolicyID:    policyID,
		RecordClass:          "FINANCIAL_LEDGER",
		LegalRegulatoryBasis: "SEC Rule 17a-4",
		EffectiveFrom:        time.Now().UTC(),
		MinRetentionDays:     2555,
		PolicyStatus:         "ACTIVE",
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: "records-officer-1",
	}

	outboxEvt := outbox.Event{
		AggregateType: "RetentionPolicy",
		AggregateID:   policyID,
		EventType:     "retention_policy.created",
		Payload:       policy,
		CorrelationID: "test-corr-retention-atomicity",
	}

	// 1. Atomically insert retention policy + outbox event
	if err := s.CreateRetentionPolicy(ctx, policy, outboxEvt); err != nil {
		t.Fatalf("CreateRetentionPolicy failed: %v", err)
	}

	// Verify policy was persisted
	found, err := s.FindApplicableRetentionPolicy(ctx, "FINANCIAL_LEDGER", nil, nil)
	if err != nil {
		t.Fatalf("FindApplicableRetentionPolicy failed: %v", err)
	}
	if found == nil || found.RetentionPolicyID != policyID {
		t.Fatalf("expected policy %s to be persisted", policyID)
	}

	// Verify outbox event row exists and is unpublished
	var outboxCount int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", policyID).Scan(&outboxCount); err != nil {
		t.Fatalf("query outbox count: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected 1 unpublished outbox event, got %d", outboxCount)
	}

	// 2. Test relay with simulated failure
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
	if err := appPool.QueryRow(ctx, "SELECT publish_attempts, last_error FROM outbox_events WHERE aggregate_id = $1", policyID).Scan(&attempts, &lastErr); err != nil {
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

	// Verify published_at is set
	var remainingUnpublished int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND published_at IS NULL", policyID).Scan(&remainingUnpublished); err != nil {
		t.Fatalf("query remaining unpublished: %v", err)
	}
	if remainingUnpublished != 0 {
		t.Fatalf("expected 0 unpublished events remaining, got %d", remainingUnpublished)
	}
}
