package store_test

// SPEC_DEVIATIONS A7 against a real Postgres: a successful match emits
// "matched" with the pre-standard "posted" name kept as its legacy event_type
// for one release; dispute and resolve each emit their own event, with the
// resolution outcome in the payload; acknowledge emits nothing because the
// governing spec names no acknowledgement event. Skips (not fails) if
// TEST_DATABASE_URL isn't set.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

const (
	a7CreatedType         = "com.zoikosuite.accounting.intercompany-entry.created"
	a7MatchedType         = "com.zoikosuite.accounting.intercompany-entry.matched"
	a7MismatchType        = "com.zoikosuite.accounting.intercompany-entry.mismatch-detected"
	a7DisputeRaisedType   = "com.zoikosuite.accounting.intercompany-entry.dispute-raised"
	a7DisputeResolvedType = "com.zoikosuite.accounting.intercompany-entry.dispute-resolved"

	a7LegacyPosted          = "intercompany.entry.posted"
	a7LegacyDisputeRaised   = "intercompany.entry.dispute-raised"
	a7LegacyDisputeResolved = "intercompany.entry.dispute-resolved"
)

// a7OutboxEvent is the part of one rendered envelope these tests inspect:
// the canonical type stored in eventing_outbox.event_type, the legacy
// event_type the pre-standard consumers filter on, and the fact's data.
type a7OutboxEvent struct {
	canonicalType string
	legacyType    string
	data          map[string]any
}

