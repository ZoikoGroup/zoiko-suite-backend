//go:build integration

// Package store_test — proof that payee-banking-identity-svc's ORG-10
// lifecycle events actually reach the transactional outbox (ZS-EVENT-001
// §6), inside the same transaction as the state change they report.
//
// Before this file, event emission for these facts lived in
// internal/events.Publisher, called from the HANDLER layer AFTER the store
// transaction had already committed — a direct, synchronous Kafka write
// that could silently lose an event on a broker outage or a crash between
// the commit and the publish call. Moving the enqueue into the store's own
// transaction is the fix; this file is the real proof it works.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/ -run TestPgStore_Outbox
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

type outboxEvent struct {
	eventType string
}

func outboxEventsFor(t *testing.T, ctx context.Context, f *fixture, tenantID, aggregateID string) []outboxEvent {
	t.Helper()
	rows, err := f.pool.Query(ctx,
		`SELECT event_type FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY created_at`,
		tenantID, aggregateID)
	if err != nil {
		t.Fatalf("query eventing_outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxEvent
	for rows.Next() {
		var e outboxEvent
		if err := rows.Scan(&e.eventType); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}
	return out
}

func legacyEventTypeOf(t *testing.T, ctx context.Context, f *fixture, tenantID, aggregateID, canonicalType string) string {
	t.Helper()
	var payload string
	err := f.pool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3
		 ORDER BY created_at DESC LIMIT 1`,
		tenantID, aggregateID, canonicalType).Scan(&payload)
	if err != nil {
		t.Fatalf("read legacy payload: %v", err)
	}
	return payload
}

// proposeAndVerify walks a fresh fixture through CANDIDATE -> VERIFIED,
// returning the verified destination — the common setup every later-stage
// test in this file needs.
func proposeAndVerify(t *testing.T, f *fixture, tenantID string) *domain.PayeeDestination {
	t.Helper()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	d, err := f.s.ProposeDestination(ctx, tenantID, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: "party-" + uuid.New().String(), FinancialInstitution: "Outbox Test Bank",
		AccountIdentifier: "GB29OTBK" + uuid.New().String()[:14], CountryCode: "GB", Currency: "GBP",
		PayeeName: "Outbox Payee Ltd", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-propose-1")
	if err != nil {
		t.Fatalf("propose destination: %v", err)
	}
	verified, err := f.s.VerifyDestination(ctx, d.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "evidence-1",
	}, "verifier-1")
	if err != nil {
		t.Fatalf("verify destination: %v", err)
	}
	return verified
}

func TestPgStore_Outbox_ProposeDestination_EnqueuesProposedEvent(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	d, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: "party-outbox-1", FinancialInstitution: "Outbox Test Bank",
		AccountIdentifier: "GB29OTBK60161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Outbox Payee Ltd", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-propose-outbox-1")
	if err != nil {
		t.Fatalf("propose destination: %v", err)
	}

	events := outboxEventsFor(t, ctx, f, f.tenant, d.DestinationID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.payee-destination.proposed" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, f, f.tenant, d.DestinationID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"PAYEE_DESTINATION_PROPOSED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

// TestPgStore_Outbox_VerifyDestination_DoesNotEnqueue is a negative
// control: VerifyDestination never published an event in the old
// dual-write (no Publish call existed for it at all — the handler's own
// doc comment calls this path caller-attested, not a decision to omit the
// event). Confirms the fix did not accidentally add one.
func TestPgStore_Outbox_VerifyDestination_DoesNotEnqueue(t *testing.T) {
	f := newFixture(t)
	verified := proposeAndVerify(t, f, f.tenant)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	events := outboxEventsFor(t, ctx, f, f.tenant, verified.DestinationID)
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payee-destination.verified" {
			t.Fatalf("VerifyDestination must not enqueue an event (it never did in the old dual-write), got %+v", events)
		}
	}
	// Only the earlier ProposeDestination event should be present.
	if len(events) != 1 {
		t.Fatalf("expected only the propose event, got %+v", events)
	}
}

func TestPgStore_Outbox_ApproveDestination_EnqueuesApprovedEvent(t *testing.T) {
	f := newFixture(t)
	verified := proposeAndVerify(t, f, f.tenant)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	approved, err := f.s.ApproveDestination(ctx, verified.DestinationID, "approver-1", "corr-approve-1")
	if err != nil {
		t.Fatalf("approve destination: %v", err)
	}

	events := outboxEventsFor(t, ctx, f, f.tenant, approved.DestinationID)
	var sawApproved bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payee-destination.approved" {
			sawApproved = true
		}
	}
	if !sawApproved {
		t.Fatalf("expected an approved row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, f, f.tenant, approved.DestinationID, "com.zoikosuite.banking.payee-destination.approved")
	if !strings.Contains(payload, `"event_type":"PAYEE_DESTINATION_APPROVED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

// TestPgStore_Outbox_ActivateDestination_OnlyEnqueuesTheNewDestinationsEvent
// proves two things: the newly-ACTIVE destination gets its activated event,
// and the OLD destination ActivateDestination auto-supersedes does NOT get
// an outbox event — the old dual-write only ever published the new
// destination's "activated" event, never a "superseded" event for the one
// it replaced (that fact was only ever recorded to the internal audit log,
// recordEvent). This fix must not expand scope by starting to publish it.
func TestPgStore_Outbox_ActivateDestination_OnlyEnqueuesTheNewDestinationsEvent(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-activate-" + uuid.New().String()

	// First destination: propose -> verify -> approve -> activate.
	first, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank One",
		AccountIdentifier: "GB29ONE160161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Payee One", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-first-propose")
	if err != nil {
		t.Fatalf("propose first: %v", err)
	}
	if _, err := f.s.VerifyDestination(ctx, first.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "ev-1",
	}, "verifier-1"); err != nil {
		t.Fatalf("verify first: %v", err)
	}
	if _, err := f.s.ApproveDestination(ctx, first.DestinationID, "approver-1", "corr-first-approve"); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	firstActive, err := f.s.ActivateDestination(ctx, first.DestinationID, "activator-1", "corr-first-activate")
	if err != nil {
		t.Fatalf("activate first: %v", err)
	}
	firstEvents := outboxEventsFor(t, ctx, f, f.tenant, firstActive.DestinationID)
	var sawFirstActivated bool
	for _, e := range firstEvents {
		if e.eventType == "com.zoikosuite.banking.payee-destination.activated" {
			sawFirstActivated = true
		}
	}
	if !sawFirstActivated {
		t.Fatalf("expected the first destination's activated event, got %+v", firstEvents)
	}

	// Second destination for the SAME party/scope: propose -> verify ->
	// approve -> activate. This auto-supersedes the first.
	second, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank Two",
		AccountIdentifier: "GB29TWO160161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Payee Two", SourceType: domain.SourceManualEntry,
	}, "proposer-2", "corr-second-propose")
	if err != nil {
		t.Fatalf("propose second: %v", err)
	}
	if _, err := f.s.VerifyDestination(ctx, second.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "ev-2",
	}, "verifier-1"); err != nil {
		t.Fatalf("verify second: %v", err)
	}
	if _, err := f.s.ApproveDestination(ctx, second.DestinationID, "approver-1", "corr-second-approve"); err != nil {
		t.Fatalf("approve second: %v", err)
	}
	secondActive, err := f.s.ActivateDestination(ctx, second.DestinationID, "activator-2", "corr-second-activate")
	if err != nil {
		t.Fatalf("activate second: %v", err)
	}
	if secondActive.Status != domain.StatusActive {
		t.Fatalf("expected second destination ACTIVE, got %s", secondActive.Status)
	}

	secondEvents := outboxEventsFor(t, ctx, f, f.tenant, secondActive.DestinationID)
	var sawSecondActivated bool
	for _, e := range secondEvents {
		if e.eventType == "com.zoikosuite.banking.payee-destination.activated" {
			sawSecondActivated = true
		}
	}
	if !sawSecondActivated {
		t.Fatalf("expected the second destination's activated event, got %+v", secondEvents)
	}

	// The auto-superseded FIRST destination must have no outbox event for
	// its supersession.
	firstEventsAfterSupersede := outboxEventsFor(t, ctx, f, f.tenant, firstActive.DestinationID)
	for _, e := range firstEventsAfterSupersede {
		if e.eventType == "com.zoikosuite.banking.payee-destination.superseded" {
			t.Fatalf("auto-superseded destination must not get its own outbox event (old dual-write never published it), got %+v", firstEventsAfterSupersede)
		}
	}
}

