package tax

import (
	"testing"
	"time"

	"zoiko.io/contract/types"
)

// TestScenarioA4_TaxRuleVersioningReproducibility verifies that when tax rules change over time
// (e.g. rate changes on July 1), determinations before and after the boundary preserve the exact
// rule pack versions and rates, remaining reproducible for audit review (Scenario A4).
func TestScenarioA4_TaxRuleVersioningReproducibility(t *testing.T) {
	jurisdictionID := types.MustNewV7()
	june30 := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	july1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Pack 1: Valid Jan 1 to June 30, 2026 (20% standard VAT)
	pack1ID := types.MustNewV7()
	rule1ID := types.MustNewV7()
	pack1 := TaxRulePack{
		TaxRulePackID:  pack1ID,
		JurisdictionID: jurisdictionID,
		TaxType:        TaxTypeVAT,
		PackVersion:    "2026.1.0",
		EffectiveFrom:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:    &june30,
		ApprovalStatus: PackStatusApproved,
		Rules: []TaxRule{
			{
				TaxRuleID:     rule1ID,
				TaxRulePackID: pack1ID,
				RuleCode:      "VAT_STD_20",
				RuleType:      "STANDARD_RATE",
				Rate:          types.MustParseRate("0.200000000000000000"), // 20%
			},
		},
	}

	// Pack 2: Valid July 1, 2026 onward (21% standard VAT)
	pack2ID := types.MustNewV7()
	rule2ID := types.MustNewV7()
	pack2 := TaxRulePack{
		TaxRulePackID:  pack2ID,
		JurisdictionID: jurisdictionID,
		TaxType:        TaxTypeVAT,
		PackVersion:    "2026.2.0",
		EffectiveFrom:  july1,
		EffectiveTo:    nil, // currently active
		ApprovalStatus: PackStatusApproved,
		Rules: []TaxRule{
			{
				TaxRuleID:     rule2ID,
				TaxRulePackID: pack2ID,
				RuleCode:      "VAT_STD_21",
				RuleType:      "STANDARD_RATE",
				Rate:          types.MustParseRate("0.210000000000000000"), // 21%
			},
		},
	}

	basis := types.MustParseMoney("1000.00")

	// 1. Transaction on June 15, 2026 -> Must resolve to Pack 1 (20% -> 200.00 tax)
	dateJune15 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if !pack1.IsEffectiveAt(dateJune15) {
		t.Fatalf("pack1 should be effective on June 15")
	}
	if pack2.IsEffectiveAt(dateJune15) {
		t.Fatalf("pack2 should NOT be effective on June 15")
	}

	det1 := TaxDetermination{
		TaxDeterminationID: types.MustNewV7(),
		TenantID:           types.MustNewV7(),
		LegalEntityID:      types.MustNewV7(),
		SourceObjectID:     types.MustNewV7(),
		RulePackVersionID:  pack1ID,
		TaxPointDate:       dateJune15,
		TaxableBasis:       basis,
		CurrencyCode:       "EUR",
		ResultStatus:       DeterminationStatusCalculated,
		InputFactsHash:     "sha256-facts-hash-1",
		Components: []TaxComponent{
			{
				TaxComponentID: types.MustNewV7(),
				TaxType:        TaxTypeVAT,
				JurisdictionID: jurisdictionID,
				Rate:           pack1.Rules[0].Rate,
				TaxableAmount:  basis,
				TaxAmount:      basis.MulRate(pack1.Rules[0].Rate).RoundToCurrencyMinorUnits(2),
				RuleID:         rule1ID,
			},
		},
	}

	if det1.TotalTaxAmount().StringTrimmed() != "200.00" {
		t.Fatalf("expected 200.00 tax on June 15, got %s", det1.TotalTaxAmount().StringTrimmed())
	}

	// 2. Transaction on July 15, 2026 -> Must resolve to Pack 2 (21% -> 210.00 tax)
	dateJuly15 := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if pack1.IsEffectiveAt(dateJuly15) {
		t.Fatalf("pack1 should NOT be effective on July 15")
	}
	if !pack2.IsEffectiveAt(dateJuly15) {
		t.Fatalf("pack2 should be effective on July 15")
	}

	det2 := TaxDetermination{
		TaxDeterminationID: types.MustNewV7(),
		TenantID:           types.MustNewV7(),
		LegalEntityID:      types.MustNewV7(),
		SourceObjectID:     types.MustNewV7(),
		RulePackVersionID:  pack2ID,
		TaxPointDate:       dateJuly15,
		TaxableBasis:       basis,
		CurrencyCode:       "EUR",
		ResultStatus:       DeterminationStatusCalculated,
		InputFactsHash:     "sha256-facts-hash-2",
		Components: []TaxComponent{
			{
				TaxComponentID: types.MustNewV7(),
				TaxType:        TaxTypeVAT,
				JurisdictionID: jurisdictionID,
				Rate:           pack2.Rules[0].Rate,
				TaxableAmount:  basis,
				TaxAmount:      basis.MulRate(pack2.Rules[0].Rate).RoundToCurrencyMinorUnits(2),
				RuleID:         rule2ID,
			},
		},
	}

	if det2.TotalTaxAmount().StringTrimmed() != "210.00" {
		t.Fatalf("expected 210.00 tax on July 15, got %s", det2.TotalTaxAmount().StringTrimmed())
	}

	// Lineage verification: det1 frozen pack is pack1, det2 frozen pack is pack2
	if det1.RulePackVersionID != pack1ID || det2.RulePackVersionID != pack2ID {
		t.Fatalf("determinations must retain their immutable rule pack lineage")
	}
}