func a7Events(t *testing.T, pool *pgxpool.Pool, tenantID, aggregateID string) []a7OutboxEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT event_type, convert_from(payload, 'UTF8')
		FROM eventing_outbox
		WHERE tenant_id = $1 AND aggregate_id = $2
		ORDER BY outbox_id
	`, tenantID, aggregateID)
	if err != nil {
		t.Fatalf("read outbox events: %v", err)
	}
	defer rows.Close()

	out := []a7OutboxEvent{}
	for rows.Next() {
		var canonical, payload string
		if err := rows.Scan(&canonical, &payload); err != nil {
			t.Fatalf("scan outbox event: %v", err)
		}
		var envelope struct {
			LegacyType string         `json:"event_type"`
			Data       map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			t.Fatalf("decode outbox event %s: %v", canonical, err)
		}
		out = append(out, a7OutboxEvent{canonicalType: canonical, legacyType: envelope.LegacyType, data: envelope.Data})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox events: %v", err)
	}
	return out
}

func a7CanonicalTypes(events []a7OutboxEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.canonicalType
	}
	return out
}

func a7String(t *testing.T, data map[string]any, key string) string {
	t.Helper()
	v, ok := data[key].(string)
	if !ok {
		t.Fatalf("event payload %q is not a string: %v", key, data[key])
	}
	return v
}

// A successful match is published as "matched", not "posted". "posted" is
// only the legacy event_type inside the same envelope, so an old consumer
// keeps working and no second, duplicate event is written.
func TestPgStore_A7_MatchEmitsMatchedWithPostedAsLegacyType(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	targetJ := uuid.New().String()
	entry.TargetJournalID, entry.MatchStatus, entry.MismatchReason = &targetJ, domain.MatchStatusMatched, nil
	if _, err := s.UpdateMatch(ctx, entry, testAttempt(entry), "k1", "matcher", "corr-match"); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}

	events := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	if len(events) != 2 {
		t.Fatalf("expected created + matched events, got %d: %+v", len(events), a7CanonicalTypes(events))
	}
	if events[0].canonicalType != a7CreatedType {
		t.Fatalf("first event type = %q, want %q", events[0].canonicalType, a7CreatedType)
	}

	matched := events[1]
	if matched.canonicalType != a7MatchedType {
		t.Fatalf("match event type = %q, want %q", matched.canonicalType, a7MatchedType)
	}
	if matched.legacyType != a7LegacyPosted {
		t.Fatalf("match legacy event_type = %q, want the pre-standard %q", matched.legacyType, a7LegacyPosted)
	}
	if got := a7String(t, matched.data, "match_status"); got != domain.MatchStatusMatched {
		t.Errorf("match event match_status = %q, want %q", got, domain.MatchStatusMatched)
	}
	if got := a7String(t, matched.data, "target_journal_id"); got != targetJ {
		t.Errorf("match event target_journal_id = %q, want %q", got, targetJ)
	}
	for _, e := range events {
		if e.canonicalType == "com.zoikosuite.accounting.intercompany-entry.posted" {
			t.Fatal("a second event was published under the old canonical name; posted must be legacy only")
		}
	}
}

// Dispute and resolution each emit the fact they represent, and the
// resolution event carries the outcome a consumer needs to decide what the
// resolution means (ACCEPTED / CORRECTED / WRITTEN_OFF).
func TestPgStore_A7_DisputeAndResolveEventsCarryResolutionOutcome(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	mismatch(entry, "amount_mismatch: counterparty booked 4000, expected 5000")
	if _, err := s.UpdateMatch(ctx, entry, testAttempt(entry), "k1", "matcher", "corr-match"); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}
	if _, err := s.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "k2", "disputer", "the UK side booked a different rate", "corr-dispute"); err != nil {
		t.Fatalf("DisputeIntercompany: %v", err)
	}
	if _, err := s.ResolveMismatch(ctx, entry.IntercompanyEntryID, "k3", "resolver", "corrected on the UK side", domain.ResolutionCorrected, "corr-resolve"); err != nil {
		t.Fatalf("ResolveMismatch: %v", err)
	}

	events := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	wantTypes := []string{a7CreatedType, a7MismatchType, a7DisputeRaisedType, a7DisputeResolvedType}
	gotTypes := a7CanonicalTypes(events)
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", gotTypes, wantTypes)
	}
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Fatalf("event %d type = %q, want %q (all: %v)", i, gotTypes[i], wantTypes[i], gotTypes)
		}
	}

	dispute := events[2]
	if dispute.legacyType != a7LegacyDisputeRaised {
		t.Errorf("dispute legacy event_type = %q, want %q", dispute.legacyType, a7LegacyDisputeRaised)
	}
	if got := a7String(t, dispute.data, "match_status"); got != domain.MatchStatusDisputed {
		t.Errorf("dispute event match_status = %q, want %q", got, domain.MatchStatusDisputed)
	}
	if got := a7String(t, dispute.data, "dispute_reason"); got != "the UK side booked a different rate" {
		t.Errorf("dispute event dispute_reason = %q", got)
	}

	resolved := events[3]
	if resolved.legacyType != a7LegacyDisputeResolved {
		t.Errorf("resolution legacy event_type = %q, want %q", resolved.legacyType, a7LegacyDisputeResolved)
	}
	if got := a7String(t, resolved.data, "match_status"); got != domain.MatchStatusResolved {
		t.Errorf("resolution event match_status = %q, want %q", got, domain.MatchStatusResolved)
	}
	if got := a7String(t, resolved.data, "resolution_outcome"); got != domain.ResolutionCorrected {
		t.Errorf("resolution event resolution_outcome = %q, want %q", got, domain.ResolutionCorrected)
	}
	if got := a7String(t, resolved.data, "resolution_note"); got != "corrected on the UK side" {
		t.Errorf("resolution event resolution_note = %q", got)
	}
}

// The event is written by the same transaction as the state change: if the
// envelope cannot be built (here, no residency region is configured), the
// transition itself must roll back rather than commit without its event.
func TestPgStore_A7_EventFailureRollsBackTheTransition(t *testing.T) {
	pool := openTestPool(t)
	working := store.New(pool, store.WithEventRegion("test"))
	unconfigured := store.New(pool) // no region: envelope.New refuses every event
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := working.CreateEntry(ctx, entry, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	mismatch(entry, "amount_mismatch: test")
	if _, err := working.UpdateMatch(ctx, entry, testAttempt(entry), "k1", "matcher", "corr-match"); err != nil {
		t.Fatalf("UpdateMatch: %v", err)
	}

	if _, err := unconfigured.DisputeIntercompany(ctx, entry.IntercompanyEntryID, "k2", "disputer", "reason", "corr-dispute"); err == nil {
		t.Fatal("expected the dispute to fail when its event cannot be built")
	}

	got, err := working.GetEntry(ctx, entry.IntercompanyEntryID)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("transition committed without its event: status = %q, want %q", got.MatchStatus, domain.MatchStatusMismatch)
	}
	events := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	if len(events) != 2 {
		t.Fatalf("a failed event must not leave a partial dispute behind; events = %v", a7CanonicalTypes(events))
	}
}

// Acknowledge is a named command but not a named event: the governing spec's
// ACC-11 "Events produced" row (and §23) names no acknowledgement fact, so
// this command must not invent one. A refused transition must not leave an
// event behind either.
func TestPgStore_A7_AcknowledgeEmitsNoEventAndRefusedTransitionEmitsNothing(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	entry := newTestEntry(tenantID, uuid.New().String())
	if _, err := s.CreateEntry(ctx, entry, "creator", "corr-create"); err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	if _, err := s.AcknowledgeCounterparty(ctx, entry.IntercompanyEntryID, "k1", "counterparty", "corr-ack"); err != nil {
		t.Fatalf("AcknowledgeCounterparty: %v", err)
	}

	events := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	if len(events) != 1 || events[0].canonicalType != a7CreatedType {
		t.Fatalf("acknowledge must emit no event; events = %v", a7CanonicalTypes(events))
	}

	if _, err := s.ResolveMismatch(ctx, entry.IntercompanyEntryID, "k2", "resolver", "note", domain.ResolutionAccepted, "corr-resolve"); err == nil {
		t.Fatal("expected ResolveMismatch from AWAITING_COUNTERPARTY to be refused")
	}
	after := a7Events(t, pool, tenantID, entry.IntercompanyEntryID)
	if len(after) != 1 || after[0].canonicalType != a7CreatedType {
		t.Fatalf("a refused transition must leave no event; events = %v", a7CanonicalTypes(after))
	}
}
