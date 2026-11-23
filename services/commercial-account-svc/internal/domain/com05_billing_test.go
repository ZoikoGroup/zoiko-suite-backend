package domain_test

import (
	"errors"
	"testing"

	"zoiko.io/commercial-account-svc/internal/domain"
)

// COM-05 Platform Commercial Billing, part 5a follow-up: RateInvoiceBasis
// now also rates ONE_TIME and DISCOUNT components (§4.5, §5). These tests
// are pure domain logic — no database — exercising the three scenarios the
// gap was found against.

func onDemandPlan() *domain.PriceVersion {
	return &domain.PriceVersion{PriceVersionID: "cpv_1", DisplayName: "business", Components: []domain.PriceComponent{
		{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, Amount: sp("100.00")},
		{ComponentKey: "setup_fee", ComponentType: domain.ComponentOneTime, Amount: sp("500.00"), TriggerEvent: sp("SUBSCRIPTION_START")},
		{ComponentKey: "commit_15", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
			DiscountValue: sp("15"), DurationIntervals: ip(3), EligibilityCode: sp("ANNUAL_COMMIT"), RequiresApproval: bp(true)},
	}}
}

func oneItem(pv *domain.PriceVersion) []domain.SubscriptionItem {
	return []domain.SubscriptionItem{{ItemNo: 1, PriceVersionID: pv.PriceVersionID}}
}

func pvMap(pv *domain.PriceVersion) map[string]*domain.PriceVersion {
	return map[string]*domain.PriceVersion{pv.PriceVersionID: pv}
}

