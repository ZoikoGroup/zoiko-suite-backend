// Package ar04 implements AR-04 Customer Invoice (spec ZS-SVC-C-001 section 7): the authoritative
// receivable / tax document. It owns the invoice, its lines, the legal number, the issue / due dates,
// the tax-result reference, the totals, the immutable issued representation and the six ORTHOGONAL
// state dimensions (lifecycle, delivery, e-invoice, settlement, dispute, attention). Tax comes only
// from tax-determination-svc, the receivable from AR-07, the accounting consequence from ACC-04 via
// the posting queue; none of that is computed or written here.
package ar04

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
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// Authorization actions. Reads are tenant-scoped only (settled AR read posture). Spec permissions
// invoice.create/manage map to AR04_INVOICE_MANAGE; invoice.approve, invoice.issue and
// invoice.cancel.request/approve to the like-named actions.
const (
	ActionInvoiceManage         = "AR04_INVOICE_MANAGE"
	ActionInvoiceApprove        = "AR04_INVOICE_APPROVE"
	ActionInvoiceIssue          = "AR04_INVOICE_ISSUE"
	ActionInvoiceCancelRequest  = "AR04_INVOICE_CANCEL_REQUEST"
	ActionInvoiceCancelApprove  = "AR04_INVOICE_CANCEL_APPROVE"
	ActionInvoiceCorrectionLink = "AR04_INVOICE_CORRECTION_LINK"
	ActionInvoiceDeliveryRecord = "AR04_INVOICE_DELIVERY_RECORD"
)

// GL mapping keys used by the issue posting (registered in general-ledger-svc).
const (
	MappingReceivable = "AR_RECEIVABLE_STANDARD"
	MappingRevenue    = "AR_REVENUE_STANDARD"
	MappingTaxOutput  = "AR_TAX_OUTPUT"
)

// MaterialInvoiceThreshold: an invoice whose gross total is strictly above it is "material": the
// principal who issues it must not be the one who approved it (maker-checker). Compared with the
// invoice's own gross in its own currency (no FX conversion is performed anywhere in AR). Below it
// the issuer may also be the approver. The approver is, at every value, neither creator nor submitter.
var MaterialInvoiceThreshold = kit.MustAmount("100000")

// Codes local to AR-04 beyond the shared spec section 14 classes.
const (
	// CodeCashApplicationRequired: the legacy pay endpoint refuses an invoice that lives in the AR-04 tables.
	CodeCashApplicationRequired = "CASH_APPLICATION_REQUIRED"

	AggregateType = "CustomerInvoice"
	maxListLimit  = 200
	maxLines      = 500
	maxText       = 4000

	// SchemaIssuedInvoice versions the frozen issued representation.
	SchemaIssuedInvoice = "zoiko.ar04.issued-invoice/1"
	// DefaultSeries is the numbering series used when no jurisdiction-pack rule selects another.
	DefaultSeries = "INV"
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventInvoiceDraftCreated        = "ar.customer_invoice.draft_created"
	EventInvoiceApproved            = "ar.customer_invoice.approved"
	EventCustomerInvoiceIssued      = "ar.customer_invoice.issued"
	EventInvoiceDeliveryFailed      = "ar.customer_invoice.delivery_failed"
	EventInvoiceCorrectionLinked    = "ar.customer_invoice.correction_linked"
	EventInvoiceCancellationRequest = "ar.customer_invoice.cancellation_requested"
	EventInvoiceCancellationDecided = "ar.customer_invoice.cancellation_decided"
	EventInvoiceCancelled           = "ar.customer_invoice.cancelled"
	EventInvoiceApprovalInvalidated = "ar.customer_invoice.approval_invalidated"
)

// State is the invoice LIFECYCLE: Draft -> PendingApproval -> Approved -> Issued -> Corrected;
// Cancelled is a branch available only before issue. Delivery, e-invoice, settlement, dispute and
// attention are separate dimensions (Dimensions) and never share this field.
type State string

const (
	StateDraft           State = "DRAFT"
	StatePendingApproval State = "PENDING_APPROVAL"
	StateApproved        State = "APPROVED"
	StateIssued          State = "ISSUED"
	StateCorrected       State = "CORRECTED"
	StateCancelled       State = "CANCELLED"
)

// IsIssued reports whether the invoice is (or was) an authoritative issued document.
func (s State) IsIssued() bool { return s == StateIssued || s == StateCorrected }

// CanAmend: a draft's content may change freely; amending a pending or approved invoice voids the
// approval and the tax result and returns it to DRAFT.
func (s State) CanAmend() bool {
	return s == StateDraft || s == StatePendingApproval || s == StateApproved
}

// Transition names a lifecycle move.
type Transition string

const (
	TrSubmit     Transition = "SUBMIT"
	TrApprove    Transition = "APPROVE"
	TrIssue      Transition = "ISSUE"
	TrAmend      Transition = "AMEND"
	TrInvalidate Transition = "INVALIDATE_APPROVAL"
	TrCancel     Transition = "CANCEL_UNISSUED"
	TrCorrect    Transition = "CORRECT"
)

