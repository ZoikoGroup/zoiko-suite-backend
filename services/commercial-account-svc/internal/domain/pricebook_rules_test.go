package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/commercial-account-svc/internal/domain"
)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }
func bp(b bool) *bool     { return &b }

var usd = domain.CommercialCurrency{CurrencyCode: "USD", MinorUnits: 2, SaleEnabled: true}
var jpy = domain.CommercialCurrency{CurrencyCode: "JPY", MinorUnits: 0, SaleEnabled: true}

func recurring(key, amount string) domain.PriceComponent {
	return domain.PriceComponent{ComponentKey: key, ComponentType: domain.ComponentRecurringFixed,
		Amount: sp(amount), BillingTiming: sp("IN_ADVANCE")}
}

// ── Identifiers ──────────────────────────────────────────────────────────────

// Negative path #01: a tenant-plane identifier is refused as cross-plane, not
// looked up.
func TestParseCommercialID_BareUUIDIsCrossPlane(t *testing.T) {
	for _, raw := range []string{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3F2504E0-4F89-11D3-9A0C-0305E82C3301"} {
		if _, err := domain.ParseCommercialID(domain.PrefixPriceVersion, raw); !errors.Is(err, domain.ErrCrossPlaneIdentifier) {
			t.Errorf("ParseCommercialID(%q) = %v, want ErrCrossPlaneIdentifier", raw, err)
		}
	}
}

func TestParseCommercialID_WrongKindIsInvalidNotCrossPlane(t *testing.T) {
	product := domain.NewCommercialID(domain.PrefixProduct)
	_, err := domain.ParseCommercialID(domain.PrefixPriceVersion, product)
	if !errors.Is(err, domain.ErrInvalidCommercialID) {
		t.Fatalf("a product id used as a price version id: got %v, want ErrInvalidCommercialID", err)
	}
	for _, raw := range []string{"", "cpv_", "cpv_not-a-uuid", "CPV_3f2504e0-4f89-11d3-9a0c-0305e82c3301", "cpv_3f2504e0-4f89-11d3-9a0c-0305e82c3301x"} {
		if _, err := domain.ParseCommercialID(domain.PrefixPriceVersion, raw); !errors.Is(err, domain.ErrInvalidCommercialID) {
			t.Errorf("ParseCommercialID(%q) = %v, want ErrInvalidCommercialID", raw, err)
		}
	}
	id := domain.NewCommercialID(domain.PrefixPriceVersion)
	if got, err := domain.ParseCommercialID(domain.PrefixPriceVersion, id); err != nil || got != id {
		t.Fatalf("a freshly minted id did not round-trip: %q, %v", got, err)
	}
}

// ── Components ───────────────────────────────────────────────────────────────

func TestValidateComponent_AcceptsEachWellFormedType(t *testing.T) {
	cases := map[string]domain.PriceComponent{
		"recurring": recurring("base", "49.00"),
		"per_unit_flat": {ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, UnitName: sp("seat"),
			BillingTiming: sp("IN_ADVANCE"), IncludedQuantity: sp("5"), QuantityRounding: sp("UP"), Amount: sp("12.50"),
			MinimumQuantity: sp("1"), MaximumQuantity: sp("500")},
		"per_unit_tiered": {ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, UnitName: sp("seat"),
			BillingTiming: sp("IN_ADVANCE"), IncludedQuantity: sp("0"), QuantityRounding: sp("NONE"), TierMode: sp("GRADUATED"),
			Tiers: []domain.PriceTier{
				{TierIndex: 1, UpToQuantity: sp("10"), UnitAmount: "10.00"},
				{TierIndex: 2, UpToQuantity: sp("100"), UnitAmount: "8.00", FlatAmount: sp("5.00")},
				{TierIndex: 3, UnitAmount: "6.00"},
			}},
		"metered": {ComponentKey: "api_calls", ComponentType: domain.ComponentMetered, MeterKey: sp("api.calls"),
			MeterVersion: ip(1), AggregationMethod: sp("SUM"), IncludedQuantity: sp("10000"),
			BillingTiming: sp("IN_ARREARS"), Amount: sp("0.0015")},
		"one_time": {ComponentKey: "setup", ComponentType: domain.ComponentOneTime, Amount: sp("199.00"),
			TriggerEvent: sp("SUBSCRIPTION_START"), ExpiresAfterDays: ip(30)},
		"discount": {ComponentKey: "launch_promo", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
			DiscountValue: sp("20"), DurationIntervals: ip(3), EligibilityCode: sp("NEW_CUSTOMER"), RequiresApproval: bp(true)},
	}
	for name, c := range cases {
		c := c
		if err := domain.ValidateComponent(&c, usd); err != nil {
			t.Errorf("%s: unexpected refusal: %v", name, err)
		}
	}
}

