// COM-01 Product & Price Book (ZS-SVC-Q-001 §4.1).
package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Commercial identifiers ───────────────────────────────────────────────────

// Commercial-plane identifiers carry a type prefix. Every tenant-plane service
// in this platform keys on bare UUIDs, so a bare UUID offered where a
// commercial identifier is expected is refused as cross-plane (§7 isolation
// invariant, negative path #01) instead of being looked up and quietly "not
// found".
const (
	PrefixProduct        = "cprod_"
	PrefixPriceVersion   = "cpv_"
	PrefixPriceComponent = "cpc_"
)

var (
	uuidPattern          = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	prefixedUUIDPattern  = regexp.MustCompile(`^([a-z]+_)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	productCodePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	componentKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	capabilityKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{1,127}$`)
	marketCodePattern    = regexp.MustCompile(`^[A-Z]{2,8}$`)
	currencyCodePattern  = regexp.MustCompile(`^[A-Z]{3}$`)
	sha256HexPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	opaqueCodePattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	meterKeyPattern      = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
)

// NewCommercialID mints an identifier of the given commercial kind.
func NewCommercialID(prefix string) string { return prefix + uuid.NewString() }

// ParseCommercialID checks that raw is an identifier of the expected kind.
func ParseCommercialID(prefix, raw string) (string, error) {
	if uuidPattern.MatchString(strings.ToLower(raw)) {
		return "", ErrCrossPlaneIdentifier
	}
	m := prefixedUUIDPattern.FindStringSubmatch(raw)
	if m == nil || m[1] != prefix {
		return "", fmt.Errorf("%w: expected a %s identifier", ErrInvalidCommercialID, strings.TrimSuffix(prefix, "_"))
	}
	return raw, nil
}

// ── Enumerations ─────────────────────────────────────────────────────────────

type ProductKind string

const (
	ProductKindPlan  ProductKind = "PLAN"
	ProductKindAddOn ProductKind = "ADD_ON"
)

type PriceVersionStatus string

const (
	PriceVersionDraft     PriceVersionStatus = "DRAFT"
	PriceVersionReview    PriceVersionStatus = "REVIEW"
	PriceVersionApproved  PriceVersionStatus = "APPROVED"
	PriceVersionPublished PriceVersionStatus = "PUBLISHED"
	PriceVersionRetired   PriceVersionStatus = "RETIRED"
)

type ComponentType string

const (
	ComponentRecurringFixed ComponentType = "RECURRING_FIXED"
	ComponentPerUnit        ComponentType = "PER_UNIT"
	ComponentMetered        ComponentType = "METERED"
	ComponentOneTime        ComponentType = "ONE_TIME"
	ComponentDiscount       ComponentType = "DISCOUNT"
)

var (
	billingIntervals   = set("MONTH", "QUARTER", "YEAR")
	billingTimings     = set("IN_ADVANCE", "IN_ARREARS")
	quantityRoundings  = set("NONE", "UP", "DOWN")
	tierModes          = set("VOLUME", "GRADUATED")
	aggregationMethods = set("SUM", "MAX", "LAST", "UNIQUE_COUNT")
	discountTypes      = set("PERCENT", "FIXED_AMOUNT")
	trialConversions   = set("CONVERT_TO_PAID", "CANCEL_AT_END", "REQUIRE_CONFIRMATION")
)

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// ── Model ────────────────────────────────────────────────────────────────────