var transitions = map[Transition]map[State]State{
	TrSubmit:     {StateDraft: StatePendingApproval},
	TrApprove:    {StatePendingApproval: StateApproved},
	TrIssue:      {StateApproved: StateIssued},
	TrAmend:      {StateDraft: StateDraft, StatePendingApproval: StateDraft, StateApproved: StateDraft},
	TrInvalidate: {StateApproved: StateDraft, StatePendingApproval: StateDraft},
	TrCancel:     {StateDraft: StateCancelled, StatePendingApproval: StateCancelled, StateApproved: StateCancelled},
	TrCorrect:    {StateIssued: StateCorrected},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION. An issued or corrected invoice
// refuses every content transition with the immutability class instead (invariant 1).
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	if cur.IsIssued() && t != TrCorrect {
		if t == TrIssue {
			return "", kit.Conflict(kit.CodeInvoiceAlreadyIssued, "the invoice is already issued: it keeps its number and is never issued twice")
		}
		return "", kit.Conflict(kit.CodeImmutable, "an issued invoice is never edited in place: use a correction document")
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// ── orthogonal dimensions ────────────────────────────────────────────────────

// Dimensions are the five states orthogonal to the lifecycle (invariant 4). Each has its own
// column, its own CHECK and its own writer; none can be derived from, or collapsed into, another.
type Dimensions struct {
	Delivery   string `json:"delivery_state"`
	EInvoice   string `json:"einvoice_state"`
	Settlement string `json:"settlement_state"`
	Dispute    string `json:"dispute_state"`
	Attention  string `json:"attention_state"`
}

const (
	DeliveryNotSent    = "NOT_SENT"
	DeliverySent       = "SENT"
	DeliveryFailed     = "DELIVERY_FAILED"
	DeliveryDelivered  = "DELIVERED"
	EInvNotRequired    = "NOT_REQUIRED"
	EInvRequired       = "REQUIRED"
	SettlementUnpaid   = "UNPAID"
	DisputeNone        = "NONE"
	AttentionNone      = "NONE"
	AttentionCancelReq = "CANCELLATION_REQUESTED"
	AttentionCancelOK  = "CANCELLATION_APPROVED"
	AttentionCorrected = "CORRECTION_LINKED"
)

var (
	deliveryStates   = setOf(DeliveryNotSent, DeliverySent, DeliveryFailed, DeliveryDelivered)
	einvoiceStates   = setOf("NOT_REQUIRED", "REQUIRED", "PREPARED", "SUBMITTED", "PENDING_UNKNOWN", "ACCEPTED", "REJECTED")
	settlementStates = setOf("UNPAID", "PARTIALLY_PAID", "PAID", "WRITTEN_OFF", "CREDITED")
	disputeStates    = setOf("NONE", "DISPUTED")
	attentionStates  = setOf(AttentionNone, AttentionCancelReq, AttentionCancelOK, AttentionCorrected)
)

func setOf(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// InitialDimensions is what every pre-issue invoice carries.
func InitialDimensions() Dimensions {
	return Dimensions{Delivery: DeliveryNotSent, EInvoice: EInvNotRequired, Settlement: SettlementUnpaid, Dispute: DisputeNone, Attention: AttentionNone}
}

// Validate checks every dimension is a legal value of its own set and that none has left its
// initial value before the invoice is issued (mirrors the table CHECKs).
func (d Dimensions) Validate(l State) error {
	switch {
	case !deliveryStates[d.Delivery]:
		return kit.Invalid("delivery_state is not a valid delivery state")
	case !einvoiceStates[d.EInvoice]:
		return kit.Invalid("einvoice_state is not a valid e-invoice state")
	case !settlementStates[d.Settlement]:
		return kit.Invalid("settlement_state is not a valid settlement state")
	case !disputeStates[d.Dispute]:
		return kit.Invalid("dispute_state is not a valid dispute state")
	case !attentionStates[d.Attention]:
		return kit.Invalid("attention_state is not a valid attention state")
	}
	if !l.IsIssued() && d != InitialDimensions() {
		return kit.Invalid("the delivery, e-invoice, settlement, dispute and attention dimensions only leave their initial value once the invoice is issued")
	}
	return nil
}

// ValidEInvoiceState, ValidSettlementState and ValidDisputeState gate the kit.Invoices writers.
func ValidEInvoiceState(s string) bool   { return einvoiceStates[s] }
func ValidSettlementState(s string) bool { return settlementStates[s] }
func ValidDisputeState(s string) bool    { return disputeStates[s] }

var deliveryMoves = map[string]map[string]bool{
	DeliveryNotSent:   {DeliverySent: true, DeliveryFailed: true},
	DeliverySent:      {DeliverySent: true, DeliveryDelivered: true, DeliveryFailed: true},
	DeliveryFailed:    {DeliverySent: true, DeliveryDelivered: true, DeliveryFailed: true},
	DeliveryDelivered: {},
}

// NextDelivery validates a delivery-dimension move. It never touches the lifecycle: a delivery or
// e-invoice failure does not void or roll back an issued invoice (spec failure semantics).
func NextDelivery(cur, to string) error {
	if !deliveryStates[to] || to == DeliveryNotSent {
		return kit.Invalid("result must be SENT, DELIVERY_FAILED or DELIVERED")
	}
	if !deliveryMoves[cur][to] {
		return kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("delivery cannot move from %s to %s", cur, to))
	}
	return nil
}

// ── money helpers (copied semantics of AR-03: exact big-integer arithmetic) ──

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9_.:/#\-]{1,255}$`)
	codeRe     = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
	seriesRe   = regexp.MustCompile(`^[A-Z0-9_-]{1,16}$`)

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

// mulRound computes a*b/divisor exactly in big integers and rounds half-up once to the currency's
// minor unit. a, b >= 0.
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

// LineNet is quantity x unit price, exact, rounded half-up once to the currency.
func LineNet(qty, price kit.Amount, currency string) kit.Amount {
	return mulRound(qty, price, 10000, currency)
}

// allocate splits total across weights in whole currency steps, proportionally, largest remainder
// first (ties to the lower index) so the parts always add up to total exactly.
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

// LineInput is a requested line. The net is always computed by the server and there is deliberately
// NO tax field: the invoice editor can neither supply nor override tax (invariant 3).
type LineInput struct {
	Description           string     `json:"description"`
	ProductRef            string     `json:"product_ref,omitempty"`
	Quantity              kit.Amount `json:"quantity"`
	UnitPrice             kit.Amount `json:"unit_price"`
	TaxCategory           string     `json:"tax_category,omitempty"`
	ProductClassification string     `json:"product_classification,omitempty"`
	SupplyKind            string     `json:"supply_kind,omitempty"` // GOODS | SERVICES | DIGITAL_SERVICES
}

// Line is a stored invoice line; Tax is the share of its tax group's determination (derived).
type Line struct {
	LineNo                int        `json:"line_no"`
	Description           string     `json:"description"`
	ProductRef            string     `json:"product_ref,omitempty"`
	Quantity              kit.Amount `json:"quantity"`
	UnitPrice             kit.Amount `json:"unit_price"`
	Net                   kit.Amount `json:"net_amount"`
	Tax                   kit.Amount `json:"tax_amount"`
	TaxCategory           string     `json:"tax_category,omitempty"`
	ProductClassification string     `json:"product_classification,omitempty"`
	SupplyKind            string     `json:"supply_kind,omitempty"`
}

// TaxContext is the tax-relevant context of the supply that is not per line. The seller is the
// verified legal entity and the buyer the customer; both are server-resolved, never typed.
type TaxContext struct {
	JurisdictionID         string `json:"jurisdiction_id,omitempty"`
	SupplyJurisdictionID   string `json:"supply_jurisdiction_id,omitempty"`
	SupplyType             string `json:"supply_type,omitempty"` // B2B | B2C | B2G
	BuyerTaxRegistrationID string `json:"buyer_tax_registration_id,omitempty"`
}

var (
	supplyKinds = setOf("GOODS", "SERVICES", "DIGITAL_SERVICES")
	supplyTypes = setOf("B2B", "B2C", "B2G")
)

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, kit.Invalid("dates must be YYYY-MM-DD")
	}
	return t, nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// BuildLines validates requested lines and computes their nets.
func BuildLines(currency string, in []LineInput) ([]Line, error) {
	if len(in) == 0 {
		return nil, kit.Invalid("an invoice needs at least one line")
	}
	if len(in) > maxLines {
		return nil, kit.Invalid(fmt.Sprintf("an invoice has at most %d lines", maxLines))
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
		case l.TaxCategory != "" && !codeRe.MatchString(l.TaxCategory):
			return nil, kit.Invalid(fmt.Sprintf("line %d: tax_category must be 1-64 characters of A-Z, 0-9 or '_'", n))
		case l.ProductClassification != "" && !refRe.MatchString(l.ProductClassification):
			return nil, kit.Invalid(fmt.Sprintf("line %d: product_classification has invalid characters", n))
		case l.SupplyKind != "" && !supplyKinds[l.SupplyKind]:
			return nil, kit.Invalid(fmt.Sprintf("line %d: supply_kind must be GOODS, SERVICES or DIGITAL_SERVICES", n))
		}
		net := LineNet(l.Quantity, l.UnitPrice, currency)
		if net > maxLineNet {
			return nil, kit.Invalid(fmt.Sprintf("line %d: net amount out of range", n))
		}
		out = append(out, Line{LineNo: n, Description: desc, ProductRef: l.ProductRef, Quantity: l.Quantity, UnitPrice: l.UnitPrice, Net: net,
			TaxCategory: l.TaxCategory, ProductClassification: l.ProductClassification, SupplyKind: l.SupplyKind})
	}
	return out, nil
}