func TestValidateComponent_RefusesMalformedShapes(t *testing.T) {
	cases := []struct {
		name  string
		c     domain.PriceComponent
		field string
	}{
		{"recurring without amount", domain.PriceComponent{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, BillingTiming: sp("IN_ADVANCE")}, "amount"},
		{"recurring with a meter", func() domain.PriceComponent { c := recurring("base", "10.00"); c.MeterKey = sp("x"); return c }(), "meter_key"},
		{"per-unit with amount and tiers", domain.PriceComponent{ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, UnitName: sp("seat"),
			BillingTiming: sp("IN_ADVANCE"), IncludedQuantity: sp("0"), QuantityRounding: sp("UP"), Amount: sp("1.00"), TierMode: sp("VOLUME"),
			Tiers: []domain.PriceTier{{TierIndex: 1, UnitAmount: "1.00"}}}, "amount"},
		{"metered billed in advance", domain.PriceComponent{ComponentKey: "calls", ComponentType: domain.ComponentMetered, MeterKey: sp("api.calls"),
			MeterVersion: ip(1), AggregationMethod: sp("SUM"), IncludedQuantity: sp("0"), BillingTiming: sp("IN_ADVANCE"), Amount: sp("0.01")}, "billing_timing"},
		{"percent over 100", domain.PriceComponent{ComponentKey: "promo", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
			DiscountValue: sp("101"), DurationIntervals: ip(1), EligibilityCode: sp("ANY"), RequiresApproval: bp(true)}, "discount_value"},
		{"discount without eligibility", domain.PriceComponent{ComponentKey: "promo", ComponentType: domain.ComponentDiscount, DiscountType: sp("PERCENT"),
			DiscountValue: sp("10"), DurationIntervals: ip(1), RequiresApproval: bp(true)}, "eligibility_code"},
		{"unknown enum", domain.PriceComponent{ComponentKey: "base", ComponentType: domain.ComponentRecurringFixed, Amount: sp("1.00"), BillingTiming: sp("SOMETIMES")}, "billing_timing"},
		{"unknown type", domain.PriceComponent{ComponentKey: "base", ComponentType: "SUBSCRIPTION_BOX"}, "component_type"},
		{"bad key", recurring("Base Price", "1.00"), "component_key"},
	}
	for _, tc := range cases {
		c := tc.c
		err := domain.ValidateComponent(&c, usd)
		var ve *domain.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: got %v, want a ValidationError", tc.name, err)
			continue
		}
		if ve.Field != tc.field {
			t.Errorf("%s: refused on field %q, want %q (%v)", tc.name, ve.Field, tc.field, err)
		}
	}
}

// A fixed charge cannot carry more precision than the currency has minor
// units; a per-unit price can go to four places.
func TestValidateComponent_ScaleFollowsCurrencyForFixedChargesOnly(t *testing.T) {
	over := recurring("base", "10.005")
	if err := domain.ValidateComponent(&over, usd); err == nil {
		t.Error("USD recurring amount 10.005 (3 dp) was accepted; USD has 2 minor units")
	}
	yen := recurring("base", "1000.5")
	if err := domain.ValidateComponent(&yen, jpy); err == nil {
		t.Error("JPY recurring amount 1000.5 was accepted; JPY has 0 minor units")
	}
	unit := domain.PriceComponent{ComponentKey: "calls", ComponentType: domain.ComponentPerUnit, UnitName: sp("call"),
		BillingTiming: sp("IN_ARREARS"), IncludedQuantity: sp("0"), QuantityRounding: sp("NONE"), Amount: sp("0.0015")}
	if err := domain.ValidateComponent(&unit, jpy); err != nil {
		t.Errorf("a four-place unit price must be allowed even in a zero-minor-unit currency: %v", err)
	}
}

