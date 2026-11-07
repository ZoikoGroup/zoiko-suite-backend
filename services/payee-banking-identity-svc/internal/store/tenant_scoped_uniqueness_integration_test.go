//go:build integration

// Real-Postgres regression tests for SPEC_DEVIATIONS A11: the two uniqueness
// invariants ("destination candidate fingerprint detects duplicates" and
// "activation only one active version per party/scope") must be tenant-
// scoped, not global, so two different tenants that happen to share a
// party_ref/fingerprint or party_ref/scope value do not falsely conflict.
package store_test

import (
	"context"
	"testing"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

// TestTenantScopedUniqueness_SameFingerprintAcrossTenantsDoesNotConflict
// proves migration 000006: before it, two tenants proposing a destination
// for the same party_ref with the same institution/account/currency (hence
// the same fingerprint) would collide on the old
// uq_payee_destinations_fingerprint (party_ref, fingerprint) index, even
// though they are different tenants with no real relationship.
func TestTenantScopedUniqueness_SameFingerprintAcrossTenantsDoesNotConflict(t *testing.T) {
	fA := newFixture(t)
	fB := newFixture(t)
	partyRef := "party-shared-across-tenants"

	req := domain.ProposeDestinationRequest{
		LegalEntityID: "le-shared", PartyRef: partyRef, FinancialInstitution: "Shared Bank",
		AccountIdentifier: "GB29SHRD00000000000099", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Shared Payee", SourceType: domain.SourceManualEntry,
	}

	ctxA := middleware.WithTenant(context.Background(), fA.tenant)
	dA, replay, err := fA.s.ProposeDestination(ctxA, fA.tenant, req, "proposer-a", "corr-a", "idem-tenant-a")
	if err != nil || replay {
		t.Fatalf("tenant A propose: replay=%v err=%v", replay, err)
	}

	ctxB := middleware.WithTenant(context.Background(), fB.tenant)
	dB, replay, err := fB.s.ProposeDestination(ctxB, fB.tenant, req, "proposer-b", "corr-b", "idem-tenant-b")
	if err != nil {
		t.Fatalf("tenant B propose with the same party_ref/fingerprint as tenant A must not conflict: %v", err)
	}
	if replay {
		t.Fatalf("tenant B propose was wrongly treated as a replay of tenant A's destination")
	}
	if dA.DestinationID == dB.DestinationID {
		t.Fatalf("tenant A and tenant B must get distinct destinations, got the same id %s", dA.DestinationID)
	}
}

// TestTenantScopedUniqueness_SameActiveScopeAcrossTenantsDoesNotConflict
// proves the same fix for uq_payee_destinations_active_scope: two tenants
// each activating a destination for the same party_ref/scope must both
// succeed, since "one active version per party/scope" is a per-tenant
// invariant, not a platform-wide one.
func TestTenantScopedUniqueness_SameActiveScopeAcrossTenantsDoesNotConflict(t *testing.T) {
	fA := newFixture(t)
	fB := newFixture(t)
	partyRef := "party-active-scope-shared"

	activate := func(f *fixture, account string) *domain.PayeeDestination {
		t.Helper()
		ctx := middleware.WithTenant(context.Background(), f.tenant)
		req := domain.ProposeDestinationRequest{
			LegalEntityID: "le-shared", PartyRef: partyRef, FinancialInstitution: "Shared Bank",
			AccountIdentifier: account, CountryCode: "GB", Currency: "GBP",
			PayeeName: "Shared Payee", SourceType: domain.SourceManualEntry,
		}
		d, _, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-propose", "idem-"+f.tenant+"-propose")
		if err != nil {
			t.Fatalf("propose: %v", err)
		}
		if _, _, err := f.s.VerifyDestination(ctx, d.DestinationID, domain.VerifyDestinationRequest{
			VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "evidence",
		}, "verifier", "idem-"+f.tenant+"-verify"); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if _, _, err := f.s.ApproveDestination(ctx, d.DestinationID, "approver", "corr-approve", "idem-"+f.tenant+"-approve"); err != nil {
			t.Fatalf("approve: %v", err)
		}
		activated, _, err := f.s.ActivateDestination(ctx, d.DestinationID, "activator", "corr-activate", "idem-"+f.tenant+"-activate")
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		return activated
	}

	dA := activate(fA, "GB29ASCP00000000000001")
	dB := activate(fB, "GB29ASCP00000000000002")

	if dA.Status != domain.StatusActive || dB.Status != domain.StatusActive {
		t.Fatalf("expected both tenants' destinations to activate, got %s and %s", dA.Status, dB.Status)
	}
}