// ValidateTaxContext checks the format of what is present (completeness is a submit-time rule).
func (t TaxContext) Validate() error {
	for name, v := range map[string]string{"jurisdiction_id": t.JurisdictionID, "supply_jurisdiction_id": t.SupplyJurisdictionID,
		"buyer_tax_registration_id": t.BuyerTaxRegistrationID} {
		if v != "" && !refRe.MatchString(v) {
			return kit.Invalid("tax_context." + name + " must be 1-255 characters of letters, digits and _ . : / # -")
		}
	}
	if t.SupplyType != "" && !supplyTypes[t.SupplyType] {
		return kit.Invalid("tax_context.supply_type must be B2B, B2C or B2G")
	}
	return nil
}

// Content is the editable content of a draft (everything the approval is bound to, apart from the
// server-derived tax result).
type Content struct {
	SupplyDate       string     `json:"supply_date"`
	RequestedDueDate string     `json:"requested_due_date,omitempty"`
	CustomerRef      string     `json:"customer_reference,omitempty"`
	Notes            string     `json:"notes,omitempty"`
	OrderID          string     `json:"order_id,omitempty"`
	MilestoneID      string     `json:"milestone_id,omitempty"`
	Tax              TaxContext `json:"tax_context"`
}

// Validate checks everything that does not need a database.
func (c Content) Validate() error {
	if _, err := parseDate(c.SupplyDate); err != nil {
		return kit.Invalid("supply_date is required (YYYY-MM-DD)")
	}
	if c.RequestedDueDate != "" {
		if _, err := parseDate(c.RequestedDueDate); err != nil {
			return kit.Invalid("due_date must be YYYY-MM-DD")
		}
	}
	if utf8.RuneCountInString(c.CustomerRef) > 255 {
		return kit.Invalid("customer_reference is limited to 255 characters")
	}
	if utf8.RuneCountInString(c.Notes) > maxText {
		return kit.Invalid(fmt.Sprintf("notes are limited to %d characters", maxText))
	}
	if (c.OrderID == "") != (c.MilestoneID == "") {
		return kit.Invalid("order_id and milestone_id are given together: a direct-amount order basis is not supported")
	}
	if c.OrderID != "" {
		if err := mustUUID("order_id", c.OrderID); err != nil {
			return err
		}
		if err := mustUUID("milestone_id", c.MilestoneID); err != nil {
			return err
		}
	}
	return c.Tax.Validate()
}

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

