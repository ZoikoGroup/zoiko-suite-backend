package store_test

// ORG-02 / ORG-03 store integration tests, against a real Postgres.
//
// These cover what the service-level tests structurally cannot:
//
//   - that a domain event and the fact it attests COMMIT TOGETHER. An
//     in-memory stub can record that an event was produced; only a database
//     can show that a rolled-back business write takes its event with it.
//   - that the bitemporal interval arithmetic is right in SQL, not just in the
//     stub's re-implementation of it.
//   - that the CHECK constraints and unique indexes refuse what they should,
//     independently of the service-layer validation that also refuses it.
//     Two independent refusals is the point: one of them can be bypassed by a
//     future caller that does not go through the service.
//   - that FORCE row-level security is on and the policies isolate tenants.

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

// orgFixture is a tenant with one active legal entity and its version-1
// profile, in a store wired to a real outbox.
type orgFixture struct {
	s        *store.PgStore
	pool     *pgxpool.Pool
	tenantID string
	entityID string
	policyID string
	ctx      context.Context
}

func newORGFixture(t *testing.T) *orgFixture {
	t.Helper()
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	s.SetOutbox(outbox.NewStore(pool, zap.NewNop()))

	tenantID := uuid.New().String()
	policyID := uuid.New().String()
	entityID := uuid.New().String()
	tctx := domain.WithTenant(ctx, tenantID)

	require.NoError(t, s.CreateTenantWithDefaultResidencyPolicy(tctx,
		&domain.Tenant{
			TenantID: tenantID, TenantCode: "ORG-" + tenantID[:8],
			LegalName: "Fixture Co", Status: domain.TenantStatusActive,
			DefaultCurrencyCode: "USD", PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
			DefaultDataResidencyPolicyID: policyID,
			LifecycleState:               domain.TenantLifecycleActive,
			CreatedAt:                    time.Now().UTC(), CreatedByPrincipalID: "p-seed",
		},
		&domain.DataResidencyPolicy{
			DataResidencyPolicyID: policyID, TenantID: tenantID,
			PolicyName: "default", PolicyCode: "DEF-" + tenantID[:8],
			ResidencyMode:          domain.ResidencyModePreferredRegion,
			ConflictResolutionMode: domain.ConflictResolutionFailClosed,
			ActiveFlag:             true, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "p-seed",
		}))

	incorporated := time.Now().UTC().Add(-8760 * time.Hour) // a year ago
	regNo := "RC-" + tenantID[:8]
	require.NoError(t, s.CreateEntity(tctx, &domain.LegalEntity{
		LegalEntityID: entityID, TenantID: tenantID,
		EntityCode: "EC-" + entityID[:8], LegalName: "Original Name Ltd",
		RegistrationNumber:  &regNo,
		EntityType:          domain.EntityTypeSubsidiary,
		DefaultCurrencyCode: "USD", FiscalCalendarID: uuid.New().String(),
		EntityStatus: domain.EntityStatusActive, PrimaryJurisdictionID: uuid.New().String(),
		DataResidencyPolicyID: policyID,
		CreatedAt:             incorporated, CreatedByPrincipalID: "p-seed",
	}))
	require.NoError(t, s.CreateInitialProfileVersion(tctx, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: tenantID,
		LegalEntityID: entityID, LegalName: "Original Name Ltd",
		RegistrationNumber: &regNo,
		EffectiveFrom:      incorporated, CreatedByPrincipalID: "p-seed",
	}))

	return &orgFixture{s: s, pool: pool, tenantID: tenantID, entityID: entityID, policyID: policyID, ctx: tctx}
}

func testEvent(t *testing.T, eventType, tenantID string) *outbox.Record {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"marker": eventType})
	require.NoError(t, err)
	return &outbox.Record{
		EventID: uuid.New().String(), EventType: eventType,
		TenantID: tenantID, PartitionKey: tenantID, Payload: payload,
	}
}

