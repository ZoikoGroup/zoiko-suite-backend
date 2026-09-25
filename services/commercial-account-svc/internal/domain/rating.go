// Recurring rating, proration and change quotes for COM-02 plan changes
// (ZS-SVC-Q-001 §4.2 plan transition rules; COM-CTRL-008, COM-CTRL-009).
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"time"

	"zoiko.io/commercial-account-svc/internal/money"
)

const PrefixTransitionRule = "ctr_"

// ChangeKind classifies a configuration change.
type ChangeKind string

const (
	ChangeKindPlan     ChangeKind = "PLAN_CHANGE"
	ChangeKindQuantity ChangeKind = "QUANTITY_CHANGE"
	ChangeKindAddOn    ChangeKind = "ADD_ON_CHANGE"
)

// Change types written on the versions a configuration change creates.
const (
	ChangePlanChanged     ChangeType = "PLAN_CHANGED"
	ChangeQuantityChanged ChangeType = "QUANTITY_CHANGED"
	ChangeAddOnChanged    ChangeType = "ADD_ON_CHANGED"
)

const (
	TimingImmediate   = "IMMEDIATE"
	TimingNextRenewal = "NEXT_RENEWAL"

	// ProrationDailyHalfEven credits the unused whole days of the old
	// configuration and charges the same days of the new one:
	//   amount = period_charge × days_remaining ÷ days_in_term
	// computed exactly, each side rounded half-even to the currency's minor
	// units, net = charge − credit. days_remaining counts only whole days
	// left in the term; the day of the change is served by the old
	// configuration.
	ProrationDailyHalfEven = "DAILY_HALF_EVEN"
	// ProrationNone applies the change without a proration line.
	ProrationNone = "NONE"
)

// PlanTransitionRule is product policy as data: which plan may change into
// which, when the change takes effect, and how it is prorated. A change with
// no rule is refused. A rule from a product to itself governs quantity and
// add-on changes on that plan.
type PlanTransitionRule struct {
	RuleID               string     `json:"rule_id"`
	FromProductID        string     `json:"from_product_id"`
	FromProductCode      string     `json:"from_product_code"`
	ToProductID          string     `json:"to_product_id"`
	ToProductCode        string     `json:"to_product_code"`
	Timing               string     `json:"timing"`
	ProrationMethod      string     `json:"proration_method"`
	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	RetiredAt            *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID *string    `json:"retired_by_principal_id,omitempty"`
	RetireReason         *string    `json:"retire_reason,omitempty"`
}

// ValidateTransitionRule checks a new rule. A change deferred to the next
// renewal starts a fresh term at the new price, so there is nothing to
// prorate.
func ValidateTransitionRule(timing, method string) error {
	if timing != TimingImmediate && timing != TimingNextRenewal {
		return invalid("timing", "must be IMMEDIATE or NEXT_RENEWAL")
	}
	if method != ProrationDailyHalfEven && method != ProrationNone {
		return invalid("proration_method", "must be DAILY_HALF_EVEN or NONE")
	}
	if timing == TimingNextRenewal && method != ProrationNone {
		return invalid("proration_method", "a change at the next renewal is never prorated; use NONE")
	}
	return nil
}

// ChangeRequest describes the configuration a customer wants. Existing items
// keep their bound price versions; only a product being added binds to the
// current sellable offer.
type ChangeRequest struct {
	Kind                    ChangeKind
	PlanProductCode         string
	PlanQuantities          map[string]string
	PlanAcceptedTermsSHA256 string
	AddOn                   *AddOnSelection
	RemoveAddOnCode         string
	ForceNextRenewal        bool
}

// Proration is the evidence for one prorated change, sufficient to recompute
// it exactly.
type Proration struct {
	Method        string `json:"method"`
	DaysInTerm    int    `json:"days_in_term"`
	DaysRemaining int    `json:"days_remaining"`
	Credit        string `json:"credit"`
	Charge        string `json:"charge"`
	Net           string `json:"net"`
}

