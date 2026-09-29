package legalhold_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/legalhold"
	"zoiko.io/contract/types"
)

func TestLegalHoldScenarios(t *testing.T) {
	engine := legalhold.NewEngine()
	tenantID := types.MustNewV7()
	now := time.Now().UTC()

	holdID := types.MustNewV7()
	hold := &legalhold.LegalHold{
		HoldID:               holdID,
		TenantID:             tenantID,
		HoldMatterCode:       "SEC_INVESTIGATION_2026",
		AuthorityDescription: "SEC Formal Order of Investigation",
		IssuedBy:             "usr_general_counsel",
		IssuedAt:             now.AddDate(0, -1, 0),
		Status:               legalhold.HoldStatusActive,
	}

	scopeV1 := &legalhold.LegalHoldScope{
		ScopeID:               types.MustNewV7(),
		TenantID:              tenantID,
		HoldID:                holdID,
		Version:               1,
		TargetEntityTypes:     []string{"SALES_INVOICE", "JOURNAL_LINE"},
		CustodianPrincipalIDs: []string{"usr_cfo", "usr_controller"},
		IsProspective:         true, // Captures future records
		CreatedAt:             now.AddDate(0, -1, 0),
		CreatedBy:             "usr_legal_team",
	}

	existingRecord := legalhold.RecordCandidate{
		RecordID:    types.MustNewV7(),
		TenantID:    tenantID,
		EntityType:  "SALES_INVOICE",
		CustodianID: "usr_controller",
		RecordDate:  now.AddDate(0, -2, 0),
	}

	t.Run("NP-13: Retention date reached while legal hold active prohibits destructive disposition", func(t *testing.T) {
		err := engine.AssertDispositionPermitted(existingRecord, []legalhold.LegalHold{*hold}, []legalhold.LegalHoldScope{*scopeV1})
		if !errors.Is(err, legalhold.ErrHoldPreemptsDisposition) {
			t.Fatalf("expected ErrHoldPreemptsDisposition, got: %v", err)
		}
	})

	t.Run("NP-14: Hold scope expanded creates new version", func(t *testing.T) {
		scopeV2, err := engine.AmendScope(scopeV1, []string{"SALES_INVOICE", "JOURNAL_LINE", "BANK_TRANSACTION"}, scopeV1.CustodianPrincipalIDs, true, "usr_legal_lead", now)
		if err != nil {
			t.Fatalf("failed to amend scope: %v", err)
		}
		if scopeV2.Version != 2 {
			t.Fatalf("expected scope version 2, got %d", scopeV2.Version)
		}
		if len(scopeV2.TargetEntityTypes) != 3 {
			t.Fatalf("expected 3 target entity types in V2")
		}
	})

	t.Run("NP-15: Prospective hold rule automatically captures matching records created after issuance", func(t *testing.T) {
		newRecord := legalhold.RecordCandidate{
			RecordID:    types.MustNewV7(),
			TenantID:    tenantID,
			EntityType:  "SALES_INVOICE",
			CustodianID: "usr_controller",
			RecordDate:  now.AddDate(0, 0, 5), // Created after hold issuance
		}

		// Because scopeV1.IsProspective == true, it must match
		matched := engine.MatchesScope(scopeV1, newRecord)
		if !matched {
			t.Fatalf("expected prospective hold to capture newly created matching record")
		}
	})

	t.Run("NP-16: Custodian attempts to release hold unilaterally is rejected", func(t *testing.T) {
		// Same user releasing and approving
		err := engine.ReleaseHold(hold, "usr_custodian", "usr_custodian", "Case settled", now)
		if !errors.Is(err, legalhold.ErrUnauthorizedRelease) {
			t.Fatalf("expected ErrUnauthorizedRelease for unilateral release, got: %v", err)
		}

		// Valid dual-approval release
		err = engine.ReleaseHold(hold, "usr_custodian", "usr_external_counsel", "Court order dismissal", now)
		if err != nil {
			t.Fatalf("expected authorized release to succeed, got: %v", err)
		}
		if hold.Status != legalhold.HoldStatusReleased {
			t.Fatalf("expected hold status RELEASED, got: %s", hold.Status)
		}
	})

	t.Run("NP-19: Held data used for unrelated AI training is rejected", func(t *testing.T) {
		// Active hold re-instated for purpose check
		activeHold := *hold
		activeHold.Status = legalhold.HoldStatusActive

		err := engine.AssertPurposePermitted(existingRecord, []legalhold.LegalHold{activeHold}, []legalhold.LegalHoldScope{*scopeV1}, "AI_MODEL_TRAINING")
		if !errors.Is(err, legalhold.ErrHeldDataPurposeProhibited) {
			t.Fatalf("expected ErrHeldDataPurposeProhibited, got: %v", err)
		}

		// Legitimate legal discovery purpose passes
		err = engine.AssertPurposePermitted(existingRecord, []legalhold.LegalHold{activeHold}, []legalhold.LegalHoldScope{*scopeV1}, "LEGAL_DISCOVERY")
		if err != nil {
			t.Fatalf("expected legal discovery to be permitted on held record, got: %v", err)
		}
	})
}