func outboxCount(t *testing.T, f *orgFixture, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM event_outbox WHERE event_type = $1 AND tenant_id = $2`,
		eventType, f.tenantID).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// §9.2 — the event and the fact commit together
// ---------------------------------------------------------------------------

func TestOutbox_EventAndFactCommitTogether(t *testing.T) {
	f := newORGFixture(t)

	ev := testEvent(t, "tenant.suspended", f.tenantID)
	res, err := f.s.ExecuteTenantCommand(f.ctx, registry.TenantCommandParams{
		TenantID: f.tenantID, Command: domain.TenantCommandSuspend,
		TargetState:     domain.TenantLifecycleSuspended,
		AllowedFrom:     []domain.TenantLifecycleState{domain.TenantLifecycleActive},
		ExpectedVersion: 1, Reason: "incident", ActorID: "p-actor",
	}, ev)
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleActive, res.FromState)
	assert.Equal(t, int64(2), res.NewVersion)

	assert.Equal(t, 1, outboxCount(t, f, "tenant.suspended"),
		"the event must be in the outbox, written by the same transaction")
}

func TestOutbox_RefusedCommandLeavesNoEvent(t *testing.T) {
	f := newORGFixture(t)

	// Wrong expected_version: the UPDATE matches zero rows, so the whole
	// transaction — including the outbox insert — must roll back. This is the
	// property the outbox exists for and the one a stub cannot demonstrate.
	ev := testEvent(t, "tenant.suspended", f.tenantID)
	_, err := f.s.ExecuteTenantCommand(f.ctx, registry.TenantCommandParams{
		TenantID: f.tenantID, Command: domain.TenantCommandSuspend,
		TargetState:     domain.TenantLifecycleSuspended,
		AllowedFrom:     []domain.TenantLifecycleState{domain.TenantLifecycleActive},
		ExpectedVersion: 99, Reason: "incident", ActorID: "p-actor",
	}, ev)
	require.ErrorIs(t, err, registry.ErrConflict)

	assert.Equal(t, 0, outboxCount(t, f, "tenant.suspended"),
		"a refused command must not leave an event claiming it happened")

	// And the tenant is untouched.
	tn, err := f.s.GetTenantByID(f.ctx, f.tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.TenantLifecycleActive, tn.LifecycleState)
	assert.Equal(t, int64(1), tn.RecordVersion)
}

func TestExecuteTenantCommand_WritesLifecycleEvidence(t *testing.T) {
	f := newORGFixture(t)

	_, err := f.s.ExecuteTenantCommand(f.ctx, registry.TenantCommandParams{
		TenantID: f.tenantID, Command: domain.TenantCommandSuspend,
		TargetState:     domain.TenantLifecycleSuspended,
		AllowedFrom:     []domain.TenantLifecycleState{domain.TenantLifecycleActive},
		ExpectedVersion: 1, Reason: "sanctions screening hit", ActorID: "p-actor",
		CorrelationID: "corr-1",
	}, nil)
	require.NoError(t, err)

	history, err := f.s.ListTenantLifecycleHistory(f.ctx, f.tenantID)
	require.NoError(t, err)
	require.Len(t, history, 1)

	// The evidence must name the COMMAND, not merely the destination state:
	// that is what distinguishes a suspension from the reversal of a mistaken
	// activation.
	assert.Equal(t, domain.TenantCommandSuspend, history[0].CommandName)
	assert.Equal(t, "sanctions screening hit", history[0].Reason)
	assert.Equal(t, "p-actor", history[0].ActorPrincipalID)
	require.NotNil(t, history[0].FromState)
	assert.Equal(t, domain.TenantLifecycleActive, *history[0].FromState,
		"from_state must be the state moved AWAY from, not the new one")
}

// ---------------------------------------------------------------------------
// §8 NP6 / §4.3 — bitemporal profile versions
// ---------------------------------------------------------------------------

func TestAmendLegalProfile_HistoricalReadResolvesTheOriginalName(t *testing.T) {
	f := newORGFixture(t)

	before := time.Now().UTC().Add(-100 * time.Hour)
	renamedAt := time.Now().UTC().Add(-1 * time.Hour)

	newName := "Renamed Holdings Plc"
	_, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID,
		LegalEntityID: f.entityID, LegalName: newName,
		EffectiveFrom: renamedAt, ChangeReason: domain.ProfileChangeLegalNameChange,
		CreatedByPrincipalID: "p-actor",
	}, 1, nil)
	require.NoError(t, err)

	now, err := f.s.GetEntityProfileAsOf(f.ctx, f.entityID, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, now.Profile)
	assert.Equal(t, newName, now.Profile.LegalName)

	then, err := f.s.GetEntityProfileAsOf(f.ctx, f.entityID, before)
	require.NoError(t, err)
	require.NotNil(t, then.Profile)
	assert.Equal(t, "Original Name Ltd", then.Profile.LegalName,
		"a read as-of the date the financial history was booked must give the name it was booked under")

	// The superseded row is closed, not deleted.
	versions, err := f.s.ListEntityProfileVersions(f.ctx, f.entityID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	v1 := versions[1]
	require.NotNil(t, v1.EffectiveTo, "the superseded version must be closed")
	require.NotNil(t, v1.SupersededAt, "and marked as superseded in record time")
	assert.True(t, v1.EffectiveTo.Equal(renamedAt.UTC()) || v1.EffectiveTo.Sub(renamedAt).Abs() < time.Millisecond)

	// The denormalized projection on legal_entities moved with it.
	e, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	assert.Equal(t, newName, e.LegalName)
	assert.Equal(t, int64(2), e.RecordVersion)
}

func TestAmendLegalProfile_BackdatedAmendmentDoesNotChangeTheCurrentName(t *testing.T) {
	f := newORGFixture(t)

	// First a current rename, so there is a version in force now.
	current := "Current Name Ltd"
	_, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID,
		LegalEntityID: f.entityID, LegalName: current,
		EffectiveFrom: time.Now().UTC().Add(-2 * time.Hour),
		ChangeReason:  domain.ProfileChangeLegalNameChange, CreatedByPrincipalID: "p",
	}, 1, nil)
	require.NoError(t, err)

	// Now a LATE-ARRIVING correction, effective in a period that has already
	// closed. §9.2 requires as-of retrieval to be verified against exactly this.
	backdated := time.Now().UTC().Add(-5000 * time.Hour)
	_, err = f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID,
		LegalEntityID: f.entityID, LegalName: "Name We Learned About Late Ltd",
		EffectiveFrom: backdated,
		ChangeReason:  domain.ProfileChangeCorrection, CreatedByPrincipalID: "p",
	}, 2, nil)
	require.NoError(t, err)

	// The present must be unaffected: learning about an old filing does not
	// rename the company today.
	e, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	assert.Equal(t, current, e.LegalName,
		"a backdated amendment must not overwrite the entity's present-day identity")

	// But a read as-of that past period now gives the corrected name.
	then, err := f.s.GetEntityProfileAsOf(f.ctx, f.entityID, backdated.Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, then.Profile)
	assert.Equal(t, "Name We Learned About Late Ltd", then.Profile.LegalName)
}

func TestGetEntityProfileAsOf_BeforeIncorporationReturnsNoProfile(t *testing.T) {
	f := newORGFixture(t)

	out, err := f.s.GetEntityProfileAsOf(f.ctx, f.entityID, time.Now().UTC().Add(-100000*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, out, "the entity exists, so the read must not 404")
	assert.Nil(t, out.Profile, "it simply had no profile then — a true and useful answer")
}

func TestAmendLegalProfile_StaleExpectedVersionIsRefused(t *testing.T) {
	f := newORGFixture(t)

	name := "Should Not Apply Ltd"
	_, err := f.s.AmendLegalProfile(f.ctx, f.entityID, &domain.LegalEntityProfileVersion{
		ProfileVersionID: uuid.New().String(), TenantID: f.tenantID,
		LegalEntityID: f.entityID, LegalName: name,
		EffectiveFrom: time.Now().UTC(), ChangeReason: domain.ProfileChangeAmendment,
		CreatedByPrincipalID: "p",
	}, 42, nil)
	require.ErrorIs(t, err, registry.ErrConflict)

	e, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	assert.Equal(t, "Original Name Ltd", e.LegalName)
}

// ---------------------------------------------------------------------------
// §8 NP5 — the registry probe and the quarantine
// ---------------------------------------------------------------------------

func TestFindActiveEntityByRegistry_OnlyMatchesActiveInSameJurisdiction(t *testing.T) {
	f := newORGFixture(t)
	e, err := f.s.GetEntityByID(f.ctx, f.entityID)
	require.NoError(t, err)
	require.NotNil(t, e.RegistrationNumber)

	found, err := f.s.FindActiveEntityByRegistry(f.ctx, *e.RegistrationNumber, e.PrimaryJurisdictionID)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, f.entityID, found.LegalEntityID)

	// Different jurisdiction: not a collision.
	other, err := f.s.FindActiveEntityByRegistry(f.ctx, *e.RegistrationNumber, uuid.New().String())
	require.NoError(t, err)
	assert.Nil(t, other)

	// Dissolved: no longer blocks re-registration.
	_, _, err = f.s.TransitionEntityStatus(f.ctx, f.entityID, domain.EntityStatusDissolved,
		[]domain.EntityStatus{domain.EntityStatusActive}, "p", "c")
	require.NoError(t, err)
	gone, err := f.s.FindActiveEntityByRegistry(f.ctx, *e.RegistrationNumber, e.PrimaryJurisdictionID)
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestRegistryConflict_QuarantineRoundTripAndSingleResolution(t *testing.T) {
	f := newORGFixture(t)

	conflictID := uuid.New().String()
	require.NoError(t, f.s.RecordRegistryConflict(f.ctx, &domain.EntityRegistryConflict{
		ConflictID: conflictID, TenantID: f.tenantID,
		RegistrationNumber: "RC-DUP", JurisdictionID: uuid.New().String(),
		ExistingLegalEntityID: f.entityID,
		AttemptedPayload:      map[string]any{"legal_name": "Impostor Ltd"},
		DetectedByPrincipalID: "p-detect",
	}))

	open, err := f.s.ListRegistryConflicts(f.ctx, true)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "Impostor Ltd", open[0].AttemptedPayload["legal_name"],
		"the rejected claim must survive: it was never written anywhere else")

	require.NoError(t, f.s.ResolveRegistryConflict(f.ctx, conflictID,
		domain.RegistryConflictResolvedDistinct, "different registrars", "p-resolve"))

	// Resolving twice must not overwrite the first conclusion.
	require.ErrorIs(t, f.s.ResolveRegistryConflict(f.ctx, conflictID,
		domain.RegistryConflictDismissed, "second opinion", "p-other"), registry.ErrConflict)

	stillOpen, err := f.s.ListRegistryConflicts(f.ctx, true)
	require.NoError(t, err)
	assert.Empty(t, stillOpen)
}

// ---------------------------------------------------------------------------
// Constraints the database enforces independently of the service
// ---------------------------------------------------------------------------

func TestConstraints_DatabaseRefusesWhatTheServiceAlsoRefuses(t *testing.T) {
	f := newORGFixture(t)
	pool := f.pool
	ctx := context.Background()

	set := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", f.tenantID)
		return err
	}

	t.Run("lifecycle history rejects self-approval", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		require.NoError(t, set(tx))

		// The service refuses this too. Having the database refuse it as well
		// means a future caller that bypasses the service cannot record a
		// maker-checker approval that never happened.
		_, err = tx.Exec(ctx, `
			INSERT INTO tenant_lifecycle_history
				(lifecycle_event_id, tenant_id, to_state, command_name, reason,
				 actor_principal_id, approved_by_principal_id)
			VALUES (gen_random_uuid(), $1, 'OFFBOARDING', 'InitiateTermination', 'r', 'p-same', 'p-same')`,
			f.tenantID)
		require.Error(t, err, "approver must not equal the actor")
	})

	t.Run("open conflict cannot carry a resolver", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		require.NoError(t, set(tx))

		_, err = tx.Exec(ctx, `
			INSERT INTO entity_registry_conflicts
				(conflict_id, tenant_id, registration_number, jurisdiction_id,
				 existing_legal_entity_id, attempted_payload, status,
				 resolved_by_principal_id, detected_by_principal_id)
			VALUES (gen_random_uuid(), $1, 'RC', gen_random_uuid(), $2, '{}'::jsonb,
			        'OPEN', 'p-resolver', 'p-detect')`,
			f.tenantID, f.entityID)
		require.Error(t, err, "an OPEN conflict must not claim to have been resolved")
	})

	t.Run("hostname must be lowercase", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		require.NoError(t, set(tx))

		_, err = tx.Exec(ctx, `
			INSERT INTO tenant_host_bindings (host_binding_id, hostname, tenant_id, created_by_principal_id)
			VALUES (gen_random_uuid(), 'MixedCase.Example.COM', $1, 'p')`, f.tenantID)
		require.Error(t, err, "two casings of one hostname must not be two bindings")
	})

	t.Run("profile version interval must be ordered", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck
		require.NoError(t, set(tx))

		_, err = tx.Exec(ctx, `
			INSERT INTO legal_entity_profile_versions
				(profile_version_id, tenant_id, legal_entity_id, version_number,
				 legal_name, effective_from, effective_to, change_reason, created_by_principal_id)
			VALUES (gen_random_uuid(), $1, $2, 99, 'X', NOW(), NOW() - interval '1 day', 'AMENDMENT', 'p')`,
			f.tenantID, f.entityID)
		require.Error(t, err, "a version cannot stop being in force before it starts")
	})
}

func TestHostBindings_OneHostnameAndOnePrimaryPerTenant(t *testing.T) {
	f := newORGFixture(t)

	host := "acme-" + f.tenantID[:8] + ".example.com"
	require.NoError(t, f.s.BindTenantHost(f.ctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), Hostname: host,
		TenantID: f.tenantID, IsPrimary: true, CreatedByPrincipalID: "p",
	}))

	// Same hostname again — for this or any tenant — is a conflict, not a race.
	err := f.s.BindTenantHost(f.ctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), Hostname: host,
		TenantID: f.tenantID, CreatedByPrincipalID: "p",
	})
	require.ErrorIs(t, err, registry.ErrConflict)

	// Mixed case resolves to the same binding: the store lowercases on write.
	require.NoError(t, f.s.BindTenantHost(f.ctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), Hostname: "SECOND-" + f.tenantID[:8] + ".Example.COM",
		TenantID: f.tenantID, CreatedByPrincipalID: "p",
	}))
	got, err := f.s.ResolveTenantByHost(f.ctx, "second-"+f.tenantID[:8]+".example.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, f.tenantID, got.TenantID)

	// A second PRIMARY for the same tenant is refused by the partial unique index.
	err = f.s.BindTenantHost(f.ctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), Hostname: "third-" + f.tenantID[:8] + ".example.com",
		TenantID: f.tenantID, IsPrimary: true, CreatedByPrincipalID: "p",
	})
	require.ErrorIs(t, err, registry.ErrConflict)
}

func TestResolveTenantByHost_UnknownHostReturnsNothing(t *testing.T) {
	f := newORGFixture(t)
	got, err := f.s.ResolveTenantByHost(f.ctx, "not-bound-anywhere.example.com")
	require.NoError(t, err)
	assert.Nil(t, got, "an unknown host must not resolve to any tenant")
}

// ---------------------------------------------------------------------------
// Row-level security on the tables migration 000006 adds
// ---------------------------------------------------------------------------

func TestORGTables_ForceRowLevelSecurityIsOn(t *testing.T) {
	f := newORGFixture(t)
	pool := f.pool

	// backend-completion-tracker.md Priority 3 row 56. ENABLE exempts the table
	// OWNER from its own policies; FORCE does not. Asserted here rather than
	// assumed, because the migration is the only thing that sets it and a
	// later migration recreating a table would silently drop it.
	for _, table := range []string{
		"tenants", "legal_entities", "workspaces", "data_residency_policies",
		"entity_hierarchies", "entity_jurisdiction_assignments", "tax_identity_bundles",
		"legal_entity_profile_versions", "tenant_lifecycle_history",
		"entity_registry_conflicts", "event_outbox",
	} {
		var rls, forced bool
		require.NoError(t, pool.QueryRow(context.Background(), `
			SELECT relrowsecurity, relforcerowsecurity
			  FROM pg_class WHERE oid = $1::regclass`, table).Scan(&rls, &forced))
		assert.True(t, rls, "%s: row level security must be enabled", table)
		assert.True(t, forced, "%s: row level security must be FORCEd", table)
	}

	// tenant_host_bindings is deliberately without RLS — it is read BEFORE a
	// tenant is known, so a policy keyed on app.tenant_id would need the answer
	// as input. Asserted so the exception stays deliberate rather than becoming
	// an oversight nobody remembers.
	var hbRLS bool
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT relrowsecurity FROM pg_class WHERE oid = 'tenant_host_bindings'::regclass`).Scan(&hbRLS))
	assert.False(t, hbRLS, "tenant_host_bindings is intentionally exempt; see migration 000006")
}

