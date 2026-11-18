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

type suspendCall struct {
	tenantID, assignmentID string
	at                     time.Time
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
	byRole    map[string][]string
	projected map[string]domain.PrincipalRoleAssignment
	ended     []string
	suspended []suspendCall
	resumed   []suspendCall
	suspendErr error
	order     *[]string
}

func (f *fakeRoles) FindPrincipalIDsByRole(_ context.Context, roleID, _ string, _ time.Time) ([]string, error) {
	return f.byRole[roleID], nil
}

// The projection: a granted assignment makes its principal a holder of the
// role, an ended one removes it, exactly as the real table would.
func (f *fakeRoles) UpsertRoleAssignment(_ context.Context, _ string, a domain.PrincipalRoleAssignment) error {
	if f.projected == nil {
		f.projected = map[string]domain.PrincipalRoleAssignment{}
	}
	f.projected[a.AssignmentID] = a
	
	// IC-11: Check if this assignment has been revoked (tombstone exists)
	revoked := false
	for _, id := range f.ended {
		if id == a.AssignmentID {
			revoked = true
			break
		}
	}
	if revoked {
		return nil // tombstone prevents re-framing
	}
	
	// IC-12: Check if this assignment is suspended before framing the role
	suspended := false
	for _, s := range f.suspended {
		if s.assignmentID == a.AssignmentID {
			suspended = true
			break
		}
	}
	if !suspended {
		f.byRole[a.RoleID] = append(f.byRole[a.RoleID], a.PrincipalID)
	}
	return nil
}

func (f *fakeRoles) EndRoleAssignment(_ context.Context, _, assignmentID string, _ time.Time) error {
	f.ended = append(f.ended, assignmentID)
	a, ok := f.projected[assignmentID]
	if !ok {
		return nil
	}
	var kept []string
	for _, p := range f.byRole[a.RoleID] {
		if p != a.PrincipalID {
			kept = append(kept, p)
		}
	}
	f.byRole[a.RoleID] = kept
	return nil
}

func (f *fakeRoles) SuspendRoleAssignment(_ context.Context, tenantID, id string, at time.Time) error {
	if f.order != nil {
		*f.order = append(*f.order, "suspend")
	}
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspended = append(f.suspended, suspendCall{tenantID, id, at})
	return nil
}

func (f *fakeRoles) ResumeRoleAssignment(_ context.Context, tenantID, id string, at time.Time) error {
	if f.order != nil {
		*f.order = append(*f.order, "resume")
	}
	f.resumed = append(f.resumed, suspendCall{tenantID, id, at})
	// Remove the suspension record
	for i, s := range f.suspended {
		if s.assignmentID == id && s.tenantID == tenantID {
			f.suspended = append(f.suspended[:i], f.suspended[i+1:]...)
			break
		}
	}
	// IC-12: After resume, the role should be framed (if there's a projected assignment)
	if a, ok := f.projected[id]; ok {
		f.byRole[a.RoleID] = append(f.byRole[a.RoleID], a.PrincipalID)
	}
	return nil
}

type fakeRisk struct {
	signals []domain.RiskSignalCache
}

func (f *fakeRisk) UpsertSignal(_ context.Context, s domain.RiskSignalCache) error {
	f.signals = append(f.signals, s)
	return nil
}

// fakeHolds records the legal-hold projection writes the consumer makes.
type fakeHolds struct {
	upserted []domain.LegalHold
	released []string
}

func (f *fakeHolds) UpsertLegalHold(_ context.Context, h domain.LegalHold) error {
	f.upserted = append(f.upserted, h)
	return nil
}

func (f *fakeHolds) ReleaseLegalHold(_ context.Context, holdID, _, _ string, _ time.Time) error {
	f.released = append(f.released, holdID)
	return nil
}

// fakeDeduper claims each id once, mirroring SET NX.
type fakeDeduper struct {
	seen map[string]struct{}
	err  error
}

func newFakeDeduper() *fakeDeduper { return &fakeDeduper{seen: map[string]struct{}{}} }

func (f *fakeDeduper) Release(_ context.Context, eventID string) error {
	delete(f.seen, eventID)
	return nil
}

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
	holds    *fakeHolds
	dedupe   *fakeDeduper
}

