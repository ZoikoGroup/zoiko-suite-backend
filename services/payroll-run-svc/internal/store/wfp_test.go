package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// These run against a real Postgres (TEST_DATABASE_URL). The embedded superuser
// bypasses RLS, so tenant-isolation here proves the explicit tenant_id predicate;
// the DB-level guards (triggers, CHECKs) are exercised with direct SQL.

func wfpTenant() (string, context.Context) {
	id := uuid.NewString()
	return id, middleware.WithTenant(context.Background(), id)
}

func evt(tenant, typ, obj string) domain.OutboxEvent {
	return domain.OutboxEvent{TenantID: tenant, ObjectID: obj, EventType: typ, Payload: []byte(`{"event_type":"` + typ + `"}`)}
}

func newProfile(source string, caps ...string) *domain.SourceTrustProfile {
	return &domain.SourceTrustProfile{
		ProfileID: uuid.NewString(), SourceSystemID: source, SourceKind: domain.SourceKindZoikoPayroll,
		AllowedCapabilities: caps, CreatedBy: "p-1",
	}
}

func newManifest(source, jur, version, digestSeed, from string, to *string) *domain.PayrollRuleManifest {
	return &domain.PayrollRuleManifest{
		ManifestID: uuid.NewString(), SourceSystemID: source, JurisdictionID: jur, ManifestVersion: version,
		ManifestDigest: strings.Repeat(digestSeed, 64), EffectiveFrom: from, EffectiveTo: to,
		EvidenceRef: "doc://" + version, RegisteredBy: "p-1",
	}
}

func newRunRef(tenant, source, runID string, version int, manifestID, period string) *domain.PayrollRunRef {
	req := domain.RegisterPayrollRunRefRequest{
		LegalEntityID: "le-1", SourceSystemID: source, SourceRunID: runID, SourceRunVersion: version,
		RunKind: domain.RunKindRegular, PayrollPeriodID: period, PayDate: "2026-09-30", RuleManifestID: manifestID,
	}
	return &domain.PayrollRunRef{
		RunRefID: uuid.NewString(), LegalEntityID: req.LegalEntityID, SourceSystemID: source, SourceRunID: runID,
		SourceRunVersion: version, RunKind: req.RunKind, PayrollPeriodID: period, PayDate: req.PayDate,
		RuleManifestID: manifestID, IdentityFingerprint: domain.RunIdentityFingerprint(tenant, req),
		CorrelationID: "corr-" + runID, CreatedBy: "p-1",
	}
}

