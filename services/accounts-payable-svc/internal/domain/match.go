package domain

// AP-06 Invoice Matching.
//
// EvaluateMatch is a PURE function: the same frozen inputs always give the same
// outcome (deterministic and replayable), it performs no I/O and it never mutates
// a source document. The handler gathers the evidence (invoice, PO revision,
// receipts, prior invoiced quantities, policy), the store persists the outcome.
//
// Doctrine, straight from the AP-06 contract and its four negative paths:
//   - Missing PO or receipt data is INCOMPLETE, never a match: a PO line with no
//     receipt record is MISSING_RECEIPT (not "received zero, difference zero").
//   - An invoice quantity above what was received (or ordered) is an exception;
//     the engine never auto-matches it.
//   - The engine never waives anything: variances are only ever cleared by a
//     human (RecordApprovedVariance), under segregation of duties.
//   - Tolerances come from the frozen policy snapshot; nothing widens them during a run.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// MatchMode is the matching policy shape. N-way is not offered: the spec's n-way
// needs further evidence sources (inspection, contract) that do not exist yet.
type MatchMode string

const (
	MatchTwoWay   MatchMode = "TWO_WAY"   // invoice vs PO
	MatchThreeWay MatchMode = "THREE_WAY" // invoice vs PO vs receipts
)