// ValidateForTax checks the inputs tax-determination-svc requires, before it is called: the buyer
// and seller come from the invoice itself; the rest from the draft's tax context and line fields.
func ValidateForTax(c Content, lines []Line) error {
	switch {
	case strings.TrimSpace(c.Tax.JurisdictionID) == "":
		return kit.Invalid("tax_context.jurisdiction_id is required before the invoice can be submitted")
	case c.Tax.SupplyType == "":
		return kit.Invalid("tax_context.supply_type (B2B, B2C or B2G) is required before the invoice can be submitted")
	}
	for _, l := range lines {
		switch {
		case l.TaxCategory == "":
			return kit.Invalid(fmt.Sprintf("line %d: tax_category is required before the invoice can be submitted", l.LineNo))
		case strings.TrimSpace(l.ProductClassification) == "":
			return kit.Invalid(fmt.Sprintf("line %d: product_classification is required before the invoice can be submitted", l.LineNo))
		case l.SupplyKind == "":
			return kit.Invalid(fmt.Sprintf("line %d: supply_kind is required before the invoice can be submitted", l.LineNo))
		}
	}
	return nil
}

// ── tax groups ───────────────────────────────────────────────────────────────

// GroupSpec is one (tax_category, product_classification, supply_kind) group of lines; one tax
// determination is obtained per group.
type GroupSpec struct {
	GroupNo               int
	TaxCategory           string
	ProductClassification string
	SupplyKind            string
	Net                   kit.Amount
	LineNos               []int
}

// BuildGroups groups lines deterministically (sorted by key, numbered from 1).
func BuildGroups(lines []Line) []GroupSpec {
	idx := map[[3]string]*GroupSpec{}
	var keys [][3]string
	for _, l := range lines {
		k := [3]string{l.TaxCategory, l.ProductClassification, l.SupplyKind}
		g, ok := idx[k]
		if !ok {
			g = &GroupSpec{TaxCategory: k[0], ProductClassification: k[1], SupplyKind: k[2]}
			idx[k] = g
			keys = append(keys, k)
		}
		g.Net = g.Net.Add(l.Net)
		g.LineNos = append(g.LineNos, l.LineNo)
	}
	sort.Slice(keys, func(i, j int) bool {
		for p := 0; p < 3; p++ {
			if keys[i][p] != keys[j][p] {
				return keys[i][p] < keys[j][p]
			}
		}
		return false
	})
	out := make([]GroupSpec, len(keys))
	for i, k := range keys {
		g := *idx[k]
		g.GroupNo = i + 1
		out[i] = g
	}
	return out
}

