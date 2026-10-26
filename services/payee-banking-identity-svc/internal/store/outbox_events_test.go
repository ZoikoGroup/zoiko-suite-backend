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
	d, _, err := f.s.ProposeDestination(ctx, tenantID, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: "party-" + uuid.New().String(), FinancialInstitution: "Outbox Test Bank",
		AccountIdentifier: "GB29OTBK" + uuid.New().String()[:14], CountryCode: "GB", Currency: "GBP",
		PayeeName: "Outbox Payee Ltd", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-propose-1", "key-propose-"+uuid.New().String())
	if err != nil {
		t.Fatalf("propose destination: %v", err)
	}
	verified, _, err := f.s.VerifyDestination(ctx, d.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "evidence-1",
	}, "verifier-1", "key-verify-"+uuid.New().String())
	if err != nil {
		t.Fatalf("verify destination: %v", err)
	}
	return verified
}

func TestPgStore_Outbox_ProposeDestination_EnqueuesProposedEvent(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	d, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: "party-outbox-1", FinancialInstitution: "Outbox Test Bank",
		AccountIdentifier: "GB29OTBK60161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Outbox Payee Ltd", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-propose-outbox-1", "key-propose-outbox-1")
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

// TestPgStore_Outbox_VerifyDestination_KnownGap_NoVerifiedEvent records a
// deviation, not a desirable behavior: §4.10's "Events produced" line names
// PayeeDestinationVerified, but the outbox has no verified event yet. This
// pass did not add one; see SPEC_DEVIATIONS A8.
func TestPgStore_Outbox_VerifyDestination_KnownGap_NoVerifiedEvent(t *testing.T) {
	f := newFixture(t)
	verified := proposeAndVerify(t, f, f.tenant)
	ctx := middleware.WithTenant(context.Background(), f.tenant)

	events := outboxEventsFor(t, ctx, f, f.tenant, verified.DestinationID)
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payee-destination.verified" {
			t.Fatalf("the known gap was closed unexpectedly; update SPEC_DEVIATIONS A8 and this test, got %+v", events)
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

	approved, _, err := f.s.ApproveDestination(ctx, verified.DestinationID, "approver-1", "corr-approve-1", "key-approve-outbox-1")
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

// TestPgStore_Outbox_ActivateDestination_EnqueuesBothSidesOfTheChange
// proves the newly-ACTIVE destination gets its activated event and the OLD
// destination ActivateDestination auto-supersedes gets a superseded event in
// the same transaction. The spec's mandatory control is "Any
// active-destination change emits an immediate high-risk event"; an
// activation that replaces the active row is such a change. Before this
// pass only the internal audit row existed for the old destination, so a
// subscriber watching the outbox would see the new active version but never
// the retirement of the previous one.
func TestPgStore_Outbox_ActivateDestination_EnqueuesBothSidesOfTheChange(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-activate-" + uuid.New().String()

	// First destination: propose -> verify -> approve -> activate.
	first, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank One",
		AccountIdentifier: "GB29ONE160161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Payee One", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-first-propose", "key-first-propose")
	if err != nil {
		t.Fatalf("propose first: %v", err)
	}
	if _, _, err := f.s.VerifyDestination(ctx, first.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "ev-1",
	}, "verifier-1", "key-first-verify"); err != nil {
		t.Fatalf("verify first: %v", err)
	}
	if _, _, err := f.s.ApproveDestination(ctx, first.DestinationID, "approver-1", "corr-first-approve", "key-first-approve"); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	firstActive, _, err := f.s.ActivateDestination(ctx, first.DestinationID, "activator-1", "corr-first-activate", "key-first-activate")
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
	second, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank Two",
		AccountIdentifier: "GB29TWO160161331926819", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Payee Two", SourceType: domain.SourceManualEntry,
	}, "proposer-2", "corr-second-propose", "key-second-propose")
	if err != nil {
		t.Fatalf("propose second: %v", err)
	}
	if _, _, err := f.s.VerifyDestination(ctx, second.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "ev-2",
	}, "verifier-1", "key-second-verify"); err != nil {
		t.Fatalf("verify second: %v", err)
	}
	if _, _, err := f.s.ApproveDestination(ctx, second.DestinationID, "approver-1", "corr-second-approve", "key-second-approve"); err != nil {
		t.Fatalf("approve second: %v", err)
	}
	secondActive, _, err := f.s.ActivateDestination(ctx, second.DestinationID, "activator-2", "corr-second-activate", "key-second-activate")
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

	// The auto-superseded FIRST destination gets its own superseded event,
	// with the safe projection, in the same transaction as the activation.
	firstEventsAfterSupersede := outboxEventsFor(t, ctx, f, f.tenant, firstActive.DestinationID)
	var sawFirstSuperseded bool
	for _, e := range firstEventsAfterSupersede {
		if e.eventType == "com.zoikosuite.banking.payee-destination.superseded" {
			sawFirstSuperseded = true
		}
	}
	if !sawFirstSuperseded {
		t.Fatalf("expected the auto-superseded destination's superseded event, got %+v", firstEventsAfterSupersede)
	}
	payload := legacyEventTypeOf(t, ctx, f, f.tenant, firstActive.DestinationID, "com.zoikosuite.banking.payee-destination.superseded")
	if !strings.Contains(payload, `"event_type":"PAYEE_DESTINATION_SUPERSEDED"`) {
		t.Fatalf("legacy event_type missing or wrong in old destination payload: %s", payload)
	}
	if !strings.Contains(payload, `"status":"SUPERSEDED"`) {
		t.Fatalf("old destination payload must carry its new SUPERSEDED status: %s", payload)
	}
}