// ChangeQuote is what a customer is shown before confirming a change. Its
// hash binds the confirmation to exactly this outcome (§4.2: treatment "must
// be disclosed before confirmation").
type ChangeQuote struct {
	SubscriptionID         string             `json:"subscription_id"`
	Kind                   ChangeKind         `json:"kind"`
	RuleID                 string             `json:"rule_id"`
	Timing                 string             `json:"timing"`
	ProrationMethod        string             `json:"proration_method"`
	EffectiveAt            time.Time          `json:"effective_at"`
	FromPlanPriceVersionID string             `json:"from_plan_price_version_id"`
	ToPlanPriceVersionID   string             `json:"to_plan_price_version_id"`
	CurrencyCode           string             `json:"currency_code"`
	Items                  []SubscriptionItem `json:"items"`
	OldPeriodCharge        string             `json:"old_period_charge"`
	NewPeriodCharge        string             `json:"new_period_charge"`
	Proration              *Proration         `json:"proration,omitempty"`
	QuoteSHA256            string             `json:"quote_sha256"`
}

// Seal computes the quote hash. An immediate change takes effect at the
// moment it is confirmed, so its exact timestamp is not part of the hash;
// the proration's whole days are, which keeps a quote valid for the rest of
// the day it was shown and no longer.
func (q *ChangeQuote) Seal() {
	shadow := *q
	shadow.QuoteSHA256 = ""
	if q.Timing == TimingImmediate {
		shadow.EffectiveAt = time.Time{}
	}
	b, err := json.Marshal(shadow)
	if err != nil {
		panic("change quote marshal: " + err.Error())
	}
	sum := sha256.Sum256(b)
	q.QuoteSHA256 = hex.EncodeToString(sum[:])
}

// SubscriptionChange is the stored evidence of one configuration change.
type SubscriptionChange struct {
	ChangeID               string     `json:"change_id"`
	SubscriptionID         string     `json:"subscription_id"`
	Kind                   ChangeKind `json:"kind"`
	RuleID                 string     `json:"rule_id"`
	Timing                 string     `json:"timing"`
	ProrationMethod        string     `json:"proration_method"`
	EffectiveAt            time.Time  `json:"effective_at"`
	FromPriceVersionID     string     `json:"from_price_version_id"`
	ToPriceVersionID       string     `json:"to_price_version_id"`
	CurrencyCode           string     `json:"currency_code"`
	OldPeriodCharge        string     `json:"old_period_charge"`
	NewPeriodCharge        string     `json:"new_period_charge"`
	TermStartsAt           time.Time  `json:"term_starts_at"`
	TermEndsAt             time.Time  `json:"term_ends_at"`
	Proration              *Proration `json:"proration,omitempty"`
	QuoteSHA256            string     `json:"quote_sha256"`
	Channel                Channel    `json:"channel"`
	CustomerBasisRef       *string    `json:"customer_basis_ref,omitempty"`
	RequestedByPrincipalID string     `json:"requested_by_principal_id"`
	CreatedAt              time.Time  `json:"created_at"`
}

var (
	ErrTransitionNotAllowed   = errorString("no active transition rule allows this change")
	ErrTransitionRuleExists   = errorString("an active transition rule already exists for this pair of plans")
	ErrTransitionRuleRetired  = errorString("the transition rule is already retired")
	ErrTransitionRuleNotFound = errorString("transition rule not found")
	ErrTimingNotAllowed       = errorString("this change cannot take effect immediately: the new plan bills on a different interval; it can only apply at the next renewal")
	ErrChangeAlreadyScheduled = errorString("a change is already scheduled for this subscription")
	ErrNoChange               = errorString("the requested configuration is the same as the current one")
	ErrQuoteChanged           = errorString("the change no longer matches the quote that was shown; preview it again")
)

// ── Rating ───────────────────────────────────────────────────────────────────

