package store_test

// SPEC_DEVIATIONS A5 (tolerance portion) / B15's deferred half, against a
// real Postgres: the tolerance MatchEntry compares against is now a
// governed, versioned object (intercompany_tolerance_policy_versions,
// migration 000011), not a magic literal. This proves:
//   - a tenant that has never published one resolves to the fixed
//     pre-governance 0.001, recorded as tolerance_version 0;
//   - a published version takes effect only from its own effective_from,
//     never retroactively from when it was published;
//   - a match attempt's recorded tolerance_version is never changed by a
//     later publish (append-only, same as every other evidence column on
//     intercompany_match_attempts);
//   - the explicit version number is itself the idempotency key: a repeat
//     publish for the same number is a conflict, not a silent second row;
//   - tenant isolation holds for the new table too.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

func TestPgStore_TolerancePolicy_VersionedNotRetroactive(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, store.WithEventRegion("test"))
	ctx := context.Background()
	tenantID := uuid.New().String()

	// No policy published yet: resolves to the fixed default, version 0.
	version0, tol0, err := s.ResolveTolerancePolicy(ctx, tenantID, time.Now().UTC())
	if err != nil || version0 != 0 || tol0 != "0.001" {
		t.Fatalf("default tolerance before any publish: version=%d tol=%s err=%v", version0, tol0, err)
	}

	now := time.Now().UTC()
	v1Effective := now.Add(-time.Hour)
	v1, err := s.PublishTolerancePolicy(ctx, tenantID, &domain.TolerancePolicyVersion{
		PolicyVersion: 1, Tolerance: "0.010", EffectiveFrom: v1Effective,
		Reason: "wider tolerance for a noisy feed", CreatedAt: now, CreatedByPrincipalID: "ops-oliver",
	})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	if v1.PolicyVersion != 1 || v1.TenantID != tenantID {
		t.Fatalf("published v1: %+v", v1)
	}

	// Effective now (now is after v1Effective): resolves to v1.
	version1, tol1, err := s.ResolveTolerancePolicy(ctx, tenantID, now)
	if err != nil || version1 != 1 {
		t.Fatalf("resolve after publishing v1: version=%d tol=%s err=%v", version1, tol1, err)
	}

	// Record a match attempt the way the handler does: resolve, then stamp
	// the attempt with exactly what was resolved.
	entry := newTestEntry(tenantID, uuid.New().String())
	tctx := svcmiddleware.WithTenant(ctx, tenantID)
	if _, err := s.CreateEntryWithAttempt(tctx, entry, nil, "test", ""); err != nil {
		t.Fatalf("create entry: %v", err)
	}
	targetJournalID := uuid.New().String()
	entry.TargetJournalID = &targetJournalID
	entry.MatchStatus = domain.MatchStatusMatched
	attempt := testAttempt(entry)
	attempt.Tolerance = tol1
	attempt.ToleranceVersion = version1
	if _, err := s.UpdateMatch(tctx, entry, attempt, uuid.NewString(), "test", ""); err != nil {
		t.Fatalf("record match attempt: %v", err)
	}

	recorded, err := s.ListMatchAttempts(tctx, entry.IntercompanyEntryID)
	if err != nil || len(recorded) != 1 || recorded[0].ToleranceVersion != 1 || recorded[0].Tolerance != tol1 {
		t.Fatalf("recorded attempt: %+v (err=%v)", recorded, err)
	}

	// Publish a SECOND, later version.
	v2Effective := now.Add(time.Hour)
	if _, err := s.PublishTolerancePolicy(ctx, tenantID, &domain.TolerancePolicyVersion{
		PolicyVersion: 2, Tolerance: "0.050", EffectiveFrom: v2Effective,
		Reason: "even wider, effective later", CreatedAt: now, CreatedByPrincipalID: "ops-oliver",
	}); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	// Resolving at the SAME instant as before (still before v2's
	// effective_from) still returns v1 — publishing v2 did not change what
	// "now" resolves to; a version takes effect only from its own
	// effective_from, never retroactively from when it was published.
	versionStillV1, _, err := s.ResolveTolerancePolicy(ctx, tenantID, now)
	if err != nil || versionStillV1 != 1 {
		t.Fatalf("resolve at the same `now` after publishing v2: version=%d err=%v", versionStillV1, err)
	}
	// Resolving after v2's effective_from returns v2.
	version2, tol2, err := s.ResolveTolerancePolicy(ctx, tenantID, v2Effective.Add(time.Minute))
	if err != nil || version2 != 2 || tol2 == tol1 {
		t.Fatalf("resolve after v2 effective: version=%d tol=%s err=%v", version2, tol2, err)
	}

	// The attempt recorded under v1 is unchanged by v2's publish: it is
	// append-only evidence, never retroactively reassigned to a version it
	// was never actually evaluated under.
	afterV2, err := s.ListMatchAttempts(tctx, entry.IntercompanyEntryID)
	if err != nil || len(afterV2) != 1 || afterV2[0].ToleranceVersion != 1 || afterV2[0].Tolerance != tol1 {
		t.Fatalf("recorded attempt after publishing v2: %+v (err=%v)", afterV2, err)
	}

	// A repeat publish of the same version number is a conflict, not a
	// silent second row: the caller-chosen version number is itself the
	// idempotency key for this governance action.
	if _, err := s.PublishTolerancePolicy(ctx, tenantID, &domain.TolerancePolicyVersion{
		PolicyVersion: 1, Tolerance: "0.020", EffectiveFrom: v1Effective,
		Reason: "replay/conflict", CreatedAt: now, CreatedByPrincipalID: "ops-oliver",
	}); !errors.Is(err, domain.ErrTolerancePolicyVersionExists) {
		t.Fatalf("republishing policy_version 1 was not refused: %v", err)
	}

	// Another tenant never sees this tenant's tolerance policy (RLS holds
	// for this new table too).
	otherTenant := uuid.New().String()
	otherVersion, otherTol, err := s.ResolveTolerancePolicy(ctx, otherTenant, v2Effective.Add(time.Minute))
	if err != nil || otherVersion != 0 || otherTol != "0.001" {
		t.Fatalf("a different tenant saw this tenant's tolerance policy: version=%d tol=%s err=%v", otherVersion, otherTol, err)
	}
}