func TestValidateComponent_TiersMustCoverEveryQuantityOnce(t *testing.T) {
	base := func(tiers ...domain.PriceTier) domain.PriceComponent {
		return domain.PriceComponent{ComponentKey: "seats", ComponentType: domain.ComponentPerUnit, UnitName: sp("seat"),
			BillingTiming: sp("IN_ADVANCE"), IncludedQuantity: sp("0"), QuantityRounding: sp("UP"), TierMode: sp("VOLUME"), Tiers: tiers}
	}
	bad := map[string]domain.PriceComponent{
		"bounded final tier": base(domain.PriceTier{TierIndex: 1, UpToQuantity: sp("10"), UnitAmount: "1"}),
		"unbounded middle":   base(domain.PriceTier{TierIndex: 1, UnitAmount: "1"}, domain.PriceTier{TierIndex: 2, UnitAmount: "1"}),
		"descending bounds": base(domain.PriceTier{TierIndex: 1, UpToQuantity: sp("100"), UnitAmount: "1"},
			domain.PriceTier{TierIndex: 2, UpToQuantity: sp("50"), UnitAmount: "1"}, domain.PriceTier{TierIndex: 3, UnitAmount: "1"}),
		"equal bounds": base(domain.PriceTier{TierIndex: 1, UpToQuantity: sp("10"), UnitAmount: "1"},
			domain.PriceTier{TierIndex: 2, UpToQuantity: sp("10.0"), UnitAmount: "1"}, domain.PriceTier{TierIndex: 3, UnitAmount: "1"}),
		"gap in numbering": base(domain.PriceTier{TierIndex: 1, UpToQuantity: sp("10"), UnitAmount: "1"}, domain.PriceTier{TierIndex: 3, UnitAmount: "1"}),
		"no tiers":         base(),
	}
	for name, c := range bad {
		c := c
		if err := domain.ValidateComponent(&c, usd); err == nil {
			t.Errorf("%s: accepted a tier table that leaves a quantity unpriced or double-priced", name)
		}
	}
}

// ── Content hash ─────────────────────────────────────────────────────────────

func sampleVersion() *domain.PriceVersion {
	limit := int64(10)
	return &domain.PriceVersion{
		PriceVersionID: domain.NewCommercialID(domain.PrefixPriceVersion), ProductID: domain.NewCommercialID(domain.PrefixProduct),
		VersionNumber: 1, DisplayName: "Business", BillingInterval: "MONTH", BillingIntervalCount: 1, CurrencyCode: "USD",
		MarketCodes: []string{"GB", "US"}, EffectiveFrom: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		Terms: &domain.CommercialTerms{TermsDocumentRef: "terms/v4", TermsDocumentSHA256: strings.Repeat("a", 64),
			AutoRenew: true, RenewalNoticeDays: 30, MinimumTermIntervals: 1},
		Components:   []domain.PriceComponent{recurring("base", "49.00"), recurring("support", "10.00")},
		Capabilities: []domain.PlanCapability{{CapabilityKey: "users", LimitValue: &limit, LimitUnit: sp("user")}},
	}
}

func TestContentHash_IgnoresRowOrderAndLifecycleEvidence(t *testing.T) {
	a := sampleVersion()
	b := *a
	b.Components = []domain.PriceComponent{a.Components[1], a.Components[0]}
	b.MarketCodes = []string{"US", "GB"}
	b.Status = domain.PriceVersionApproved
	b.RowVersion = 9
	who := "someone-else"
	b.ApprovedByPrincipalID = &who
	if domain.ContentHash(a) != domain.ContentHash(&b) {
		t.Fatal("content hash changed with row order or lifecycle evidence; it must depend on commercial content only")
	}
}