// CommercialCurrency is sale-eligibility and minor-unit data for one currency.
type CommercialCurrency struct {
	CurrencyCode         string    `json:"currency_code"`
	MinorUnits           int       `json:"minor_units"`
	SaleEnabled          bool      `json:"sale_enabled"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	UpdatedAt            time.Time `json:"updated_at"`
	UpdatedByPrincipalID string    `json:"updated_by_principal_id"`
}

// Product is the stable sellable identity. Its commercial content — label,
// prices, terms — lives on ProductPriceVersion.
type Product struct {
	ProductID            string      `json:"product_id"`
	ProductCode          string      `json:"product_code"`
	ProductKind          ProductKind `json:"product_kind"`
	CreatedAt            time.Time   `json:"created_at"`
	CreatedByPrincipalID string      `json:"created_by_principal_id"`
}

// PriceTier is one band of a tiered PER_UNIT or METERED component. A nil
// UpToQuantity is the final, unbounded band.
type PriceTier struct {
	TierIndex    int     `json:"tier_index"`
	UpToQuantity *string `json:"up_to_quantity"`
	UnitAmount   string  `json:"unit_amount"`
	FlatAmount   *string `json:"flat_amount,omitempty"`
}

// PriceComponent is one line of a price version's matrix. Which attributes
// are set depends on ComponentType; the schema enforces the same shape.
type PriceComponent struct {
	PriceComponentID     string        `json:"price_component_id"`
	PriceVersionID       string        `json:"price_version_id"`
	ComponentKey         string        `json:"component_key"`
	ComponentType        ComponentType `json:"component_type"`
	Amount               *string       `json:"amount,omitempty"`
	BillingTiming        *string       `json:"billing_timing,omitempty"`
	UnitName             *string       `json:"unit_name,omitempty"`
	IncludedQuantity     *string       `json:"included_quantity,omitempty"`
	MinimumQuantity      *string       `json:"minimum_quantity,omitempty"`
	MaximumQuantity      *string       `json:"maximum_quantity,omitempty"`
	QuantityRounding     *string       `json:"quantity_rounding,omitempty"`
	TierMode             *string       `json:"tier_mode,omitempty"`
	Tiers                []PriceTier   `json:"tiers,omitempty"`
	MeterKey             *string       `json:"meter_key,omitempty"`
	MeterVersion         *int          `json:"meter_version,omitempty"`
	AggregationMethod    *string       `json:"aggregation_method,omitempty"`
	TriggerEvent         *string       `json:"trigger_event,omitempty"`
	EligibilityCode      *string       `json:"eligibility_code,omitempty"`
	ExpiresAfterDays     *int          `json:"expires_after_days,omitempty"`
	DiscountType         *string       `json:"discount_type,omitempty"`
	DiscountValue        *string       `json:"discount_value,omitempty"`
	DiscountCapAmount    *string       `json:"discount_cap_amount,omitempty"`
	DurationIntervals    *int          `json:"duration_intervals,omitempty"`
	RequiresApproval     *bool         `json:"requires_approval,omitempty"`
	CreatedAt            time.Time     `json:"created_at"`
	CreatedByPrincipalID string        `json:"created_by_principal_id"`
}

// TrialPolicy is the disclosed trial for a price version (COM-CTRL-006).
// No trial exists unless one is stated.
type TrialPolicy struct {
	DurationDays          int    `json:"duration_days"`
	Conversion            string `json:"conversion"`
	PaymentMethodRequired bool   `json:"payment_method_required"`
}

// CommercialTerms is the CommercialTermVersion bound to a price version.
type CommercialTerms struct {
	TermsDocumentRef     string       `json:"terms_document_ref"`
	TermsDocumentSHA256  string       `json:"terms_document_sha256"`
	AutoRenew            bool         `json:"auto_renew"`
	RenewalNoticeDays    int          `json:"renewal_notice_days"`
	MinimumTermIntervals int          `json:"minimum_term_intervals"`
	Trial                *TrialPolicy `json:"trial,omitempty"`
	SetAt                time.Time    `json:"set_at"`
	SetByPrincipalID     string       `json:"set_by_principal_id"`
}

// PlanCapability is one row of the plan capability matrix. A nil LimitValue
// means unlimited. COM-03 reads this; it grants nothing by itself.
type PlanCapability struct {
	CapabilityKey string  `json:"capability_key"`
	LimitValue    *int64  `json:"limit_value"`
	LimitUnit     *string `json:"limit_unit,omitempty"`
}

// PriceVersion is an immutable-once-submitted ProductPriceVersion together
// with its components and capabilities.
type PriceVersion struct {
	PriceVersionID           string             `json:"price_version_id"`
	ProductID                string             `json:"product_id"`
	ProductCode              string             `json:"product_code"`
	ProductKind              ProductKind        `json:"product_kind"`
	VersionNumber            int                `json:"version_number"`
	SupersedesPriceVersionID *string            `json:"supersedes_price_version_id,omitempty"`
	DisplayName              string             `json:"display_name"`
	BillingInterval          string             `json:"billing_interval"`
	BillingIntervalCount     int                `json:"billing_interval_count"`
	CurrencyCode             string             `json:"currency_code"`
	MarketCodes              []string           `json:"market_codes"`
	EffectiveFrom            time.Time          `json:"effective_from"`
	EffectiveTo              *time.Time         `json:"effective_to,omitempty"`
	ChangeReason             string             `json:"change_reason"`
	Terms                    *CommercialTerms   `json:"commercial_terms,omitempty"`
	Status                   PriceVersionStatus `json:"status"`
	RowVersion               int                `json:"row_version"`
	ContentSHA256            *string            `json:"content_sha256,omitempty"`

	CreatedAt                 time.Time  `json:"created_at"`
	CreatedByPrincipalID      string     `json:"created_by_principal_id"`
	SubmittedAt               *time.Time `json:"submitted_at,omitempty"`
	SubmittedByPrincipalID    *string    `json:"submitted_by_principal_id,omitempty"`
	ApprovedAt                *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID     *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedContentSHA256     *string    `json:"approved_content_sha256,omitempty"`
	PublishedAt               *time.Time `json:"published_at,omitempty"`
	PublishedByPrincipalID    *string    `json:"published_by_principal_id,omitempty"`
	RetiredAt                 *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID      *string    `json:"retired_by_principal_id,omitempty"`
	RetireReason              *string    `json:"retire_reason,omitempty"`
	LastRejectedAt            *time.Time `json:"last_rejected_at,omitempty"`
	LastRejectedByPrincipalID *string    `json:"last_rejected_by_principal_id,omitempty"`
	LastRejectionReason       *string    `json:"last_rejection_reason,omitempty"`

	Components   []PriceComponent `json:"components"`
	Capabilities []PlanCapability `json:"capabilities"`
}

// ProductSummary is one product as tenants see it: sellable identity plus
// the labels of its currently published versions.
type ProductSummary struct {
	ProductID   string      `json:"product_id"`
	ProductCode string      `json:"product_code"`
	ProductKind ProductKind `json:"product_kind"`
	DisplayName string      `json:"display_name"`
	Currencies  []string    `json:"currencies"`
}

// SellableOfferFilter narrows ResolveSellableOffers. Empty fields do not filter.
type SellableOfferFilter struct {
	ProductCode  string
	CurrencyCode string
	MarketCode   string
}

// VersionDiff is CompareVersions' result: what changed from Base to Target.
type VersionDiff struct {
	BasePriceVersionID   string        `json:"base_price_version_id"`
	TargetPriceVersionID string        `json:"target_price_version_id"`
	Fields               []FieldChange `json:"fields"`
	ComponentsAdded      []string      `json:"components_added"`
	ComponentsRemoved    []string      `json:"components_removed"`
	ComponentsChanged    []string      `json:"components_changed"`
	CapabilitiesChanged  []FieldChange `json:"capabilities_changed"`
}

type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// IdempotencyClaim is recorded in the same transaction as the change it
// guards. OwnerScope is "seller" for price-book commands.
type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

// SellerScope is the idempotency owner scope for seller-plane commands.
const SellerScope = "seller"

// ── Errors ───────────────────────────────────────────────────────────────────

var (
	ErrCrossPlaneIdentifier     = errorString("identifier belongs to the tenant plane, not the commercial plane")
	ErrInvalidCommercialID      = errorString("invalid commercial identifier")
	ErrProductNotFound          = errorString("product not found")
	ErrProductCodeTaken         = errorString("product_code already exists")
	ErrPriceVersionNotFound     = errorString("price version not found")
	ErrPriceComponentNotFound   = errorString("price component not found")
	ErrCurrencyNotFound         = errorString("commercial currency not found")
	ErrCurrencyNotEnabled       = errorString("currency is not enabled for sale")
	ErrPriceVersionImmutable    = errorString("price version content is immutable once submitted")
	ErrPriceVersionInvalidState = errorString("price version is not in a state that allows this action")
	ErrVersionConflict          = errorString("expected_version does not match the current row_version")
	ErrSoDViolation             = errorString("approver must be independent of the version's creator and submitter")
	ErrInFlightVersionExists    = errorString("product already has a price version in DRAFT, REVIEW or APPROVED")
	ErrMeterNotRegistered       = errorString("metered components need a registered COM-04 meter definition, which does not exist yet")
	ErrContentHashMismatch      = errorString("price version content no longer matches the hash that was submitted or approved")
	ErrIdempotencyKeyReused     = errorString("idempotency key was already used for a different request")
	ErrVersionsNotComparable    = errorString("versions belong to different products")
	ErrPriceVersionNotSellable  = errorString("price version is not published and effective")
)

// ValidationError is a refused command input, naming the field at fault.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, reason string) error { return &ValidationError{Field: field, Reason: reason} }

// PublicationBlockedError lists every reason a version cannot move forward,
// so an operator fixes them in one pass rather than one refusal at a time.
type PublicationBlockedError struct {
	Reasons []string
}

func (e *PublicationBlockedError) Error() string {
	return "publication blocked: " + strings.Join(e.Reasons, "; ")
}

// IdempotentReplayError reports that the idempotency key was already used for
// this exact request. The caller answers with the existing resource.
type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string { return "idempotent replay of " + e.ResourceID }
