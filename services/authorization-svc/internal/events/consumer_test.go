package events_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

// fakeProjector records what the consumer asked the store to do. The
// consumer's whole job is translating an upstream event into one of two store
// calls, so the recorded arguments ARE the behaviour under test.
type fakeProjector struct {
	projected []domain.ProjectDelegationParams
	revoked   [][3]string // service, delegationID, tenantID
	revokeErr error
	projErr   error
}

func (f *fakeProjector) ProjectDelegation(_ context.Context, p domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error) {
	if f.projErr != nil {
		return nil, f.projErr
	}
	f.projected = append(f.projected, p)
	return &domain.DelegatedAuthority{DelegatedAuthorityID: "local-1"}, nil
}

func (f *fakeProjector) RevokeProjectedDelegation(_ context.Context, service, delegationID, tenantID string) (*domain.DelegatedAuthority, error) {
	if f.revokeErr != nil {
		return nil, f.revokeErr
	}
	f.revoked = append(f.revoked, [3]string{service, delegationID, tenantID})
	return &domain.DelegatedAuthority{DelegatedAuthorityID: "local-1"}, nil
}

const delegatedEvent = `{
  "event_id": "evt-1",
  "event_type": "authority.delegated",
  "event_version": "1.0",
  "source_service": "delegated-authority-svc",
  "tenant_id": "00000000-0000-0000-0000-0000000000a1",
  "legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
  "actor_id": "admin-1",
  "correlation_id": "corr-1",
  "payload": {
    "delegation_id": "upstream-1",
    "legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
    "delegator_principal_id": "boss-1",
    "delegate_principal_id": "assistant-1",
    "action_type": "PAYMENT_APPROVE",
    "effective_from": "2026-09-01T00:00:00Z",
    "effective_to": null
  }
}`

func TestConsumer_ProjectsDelegatedEvent(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	c.Handle(context.Background(), []byte(delegatedEvent))

	if len(f.projected) != 1 {
		t.Fatalf("projected %d delegations, want 1", len(f.projected))
	}
	got := f.projected[0]
	if got.SourceDelegationID != "upstream-1" {
		t.Errorf("source_delegation_id = %q, want upstream-1", got.SourceDelegationID)
	}
	if got.SourceService != "delegated-authority-svc" {
		t.Errorf("source_service = %q, want delegated-authority-svc", got.SourceService)
	}
	if got.TenantID != "00000000-0000-0000-0000-0000000000a1" {
		t.Errorf("tenant_id = %q, want the envelope's tenant", got.TenantID)
	}
	if got.DelegatorPrincipalID != "boss-1" || got.DelegatePrincipalID != "assistant-1" {
		t.Errorf("principals = %q -> %q", got.DelegatorPrincipalID, got.DelegatePrincipalID)
	}
	// Upstream delegates ONE action per grant, which is the ACTION_SUBSET case
	// migration 000008 added delegated_actions for. Projecting it as a FULL
	// delegation would confer the delegator's whole grant set instead of the
	// single action upstream authorised.
	if len(got.DelegatedActions) != 1 || got.DelegatedActions[0] != "PAYMENT_APPROVE" {
		t.Errorf("delegated_actions = %v, want [PAYMENT_APPROVE] — projecting this as full authority would over-grant", got.DelegatedActions)
	}
	if got.EffectiveTo != nil {
		t.Errorf("effective_to = %v, want nil for an open-ended delegation — a zero time here would mark it expired in year 1", got.EffectiveTo)
	}
	if got.LegalEntityID == nil || *got.LegalEntityID != "00000000-0000-0000-0000-0000000000e1" {
		t.Errorf("legal_entity_id = %v", got.LegalEntityID)
	}
}

func TestConsumer_DeduplicatesOnEventID(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	c.Handle(context.Background(), []byte(delegatedEvent))
	c.Handle(context.Background(), []byte(delegatedEvent))
	c.Handle(context.Background(), []byte(delegatedEvent))

	if len(f.projected) != 1 {
		t.Fatalf("projected %d times for 3 redeliveries of one event, want 1", len(f.projected))
	}
}

func TestConsumer_RevokedAndExpiredEndTheProjection(t *testing.T) {
	for _, eventType := range []string{"authority.revoked", "authority.expired"} {
		t.Run(eventType, func(t *testing.T) {
			f := &fakeProjector{}
			c := events.NewConsumer(zap.NewNop(), f)

			c.Handle(context.Background(), []byte(`{
			  "event_id": "evt-2",
			  "event_type": "`+eventType+`",
			  "tenant_id": "00000000-0000-0000-0000-0000000000a1",
			  "correlation_id": "corr-2",
			  "payload": {"delegation_id": "upstream-1", "legal_entity_id": "00000000-0000-0000-0000-0000000000e1"}
			}`))

			if len(f.revoked) != 1 {
				t.Fatalf("revoked %d delegations, want 1", len(f.revoked))
			}
			if f.revoked[0] != [3]string{"delegated-authority-svc", "upstream-1", "00000000-0000-0000-0000-0000000000a1"} {
				t.Fatalf("revoked with %v", f.revoked[0])
			}
		})
	}
}

