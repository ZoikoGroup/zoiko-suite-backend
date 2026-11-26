// COM-05 Platform Commercial Billing, part 5a (ZS-SVC-Q-001 §4.5). Owns the
// BillingAccount, InvoiceCandidate and PlatformCommercialInvoice/InvoiceLine
// entities and the rating step that turns a subscription's effective
// configuration plus certified usage into billable lines.
//
// COM-05 must not own (§4.5): tenant AR/GL/tax/bank records, a subscription
// feature decision (COM-03's job), tax rule logic, or bank/provider
// settlement truth. Rating here reads COM-02's effective SubscriptionVersion
// and COM-04's certified UsageStatement as given facts; it never recomputes
// or overrides either.
package domain

import (
	"fmt"
	"math/big"
	"time"

	"zoiko.io/commercial-account-svc/internal/money"
)

const (
	PrefixBillingAccount   = "cba_"
	PrefixInvoiceCandidate = "cic_"
	PrefixInvoice          = "cinv_"
)

type BillingAccountStatus string

const (
	BillingAccountActive BillingAccountStatus = "ACTIVE"
	BillingAccountClosed BillingAccountStatus = "CLOSED"
)

// BillingAccount is COM-05's own server-resolved billing context for one
// organization — which ZoikoSuite selling entity, numbering series and
// payment provider it is invoiced under. Distinct from the pre-existing
// CommercialAccount (types.go): that is the verified customer identity;
// this is the platform's own billing configuration for it.
type BillingAccount struct {
	BillingAccountID        string `json:"billing_account_id"`
	OrganizationID          string `json:"organization_id"`
	SellingEntity           string `json:"selling_entity"`
	BillingCurrencyCode     string `json:"billing_currency_code"`
	InvoiceNumberingProfile string `json:"invoice_numbering_profile"`
	PaymentProviderRef      string `json:"payment_provider_ref"`
	// AccountingMappingKey is this seller's own GL/accounting classification
	// for every money-moving event on this billing account (COM-CTRL-033).
	// Caller-supplied at OpenBillingAccount time (a privileged seller-operator
	// action); never accepted on any later command, so a tenant-supplied
	// accounting-book reference has no field to be injected through
	// (negative path #44 is refused by construction).
	AccountingMappingKey string               `json:"accounting_mapping_key"`
	Status               BillingAccountStatus `json:"status"`
	CreatedAt            time.Time            `json:"created_at"`
	CreatedByPrincipalID string               `json:"created_by_principal_id"`
}

func ValidateBillingAccount(b *BillingAccount) error {
	if strEmpty(b.SellingEntity) {
		return invalid("selling_entity", "is required")
	}
	if !currencyCodePattern.MatchString(b.BillingCurrencyCode) {
		return invalid("billing_currency_code", "must be a 3-letter ISO 4217 code")
	}
	if strEmpty(b.InvoiceNumberingProfile) {
		return invalid("invoice_numbering_profile", "is required")
	}
	if strEmpty(b.PaymentProviderRef) {
		return invalid("payment_provider_ref", "is required")
	}
	if strEmpty(b.AccountingMappingKey) {
		return invalid("accounting_mapping_key", "is required")
	}
	return nil
}

// ── Invoice lines ────────────────────────────────────────────────────────────

type InvoiceLineKind string

const (
	LineRecurring InvoiceLineKind = "RECURRING"
	LineUsage     InvoiceLineKind = "USAGE"
	// LineOneTime is one ONE_TIME component's flat charge. Rated at most
	// once ever per (price_version_id, component_key) on one subscription
	// (see billedOneTime on RateInvoiceBasis).
	LineOneTime InvoiceLineKind = "ONE_TIME"
	// LineDiscount is a negative-amount line reducing the invoice's
	// subtotal, rated only against an APPROVED DiscountApplication (see
	// approvedDiscounts on RateInvoiceBasis). DiscountApplicationID carries
	// which application authorized it (COM-CTRL-019 lineage).
	LineDiscount InvoiceLineKind = "DISCOUNT"
)

