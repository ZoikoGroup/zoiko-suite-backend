// Package events_test covers the inbound consumer's dispatch and its refusal
// rules. These paths were unreachable before the consumer was wired into
// main.go, so every assertion here is about behaviour that previously did not
// run at all.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
)

type revocation struct {
	id       string
	tenantID string
	reason   domain.InvalidationReason
}

type fakeRevoker struct {
	principals []revocation
	entities   []revocation
	err        error
}

func (f *fakeRevoker) EvictAllForPrincipal(_ context.Context, principalID, tenantID string, reason domain.InvalidationReason) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.principals = append(f.principals, revocation{principalID, tenantID, reason})
	return 1, nil
}

func (f *fakeRevoker) EvictAllForEntity(_ context.Context, legalEntityID, tenantID string, reason domain.InvalidationReason) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.entities = append(f.entities, revocation{legalEntityID, tenantID, reason})
	return 1, nil
}

type fakeRoles struct {
	byRole map[string][]string
}

func (f *fakeRoles) FindPrincipalIDsByRole(_ context.Context, roleID, _ string, _ time.Time) ([]string, error) {
	return f.byRole[roleID], nil
}

type fakeRisk struct {
	signals []domain.RiskSignalCache
}

func (f *fakeRisk) UpsertSignal(_ context.Context, s domain.RiskSignalCache) error {
	f.signals = append(f.signals, s)
	return nil
}

// fakeDeduper claims each id once, mirroring SET NX.
type fakeDeduper struct {
	seen map[string]struct{}
	err  error
}

func newFakeDeduper() *fakeDeduper { return &fakeDeduper{seen: map[string]struct{}{}} }

func (f *fakeDeduper) Claim(_ context.Context, eventID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if _, dup := f.seen[eventID]; dup {
		return false, nil
	}
	f.seen[eventID] = struct{}{}
	return true, nil
}

type harness struct {
	consumer *events.Consumer
	sessions *fakeRevoker
	roles    *fakeRoles
	risk     *fakeRisk
	dedupe   *fakeDeduper
}

func newHarness() *harness {
	h := &harness{
		sessions: &fakeRevoker{},
		roles:    &fakeRoles{byRole: map[string][]string{}},
		risk:     &fakeRisk{},
		dedupe:   newFakeDeduper(),
	}
	h.consumer = events.NewConsumer(zap.NewNop(), h.sessions, h.roles, h.risk, h.dedupe)
	return h
}

func event(t *testing.T, eventType, eventID, tenantID string, payload map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"event_id":       eventID,
		"event_type":     eventType,
		"tenant_id":      tenantID,
		"correlation_id": "corr-1",
	}
	if payload != nil {
		body["payload"] = payload
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

func TestAuthorityRevokedRevokesDelegateSessions(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.revoked", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))

	require.Len(t, h.sessions.principals, 1)
	assert.Equal(t, "p-42", h.sessions.principals[0].id)
	assert.Equal(t, "tenant-1", h.sessions.principals[0].tenantID)
	assert.Equal(t, domain.InvalidationReasonDelegationRevoked, h.sessions.principals[0].reason,
		"the evidence trail must say the session was revoked, not that it expired")
}

func TestAuthorityExpiredRevokesToo(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.expired", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))
	assert.Len(t, h.sessions.principals, 1, "an expired authority grants nothing and must revoke like a revoked one")
}

func TestDuplicateEventIsNotActedOnTwice(t *testing.T) {
	h := newHarness()
	raw := event(t, "authority.revoked", "same-id", "tenant-1", map[string]any{"delegate_principal_id": "p-42"})

	h.consumer.Handle(context.Background(), raw)
	h.consumer.Handle(context.Background(), raw)

	assert.Len(t, h.sessions.principals, 1, "redelivery must not revoke twice")
}

func TestDedupeFailureStillProcesses(t *testing.T) {
	h := newHarness()
	h.dedupe.err = errors.New("redis down")

	h.consumer.Handle(context.Background(), event(t, "authority.revoked", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))

	assert.Len(t, h.sessions.principals, 1,
		"a dedupe outage must not suppress a revocation — acting twice is cheaper than a revoked authority that keeps working")
}

func TestEventWithoutTenantIsDropped(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.revoked", "e1", "",
		map[string]any{"delegate_principal_id": "p-42"}))

	assert.Empty(t, h.sessions.principals, "session storage is tenant-scoped; there is no safe default tenant")
}

func TestEventWithoutEventIDIsDropped(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.revoked", "", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))

	assert.Empty(t, h.sessions.principals, "an event that cannot be deduplicated cannot be acted on safely")
}

func TestAuthorityDelegatedRevokesNothing(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.delegated", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))

	assert.Empty(t, h.sessions.principals, "a new delegation only adds authority — revoking would log everyone out to grant them more")
}

func TestRoleUpdatedRevokesEveryHolder(t *testing.T) {
	h := newHarness()
	h.roles.byRole["role-9"] = []string{"p-1", "p-2"}

	h.consumer.Handle(context.Background(), event(t, "role.updated", "e1", "tenant-1",
		map[string]any{"role_id": "role-9"}))

	require.Len(t, h.sessions.principals, 2)
	assert.Equal(t, "p-1", h.sessions.principals[0].id)
	assert.Equal(t, "p-2", h.sessions.principals[1].id)
}

func TestEntityUpdatedRevokesEntityScopedSessions(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "entity.updated", "e1", "tenant-1",
		map[string]any{"legal_entity_id": "ent-7"}))

	require.Len(t, h.sessions.entities, 1)
	assert.Equal(t, "ent-7", h.sessions.entities[0].id)
}

func TestRiskSignalIsCached(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "session.risk.changed", "e1", "tenant-1",
		map[string]any{
			"risk_signal_id": "rs-1",
			"principal_id":   "p-42",
			"signal_value":   85,
			"signal_source":  "INTELLIGENCE_PLANE",
		}))

	require.Len(t, h.risk.signals, 1, "this is the only writer of the cache the resolver reads")
	assert.Equal(t, "p-42", h.risk.signals[0].PrincipalID)
	assert.Equal(t, 85, h.risk.signals[0].SignalValue)
	assert.Equal(t, "tenant-1", h.risk.signals[0].TenantID,
		"a signal that omits its tenant inherits the event envelope's")
}

func TestRiskSignalWithoutPrincipalIsDropped(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "session.risk.changed", "e1", "tenant-1",
		map[string]any{"signal_value": 85}))

	assert.Empty(t, h.risk.signals)
}

func TestUndecodableEventIsDropped(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), []byte("{not json"))
	assert.Empty(t, h.sessions.principals)
}

func TestUnknownEventTypeIsIgnored(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "something.else", "e1", "tenant-1", nil))
	assert.Empty(t, h.sessions.principals)
	assert.Empty(t, h.risk.signals)
}

// A UTF-8 BOM in front of otherwise valid JSON must not cost a revocation.
// encoding/json rejects it outright, and Windows producers emit one readily —
// a PowerShell pipe adds one without asking. Three invisible bytes should not
// be the difference between a revoked authority and one that keeps working.
func TestLeadingBOMIsTolerated(t *testing.T) {
	h := newHarness()
	raw := append([]byte("\uFEFF"), event(t, "authority.revoked", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"})...)

	h.consumer.Handle(context.Background(), raw)

	require.Len(t, h.sessions.principals, 1, "a BOM-prefixed event must still be acted on")
	assert.Equal(t, "p-42", h.sessions.principals[0].id)
}
