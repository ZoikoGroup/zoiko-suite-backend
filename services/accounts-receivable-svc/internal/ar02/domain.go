// Package ar02 implements AR-02 Quote / Estimate (spec ZS-SVC-C-001 section 5):
// non-posting customer offers kept strictly apart from authoritative financial
// documents. A quote owns its versions, lines, validity, assumptions, price
// source references, approval, acceptance evidence and conversion linkage. It
// never queues a posting and never creates a receivable.
package ar02

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture).
// Spec permissions quote.create/manage/send map to QUOTE_MANAGE; quote.approve to
// QUOTE_APPROVE; quote.convert to QUOTE_CONVERT. QUOTE_DISCOUNT_APPROVE is the
// extra authority needed to approve a discount above DiscountSelfAuthorityPercent.
const (
	ActionQuoteManage          = "AR02_QUOTE_MANAGE"
	ActionQuoteApprove         = "AR02_QUOTE_APPROVE"
	ActionQuoteDiscountApprove = "AR02_QUOTE_DISCOUNT_APPROVE"
	ActionQuoteConvert         = "AR02_QUOTE_CONVERT"
)

// DiscountSelfAuthorityPercent is the discount a quote may carry without the
// discount-approval authority. A discount strictly above it must be approved by a
// principal holding ActionQuoteDiscountApprove who is not the quote's preparer.
var DiscountSelfAuthorityPercent = kit.MustAmount("10")

// Codes local to AR-02 beyond the shared spec section 14 classes.
const (
	CodeApprovalInvalidated = "QUOTE_APPROVAL_INVALIDATED"
	CodeVersionSuperseded   = "QUOTE_VERSION_SUPERSEDED"

	AggregateType       = "Quote"
	dateLayout          = "2006-01-02"
	maxListLimit        = 200
	maxLines            = 500
	maxText             = 4000
	DefaultExpireReason = "VALIDITY_ELAPSED"
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventQuoteCreated   = "ar.quote.created"
	EventQuoteApproved  = "ar.quote.approved"
	EventQuoteSent      = "ar.quote.sent"
	EventQuoteAccepted  = "ar.quote.accepted"
	EventQuoteRejected  = "ar.quote.rejected"
	EventQuoteExpired   = "ar.quote.expired"
	EventQuoteConverted = "ar.quote.converted_to_order"
)

// State is the quote lifecycle: Draft -> PendingApproval -> Approved -> Sent ->
// Accepted/Rejected/Expired -> Converted.
type State string

const (
	StateDraft           State = "DRAFT"
	StatePendingApproval State = "PENDING_APPROVAL"
	StateApproved        State = "APPROVED"
	StateSent            State = "SENT"
	StateAccepted        State = "ACCEPTED"
	StateRejected        State = "REJECTED"
	StateExpired         State = "EXPIRED"
	StateConverted       State = "CONVERTED"
)

// Transition names a lifecycle move.
type Transition string

const (
	TrSubmit  Transition = "SUBMIT"
	TrApprove Transition = "APPROVE"
	TrSend    Transition = "SEND"
	TrAccept  Transition = "ACCEPT"
	TrReject  Transition = "REJECT"
	TrExpire  Transition = "EXPIRE"
	TrConvert Transition = "CONVERT"
)

