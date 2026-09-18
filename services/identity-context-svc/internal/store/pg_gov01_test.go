// Integration tests for the GOV-01 store methods. Skipped unless
// TEST_DATABASE_URL is set, matching pg_store_test.go, and sharing its
// openTestPool harness — which drops and recreates the schema, so point it at
// a scratch database only.
//
// These exist because everything in pg_gov01.go is SQL, and SQL that has never
// been executed is not code that has been reviewed. The unit tests elsewhere
// exercise the logic AROUND these calls with fakes; nothing but a real
// Postgres exercises the RLS predicates, the ON CONFLICT clauses, the CHECK
// constraints or the partial indexes.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

func sessionFor(id, principalID, tenantID string) domain.SessionContext {
	now := time.Now().UTC()
	return domain.SessionContext{
		SessionContextID: id,
		PrincipalID:      principalID,
		TenantID:         tenantID,
		LegalEntityID:    "entity-1",
		CorrelationID:    "corr-" + id,
		TrustPosture:     domain.TrustPostureStandard,
		RiskSignalSource: "UNAVAILABLE",
		EnvelopeJWTJTI:   "jti-" + id,
		IssuedAt:         now,
		ExpiresAt:        now.Add(5 * time.Minute),
		SourceService:    "identity-context-svc",
		SchemaVersion:    "1.0",
		IngressSource:    "tenant-a.zoiko.io",
		Environment:      domain.EnvironmentProduction,
		EvidenceID:       "ev-" + id,
		RetentionClass:   domain.RetentionClassSessionEvidence,
	}
}

func eventFor(id, tenantID string) outbox.Record {
	return outbox.Record{
		EventID:      "evt-" + id,
		EventType:    "identity.context.resolved",
		TenantID:     tenantID,
		PartitionKey: id,
		Payload:      []byte(`{"session_context_id":"` + id + `"}`),
	}
}

// ── Atomic evidence ──────────────────────────────────────────────────────────

// TestInsertSessionContextWithEvent_IsAtomic is the whole point of the outbox.
func TestInsertSessionContextWithEvent_IsAtomic(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")

	require.NoError(t, s.InsertSessionContextWithEvent(ctx, sessionFor("sc-1", "p-1", "tenant-a"), eventFor("sc-1", "tenant-a")))

	var sessions, events int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_contexts WHERE session_context_id='sc-1'`).Scan(&sessions))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE event_id='evt-sc-1'`).Scan(&events))

	assert.Equal(t, 1, sessions)
	assert.Equal(t, 1, events, "the event must land in the same transaction as the row it attests")
}

