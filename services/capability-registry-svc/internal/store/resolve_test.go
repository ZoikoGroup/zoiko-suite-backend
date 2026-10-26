package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/capability-registry-svc/internal/domain"
)

// TestPgStoreCreateRelease_NonexistentCapabilityMapsToNotFound covers both
// database failure modes for SetReleaseState's capability_id: a well-formed
// UUID with no matching capability (23503) and a malformed UUID (22P02). Both
// must surface as domain.ErrCapabilityNotFound so the handler returns 404
// rather than a generic 500, matching CreateMarketRelease,
// CreateIntegrationCapability and CreateCapabilityClaim.
func TestPgStoreCreateRelease_NonexistentCapabilityMapsToNotFound(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)

	for _, capabilityID := range []string{uuid.NewString(), "not-a-uuid"} {
		t.Run(capabilityID, func(t *testing.T) {
			release := &domain.Release{
				ReleaseID: uuid.NewString(), CapabilityID: capabilityID, State: domain.ReleaseStateGA,
				EffectiveFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
				CreatedByPrincipalID: "resolve-test-principal",
			}
			err := store.CreateRelease(ctx, release, testIdempotencyClaim(
				"tenant-resolve", "SetReleaseState", "release-"+release.ReleaseID,
				"hash-"+release.ReleaseID, release.ReleaseID))
			if !errors.Is(err, domain.ErrCapabilityNotFound) {
				t.Fatalf("expected ErrCapabilityNotFound for capability_id %q, got %v", capabilityID, err)
			}
		})
	}
}

// TestPgStoreResolveCapability_LegalApprovalFailsClosed proves GOV-11's
// "cannot ... activate unapproved contracts" rule at the store layer: a GA
// market release with PENDING or REJECTED legal approval must not resolve to
// enabled. After an APPROVED release becomes the current row, resolution can
// succeed again.
func TestPgStoreResolveCapability_LegalApprovalFailsClosed(t *testing.T) {
	for _, legalStatus := range []string{"PENDING", "REJECTED"} {
		t.Run(legalStatus, func(t *testing.T) {
			_, pool := openIdempotencyTestPool(t)
			ctx := context.Background()
			store := NewPgStore(pool)

			capability := newTestCapability("RESOLVE_LEGAL_" + legalStatus)
			if err := store.CreateCapability(ctx, capability, testIdempotencyClaim(
				"tenant-resolve", "CreateCapability", "cap-"+legalStatus, "cap-"+legalStatus, capability.CapabilityID)); err != nil {
				t.Fatalf("create capability: %v", err)
			}

			now := time.Now().UTC()
			pending := &domain.MarketRelease{
				MarketReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID,
				MarketCode: "US", LegalApprovalStatus: legalStatus, State: domain.MarketReleaseGA,
				EffectiveFrom: now.Add(-time.Hour), CreatedAt: now.Add(-time.Minute),
				CreatedByPrincipalID: "resolve-test-principal",
			}
			if err := store.CreateMarketRelease(ctx, pending, testIdempotencyClaim(
				"tenant-resolve", "CreateMarketRelease", "market-"+legalStatus, "market-"+legalStatus, pending.MarketReleaseID)); err != nil {
				t.Fatalf("create market release: %v", err)
			}

			resolved, err := store.ResolveCapability(ctx, capability.CapabilityCode, "US")
			if err != nil {
				t.Fatalf("resolve capability: %v", err)
			}
			if resolved.Enabled || resolved.ReasonCode != "MARKET_BLOCKED" {
				t.Fatalf("expected %s legal approval to fail closed, got %+v", legalStatus, resolved)
			}

			approved := &domain.MarketRelease{
				MarketReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID,
				MarketCode: "US", LegalApprovalStatus: "APPROVED", State: domain.MarketReleaseGA,
				EffectiveFrom: now.Add(-time.Hour), CreatedAt: now,
				CreatedByPrincipalID: "resolve-test-principal",
			}
			if err := store.CreateMarketRelease(ctx, approved, testIdempotencyClaim(
				"tenant-resolve", "CreateMarketRelease", "approved-"+legalStatus, "approved-"+legalStatus, approved.MarketReleaseID)); err != nil {
				t.Fatalf("create approved market release: %v", err)
			}

			resolved, err = store.ResolveCapability(ctx, capability.CapabilityCode, "US")
			if err != nil {
				t.Fatalf("resolve capability after approval: %v", err)
			}
			if !resolved.Enabled || resolved.ReasonCode != "ENABLED" {
				t.Fatalf("expected approved market release to resolve enabled, got %+v", resolved)
			}
		})
	}
}

// TestPgStoreResolveCapability_ReleaseTieBreakerIsDeterministic covers the
// case where two append-only release rows share effective_from. Without a
// stable ORDER BY, which one is "current" depends on physical row order.
// created_at (then release_id) must break the tie so repeated resolutions
// agree.
func TestPgStoreResolveCapability_ReleaseTieBreakerIsDeterministic(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)

	capability := newTestCapability("RESOLVE_RELEASE_TIE")
	if err := store.CreateCapability(ctx, capability, testIdempotencyClaim(
		"tenant-resolve", "CreateCapability", "cap-tie", "cap-tie", capability.CapabilityID)); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	effectiveFrom := time.Now().UTC().Add(-time.Hour)
	older := &domain.Release{
		ReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID,
		State: domain.ReleaseStateGA, EffectiveFrom: effectiveFrom,
		CreatedAt: effectiveFrom.Add(time.Second), CreatedByPrincipalID: "resolve-test-principal",
	}
	if err := store.CreateRelease(ctx, older, testIdempotencyClaim(
		"tenant-resolve", "SetReleaseState", "release-tie-old", "release-tie-old", older.ReleaseID)); err != nil {
		t.Fatalf("create older release: %v", err)
	}
	newer := &domain.Release{
		ReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID,
		State: domain.ReleaseStateDisabled, EffectiveFrom: effectiveFrom,
		CreatedAt: effectiveFrom.Add(2 * time.Second), CreatedByPrincipalID: "resolve-test-principal",
	}
	if err := store.CreateRelease(ctx, newer, testIdempotencyClaim(
		"tenant-resolve", "SetReleaseState", "release-tie-new", "release-tie-new", newer.ReleaseID)); err != nil {
		t.Fatalf("create newer release: %v", err)
	}

	for i := 0; i < 10; i++ {
		resolved, err := store.ResolveCapability(ctx, capability.CapabilityCode, "")
		if err != nil {
			t.Fatalf("resolve capability: %v", err)
		}
		if resolved.Enabled || resolved.ReasonCode != "DISABLED" {
			t.Fatalf("resolution %d chose the wrong tied release, got %+v", i, resolved)
		}
	}
}