var transitions = map[Transition]map[State]State{
	TrSubmit:  {StateDraft: StatePendingApproval},
	TrApprove: {StatePendingApproval: StateApproved},
	TrSend:    {StateApproved: StateSent},
	TrAccept:  {StateSent: StateAccepted},
	TrReject:  {StatePendingApproval: StateRejected, StateApproved: StateRejected, StateSent: StateRejected},
	// An accepted quote that was never converted lapses with its validity too.
	TrExpire:  {StateDraft: StateExpired, StatePendingApproval: StateExpired, StateApproved: StateExpired, StateSent: StateExpired, StateAccepted: StateExpired},
	TrConvert: {StateAccepted: StateConverted},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION.
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// CanAmend reports whether content may still change. A material change from any of
// these states returns the quote to DRAFT; accepted/rejected/expired/converted
// quotes are final and a new quote must be raised instead.
func (s State) CanAmend() bool {
	return s == StateDraft || s == StatePendingApproval || s == StateApproved || s == StateSent
}

// AfterAmend is the state following an amendment: a material change always
// returns the quote to DRAFT (the prior approval cannot be reused); a
// non-material one leaves the state alone.
func AfterAmend(cur State, material bool) State {
	if material {
		return StateDraft
	}
	return cur
}

// ── content ──────────────────────────────────────────────────────────────────

// LineInput is a requested line. Net is always computed by the server.
type LineInput struct {
	Description string     `json:"description"`
	ProductRef  string     `json:"product_ref,omitempty"`
	Quantity    kit.Amount `json:"quantity"`
	UnitPrice   kit.Amount `json:"unit_price"`
}

// Line is a stored line: net = quantity x unit_price rounded half-up to the currency's minor unit.
type Line struct {
	LineNo      int        `json:"line_no"`
	Description string     `json:"description"`
	ProductRef  string     `json:"product_ref,omitempty"`
	Quantity    kit.Amount `json:"quantity"`
	UnitPrice   kit.Amount `json:"unit_price"`
	LineNet     kit.Amount `json:"line_net"`
}

// TaxParams are the caller-supplied inputs tax-determination needs for an estimate.
type TaxParams struct {
	JurisdictionID         string `json:"jurisdiction_id"`
	SupplyJurisdictionID   string `json:"supply_jurisdiction_id,omitempty"`
	TaxCategory            string `json:"tax_category"`
	SupplyKind             string `json:"supply_kind"` // GOODS | SERVICES | DIGITAL_SERVICES
	SupplyType             string `json:"supply_type"` // B2B | B2C | B2G
	ProductClassification  string `json:"product_classification,omitempty"`
	BuyerTaxRegistrationID string `json:"buyer_tax_registration_id,omitempty"`
}

// Content is everything a version records about the offer.
type Content struct {
	Currency           string     `json:"currency"`
	ValidityDate       string     `json:"validity_date"`
	Terms              string     `json:"terms,omitempty"`
	Assumptions        string     `json:"assumptions,omitempty"`
	DiscountPercent    kit.Amount `json:"discount_percent"`
	PriceSourceRef     string     `json:"price_source_ref,omitempty"`
	CatalogVersionRef  string     `json:"catalog_version_ref,omitempty"`
	RequestTaxEstimate bool       `json:"request_tax_estimate"`
	TaxParams          *TaxParams `json:"tax_params,omitempty"`
	Lines              []Line     `json:"lines"`
}

// Totals are derived from the lines and discount; never caller-supplied.
type Totals struct {
	Subtotal       kit.Amount `json:"subtotal"`
	DiscountAmount kit.Amount `json:"discount_amount"`
	NetTotal       kit.Amount `json:"net_total"`
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9_.:/#\-]{1,255}$`)
	codeRe     = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`)
)

func roundStep(currency string) *big.Int {
	step := int64(1)
	for i := 0; i < 4-kit.MinorUnitDigits(currency); i++ {
		step *= 10
	}
	return big.NewInt(step)
}

// mulRound computes a*b/divisor exactly in big integers and rounds half-up once to
// the currency's minor unit (no intermediate double rounding). a, b >= 0.
func mulRound(a, b kit.Amount, divisor int64, currency string) kit.Amount {
	p := new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(b)))
	step := roundStep(currency)
	den := new(big.Int).Mul(big.NewInt(divisor), step)
	half := new(big.Int).Div(den, big.NewInt(2))
	r := new(big.Int).Mul(new(big.Int).Div(new(big.Int).Add(p, half), den), step)
	if !r.IsInt64() || r.Int64() > int64(saturated) {
		return saturated // out of range: callers refuse anything above maxLineNet
	}
	return kit.Amount(r.Int64())
}

// maxLineNet bounds a line so that 500 lines can never overflow the int64 Amount.
var (
	maxLineNet = kit.MustAmount("1000000000000")
	saturated  = kit.Amount(1 << 62)
)

// LineNet is quantity x unit price, exact, rounded half-up to the currency.
func LineNet(qty, price kit.Amount, currency string) kit.Amount {
	return mulRound(qty, price, 10000, currency)
}

// BuildLines validates requested lines and computes their nets.
func BuildLines(currency string, in []LineInput) ([]Line, error) {
	if len(in) == 0 {
		return nil, kit.Invalid("a quote needs at least one line")
	}
	if len(in) > maxLines {
		return nil, kit.Invalid(fmt.Sprintf("a quote has at most %d lines", maxLines))
	}
	out := make([]Line, 0, len(in))
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
		out = append(out, Line{LineNo: n, Description: desc, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice, LineNet: net})
	}
	return out, nil
}

// ComputeTotals derives subtotal, discount and net total, each fitting the currency.
func ComputeTotals(currency string, lines []Line, discountPct kit.Amount) (Totals, error) {
	var sub kit.Amount
	for _, l := range lines {
		if l.LineNet.IsNegative() || !l.LineNet.FitsCurrency(currency) {
			return Totals{}, kit.Invalid(fmt.Sprintf("line %d: net does not fit the currency", l.LineNo))
		}
		sub = sub.Add(l.LineNet)
	}
	disc := mulRound(sub, discountPct, 100*10000, currency)
	t := Totals{Subtotal: sub, DiscountAmount: disc, NetTotal: sub.Sub(disc)}
	for _, a := range []kit.Amount{t.Subtotal, t.DiscountAmount, t.NetTotal} {
		if !a.FitsCurrency(currency) {
			return Totals{}, kit.Invalid("a total has more precision than the currency allows")
		}
	}
	return t, nil
}

// NeedsDiscountAuthority reports whether the discount is beyond a preparer's own authority.
func NeedsDiscountAuthority(pct kit.Amount) bool { return pct > DiscountSelfAuthorityPercent }

// ParseDay parses YYYY-MM-DD.
func ParseDay(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, kit.Invalid("dates must be YYYY-MM-DD")
	}
	return t, nil
}

func day(t time.Time) string { return t.UTC().Format(dateLayout) }

// Expired reports whether a quote valid through validity (inclusive) has lapsed on today.
func Expired(validity string, today time.Time) bool { return day(today) > validity }

// Validate checks everything that does not need a database, and requires lines
// already built with BuildLines.
func (c Content) Validate() error {
	if !currencyRe.MatchString(c.Currency) {
		return kit.Invalid("currency must be a 3-letter upper-case ISO 4217 code")
	}
	if _, err := ParseDay(c.ValidityDate); err != nil {
		return kit.Invalid("validity_date is required (YYYY-MM-DD)")
	}
	if c.DiscountPercent.IsNegative() || c.DiscountPercent > kit.MustAmount("100") {
		return kit.Invalid("discount_percent must be between 0 and 100")
	}
	if utf8.RuneCountInString(c.Terms) > maxText || utf8.RuneCountInString(c.Assumptions) > maxText {
		return kit.Invalid(fmt.Sprintf("terms and assumptions are limited to %d characters", maxText))
	}
	for name, v := range map[string]string{"price_source_ref": c.PriceSourceRef, "catalog_version_ref": c.CatalogVersionRef} {
		if v != "" && !refRe.MatchString(v) {
			return kit.Invalid(name + " must be 1-255 characters of letters, digits and _ . : / # -")
		}
	}
	if len(c.Lines) == 0 {
		return kit.Invalid("a quote needs at least one line")
	}
	if _, err := ComputeTotals(c.Currency, c.Lines, c.DiscountPercent); err != nil {
		return err
	}
	if c.RequestTaxEstimate {
		p := c.TaxParams
		if p == nil || strings.TrimSpace(p.JurisdictionID) == "" || strings.TrimSpace(p.TaxCategory) == "" ||
			strings.TrimSpace(p.SupplyKind) == "" || strings.TrimSpace(p.SupplyType) == "" {
			return kit.Invalid("tax_params (jurisdiction_id, tax_category, supply_kind, supply_type) are required when request_tax_estimate is set")
		}
		if p.SupplyKind != "GOODS" && p.SupplyKind != "SERVICES" && p.SupplyKind != "DIGITAL_SERVICES" {
			return kit.Invalid("tax_params.supply_kind must be GOODS, SERVICES or DIGITAL_SERVICES")
		}
		if p.SupplyType != "B2B" && p.SupplyType != "B2C" && p.SupplyType != "B2G" {
			return kit.Invalid("tax_params.supply_type must be B2B, B2C or B2G")
		}
	}
	return nil
}

// ── content hash and material change ─────────────────────────────────────────

type hashLine struct {
	Description string `json:"d"`
	ProductRef  string `json:"p"`
	Quantity    string `json:"q"`
	UnitPrice   string `json:"u"`
	LineNet     string `json:"n"`
}

type hashDoc struct {
	Currency       string     `json:"currency"`
	Validity       string     `json:"validity"`
	Terms          string     `json:"terms"`
	Discount       string     `json:"discount"`
	PriceSourceRef string     `json:"price_source"`
	CatalogRef     string     `json:"catalog"`
	Lines          []hashLine `json:"lines"`
}

func hashOf(c Content, withValidity bool) string {
	d := hashDoc{Currency: c.Currency, Terms: c.Terms, Discount: c.DiscountPercent.String(), PriceSourceRef: c.PriceSourceRef, CatalogRef: c.CatalogVersionRef}
	if withValidity {
		d.Validity = c.ValidityDate
	}
	for _, l := range c.Lines {
		d.Lines = append(d.Lines, hashLine{l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.LineNet.String()})
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ContentHash is the approval-binding hash of the commercial content: currency,
// lines, prices, discount, validity, terms and price/catalog source. Assumptions and
// tax-estimate parameters are deliberately outside it.
func ContentHash(c Content) string { return hashOf(c, true) }

// Material reports whether moving from old to next is a MATERIAL change: any change
// to lines, prices, discount, currency, terms, price/catalog source, or an extension
// of the validity, or of the tax-estimate request (the approval covered its estimate).
// A validity shortening or an edit of assumptions is not material and keeps a prior approval.
func Material(old, next Content) bool {
	return hashOf(old, false) != hashOf(next, false) || next.ValidityDate > old.ValidityDate ||
		old.RequestTaxEstimate != next.RequestTaxEstimate || !taxParamsEqual(old.TaxParams, next.TaxParams)
}

// SameContent reports whether nothing at all differs (amendment would be a no-op).
func SameContent(a, b Content) bool {
	return ContentHash(a) == ContentHash(b) && a.Assumptions == b.Assumptions && a.RequestTaxEstimate == b.RequestTaxEstimate &&
		taxParamsEqual(a.TaxParams, b.TaxParams)
}

func taxParamsEqual(a, b *TaxParams) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

// ── persisted shapes ─────────────────────────────────────────────────────────

// Header is the stable quote.
type Header struct {
	QuoteID              string     `json:"quote_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	QuoteNumber          string     `json:"quote_number"`
	CustomerPartyID      string     `json:"customer_party_id"`
	Currency             string     `json:"currency"`
	State                State      `json:"state"`
	CurrentVersionNo     int        `json:"current_version_no"`
	Version              int        `json:"version"`
	CreatedBy            string     `json:"created_by"`
	SubmittedBy          string     `json:"submitted_by,omitempty"`
	ApprovedVersionNo    int        `json:"approved_version_no,omitempty"`
	ApprovedContentHash  string     `json:"approved_content_hash,omitempty"`
	ApprovedBy           string     `json:"approved_by,omitempty"`
	ApprovedAt           *time.Time `json:"approved_at,omitempty"`
	SentAt               *time.Time `json:"sent_at,omitempty"`
	SentVersionNo        int        `json:"sent_version_no,omitempty"`
	AcceptedAt           *time.Time `json:"accepted_at,omitempty"`
	AcceptedBy           string     `json:"accepted_by,omitempty"`
	AcceptanceChannel    string     `json:"acceptance_channel,omitempty"`
	AcceptanceEvidenceID string     `json:"acceptance_evidence_id,omitempty"`
	AcceptedVersionNo    int        `json:"accepted_version_no,omitempty"`
	RejectedAt           *time.Time `json:"rejected_at,omitempty"`
	RejectionReason      string     `json:"rejection_reason,omitempty"`
	ExpiredAt            *time.Time `json:"expired_at,omitempty"`
	ConvertedOrderID     string     `json:"converted_order_id,omitempty"`
	ConvertedAt          *time.Time `json:"converted_at,omitempty"`
	ConvertedVersionNo   int        `json:"converted_version_no,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// TaxEstimate is the stored provenance of a requested tax estimate.
type TaxEstimate struct {
	DeterminationID string     `json:"determination_id"`
	Provenance      string     `json:"provenance,omitempty"`
	Status          string     `json:"status"`
	TaxableAmount   kit.Amount `json:"taxable_amount"`
	TaxRate         string     `json:"tax_rate,omitempty"`
	TaxAmount       kit.Amount `json:"tax_amount"`
	Currency        string     `json:"currency"`
	JurisdictionID  string     `json:"jurisdiction_id,omitempty"`
	IsEstimate      bool       `json:"is_estimate"`
	RecordedBy      string     `json:"recorded_by"`
	RecordedAt      time.Time  `json:"recorded_at"`
}

// Version is one immutable recorded version of the quote content.
type Version struct {
	VersionNo      int          `json:"version_no"`
	ContentHash    string       `json:"content_hash"`
	ChangeKind     string       `json:"change_kind"`
	MaterialChange bool         `json:"material_change"`
	CreatedBy      string       `json:"created_by"`
	CreatedAt      time.Time    `json:"created_at"`
	TaxEstimate    *TaxEstimate `json:"tax_estimate,omitempty"`
	Content
	Totals
}

// Quote is the read model: the header plus its current version.
type Quote struct {
	Header
	Current *Version `json:"current_version"`
}

// HistoryEntry is one row of the append-only quote history.
type HistoryEntry struct {
	QuoteVersion  int            `json:"quote_version"`
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

// ── errors ───────────────────────────────────────────────────────────────────

func errExpired(validity string) *kit.Problem {
	return kit.Unprocessable(kit.CodeQuoteExpired, "the quote validity ended on "+validity)
}

func errApprovalInvalidated(msg string) *kit.Problem {
	return kit.Conflict(CodeApprovalInvalidated, msg)
}

var errSoD = kit.Forbidden(kit.CodeSegregationOfDuties, "a discount beyond the preparer's own authority must be approved by a different principal")