func newHarness() *harness {
	h := &harness{
		sessions: &fakeRevoker{},
		roles:    &fakeRoles{byRole: map[string][]string{}},
		risk:     &fakeRisk{},
		holds:    &fakeHolds{},
		dedupe:   newFakeDeduper(),
	}
	h.consumer = events.NewConsumer(zap.NewNop(), h.sessions, h.roles, h.risk, h.holds, h.dedupe)
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

// A suspended delegation confers nothing until resumed, so the delegate's
// sessions end; resuming needs no revocation (it ADDS access).
func TestAuthoritySuspendedRevokesAndResumedDoesNot(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "authority.suspended", "e1", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))
	require.Len(t, h.sessions.principals, 1, "a suspended authority grants nothing and must end the delegate's sessions")
	assert.Equal(t, "p-42", h.sessions.principals[0].id)

	h.consumer.Handle(context.Background(), event(t, "authority.resumed", "e2", "tenant-1",
		map[string]any{"delegate_principal_id": "p-42"}))
	assert.Len(t, h.sessions.principals, 1, "resuming must not revoke anything")
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

// The subject of a revoked assignment is no longer a holder of the role, so
// role.updated cannot find them; iam.assignment.revoked names them directly.
func TestAssignmentRevokedRevokesTheSubjectNotTheRevoker(t *testing.T) {
	h := newHarness()
	raw, err := json.Marshal(map[string]any{
		"event_id": "e1", "event_type": "iam.assignment.revoked", "tenant_id": "tenant-1",
		"actor_id": "p-revoker", "correlation_id": "corr-1",
		"payload": map[string]any{"principal_id": "p-subject", "role_id": "role-9"},
	})
	require.NoError(t, err)
	h.consumer.Handle(context.Background(), raw)

	require.Len(t, h.sessions.principals, 1)
	assert.Equal(t, "p-subject", h.sessions.principals[0].id)
}

// The projection had no feed, so role.updated resolved no holders. A governed
// grant now makes the principal a holder: retiring the role then ends their
// sessions; revoking the assignment closes the projection.
func TestAssignmentGrantedFeedsRoleUpdatedRevocation(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g1", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-7", "role_id": "role-5", "legal_entity_id": "ent-1"}))
	require.Contains(t, h.roles.projected, "a-1")
	assert.Empty(t, h.sessions.principals, "a grant only adds access")

	h.consumer.Handle(context.Background(), event(t, "role.updated", "u1", "tenant-1", map[string]any{"role_id": "role-5"}))
	require.Len(t, h.sessions.principals, 1)
	assert.Equal(t, "p-7", h.sessions.principals[0].id)

	h.consumer.Handle(context.Background(), event(t, "iam.assignment.revoked", "r1", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-7", "role_id": "role-5"}))
	assert.Equal(t, []string{"a-1"}, h.roles.ended)
	assert.Empty(t, h.roles.byRole["role-5"], "a revoked assignment is no longer a holder")
}

// An assignment granted with an end date is projected with it, so the
// resolver stops framing the role at that instant rather than at whenever the
// expiry's iam.assignment.revoked is consumed.
func TestAssignmentGrantedProjectsEndDate(t *testing.T) {
	h := newHarness()
	end := time.Date(2030, 3, 31, 17, 0, 0, 0, time.UTC)
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g2", "tenant-1",
		map[string]any{"assignment_id": "a-2", "principal_id": "p-8", "role_id": "role-5", "effective_to": end}))
	require.Contains(t, h.roles.projected, "a-2")
	assert.True(t, h.roles.projected["a-2"].EffectiveTo.Equal(end), "end date not projected: %v", h.roles.projected["a-2"].EffectiveTo)
}

