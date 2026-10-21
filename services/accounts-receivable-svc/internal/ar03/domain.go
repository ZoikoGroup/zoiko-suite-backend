// Package ar03 implements AR-03 Sales Order (spec ZS-SVC-C-001 section 6): the governed
// customer commitment, its billing basis and its invoiced / remaining position. An order is
// a COMMERCIAL SOURCE only: it never queues a posting and never creates a receivable. It owns
// the order, its lines, billing milestones, hold/cancel state, governed amendment versions and
// the invoiced/remaining quantities and amounts; fulfillment evidence is owned by the
// operational domain and only its reference is recorded here.
package ar03

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture). Spec
// permissions salesorder.create/manage map to ORDER_MANAGE; salesorder.approve/confirm/
// hold/cancel to the like-named actions. ORDER_CREDIT_EXCEPTION_APPROVE is the extra
// authority to approve a credit exception and to release a credit hold.
const (
	ActionOrderManage            = "AR03_ORDER_MANAGE"
	ActionOrderApprove           = "AR03_ORDER_APPROVE"
	ActionOrderConfirm           = "AR03_ORDER_CONFIRM"
	ActionOrderHold              = "AR03_ORDER_HOLD"
	ActionOrderCancel            = "AR03_ORDER_CANCEL"
	ActionCreditExceptionApprove = "AR03_ORDER_CREDIT_EXCEPTION_APPROVE"
)

// CommercialApprovalThreshold: an order whose net total is strictly above it must be
// approved by a principal who neither created nor submitted it. It is compared with the
// order's own net in the order currency (no FX conversion is performed anywhere in AR).
var CommercialApprovalThreshold = kit.MustAmount("100000")

// CreditApprovalThresholdPercent: an order whose net exceeds this percentage of the
// customer's credit limit (same currency) needs the same independent approval.
var CreditApprovalThresholdPercent = kit.MustAmount("50")

// Codes local to AR-03 beyond the shared spec section 14 classes.
const (
	// CodeDuplicateMilestone: the external milestone reference was already recorded for the order.
	CodeDuplicateMilestone = "DUPLICATE_BILLABLE_MILESTONE"
	// CodeAmendmentNotAllowed: a post-confirmation amendment that is not a pure reduction.
	CodeAmendmentNotAllowed = "ORDER_AMENDMENT_NOT_ALLOWED"

	AggregateType = "SalesOrder"
	maxListLimit  = 200
	maxLines      = 500
	maxText       = 4000
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventSalesOrderCreated         = "ar.sales_order.created"
	EventSalesOrderConfirmed       = "ar.sales_order.confirmed"
	EventSalesOrderHeld            = "ar.sales_order.held"
	EventSalesOrderReleased        = "ar.sales_order.released"
	EventBillableMilestoneRecorded = "ar.sales_order.billable_milestone_recorded"
	EventSalesOrderPartiallyBilled = "ar.sales_order.partially_billed"
	EventSalesOrderFullyBilled     = "ar.sales_order.fully_billed"
	EventSalesOrderCancelled       = "ar.sales_order.cancelled"
)

// State is the order lifecycle: Draft -> PendingApproval -> Approved -> Confirmed ->
// InFulfillment / PartiallyBilled -> FullyBilled -> Closed; Cancelled is a branch. A hold is
// an overlay (HoldActive) on a live state so the underlying state is never lost.
type State string

const (
	StateDraft           State = "DRAFT"
	StatePendingApproval State = "PENDING_APPROVAL"
	StateApproved        State = "APPROVED"
	StateConfirmed       State = "CONFIRMED"
	StateInFulfillment   State = "IN_FULFILLMENT"
	StatePartiallyBilled State = "PARTIALLY_BILLED"
	StateFullyBilled     State = "FULLY_BILLED"
	StateClosed          State = "CLOSED"
	StateCancelled       State = "CANCELLED"
)

// Hold kinds. A CREDIT hold is released only by a different principal holding the credit authority.
const (
	HoldCredit      = "CREDIT"
	HoldCommercial  = "COMMERCIAL"
	HoldFulfillment = "FULFILLMENT"
	HoldOther       = "OTHER"
)

var holdKinds = map[string]bool{HoldCredit: true, HoldCommercial: true, HoldFulfillment: true, HoldOther: true}

