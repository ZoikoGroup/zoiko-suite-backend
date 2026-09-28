// COM-02 Subscription (ZS-SVC-Q-001 §4.2). Distinct from the doc7
// CommercialSubscription in subscription_types.go, which stays for its
// existing rows; an account holds a live subscription in one model or the
// other, never both.
package domain

import (
	"fmt"
	"sort"
	"time"

	"zoiko.io/commercial-account-svc/internal/money"
)

const (
	PrefixSubscription        = "csub_"
	PrefixSubscriptionVersion = "csv_"
	PrefixCommercialChange    = "cchg_"
)

// LifecycleStatus is the §6 subscription lifecycle dimension only. Billing
// health (current, past due, collections) and entitlement are separate
// dimensions owned by COM-05 and COM-03.
type LifecycleStatus string

const (
	LifecycleTrialing      LifecycleStatus = "TRIALING"
	LifecyclePending       LifecycleStatus = "PENDING"
	LifecycleActive        LifecycleStatus = "ACTIVE"
	LifecycleCancelPending LifecycleStatus = "CANCEL_PENDING"
	LifecycleCanceled      LifecycleStatus = "CANCELED"
	LifecycleExpired       LifecycleStatus = "EXPIRED"
)

// Terminal reports whether the status ends the subscription.
func (s LifecycleStatus) Terminal() bool { return s == LifecycleCanceled || s == LifecycleExpired }

// ChangeType records why a version exists.
type ChangeType string

const (
	ChangeStarted               ChangeType = "STARTED"
	ChangeTrialConversion       ChangeType = "TRIAL_CONVERSION"
	ChangeTrialExpiry           ChangeType = "TRIAL_EXPIRY"
	ChangeActivated             ChangeType = "ACTIVATED"
	ChangeCancellationScheduled ChangeType = "CANCELLATION_SCHEDULED"
	ChangeCancellationEffective ChangeType = "CANCELLATION_EFFECTIVE"
	ChangeCanceledNow           ChangeType = "CANCELED_NOW"
	ChangeReactivated           ChangeType = "REACTIVATED"
	ChangeTermExpiry            ChangeType = "TERM_EXPIRY"
)

// Channel is how a change was made. An ASSISTED change is made by a
// ZoikoSuite operator and must carry the customer's basis for it.
type Channel string

const (
	ChannelSelfService Channel = "SELF_SERVICE"
	ChannelAssisted    Channel = "ASSISTED"
	// ChannelSystem marks events raised by the boundary worker. It is never
	// written on a version: the worker only adds renewal terms.
	ChannelSystem Channel = "SYSTEM"
)

// Subscription is the stable identity. Its state lives in its versions.
type Subscription struct {
	SubscriptionID       string     `json:"subscription_id"`
	OrganizationID       string     `json:"organization_id"`
	CommercialAccountID  string     `json:"commercial_account_id"`
	ProductID            string     `json:"product_id"`
	CurrencyCode         string     `json:"currency_code"`
	MarketCode           string     `json:"market_code"`
	BillingSource        string     `json:"billing_source"`
	Channel              Channel    `json:"channel"`
	CustomerBasisRef     *string    `json:"customer_basis_ref,omitempty"`
	PaymentMethodRef     *string    `json:"payment_method_ref,omitempty"`
	StartsAt             time.Time  `json:"starts_at"`
	EndsAt               *time.Time `json:"ends_at,omitempty"`
	RowVersion           int        `json:"row_version"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
}

type SubscriptionItemQuantity struct {
	ComponentKey string `json:"component_key"`
	Quantity     string `json:"quantity"`
}

// SubscriptionItem binds one product to an immutable price version.
type SubscriptionItem struct {
	ItemNo              int                        `json:"item_no"`
	ItemRole            string                     `json:"item_role"`
	PriceVersionID      string                     `json:"price_version_id"`
	PriceContentSHA256  string                     `json:"price_content_sha256"`
	AcceptedTermsSHA256 string                     `json:"accepted_terms_sha256"`
	Quantities          []SubscriptionItemQuantity `json:"quantities"`
}

// SubscriptionVersion is one immutable, effective-dated state of the
// subscription with its full commercial configuration. EffectiveTo is
// derived from the next non-voided version and never stored.
type SubscriptionVersion struct {
	SubscriptionVersionID string             `json:"subscription_version_id"`
	SubscriptionID        string             `json:"subscription_id"`
	VersionNumber         int                `json:"version_number"`
	LifecycleStatus       LifecycleStatus    `json:"lifecycle_status"`
	ChangeType            ChangeType         `json:"change_type"`
	EffectiveFrom         time.Time          `json:"effective_from"`
	EffectiveTo           *time.Time         `json:"effective_to,omitempty"`
	TrialEndsAt           *time.Time         `json:"trial_ends_at,omitempty"`
	ChangeID              string             `json:"change_id"`
	Reason                *string            `json:"reason,omitempty"`
	Channel               Channel            `json:"channel"`
	CustomerBasisRef      *string            `json:"customer_basis_ref,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
	CreatedByPrincipalID  string             `json:"created_by_principal_id"`
	VoidedAt              *time.Time         `json:"voided_at,omitempty"`
	VoidedByPrincipalID   *string            `json:"voided_by_principal_id,omitempty"`
	VoidReason            *string            `json:"void_reason,omitempty"`
	VoidedByChangeID      *string            `json:"voided_by_change_id,omitempty"`
	Items                 []SubscriptionItem `json:"items"`
}