func TestORGTables_ReadsAreScopedToTheCallersTenant(t *testing.T) {
	f := newORGFixture(t)

	// Everything written above belongs to f.tenantID. A context carrying a
	// different tenant must see none of it — the explicit WHERE tenant_id
	// predicate, which is this service's actual isolation guarantee.
	foreign := domain.WithTenant(context.Background(), uuid.New().String())

	history, err := f.s.ListTenantLifecycleHistory(foreign, f.tenantID)
	require.NoError(t, err)
	assert.Empty(t, history)

	versions, err := f.s.ListEntityProfileVersions(foreign, f.entityID)
	require.NoError(t, err)
	assert.Empty(t, versions, "another tenant must not read this entity's profile history")

	conflicts, err := f.s.ListRegistryConflicts(foreign, false)
	require.NoError(t, err)
	assert.Empty(t, conflicts)

	defaults, err := f.s.GetTenantDefaults(foreign, f.tenantID)
	require.NoError(t, err)
	assert.Nil(t, defaults)
}

// ---------------------------------------------------------------------------
// A request with no verified tenant
// ---------------------------------------------------------------------------

// TestUnscopedRead_IsNotFoundRatherThanAServerError pins the withRLS guard.
//
// Before it, an empty tenant reached the query, Postgres tried to cast ” to
// uuid, and the driver returned "invalid input syntax for type uuid" — which
// the handler mapped to 500. No data leaked, so the behaviour was safe, but it
// was reported as a SERVER FAULT: an unauthenticated probe looked exactly like
// an outage in the logs and in monitoring, and a real outage was
// indistinguishable from one.
//
// Asserted across several methods rather than one, because the guard lives in
// the shared wrapper and a future method that bypasses it would still be wrong.
func TestUnscopedRead_IsNotFoundRatherThanAServerError(t *testing.T) {
	f := newORGFixture(t)
	unscoped := context.Background() // no tenant in context at all

	checks := map[string]func() error{
		"GetEntityByID": func() error {
			_, err := f.s.GetEntityByID(unscoped, f.entityID)
			return err
		},

		"ListEntityProfileVersions": func() error {
			_, err := f.s.ListEntityProfileVersions(unscoped, f.entityID)
			return err
		},

		"ListRegistryConflicts": func() error {
			_, err := f.s.ListRegistryConflicts(unscoped, false)
			return err
		},
		"GetEntityProfileAsOf": func() error {
			_, err := f.s.GetEntityProfileAsOf(unscoped, f.entityID, time.Now().UTC())
			return err
		},
	}

	// GetTenantByID and ListTenantLifecycleHistory are deliberately NOT in this
	// set. They take the tenant as an explicit argument and fall back to it, so
	// they are scoped by what the caller passed rather than by context — which
	// is what lets ProvisionTenant read back a tenant that does not exist in
	// any context yet. Their cross-tenant guard is Service.assertTenantScope,
	// which refuses a path tenant that is not the caller's verified one and is
	// covered by TestORGReads_RefuseACrossTenantRequest in the registry tests.
	for name, call := range checks {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, registry.ErrNotFound,
				"an unscoped read must be not-found, not a database error")
			assert.NotContains(t, err.Error(), "invalid input syntax",
				"the uuid cast must not be reached at all")
		})
	}
}