// Transition names a lifecycle move.
type Transition string

const (
	TrSubmit    Transition = "SUBMIT"
	TrApprove   Transition = "APPROVE"
	TrConfirm   Transition = "CONFIRM"
	TrFulfil    Transition = "FULFIL"
	TrPartBill  Transition = "PART_BILL"
	TrFullBill  Transition = "FULL_BILL"
	TrCancel    Transition = "CANCEL"
	TrClose     Transition = "CLOSE"
	TrPlaceHold Transition = "HOLD" // overlay: the state does not change
)

var transitions = map[Transition]map[State]State{
	TrSubmit:   {StateDraft: StatePendingApproval},
	TrApprove:  {StatePendingApproval: StateApproved},
	TrConfirm:  {StateApproved: StateConfirmed},
	TrFulfil:   {StateConfirmed: StateInFulfillment},
	TrPartBill: {StateConfirmed: StatePartiallyBilled, StateInFulfillment: StatePartiallyBilled},
	TrFullBill: {StateConfirmed: StateFullyBilled, StateInFulfillment: StateFullyBilled, StatePartiallyBilled: StateFullyBilled},
	TrCancel: {StateDraft: StateCancelled, StatePendingApproval: StateCancelled, StateApproved: StateCancelled, StateConfirmed: StateCancelled,
		StateInFulfillment: StateCancelled, StatePartiallyBilled: StateCancelled},
	// FullyBilled closes normally; a live partly-billed order short-closes (reason required).
	TrClose: {StateFullyBilled: StateClosed, StateConfirmed: StateClosed, StateInFulfillment: StateClosed, StatePartiallyBilled: StateClosed},
	// Overlay: listed so CanHold reuses the table; the target equals the source.
	TrPlaceHold: {StatePendingApproval: StatePendingApproval, StateApproved: StateApproved, StateConfirmed: StateConfirmed,
		StateInFulfillment: StateInFulfillment, StatePartiallyBilled: StatePartiallyBilled},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION.
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// CanAmendDraft: content may change freely and returns the order to DRAFT.
func (s State) CanAmendDraft() bool {
	return s == StateDraft || s == StatePendingApproval || s == StateApproved
}

// CanAmendLive: a governed post-confirmation amendment (reduction only, reason + evidence).
func (s State) CanAmendLive() bool {
	return s == StateConfirmed || s == StateInFulfillment || s == StatePartiallyBilled
}

// Billable reports whether invoices may be raised against an order in the state.
func (s State) Billable() bool {
	return s == StateConfirmed || s == StateInFulfillment || s == StatePartiallyBilled
}

// ── money helpers ────────────────────────────────────────────────────────────

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9_.:/#\-]{1,255}$`)
	codeRe     = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

	maxLineNet = kit.MustAmount("1000000000000")
	saturated  = kit.Amount(1 << 62)
)

func roundStep(currency string) *big.Int {
	step := int64(1)
	for i := 0; i < 4-kit.MinorUnitDigits(currency); i++ {
		step *= 10
	}
	return big.NewInt(step)
}

// mulRound computes a*b/divisor exactly in big integers and rounds half-up once to the
// currency's minor unit. a, b >= 0.
func mulRound(a, b kit.Amount, divisor int64, currency string) kit.Amount {
	p := new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(b)))
	step := roundStep(currency)
	den := new(big.Int).Mul(big.NewInt(divisor), step)
	half := new(big.Int).Div(den, big.NewInt(2))
	r := new(big.Int).Mul(new(big.Int).Div(new(big.Int).Add(p, half), den), step)
	if !r.IsInt64() || r.Int64() > int64(saturated) {
		return saturated
	}
	return kit.Amount(r.Int64())
}

// LineNet is quantity x unit price, exact, rounded half-up to the currency.
func LineNet(qty, price kit.Amount, currency string) kit.Amount {
	return mulRound(qty, price, 10000, currency)
}