func TestPgStore_Outbox_SuspendDestination_EnqueuesSuspendedEvent(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	verified := proposeAndVerify(t, f, f.tenant)
	if _, _, err := f.s.ApproveDestination(ctx, verified.DestinationID, "approver-1", "corr-approve-suspend", "key-approve-suspend"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	active, _, err := f.s.ActivateDestination(ctx, verified.DestinationID, "activator-1", "corr-activate-suspend", "key-activate-suspend")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	suspended, _, err := f.s.SuspendDestination(ctx, active.DestinationID, domain.SuspendDestinationRequest{
		Reason: "suspected fraud",
	}, "suspender-1", "corr-suspend-1", "key-suspend-1")
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

	superseded, _, err := f.s.SupersedeDestination(ctx, verified.DestinationID, domain.SupersedeDestinationRequest{
		Reason: "replaced by corrected details",
	}, "operator-1", "corr-supersede-1", "key-supersede-1")
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

// TestPgStore_Outbox_NeverCarriesFullAccountIdentifier is the real proof of
// a property previously (and more weakly) tested at the handler layer in
// internal/handler/event_payload_test.go, which asserted it against the
// stub publisher's in-memory params — a test that could never catch a leak
// introduced at the store layer, since publishing has moved there. This is
// that same proof, against the actual bytes written to eventing_outbox:
// every lifecycle event uses domain.NewDestinationEventPayload's safe
// projection, never the raw *domain.PayeeDestination (which carries the
// full, unmasked AccountIdentifier) — across every event-producing path,
// not just propose.
func TestPgStore_Outbox_NeverCarriesFullAccountIdentifier(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	const fullAccount = "GB29SECRET1234567890"
	const institutionName = "Confidential Bank Of Testing"

	d, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: "party-" + uuid.New().String(), FinancialInstitution: institutionName,
		AccountIdentifier: fullAccount, CountryCode: "GB", Currency: "GBP",
		PayeeName: "Secret Payee Ltd", SourceType: domain.SourceManualEntry,
	}, "proposer-1", "corr-secret-propose", "key-secret-propose")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, _, err := f.s.VerifyDestination(ctx, d.DestinationID, domain.VerifyDestinationRequest{
		VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "evidence-secret",
	}, "verifier-1", "key-secret-verify"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, _, err := f.s.ApproveDestination(ctx, d.DestinationID, "approver-1", "corr-secret-approve", "key-secret-approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	active, _, err := f.s.ActivateDestination(ctx, d.DestinationID, "activator-1", "corr-secret-activate", "key-secret-activate")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, _, err := f.s.SuspendDestination(ctx, active.DestinationID, domain.SuspendDestinationRequest{
		Reason: "fraud review",
	}, "suspender-1", "corr-secret-suspend", "key-secret-suspend"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	events := outboxEventsFor(t, ctx, f, f.tenant, d.DestinationID)
	wantTypes := map[string]bool{
		"com.zoikosuite.banking.payee-destination.proposed":  false,
		"com.zoikosuite.banking.payee-destination.approved":  false,
		"com.zoikosuite.banking.payee-destination.activated": false,
		"com.zoikosuite.banking.payee-destination.suspended": false,
	}
	secrets := []string{fullAccount, institutionName}
	for _, e := range events {
		wantTypes[e.eventType] = true
		raw := legacyEventTypeOf(t, ctx, f, f.tenant, d.DestinationID, e.eventType)
		for _, s := range secrets {
			if strings.Contains(raw, s) {
				t.Errorf("%s: outbox payload contains %q, the safe projection must never carry it: %s", e.eventType, s, raw)
			}
		}
		lower := strings.ToLower(raw)
		if strings.Contains(lower, "accountidentifier") || strings.Contains(lower, "account_identifier") {
			t.Errorf("%s: outbox payload carries an account-identifier field at all: %s", e.eventType, raw)
		}
		if !strings.Contains(raw, d.DestinationID) {
			t.Errorf("%s: projection lost its own destination_id: %s", e.eventType, raw)
		}
	}
	for et, seen := range wantTypes {
		if !seen {
			t.Errorf("no %s event was found in the outbox", et)
		}
	}
}