func outboxCount(t *testing.T, pool *pgxpool.Pool, tenant, typ string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wfp_outbox WHERE tenant_id = $1 AND event_type = $2`, tenant, typ).Scan(&n))
	return n
}

func TestWFP_TrustProfile_LifecycleAndConcurrency(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mk := func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return evt(tenant, domain.EventSourceTrustProfileChanged, p.ProfileID), nil
	}

	p := newProfile("zoiko-payroll", domain.CapPayrollResult, domain.CapPayrollInput)
	created, err := s.CreateSourceTrustProfile(ctx, p, mk)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, domain.TrustPendingCertification, p.Status)
	assert.Equal(t, 1, p.Version)
	assert.False(t, p.Allows(domain.CapPayrollResult), "an uncertified source is not trusted")

	// Replay with the same content returns the same profile and emits nothing new.
	replay := newProfile("zoiko-payroll", domain.CapPayrollInput, domain.CapPayrollResult)
	created, err = s.CreateSourceTrustProfile(ctx, replay, mk)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, p.ProfileID, replay.ProfileID)
	assert.Equal(t, 1, outboxCount(t, pool, tenant, domain.EventSourceTrustProfileChanged))

	// Different content for the same source is a conflict, never an overwrite.
	_, err = s.CreateSourceTrustProfile(ctx, newProfile("zoiko-payroll", domain.CapTimeLeave), mk)
	assert.ErrorIs(t, err, domain.ErrTrustProfileConflict)

	// Activation needs certification evidence.
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 1, domain.TrustActive, "", mk)
	assert.ErrorIs(t, err, domain.ErrCertificationRequired)

	// A stale expected_version cannot overwrite.
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 99, domain.TrustActive, "cert-1", mk)
	assert.ErrorIs(t, err, domain.ErrVersionConflict)

	active, err := s.TransitionSourceTrustProfile(ctx, p.ProfileID, 1, domain.TrustActive, "cert-1", mk)
	require.NoError(t, err)
	assert.Equal(t, 2, active.Version)
	assert.True(t, active.Allows(domain.CapPayrollResult))
	require.NotNil(t, active.CertifiedAt)

	// Two writers racing on version 2: exactly one wins.
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 2, domain.TrustSuspended, "", mk)
	require.NoError(t, err)
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 2, domain.TrustRetired, "", mk)
	assert.ErrorIs(t, err, domain.ErrVersionConflict)

	// Reinstating a suspended source reuses its certification; RETIRED is terminal.
	back, err := s.TransitionSourceTrustProfile(ctx, p.ProfileID, 3, domain.TrustActive, "", mk)
	require.NoError(t, err)
	require.NotNil(t, back.CertificationRef)
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 4, domain.TrustRetired, "", mk)
	require.NoError(t, err)
	_, err = s.TransitionSourceTrustProfile(ctx, p.ProfileID, 5, domain.TrustActive, "cert-2", mk)
	assert.ErrorIs(t, err, domain.ErrInvalidTrustTransition)
}

func TestWFP_TrustProfile_ActiveWithoutCertificationIsUnrepresentable(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	p := newProfile("ext-provider", domain.CapPayrollResult)
	_, err := s.CreateSourceTrustProfile(ctx, p, nil)
	require.NoError(t, err)

	_, err = pool.Exec(context.Background(),
		`UPDATE wfp_source_trust_profiles SET status = 'ACTIVE' WHERE tenant_id = $1`, tenant)
	assert.Error(t, err, "the CHECK must refuse ACTIVE with no certification evidence")
}

func TestWFP_RuleManifest_AppendOnlyAndAsOfResolution(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mk := func(m *domain.PayrollRuleManifest) (domain.OutboxEvent, error) {
		return evt(tenant, domain.EventRuleManifestRegistered, m.ManifestID), nil
	}

	cutover := "2026-04-06"
	old := newManifest("zp", "GB", "2025-26.4", "a", "2025-04-06", &cutover)
	cur := newManifest("zp", "GB", "2026-27.1", "b", "2026-04-06", nil)
	for _, m := range []*domain.PayrollRuleManifest{old, cur} {
		created, err := s.RegisterRuleManifest(ctx, m, mk)
		require.NoError(t, err)
		assert.True(t, created)
	}

	// Idempotent re-registration returns the stored row and emits no second event.
	again := newManifest("zp", "GB", "2026-27.1", "b", "2026-04-06", nil)
	created, err := s.RegisterRuleManifest(ctx, again, mk)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, cur.ManifestID, again.ManifestID)
	assert.Equal(t, 2, outboxCount(t, pool, tenant, domain.EventRuleManifestRegistered))

	// A historical pay date resolves to the rules in force THEN, never "current".
	got, err := s.ResolveRuleManifest(ctx, "zp", "GB", "2026-03-31")
	require.NoError(t, err)
	assert.Equal(t, "2025-26.4", got.ManifestVersion)
	got, err = s.ResolveRuleManifest(ctx, "zp", "GB", "2026-04-06")
	require.NoError(t, err)
	assert.Equal(t, "2026-27.1", got.ManifestVersion)

	// No manifest in force fails closed.
	_, err = s.ResolveRuleManifest(ctx, "zp", "GB", "2024-01-01")
	assert.ErrorIs(t, err, domain.ErrManifestNotFound)
	_, err = s.ResolveRuleManifest(ctx, "zp", "US", "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrManifestNotFound)

	// Evidence is append-only at the database.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_payroll_rule_manifests SET evidence_ref = 'x' WHERE tenant_id = $1`, tenant)
	assert.Error(t, err)
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_payroll_rule_manifests WHERE tenant_id = $1`, tenant)
	assert.Error(t, err)
}

func TestWFP_RunRef_IdentityVersioningAndImmutability(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()
	mkRef := func(r *domain.PayrollRunRef) (domain.OutboxEvent, error) {
		return evt(tenant, domain.EventPayrollResultReceived, r.RunRefID), nil
	}

	m1 := newManifest("zp", "GB", "v1", "a", "2026-01-01", nil)
	m2 := newManifest("zp", "GB", "v2", "b", "2026-01-01", nil)
	for _, m := range []*domain.PayrollRuleManifest{m1, m2} {
		_, err := s.RegisterRuleManifest(ctx, m, nil)
		require.NoError(t, err)
	}

	v1 := newRunRef(tenant, "zp", "RUN-1", 1, m1.ManifestID, "2026-09")
	created, err := s.RegisterPayrollRunRef(ctx, v1, mkRef)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, domain.AcceptanceReceived, v1.AcceptanceState)
	assert.Nil(t, v1.SupersedesRunRefID)

	// Provider retry of the same run version: same identity, no new row or event.
	retry := newRunRef(tenant, "zp", "RUN-1", 1, m1.ManifestID, "2026-09")
	created, err = s.RegisterPayrollRunRef(ctx, retry, mkRef)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, v1.RunRefID, retry.RunRefID)
	assert.Equal(t, 1, outboxCount(t, pool, tenant, domain.EventPayrollResultReceived))

	// The same run version with a different manifest is a changed run reusing an
	// identity: refused. It must arrive as a new source_run_version.
	clash := newRunRef(tenant, "zp", "RUN-1", 1, m2.ManifestID, "2026-09")
	_, err = s.RegisterPayrollRunRef(ctx, clash, mkRef)
	assert.ErrorIs(t, err, domain.ErrRunIdentityConflict)

	// Version 3 cannot skip version 2.
	_, err = s.RegisterPayrollRunRef(ctx, newRunRef(tenant, "zp", "RUN-1", 3, m1.ManifestID, "2026-09"), mkRef)
	assert.ErrorIs(t, err, domain.ErrPriorVersionMissing)

	// Version 2 supersedes version 1 and carries the new manifest.
	v2 := newRunRef(tenant, "zp", "RUN-1", 2, m2.ManifestID, "2026-09")
	_, err = s.RegisterPayrollRunRef(ctx, v2, mkRef)
	require.NoError(t, err)
	require.NotNil(t, v2.SupersedesRunRefID)
	assert.Equal(t, v1.RunRefID, *v2.SupersedesRunRefID)

	// Parallel/off-cycle run in the same period is a distinct identity.
	other := newRunRef(tenant, "zp", "RUN-1-OFFCYCLE", 1, m1.ManifestID, "2026-09")
	created, err = s.RegisterPayrollRunRef(ctx, other, mkRef)
	require.NoError(t, err)
	assert.True(t, created)

	// Acceptance walks the edge table under expected_version.
	_, err = s.TransitionPayrollRunRef(ctx, v1.RunRefID, 1, domain.AcceptanceAccepted, "", nil)
	assert.ErrorIs(t, err, domain.ErrInvalidAcceptanceMove, "RECEIVED cannot jump to ACCEPTED")
	validating, err := s.TransitionPayrollRunRef(ctx, v1.RunRefID, 1, domain.AcceptanceValidating, "", nil)
	require.NoError(t, err)
	_, err = s.TransitionPayrollRunRef(ctx, v1.RunRefID, 1, domain.AcceptanceAccepted, "fp-1", nil)
	assert.ErrorIs(t, err, domain.ErrVersionConflict, "stale version")

	// Acceptance needs its evidence: an ACCEPTED result with the same fingerprint.
	_, err = s.TransitionPayrollRunRef(ctx, v1.RunRefID, validating.Version, domain.AcceptanceAccepted, "fp-1", nil)
	assert.Error(t, err, "no run version becomes ACCEPTED without its result")
	insertBareResult(t, pool, tenant, v1.RunRefID, "ACCEPTED", "fp-1")

	accepted, err := s.TransitionPayrollRunRef(ctx, v1.RunRefID, validating.Version, domain.AcceptanceAccepted, "fp-1", nil)
	require.NoError(t, err)
	require.NotNil(t, accepted.ResultFingerprint)
	assert.Equal(t, "fp-1", *accepted.ResultFingerprint)

	// An accepted result is immutable at the database, not just in Go.
	_, err = pool.Exec(context.Background(),
		`UPDATE wfp_payroll_run_refs SET acceptance_state = 'REJECTED' WHERE run_ref_id = $1`, v1.RunRefID)
	assert.Error(t, err, "ACCEPTED can only be superseded")
	_, err = pool.Exec(context.Background(),
		`UPDATE wfp_payroll_run_refs SET result_fingerprint = 'other' WHERE run_ref_id = $1`, v1.RunRefID)
	assert.Error(t, err, "result_fingerprint is set once")
	_, err = pool.Exec(context.Background(),
		`UPDATE wfp_payroll_run_refs SET pay_date = '2026-10-31' WHERE run_ref_id = $1`, v1.RunRefID)
	assert.Error(t, err, "identity columns never change")
	_, err = pool.Exec(context.Background(), `DELETE FROM wfp_payroll_run_refs WHERE run_ref_id = $1`, v1.RunRefID)
	assert.Error(t, err, "run refs are never deleted")

	// ...and supersession needs a correction chain (covered with WFP-04 below).
	_, err = s.TransitionPayrollRunRef(ctx, v1.RunRefID, accepted.Version, domain.AcceptanceSuperseded, "", nil)
	assert.Error(t, err, "SUPERSEDED only through a correction chain")

	refs, err := s.ListPayrollRunRefs(ctx, store.RunRefFilter{SourceSystemID: "zp", SourceRunID: "RUN-1"})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, 1, refs[0].SourceRunVersion)
	assert.Equal(t, 2, refs[1].SourceRunVersion)
}

func TestWFP_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := wfpTenant()
	_, ctxB := wfpTenant()

	p := newProfile("zp", domain.CapPayrollResult)
	_, err := s.CreateSourceTrustProfile(ctxA, p, nil)
	require.NoError(t, err)
	m := newManifest("zp", "GB", "v1", "a", "2026-01-01", nil)
	_, err = s.RegisterRuleManifest(ctxA, m, nil)
	require.NoError(t, err)
	ref := newRunRef(tenantA, "zp", "RUN-1", 1, m.ManifestID, "2026-09")
	_, err = s.RegisterPayrollRunRef(ctxA, ref, nil)
	require.NoError(t, err)

	_, err = s.GetSourceTrustProfile(ctxB, p.ProfileID)
	assert.ErrorIs(t, err, domain.ErrTrustProfileNotFound)
	_, err = s.GetSourceTrustProfileBySource(ctxB, "zp")
	assert.ErrorIs(t, err, domain.ErrTrustProfileNotFound)
	_, err = s.GetRuleManifest(ctxB, m.ManifestID)
	assert.ErrorIs(t, err, domain.ErrManifestNotFound)
	_, err = s.ResolveRuleManifest(ctxB, "zp", "GB", "2026-09-30")
	assert.ErrorIs(t, err, domain.ErrManifestNotFound)
	_, err = s.GetPayrollRunRef(ctxB, ref.RunRefID)
	assert.ErrorIs(t, err, domain.ErrRunRefNotFound)
	refs, err := s.ListPayrollRunRefs(ctxB, store.RunRefFilter{})
	require.NoError(t, err)
	assert.Empty(t, refs)

	// Tenant B may register the same source system name independently.
	created, err := s.CreateSourceTrustProfile(ctxB, newProfile("zp", domain.CapTimeLeave), nil)
	require.NoError(t, err)
	assert.True(t, created)

	// No tenant in context is refused outright.
	_, err = s.GetSourceTrustProfile(context.Background(), p.ProfileID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)
}

func TestWFP_Outbox_EventIsAtomicWithStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := wfpTenant()

	// A builder that fails rolls back the state change: no row without its event.
	boom := errors.New("cannot build event")
	p := newProfile("zp", domain.CapPayrollResult)
	_, err := s.CreateSourceTrustProfile(ctx, p, func(*domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return domain.OutboxEvent{}, boom
	})
	require.Error(t, err)
	_, err = s.GetSourceTrustProfileBySource(ctx, "zp")
	assert.ErrorIs(t, err, domain.ErrTrustProfileNotFound, "the profile must not exist without its event")

	// An event type outside the catalogue is refused (and rolls back).
	_, err = s.CreateSourceTrustProfile(ctx, newProfile("zp", domain.CapPayrollResult), func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return evt(tenant, "payroll_paid", p.ProfileID), nil
	})
	require.Error(t, err)
	_, err = s.GetSourceTrustProfileBySource(ctx, "zp")
	assert.ErrorIs(t, err, domain.ErrTrustProfileNotFound)

	// Committed change: event is queued, then drained exactly once.
	ok := newProfile("zp", domain.CapPayrollResult)
	_, err = s.CreateSourceTrustProfile(ctx, ok, func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return evt(tenant, domain.EventSourceTrustProfileChanged, p.ProfileID), nil
	})
	require.NoError(t, err)

	var got []store.OutboxRecord
	require.NoError(t, s.ClaimOutbox(ctx, 100, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			if strings.Contains(string(r.Body), domain.EventSourceTrustProfileChanged) {
				got = append(got, r)
			}
		}
		return nil
	}))
	require.NotEmpty(t, got)
	assert.Equal(t, ok.ProfileID, got[len(got)-1].Key, "the object id is the Kafka key, so one object's events stay ordered")

	var unpublished int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wfp_outbox WHERE tenant_id = $1 AND published_at IS NULL`, tenant).Scan(&unpublished))
	assert.Zero(t, unpublished, "claimed events are marked published")

	// A publish failure leaves the event queued for retry and counts the attempt.
	_, err = s.CreateSourceTrustProfile(ctx, newProfile("zp-2", domain.CapPayrollResult), func(p *domain.SourceTrustProfile) (domain.OutboxEvent, error) {
		return evt(tenant, domain.EventSourceTrustProfileChanged, p.ProfileID), nil
	})
	require.NoError(t, err)
	err = s.ClaimOutbox(ctx, 100, func([]store.OutboxRecord) error { return errors.New("broker down") })
	require.Error(t, err)
	var attempts int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT max(attempts) FROM wfp_outbox WHERE tenant_id = $1 AND published_at IS NULL`, tenant).Scan(&attempts))
	assert.Equal(t, 1, attempts)
}

// insertBareResult writes the minimum ACCEPTED/REJECTED result row for a run version
// that is being VALIDATED, so a test can exercise the run-reference guard alone.
func insertBareResult(t *testing.T, pool *pgxpool.Pool, tenant, runRefID, outcome, fingerprint string) {
	t.Helper()
	reasons := `[]`
	if outcome == "REJECTED" {
		reasons = `[{"code":"X"}]`
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO wfp_payroll_results (result_id, tenant_id, run_ref_id, legal_entity_id, outcome, rejection_reasons, currency,
			source_run_status, source_approval_ref, calculation_ref, worker_count, total_gross, total_taxable_pay,
			total_employee_deductions, total_employer_liabilities, total_net_pay, declared_totals, result_fingerprint,
			received_by, correlation_id)
		VALUES (gen_random_uuid(), $1, $2::uuid, 'le-1', $3, $4::jsonb, 'GBP', 'FINAL', 'approval', 'calc', 0, 0, 0, 0, 0, 0, '{}'::jsonb, $5, 'p', 'c')`,
		tenant, runRefID, outcome, reasons, fingerprint)
	require.NoError(t, err)
}