// InvoiceLine is one rated line, carrying the evidence it was rated from
// (COM-CTRL-019). StatementTotalQuantityAtGenerate is only set for a USAGE
// line and is the usage basis IssueInvoice re-checks for drift.
// DiscountApplicationID is only set for a DISCOUNT line.
type InvoiceLine struct {
	LineNo                           int             `json:"line_no"`
	Kind                             InvoiceLineKind `json:"kind"`
	Description                      string          `json:"description"`
	PriceVersionID                   string          `json:"price_version_id"`
	ComponentKey                     string          `json:"component_key"`
	MeterKey                         *string         `json:"meter_key,omitempty"`
	StatementID                      *string         `json:"statement_id,omitempty"`
	StatementTotalQuantityAtGenerate *string         `json:"statement_total_quantity_at_generate,omitempty"`
	Quantity                         *string         `json:"quantity,omitempty"`
	UnitAmount                       *string         `json:"unit_amount,omitempty"`
	Amount                           string          `json:"amount"`
	DiscountApplicationID            *string         `json:"discount_application_id,omitempty"`
}

// ── Invoice candidate ────────────────────────────────────────────────────────

type InvoiceCandidateStatus string

const (
	CandidateDraft    InvoiceCandidateStatus = "DRAFT"
	CandidateApproved InvoiceCandidateStatus = "APPROVED"
	CandidateIssued   InvoiceCandidateStatus = "ISSUED"
)

