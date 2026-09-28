package privacy_test

import (
	"testing"
	"time"

	"zoiko.io/contract/governance/privacy"
	"zoiko.io/contract/types"
)

func TestPrivacyConflictScenarios(t *testing.T) {
	resolver := privacy.NewResolver()
	tenantID := types.MustNewV7()
	now := time.Now().UTC()

	req := &privacy.PrivacyDispositionRequest{
		RequestID:     types.MustNewV7(),
		TenantID:      tenantID,
		DataSubjectID: "sub_john_doe_99",
		RequestType:   privacy.RequestTypeErasure,
		RequestedAt:   now,
		Status:        "PENDING",
	}

	t.Run("NP-17: Privacy erasure request targets held data resolves to DEFER", func(t *testing.T) {
		heldContext := []privacy.FieldPolicyContext{
			{
				FieldName:        "customer_email",
				IsUnderLegalHold: true,
				HoldRef:          "LIT_2026_DISPUTE_042",
			},
			{
				FieldName:        "invoice_history",
				IsUnderLegalHold: true,
				HoldRef:          "LIT_2026_DISPUTE_042",
			},
		}

		res, err := resolver.Resolve(req, heldContext, "usr_privacy_officer", now)
		if err != nil {
			t.Fatalf("unexpected error resolving held data: %v", err)
		}
		if res.Outcome != privacy.OutcomeDefer {
			t.Fatalf("expected outcome DEFER, got: %s", res.Outcome)
		}
		if res.HoldBlockRef == nil || *res.HoldBlockRef != "LIT_2026_DISPUTE_042" {
			t.Fatalf("expected hold block ref LIT_2026_DISPUTE_042")
		}
		if res.ReviewDate == nil {
			t.Fatalf("expected review date to be set for deferred privacy request")
		}
	})

	t.Run("NP-18: Privacy request targets mixed retained and non-retained fields resolves to PARTIAL", func(t *testing.T) {
		mixedContext := []privacy.FieldPolicyContext{
			{
				FieldName:            "billing_tax_id",
				HasStatutoryBasis:    true,
				LegalRegulatoryBasis: "HMRC_VAT_ACT_1994_S58",
			},
			{
				FieldName:            "invoice_total_amount",
				HasStatutoryBasis:    true,
				LegalRegulatoryBasis: "HMRC_VAT_ACT_1994_S58",
			},
			{
				FieldName:         "marketing_preferences",
				HasStatutoryBasis: false, // Non-retained, eligible for erasure
			},
			{
				FieldName:         "browsing_session_history",
				HasStatutoryBasis: false, // Non-retained, eligible for erasure
			},
		}

		res, err := resolver.Resolve(req, mixedContext, "usr_privacy_officer", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Outcome != privacy.OutcomePartial {
			t.Fatalf("expected outcome PARTIAL, got: %s", res.Outcome)
		}
		if len(res.FieldActions) != 4 {
			t.Fatalf("expected 4 field actions, got %d", len(res.FieldActions))
		}

		erasedCount := 0
		retainedCount := 0
		for _, fa := range res.FieldActions {
			if fa.Action == "ERASE" {
				erasedCount++
			} else if fa.Action == "RETAIN" {
				retainedCount++
			}
		}
		if erasedCount != 2 || retainedCount != 2 {
			t.Fatalf("expected 2 ERASE and 2 RETAIN actions, got %d erase, %d retain", erasedCount, retainedCount)
		}
	})
}