// TestConsumer_AlreadyEndedIsNotAnError — a redelivered revocation, or one
// naming a delegation created before this projection existed, finds no row.
// The outcome the event asked for already holds, so it is benign; treating it
// as a failure would fill the log with alarms about work that is done.
func TestConsumer_AlreadyEndedIsNotAnError(t *testing.T) {
	f := &fakeProjector{revokeErr: domain.ErrDelegatedAuthorityNotFound}
	c := events.NewConsumer(zap.NewNop(), f)

	c.Handle(context.Background(), []byte(`{
	  "event_id": "evt-3",
	  "event_type": "authority.revoked",
	  "tenant_id": "00000000-0000-0000-0000-0000000000a1",
	  "payload": {"delegation_id": "never-projected"}
	}`))
	// No panic, no projection. The assertion is that Handle returns at all —
	// this consumer must never let one event stall the partition.
	if len(f.projected) != 0 {
		t.Fatalf("a revocation projected something: %v", f.projected)
	}
}

// TestConsumer_RefusesTenantlessEvents. delegated_authorities.tenant_id is NOT
// NULL since 000006, and a row written under a guessed tenant is a delegation
// that grants authority in the wrong place. Refusing is the only correct
// outcome.
func TestConsumer_RefusesTenantlessEvents(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	c.Handle(context.Background(), []byte(`{
	  "event_id": "evt-4",
	  "event_type": "authority.delegated",
	  "tenant_id": "",
	  "payload": {"delegation_id": "upstream-2", "delegator_principal_id": "boss-1", "delegate_principal_id": "assistant-1", "action_type": "PAYMENT_APPROVE"}
	}`))

	if len(f.projected) != 0 {
		t.Fatalf("projected a tenantless delegation: %v", f.projected)
	}
}

// TestConsumer_IgnoresUnrelatedEvents — a producer adding an event type to the
// topic must not stall this consumer or be misapplied.
func TestConsumer_IgnoresUnrelatedEvents(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	for _, eventType := range []string{"authority.approved", "delegation.reminder.sent", "tenant.created"} {
		c.Handle(context.Background(), []byte(`{
		  "event_id": "evt-`+eventType+`",
		  "event_type": "`+eventType+`",
		  "tenant_id": "00000000-0000-0000-0000-0000000000a1",
		  "payload": {"delegation_id": "upstream-9"}
		}`))
	}
	if len(f.projected) != 0 || len(f.revoked) != 0 {
		t.Fatalf("acted on unrelated events: projected=%v revoked=%v", f.projected, f.revoked)
	}
}

// TestConsumer_SurvivesMalformedMessages. This consumer commits its offset by
// reading the next message, so a message it cannot apply must be logged and
// skipped — one malformed event blocking every subsequent delegation is worse
// for a projection whose staleness silently denies people access.
func TestConsumer_SurvivesMalformedMessages(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	for _, raw := range []string{
		``,
		`not json at all`,
		`{"event_type": "authority.delegated"`,
		// Right shape, no tenant and no payload.
		`{"event_id":"x","event_type":"authority.delegated"}`,
		// Tenant present, payload names no delegation_id — nothing to key on.
		`{"event_id":"y","event_type":"authority.delegated","tenant_id":"00000000-0000-0000-0000-0000000000a1","payload":{}}`,
		// Payload is a string, not an object.
		`{"event_id":"z","event_type":"authority.delegated","tenant_id":"00000000-0000-0000-0000-0000000000a1","payload":"nope"}`,
	} {
		c.Handle(context.Background(), []byte(raw))
	}

	if len(f.projected) != 0 || len(f.revoked) != 0 {
		t.Fatalf("a malformed message produced a store write: projected=%v revoked=%v", f.projected, f.revoked)
	}

	// And a good message still lands afterwards.
	c.Handle(context.Background(), []byte(delegatedEvent))
	if len(f.projected) != 1 {
		t.Fatalf("the stream did not recover: projected %d", len(f.projected))
	}
}

// TestConsumer_MissingEffectiveFromDefaultsToNow — effective_from is NOT NULL
// in the schema. A zero time would make the delegation retroactively effective
// from year 1, which is a wider grant than upstream described.
func TestConsumer_MissingEffectiveFromDefaultsToNow(t *testing.T) {
	f := &fakeProjector{}
	c := events.NewConsumer(zap.NewNop(), f)

	before := time.Now().Add(-time.Minute)
	c.Handle(context.Background(), []byte(`{
	  "event_id": "evt-5",
	  "event_type": "authority.delegated",
	  "tenant_id": "00000000-0000-0000-0000-0000000000a1",
	  "payload": {"delegation_id": "upstream-3", "delegator_principal_id": "boss-1", "delegate_principal_id": "assistant-1", "action_type": "PAYMENT_APPROVE"}
	}`))

	if len(f.projected) != 1 {
		t.Fatalf("projected %d, want 1", len(f.projected))
	}
	if f.projected[0].EffectiveFrom.Before(before) {
		t.Fatalf("effective_from = %v, want approximately now rather than the zero time",
			f.projected[0].EffectiveFrom)
	}
}

// TestConsumedEventTypes pins the deliberate scope: the three events that have
// a real producer, and not the ones that still do not. progress.md's v1 note
// listed four events as unconsumed because nothing published them; three of
// those still have no producer anywhere on the estate, and a consumer for them
// would still be dead infrastructure.
func TestConsumedEventTypes(t *testing.T) {
	for _, want := range []string{"authority.delegated", "authority.revoked", "authority.expired"} {
		if !events.ConsumedEventTypes[want] {
			t.Errorf("%s is not consumed", want)
		}
	}
	for _, notWanted := range []string{"role.assigned", "employment.changed", "entity.scope.updated"} {
		if events.ConsumedEventTypes[notWanted] {
			t.Errorf("%s is consumed — it has no producer on this estate, so the handler cannot have been tested against a real event", notWanted)
		}
	}
}