// allocate splits total across weights in whole currency steps, proportionally, largest
// remainder first (ties to the lower index) so the parts always add up to total exactly.
func allocate(total kit.Amount, weights []kit.Amount, currency string) []kit.Amount {
	out := make([]kit.Amount, len(weights))
	sumW := new(big.Int)
	for _, w := range weights {
		sumW.Add(sumW, big.NewInt(int64(w)))
	}
	if sumW.Sign() == 0 || total == 0 {
		return out
	}
	step := roundStep(currency)
	units := new(big.Int).Div(big.NewInt(int64(total)), step)
	q := make([]*big.Int, len(weights))
	rem := make([]*big.Int, len(weights))
	given := new(big.Int)
	for i, w := range weights {
		num := new(big.Int).Mul(units, big.NewInt(int64(w)))
		q[i], rem[i] = new(big.Int).QuoRem(num, sumW, new(big.Int))
		given.Add(given, q[i])
	}
	left := new(big.Int).Sub(units, given).Int64()
	idx := make([]int, len(weights))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return rem[idx[a]].Cmp(rem[idx[b]]) > 0 })
	for k := int64(0); k < left && int(k) < len(idx); k++ {
		q[idx[k]].Add(q[idx[k]], big.NewInt(1))
	}
	for i := range q {
		out[i] = kit.Amount(new(big.Int).Mul(q[i], step).Int64())
	}
	return out
}

// ── content ──────────────────────────────────────────────────────────────────

// LineInput is a requested line. Nets are always computed by the server. LineNo is only
// meaningful for a post-confirmation amendment, where lines are matched by number.
type LineInput struct {
	LineNo      *int       `json:"line_no,omitempty"`
	Description string     `json:"description"`
	ProductRef  string     `json:"product_ref,omitempty"`
	Quantity    kit.Amount `json:"quantity"`
	UnitPrice   kit.Amount `json:"unit_price"`
}

// ContentLine is a line of a content version: ListNet = quantity x unit price (rounded
// half-up to the currency), LineNet = ListNet - DiscountAmount.
type ContentLine struct {
	LineNo         int        `json:"line_no"`
	Description    string     `json:"description"`
	ProductRef     string     `json:"product_ref,omitempty"`
	Quantity       kit.Amount `json:"quantity"`
	UnitPrice      kit.Amount `json:"unit_price"`
	ListNet        kit.Amount `json:"list_net"`
	DiscountAmount kit.Amount `json:"discount_amount"`
	LineNet        kit.Amount `json:"line_net"`
}

// Line is a current order line with its invoiced / remaining position.
type Line struct {
	ContentLine
	InvoicedQty     kit.Amount `json:"invoiced_qty"`
	InvoicedAmount  kit.Amount `json:"invoiced_amount"`
	RemainingQty    kit.Amount `json:"remaining_qty"`
	RemainingAmount kit.Amount `json:"remaining_amount"`
}

// Content is everything a version records about the commitment.
type Content struct {
	Currency          string        `json:"currency"`
	CustomerPORef     string        `json:"customer_po_ref,omitempty"`
	Terms             string        `json:"terms,omitempty"`
	PriceSourceRef    string        `json:"price_source_ref,omitempty"`
	CatalogVersionRef string        `json:"catalog_version_ref,omitempty"`
	DiscountPercent   kit.Amount    `json:"discount_percent"`
	Lines             []ContentLine `json:"lines"`
}

// Totals are derived from the lines; never caller-supplied.
type Totals struct {
	Subtotal       kit.Amount `json:"subtotal"`
	DiscountAmount kit.Amount `json:"discount_amount"`
	NetTotal       kit.Amount `json:"net_total"`
}

// BuildLines validates requested lines and computes their list nets (no discount yet).
func BuildLines(currency string, in []LineInput) ([]ContentLine, error) {
	if len(in) == 0 {
		return nil, kit.Invalid("an order needs at least one line")
	}
	if len(in) > maxLines {
		return nil, kit.Invalid(fmt.Sprintf("an order has at most %d lines", maxLines))
	}
	out := make([]ContentLine, 0, len(in))
	for i, l := range in {
		n := i + 1
		desc := strings.TrimSpace(l.Description)
		switch {
		case desc == "" || utf8.RuneCountInString(desc) > 500:
			return nil, kit.Invalid(fmt.Sprintf("line %d: description is required (max 500 characters)", n))
		case l.ProductRef != "" && !refRe.MatchString(l.ProductRef):
			return nil, kit.Invalid(fmt.Sprintf("line %d: product_ref has invalid characters", n))
		case !l.Quantity.IsPositive():
			return nil, kit.Invalid(fmt.Sprintf("line %d: quantity must be greater than zero", n))
		case l.UnitPrice.IsNegative():
			return nil, kit.Invalid(fmt.Sprintf("line %d: unit_price must not be negative", n))
		case l.Quantity > kit.MustAmount("1000000000") || l.UnitPrice > kit.MustAmount("1000000000"):
			return nil, kit.Invalid(fmt.Sprintf("line %d: quantity or unit_price out of range", n))
		}
		net := LineNet(l.Quantity, l.UnitPrice, currency)
		if net > maxLineNet {
			return nil, kit.Invalid(fmt.Sprintf("line %d: net amount out of range", n))
		}
		out = append(out, ContentLine{LineNo: n, Description: desc, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice,
			ListNet: net, LineNet: net})
	}
	return out, nil
}