// MatchPolicy is one immutable policy version.
type MatchPolicy struct {
	TenantID           string    `json:"tenant_id,omitempty"`
	LegalEntityID      string    `json:"legal_entity_id"`
	PolicyVersion      int       `json:"policy_version"` // 0 = the built-in default
	Mode               MatchMode `json:"mode"`
	QtyTolerancePct    float64   `json:"qty_tolerance_pct"`
	PriceTolerancePct  float64   `json:"price_tolerance_pct"`
	AmountToleranceAbs float64   `json:"amount_tolerance_abs"`
	Reason             string    `json:"reason,omitempty"`
	CreatedBy          string    `json:"created_by,omitempty"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
	IsDefault          bool      `json:"is_default"`
}

// DefaultMatchPolicy is what applies when a legal entity has configured nothing:
// three-way, ZERO tolerance. It is the strictest possible reading, so using it can
// only produce more exceptions, never a falsely cleared match.
func DefaultMatchPolicy(legalEntityID string) MatchPolicy {
	return MatchPolicy{LegalEntityID: legalEntityID, PolicyVersion: 0, Mode: MatchThreeWay, IsDefault: true}
}

// Validate refuses a policy that is not a usable configuration.
func (p MatchPolicy) Validate() error {
	if p.Mode != MatchTwoWay && p.Mode != MatchThreeWay {
		return fmt.Errorf("mode must be TWO_WAY or THREE_WAY")
	}
	for name, v := range map[string]float64{"qty_tolerance_pct": p.QtyTolerancePct, "price_tolerance_pct": p.PriceTolerancePct, "amount_tolerance_abs": p.AmountToleranceAbs} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a non-negative number", name)
		}
	}
	if p.QtyTolerancePct > 100 || p.PriceTolerancePct > 100 {
		return fmt.Errorf("percentage tolerances cannot exceed 100")
	}
	return nil
}

// Exception categories.
const (
	ExcMissingPOData       = "MISSING_PO_DATA"       // PO, its revision or its lines could not be obtained
	ExcMissingReceiptData  = "MISSING_RECEIPT_DATA"  // receipt evidence could not be obtained at all
	ExcMissingReceipt      = "MISSING_RECEIPT"       // this PO line has no receipt record
	ExcMissingInvoiceLines = "MISSING_INVOICE_LINES" // a pre-contract invoice has no lines to match
	ExcUnmappedLine        = "UNMAPPED_LINE"         // invoice line names no PO line
	ExcUnknownPOLine       = "UNKNOWN_PO_LINE"       // invoice line names a PO line the PO does not have
	ExcCurrencyMismatch    = "CURRENCY_MISMATCH"     // invoice and PO currencies differ (no FX is approved)
	ExcQtyOverOrder        = "QTY_OVER_ORDER"        // invoiced (cumulative) above ordered
	ExcQtyOverReceipt      = "QTY_OVER_RECEIPT"      // invoiced (cumulative) above received
	ExcPriceVariance       = "PRICE_VARIANCE"        // unit price differs beyond tolerance
	ExcAmountVariance      = "AMOUNT_VARIANCE"       // line amount differs beyond tolerance
	ClassIncomplete        = "INCOMPLETE"            // evidence is missing: fix the evidence and re-perform
	ClassVariance          = "VARIANCE"              // evidence is complete and differs: a human may approve
)

// Exception statuses.
const (
	ExceptionOpen             = "OPEN"
	ExceptionAcknowledged     = "ACKNOWLEDGED"
	ExceptionRouted           = "ROUTED"
	ExceptionVarianceApproved = "VARIANCE_APPROVED"
)

// ── evidence (frozen inputs) ────────────────────────────────────────────────

// POLineEvidence / POEvidence mirror what AP-03 stated, decoupled from the client package.
type POLineEvidence struct {
	LineID    string  `json:"line_id"`
	Quantity  float64 `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

type POEvidence struct {
	PurchaseOrderID string           `json:"purchase_order_id"`
	Currency        string           `json:"currency"`
	Revision        int              `json:"revision"`
	HasRevision     bool             `json:"has_revision"`
	HasLines        bool             `json:"has_lines"`
	Lines           []POLineEvidence `json:"lines"`
	// Unavailable records that AP-03 could not be reached or did not know the PO.
	Unavailable bool `json:"unavailable,omitempty"`
}

// ReceiptEvidence is AP-04's received-to-date, per PO line.
type ReceiptEvidence struct {
	HasLines    bool               `json:"has_lines"`
	Received    map[string]float64 `json:"received"` // po_line_id -> quantity; ABSENT means no receipt record
	Unavailable bool               `json:"unavailable,omitempty"`
}

// MatchEvidence is everything one run is decided from.
type MatchEvidence struct {
	Invoice       VendorInvoice
	PO            POEvidence
	Receipts      ReceiptEvidence
	PriorInvoiced map[string]float64 // po_line_id -> quantity invoiced by OTHER invoices (AP-03 progress)
	PriorUnavail  bool
	Policy        MatchPolicy
}

// ── outcome ─────────────────────────────────────────────────────────────────

type MatchLineResult struct {
	InvoiceLineID    string     `json:"invoice_line_id,omitempty"`
	LineNumber       int        `json:"line_number"`
	POLineID         string     `json:"po_line_id,omitempty"`
	InvoicedQuantity float64    `json:"invoiced_quantity"`
	UnitPrice        float64    `json:"unit_price"`
	NetAmount        float64    `json:"net_amount"`
	OrderedQuantity  float64    `json:"ordered_quantity"`
	POUnitPrice      float64    `json:"po_unit_price"`
	ReceivedQuantity *float64   `json:"received_quantity,omitempty"` // nil = no receipt record
	PriorInvoiced    float64    `json:"prior_invoiced_quantity"`
	PriceVariancePct float64    `json:"price_variance_pct"`
	AmountVariance   float64    `json:"amount_variance"`
	Result           MatchState `json:"result"`
	Categories       []string   `json:"exception_categories"`
}

type MatchExceptionRecord struct {
	ExceptionID      string  `json:"exception_id,omitempty"`
	RunID            string  `json:"run_id,omitempty"`
	InvoiceID        string  `json:"invoice_id,omitempty"`
	LegalEntityID    string  `json:"legal_entity_id,omitempty"`
	InvoiceLineID    string  `json:"invoice_line_id,omitempty"`
	POLineID         string  `json:"po_line_id,omitempty"`
	Category         string  `json:"category"`
	Class            string  `json:"class"`
	Waivable         bool    `json:"waivable"`
	Expected         float64 `json:"expected"`
	Actual           float64 `json:"actual"`
	Difference       float64 `json:"difference"`
	Detail           string  `json:"detail"`
	Status           string  `json:"status"`
	AcknowledgedBy   string  `json:"acknowledged_by,omitempty"`
	RoutedTo         string  `json:"routed_to,omitempty"`
	RoutedBy         string  `json:"routed_by,omitempty"`
	RouteReason      string  `json:"route_reason,omitempty"`
	ResolvedBy       string  `json:"resolved_by,omitempty"`
	ResolutionReason string  `json:"resolution_reason,omitempty"`
	ResolutionRef    string  `json:"resolution_ref,omitempty"`
}

type MatchTotals struct {
	InvoiceLines    int     `json:"invoice_lines"`
	MatchedLines    int     `json:"matched_lines"`
	ToleranceLines  int     `json:"within_tolerance_lines"`
	ExceptionLines  int     `json:"exception_lines"`
	IncompleteLines int     `json:"incomplete_lines"`
	InvoiceNet      float64 `json:"invoice_net_amount"`
	Exceptions      int     `json:"exceptions"`
}

type MatchOutcome struct {
	Result     MatchState             `json:"result"`
	Lines      []MatchLineResult      `json:"lines"`
	Exceptions []MatchExceptionRecord `json:"exceptions"`
	Totals     MatchTotals            `json:"totals"`
	InputHash  string                 `json:"input_hash"`
}

// Cleared reports whether the outcome, on its own, lets the invoice go to approval.
func (o MatchOutcome) Cleared() bool {
	return o.Result == MatchMatched || o.Result == MatchWithinTolerance
}

const eps = 0.005

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// InputHash fingerprints the frozen evidence. Two runs over identical evidence have
// identical hashes, which is how a re-performance on unchanged evidence is recognised
// as a no-op rather than a new run.
func (e MatchEvidence) InputHash() string {
	type line struct {
		ID    string  `json:"id"`
		PO    string  `json:"po"`
		Qty   float64 `json:"q"`
		Price float64 `json:"p"`
		Net   float64 `json:"n"`
	}
	lines := make([]line, 0, len(e.Invoice.Lines))
	for _, l := range e.Invoice.Lines {
		po := ""
		if l.POLineReference != nil {
			po = *l.POLineReference
		}
		lines = append(lines, line{l.InvoiceLineID, po, l.Quantity, l.UnitPrice, l.NetAmount})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].ID < lines[j].ID })
	po := e.PO
	po.Lines = append([]POLineEvidence(nil), po.Lines...)
	sort.Slice(po.Lines, func(i, j int) bool { return po.Lines[i].LineID < po.Lines[j].LineID })
	payload := map[string]any{
		"invoice_id": e.Invoice.InvoiceID, "currency": e.Invoice.CurrencyCode, "po_ref": ptrStr(e.Invoice.PurchaseOrderID),
		"lines": lines, "po": po, "receipts": e.Receipts, "prior": e.PriorInvoiced, "prior_unavailable": e.PriorUnavail,
		"policy": map[string]any{"v": e.Policy.PolicyVersion, "mode": e.Policy.Mode, "q": e.Policy.QtyTolerancePct, "p": e.Policy.PriceTolerancePct, "a": e.Policy.AmountToleranceAbs},
	}
	raw, _ := json.Marshal(payload) // map keys are sorted by encoding/json: canonical
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type lineAcc struct {
	res  *MatchLineResult
	cats map[string]bool
	tol  bool
}

