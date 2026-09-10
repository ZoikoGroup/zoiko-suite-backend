package store_test

// TestPgStore_LineageConsumer_RedeliveryIsIdempotent is the real
// end-to-end proof behind internal/consumer.Consumer's own reason to
// exist: Kafka's at-least-once delivery means the SAME "accounting event
// emitted" message can reach Handle more than once (a redelivered
// offset, or Runner's own bounded retry after a transient store error).
// This proves that redelivery against a REAL Postgres — not a stub's
// map — produces exactly one lineage_edges row, via the exact
// ON CONFLICT (tenant_id, from_type, from_id, to_type, to_id) DO NOTHING
// path RecordLineageEdge already uses. Skips (not fails) if
// TEST_DATABASE_URL isn't set.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/consumer"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
	"zoiko.io/financial-close-svc/internal/store"
)

func TestPgStore_LineageConsumer_RedeliveryIsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}
	c := consumer.New(s, log)

	tenantID := uuid.New().String()
	// Handle itself injects the tenant into ctx from the envelope's own
	// tenant_id — the background context here stands in for what
	// Runner.Run passes it (no HTTP request, no pre-set tenant).
	baseCtx := context.Background()

	raw := []byte(`{
		"event_type": "project.recognition.accounting_event.emitted",
		"tenant_id": "` + tenantID + `",
		"legal_entity_id": "le-1",
		"payload": {"run_id": "run-redelivery-1", "journal_id": "jnl-redelivery-1"}
	}`)

	// First delivery.
	if err := c.Handle(baseCtx, "evt-1", raw); err != nil {
		t.Fatalf("Handle (first delivery): %v", err)
	}
	// Kafka redelivers the SAME offset — same event_id, same bytes,
	// possibly after Runner's own bounded retry on a transient failure.
	if err := c.Handle(baseCtx, "evt-1", raw); err != nil {
		t.Fatalf("Handle (redelivery): %v", err)
	}

	tenantCtx := svcmiddleware.WithTenant(baseCtx, tenantID)
	edges, err := s.ListLineageEdgesToAsOf(tenantCtx, "journal", "jnl-redelivery-1", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListLineageEdgesToAsOf: %v", err)
	}
	var matching int
	for _, e := range edges {
		if e.FromType == "project_recognition_run" && e.FromID == "run-redelivery-1" {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("expected exactly 1 lineage edge after redelivery, got %d", matching)
	}
}