// TaxGroup is the stored result of one tax determination: the reference, its provenance and the
// exact amounts. The amounts are the TAX domain's, copied; never computed here.
type TaxGroup struct {
	GroupNo               int        `json:"group_no"`
	TaxCategory           string     `json:"tax_category"`
	ProductClassification string     `json:"product_classification"`
	SupplyKind            string     `json:"supply_kind"`
	DeterminationID       string     `json:"determination_id"`
	Provenance            string     `json:"provenance"`
	Status                string     `json:"determination_status"`
	JurisdictionID        string     `json:"jurisdiction_id,omitempty"`
	TaxRate               string     `json:"tax_rate,omitempty"`
	Net                   kit.Amount `json:"net_amount"`
	Taxable               kit.Amount `json:"taxable_amount"`
	Tax                   kit.Amount `json:"tax_amount"`
	RecordedBy            string     `json:"recorded_by,omitempty"`
	RecordedAt            *time.Time `json:"recorded_at,omitempty"`
}

// GroupFromDetermination validates a determination against the group it was requested for and
// converts it. Anything unusable is TAX_DETERMINATION_MISSING: AR never improvises a figure.
func GroupFromDetermination(d *taxdet.Determination, spec GroupSpec, entityID, currency, by string, at time.Time) (TaxGroup, error) {
	missing := func(msg string) error { return kit.Unprocessable(kit.CodeTaxDeterminationMissing, msg) }
	switch {
	case d == nil || strings.TrimSpace(d.DeterminationID) == "":
		return TaxGroup{}, missing("tax determination returned no result")
	case !d.Usable():
		return TaxGroup{}, missing("tax determination " + d.DeterminationID + " is not usable (status " + d.Status + ")")
	case d.LegalEntityID != "" && d.LegalEntityID != entityID:
		return TaxGroup{}, missing("tax determination " + d.DeterminationID + " belongs to another legal entity")
	case d.Currency != "" && d.Currency != currency:
		return TaxGroup{}, missing("tax determination " + d.DeterminationID + " is in another currency")
	case strings.TrimSpace(d.Provenance()) == "":
		return TaxGroup{}, missing("tax determination " + d.DeterminationID + " carries no rule-pack provenance")
	case d.Tax.IsNegative() || !d.Tax.FitsCurrency(currency):
		return TaxGroup{}, missing("tax determination " + d.DeterminationID + " carries a tax amount that is not valid for the currency")
	}
	t := at
	return TaxGroup{GroupNo: spec.GroupNo, TaxCategory: spec.TaxCategory, ProductClassification: spec.ProductClassification, SupplyKind: spec.SupplyKind,
		DeterminationID: d.DeterminationID, Provenance: d.Provenance(), Status: d.Status, JurisdictionID: d.JurisdictionID, TaxRate: d.TaxRatePercentage,
		Net: spec.Net, Taxable: d.TaxableAmount, Tax: d.Tax, RecordedBy: by, RecordedAt: &t}, nil
}

// ApplyTax allocates each group's tax across its lines (exactly, largest remainder) so the line
// taxes always add up to the determination's tax. It resets all line taxes first.
func ApplyTax(lines []Line, groups []TaxGroup, currency string) error {
	for i := range lines {
		lines[i].Tax = 0
	}
	for _, g := range groups {
		var idxs []int
		var weights []kit.Amount
		for i, l := range lines {
			if l.TaxCategory == g.TaxCategory && l.ProductClassification == g.ProductClassification && l.SupplyKind == g.SupplyKind {
				idxs = append(idxs, i)
				weights = append(weights, l.Net)
			}
		}
		if len(idxs) == 0 {
			return kit.Invalid(fmt.Sprintf("tax group %d matches no line", g.GroupNo))
		}
		if g.Tax.IsPositive() && func() bool {
			var s kit.Amount
			for _, w := range weights {
				s = s.Add(w)
			}
			return s.IsZero()
		}() {
			return kit.Invalid(fmt.Sprintf("tax group %d has tax but no net to allocate it to", g.GroupNo))
		}
		parts := allocate(g.Tax, weights, currency)
		for k, i := range idxs {
			lines[i].Tax = parts[k]
		}
	}
	return nil
}

// Totals are derived: net from the lines, tax from the determinations, gross is their exact sum.
type Totals struct {
	Net   kit.Amount `json:"net_total"`
	Tax   kit.Amount `json:"tax_total"`
	Gross kit.Amount `json:"gross_total"`
}

// TotalsOf recomputes the totals and proves lines and groups agree: line nets and line taxes must
// add up to the totals exactly, and every figure must fit the currency.
func TotalsOf(lines []Line, groups []TaxGroup, currency string) (Totals, error) {
	var t Totals
	var lineTax, groupNet kit.Amount
	for _, l := range lines {
		if !l.Net.FitsCurrency(currency) || !l.Tax.FitsCurrency(currency) {
			return Totals{}, kit.Invalid(fmt.Sprintf("line %d: amounts have more precision than the currency allows", l.LineNo))
		}
		t.Net = t.Net.Add(l.Net)
		lineTax = lineTax.Add(l.Tax)
	}
	for _, g := range groups {
		t.Tax = t.Tax.Add(g.Tax)
		groupNet = groupNet.Add(g.Net)
	}
	if len(groups) > 0 && (lineTax != t.Tax || groupNet != t.Net) {
		return Totals{}, kit.Invalid("line amounts and tax determinations do not add up to the totals")
	}
	t.Gross = t.Net.Add(t.Tax)
	if !t.Gross.FitsCurrency(currency) {
		return Totals{}, kit.Invalid("the totals have more precision than the currency allows")
	}
	return t, nil
}

