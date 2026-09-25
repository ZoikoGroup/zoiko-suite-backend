package domain_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func utc(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }

func monthlyPlan(autoRenew bool, noticeDays, minTerm int, trial *domain.TrialPolicy) *domain.PriceVersion {
	return &domain.PriceVersion{BillingInterval: "MONTH", BillingIntervalCount: 1, Terms: &domain.CommercialTerms{
		AutoRenew: autoRenew, RenewalNoticeDays: noticeDays, MinimumTermIntervals: minTerm, Trial: trial}}
}

// A term anchored on the 31st ends on the last day of short months and comes
// back to the 31st — anchored arithmetic, never chained.
func TestAddBillingIntervals_ClampsToMonthEndWithoutDrifting(t *testing.T) {
	anchor := utc(2031, time.January, 31, 9)
	cases := []struct {
		interval string
		count, n int
		want     time.Time
	}{
		{"MONTH", 1, 1, utc(2031, time.February, 28, 9)},
		{"MONTH", 1, 2, utc(2031, time.March, 31, 9)},
		{"MONTH", 1, 13, utc(2032, time.February, 29, 9)}, // leap year
		{"QUARTER", 1, 1, utc(2031, time.April, 30, 9)},
		{"YEAR", 1, 1, utc(2032, time.January, 31, 9)},
		{"MONTH", 3, 1, utc(2031, time.April, 30, 9)},
		{"MONTH", 1, 0, anchor},
	}
	for _, tc := range cases {
		if got := domain.AddBillingIntervals(anchor, tc.interval, tc.count, tc.n); !got.Equal(tc.want) {
			t.Errorf("%d x %d %s from %s = %s, want %s", tc.n, tc.count, tc.interval, anchor, got, tc.want)
		}
	}
	leapAnchor := utc(2032, time.February, 29, 0)
	if got := domain.AddBillingIntervals(leapAnchor, "YEAR", 1, 1); !got.Equal(utc(2033, time.February, 28, 0)) {
		t.Errorf("29 Feb + 1 year = %s, want 28 Feb", got)
	}
}

func TestCancellationBoundary_HonoursNoticeAndMinimumTerm(t *testing.T) {
	anchor := utc(2031, time.January, 1, 0)
	plan := monthlyPlan(true, 30, 1, nil)

	// On 15 Jan the next term end (1 Feb) is only 17 days away, inside the
	// 30-day notice window, so the cancellation lands on 1 March.
	if got := domain.CancellationBoundary(plan, anchor, utc(2031, time.January, 15, 0)); !got.Equal(utc(2031, time.March, 1, 0)) {
		t.Fatalf("inside the notice window: boundary = %s, want 1 March", got)
	}

	// Exactly at the notice limit still counts.
	if got := domain.CancellationBoundary(plan, anchor, utc(2031, time.January, 2, 0)); !got.Equal(utc(2031, time.February, 1, 0)) {
		t.Fatalf("30 days of notice exactly: boundary = %s, want 1 Feb", got)
	}

	// A 12-month minimum term: nothing ends before it is served.
	annualCommit := monthlyPlan(true, 0, 12, nil)
	if got := domain.CancellationBoundary(annualCommit, anchor, utc(2031, time.March, 10, 0)); !got.Equal(utc(2032, time.January, 1, 0)) {
		t.Fatalf("minimum term: boundary = %s, want 1 Jan 2032", got)
	}
	if got := domain.MinimumTermEnd(annualCommit, anchor); !got.Equal(utc(2032, time.January, 1, 0)) {
		t.Fatalf("minimum term end = %s", got)
	}
}

func TestPlanStart_FollowsTheDisclosedTrialPolicyOnly(t *testing.T) {
	start := utc(2031, time.January, 1, 0)
	trialEnd := start.Add(14 * 24 * time.Hour)

	none := domain.PlanStart(monthlyPlan(true, 0, 1, nil), start)
	if len(none.Versions) != 1 || none.Versions[0].Status != domain.LifecyclePending || len(none.Terms) != 0 || none.EndsAt != nil {
		t.Fatalf("no trial: %+v", none)
	}

	convert := domain.PlanStart(monthlyPlan(true, 0, 1, &domain.TrialPolicy{DurationDays: 14, Conversion: "CONVERT_TO_PAID"}), start)
	if len(convert.Versions) != 2 || convert.Versions[0].Status != domain.LifecycleTrialing ||
		convert.Versions[1].Status != domain.LifecycleActive || !convert.Versions[1].EffectiveFrom.Equal(trialEnd) ||
		len(convert.Terms) != 1 || !convert.Terms[0].StartsAt.Equal(trialEnd) || convert.EndsAt != nil {
		t.Fatalf("convert-to-paid trial: %+v", convert)
	}

	for _, policy := range []string{"CANCEL_AT_END", "REQUIRE_CONFIRMATION"} {
		p := domain.PlanStart(monthlyPlan(true, 0, 1, &domain.TrialPolicy{DurationDays: 14, Conversion: policy}), start)
		if len(p.Versions) != 2 || p.Versions[1].Status != domain.LifecycleExpired || len(p.Terms) != 0 ||
			p.EndsAt == nil || !p.EndsAt.Equal(trialEnd) {
			t.Fatalf("%s trial must expire, not silently convert: %+v", policy, p)
		}
	}

	noRenew := domain.PlanActivation(monthlyPlan(false, 0, 1, nil), start, domain.ChangeActivated)
	if len(noRenew.Versions) != 2 || noRenew.Versions[1].Status != domain.LifecycleExpired || noRenew.EndsAt == nil ||
		!noRenew.EndsAt.Equal(utc(2031, time.February, 1, 0)) {
		t.Fatalf("a non-renewing term must schedule its expiry: %+v", noRenew)
	}
}

func TestValidateQuantities(t *testing.T) {
	pv := &domain.PriceVersion{Components: []domain.PriceComponent{
		{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed},
		{ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, QuantityRounding: sp("UP"),
			MinimumQuantity: sp("3"), MaximumQuantity: sp("100")},
		{ComponentKey: "storage_gb", ComponentType: domain.ComponentPerUnit, QuantityRounding: sp("NONE")},
	}}
	got, err := domain.ValidateQuantities(pv, map[string]string{"seats": "4.2", "storage_gb": "12.5"})
	if err != nil || len(got) != 2 || got[0].ComponentKey != "seats" || got[0].Quantity != "5" || got[1].Quantity != "12.5" {
		t.Fatalf("quantities: %+v, %v (4.2 seats with rounding UP must become 5)", got, err)
	}
	bad := map[string]map[string]string{
		"missing":      {"seats": "5"},
		"below min":    {"seats": "2", "storage_gb": "1"},
		"above max":    {"seats": "101", "storage_gb": "1"},
		"not per-unit": {"seats": "5", "storage_gb": "1", "base": "2"},
		"unknown":      {"seats": "5", "storage_gb": "1", "gpus": "1"},
		"negative":     {"seats": "-5", "storage_gb": "1"},
	}
	for name, q := range bad {
		var ve *domain.ValidationError
		if _, err := domain.ValidateQuantities(pv, q); !errors.As(err, &ve) {
			t.Errorf("%s: got %v, want a ValidationError", name, err)
		}
	}
}