// TestResolveTenantByHost_WorksWithoutATenant is the counterpart: the ONE read
// that must still work unscoped.
//
// Without this, the guard above would be indistinguishable from a rule that
// breaks tenant resolution — which is circular by construction, since resolving
// a hostname is how a caller learns which tenant it belongs to.
func TestResolveTenantByHost_WorksWithoutATenant(t *testing.T) {
	f := newORGFixture(t)

	host := "unscoped-" + f.tenantID[:8] + ".example.com"
	require.NoError(t, f.s.BindTenantHost(f.ctx, &domain.TenantHostBinding{
		HostBindingID: uuid.New().String(), Hostname: host,
		TenantID: f.tenantID, CreatedByPrincipalID: "p",
	}))

	got, err := f.s.ResolveTenantByHost(context.Background(), host)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, f.tenantID, got.TenantID)
}

// TestTenantResolveCapability_IsContained is the check that matters for a
// deliberate cross-tenant escape hatch.
//
// The assertion is not that app.tenant_resolve works — ResolveTenantByHost
// already proves that — but that it is CONTAINED: a connection holding it must
// be able to see tenant rows and nothing else, and must not be able to WRITE
// one. An escape hatch nobody has bounded is a bypass with a nicer name.
func TestTenantResolveCapability_IsContained(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()

	// A non-superuser, non-owner role, so the policies are actually load-bearing.
	// The pooled superuser used elsewhere in this file would bypass them.
	role := "resolve_probe_" + f.tenantID[:8]
	_, _ = f.pool.Exec(ctx, `DROP OWNED BY `+role)
	_, _ = f.pool.Exec(ctx, `DROP ROLE IF EXISTS `+role)
	if _, err := f.pool.Exec(ctx,
		`CREATE ROLE `+role+` NOSUPERUSER NOBYPASSRLS LOGIN PASSWORD 'probe'`); err != nil {
		t.Skipf("cannot create a probe role here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	for _, g := range []string{
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := f.pool.Exec(ctx, g); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	probe, err := pgxpool.New(ctx, probeDSN(t, role, "probe"))
	if err != nil {
		t.Skipf("cannot open a probe connection: %v", err)
	}
	t.Cleanup(probe.Close)

	tx, err := probe.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_resolve','true',true)")
	require.NoError(t, err)

	// It CAN see tenants — that is the whole point.
	var tenants int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&tenants))
	assert.Greater(t, tenants, 0, "the capability must make tenant rows visible")

	// It can see NOTHING else. Each of these is a table the capability must not
	// unlock; app.tenant_id is unset, so the ordinary policy matches no row.
	for _, table := range []string{
		"legal_entities", "workspaces", "data_residency_policies",
		"legal_entity_profile_versions", "tenant_lifecycle_history",
		"entity_registry_conflicts", "event_outbox",
	} {
		var n int
		err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		require.NoError(t, err, "%s", table)
		assert.Equal(t, 0, n, "%s must stay invisible under app.tenant_resolve", table)
	}

	// And it is READ-ONLY: the policy's WITH CHECK half does not honour it.
	_, err = tx.Exec(ctx, `
		UPDATE tenants SET legal_name = 'Rewritten By Probe' WHERE tenant_id = $1`, f.tenantID)
	if err == nil {
		var name string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT legal_name FROM tenants WHERE tenant_id = $1`, f.tenantID).Scan(&name))
		assert.NotEqual(t, "Rewritten By Probe", name,
			"the resolve capability must not permit a write")
	}
}

// probeDSN rebuilds TEST_DATABASE_URL with a different user and password.
func probeDSN(t *testing.T, user, password string) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil {
		t.Skipf("TEST_DATABASE_URL is not a URL: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}