// A one-time setup fee must appear on the first invoice and never again on
// a later one, with no new error and no re-billing.
func TestRateInvoiceBasis_OneTimeChargeBillsOnceThenNeverAgain(t *testing.T) {
	pv := onDemandPlan()
	// Strip the discount component for this test: only the one-time fee is
	// under test here.
	pv.Components = pv.Components[:2]
	items := oneItem(pv)
	pvs := pvMap(pv)

	// Term 1: not yet billed, so the setup fee is rated.
	lines, err := domain.RateInvoiceBasis(items, pvs, nil, map[string]bool{}, map[string]domain.ApprovedDiscount{}, 1)
	if err != nil {
		t.Fatalf("term 1: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("term 1 lines = %d, want 2 (base + setup_fee): %+v", len(lines), lines)
	}
	var setup *domain.InvoiceLine
	for i := range lines {
		if lines[i].Kind == domain.LineOneTime {
			setup = &lines[i]
		}
	}
	if setup == nil || setup.ComponentKey != "setup_fee" || setup.Amount != "500.00000000" {
		t.Fatalf("term 1 one-time line: %+v", setup)
	}

	// Term 2: the fee has now been billed (as the store would report from
	// this subscription's own issued-invoice history) — it must not
	// reappear, and this is not an error.
	billedOneTime := map[string]bool{domain.LineComponentKey(pv.PriceVersionID, "setup_fee"): true}
	lines2, err := domain.RateInvoiceBasis(items, pvs, nil, billedOneTime, map[string]domain.ApprovedDiscount{}, 2)
	if err != nil {
		t.Fatalf("term 2: %v", err)
	}
	if len(lines2) != 1 {
		t.Fatalf("term 2 lines = %d, want 1 (base only, no re-billed setup fee): %+v", len(lines2), lines2)
	}
	if lines2[0].Kind != domain.LineRecurring {
		t.Fatalf("term 2 line: %+v", lines2[0])
	}
}

// A percentage discount reduces the invoice total by the right amount while
// its duration_intervals window is open, and stops applying — without
// error — once that window has passed.
func TestRateInvoiceBasis_PercentDiscountAppliesThenExpires(t *testing.T) {
	pv := onDemandPlan()
	pv.Components = pv.Components[:1:1] // base only
	pv.Components = append(pv.Components, domain.PriceComponent{
		ComponentKey: "commit_15", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
		DiscountValue: sp("15"), DurationIntervals: ip(3), EligibilityCode: sp("ANNUAL_COMMIT"), RequiresApproval: bp(true),
	})
	items := oneItem(pv)
	pvs := pvMap(pv)
	approved := map[string]domain.ApprovedDiscount{
		domain.LineComponentKey(pv.PriceVersionID, "commit_15"): {DiscountApplicationID: "cdsc_1", FirstEligibleTermNo: 1},
	}

	for _, termNo := range []int{1, 2, 3} {
		lines, err := domain.RateInvoiceBasis(items, pvs, nil, map[string]bool{}, approved, termNo)
		if err != nil {
			t.Fatalf("term %d: %v", termNo, err)
		}
		if len(lines) != 2 {
			t.Fatalf("term %d lines = %d, want 2 (base + discount): %+v", termNo, len(lines), lines)
		}
		disc := lines[1]
		if disc.Kind != domain.LineDiscount || disc.Amount != "-15.00000000" {
			t.Fatalf("term %d discount line: %+v", termNo, disc)
		}
		if disc.DiscountApplicationID == nil || *disc.DiscountApplicationID != "cdsc_1" {
			t.Fatalf("term %d discount lineage: %+v", termNo, disc)
		}
	}

	// Term 4 is outside the 3-interval window: the discount must not
	// appear, and this is not an error either.
	lines, err := domain.RateInvoiceBasis(items, pvs, nil, map[string]bool{}, approved, 4)
	if err != nil {
		t.Fatalf("term 4: %v", err)
	}
	if len(lines) != 1 || lines[0].Kind != domain.LineRecurring {
		t.Fatalf("term 4 lines (discount must have expired): %+v", lines)
	}
}

// A discount that requires approval and has none (no entry in
// approvedDiscounts — proposed-but-undecided looks the same as never
// proposed) must never be applied. This is the COM-CTRL-005 guard, enforced
// in the rating engine itself so there is no path that silently bills an
// unapproved discount.
func TestRateInvoiceBasis_UnapprovedDiscountIsNeverApplied(t *testing.T) {
	pv := onDemandPlan()
	pv.Components = pv.Components[:1:1]
	pv.Components = append(pv.Components, domain.PriceComponent{
		ComponentKey: "commit_15", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
		DiscountValue: sp("15"), DurationIntervals: ip(3), EligibilityCode: sp("ANNUAL_COMMIT"), RequiresApproval: bp(true),
	})
	items := oneItem(pv)
	pvs := pvMap(pv)

	// No approvedDiscounts entry at all: this is what the store reports when
	// the discount was never proposed, is still PROPOSED, or was REJECTED
	// or WITHDRAWN — every one of those is "not approved".
	lines, err := domain.RateInvoiceBasis(items, pvs, nil, map[string]bool{}, map[string]domain.ApprovedDiscount{}, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("an unapproved discount was applied: %+v", lines)
	}
	if lines[0].Kind != domain.LineRecurring {
		t.Fatalf("unexpected line: %+v", lines[0])
	}
}

// A fixed-amount discount larger than the base it discounts is clamped to
// the base: a discount reduces what is owed, it never flips the invoice
// into a credit.
func TestRateInvoiceBasis_DiscountNeverExceedsBase(t *testing.T) {
	pv := &domain.PriceVersion{PriceVersionID: "cpv_2", DisplayName: "starter", Components: []domain.PriceComponent{
		{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, Amount: sp("10.00")},
		{ComponentKey: "big_discount", ComponentType: domain.ComponentDiscount, DiscountType: sp("FIXED_AMOUNT"),
			DiscountValue: sp("50.00"), DurationIntervals: ip(1), EligibilityCode: sp("PROMO"), RequiresApproval: bp(false)},
	}}
	items := oneItem(pv)
	pvs := pvMap(pv)
	approved := map[string]domain.ApprovedDiscount{
		domain.LineComponentKey(pv.PriceVersionID, "big_discount"): {DiscountApplicationID: "cdsc_2", FirstEligibleTermNo: 1},
	}
	lines, err := domain.RateInvoiceBasis(items, pvs, nil, map[string]bool{}, approved, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 2 || lines[1].Amount != "-10.00000000" {
		t.Fatalf("discount clamp: %+v", lines)
	}
}

// A METERED component with no certified usage still blocks generation
// exactly as before — the new ONE_TIME/DISCOUNT handling must not change
// this doctrine.
func TestRateInvoiceBasis_StillRefusesUncertifiedMeteredUsage(t *testing.T) {
	pv := &domain.PriceVersion{PriceVersionID: "cpv_3", Components: []domain.PriceComponent{
		{ComponentKey: "api_calls", ComponentType: domain.ComponentMetered, MeterKey: sp("api.calls"),
			IncludedQuantity: sp("0"), Amount: sp("0.01")},
	}}
	items := oneItem(pv)
	pvs := pvMap(pv)
	_, err := domain.RateInvoiceBasis(items, pvs, map[string]domain.UsageBasis{}, map[string]bool{}, map[string]domain.ApprovedDiscount{}, 1)
	if !errors.Is(err, domain.ErrUsageBasisNotCertified) {
		t.Fatalf("got %v, want ErrUsageBasisNotCertified", err)
	}
}