func TestPgStore_Outbox_SuspendDestination_EnqueuesSuspendedEvent(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	verified := proposeAndVerify(t, f, f.tenant)
	if _, err := f.s.ApproveDestination(ctx, verified.DestinationID, "approver-1", "corr-approve-suspend"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	active, err := f.s.ActivateDestination(ctx, verified.DestinationID, "activator-1", "corr-activate-suspend")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	suspended, err := f.s.SuspendDestination(ctx, active.DestinationID, domain.SuspendDestinationRequest{
		Reason: "suspected fraud",
	}, "suspender-1", "corr-suspend-1")
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if suspended.Status != domain.StatusSuspended {
		t.Fatalf("expected SUSPENDED status, got %s", suspended.Status)
	}

	events := outboxEventsFor(t, ctx, f, f.tenant, suspended.DestinationID)
	var sawSuspended bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payee-destination.suspended" {
			sawSuspended = true
		}
	}
	if !sawSuspended {
		t.Fatalf("expected a suspended row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, f, f.tenant, suspended.DestinationID, "com.zoikosuite.banking.payee-destination.suspended")
	if !strings.Contains(payload, `"event_type":"PAYEE_DESTINATION_SUSPENDED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_SupersedeDestination_EnqueuesSupersededEvent(t *testing.T) {
	f := newFixture(t)
	verified := proposeAndVerify(t, f, f.tenant)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	superseded, err := f.s.SupersedeDestination(ctx, verified.DestinationID, domain.SupersedeDestinationRequest{
		Reason: "replaced by corrected details",
	}, "operator-1", "corr-supersede-1")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if superseded.Status != domain.StatusSuperseded {
		t.Fatalf("expected SUPERSEDED status, got %s", superseded.Status)
	}

	events := outboxEventsFor(t, ctx, f, f.tenant, superseded.DestinationID)
	var sawSuperseded bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payee-destination.superseded" {
			sawSuperseded = true
		}
	}
	if !sawSuperseded {
		t.Fatalf("expected a superseded row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, f, f.tenant, superseded.DestinationID, "com.zoikosuite.banking.payee-destination.superseded")
	if !strings.Contains(payload, `"event_type":"PAYEE_DESTINATION_SUPERSEDED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}
