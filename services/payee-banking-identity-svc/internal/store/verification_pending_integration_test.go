//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

// TestProposeDestination_LandsInVerificationPending is the direct, real-row
// proof for SPEC_DEVIATIONS A4: immediately after ProposeDestination
// commits — before VerifyDestination is ever called — the row's own status
// column is VERIFICATION_PENDING, read back in a fresh query, not inferred
// from the handler's response.
func TestProposeDestination_LandsInVerificationPending(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-vp-" + uuid.New().String()

	req := domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank of VP",
		AccountIdentifier: "GB29VPEND00000000000001", CountryCode: "GB", Currency: "GBP",
		PayeeName: "VP Test Payee", SourceType: domain.SourceSupplierPortal,
	}
	created, _, err := f.s.ProposeDestination(ctx, f.tenant, req, "proposer", "corr-vp", "idem-vp-"+uuid.New().String())
	if err != nil {
		t.Fatalf("ProposeDestination: %v", err)
	}
	if created.Status != domain.StatusVerificationPending {
		t.Fatalf("expected the freshly-returned row to be VERIFICATION_PENDING, got %s", created.Status)
	}

	// Re-read in a brand new query — not the same row object the INSERT's
	// RETURNING clause handed back — to prove the committed state, not just
	// an in-memory value.
	reread, err := f.s.FindDestination(ctx, created.DestinationID)
	if err != nil {
		t.Fatalf("FindDestination: %v", err)
	}
	if reread.Status != domain.StatusVerificationPending {
		t.Fatalf("expected the re-read row to be VERIFICATION_PENDING, got %s", reread.Status)
	}

	// From VERIFICATION_PENDING, VerifyDestination still reaches VERIFIED —
	// the state is a real resting point, not a dead end.
	verified, _, err := f.s.VerifyDestination(ctx, created.DestinationID,
		domain.VerifyDestinationRequest{VerificationMethod: "OUTBOUND_CALL_TO_SUPPLIER", VerificationEvidenceRef: "ev-vp-1"},
		"verifier", "idem-vp-verify-"+uuid.New().String())
	if err != nil {
		t.Fatalf("VerifyDestination from VERIFICATION_PENDING: %v", err)
	}
	if verified.Status != domain.StatusVerified {
		t.Fatalf("expected VERIFIED after verifying a VERIFICATION_PENDING row, got %s", verified.Status)
	}
}

// TestProposeDestination_ChangeRiskClassification is the store-level proof
// that a party's first destination is classified NEW_DESTINATION with no
// prior version, and a later destination for a party that already has an
// ACTIVE one is classified DESTINATION_CHANGE with PriorDestinationID
// pointing at it.
func TestProposeDestination_ChangeRiskClassification(t *testing.T) {
	f := newFixture(t)
	ctx := middleware.WithTenant(context.Background(), f.tenant)
	partyRef := "party-risk-" + uuid.New().String()

	first, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank One",
		AccountIdentifier: "GB29RISK00000000000001", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Risk Test Payee", SourceType: domain.SourceSupplierPortal,
	}, "proposer", "corr-risk-1", "idem-risk-propose-1-"+uuid.New().String())
	if err != nil {
		t.Fatalf("propose first: %v", err)
	}
	if first.ChangeRiskClassification != domain.ChangeRiskNewDestination {
		t.Fatalf("expected NEW_DESTINATION for a party with no prior destination at all, got %s", first.ChangeRiskClassification)
	}
	if first.PriorDestinationID != "" {
		t.Fatalf("expected no prior destination id, got %q", first.PriorDestinationID)
	}

	if _, _, err := f.s.VerifyDestination(ctx, first.DestinationID,
		domain.VerifyDestinationRequest{VerificationMethod: "OUTBOUND_CALL_TO_SUPPLIER", VerificationEvidenceRef: "ev-risk-1"},
		"verifier", "idem-risk-verify-1-"+uuid.New().String()); err != nil {
		t.Fatalf("verify first: %v", err)
	}
	if _, _, err := f.s.ApproveDestination(ctx, first.DestinationID, "approver", "corr-risk-1", "idem-risk-approve-1-"+uuid.New().String()); err != nil {
		t.Fatalf("approve first: %v", err)
	}
	if _, _, err := f.s.ActivateDestination(ctx, first.DestinationID, "activator", "corr-risk-1", "idem-risk-activate-1-"+uuid.New().String()); err != nil {
		t.Fatalf("activate first: %v", err)
	}

	second, _, err := f.s.ProposeDestination(ctx, f.tenant, domain.ProposeDestinationRequest{
		LegalEntityID: f.entity, PartyRef: partyRef, FinancialInstitution: "Bank Two",
		AccountIdentifier: "GB29RISK00000000000002", CountryCode: "GB", Currency: "GBP",
		PayeeName: "Risk Test Payee", SourceType: domain.SourceSupplierPortal,
	}, "proposer", "corr-risk-2", "idem-risk-propose-2-"+uuid.New().String())
	if err != nil {
		t.Fatalf("propose second: %v", err)
	}
	if second.ChangeRiskClassification != domain.ChangeRiskDestinationChange {
		t.Fatalf("expected DESTINATION_CHANGE once the party has an ACTIVE destination, got %s", second.ChangeRiskClassification)
	}
	if second.PriorDestinationID != first.DestinationID {
		t.Fatalf("expected PriorDestinationID %s, got %s", first.DestinationID, second.PriorDestinationID)
	}
}