// IsMaterial reports whether the gross is above MaterialInvoiceThreshold.
func IsMaterial(gross kit.Amount) bool { return gross > MaterialInvoiceThreshold }

// ── hashing / the sealed issued representation ───────────────────────────────

// Header is the stable invoice. Timestamps and dates are UTC; dates are YYYY-MM-DD.
type Header struct {
	InvoiceID       string `json:"invoice_id"`
	TenantID        string `json:"tenant_id"`
	LegalEntityID   string `json:"legal_entity_id"`
	CustomerPartyID string `json:"customer_party_id"`
	Currency        string `json:"currency"`
	State           State  `json:"lifecycle_state"`
	Dimensions
	AccountingState     string     `json:"accounting_state"`
	AccountingJournalID string     `json:"accounting_journal_id,omitempty"`
	AccountingError     string     `json:"accounting_error,omitempty"`
	InvoiceNumber       string     `json:"invoice_number,omitempty"`
	NumberingSeries     string     `json:"numbering_series,omitempty"`
	SequenceNo          int64      `json:"sequence_no,omitempty"`
	Content             Content    `json:"content"`
	IssueDate           string     `json:"issue_date,omitempty"`
	DueDate             string     `json:"due_date,omitempty"`
	PaymentTermsCode    string     `json:"payment_terms_code,omitempty"`
	PaymentTermsDays    int        `json:"payment_terms_days,omitempty"`
	NetTotal            kit.Amount `json:"net_total"`
	TaxTotal            kit.Amount `json:"tax_total"`
	GrossTotal          kit.Amount `json:"gross_total"`
	ContentHash         string     `json:"content_hash"`
	ApprovedHash        string     `json:"approved_content_hash,omitempty"`
	ApprovedBy          string     `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	CreatedBy           string     `json:"created_by"`
	SubmittedBy         string     `json:"submitted_by,omitempty"`
	SubmittedAt         *time.Time `json:"submitted_at,omitempty"`
	CancelReason        string     `json:"cancel_reason,omitempty"`
	CancelledBy         string     `json:"cancelled_by,omitempty"`
	CancelledAt         *time.Time `json:"cancelled_at,omitempty"`
	IssuedBy            string     `json:"issued_by,omitempty"`
	IssuedAt            *time.Time `json:"issued_at,omitempty"`
	IssuedContentHash   string     `json:"issued_content_hash,omitempty"`
	OpenItemID          string     `json:"open_item_id,omitempty"`
	DeliveryReference   string     `json:"delivery_reference,omitempty"`
	DeliveryDetail      string     `json:"delivery_detail,omitempty"`
	Version             int        `json:"version"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`

	issuedCanonical string
}

// Invoice is the read model: header, lines and tax result.
type Invoice struct {
	Header
	Lines     []Line     `json:"lines"`
	TaxGroups []TaxGroup `json:"tax_groups"`
}

// ContentHash is the approval-binding hash of everything an approver relies on: the header content,
// the lines and the tax result (determination ids, provenance and amounts). It deliberately excludes
// the legal number and issue date, which are allocated only at issue.
func ContentHash(h *Header, lines []Line, groups []TaxGroup) string {
	type hl struct {
		N int    `json:"n"`
		D string `json:"d"`
		P string `json:"p"`
		Q string `json:"q"`
		U string `json:"u"`
		L string `json:"l"`
		X string `json:"x"`
		C string `json:"c"`
		K string `json:"k"`
		S string `json:"s"`
	}
	type hg struct {
		N  int    `json:"n"`
		C  string `json:"c"`
		K  string `json:"k"`
		S  string `json:"s"`
		ID string `json:"id"`
		PV string `json:"pv"`
		B  string `json:"b"`
		X  string `json:"x"`
	}
	type doc struct {
		Entity   string `json:"entity"`
		Customer string `json:"customer"`
		Currency string `json:"currency"`
		Supply   string `json:"supply"`
		Due      string `json:"due"`
		Ref      string `json:"ref"`
		Notes    string `json:"notes"`
		Order    string `json:"order"`
		Mile     string `json:"mile"`
		Tax      TaxContext
		Lines    []hl `json:"lines"`
		Groups   []hg `json:"groups"`
	}
	c := h.Content
	d := doc{Entity: h.LegalEntityID, Customer: h.CustomerPartyID, Currency: h.Currency, Supply: c.SupplyDate, Due: c.RequestedDueDate, Ref: c.CustomerRef,
		Notes: c.Notes, Order: c.OrderID, Mile: c.MilestoneID, Tax: c.Tax}
	for _, l := range lines {
		d.Lines = append(d.Lines, hl{l.LineNo, l.Description, l.ProductRef, l.Quantity.String(), l.UnitPrice.String(), l.Net.String(), l.Tax.String(),
			l.TaxCategory, l.ProductClassification, l.SupplyKind})
	}
	for _, g := range groups {
		d.Groups = append(d.Groups, hg{g.GroupNo, g.TaxCategory, g.ProductClassification, g.SupplyKind, g.DeterminationID, g.Provenance, g.Net.String(), g.Tax.String()})
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IssuedLine / IssuedTaxGroup / IssuedDocument form the frozen issued representation. Field order
// is the struct order, so the canonical JSON is deterministic.
type IssuedLine struct {
	LineNo                int        `json:"line_no"`
	Description           string     `json:"description"`
	ProductRef            string     `json:"product_ref"`
	Quantity              kit.Amount `json:"quantity"`
	UnitPrice             kit.Amount `json:"unit_price"`
	Net                   kit.Amount `json:"net_amount"`
	Tax                   kit.Amount `json:"tax_amount"`
	TaxCategory           string     `json:"tax_category"`
	ProductClassification string     `json:"product_classification"`
	SupplyKind            string     `json:"supply_kind"`
}

type IssuedTaxGroup struct {
	GroupNo               int        `json:"group_no"`
	TaxCategory           string     `json:"tax_category"`
	ProductClassification string     `json:"product_classification"`
	SupplyKind            string     `json:"supply_kind"`
	DeterminationID       string     `json:"determination_id"`
	Provenance            string     `json:"rule_pack_provenance"`
	JurisdictionID        string     `json:"jurisdiction_id"`
	TaxRate               string     `json:"tax_rate"`
	Net                   kit.Amount `json:"net_amount"`
	Tax                   kit.Amount `json:"tax_amount"`
}

type IssuedDocument struct {
	Schema            string           `json:"schema"`
	InvoiceID         string           `json:"invoice_id"`
	TenantID          string           `json:"tenant_id"`
	LegalEntityID     string           `json:"legal_entity_id"`
	CustomerPartyID   string           `json:"customer_party_id"`
	InvoiceNumber     string           `json:"invoice_number"`
	NumberingSeries   string           `json:"numbering_series"`
	SequenceNo        int64            `json:"sequence_no"`
	Currency          string           `json:"currency"`
	IssueDate         string           `json:"issue_date"`
	SupplyDate        string           `json:"supply_date"`
	DueDate           string           `json:"due_date"`
	PaymentTermsCode  string           `json:"payment_terms_code"`
	PaymentTermsDays  int              `json:"payment_terms_days"`
	CustomerReference string           `json:"customer_reference"`
	Notes             string           `json:"notes"`
	OrderID           string           `json:"order_id"`
	MilestoneID       string           `json:"milestone_id"`
	TaxContext        TaxContext       `json:"tax_context"`
	Lines             []IssuedLine     `json:"lines"`
	TaxGroups         []IssuedTaxGroup `json:"tax_groups"`
	Net               kit.Amount       `json:"net_total"`
	Tax               kit.Amount       `json:"tax_total"`
	Gross             kit.Amount       `json:"gross_total"`
	ApprovedHash      string           `json:"approved_content_hash"`
	ApprovedBy        string           `json:"approved_by"`
	ApprovedAt        string           `json:"approved_at"`
	IssuedBy          string           `json:"issued_by"`
	IssuedAt          string           `json:"issued_at"`
	EInvoiceRequired  bool             `json:"einvoice_required"`
}

// IssueInput is everything the sealing needs, all server-derived.
type IssueInput struct {
	H                *Header
	Lines            []Line
	Groups           []TaxGroup
	Number           string
	Series           string
	Sequence         int64
	IssueDate        string
	DueDate          string
	TermsCode        string
	TermsDays        int
	IssuedBy         string
	IssuedAt         time.Time
	EInvoiceRequired bool
}

// BuildIssued produces the canonical issued-representation JSON and its sha256. The exact bytes are
// stored (TEXT, so no normalisation can change them) beside the hash; GetInvoiceAsIssued returns
// them verbatim and proves the hash.
func BuildIssued(in IssueInput) (canonical string, hash string, err error) {
	h := in.H
	d := IssuedDocument{Schema: SchemaIssuedInvoice, InvoiceID: h.InvoiceID, TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, CustomerPartyID: h.CustomerPartyID,
		InvoiceNumber: in.Number, NumberingSeries: in.Series, SequenceNo: in.Sequence, Currency: h.Currency, IssueDate: in.IssueDate, SupplyDate: h.Content.SupplyDate,
		DueDate: in.DueDate, PaymentTermsCode: in.TermsCode, PaymentTermsDays: in.TermsDays, CustomerReference: h.Content.CustomerRef, Notes: h.Content.Notes,
		OrderID: h.Content.OrderID, MilestoneID: h.Content.MilestoneID, TaxContext: h.Content.Tax, Lines: []IssuedLine{}, TaxGroups: []IssuedTaxGroup{},
		Net: h.NetTotal, Tax: h.TaxTotal, Gross: h.GrossTotal, ApprovedHash: h.ApprovedHash, ApprovedBy: h.ApprovedBy, IssuedBy: in.IssuedBy,
		IssuedAt: in.IssuedAt.UTC().Format(time.RFC3339Nano), EInvoiceRequired: in.EInvoiceRequired}
	if h.ApprovedAt != nil {
		d.ApprovedAt = h.ApprovedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, l := range in.Lines {
		d.Lines = append(d.Lines, IssuedLine{l.LineNo, l.Description, l.ProductRef, l.Quantity, l.UnitPrice, l.Net, l.Tax, l.TaxCategory, l.ProductClassification, l.SupplyKind})
	}
	for _, g := range in.Groups {
		d.TaxGroups = append(d.TaxGroups, IssuedTaxGroup{g.GroupNo, g.TaxCategory, g.ProductClassification, g.SupplyKind, g.DeterminationID, g.Provenance, g.JurisdictionID, g.TaxRate, g.Net, g.Tax})
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

// HashOf is the sha256 of the stored canonical text (used to verify integrity on read).
func HashOf(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ── numbering ────────────────────────────────────────────────────────────────

// NumberingProfile is the per-entity format of the legal number. The format and prefix are a
// jurisdiction-controlled, effective-dated decision (invariant 18): nothing is hard-coded globally.
// When an entity has no profile row, DefaultNumbering applies; that default is a documented
// pre-production placeholder and carries no legal meaning.
type NumberingProfile struct {
	Series string
	Prefix string
	Width  int
}

// DefaultNumbering applies when a legal entity has no ar04_numbering_profiles row: INV-000001.
var DefaultNumbering = NumberingProfile{Series: DefaultSeries, Prefix: "INV-", Width: 6}

// Format renders sequence n as prefix + zero-padded number. A number wider than the width is never
// truncated.
func (p NumberingProfile) Format(n int64) string {
	w := p.Width
	if w < 1 {
		w = 1
	}
	return fmt.Sprintf("%s%0*d", p.Prefix, w, n)
}

// Validate checks a profile row.
func (p NumberingProfile) Validate() error {
	if !seriesRe.MatchString(p.Series) {
		return kit.Invalid("numbering series must be 1-16 characters of A-Z, 0-9, _ or -")
	}
	if p.Width < 1 || p.Width > 12 {
		return kit.Invalid("numbering width must be 1-12")
	}
	return nil
}

// ── persisted evidence shapes ────────────────────────────────────────────────

// HistoryEntry is one row of the append-only invoice history.
type HistoryEntry struct {
	InvoiceVersion int            `json:"invoice_version"`
	Command        string         `json:"command"`
	LifecycleAfter State          `json:"lifecycle_after"`
	ActorID        string         `json:"actor_id"`
	ApproverID     string         `json:"approver_id,omitempty"`
	ReasonCode     string         `json:"reason_code,omitempty"`
	EvidenceID     string         `json:"evidence_id,omitempty"`
	ContentHash    string         `json:"content_hash,omitempty"`
	PriorValues    map[string]any `json:"prior_values,omitempty"`
	NewValues      map[string]any `json:"new_values,omitempty"`
	CorrelationID  string         `json:"correlation_id,omitempty"`
	RecordedAt     time.Time      `json:"recorded_at"`
}

// ApprovalRecord is one approval bound to a content hash.
type ApprovalRecord struct {
	ContentHash string     `json:"content_hash"`
	GrossTotal  kit.Amount `json:"gross_total"`
	CreatedBy   string     `json:"created_by"`
	SubmittedBy string     `json:"submitted_by"`
	ApprovedBy  string     `json:"approved_by"`
	ApprovedAt  time.Time  `json:"approved_at"`
}

// CancellationRequest is a legal-cancellation request and its decision.
type CancellationRequest struct {
	RequestID      string     `json:"request_id"`
	Status         string     `json:"status"`
	ReasonCode     string     `json:"reason_code"`
	EvidenceID     string     `json:"evidence_id"`
	RequestedBy    string     `json:"requested_by"`
	RequestedAt    time.Time  `json:"requested_at"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
}

// CorrectionLink is one correction document linked to an issued invoice.
type CorrectionLink struct {
	CorrectionID string    `json:"correction_id"`
	Kind         string    `json:"kind"`
	ReasonCode   string    `json:"reason_code,omitempty"`
	EvidenceID   string    `json:"evidence_id,omitempty"`
	LinkedBy     string    `json:"linked_by"`
	LinkedAt     time.Time `json:"linked_at"`
}

// Correction kinds.
const (
	KindCreditNote     = "CREDIT_NOTE"
	KindDebitNote      = "DEBIT_NOTE"
	KindFullCorrection = "FULL_CORRECTION"
)

var correctionKinds = setOf(KindCreditNote, KindDebitNote, KindFullCorrection)