// TestInsertSessionContextWithEvent_RollsBackBothOnFailure.
//
// A foreign-key violation on the session row must take the event with it.
// "An event for a session that failed to persist" is one of the two states the
// atomic write exists to make unreachable.
func TestInsertSessionContextWithEvent_RollsBackBothOnFailure(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	// p-missing does not exist, so the session_contexts FK fails.
	err := s.InsertSessionContextWithEvent(ctx,
		sessionFor("sc-2", "p-missing", "tenant-a"), eventFor("sc-2", "tenant-a"))
	require.Error(t, err)

	var events int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE event_id='evt-sc-2'`).Scan(&events))
	assert.Zero(t, events, "the event must roll back with the row it attests")
}

func TestInsertSessionContextWithEvent_PersistsTheNewDecisionFields(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")
	require.NoError(t, s.InsertSessionContextWithEvent(ctx, sessionFor("sc-3", "p-1", "tenant-a"), eventFor("sc-3", "tenant-a")))

	got, err := s.FindSessionContext(ctx, "sc-3", "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, got)

	// These four are the TenantContextDecision fields that had no column
	// before migration 000007 — the reason negative path #2 was untestable.
	assert.Equal(t, "tenant-a.zoiko.io", got.IngressSource)
	assert.Equal(t, domain.EnvironmentProduction, got.Environment)
	assert.Equal(t, "ev-sc-3", got.EvidenceID)
	assert.Equal(t, domain.RetentionClassSessionEvidence, got.RetentionClass)
}

// TestInsertSessionContext_DefaultsRatherThanWritingEmptyStrings.
//
// An empty ingress must be recorded as UNKNOWN, for the same reason
// risk_signal_source records UNAVAILABLE: "we did not observe one" and "we
// forgot to record one" must not look identical in the evidence.
func TestInsertSessionContext_DefaultsIngressAndEnvironment(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")
	sc := sessionFor("sc-4", "p-1", "tenant-a")
	sc.IngressSource = ""
	sc.Environment = ""
	sc.RetentionClass = ""
	require.NoError(t, s.InsertSessionContext(ctx, sc))

	got, err := s.FindSessionContext(ctx, "sc-4", "tenant-a")
	require.NoError(t, err)
	assert.Equal(t, domain.IngressUnknown, got.IngressSource)
	assert.Equal(t, domain.EnvironmentLocal, got.Environment)
}

// ── Tenant-wide revocation ───────────────────────────────────────────────────

func TestFindLiveSessionIDsForTenant_ExcludesExpiredAndInvalidated(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")

	live := sessionFor("sc-live", "p-1", "tenant-a")
	require.NoError(t, s.InsertSessionContext(ctx, live))

	expired := sessionFor("sc-expired", "p-1", "tenant-a")
	expired.ExpiresAt = now.Add(-time.Minute)
	require.NoError(t, s.InsertSessionContext(ctx, expired))

	revoked := sessionFor("sc-revoked", "p-1", "tenant-a")
	require.NoError(t, s.InsertSessionContext(ctx, revoked))
	require.NoError(t, s.MarkSessionInvalidated(ctx, "sc-revoked", "tenant-a", domain.InvalidationReasonLogout, now))

	ids, err := s.FindLiveSessionIDsForTenant(ctx, "tenant-a", now)
	require.NoError(t, err)
	assert.Equal(t, []string{"sc-live"}, ids)
}

func TestFindLiveSessionIDsForTenant_IsTenantScoped(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-a", "tenant-a", "idp|a", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-b", "tenant-b", "idp|b", "ACTIVE")
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-a", "p-a", "tenant-a")))
	require.NoError(t, s.InsertSessionContext(ctx, sessionFor("sc-b", "p-b", "tenant-b")))

	ids, err := s.FindLiveSessionIDsForTenant(ctx, "tenant-a", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, []string{"sc-a"}, ids, "a tenant-wide revocation must not reach another tenant")
}

// ── Ingress bindings ─────────────────────────────────────────────────────────

func TestIngressBinding_LookupIsCaseAndPortInsensitive(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	require.NoError(t, s.UpsertIngressBinding(ctx, domain.TenantIngressBinding{
		IngressIdentifier: "Tenant-A.Zoiko.IO", // upsert lowercases
		TenantID:          "tenant-a",
		Environment:       domain.EnvironmentProduction,
		ActiveFlag:        true,
	}))

	// A mixed-case lookup must hit, or a miss would become a refusal.
	got, err := s.FindIngressBinding(ctx, "TENANT-A.ZOIKO.IO")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tenant-a", got.TenantID)
}

func TestIngressBinding_UnknownIdentifierReturnsNilNotError(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	got, err := s.FindIngressBinding(ctx, "nobody.example.com")
	require.NoError(t, err, "an unknown host is a real answer, not a failure")
	assert.Nil(t, got)
}

func TestIngressBinding_InactiveBindingDoesNotResolve(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	require.NoError(t, s.UpsertIngressBinding(ctx, domain.TenantIngressBinding{
		IngressIdentifier: "retired.zoiko.io",
		TenantID:          "tenant-a",
		Environment:       domain.EnvironmentProduction,
		ActiveFlag:        false,
	}))

	got, err := s.FindIngressBinding(ctx, "retired.zoiko.io")
	require.NoError(t, err)
	assert.Nil(t, got, "a decommissioned hostname must not still resolve to its old tenant")
}

// TestTouchIngressBindings_MarksStaleWithoutDeleting.
//
// Deleting would make every request on that hostname fail closed until the
// registry sync ran again — a cache refresh becoming an outage.
func TestTouchIngressBindings_MarksStaleWithoutDeleting(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	require.NoError(t, s.UpsertIngressBinding(ctx, domain.TenantIngressBinding{
		IngressIdentifier: "a.zoiko.io", TenantID: "tenant-a",
		Environment: domain.EnvironmentProduction, ActiveFlag: true,
	}))
	require.NoError(t, s.UpsertIngressBinding(ctx, domain.TenantIngressBinding{
		IngressIdentifier: "b.zoiko.io", TenantID: "tenant-b",
		Environment: domain.EnvironmentProduction, ActiveFlag: true,
	}))

	n, err := s.TouchIngressBindings(ctx, "tenant-a", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Still resolvable — stale, not gone.
	got, err := s.FindIngressBinding(ctx, "a.zoiko.io")
	require.NoError(t, err)
	require.NotNil(t, got, "a refreshed binding must still serve traffic")
	assert.True(t, got.RefreshedAt.Year() < 2000, "refreshed_at should be reset to the epoch")

	// The other tenant is untouched.
	other, err := s.FindIngressBinding(ctx, "b.zoiko.io")
	require.NoError(t, err)
	assert.True(t, other.RefreshedAt.Year() > 2000)
}

// ── Support contexts ─────────────────────────────────────────────────────────

func supportFor(id, tenantID, eng, approver string, ttl time.Duration) domain.SupportContext {
	now := time.Now().UTC()
	return domain.SupportContext{
		SupportContextID:    id,
		TenantID:            tenantID,
		SupportPrincipalID:  eng,
		ReasonCode:          domain.SupportReasonIncident,
		Justification:       "Investigating estate-wide envelope rejections reported at 09:15.",
		TicketRef:           "INC-1",
		ApproverPrincipalID: approver,
		GrantedAt:           now,
		ExpiresAt:           now.Add(ttl),
		EvidenceID:          "ev-" + id,
		CorrelationID:       "corr-" + id,
	}
}

func TestSupportContext_InsertAndFindLive(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	sc := supportFor("sup-1", "tenant-a", "eng-1", "lead-9", time.Hour)
	require.NoError(t, s.InsertSupportContextWithEvent(ctx, sc, outbox.Record{
		EventID: "evt-sup-1", EventType: "identity.support_context.attached",
		TenantID: "tenant-a", PartitionKey: "sup-1", Payload: []byte(`{}`),
	}))

	live, err := s.FindLiveSupportContext(ctx, "eng-1", "tenant-a", time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, "sup-1", live.SupportContextID)

	var events int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_outbox WHERE event_id='evt-sup-1'`).Scan(&events))
	assert.Equal(t, 1, events, "a privileged elevation the security team was never told about is the failure to prevent")
}