func dec(s string) *big.Rat {
	d, err := money.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("stored amount %q is not a decimal: %v", s, err))
	}
	return d.Rat()
}

// tierCharge prices a billable quantity against tier bands. VOLUME prices
// every unit at the band the whole quantity falls in; GRADUATED prices each
// band's units at that band's rate. A band's flat amount applies once when
// any unit falls in it.
func tierCharge(billable *big.Rat, tiers []PriceTier, mode string) *big.Rat {
	total := new(big.Rat)
	if billable.Sign() == 0 {
		return total
	}
	if mode == "VOLUME" {
		for _, t := range tiers {
			if t.UpToQuantity == nil || billable.Cmp(dec(*t.UpToQuantity)) <= 0 {
				total.Mul(billable, dec(t.UnitAmount))
				if t.FlatAmount != nil {
					total.Add(total, dec(*t.FlatAmount))
				}
				return total
			}
		}
		return total
	}
	prev := new(big.Rat)
	for _, t := range tiers {
		upper := billable
		if t.UpToQuantity != nil && dec(*t.UpToQuantity).Cmp(billable) < 0 {
			upper = dec(*t.UpToQuantity)
		}
		units := new(big.Rat).Sub(upper, prev)
		if units.Sign() <= 0 {
			break
		}
		total.Add(total, new(big.Rat).Mul(units, dec(t.UnitAmount)))
		if t.FlatAmount != nil {
			total.Add(total, dec(*t.FlatAmount))
		}
		prev = upper
		if upper == billable {
			break
		}
	}
	return total
}

// ItemPeriodCharge is one item's recurring charge for one billing interval:
// recurring fixed components plus per-unit components on the quantity above
// what is included. One-time, metered and discount components are not
// recurring subscription charges and are rated elsewhere.
func ItemPeriodCharge(pv *PriceVersion, qty []SubscriptionItemQuantity) (*big.Rat, error) {
	q := map[string]string{}
	for _, x := range qty {
		q[x.ComponentKey] = x.Quantity
	}
	total := new(big.Rat)
	for _, c := range pv.Components {
		switch c.ComponentType {
		case ComponentRecurringFixed:
			total.Add(total, dec(*c.Amount))
		case ComponentPerUnit:
			raw, ok := q[c.ComponentKey]
			if !ok {
				return nil, fmt.Errorf("no quantity for per-unit component %s of %s", c.ComponentKey, pv.PriceVersionID)
			}
			billable := new(big.Rat).Sub(dec(raw), dec(*c.IncludedQuantity))
			if billable.Sign() < 0 {
				billable = new(big.Rat)
			}
			if c.Amount != nil {
				total.Add(total, new(big.Rat).Mul(billable, dec(*c.Amount)))
			} else {
				total.Add(total, tierCharge(billable, c.Tiers, *c.TierMode))
			}
		}
	}
	return total, nil
}

// ConfigurationPeriodCharge sums the recurring charge of every item.
func ConfigurationPeriodCharge(items []SubscriptionItem, pvs map[string]*PriceVersion) (*big.Rat, error) {
	total := new(big.Rat)
	for _, it := range items {
		pv, ok := pvs[it.PriceVersionID]
		if !ok {
			return nil, fmt.Errorf("price version %s not loaded", it.PriceVersionID)
		}
		c, err := ItemPeriodCharge(pv, it.Quantities)
		if err != nil {
			return nil, err
		}
		total.Add(total, c)
	}
	return total, nil
}

// EvidenceAmount formats an unrounded rating result exactly. Four-place unit
// prices times four-place quantities need at most eight places.
func EvidenceAmount(r *big.Rat) string { return money.FormatHalfEven(r, 8) }