type InvoiceCandidate struct {
	CandidateID           string                 `json:"candidate_id"`
	OrganizationID        string                 `json:"organization_id"`
	BillingAccountID      string                 `json:"billing_account_id"`
	SubscriptionID        string                 `json:"subscription_id"`
	SubscriptionVersionID string                 `json:"subscription_version_id"`
	TermNo                int                    `json:"term_no"`
	CurrencyCode          string                 `json:"currency_code"`
	Status                InvoiceCandidateStatus `json:"status"`
	Lines                 []InvoiceLine          `json:"lines"`
	SubtotalAmount        string                 `json:"subtotal_amount"`
	TaxJurisdictionCode   string                 `json:"tax_jurisdiction_code"`
	TaxRateBasisPoints    int                    `json:"tax_rate_basis_points"`
	TaxAmount             string                 `json:"tax_amount"`
	TotalAmount           string                 `json:"total_amount"`
	CreatedAt             time.Time              `json:"created_at"`
	CreatedByPrincipalID  string                 `json:"created_by_principal_id"`
	ApprovedAt            *time.Time             `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string                `json:"approved_by_principal_id,omitempty"`
	IssuedInvoiceID       *string                `json:"issued_invoice_id,omitempty"`
}

// GenerateInvoiceCandidateRequest is the caller's ask: rate this subscription
// term, under this tax evidence. Tax is asserted by the caller (COM-CTRL-020:
// this service owns no tax rule logic) and stored as evidence, not derived.
type GenerateInvoiceCandidateRequest struct {
	// CandidateID is minted by the caller before the idempotency claim is
	// built (same idiom as CreateProduct/StartSubscription elsewhere in this
	// service): the claim's resource id and the row this command creates
	// must be the same id, so an idempotent replay can be resolved back to
	// the original candidate.
	CandidateID          string
	OrganizationID       string
	SubscriptionID       string
	TermNo               int
	TaxJurisdictionCode  string
	TaxRateBasisPoints   int
	TaxAmount            string
	CreatedByPrincipalID string
}

func (r *GenerateInvoiceCandidateRequest) Validate() error {
	if r.TermNo < 1 {
		return invalid("term_no", "must be 1 or greater")
	}
	if strEmpty(r.TaxJurisdictionCode) {
		return invalid("tax_jurisdiction_code", "is required")
	}
	if r.TaxRateBasisPoints < 0 {
		return invalid("tax_rate_basis_points", "must not be negative")
	}
	if _, err := money.Parse(r.TaxAmount); err != nil {
		return invalid("tax_amount", "must be a non-negative decimal")
	}
	return nil
}

// PlatformCommercialInvoice is the immutable, issued invoice — a frozen copy
// of its candidate's totals and lines, addressed by its own sealed
// invoice_number (COM-CTRL-021, -022).
type PlatformCommercialInvoice struct {
	InvoiceID           string        `json:"invoice_id"`
	InvoiceNumber       string        `json:"invoice_number"`
	OrganizationID      string        `json:"organization_id"`
	BillingAccountID    string        `json:"billing_account_id"`
	CandidateID         string        `json:"candidate_id"`
	SubscriptionID      string        `json:"subscription_id"`
	TermNo              int           `json:"term_no"`
	CurrencyCode        string        `json:"currency_code"`
	Lines               []InvoiceLine `json:"lines"`
	SubtotalAmount      string        `json:"subtotal_amount"`
	TaxJurisdictionCode string        `json:"tax_jurisdiction_code"`
	TaxRateBasisPoints  int           `json:"tax_rate_basis_points"`
	TaxAmount           string        `json:"tax_amount"`
	TotalAmount         string        `json:"total_amount"`
	IssuedAt            time.Time     `json:"issued_at"`
	IssuedByPrincipalID string        `json:"issued_by_principal_id"`
}

// ── Rating ───────────────────────────────────────────────────────────────────

// UsageBasis is one meter's certified/adjusted total for the term being
// billed — the frozen fact RateInvoiceBasis rates a METERED component
// against.
type UsageBasis struct {
	StatementID   string
	TotalQuantity string
}

// ApprovedDiscount is the frozen fact RateInvoiceBasis needs to rate one
// DISCOUNT component: that a DiscountApplication for it reached APPROVED
// (either by a second approver, or by catalogue policy for a component that
// does not require approval — see ProposeDiscount/DecideDiscount, COM-02
// part 2c), and the first term number it is eligible from (the term in which
// it was approved, never recomputed on a later rating so a subscription with
// no new approval never drifts). A DISCOUNT component with no entry here is
// not applied: COM-CTRL-005 forbids applying an unapproved discount, and
// "no application" is indistinguishable from "proposed but not yet decided"
// — both must be refused, never silently skipped as if approved.
type ApprovedDiscount struct {
	DiscountApplicationID string
	FirstEligibleTermNo   int
}

// LineComponentKey is the identity RateInvoiceBasis's billedOneTime and
// approvedDiscounts maps are keyed by: one price component of one immutable
// price version. Never just ComponentKey alone — a plan change that moves a
// subscription to a new PriceVersionID binds a component the author of that
// new version chose to define (or not), and COM-CTRL-002/-003's immutable
// published-price-version doctrine treats it as a distinct fact from the
// identically-named component on the old version.
func LineComponentKey(priceVersionID, componentKey string) string {
	return priceVersionID + "/" + componentKey
}

// RateInvoiceBasis computes one line per chargeable component of every
// item's bound price version:
//
//   - RECURRING_FIXED/PER_UNIT/METERED as before.
//   - ONE_TIME: billed exactly once ever for a given component on this
//     subscription. billedOneTime (keyed by LineComponentKey) is this
//     subscription's own already-issued-invoice history; a component already
//     present there is skipped, never re-billed and never an error — a fee
//     that already fired is not a basis failure, it is simply done.
//   - DISCOUNT: rated as a single negative-amount line against the sum of
//     every other rated line on this same invoice (§5 doctrine: discount
//     policy versions are a rating input alongside price and quantity), only
//     when approvedDiscounts (keyed by LineComponentKey) carries an approved
//     application for it and the term being rated (termNo) still falls
//     inside that application's duration_intervals counted from the term it
//     was first approved in. Multiple independent discounts on one invoice
//     each apply against that same pre-discount base, not against each
//     other's result — order among them is therefore immaterial.
//
// A METERED component with no entry in usage is refused
// (ErrUsageBasisNotCertified): this service never bills from an uncertified
// or absent usage window.
func RateInvoiceBasis(items []SubscriptionItem, pvs map[string]*PriceVersion, usage map[string]UsageBasis,
	billedOneTime map[string]bool, approvedDiscounts map[string]ApprovedDiscount, termNo int) ([]InvoiceLine, error) {
	var lines []InvoiceLine
	lineNo := 1
	base := new(big.Rat)
	type discountCandidate struct {
		pv *PriceVersion
		c  PriceComponent
	}
	var discounts []discountCandidate

	for _, it := range items {
		pv, ok := pvs[it.PriceVersionID]
		if !ok {
			return nil, fmt.Errorf("price version %s not loaded", it.PriceVersionID)
		}
		q := map[string]string{}
		for _, x := range it.Quantities {
			q[x.ComponentKey] = x.Quantity
		}
		for _, c := range pv.Components {
			switch c.ComponentType {
			case ComponentRecurringFixed:
				amt := dec(*c.Amount)
				base.Add(base, amt)
				lines = append(lines, InvoiceLine{
					LineNo: lineNo, Kind: LineRecurring, Description: pv.DisplayName + " / " + c.ComponentKey,
					PriceVersionID: pv.PriceVersionID, ComponentKey: c.ComponentKey,
					Amount: EvidenceAmount(amt),
				})
				lineNo++
			case ComponentPerUnit:
				raw, ok := q[c.ComponentKey]
				if !ok {
					return nil, fmt.Errorf("no quantity for per-unit component %s of %s", c.ComponentKey, pv.PriceVersionID)
				}
				billable := billableQuantity(dec(raw), c.IncludedQuantity)
				amt, unit := rateComponent(billable, c)
				base.Add(base, amt)
				qCopy := raw
				lines = append(lines, InvoiceLine{
					LineNo: lineNo, Kind: LineRecurring, Description: pv.DisplayName + " / " + c.ComponentKey,
					PriceVersionID: pv.PriceVersionID, ComponentKey: c.ComponentKey,
					Quantity: &qCopy, UnitAmount: unit, Amount: EvidenceAmount(amt),
				})
				lineNo++
			case ComponentMetered:
				if c.MeterKey == nil {
					continue
				}
				ub, ok := usage[*c.MeterKey]
				if !ok {
					return nil, fmt.Errorf("%w: meter %s", ErrUsageBasisNotCertified, *c.MeterKey)
				}
				billable := billableQuantity(dec(ub.TotalQuantity), c.IncludedQuantity)
				amt, unit := rateComponent(billable, c)
				base.Add(base, amt)
				qCopy, sidCopy := ub.TotalQuantity, ub.StatementID
				lines = append(lines, InvoiceLine{
					LineNo: lineNo, Kind: LineUsage, Description: pv.DisplayName + " / " + c.ComponentKey,
					PriceVersionID: pv.PriceVersionID, ComponentKey: c.ComponentKey, MeterKey: c.MeterKey,
					StatementID: &sidCopy, StatementTotalQuantityAtGenerate: &qCopy,
					Quantity: &qCopy, UnitAmount: unit, Amount: EvidenceAmount(amt),
				})
				lineNo++
			case ComponentOneTime:
				if billedOneTime[LineComponentKey(pv.PriceVersionID, c.ComponentKey)] {
					continue
				}
				amt := dec(*c.Amount)
				base.Add(base, amt)
				lines = append(lines, InvoiceLine{
					LineNo: lineNo, Kind: LineOneTime, Description: pv.DisplayName + " / " + c.ComponentKey,
					PriceVersionID: pv.PriceVersionID, ComponentKey: c.ComponentKey,
					Amount: EvidenceAmount(amt),
				})
				lineNo++
			case ComponentDiscount:
				// Deferred to the second pass below: a discount is rated
				// against the full base of every other line on this
				// invoice, which is not known until every item has been
				// walked.
				discounts = append(discounts, discountCandidate{pv: pv, c: c})
			}
		}
	}

	for _, d := range discounts {
		pv, c := d.pv, d.c
		ad, ok := approvedDiscounts[LineComponentKey(pv.PriceVersionID, c.ComponentKey)]
		if !ok {
			continue
		}
		if termNo < ad.FirstEligibleTermNo {
			continue
		}
		if termNo > ad.FirstEligibleTermNo+*c.DurationIntervals-1 {
			continue
		}
		var amt *big.Rat
		if *c.DiscountType == "PERCENT" {
			amt = new(big.Rat).Mul(base, new(big.Rat).Quo(dec(*c.DiscountValue), big.NewRat(100, 1)))
		} else {
			amt = dec(*c.DiscountValue)
		}
		if c.DiscountCapAmount != nil {
			if cap := dec(*c.DiscountCapAmount); amt.Cmp(cap) > 0 {
				amt = cap
			}
		}
		// A discount never makes the invoice's subtotal negative: it
		// reduces what is owed, it does not itself become a credit.
		if amt.Cmp(base) > 0 {
			amt = base
		}
		if amt.Sign() <= 0 {
			continue
		}
		applicationID := ad.DiscountApplicationID
		lines = append(lines, InvoiceLine{
			LineNo: lineNo, Kind: LineDiscount, Description: pv.DisplayName + " / " + c.ComponentKey + " (discount)",
			PriceVersionID: pv.PriceVersionID, ComponentKey: c.ComponentKey,
			Amount: EvidenceAmount(new(big.Rat).Neg(amt)), DiscountApplicationID: &applicationID,
		})
		lineNo++
	}

	return lines, nil
}

func billableQuantity(raw *big.Rat, included *string) *big.Rat {
	if included == nil {
		return raw
	}
	billable := new(big.Rat).Sub(raw, dec(*included))
	if billable.Sign() < 0 {
		return new(big.Rat)
	}
	return billable
}

func rateComponent(billable *big.Rat, c PriceComponent) (*big.Rat, *string) {
	if c.Amount != nil {
		return new(big.Rat).Mul(billable, dec(*c.Amount)), c.Amount
	}
	return tierCharge(billable, c.Tiers, *c.TierMode), nil
}

// SumLines totals a set of lines exactly and rounds half-even to the
// currency's minor units — the one place a candidate's subtotal is computed,
// so generate and any later re-verification never disagree. A line's Amount
// is EvidenceAmount's unrounded (up to 8 fractional digits) rating result,
// not yet a currency-scaled money value, so it is parsed as a plain rational
// rather than through money.Parse (which caps at 4 fractional digits and
// would reject it).
func SumLines(lines []InvoiceLine, minorUnits int) string {
	total := new(big.Rat)
	for _, l := range lines {
		r, ok := new(big.Rat).SetString(l.Amount)
		if !ok {
			panic("invoice line amount " + l.Amount + " is not a decimal")
		}
		total.Add(total, r)
	}
	return money.FormatHalfEven(total, minorUnits)
}

var (
	ErrBillingAccountNotFound       = errorString("billing account not found")
	ErrBillingAccountExists         = errorString("this organization already has a billing account")
	ErrBillingAccountNotActive      = errorString("billing account is not active")
	ErrUsageBasisNotCertified       = errorString("a metered component has no certified or adjusted usage statement for this term")
	ErrInvoiceCandidateNotFound     = errorString("invoice candidate not found")
	ErrInvoiceCandidateInvalidState = errorString("invoice candidate is not in a state that allows this action")
	ErrInvoiceCandidateSelfApproval = errorString("approver must be independent of the candidate's generator")
	ErrInvoiceAlreadyIssuedForTerm  = errorString("this subscription term already has an issued invoice")
	ErrInvoiceBasisChanged          = errorString("the usage basis changed since this candidate was generated; regenerate the candidate")
	ErrInvoiceNotFound              = errorString("invoice not found")
	ErrEmptyInvoiceCandidate        = errorString("this subscription term has no billable lines")
	ErrTaxJurisdictionNotRegistered = errorString("tax jurisdiction is not registered for this billing account")
	ErrTaxRateMismatch              = errorString("supplied tax rate does not match the registered rate for this jurisdiction")
)