// EvaluateMatch decides one run. See the package comment for the rules.
func EvaluateMatch(e MatchEvidence) MatchOutcome {
	out := MatchOutcome{InputHash: e.InputHash(), Lines: []MatchLineResult{}, Exceptions: []MatchExceptionRecord{}}
	inv := e.Invoice
	out.Totals.InvoiceLines = len(inv.Lines)
	threeWay := e.Policy.Mode == MatchThreeWay

	add := func(x MatchExceptionRecord) {
		x.Status = ExceptionOpen
		out.Exceptions = append(out.Exceptions, x)
	}
	incomplete := func(cat, detail string) {
		add(MatchExceptionRecord{Category: cat, Class: ClassIncomplete, Waivable: false, Detail: detail})
	}

	// Header-level evidence. Any hole here makes the whole run INCOMPLETE.
	headerIncomplete := false
	switch {
	case len(inv.Lines) == 0:
		incomplete(ExcMissingInvoiceLines, "the invoice carries no lines to match (a pre-contract invoice must be re-captured)")
		headerIncomplete = true
	case e.PO.Unavailable || !e.PO.HasLines || !e.PO.HasRevision:
		incomplete(ExcMissingPOData, "purchase-order lines/revision are not available from AP-03; a missing PO is never a match")
		headerIncomplete = true
	}
	if !headerIncomplete && threeWay && (e.Receipts.Unavailable || !e.Receipts.HasLines) {
		incomplete(ExcMissingReceiptData, "no receipt evidence is available from AP-04; a missing receipt is never a zero-difference match")
		headerIncomplete = true
	}
	if !headerIncomplete && e.PriorUnavail {
		incomplete(ExcMissingPOData, "AP-03 invoiced-to-date quantities are not available, so over-invoicing cannot be ruled out")
		headerIncomplete = true
	}
	if !headerIncomplete && inv.CurrencyCode != e.PO.Currency {
		add(MatchExceptionRecord{Category: ExcCurrencyMismatch, Class: ClassIncomplete, Waivable: false,
			Detail: fmt.Sprintf("invoice currency %s differs from PO currency %s and no FX conversion is approved", inv.CurrencyCode, e.PO.Currency)})
	}

	poLines := map[string]POLineEvidence{}
	for _, l := range e.PO.Lines {
		poLines[l.LineID] = l
	}

	// Per-line evaluation.
	accs := make([]*lineAcc, 0, len(inv.Lines))
	groupQty := map[string]float64{}
	groupFirst := map[string]int{}
	for i, l := range inv.Lines {
		res := &MatchLineResult{
			InvoiceLineID: l.InvoiceLineID, LineNumber: l.LineNumber, InvoicedQuantity: l.Quantity,
			UnitPrice: l.UnitPrice, NetAmount: l.NetAmount, Categories: []string{},
		}
		out.Totals.InvoiceNet = round2(out.Totals.InvoiceNet + l.NetAmount)
		a := &lineAcc{res: res, cats: map[string]bool{}}
		accs = append(accs, a)
		if l.POLineReference != nil && *l.POLineReference != "" {
			res.POLineID = *l.POLineReference
			groupQty[res.POLineID] += l.Quantity
			if _, seen := groupFirst[res.POLineID]; !seen {
				groupFirst[res.POLineID] = i
			}
		}
	}

	lineExc := func(a *lineAcc, x MatchExceptionRecord) {
		x.InvoiceLineID, x.POLineID = a.res.InvoiceLineID, a.res.POLineID
		a.cats[x.Category] = true
		add(x)
	}

	if !headerIncomplete {
		for i, a := range accs {
			r := a.res
			if r.POLineID == "" {
				lineExc(a, MatchExceptionRecord{Category: ExcUnmappedLine, Class: ClassIncomplete, Waivable: false,
					Detail: "the invoice line names no purchase-order line, so there is nothing to match it against"})
				continue
			}
			po, ok := poLines[r.POLineID]
			if !ok {
				lineExc(a, MatchExceptionRecord{Category: ExcUnknownPOLine, Class: ClassIncomplete, Waivable: false,
					Detail: "the invoice line names a purchase-order line that the PO does not have"})
				continue
			}
			r.OrderedQuantity, r.POUnitPrice = po.Quantity, po.UnitPrice
			r.PriorInvoiced = e.PriorInvoiced[r.POLineID]

			// Price: unit price vs the PO's.
			var pct float64
			switch {
			case po.UnitPrice > 0:
				pct = math.Abs(r.UnitPrice-po.UnitPrice) / po.UnitPrice * 100
			case math.Abs(r.UnitPrice) > eps:
				pct = 100
			}
			r.PriceVariancePct = round4(pct)
			priceFlag := false
			switch {
			case pct < 1e-9:
			case pct <= e.Policy.PriceTolerancePct+1e-9:
				a.tol = true
			default:
				priceFlag = true
				lineExc(a, MatchExceptionRecord{Category: ExcPriceVariance, Class: ClassVariance, Waivable: true,
					Expected: po.UnitPrice, Actual: r.UnitPrice, Difference: round4(r.UnitPrice - po.UnitPrice),
					Detail: fmt.Sprintf("unit price differs from the PO by %.4f%% (tolerance %.4f%%)", pct, e.Policy.PriceTolerancePct)})
			}

			// Amount: the line's net against quantity x PO price. A price already
			// flagged explains its own amount difference, so it is not counted twice.
			expectedNet := round2(r.InvoicedQuantity * po.UnitPrice)
			r.AmountVariance = round2(r.NetAmount - expectedNet)
			if !priceFlag && math.Abs(r.AmountVariance) > eps {
				allowance := math.Max(e.Policy.AmountToleranceAbs, e.Policy.PriceTolerancePct/100*math.Abs(expectedNet))
				if math.Abs(r.AmountVariance) <= allowance+eps {
					a.tol = true
				} else {
					lineExc(a, MatchExceptionRecord{Category: ExcAmountVariance, Class: ClassVariance, Waivable: true,
						Expected: expectedNet, Actual: r.NetAmount, Difference: r.AmountVariance,
						Detail: "line amount differs from invoiced quantity x PO unit price beyond the policy allowance"})
				}
			}

			// Quantities are judged once per PO line (cumulative), on its first invoice line.
			if groupFirst[r.POLineID] != i {
				continue
			}
			total := r.PriorInvoiced + groupQty[r.POLineID]
			qtyTol := e.Policy.QtyTolerancePct / 100
			switch {
			case total > po.Quantity*(1+qtyTol)+eps:
				lineExc(a, MatchExceptionRecord{Category: ExcQtyOverOrder, Class: ClassVariance, Waivable: true,
					Expected: po.Quantity, Actual: round4(total), Difference: round4(total - po.Quantity),
					Detail: fmt.Sprintf("cumulative invoiced quantity %.4f exceeds the ordered quantity %.4f", total, po.Quantity)})
			case total > po.Quantity+eps:
				a.tol = true
			}
			if threeWay {
				rec, has := e.Receipts.Received[r.POLineID]
				if !has {
					lineExc(a, MatchExceptionRecord{Category: ExcMissingReceipt, Class: ClassIncomplete, Waivable: false,
						Detail: "no receipt record exists for this PO line; absence of a receipt is not a zero difference"})
					continue
				}
				r.ReceivedQuantity = &rec
				switch {
				case total > rec*(1+qtyTol)+eps:
					lineExc(a, MatchExceptionRecord{Category: ExcQtyOverReceipt, Class: ClassVariance, Waivable: true,
						Expected: rec, Actual: round4(total), Difference: round4(total - rec),
						Detail: fmt.Sprintf("cumulative invoiced quantity %.4f exceeds the received quantity %.4f", total, rec)})
				case total > rec+eps:
					a.tol = true
				}
			}
		}
	}

	// Each invoice line shares its PO line's group exceptions; give the per-line
	// result and the totals.
	for _, a := range accs {
		r := a.res
		for c := range a.cats {
			r.Categories = append(r.Categories, c)
		}
		sort.Strings(r.Categories)
		switch {
		case headerIncomplete:
			r.Result = MatchIncomplete
		case lineHasClass(out.Exceptions, a, ClassIncomplete):
			r.Result = MatchIncomplete
		case lineHasClass(out.Exceptions, a, ClassVariance):
			r.Result = MatchException
		case a.tol:
			r.Result = MatchWithinTolerance
		default:
			r.Result = MatchMatched
		}
		switch r.Result {
		case MatchMatched:
			out.Totals.MatchedLines++
		case MatchWithinTolerance:
			out.Totals.ToleranceLines++
		case MatchException:
			out.Totals.ExceptionLines++
		case MatchIncomplete:
			out.Totals.IncompleteLines++
		}
		out.Lines = append(out.Lines, *r)
	}

	// Run result. Missing evidence outranks a variance: a run that cannot see
	// everything cannot be certified, whatever it did see.
	hasIncomplete, hasVariance, anyTol := false, false, false
	for _, x := range out.Exceptions {
		if x.Class == ClassIncomplete {
			hasIncomplete = true
		} else {
			hasVariance = true
		}
	}
	for _, a := range accs {
		anyTol = anyTol || a.tol
	}
	switch {
	case hasIncomplete:
		out.Result = MatchIncomplete
	case hasVariance:
		out.Result = MatchException
	case anyTol:
		out.Result = MatchWithinTolerance
	default:
		out.Result = MatchMatched
	}

	sort.SliceStable(out.Exceptions, func(i, j int) bool {
		if out.Exceptions[i].InvoiceLineID != out.Exceptions[j].InvoiceLineID {
			return out.Exceptions[i].InvoiceLineID < out.Exceptions[j].InvoiceLineID
		}
		return out.Exceptions[i].Category < out.Exceptions[j].Category
	})
	out.Totals.Exceptions = len(out.Exceptions)
	return out
}