// Prorate applies ProrationDailyHalfEven.
func Prorate(oldCharge, newCharge *big.Rat, termStart, termEnd, now time.Time, minorUnits int) Proration {
	day := 24 * time.Hour
	total := int(termEnd.Sub(termStart) / day)
	remaining := int(termEnd.Sub(now) / day)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > total {
		remaining = total
	}
	share := func(r *big.Rat) *big.Rat {
		if total == 0 {
			return new(big.Rat)
		}
		return new(big.Rat).Mul(r, big.NewRat(int64(remaining), int64(total)))
	}
	credit := money.FormatHalfEven(share(oldCharge), minorUnits)
	charge := money.FormatHalfEven(share(newCharge), minorUnits)
	c1, _ := new(big.Rat).SetString(credit)
	c2, _ := new(big.Rat).SetString(charge)
	return Proration{
		Method: ProrationDailyHalfEven, DaysInTerm: total, DaysRemaining: remaining,
		Credit: credit, Charge: charge, Net: money.FormatHalfEven(new(big.Rat).Sub(c2, c1), minorUnits),
	}
}

// ── Term arithmetic across plan changes ──────────────────────────────────────

// TermSpan is one live term with the billing interval it was taken under.
type TermSpan struct {
	StartsAt      time.Time
	EndsAt        time.Time
	Interval      string
	IntervalCount int
}

func sameInterval(t TermSpan, interval string, count int) bool {
	return t.Interval == interval && t.IntervalCount == count
}

// RunAnchor finds the start of the run of consecutive terms, ending at index
// i, that share term i's interval, and term i's 1-based position in it. Term
// ends are always computed from that anchor, so a run anchored on the 31st
// keeps returning to the 31st however many short months it crosses; a plan
// change to a different interval starts a new run.
func RunAnchor(terms []TermSpan, i int) (anchor time.Time, position int) {
	anchor, position = terms[i].StartsAt, 1
	for j := i - 1; j >= 0; j-- {
		if !sameInterval(terms[j], terms[i].Interval, terms[i].IntervalCount) || !terms[j].EndsAt.Equal(terms[j+1].StartsAt) {
			break
		}
		anchor = terms[j].StartsAt
		position++
	}
	return anchor, position
}

// NextTermWindow is the term that follows the last of terms under a plan
// billed every count × interval.
func NextTermWindow(terms []TermSpan, interval string, count int) (start, end time.Time) {
	last := len(terms) - 1
	start = terms[last].EndsAt
	if !sameInterval(terms[last], interval, count) {
		return start, AddBillingIntervals(start, interval, count, 1)
	}
	anchor, position := RunAnchor(terms, last)
	return start, AddBillingIntervals(anchor, interval, count, position+1)
}

// MinimumTermEnd is when the minimum commitment, counted in the first term's
// intervals from the first term's start, has been served.
func MinimumTermEnd(first TermSpan, minimumIntervals int) time.Time {
	return AddBillingIntervals(first.StartsAt, first.Interval, first.IntervalCount, minimumIntervals)
}

// CancellationBoundary is the earliest term end at which a cancellation
// requested at now can take effect: a term end of the current run (term
// index current), after now, and no earlier than notBefore — which the
// caller sets to the later of now + notice and the end of the minimum term.
// A request inside the notice window therefore lands on the following term
// end, not the current one.
func CancellationBoundary(terms []TermSpan, current int, notBefore, now time.Time) time.Time {
	anchor, position := RunAnchor(terms, current)
	t := terms[current]
	for k := position; ; k++ {
		e := AddBillingIntervals(anchor, t.Interval, t.IntervalCount, k)
		if e.After(now) && !e.Before(notBefore) {
			return e
		}
	}
}

// ItemsEqual reports whether two configurations bind the same price versions
// with the same quantities.
func ItemsEqual(a, b []SubscriptionItem) bool {
	key := func(items []SubscriptionItem) string {
		type k struct {
			PV string
			Q  []SubscriptionItemQuantity
		}
		ks := make([]k, len(items))
		for i, it := range items {
			ks[i] = k{it.PriceVersionID, it.Quantities}
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i].PV < ks[j].PV })
		out, _ := json.Marshal(ks)
		return string(out)
	}
	return key(a) == key(b)
}
