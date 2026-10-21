//go:build integration

// Proof that ACC-12's four adjustment lifecycle events reach the transactional
// outbox (ZS-EVENT-001 §6) inside the same transaction as the adjustment
// state change they report, with the adjustment itself — never the run or the
// consolidation-book journal — as the aggregate, and carrying the exact
// pre-standard event name downstream consumers filter on.
//
// Run: go test -v -tags=integration -count=1 -timeout=400s ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

const (
	typeAdjustmentCreated  = "com.zoikosuite.accounting.consolidation-adjustment.created"
	typeAdjustmentApproved = "com.zoikosuite.accounting.consolidation-adjustment.approved"
	typeAdjustmentPosted   = "com.zoikosuite.accounting.consolidation-adjustment.posted"
	typeAdjustmentReversed = "com.zoikosuite.accounting.consolidation-adjustment.reversed"

	legacyAdjustmentCreated  = "ConsolidationAdjustmentCreated"
	legacyAdjustmentApproved = "ConsolidationAdjustmentApproved"
	legacyAdjustmentPosted   = "ConsolidationAdjustmentPosted"
	legacyAdjustmentReversed = "ConsolidationAdjustmentReversed"
)

// adjustmentEventPayload is the part of the rendered envelope these tests
// inspect: the aggregate subject, the legacy event name, the actor/correlation
// context and the fact's data object.
type adjustmentEventPayload struct {
	Subject       string         `json:"subject"`
	LegalEntityID string         `json:"legalentityid"`
	EventType     string         `json:"event_type"`
	ActorID       string         `json:"actorid"`
	CorrelationID string         `json:"correlationid"`
	Data          map[string]any `json:"data"`
}

func decodeAdjustmentPayload(t *testing.T, payload string) adjustmentEventPayload {
	t.Helper()
	var out adjustmentEventPayload
	require.NoError(t, json.Unmarshal([]byte(payload), &out), "payload: %s", payload)
	return out
}

func TestPgStore_AdjustmentLifecycle_EnqueuesFourEvents(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-le-1", "preparer-1")

	journalID := uuid.NewString()
	require.NoError(t, createAdjustment(s, ctx, adj, "corr-create"))
	require.NoError(t, approveAdjustment(s, ctx, adj.ConsolidationAdjustmentID, "approver-1", "corr-approve"))
	require.NoError(t, postAdjustment(s, ctx, adj.ConsolidationAdjustmentID, "poster-1", journalID, "corr-post"))
	require.NoError(t, reverseAdjustment(s, ctx, adj.ConsolidationAdjustmentID, "reverser-1", "correction", nil, "corr-reverse"))

	rows := outboxRowsFor(t, pool, tenantID, adj.ConsolidationAdjustmentID)
	require.Len(t, rows, 4, "one event per lifecycle command, all keyed on the adjustment's own id")
	assert.Equal(t,
		[]string{typeAdjustmentCreated, typeAdjustmentApproved, typeAdjustmentPosted, typeAdjustmentReversed},
		[]string{rows[0].eventType, rows[1].eventType, rows[2].eventType, rows[3].eventType})

	wantLegacy := []string{
		legacyAdjustmentCreated,
		legacyAdjustmentApproved,
		legacyAdjustmentPosted,
		legacyAdjustmentReversed,
	}
	wantActor := []string{"preparer-1", "approver-1", "poster-1", "reverser-1"}
	wantCorrelation := []string{"corr-create", "corr-approve", "corr-post", "corr-reverse"}
	wantSubject := "urn:zoikosuite:consolidation-adjustment:" + adj.ConsolidationAdjustmentID

	for i, row := range rows {
		p := decodeAdjustmentPayload(t, row.payload)
		assert.Equal(t, wantLegacy[i], p.EventType)
		assert.Equal(t, wantSubject, p.Subject, "the adjustment, not the run or journal, is the aggregate")
		assert.Equal(t, "group-le-1", p.LegalEntityID)
		assert.Equal(t, wantActor[i], p.ActorID)
		assert.Equal(t, wantCorrelation[i], p.CorrelationID)
		assert.Equal(t, adj.ConsolidationAdjustmentID, p.Data["consolidation_adjustment_id"])
		assert.Equal(t, tenantID, p.Data["tenant_id"])
		assert.Equal(t, "group-le-1", p.Data["group_legal_entity_id"])
		assert.Equal(t, "2026-07", p.Data["fiscal_period"])
	}

	assert.Equal(t, "PENDING_APPROVAL", decodeAdjustmentPayload(t, rows[0].payload).Data["status"])
	assert.Equal(t, "MANUAL", decodeAdjustmentPayload(t, rows[0].payload).Data["adjustment_type"])
	assert.Equal(t, "APPROVED", decodeAdjustmentPayload(t, rows[1].payload).Data["status"])
	assert.Equal(t, "POSTED", decodeAdjustmentPayload(t, rows[2].payload).Data["status"])
	assert.Equal(t, journalID, decodeAdjustmentPayload(t, rows[2].payload).Data["consolidation_book_journal_id"])
	assert.Equal(t, "REVERSED", decodeAdjustmentPayload(t, rows[3].payload).Data["status"])
	assert.Equal(t, "correction", decodeAdjustmentPayload(t, rows[3].payload).Data["reversal_reason"])
}

func TestPgStore_AdjustmentTransitionFromWrongStatus_EnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-le-1", "preparer-1")

	require.NoError(t, createAdjustment(s, ctx, adj, "corr-create"))
	require.NoError(t, approveAdjustment(s, ctx, adj.ConsolidationAdjustmentID, "approver-1", "corr-approve"))

	before := outboxRowsFor(t, pool, tenantID, adj.ConsolidationAdjustmentID)
	require.Len(t, before, 2)

	// A second approval cannot move APPROVED -> APPROVED; the guarded UPDATE
	// affects no row, so no third event may be left behind.
	require.Error(t, approveAdjustment(s, ctx, adj.ConsolidationAdjustmentID, "approver-2", "corr-again"))

	after := outboxRowsFor(t, pool, tenantID, adj.ConsolidationAdjustmentID)
	require.Len(t, after, 2)
}

func TestPgStore_CreateAdjustment_InsertFailure_EnqueuesNothing(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-le-1", "preparer-1")

	require.NoError(t, createAdjustment(s, ctx, adj, "corr-create"))
	require.Error(t, createAdjustment(s, ctx, adj, "corr-duplicate"), "same primary key must fail")

	rows := outboxRowsFor(t, pool, tenantID, adj.ConsolidationAdjustmentID)
	require.Len(t, rows, 1, "a failed CreateAdjustment must not leave a second created event behind")
	assert.Equal(t, typeAdjustmentCreated, rows[0].eventType)
}
