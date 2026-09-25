package domain_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
	"zoiko.io/commercial-account-svc/internal/money"
)

func seatPlan(mode string) *domain.PriceVersion {
	return &domain.PriceVersion{PriceVersionID: "cpv_x", Components: []domain.PriceComponent{
		{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, Amount: sp("49.00")},
		{ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, IncludedQuantity: sp("5"), TierMode: sp(mode),
			Tiers: []domain.PriceTier{
				{TierIndex: 1, UpToQuantity: sp("10"), UnitAmount: "12.00"},
				{TierIndex: 2, UpToQuantity: sp("50"), UnitAmount: "9.50", FlatAmount: sp("20.00")},
				{TierIndex: 3, UnitAmount: "7.00"},
			}},
		{ComponentKey: "setup", ComponentType: domain.ComponentOneTime, Amount: sp("500.00")},
	}}
}

func charge(t *testing.T, pv *domain.PriceVersion, seats string) string {
	t.Helper()
	r, err := domain.ItemPeriodCharge(pv, []domain.SubscriptionItemQuantity{{ComponentKey: "seats", Quantity: seats}})
	if err != nil {
		t.Fatal(err)
	}
	return money.FormatHalfEven(r, 2)
}

// Included seats are free, one-time charges are not recurring, and the two
// tier modes differ exactly as documented.
func TestItemPeriodCharge_TierModes(t *testing.T) {
	cases := []struct {
		mode, seats, want string
	}{
		{"GRADUATED", "5", "49.00"},   // all included
		{"GRADUATED", "8", "85.00"},   // 3 billable x 12
		{"GRADUATED", "20", "236.50"}, // 10x12 + 5x9.50 + 20 flat
		{"VOLUME", "20", "211.50"},    // 15 billable all at 9.50 + 20 flat
		{"VOLUME", "70", "504.00"},    // 65 at 7.00
		{"GRADUATED", "70", "674.00"}, // 49 + 10x12 + 40x9.50 + 20 + 15x7
	}
	for _, tc := range cases {
		if got := charge(t, seatPlan(tc.mode), tc.seats); got != tc.want {
			t.Errorf("%s with %s seats = %s, want %s", tc.mode, tc.seats, got, tc.want)
		}
	}
	if _, err := domain.ItemPeriodCharge(seatPlan("VOLUME"), nil); err == nil {
		t.Fatal("rated a per-unit component with no quantity")
	}
}

// The worked example: $49 -> $99 with 21 of 31 days left.
func TestProrate_DailyHalfEven(t *testing.T) {
	start := time.Date(2030, 1, 12, 9, 0, 0, 0, time.UTC)
	end := time.Date(2030, 2, 12, 9, 0, 0, 0, time.UTC)
	now := time.Date(2030, 1, 22, 11, 30, 0, 0, time.UTC) // partial day belongs to the old plan
	p := domain.Prorate(big.NewRat(49, 1), big.NewRat(99, 1), start, end, now, 2)
	if p.DaysInTerm != 31 || p.DaysRemaining != 20 {
		t.Fatalf("days: %d of %d", p.DaysRemaining, p.DaysInTerm)
	}
	// 49 x 20/31 = 31.6129.. -> 31.61 ; 99 x 20/31 = 63.8709.. -> 63.87
	if p.Credit != "31.61" || p.Charge != "63.87" || p.Net != "32.26" {
		t.Fatalf("proration: %+v", p)
	}
	down := domain.Prorate(big.NewRat(99, 1), big.NewRat(49, 1), start, end, now, 2)
	if down.Net != "-32.26" {
		t.Fatalf("a downgrade prorates to a credit: %+v", down)
	}
	yen := domain.Prorate(big.NewRat(4900, 1), big.NewRat(9900, 1), start, end, now, 0)
	if yen.Credit != "3161" || yen.Charge != "6387" {
		t.Fatalf("zero-minor-unit currency: %+v", yen)
	}
}

func TestChangeQuote_SealBindsTheOutcomeNotTheInstant(t *testing.T) {
	q := domain.ChangeQuote{Kind: domain.ChangeKindPlan, Timing: domain.TimingImmediate, EffectiveAt: time.Now(),
		OldPeriodCharge: "49.00000000", NewPeriodCharge: "99.00000000",
		Proration: &domain.Proration{Method: domain.ProrationDailyHalfEven, DaysInTerm: 31, DaysRemaining: 20, Net: "32.26"}}
	q.Seal()
	later := q
	later.EffectiveAt = later.EffectiveAt.Add(3 * time.Hour)
	later.Seal()
	if later.QuoteSHA256 != q.QuoteSHA256 {
		t.Fatal("an immediate quote must not depend on the exact instant it was shown")
	}
	nextDay := q
	p := *q.Proration
	p.DaysRemaining, p.Net = 19, "30.65"
	nextDay.Proration = &p
	nextDay.Seal()
	if nextDay.QuoteSHA256 == q.QuoteSHA256 {
		t.Fatal("a quote with a different proration hashed the same")
	}
	renewal := q
	renewal.Timing, renewal.Proration = domain.TimingNextRenewal, nil
	renewal.Seal()
	moved := renewal
	moved.EffectiveAt = moved.EffectiveAt.Add(24 * time.Hour)
	moved.Seal()
	if moved.QuoteSHA256 == renewal.QuoteSHA256 {
		t.Fatal("a scheduled change's effective date must be part of its quote")
	}
}

func TestValidateTransitionRule(t *testing.T) {
	var ve *domain.ValidationError
	if err := domain.ValidateTransitionRule(domain.TimingNextRenewal, domain.ProrationDailyHalfEven); !errors.As(err, &ve) {
		t.Fatal("a next-renewal change with proration was accepted")
	}
	for _, ok := range [][2]string{{domain.TimingImmediate, domain.ProrationDailyHalfEven}, {domain.TimingImmediate, domain.ProrationNone},
		{domain.TimingNextRenewal, domain.ProrationNone}} {
		if err := domain.ValidateTransitionRule(ok[0], ok[1]); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
}