func TestContentHash_ChangesWhenAnyPriceChanges(t *testing.T) {
	a := sampleVersion()
	before := domain.ContentHash(a)
	mutations := map[string]func(v *domain.PriceVersion){
		"amount":       func(v *domain.PriceVersion) { v.Components[0].Amount = sp("49.01") },
		"amount scale": func(v *domain.PriceVersion) { v.Components[0].Amount = sp("49.0") },
		"currency":     func(v *domain.PriceVersion) { v.CurrencyCode = "EUR" },
		"effective":    func(v *domain.PriceVersion) { v.EffectiveFrom = v.EffectiveFrom.Add(time.Second) },
		"terms":        func(v *domain.PriceVersion) { v.Terms.RenewalNoticeDays = 60 },
		"trial added": func(v *domain.PriceVersion) {
			v.Terms.Trial = &domain.TrialPolicy{DurationDays: 14, Conversion: "CANCEL_AT_END"}
		},
		"capability":    func(v *domain.PriceVersion) { n := int64(11); v.Capabilities[0].LimitValue = &n },
		"component out": func(v *domain.PriceVersion) { v.Components = v.Components[:1] },
	}
	for name, mutate := range mutations {
		v := sampleVersion()
		v.PriceVersionID, v.ProductID = a.PriceVersionID, a.ProductID
		mutate(v)
		if domain.ContentHash(v) == before {
			t.Errorf("%s: content hash did not change; an approval would still match altered content", name)
		}
	}
}

// ── Lifecycle gates ──────────────────────────────────────────────────────────

func TestCheckSubmittable_ListsEveryBlocker(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	v := sampleVersion()
	v.Terms = nil
	v.EffectiveFrom = now
	v.Components = []domain.PriceComponent{{ComponentKey: "promo", ComponentType: domain.ComponentDiscount,
		DiscountType: sp("PERCENT"), DiscountValue: sp("10"), DurationIntervals: ip(1), EligibilityCode: sp("ANY"), RequiresApproval: bp(true)}}
	disabled := usd
	disabled.SaleEnabled = false

	err := domain.CheckSubmittable(v, disabled, now)
	var blocked *domain.PublicationBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("got %v, want PublicationBlockedError", err)
	}
	if len(blocked.Reasons) != 4 {
		t.Fatalf("want 4 reasons (terms, currency, effective date, no chargeable component), got %d: %v", len(blocked.Reasons), blocked.Reasons)
	}
}

// Metered pricing waits for the COM-04 meter registry: blocked, not assumed.
func TestCheckSubmittable_MeteredComponentIsBlockedUntilMetersExist(t *testing.T) {
	v := sampleVersion()
	v.Components = append(v.Components, domain.PriceComponent{ComponentKey: "calls", ComponentType: domain.ComponentMetered,
		MeterKey: sp("api.calls"), MeterVersion: ip(1), AggregationMethod: sp("SUM"), IncludedQuantity: sp("0"),
		BillingTiming: sp("IN_ARREARS"), Amount: sp("0.01")})
	if err := domain.CheckSubmittable(v, usd, v.EffectiveFrom.Add(-time.Hour)); !errors.Is(err, domain.ErrMeterNotRegistered) {
		t.Fatalf("got %v, want ErrMeterNotRegistered", err)
	}
}

func TestCheckPublishable_RefusesARetroactivePrice(t *testing.T) {
	v := sampleVersion()
	if err := domain.CheckPublishable(v, usd, v.EffectiveFrom.Add(time.Minute)); err == nil {
		t.Fatal("published a price whose effective_from had already passed")
	}
	if err := domain.CheckPublishable(v, usd, v.EffectiveFrom.Add(-time.Minute)); err != nil {
		t.Fatalf("a future-effective price must publish: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	base := sampleVersion()
	target := sampleVersion()
	target.PriceVersionID, target.ProductID = domain.NewCommercialID(domain.PrefixPriceVersion), base.ProductID
	target.Components = []domain.PriceComponent{recurring("base", "59.00"), recurring("storage", "5.00")}
	target.DisplayName = "Business Plus"

	d, err := domain.CompareVersions(base, target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.ComponentsChanged, ",") != "base" || strings.Join(d.ComponentsAdded, ",") != "storage" ||
		strings.Join(d.ComponentsRemoved, ",") != "support" {
		t.Fatalf("component diff wrong: changed=%v added=%v removed=%v", d.ComponentsChanged, d.ComponentsAdded, d.ComponentsRemoved)
	}
	if len(d.Fields) != 1 || d.Fields[0].Field != "display_name" {
		t.Fatalf("field diff wrong: %+v", d.Fields)
	}

	other := sampleVersion()
	if _, err := domain.CompareVersions(base, other); !errors.Is(err, domain.ErrVersionsNotComparable) {
		t.Fatalf("versions of different products compared: %v", err)
	}
}