// ApplyDiscountAmount allocates a total discount across the lines (exactly, in currency
// steps) and returns the totals. Every figure must fit the currency.
func ApplyDiscountAmount(currency string, lines []ContentLine, discount kit.Amount) (Totals, error) {
	var sub kit.Amount
	weights := make([]kit.Amount, len(lines))
	for i, l := range lines {
		if l.ListNet.IsNegative() || !l.ListNet.FitsCurrency(currency) {
			return Totals{}, kit.Invalid(fmt.Sprintf("line %d: net does not fit the currency", l.LineNo))
		}
		sub = sub.Add(l.ListNet)
		weights[i] = l.ListNet
	}
	if discount.IsNegative() || discount > sub || !discount.FitsCurrency(currency) {
		return Totals{}, kit.Invalid("the discount is not a valid amount for these lines and currency")
	}
	parts := allocate(discount, weights, currency)
	for i := range lines {
		lines[i].DiscountAmount = parts[i]
		lines[i].LineNet = lines[i].ListNet.Sub(parts[i])
	}
	return Totals{Subtotal: sub, DiscountAmount: discount, NetTotal: sub.Sub(discount)}, nil
}

// ApplyDiscountPercent derives the discount amount from a percentage (half-up, once, on
// the subtotal) and allocates it.
func ApplyDiscountPercent(currency string, lines []ContentLine, pct kit.Amount) (Totals, error) {
	if pct.IsNegative() || pct > kit.MustAmount("100") {
		return Totals{}, kit.Invalid("discount_percent must be between 0 and 100")
	}
	var sub kit.Amount
	for _, l := range lines {
		sub = sub.Add(l.ListNet)
	}
	return ApplyDiscountAmount(currency, lines, mulRound(sub, pct, 100*10000, currency))
}

// Validate checks everything that does not need a database.
func (c Content) Validate() error {
	if !currencyRe.MatchString(c.Currency) {
		return kit.Invalid("currency must be a 3-letter upper-case ISO 4217 code")
	}
	if utf8.RuneCountInString(c.Terms) > maxText {
		return kit.Invalid(fmt.Sprintf("terms are limited to %d characters", maxText))
	}
	for name, v := range map[string]string{"price_source_ref": c.PriceSourceRef, "catalog_version_ref": c.CatalogVersionRef, "customer_po_ref": c.CustomerPORef} {
		if v != "" && !refRe.MatchString(v) {
			return kit.Invalid(name + " must be 1-255 characters of letters, digits and _ . : / # -")
		}
	}
	if len(c.Lines) == 0 {
		return kit.Invalid("an order needs at least one line")
	}
	for _, l := range c.Lines {
		if !l.LineNet.FitsCurrency(c.Currency) || !l.ListNet.FitsCurrency(c.Currency) || !l.DiscountAmount.FitsCurrency(c.Currency) {
			return kit.Invalid(fmt.Sprintf("line %d: amounts have more precision than the currency allows", l.LineNo))
		}
	}
	return nil
}

// TotalsOf recomputes the totals from stored lines.
func TotalsOf(lines []ContentLine) Totals {
	var t Totals
	for _, l := range lines {
		t.Subtotal = t.Subtotal.Add(l.ListNet)
		t.DiscountAmount = t.DiscountAmount.Add(l.DiscountAmount)
		t.NetTotal = t.NetTotal.Add(l.LineNet)
	}
	return t
}