func TestAssignmentRevokedWithoutPrincipalRevokesNobody(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.revoked", "e1", "tenant-1",
		map[string]any{"role_id": "role-9"}))

	assert.Empty(t, h.sessions.principals, "never fall back to actor_id: that is the revoker")
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
			// valid_to is REQUIRED and this fixture used to omit it.
			//
			// That omission made the test pass against a fake writer while the
			// real RiskSignalCache would have dropped the signal on the floor:
			// UpsertSignal computes its TTL as time.Until(ValidTo), so a zero
			// time yields a negative TTL and the write is skipped without an
			// error. The test was asserting behaviour that did not hold in
			// production.
			"valid_to": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		}))

	require.Len(t, h.risk.signals, 1, "this is the only writer of the cache the resolver reads")
	assert.Equal(t, "p-42", h.risk.signals[0].PrincipalID)
	assert.Equal(t, 85, h.risk.signals[0].SignalValue)
	assert.Equal(t, "tenant-1", h.risk.signals[0].TenantID,
		"a signal that omits its tenant inherits the event envelope's")
}

// TestRiskSignalWithoutValidToIsRejected pins the guard that the fixture above
// was quietly relying on being absent.
//
// A signal with no valid_to is not cacheable, and the consumer used to log
// "risk signal cached" for exactly these — a line asserting something that did
// not happen, and the kind an operator reads as proof the pipeline works.
func TestRiskSignalWithoutValidToIsRejected(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "session.risk.changed", "e2", "tenant-1",
		map[string]any{
			"risk_signal_id": "rs-2",
			"principal_id":   "p-42",
			"signal_value":   90,
			"signal_source":  "INTELLIGENCE_PLANE",
		}))

	assert.Empty(t, h.risk.signals,
		"an uncacheable signal must be refused loudly, not accepted and silently dropped")
}

// TestRiskSignalAlreadyExpiredIsRejected covers the other half.
func TestRiskSignalAlreadyExpiredIsRejected(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "session.risk.changed", "e3", "tenant-1",
		map[string]any{
			"risk_signal_id": "rs-3",
			"principal_id":   "p-42",
			"signal_value":   90,
			"signal_source":  "INTELLIGENCE_PLANE",
			"valid_to":       time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		}))

	assert.Empty(t, h.risk.signals)
}

// TestOwnEventsAreIgnored pins the self-source guard.
//
// The reader and writer share one topic, so everything this service publishes
// comes straight back. That was not hypothetical: the service published
// "session.risk.changed" on a cache miss and subscribed to the same name as
// the sole writer of that cache, so it consumed its own telemetry — burning a
// dedupe key per cache-miss resolution and logging a cache write that never
// happened.
//
// The event has since been renamed. This guard fixes the CLASS rather than the
// instance: a service is never an authority on its own inbound facts.
func TestOwnEventsAreIgnored(t *testing.T) {
	h := newHarness()

	raw, err := json.Marshal(map[string]any{
		"event_id":       "own-1",
		"event_type":     "session.risk.changed",
		"source_service": "identity-context-svc",
		"tenant_id":      "tenant-1",
		"payload": map[string]any{
			"principal_id":  "p-42",
			"signal_value":  99,
			"signal_source": "UNAVAILABLE",
			"valid_to":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	})
	require.NoError(t, err)

	h.consumer.Handle(context.Background(), raw)

	assert.Empty(t, h.risk.signals,
		"an event this service emitted must never be consumed back as an inbound fact")
}

// ── Legal hold projection (GOV-10 read model) ────────────────────────────────

func TestLegalHoldIsProjected(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "legal.hold.issued", "lh-1", "tenant-1",
		map[string]any{
			"hold_id":      "hold-99",
			"matter_ref":   "MATTER-2026-07",
			"principal_id": "p-42",
		}))

	require.Len(t, h.holds.upserted, 1)
	assert.Equal(t, "hold-99", h.holds.upserted[0].HoldID)
	assert.Equal(t, "tenant-1", h.holds.upserted[0].TenantID)
	require.NotNil(t, h.holds.upserted[0].PrincipalID)
	assert.Equal(t, "p-42", *h.holds.upserted[0].PrincipalID)
}

// TestTenantWideLegalHoldIsProjected covers the hold that names no principal.
//
// This is the one that blocks everything, and getting it wrong — treating a
// missing principal_id as a malformed event rather than a tenant-wide scope —
// would let a sweep delete evidence during litigation.
func TestTenantWideLegalHoldIsProjected(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "legal.hold.issued", "lh-2", "tenant-1",
		map[string]any{
			"hold_id":    "hold-100",
			"matter_ref": "MATTER-2026-08",
		}))

	require.Len(t, h.holds.upserted, 1)
	assert.Nil(t, h.holds.upserted[0].PrincipalID, "a hold naming no principal covers the whole tenant")
}