func lineHasClass(excs []MatchExceptionRecord, a *lineAcc, class string) bool {
	for _, x := range excs {
		if x.InvoiceLineID == a.res.InvoiceLineID && x.InvoiceLineID != "" && x.Class == class {
			return true
		}
	}
	return false
}

// ── invoice-side rules ──────────────────────────────────────────────────────

// RequiresMatch reports whether this document must carry a cleared AP-06 match
// before it can be approved: an invoice (not a credit/debit note) that names a
// purchase order. Deriving it here, rather than trusting the stored match_required
// flag alone, closes the gap for invoices validated before AP-06 existed.
func (v *VendorInvoice) RequiresMatch() bool {
	if v.MatchRequired {
		return true
	}
	docType := v.DocumentType
	if docType == "" {
		docType = DocInvoice
	}
	return docType == DocInvoice && v.PurchaseOrderID != nil && *v.PurchaseOrderID != ""
}

// ── errors and codes ────────────────────────────────────────────────────────

const (
	CodeMatchNotApplicable = "MATCH_NOT_APPLICABLE"
	CodeMatchRunSuperseded = "MATCH_RUN_SUPERSEDED"
	CodeNotWaivable        = "MATCH_EXCEPTION_NOT_WAIVABLE"
	CodeInvalidState       = "INVALID_STATE"
)

var (
	ErrMatchNotApplicable     = errorString("matching applies only to an invoice that names a purchase order")
	ErrMatchInvoiceFinal      = errorString("the invoice is approved or closed; its match can no longer be re-performed or superseded")
	ErrMatchInvoiceState      = errorString("the invoice is quarantined or rejected and cannot be matched")
	ErrMatchRunNotFound       = errorString("no match run exists for this invoice")
	ErrMatchRunSuperseded     = errorString("this match run has been superseded; act on the current run")
	ErrMatchExceptionNotFound = errorString("match exception not found")
	ErrExceptionNotWaivable   = errorString("this exception reports missing or inconsistent evidence; it cannot be waived — fix the evidence and re-perform the match")
	ErrExceptionTransition    = errorString("the exception is not in a state that allows this action")
	ErrMatchSelfWaiver        = errorString("segregation of duties: the principal who ran the match or created the invoice cannot approve its variance")
	ErrMatchPolicyNotFound    = errorString("match policy version not found")
	ErrMatchPolicyInvalid     = errorString("match policy is invalid")
	ErrMatchPolicySoD         = errorString("segregation of duties: the policy editor cannot certify results under their own policy version")
)