// ContentHash is the approval-binding hash of the commercial content.
func ContentHash(c Content) string {
	type hl struct {
		D string `json:"d"`
		P string `json:"p"`
		Q string `json:"q"`
		U string `json:"u"`
		L string `json:"l"`
		X string `json:"x"`
		N string `json:"n"`
	}
	type doc struct {
		Currency string `json:"currency"`
		PO       string `json:"po"`
		Terms    string `json:"terms"`
		Price    string `json:"price"`
		Catalog  string `json:"catalog"`
		Discount string `json:"discount"`
		Lines    []hl   `json:"lines"`
	}
	d := doc{Currency: c.Currency, PO: c.CustomerPORef, Terms: c.Terms, Price: c.PriceSourceRef, Catalog: c.CatalogVersionRef, Discount: c.DiscountPercent.String()}
	for _, l := range c.Lines {
		d.Lines = append(d.Lines, hl{l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.ListNet.String(), l.DiscountAmount.String(), l.LineNet.String()})
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

// ── remaining-to-bill (deterministic, exact) ─────────────────────────────────

// BillingSummary is the order-level position derived from the lines.
type BillingSummary struct {
	OrderedNet   kit.Amount `json:"ordered_net"`
	InvoicedNet  kit.Amount `json:"invoiced_net"`
	RemainingNet kit.Amount `json:"remaining_net"`
	AnyBilled    bool       `json:"any_billed"`
	FullyBilled  bool       `json:"fully_billed"`
}

// Remaining returns what is left on a line (never negative).
func Remaining(ordered, invoiced kit.Amount) kit.Amount {
	if r := ordered.Sub(invoiced); r.IsPositive() {
		return r
	}
	return 0
}

// Summarize derives the position. FullyBilled means every line has nothing left to bill in
// both amount and quantity.
func Summarize(lines []Line) BillingSummary {
	s := BillingSummary{FullyBilled: len(lines) > 0}
	for _, l := range lines {
		s.OrderedNet = s.OrderedNet.Add(l.LineNet)
		s.InvoicedNet = s.InvoicedNet.Add(l.InvoicedAmount)
		s.RemainingNet = s.RemainingNet.Add(l.RemainingAmount)
		if l.InvoicedAmount.IsPositive() || l.InvoicedQty.IsPositive() {
			s.AnyBilled = true
		}
		if l.RemainingAmount.IsPositive() || l.RemainingQty.IsPositive() {
			s.FullyBilled = false
		}
	}
	return s
}

// StateAfterBilling is the order state once a billing movement has been applied.
func StateAfterBilling(cur State, s BillingSummary) State {
	switch {
	case s.FullyBilled:
		return StateFullyBilled
	case s.AnyBilled && cur != StatePartiallyBilled:
		return StatePartiallyBilled
	}
	return cur
}

// ── credit control ───────────────────────────────────────────────────────────

// CreditResult is the evidence of one credit evaluation.
type CreditResult struct {
	Limit             kit.Amount `json:"credit_limit"`
	CreditCurrency    string     `json:"credit_currency,omitempty"`
	Hold              bool       `json:"credit_hold"`
	OrderNet          kit.Amount `json:"order_net"`
	Exposure          kit.Amount `json:"exposure"`
	ExposureAvailable bool       `json:"exposure_available"`
	Projected         kit.Amount `json:"projected"`
	Exceeded          bool       `json:"exceeded"`
	Unverifiable      bool       `json:"unverifiable,omitempty"`
}

// Blocked reports whether confirmation needs an approved credit exception.
func (r CreditResult) Blocked() bool { return r.Hold || r.Exceeded || r.Unverifiable }

// Detail is the human-readable reason a confirmation is blocked.
func (r CreditResult) Detail(currency string) string {
	switch {
	case r.Hold:
		return "the customer is on credit hold: an approved credit exception is required to confirm the order"
	case r.Unverifiable:
		return fmt.Sprintf("the credit limit is in %s but the order is in %s: it cannot be verified without an approved credit exception", r.CreditCurrency, currency)
	case !r.ExposureAvailable:
		return fmt.Sprintf("the order value %s exceeds the credit limit %s (existing exposure is unavailable and was not assumed): an approved credit exception is required", r.OrderNet, r.Limit)
	default:
		return fmt.Sprintf("exposure %s plus this order %s exceeds the credit limit %s: an approved credit exception is required", r.Exposure, r.OrderNet, r.Limit)
	}
}

// EvaluateCredit compares exposure (when known) plus the order's own net with the credit
// limit. Exposure is never guessed: when unavailable only the order's own value is compared
// and ExposureAvailable is false in the evidence. A zero limit means none is configured.
func EvaluateCredit(p *kit.ProfileView, orderCurrency string, orderNet, exposure kit.Amount, exposureAvailable bool) CreditResult {
	r := CreditResult{Limit: p.CreditLimit, CreditCurrency: p.CreditCurrency, Hold: p.CreditHold, OrderNet: orderNet,
		Exposure: exposure, ExposureAvailable: exposureAvailable}
	if !exposureAvailable {
		r.Exposure = 0
	}
	r.Projected = r.Exposure.Add(orderNet)
	if !p.CreditLimit.IsPositive() {
		return r
	}
	if p.CreditCurrency != "" && p.CreditCurrency != orderCurrency {
		r.Unverifiable = true
		return r
	}
	r.Exceeded = r.Projected > p.CreditLimit
	return r
}

// SoDRequired reports whether the approval must come from a principal who neither created
// nor submitted the order: the net is above the commercial threshold, or above the credit
// threshold share of the credit limit, or a credit exception is attached.
func SoDRequired(net kit.Amount, orderCurrency string, p *kit.ProfileView, withException bool) bool {
	if withException || net > CommercialApprovalThreshold {
		return true
	}
	if p != nil && p.CreditLimit.IsPositive() && (p.CreditCurrency == "" || p.CreditCurrency == orderCurrency) {
		// net/limit > pct/100  <=>  net*100 > limit*pct (kept in big integers).
		l := new(big.Int).Mul(big.NewInt(int64(net)), big.NewInt(100*10000))
		r := new(big.Int).Mul(big.NewInt(int64(p.CreditLimit)), big.NewInt(int64(CreditApprovalThresholdPercent)))
		return l.Cmp(r) > 0
	}
	return false
}

// ── persisted shapes ─────────────────────────────────────────────────────────

// Header is the stable order.
type Header struct {
	OrderID           string     `json:"order_id"`
	TenantID          string     `json:"tenant_id"`
	LegalEntityID     string     `json:"legal_entity_id"`
	OrderNumber       string     `json:"order_number"`
	CustomerPartyID   string     `json:"customer_party_id"`
	Currency          string     `json:"currency"`
	State             State      `json:"state"`
	HoldActive        bool       `json:"hold_active"`
	HoldKind          string     `json:"hold_kind,omitempty"`
	HoldReason        string     `json:"hold_reason,omitempty"`
	HoldEvidenceID    string     `json:"hold_evidence_id,omitempty"`
	HeldBy            string     `json:"held_by,omitempty"`
	HeldAt            *time.Time `json:"held_at,omitempty"`
	CustomerPORef     string     `json:"customer_po_ref,omitempty"`
	SourceQuoteRef    string     `json:"source_quote_ref,omitempty"`
	SourceRef         string     `json:"source_ref,omitempty"`
	NetTotal          kit.Amount `json:"net_total"`
	DiscountPercent   kit.Amount `json:"discount_percent"`
	CurrentVersionNo  int        `json:"current_version_no"`
	Version           int        `json:"version"`
	CreatedBy         string     `json:"created_by"`
	SubmittedBy       string     `json:"submitted_by,omitempty"`
	ApprovedVersionNo int        `json:"approved_version_no,omitempty"`
	ApprovedHash      string     `json:"approved_content_hash,omitempty"`
	ApprovedBy        string     `json:"approved_by,omitempty"`
	ApprovedAt        *time.Time `json:"approved_at,omitempty"`
	CreditExceptionID string     `json:"credit_exception_id,omitempty"`
	ConfirmedBy       string     `json:"confirmed_by,omitempty"`
	ConfirmedAt       *time.Time `json:"confirmed_at,omitempty"`
	CancelReason      string     `json:"cancel_reason,omitempty"`
	CancelledBy       string     `json:"cancelled_by,omitempty"`
	CancelledAt       *time.Time `json:"cancelled_at,omitempty"`
	CloseReason       string     `json:"close_reason,omitempty"`
	ClosedBy          string     `json:"closed_by,omitempty"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Version is one immutable recorded version (governed amendment) of the order content.
type Version struct {
	VersionNo  int       `json:"version_no"`
	Hash       string    `json:"content_hash"`
	ChangeKind string    `json:"change_kind"`
	ReasonCode string    `json:"reason_code,omitempty"`
	EvidenceID string    `json:"evidence_id,omitempty"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	Content
	Totals
}

// Milestone states.
const (
	MsRecorded = "RECORDED"
	MsInvoiced = "INVOICED"
	MsVoided   = "VOIDED"
)

// Milestone is one billable milestone of an order line.
type Milestone struct {
	MilestoneID            string     `json:"milestone_id"`
	OrderID                string     `json:"order_id"`
	ExternalRef            string     `json:"external_ref"`
	LineNo                 int        `json:"line_no"`
	Description            string     `json:"description,omitempty"`
	ScheduledQty           kit.Amount `json:"scheduled_qty"`
	ScheduledAmount        kit.Amount `json:"scheduled_amount"`
	State                  string     `json:"state"`
	FulfillmentSource      string     `json:"fulfillment_source"`
	FulfillmentEvidenceRef string     `json:"fulfillment_evidence_ref"`
	RecordedBy             string     `json:"recorded_by"`
	RecordedAt             time.Time  `json:"recorded_at"`
	InvoiceID              string     `json:"invoice_id,omitempty"`
	InvoicedNet            kit.Amount `json:"invoiced_net,omitempty"`
	InvoicedAt             *time.Time `json:"invoiced_at,omitempty"`
	VoidReason             string     `json:"void_reason,omitempty"`
	VoidedAt               *time.Time `json:"voided_at,omitempty"`
}

// Order is the read model: the header, its current version, current lines and milestones.
type Order struct {
	Header
	Current    *Version       `json:"current_version"`
	Lines      []Line         `json:"lines"`
	Milestones []Milestone    `json:"milestones"`
	Billing    BillingSummary `json:"billing"`
}

// HistoryEntry is one row of the append-only order history.
type HistoryEntry struct {
	OrderVersion  int            `json:"order_version"`
	VersionNo     int            `json:"version_no"`
	Command       string         `json:"command"`
	StateAfter    State          `json:"state_after"`
	ActorID       string         `json:"actor_id"`
	ApproverID    string         `json:"approver_id,omitempty"`
	ReasonCode    string         `json:"reason_code,omitempty"`
	EvidenceID    string         `json:"evidence_id,omitempty"`
	ContentHash   string         `json:"content_hash,omitempty"`
	PriorValues   map[string]any `json:"prior_values,omitempty"`
	NewValues     map[string]any `json:"new_values,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	RecordedAt    time.Time      `json:"recorded_at"`
}

// ── errors and eligibility ───────────────────────────────────────────────────

func errNotBillable(msg string) *kit.Problem {
	return kit.Unprocessable(kit.CodeOrderNotBillable, msg)
}

// CheckBillable decides whether the order and milestone are billable right now, without
// looking at amounts. The first failing reason wins; cancelled, held, unconfirmed and
// finished orders and unknown, already-billed or voided milestones are all ORDER_NOT_BILLABLE.
func CheckBillable(h *Header, m *Milestone) *kit.Problem {
	switch {
	case h.State == StateCancelled:
		return errNotBillable("the order is cancelled: it cannot be invoiced")
	case h.State == StateClosed || h.State == StateFullyBilled:
		return errNotBillable("the order is already fully billed or closed")
	case h.HoldActive && h.HoldKind == HoldCredit:
		return errNotBillable("the order is on credit hold: invoicing is blocked")
	case h.HoldActive:
		return errNotBillable("the order is on hold: invoicing is blocked")
	case !h.State.Billable():
		return errNotBillable("the order is not confirmed (state " + string(h.State) + ")")
	case m == nil:
		return errNotBillable("the billable milestone is unknown for this order")
	case m.State == MsInvoiced:
		return errNotBillable("the milestone is already billed")
	case m.State == MsVoided:
		return errNotBillable("the milestone was voided")
	}
	return nil
}