// TestSupportContext_SelfApprovalIsRefusedBySchema proves the control holds
// below the handler, for a future caller that reaches the store directly.
func TestSupportContext_SelfApprovalIsRefusedBySchema(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	sc := supportFor("sup-2", "tenant-a", "eng-1", "eng-1", time.Hour)
	err := s.InsertSupportContextWithEvent(ctx, sc, outbox.Record{})
	require.ErrorIs(t, err, domain.ErrSupportSelfApproval)
}

func TestSupportContext_ExpiredGrantIsNotLive(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	sc := supportFor("sup-3", "tenant-a", "eng-1", "lead-9", time.Hour)
	sc.GrantedAt = time.Now().UTC().Add(-2 * time.Hour)
	sc.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	require.NoError(t, s.InsertSupportContextWithEvent(ctx, sc, outbox.Record{}))

	live, err := s.FindLiveSupportContext(ctx, "eng-1", "tenant-a", time.Now().UTC())
	require.NoError(t, err)
	assert.Nil(t, live, "expiry is a property of the record — no sweep has to run")
}

func TestSupportContext_RevokeIsIdempotentAndKeepsTheFirstReason(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	require.NoError(t, s.InsertSupportContextWithEvent(ctx,
		supportFor("sup-4", "tenant-a", "eng-1", "lead-9", time.Hour), outbox.Record{}))

	changed, err := s.RevokeSupportContextWithEvent(ctx, "sup-4", "tenant-a", "incident closed", now, outbox.Record{})
	require.NoError(t, err)
	assert.True(t, changed)

	changed2, err := s.RevokeSupportContextWithEvent(ctx, "sup-4", "tenant-a", "a different reason", now, outbox.Record{})
	require.NoError(t, err)
	assert.False(t, changed2, "a second revocation is a no-op, not an overwrite")

	got, err := s.FindSupportContext(ctx, "sup-4", "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, got.RevocationReason)
	assert.Equal(t, "incident closed", *got.RevocationReason,
		"the first reason stands — it is the part an investigation reads")
}

func TestSupportContext_ForeignTenantIsInvisible(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	require.NoError(t, s.InsertSupportContextWithEvent(ctx,
		supportFor("sup-5", "tenant-a", "eng-1", "lead-9", time.Hour), outbox.Record{}))

	got, err := s.FindSupportContext(ctx, "sup-5", "tenant-b")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSupportContext_ReconciliationFindsEndedAndUnreviewed(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	ended := supportFor("sup-6", "tenant-a", "eng-1", "lead-9", time.Hour)
	ended.GrantedAt = now.Add(-2 * time.Hour)
	ended.ExpiresAt = now.Add(-time.Hour)
	require.NoError(t, s.InsertSupportContextWithEvent(ctx, ended, outbox.Record{}))

	stillLive := supportFor("sup-7", "tenant-a", "eng-2", "lead-9", time.Hour)
	require.NoError(t, s.InsertSupportContextWithEvent(ctx, stillLive, outbox.Record{}))

	pending, err := s.FindUnreviewedExpiredSupportContexts(ctx, "tenant-a", now, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "sup-6", pending[0].SupportContextID)

	require.NoError(t, s.MarkSupportContextReviewed(ctx, "sup-6", "tenant-a", "security-1", now))

	after, err := s.FindUnreviewedExpiredSupportContexts(ctx, "tenant-a", now, 10)
	require.NoError(t, err)
	assert.Empty(t, after)
}

// ── Legal hold projection ────────────────────────────────────────────────────

func TestLegalHold_TenantWideBlocksEveryPrincipal(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	require.NoError(t, s.UpsertLegalHold(ctx, domain.LegalHold{
		HoldID: "h-1", TenantID: "tenant-a", MatterRef: "M-1",
		IssuedAt: now, LastEventID: "e-1", LastEventAt: now,
	}))

	// A hold naming no principal covers everyone. Getting this backwards is
	// what deletes evidence during litigation.
	for _, p := range []string{"p-1", "p-2", "anybody"} {
		held, err := s.HasActiveLegalHold(ctx, "tenant-a", p)
		require.NoError(t, err)
		assert.True(t, held, "tenant-wide hold must cover %s", p)
	}

	other, err := s.HasActiveLegalHold(ctx, "tenant-b", "p-1")
	require.NoError(t, err)
	assert.False(t, other, "a hold must not leak across tenants")
}

func TestLegalHold_PrincipalScopedBlocksOnlyItsSubject(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()
	pid := "p-held"

	require.NoError(t, s.UpsertLegalHold(ctx, domain.LegalHold{
		HoldID: "h-2", TenantID: "tenant-a", MatterRef: "M-2", PrincipalID: &pid,
		IssuedAt: now, LastEventID: "e-2", LastEventAt: now,
	}))

	held, err := s.HasActiveLegalHold(ctx, "tenant-a", "p-held")
	require.NoError(t, err)
	assert.True(t, held)

	free, err := s.HasActiveLegalHold(ctx, "tenant-a", "p-free")
	require.NoError(t, err)
	assert.False(t, free)
}

func TestLegalHold_ReleaseUnblocks(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	require.NoError(t, s.UpsertLegalHold(ctx, domain.LegalHold{
		HoldID: "h-3", TenantID: "tenant-a", MatterRef: "M-3",
		IssuedAt: now, LastEventID: "e-3", LastEventAt: now,
	}))
	require.NoError(t, s.ReleaseLegalHold(ctx, "h-3", "tenant-a", "e-4", now.Add(time.Minute)))

	held, err := s.HasActiveLegalHold(ctx, "tenant-a", "p-1")
	require.NoError(t, err)
	assert.False(t, held)
}

// TestLegalHold_StaleEventCannotResurrectAReleasedHold.
//
// A redelivered LegalHoldIssued arriving after the release must be ignored.
// At-least-once delivery makes this a real ordering, not a hypothetical one.
func TestLegalHold_StaleEventCannotResurrectAReleasedHold(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	require.NoError(t, s.UpsertLegalHold(ctx, domain.LegalHold{
		HoldID: "h-4", TenantID: "tenant-a", MatterRef: "M-4",
		IssuedAt: now, LastEventID: "e-5", LastEventAt: now,
	}))
	require.NoError(t, s.ReleaseLegalHold(ctx, "h-4", "tenant-a", "e-6", now.Add(time.Minute)))

	// Replayed original issue event, older than the release.
	require.NoError(t, s.UpsertLegalHold(ctx, domain.LegalHold{
		HoldID: "h-4", TenantID: "tenant-a", MatterRef: "M-4",
		IssuedAt: now, LastEventID: "e-5", LastEventAt: now,
	}))

	held, err := s.HasActiveLegalHold(ctx, "tenant-a", "p-1")
	require.NoError(t, err)
	assert.False(t, held, "an out-of-order redelivery must not resurrect a released hold")
}

// ── Retention ────────────────────────────────────────────────────────────────

func TestDisposition_FindsOnlyDueAndUndisposed(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")

	due := sessionFor("sc-due", "p-1", "tenant-a")
	past := now.Add(-time.Hour)
	due.DispositionDueAt = &past
	require.NoError(t, s.InsertSessionContext(ctx, due))

	notDue := sessionFor("sc-notdue", "p-1", "tenant-a")
	future := now.Add(24 * time.Hour)
	notDue.DispositionDueAt = &future
	require.NoError(t, s.InsertSessionContext(ctx, notDue))

	// No due date at all reads as "never due" — the safe default.
	never := sessionFor("sc-never", "p-1", "tenant-a")
	require.NoError(t, s.InsertSessionContext(ctx, never))

	rows, err := s.FindDisposableSessions(ctx, "tenant-a", now, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "sc-due", rows[0].SessionContextID)
}

// TestDisposition_RedactsRatherThanDeletes.
//
// The row is the evidence that a session existed; deleting it would destroy
// the audit trail the retention policy exists to bound. What the period
// governs is the PERSONAL data.
func TestDisposition_RedactsRatherThanDeletes(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")
	sc := sessionFor("sc-d", "p-1", "tenant-a")
	past := now.Add(-time.Hour)
	sc.DispositionDueAt = &past
	sc.AdaptiveRiskScore = 42
	require.NoError(t, s.InsertSessionContext(ctx, sc))

	n, err := s.DisposeSessions(ctx, "tenant-a", []string{"sc-d"}, now)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, err := s.FindSessionContext(ctx, "sc-d", "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, got, "the row must survive — it is the evidence a session existed")

	assert.NotNil(t, got.DisposedAt)
	assert.Equal(t, "DISPOSED", got.CorrelationID)
	assert.Equal(t, "DISPOSED", got.IngressSource)
	assert.Zero(t, got.AdaptiveRiskScore)
	// The skeleton of the decision is intact.
	assert.Equal(t, "p-1", got.PrincipalID)
	assert.Equal(t, "jti-sc-d", got.EnvelopeJWTJTI)
}

func TestDisposition_IsIdempotent(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	insertPrincipal(t, ctx, pool, "p-1", "tenant-a", "idp|1", "ACTIVE")
	sc := sessionFor("sc-i", "p-1", "tenant-a")
	past := now.Add(-time.Hour)
	sc.DispositionDueAt = &past
	require.NoError(t, s.InsertSessionContext(ctx, sc))

	_, err := s.DisposeSessions(ctx, "tenant-a", []string{"sc-i"}, now)
	require.NoError(t, err)

	n, err := s.DisposeSessions(ctx, "tenant-a", []string{"sc-i"}, now)
	require.NoError(t, err)
	assert.Zero(t, n, "an already-disposed row must not be re-disposed")
}

// TestTenantsWithDisposableRecords_UsesTheSweepCapability.
//
// The sweep must enumerate tenants, which no tenant-scoped connection can do.
// This exercises the app.retention_sweep capability migration 000007 grants.
func TestTenantsWithDisposableRecords_SeesEveryTenant(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()
	past := now.Add(-time.Hour)

	insertPrincipal(t, ctx, pool, "p-a", "tenant-a", "idp|a", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-b", "tenant-b", "idp|b", "ACTIVE")

	a := sessionFor("sc-a", "p-a", "tenant-a")
	a.DispositionDueAt = &past
	require.NoError(t, s.InsertSessionContext(ctx, a))

	b := sessionFor("sc-b", "p-b", "tenant-b")
	b.DispositionDueAt = &past
	require.NoError(t, s.InsertSessionContext(ctx, b))

	tenants, err := s.TenantsWithDisposableRecords(ctx, now)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"tenant-a", "tenant-b"}, tenants,
		"a retention obligation that applies only to remembered tenants is not one")
}

func TestPurgePublishedOutbox_LeavesUndeliveredRows(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := store.New(pool, zap.NewNop())

	_, err := pool.Exec(ctx, `
		SELECT set_config('app.outbox_relay','true',false);
		INSERT INTO event_outbox (event_id, event_type, tenant_id, partition_key, payload, published_at)
		VALUES ('old','t','tenant-a','k','{}', NOW() - interval '30 days'),
		       ('recent','t','tenant-a','k','{}', NOW()),
		       ('pending','t','tenant-a','k','{}', NULL)`)
	require.NoError(t, err)

	n, err := s.PurgePublishedOutbox(ctx, time.Now().UTC().Add(-7*24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	var remaining int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM event_outbox WHERE event_id IN ('recent','pending')`).Scan(&remaining))
	assert.Equal(t, 2, remaining, "an undelivered event must never be purged")
}
