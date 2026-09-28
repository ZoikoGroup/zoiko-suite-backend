package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"zoiko.io/commercial-account-svc/internal/money"
)

// ── Draft header ─────────────────────────────────────────────────────────────

// ValidateProductInput checks a CreateProduct request.
func ValidateProductInput(code string, kind ProductKind) error {
	if !productCodePattern.MatchString(code) {
		return invalid("product_code", "must be 2-64 chars of lowercase letters, digits and underscores, starting with a letter")
	}
	if kind != ProductKindPlan && kind != ProductKindAddOn {
		return invalid("product_kind", "must be PLAN or ADD_ON")
	}
	return nil
}

// NormalizeMarketCodes validates, de-duplicates and sorts market codes, so the
// same market set always hashes the same way.
func NormalizeMarketCodes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, invalid("market_codes", "at least one market is required")
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		if !marketCodePattern.MatchString(m) {
			return nil, invalid("market_codes", fmt.Sprintf("%q is not a market code (2-8 uppercase letters)", m))
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ValidateDraftHeader checks the version-level fields of CreateDraftVersion.
func ValidateDraftHeader(v *PriceVersion) error {
	if strings.TrimSpace(v.DisplayName) == "" || len(v.DisplayName) > 255 {
		return invalid("display_name", "is required and at most 255 characters")
	}
	if !billingIntervals[v.BillingInterval] {
		return invalid("billing_interval", "must be MONTH, QUARTER or YEAR")
	}
	if v.BillingIntervalCount < 1 || v.BillingIntervalCount > 36 {
		return invalid("billing_interval_count", "must be between 1 and 36")
	}
	if !currencyCodePattern.MatchString(v.CurrencyCode) {
		return invalid("currency_code", "must be a 3-letter uppercase currency code")
	}
	if v.EffectiveFrom.IsZero() {
		return invalid("effective_from", "is required")
	}
	if v.EffectiveTo != nil && !v.EffectiveTo.After(v.EffectiveFrom) {
		return invalid("effective_to", "must be after effective_from")
	}
	if strings.TrimSpace(v.ChangeReason) == "" {
		return invalid("change_reason", "is required: every price version records why it exists")
	}
	return nil
}

// ValidateTerms checks a SetCommercialTerms request.
func ValidateTerms(t *CommercialTerms) error {
	if strings.TrimSpace(t.TermsDocumentRef) == "" || len(t.TermsDocumentRef) > 512 {
		return invalid("terms_document_ref", "is required and at most 512 characters")
	}
	if !sha256HexPattern.MatchString(t.TermsDocumentSHA256) {
		return invalid("terms_document_sha256", "must be the lowercase hex SHA-256 of the terms document")
	}
	if t.RenewalNoticeDays < 0 || t.RenewalNoticeDays > 3650 {
		return invalid("renewal_notice_days", "must be between 0 and 3650")
	}
	if t.MinimumTermIntervals < 1 || t.MinimumTermIntervals > 120 {
		return invalid("minimum_term_intervals", "must be between 1 and 120")
	}
	if t.Trial != nil {
		if t.Trial.DurationDays < 1 || t.Trial.DurationDays > 365 {
			return invalid("trial.duration_days", "must be between 1 and 365")
		}
		if !trialConversions[t.Trial.Conversion] {
			return invalid("trial.conversion", "must be CONVERT_TO_PAID, CANCEL_AT_END or REQUIRE_CONFIRMATION")
		}
	}
	return nil
}

// ValidateCapabilities checks a SetPlanCapabilities request.
func ValidateCapabilities(caps []PlanCapability) error {
	seen := make(map[string]bool, len(caps))
	for i, c := range caps {
		field := fmt.Sprintf("capabilities[%d]", i)
		if !capabilityKeyPattern.MatchString(c.CapabilityKey) {
			return invalid(field+".capability_key", "must be 2-128 chars of lowercase letters, digits and . _ : -")
		}
		if seen[c.CapabilityKey] {
			return invalid(field+".capability_key", "duplicate capability "+c.CapabilityKey)
		}
		seen[c.CapabilityKey] = true
		if c.LimitValue != nil {
			if *c.LimitValue < 0 {
				return invalid(field+".limit_value", "must be zero or greater; omit it for unlimited")
			}
			if c.LimitUnit == nil || strings.TrimSpace(*c.LimitUnit) == "" || len(*c.LimitUnit) > 32 {
				return invalid(field+".limit_unit", "is required (at most 32 characters) when limit_value is set")
			}
		}
	}
	return nil
}

// ── Components ───────────────────────────────────────────────────────────────

type componentShape struct {
	required []string
	optional []string
}

// componentShapes mirrors price_components_shape in migration 000006. The
// schema is the enforcement; this exists to name the offending field.
var componentShapes = map[ComponentType]componentShape{
	ComponentRecurringFixed: {required: []string{"amount", "billing_timing"}},
	ComponentPerUnit: {
		required: []string{"unit_name", "billing_timing", "included_quantity", "quantity_rounding"},
		optional: []string{"amount", "tier_mode", "tiers", "minimum_quantity", "maximum_quantity"},
	},
	ComponentMetered: {
		required: []string{"meter_key", "meter_version", "aggregation_method", "included_quantity", "billing_timing"},
		optional: []string{"amount", "tier_mode", "tiers"},
	},
	ComponentOneTime: {
		required: []string{"amount", "trigger_event"},
		optional: []string{"eligibility_code", "expires_after_days"},
	},
	ComponentDiscount: {
		required: []string{"discount_type", "discount_value", "duration_intervals", "eligibility_code", "requires_approval"},
		optional: []string{"discount_cap_amount"},
	},
}

func presentFields(c *PriceComponent) map[string]bool {
	return map[string]bool{
		"amount":              c.Amount != nil,
		"billing_timing":      c.BillingTiming != nil,
		"unit_name":           c.UnitName != nil,
		"included_quantity":   c.IncludedQuantity != nil,
		"minimum_quantity":    c.MinimumQuantity != nil,
		"maximum_quantity":    c.MaximumQuantity != nil,
		"quantity_rounding":   c.QuantityRounding != nil,
		"tier_mode":           c.TierMode != nil,
		"tiers":               len(c.Tiers) > 0,
		"meter_key":           c.MeterKey != nil,
		"meter_version":       c.MeterVersion != nil,
		"aggregation_method":  c.AggregationMethod != nil,
		"trigger_event":       c.TriggerEvent != nil,
		"eligibility_code":    c.EligibilityCode != nil,
		"expires_after_days":  c.ExpiresAfterDays != nil,
		"discount_type":       c.DiscountType != nil,
		"discount_value":      c.DiscountValue != nil,
		"discount_cap_amount": c.DiscountCapAmount != nil,
		"duration_intervals":  c.DurationIntervals != nil,
		"requires_approval":   c.RequiresApproval != nil,
	}
}

// ValidateComponent checks one component against the §4.1 price component
// model and the currency it is priced in. Fixed charges may carry no more
// fractional digits than the currency has minor units; per-unit prices may go
// to money.MaxScale ($0.0015 per API call is a real price).
func ValidateComponent(c *PriceComponent, cur CommercialCurrency) error {
	if !componentKeyPattern.MatchString(c.ComponentKey) {
		return invalid("component_key", "must be 1-64 chars of lowercase letters, digits and underscores, starting with a letter")
	}
	shape, ok := componentShapes[c.ComponentType]
	if !ok {
		return invalid("component_type", "must be RECURRING_FIXED, PER_UNIT, METERED, ONE_TIME or DISCOUNT")
	}
	present := presentFields(c)
	allowed := map[string]bool{}
	for _, f := range shape.required {
		allowed[f] = true
		if !present[f] {
			return invalid(f, "is required for a "+string(c.ComponentType)+" component")
		}
	}
	for _, f := range shape.optional {
		allowed[f] = true
	}
	for f, p := range present {
		if p && !allowed[f] {
			return invalid(f, "does not apply to a "+string(c.ComponentType)+" component")
		}
	}

	switch c.ComponentType {
	case ComponentRecurringFixed:
		if err := checkEnum("billing_timing", c.BillingTiming, billingTimings); err != nil {
			return err
		}
		return checkAmount("amount", c.Amount, cur.MinorUnits)

	case ComponentPerUnit, ComponentMetered:
		if c.ComponentType == ComponentPerUnit {
			if strings.TrimSpace(*c.UnitName) == "" || len(*c.UnitName) > 64 {
				return invalid("unit_name", "is required and at most 64 characters")
			}
			if err := checkEnum("billing_timing", c.BillingTiming, billingTimings); err != nil {
				return err
			}
			if err := checkEnum("quantity_rounding", c.QuantityRounding, quantityRoundings); err != nil {
				return err
			}
			if err := checkQuantityBounds(c.MinimumQuantity, c.MaximumQuantity); err != nil {
				return err
			}
		} else {
			if !meterKeyPattern.MatchString(*c.MeterKey) {
				return invalid("meter_key", "must be 1-128 chars of lowercase letters, digits and . _ : -")
			}
			if *c.MeterVersion < 1 {
				return invalid("meter_version", "must be 1 or greater")
			}
			if err := checkEnum("aggregation_method", c.AggregationMethod, aggregationMethods); err != nil {
				return err
			}
			if *c.BillingTiming != "IN_ARREARS" {
				return invalid("billing_timing", "metered usage can only be billed IN_ARREARS")
			}
		}
		if err := checkQuantity("included_quantity", c.IncludedQuantity); err != nil {
			return err
		}
		if (c.Amount == nil) == (c.TierMode == nil) {
			return invalid("amount", "set exactly one of amount (flat unit price) or tier_mode with tiers")
		}
		if c.Amount != nil {
			return checkAmount("amount", c.Amount, money.MaxScale)
		}
		if err := checkEnum("tier_mode", c.TierMode, tierModes); err != nil {
			return err
		}
		return checkTiers(c.Tiers, cur.MinorUnits)

	case ComponentOneTime:
		if err := checkAmount("amount", c.Amount, cur.MinorUnits); err != nil {
			return err
		}
		if !opaqueCodePattern.MatchString(*c.TriggerEvent) {
			return invalid("trigger_event", "must be an UPPER_SNAKE_CASE code")
		}
		if c.EligibilityCode != nil && !opaqueCodePattern.MatchString(*c.EligibilityCode) {
			return invalid("eligibility_code", "must be an UPPER_SNAKE_CASE code")
		}
		if c.ExpiresAfterDays != nil && *c.ExpiresAfterDays < 1 {
			return invalid("expires_after_days", "must be 1 or greater")
		}
		return nil

	case ComponentDiscount:
		if err := checkEnum("discount_type", c.DiscountType, discountTypes); err != nil {
			return err
		}
		scale := money.MaxScale
		if *c.DiscountType == "FIXED_AMOUNT" {
			scale = cur.MinorUnits
		}
		v, err := parseScaled("discount_value", *c.DiscountValue, scale)
		if err != nil {
			return err
		}
		if v.IsZero() {
			return invalid("discount_value", "must be greater than zero")
		}
		if *c.DiscountType == "PERCENT" {
			hundred, _ := money.Parse("100")
			if v.Cmp(hundred) > 0 {
				return invalid("discount_value", "a percentage discount cannot exceed 100")
			}
		}
		if c.DiscountCapAmount != nil {
			capAmt, err := parseScaled("discount_cap_amount", *c.DiscountCapAmount, cur.MinorUnits)
			if err != nil {
				return err
			}
			if capAmt.IsZero() {
				return invalid("discount_cap_amount", "must be greater than zero")
			}
		}
		if *c.DurationIntervals < 1 {
			return invalid("duration_intervals", "must be 1 or greater")
		}
		if !opaqueCodePattern.MatchString(*c.EligibilityCode) {
			return invalid("eligibility_code", "must be an UPPER_SNAKE_CASE code")
		}
		return nil
	}
	return nil
}

func checkEnum(field string, v *string, allowed map[string]bool) error {
	if !allowed[*v] {
		keys := make([]string, 0, len(allowed))
		for k := range allowed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return invalid(field, "must be one of "+strings.Join(keys, ", "))
	}
	return nil
}

func parseScaled(field, s string, maxScale int) (money.Decimal, error) {
	d, err := money.Parse(s)
	if err != nil {
		return money.Decimal{}, invalid(field, err.Error())
	}
	if d.Scale() > maxScale {
		return money.Decimal{}, invalid(field, fmt.Sprintf("has %d fractional digits; at most %d allowed here", d.Scale(), maxScale))
	}
	return d, nil
}

func checkAmount(field string, v *string, maxScale int) error {
	_, err := parseScaled(field, *v, maxScale)
	return err
}

func checkQuantity(field string, v *string) error {
	_, err := parseScaled(field, *v, money.MaxScale)
	return err
}

func checkQuantityBounds(minQ, maxQ *string) error {
	var lo, hi money.Decimal
	var err error
	if minQ != nil {
		if lo, err = parseScaled("minimum_quantity", *minQ, money.MaxScale); err != nil {
			return err
		}
	}
	if maxQ != nil {
		if hi, err = parseScaled("maximum_quantity", *maxQ, money.MaxScale); err != nil {
			return err
		}
		if hi.IsZero() {
			return invalid("maximum_quantity", "must be greater than zero")
		}
		if minQ != nil && hi.Cmp(lo) < 0 {
			return invalid("maximum_quantity", "must not be below minimum_quantity")
		}
	}
	return nil
}

// checkTiers requires ascending bands ending in one unbounded band, so every
// quantity has exactly one price.
func checkTiers(tiers []PriceTier, minorUnits int) error {
	if len(tiers) == 0 {
		return invalid("tiers", "at least one tier is required when tier_mode is set")
	}
	var prev money.Decimal
	for i, t := range tiers {
		field := fmt.Sprintf("tiers[%d]", i)
		last := i == len(tiers)-1
		if t.TierIndex != i+1 {
			return invalid(field+".tier_index", "tiers must be numbered 1..n in order")
		}
		if last && t.UpToQuantity != nil {
			return invalid(field+".up_to_quantity", "the final tier must be unbounded (omit up_to_quantity)")
		}
		if !last {
			if t.UpToQuantity == nil {
				return invalid(field+".up_to_quantity", "only the final tier may be unbounded")
			}
			q, err := parseScaled(field+".up_to_quantity", *t.UpToQuantity, money.MaxScale)
			if err != nil {
				return err
			}
			if q.IsZero() || (i > 0 && q.Cmp(prev) <= 0) {
				return invalid(field+".up_to_quantity", "tier bounds must be positive and strictly ascending")
			}
			prev = q
		}
		if _, err := parseScaled(field+".unit_amount", t.UnitAmount, money.MaxScale); err != nil {
			return err
		}
		if t.FlatAmount != nil {
			if _, err := parseScaled(field+".flat_amount", *t.FlatAmount, minorUnits); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── Lifecycle gates ──────────────────────────────────────────────────────────

// CheckSubmittable is the SubmitForApproval gate. Missing currency, unit or
// term version blocks publication (§4.1 failure semantics), and it is checked
// here, before anyone spends an approval on a version that could never
// publish.
func CheckSubmittable(v *PriceVersion, cur CommercialCurrency, now time.Time) error {
	for _, c := range v.Components {
		if c.ComponentType == ComponentMetered {
			return ErrMeterNotRegistered
		}
	}
	var reasons []string
	if v.Terms == nil {
		reasons = append(reasons, "commercial terms are not set")
	}
	if !cur.SaleEnabled {
		reasons = append(reasons, "currency "+cur.CurrencyCode+" is not enabled for sale")
	}
	if !v.EffectiveFrom.After(now) {
		reasons = append(reasons, "effective_from must be in the future when the version is submitted")
	}
	chargeable := 0
	for i := range v.Components {
		c := &v.Components[i]
		if c.ComponentType != ComponentDiscount {
			chargeable++
		}
		if err := ValidateComponent(c, cur); err != nil {
			reasons = append(reasons, "component "+c.ComponentKey+": "+err.Error())
		}
	}
	if chargeable == 0 {
		reasons = append(reasons, "at least one chargeable (non-discount) component is required")
	}
	if len(reasons) > 0 {
		return &PublicationBlockedError{Reasons: reasons}
	}
	return nil
}

// CheckPublishable is the PublishPriceVersion gate. A price is published
// before it takes effect, never after: publishing a version whose
// effective_from has already passed would make it retroactive.
func CheckPublishable(v *PriceVersion, cur CommercialCurrency, now time.Time) error {
	var reasons []string
	if !cur.SaleEnabled {
		reasons = append(reasons, "currency "+cur.CurrencyCode+" is not enabled for sale")
	}
	if v.EffectiveFrom.Before(now) {
		reasons = append(reasons, "effective_from has already passed; create a new draft with a future effective_from")
	}
	if len(reasons) > 0 {
		return &PublicationBlockedError{Reasons: reasons}
	}
	return nil
}

// ── Content hash ─────────────────────────────────────────────────────────────

type manifestTier struct {
	TierIndex    int     `json:"tier_index"`
	UpToQuantity *string `json:"up_to_quantity"`
	UnitAmount   string  `json:"unit_amount"`
	FlatAmount   *string `json:"flat_amount"`
}

type manifestComponent struct {
	ComponentKey      string         `json:"component_key"`
	ComponentType     ComponentType  `json:"component_type"`
	Amount            *string        `json:"amount"`
	BillingTiming     *string        `json:"billing_timing"`
	UnitName          *string        `json:"unit_name"`
	IncludedQuantity  *string        `json:"included_quantity"`
	MinimumQuantity   *string        `json:"minimum_quantity"`
	MaximumQuantity   *string        `json:"maximum_quantity"`
	QuantityRounding  *string        `json:"quantity_rounding"`
	TierMode          *string        `json:"tier_mode"`
	Tiers             []manifestTier `json:"tiers"`
	MeterKey          *string        `json:"meter_key"`
	MeterVersion      *int           `json:"meter_version"`
	AggregationMethod *string        `json:"aggregation_method"`
	TriggerEvent      *string        `json:"trigger_event"`
	EligibilityCode   *string        `json:"eligibility_code"`
	ExpiresAfterDays  *int           `json:"expires_after_days"`
	DiscountType      *string        `json:"discount_type"`
	DiscountValue     *string        `json:"discount_value"`
	DiscountCapAmount *string        `json:"discount_cap_amount"`
	DurationIntervals *int           `json:"duration_intervals"`
	RequiresApproval  *bool          `json:"requires_approval"`
}

type manifestTerms struct {
	TermsDocumentRef     string       `json:"terms_document_ref"`
	TermsDocumentSHA256  string       `json:"terms_document_sha256"`
	AutoRenew            bool         `json:"auto_renew"`
	RenewalNoticeDays    int          `json:"renewal_notice_days"`
	MinimumTermIntervals int          `json:"minimum_term_intervals"`
	Trial                *TrialPolicy `json:"trial"`
}

type priceManifest struct {
	PriceVersionID       string              `json:"price_version_id"`
	ProductID            string              `json:"product_id"`
	VersionNumber        int                 `json:"version_number"`
	DisplayName          string              `json:"display_name"`
	BillingInterval      string              `json:"billing_interval"`
	BillingIntervalCount int                 `json:"billing_interval_count"`
	CurrencyCode         string              `json:"currency_code"`
	MarketCodes          []string            `json:"market_codes"`
	EffectiveFrom        string              `json:"effective_from"`
	EffectiveTo          *string             `json:"effective_to"`
	Terms                *manifestTerms      `json:"terms"`
	Components           []manifestComponent `json:"components"`
	Capabilities         []PlanCapability    `json:"capabilities"`
}

func componentManifest(c PriceComponent) manifestComponent {
	tiers := make([]manifestTier, len(c.Tiers))
	for i, t := range c.Tiers {
		tiers[i] = manifestTier{TierIndex: t.TierIndex, UpToQuantity: t.UpToQuantity, UnitAmount: t.UnitAmount, FlatAmount: t.FlatAmount}
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].TierIndex < tiers[j].TierIndex })
	return manifestComponent{
		ComponentKey: c.ComponentKey, ComponentType: c.ComponentType, Amount: c.Amount,
		BillingTiming: c.BillingTiming, UnitName: c.UnitName, IncludedQuantity: c.IncludedQuantity,
		MinimumQuantity: c.MinimumQuantity, MaximumQuantity: c.MaximumQuantity,
		QuantityRounding: c.QuantityRounding, TierMode: c.TierMode, Tiers: tiers,
		MeterKey: c.MeterKey, MeterVersion: c.MeterVersion, AggregationMethod: c.AggregationMethod,
		TriggerEvent: c.TriggerEvent, EligibilityCode: c.EligibilityCode, ExpiresAfterDays: c.ExpiresAfterDays,
		DiscountType: c.DiscountType, DiscountValue: c.DiscountValue, DiscountCapAmount: c.DiscountCapAmount,
		DurationIntervals: c.DurationIntervals, RequiresApproval: c.RequiresApproval,
	}
}

func timeKey(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ContentHash is the SHA-256 of the version's commercial content: the exact
// price matrix, terms, trial and capabilities, independent of row order.
// Lifecycle evidence (who submitted, when approved) is deliberately excluded:
// the hash answers "is this still the price that was approved", not "who
// touched it".
func ContentHash(v *PriceVersion) string {
	m := priceManifest{
		PriceVersionID: v.PriceVersionID, ProductID: v.ProductID, VersionNumber: v.VersionNumber,
		DisplayName: v.DisplayName, BillingInterval: v.BillingInterval, BillingIntervalCount: v.BillingIntervalCount,
		CurrencyCode: v.CurrencyCode, MarketCodes: append([]string(nil), v.MarketCodes...),
		EffectiveFrom: timeKey(v.EffectiveFrom),
	}
	sort.Strings(m.MarketCodes)
	if v.EffectiveTo != nil {
		s := timeKey(*v.EffectiveTo)
		m.EffectiveTo = &s
	}
	if v.Terms != nil {
		m.Terms = &manifestTerms{
			TermsDocumentRef: v.Terms.TermsDocumentRef, TermsDocumentSHA256: v.Terms.TermsDocumentSHA256,
			AutoRenew: v.Terms.AutoRenew, RenewalNoticeDays: v.Terms.RenewalNoticeDays,
			MinimumTermIntervals: v.Terms.MinimumTermIntervals, Trial: v.Terms.Trial,
		}
	}
	m.Components = make([]manifestComponent, len(v.Components))
	for i, c := range v.Components {
		m.Components[i] = componentManifest(c)
	}
	sort.Slice(m.Components, func(i, j int) bool { return m.Components[i].ComponentKey < m.Components[j].ComponentKey })
	m.Capabilities = append([]PlanCapability(nil), v.Capabilities...)
	sort.Slice(m.Capabilities, func(i, j int) bool { return m.Capabilities[i].CapabilityKey < m.Capabilities[j].CapabilityKey })

	b, err := json.Marshal(m)
	if err != nil {
		// Every field is a plain value; Marshal cannot fail on this struct.
		panic("price manifest marshal: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── CompareVersions ──────────────────────────────────────────────────────────

func optStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func optTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return timeKey(*t)
}

// CompareVersions reports what changed between two versions of one product.
func CompareVersions(base, target *PriceVersion) (*VersionDiff, error) {
	if base.ProductID != target.ProductID {
		return nil, ErrVersionsNotComparable
	}
	d := &VersionDiff{
		BasePriceVersionID: base.PriceVersionID, TargetPriceVersionID: target.PriceVersionID,
		Fields: []FieldChange{}, ComponentsAdded: []string{}, ComponentsRemoved: []string{},
		ComponentsChanged: []string{}, CapabilitiesChanged: []FieldChange{},
	}
	field := func(name, before, after string) {
		if before != after {
			d.Fields = append(d.Fields, FieldChange{Field: name, Before: before, After: after})
		}
	}
	field("display_name", base.DisplayName, target.DisplayName)
	field("billing_interval", base.BillingInterval, target.BillingInterval)
	field("billing_interval_count", strconv.Itoa(base.BillingIntervalCount), strconv.Itoa(target.BillingIntervalCount))
	field("currency_code", base.CurrencyCode, target.CurrencyCode)
	field("market_codes", strings.Join(base.MarketCodes, ","), strings.Join(target.MarketCodes, ","))
	field("effective_from", timeKey(base.EffectiveFrom), timeKey(target.EffectiveFrom))
	field("effective_to", optTime(base.EffectiveTo), optTime(target.EffectiveTo))
	field("commercial_terms", termsKey(base.Terms), termsKey(target.Terms))

	baseC := componentIndex(base.Components)
	targetC := componentIndex(target.Components)
	for k, bj := range baseC {
		tj, ok := targetC[k]
		switch {
		case !ok:
			d.ComponentsRemoved = append(d.ComponentsRemoved, k)
		case bj != tj:
			d.ComponentsChanged = append(d.ComponentsChanged, k)
		}
	}
	for k := range targetC {
		if _, ok := baseC[k]; !ok {
			d.ComponentsAdded = append(d.ComponentsAdded, k)
		}
	}
	sort.Strings(d.ComponentsAdded)
	sort.Strings(d.ComponentsRemoved)
	sort.Strings(d.ComponentsChanged)

	baseCap := capabilityIndex(base.Capabilities)
	targetCap := capabilityIndex(target.Capabilities)
	keys := map[string]bool{}
	for k := range baseCap {
		keys[k] = true
	}
	for k := range targetCap {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if baseCap[k] != targetCap[k] {
			d.CapabilitiesChanged = append(d.CapabilitiesChanged, FieldChange{Field: k, Before: baseCap[k], After: targetCap[k]})
		}
	}
	return d, nil
}

func termsKey(t *CommercialTerms) string {
	if t == nil {
		return ""
	}
	b, _ := json.Marshal(manifestTerms{
		TermsDocumentRef: t.TermsDocumentRef, TermsDocumentSHA256: t.TermsDocumentSHA256, AutoRenew: t.AutoRenew,
		RenewalNoticeDays: t.RenewalNoticeDays, MinimumTermIntervals: t.MinimumTermIntervals, Trial: t.Trial,
	})
	return string(b)
}

func componentIndex(cs []PriceComponent) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		b, _ := json.Marshal(componentManifest(c))
		out[c.ComponentKey] = string(b)
	}
	return out
}

func capabilityIndex(cs []PlanCapability) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		v := "unlimited"
		if c.LimitValue != nil {
			v = strconv.FormatInt(*c.LimitValue, 10) + " " + optStr(c.LimitUnit)
		}
		out[c.CapabilityKey] = v
	}
	return out
}