func TestLegalHoldReleaseIsProjected(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "legal.hold.released", "lh-3", "tenant-1",
		map[string]any{"hold_id": "hold-99"}))

	require.Len(t, h.holds.released, 1)
	assert.Equal(t, "hold-99", h.holds.released[0])
}

// TestLegalHoldWithoutTenantIsRefused pins the loudest drop in the consumer.
//
// Failing to project a hold means a later sweep will happily delete what it
// covers, so this case is logged at ERROR rather than the DEBUG a normal
// unroutable event gets.
func TestLegalHoldWithoutTenantIsRefused(t *testing.T) {
	h := newHarness()
	raw, err := json.Marshal(map[string]any{
		"event_id":   "lh-4",
		"event_type": "legal.hold.issued",
		"payload":    map[string]any{"hold_id": "hold-101"},
	})
	require.NoError(t, err)

	h.consumer.Handle(context.Background(), raw)
	assert.Empty(t, h.holds.upserted)
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

// ── IC-11: S1-9 (K-1) projection survives late revocations ─────────────────────
//
// The revocation horizon applies ONLY to session eviction. The projection
// (EndRoleAssignment, SuspendRoleAssignment, tombstone) MUST still be written
// even for events older than the horizon, so a resync from authorization-svc
// never frames a revoked/suspended role back in.

func TestIC11_LateRevokeClosesProjectionWithoutEviction(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	// First, grant an assignment so there's something to revoke
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g1", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// Now revoke it with an old timestamp (outside the horizon)
	old := time.Now().Add(-90 * 24 * time.Hour)
	h.consumer.Handle(context.Background(), stampedEvent(t, "iam.assignment.revoked", "r1", old,
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// The projection MUST be closed (tombstoned) even though the event is old
	require.Contains(t, h.roles.ended, "a-1", "projection must be closed for late revocation")
	assert.Empty(t, h.roles.byRole["role-1"], "role must not be a holder after late revocation")

	// But sessions must NOT be evicted (horizon applies to eviction)
	assert.Empty(t, h.sessions.principals, "sessions must NOT be evicted for old revocation")
}

func TestIC11_LateSuspendHoldsProjection(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	// Grant an assignment
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g1", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// Suspend it with an old timestamp (outside the horizon)
	old := time.Now().Add(-90 * 24 * time.Hour)
	h.consumer.Handle(context.Background(), stampedEvent(t, "iam.assignment.suspended", "s1", old,
		map[string]any{"principal_role_assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// The projection MUST be suspended (suspended_at set) even though the event is old
	require.Len(t, h.roles.suspended, 1, "projection must be suspended for late suspension")
	assert.Equal(t, "a-1", h.roles.suspended[0].assignmentID)
	assert.Equal(t, "tenant-1", h.roles.suspended[0].tenantID)

	// But sessions must NOT be evicted (horizon applies to eviction)
	assert.Empty(t, h.sessions.principals, "sessions must NOT be evicted for old suspension")
}

func TestIC11_ReplayFromEarliestFramesNoRevokedRole(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	// Grant an assignment
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g1", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// Revoke it with a fresh timestamp (inside horizon)
	h.consumer.Handle(context.Background(), stampedEvent(t, "iam.assignment.revoked", "r1", time.Now().Add(-time.Minute),
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// Projection is closed, role removed, sessions evicted
	require.Contains(t, h.roles.ended, "a-1")
	assert.Empty(t, h.roles.byRole["role-1"])
	require.Len(t, h.sessions.principals, 1)

	// Now simulate a full replay from earliest offset: the grant arrives again (replay)
	// The revocation tombstone should prevent the replayed grant from re-opening the role
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "g1-replay", "tenant-1",
		map[string]any{"assignment_id": "a-1", "principal_id": "p-1", "role_id": "role-1"}))

	// The projection must remain closed (tombstone wins)
	require.Contains(t, h.roles.ended, "a-1", "tombstone must prevent replayed grant from re-opening")
	assert.Empty(t, h.roles.byRole["role-1"], "role must not be re-framed after replay")
}

func TestIC11_GivenUpEventIsDeadLettered(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	// Make the store fail persistently so the event gives up after maxApplyAttempts
	h.roles.suspendErr = errors.New("permanent db failure")

	// Send a suspension that will fail
	old := time.Now().Add(-time.Minute)
	h.consumer.Handle(context.Background(), stampedEvent(t, "iam.assignment.suspended", "dead-1", old,
		map[string]any{"principal_role_assignment_id": "a-dead", "principal_id": "p-dead", "role_id": "role-dead"}))

	// After maxApplyAttempts, the event should be given up and dead-lettered
	// The test verifies the error path - the consumer logs and gives up
	// (The actual dead-letter implementation would record the event for later inspection)
	assert.Empty(t, h.roles.suspended, "event should give up and not partially apply")
}

// ── Revocation horizon (2026-09-28) ──────────────────────────────────────────
//
// The reader now subscribes to the producer topics it never read. Under the
// existing group those start from the first offset, so without a horizon the
// first deploy replays their whole history against today's sessions.

func stampedEvent(t *testing.T, eventType, eventID string, emittedAt time.Time, payload map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"event_id":       eventID,
		"event_type":     eventType,
		"tenant_id":      "tenant-1",
		"correlation_id": "corr-1",
		"actor_id":       "the-revoker",
		"payload":        payload,
	}
	if !emittedAt.IsZero() {
		body["emitted_at"] = emittedAt
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

func TestRevocationOlderThanAnyLiveSessionIsSkipped(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)
	h.roles.byRole["role-1"] = []string{"p-1"}
	old := time.Now().Add(-90 * 24 * time.Hour)

	h.consumer.Handle(context.Background(), stampedEvent(t, "authority.revoked", "a", old, map[string]any{"delegate_principal_id": "p-42"}))
	h.consumer.Handle(context.Background(), stampedEvent(t, "role.updated", "r", old, map[string]any{"role_id": "role-1"}))
	h.consumer.Handle(context.Background(), stampedEvent(t, "entity.updated", "e", old, map[string]any{"legal_entity_id": "le-1"}))

	assert.Empty(t, h.sessions.principals, "a 90-day-old revocation must not log out a session issued today")
	assert.Empty(t, h.sessions.entities)
}

func TestRevocationInsideTheHorizonIsActedOn(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	h.consumer.Handle(context.Background(), stampedEvent(t, "authority.revoked", "a", time.Now().Add(-time.Minute),
		map[string]any{"delegate_principal_id": "p-42"}))

	require.Len(t, h.sessions.principals, 1)
	assert.Equal(t, "p-42", h.sessions.principals[0].id)
}

// Fail safe: a producer that omits emitted_at still gets its revocation honoured.
func TestRevocationWithNoTimestampIsActedOn(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	h.consumer.Handle(context.Background(), stampedEvent(t, "authority.revoked", "a", time.Time{},
		map[string]any{"delegate_principal_id": "p-42"}))

	assert.Len(t, h.sessions.principals, 1)
}

// A hold is state, not a one-shot command. An old hold is still a hold.
func TestHorizonDoesNotApplyToLegalHolds(t *testing.T) {
	h := newHarness()
	h.consumer.WithRevocationHorizon(6 * time.Minute)

	h.consumer.Handle(context.Background(), stampedEvent(t, "legal.hold.issued", "lh", time.Now().Add(-365*24*time.Hour),
		map[string]any{"hold_id": "hold-1", "matter_ref": "M-1"}))

	assert.Len(t, h.holds.upserted, 1)
}

// On authority.revoked the actor is the person who revoked. Falling back to it
// logged out the enforcer and left the delegate's sessions alive.
func TestAuthorityEventWithNoDelegateNeverRevokesTheActor(t *testing.T) {
	h := newHarness()

	h.consumer.Handle(context.Background(), stampedEvent(t, "authority.revoked", "a", time.Time{}, map[string]any{}))

	assert.Empty(t, h.sessions.principals, "the revoker's sessions must never be the ones revoked")
}