// SubscriptionTerm is one renewal term (RenewalTerm), with the commercial
// terms it was taken under.
type SubscriptionTerm struct {
	TermNo               int        `json:"term_no"`
	StartsAt             time.Time  `json:"starts_at"`
	EndsAt               time.Time  `json:"ends_at"`
	PriceVersionID       string     `json:"price_version_id"`
	AutoRenew            bool       `json:"auto_renew"`
	RenewalNoticeDays    int        `json:"renewal_notice_days"`
	MinimumTermIntervals int        `json:"minimum_term_intervals"`
	ChangeID             string     `json:"change_id"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	VoidedAt             *time.Time `json:"voided_at,omitempty"`
	VoidReason           *string    `json:"void_reason,omitempty"`
}

// SubscriptionView is a subscription as of one instant.
type SubscriptionView struct {
	Subscription
	AsOf        time.Time             `json:"as_of"`
	Status      *LifecycleStatus      `json:"status"`
	Effective   *SubscriptionVersion  `json:"effective_version"`
	Scheduled   []SubscriptionVersion `json:"scheduled_versions"`
	CurrentTerm *SubscriptionTerm     `json:"current_term,omitempty"`
}

// RenewalState answers GetRenewalState.
type RenewalState struct {
	SubscriptionID               string            `json:"subscription_id"`
	AsOf                         time.Time         `json:"as_of"`
	Status                       *LifecycleStatus  `json:"status"`
	CurrentTerm                  *SubscriptionTerm `json:"current_term,omitempty"`
	AutoRenew                    bool              `json:"auto_renew"`
	RenewalDue                   bool              `json:"renewal_due"`
	NextTermStartsAt             *time.Time        `json:"next_term_starts_at,omitempty"`
	EndsAt                       *time.Time        `json:"ends_at,omitempty"`
	MinimumTermEndsAt            *time.Time        `json:"minimum_term_ends_at,omitempty"`
	EarliestCancellationBoundary *time.Time        `json:"earliest_cancellation_boundary,omitempty"`
}

// AddOnSelection is one add-on in a StartSubscription request.
type AddOnSelection struct {
	ProductCode         string
	Quantities          map[string]string
	AcceptedTermsSHA256 string
}

// StartSubscriptionParams is a validated StartSubscription command. The
// currency and market are not here: they are resolved from the commercial
// account, and the price version from the sellable offer.
type StartSubscriptionParams struct {
	SubscriptionID         string
	ChangeID               string
	CommercialAccountID    string
	PlanProductCode        string
	Quantities             map[string]string
	AcceptedTermsSHA256    string
	ExpectedPriceVersionID *string
	AddOns                 []AddOnSelection
	StartsAt               time.Time
	Channel                Channel
	CustomerBasisRef       *string
	PaymentMethodRef       *string
	Actor                  string
	Now                    time.Time
}

// SubscriptionCommand carries the fields every lifecycle command shares.
type SubscriptionCommand struct {
	SubscriptionID   string
	ExpectedVersion  int
	ChangeID         string
	Actor            string
	Channel          Channel
	CustomerBasisRef *string
	Reason           string
	Now              time.Time
}

var (
	ErrAccountMarketNotSet        = errorString("the commercial account has no market set; a ZoikoSuite operator must set it before a subscription can be sold")
	ErrAccountNotActive           = errorString("the commercial account is not active")
	ErrWrongProductKind           = errorString("product kind does not fit this position (a plan must be PLAN, an add-on ADD_ON)")
	ErrOfferChanged               = errorString("the sellable offer changed since it was shown; re-read it and confirm again")
	ErrTermsNotAccepted           = errorString("accepted_terms_sha256 does not match the terms of the offer being bought")
	ErrPaymentMethodRequired      = errorString("this offer's trial requires a payment method reference")
	ErrSubscriptionOverlap        = errorString("the account already has a subscription for an overlapping period")
	ErrLegacySubscriptionLive     = errorString("the account already has a live subscription in the other subscription model")
	ErrSubscriptionInvalidState   = errorString("subscription is not in a state that allows this action")
	ErrSellerActivationRequired   = errorString("a pending subscription is activated by ZoikoSuite, not by the customer")
	ErrMinimumTermNotMet          = errorString("the minimum term has not been served; schedule the cancellation instead")
	ErrRenewalNotDue              = errorString("the current term has not ended; an auto-renewing subscription renews at its term end")
	ErrSubscriptionEnded          = errorString("the subscription has ended")
	ErrConversionAlreadyScheduled = errorString("the trial already converts to a paid subscription at its end")
)

// ── Billing interval arithmetic ──────────────────────────────────────────────

func intervalMonths(interval string) int {
	switch interval {
	case "MONTH":
		return 1
	case "QUARTER":
		return 3
	case "YEAR":
		return 12
	}
	panic("unknown billing interval " + interval)
}

// AddBillingIntervals returns the end of the n-th billing interval counted
// from anchor. It is always computed from the anchor, never by chaining, and
// a day that does not exist in the target month is clamped to that month's
// last day: a term anchored on 31 January ends on 28/29 February, then on
// 31 March — it does not drift to the 28th forever.
func AddBillingIntervals(anchor time.Time, interval string, count, n int) time.Time {
	a := anchor.UTC()
	total := int(a.Month()) - 1 + intervalMonths(interval)*count*n
	y := a.Year() + total/12
	m := time.Month(total%12 + 1)
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	d := a.Day()
	if d > last {
		d = last
	}
	return time.Date(y, m, d, a.Hour(), a.Minute(), a.Second(), a.Nanosecond(), time.UTC)
}

// TermEnd is the end of term k (1-based) of a subscription anchored at anchor.
func TermEnd(plan *PriceVersion, anchor time.Time, k int) time.Time {
	return AddBillingIntervals(anchor, plan.BillingInterval, plan.BillingIntervalCount, k)
}

// ── Lifecycle planning ───────────────────────────────────────────────────────

type PlannedVersion struct {
	Status        LifecycleStatus
	ChangeType    ChangeType
	EffectiveFrom time.Time
	TrialEndsAt   *time.Time
}

type PlannedTerm struct {
	StartsAt time.Time
	EndsAt   time.Time
}

// LifecyclePlan is a set of versions and terms one command writes, and the resulting
// subscription end (nil when nothing ends it).
type LifecyclePlan struct {
	Versions []PlannedVersion
	Terms    []PlannedTerm
	EndsAt   *time.Time
}

// PlanActivation schedules paid activation at `at`: an ACTIVE version, the
// first term, and — when the terms do not auto-renew — the expiry at that
// term's end.
func PlanActivation(plan *PriceVersion, at time.Time, why ChangeType) LifecyclePlan {
	end := TermEnd(plan, at, 1)
	p := LifecyclePlan{
		Versions: []PlannedVersion{{Status: LifecycleActive, ChangeType: why, EffectiveFrom: at}},
		Terms:    []PlannedTerm{{StartsAt: at, EndsAt: end}},
	}
	if !plan.Terms.AutoRenew {
		p.Versions = append(p.Versions, PlannedVersion{Status: LifecycleExpired, ChangeType: ChangeTermExpiry, EffectiveFrom: end})
		p.EndsAt = &end
	}
	return p
}

// PlanStart schedules a new subscription. Without a trial it is PENDING until
// ZoikoSuite activates it. With a trial it is TRIALING, and what happens at
// the trial's end is exactly the disclosed conversion policy — nothing is
// assumed (§4.1: "no undocumented auto-convert behavior").
func PlanStart(plan *PriceVersion, startsAt time.Time) LifecyclePlan {
	if plan.Terms.Trial == nil {
		return LifecyclePlan{Versions: []PlannedVersion{{Status: LifecyclePending, ChangeType: ChangeStarted, EffectiveFrom: startsAt}}}
	}
	trialEnd := startsAt.Add(time.Duration(plan.Terms.Trial.DurationDays) * 24 * time.Hour)
	trialing := PlannedVersion{Status: LifecycleTrialing, ChangeType: ChangeStarted, EffectiveFrom: startsAt, TrialEndsAt: &trialEnd}
	if plan.Terms.Trial.Conversion == "CONVERT_TO_PAID" {
		p := PlanActivation(plan, trialEnd, ChangeTrialConversion)
		p.Versions = append([]PlannedVersion{trialing}, p.Versions...)
		return p
	}
	return LifecyclePlan{
		Versions: []PlannedVersion{trialing, {Status: LifecycleExpired, ChangeType: ChangeTrialExpiry, EffectiveFrom: trialEnd}},
		EndsAt:   &trialEnd,
	}
}

// ── Quantities ───────────────────────────────────────────────────────────────

// ValidateQuantities checks the quantity basis for every PER_UNIT component
// of a price version and applies its rounding rule. Every PER_UNIT component
// needs a quantity, no other component takes one, and the result must sit
// inside the component's minimum and maximum.
func ValidateQuantities(pv *PriceVersion, in map[string]string) ([]SubscriptionItemQuantity, error) {
	out := []SubscriptionItemQuantity{}
	perUnit := map[string]bool{}
	for _, c := range pv.Components {
		if c.ComponentType != ComponentPerUnit {
			continue
		}
		perUnit[c.ComponentKey] = true
		field := "quantities." + c.ComponentKey
		raw, ok := in[c.ComponentKey]
		if !ok {
			return nil, invalid(field, "a quantity is required for this per-unit component")
		}
		q, err := money.Parse(raw)
		if err != nil {
			return nil, invalid(field, "must be a non-negative decimal with at most 4 fractional digits")
		}
		switch *c.QuantityRounding {
		case "UP":
			q = q.Ceil()
		case "DOWN":
			q = q.Floor()
		}
		if c.MinimumQuantity != nil {
			lo, _ := money.Parse(*c.MinimumQuantity)
			if q.Cmp(lo) < 0 {
				return nil, invalid(field, fmt.Sprintf("%s is below the minimum of %s", q.String(), *c.MinimumQuantity))
			}
		}
		if c.MaximumQuantity != nil {
			hi, _ := money.Parse(*c.MaximumQuantity)
			if q.Cmp(hi) > 0 {
				return nil, invalid(field, fmt.Sprintf("%s is above the maximum of %s", q.String(), *c.MaximumQuantity))
			}
		}
		out = append(out, SubscriptionItemQuantity{ComponentKey: c.ComponentKey, Quantity: q.String()})
	}
	for k := range in {
		if !perUnit[k] {
			return nil, invalid("quantities."+k, "no per-unit component with this key exists on the offer")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ComponentKey < out[j].ComponentKey })
	return out, nil
}
