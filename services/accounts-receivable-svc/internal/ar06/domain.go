// Package ar06 implements AR-06 Credit / Debit Note (spec ZS-SVC-C-001 section 9): correction documents that link to an
// ISSUED source invoice and never edit it (invariants 1 and 11). It owns the note, its lines, the legal number, the
// correction reason, the tax-result reference, the immutable issued representation and the correction chain. Tax comes only
// from tax-determination-svc, the receivable consequence from AR-07 (adjustment of the source open item for a credit note, a
// new open item for a debit note), the accounting consequence from ACC-04 via the posting queue; none of that is computed
// or written here.
package ar06

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Authorization actions (spec permissions creditnote/debitnote.read/create/manage -> MANAGE; correction.approve/issue).
// Reads are tenant-scoped only (settled AR read posture).
const (
	ActionCorrectionManage  = "AR06_CORRECTION_MANAGE"  // create / submit / cancel an unissued correction
	ActionCorrectionApprove = "AR06_CORRECTION_APPROVE" // approve a correction
	ActionCorrectionIssue   = "AR06_CORRECTION_ISSUE"   // issue a credit or debit note
	ActionCorrectionLink    = "AR06_CORRECTION_LINK"    // link a replacement invoice to an issued correction
)

// GL mapping keys of the correction accounting events. AR_RECEIVABLE_STANDARD, AR_REVENUE_STANDARD and AR_TAX_OUTPUT are
// registered in general-ledger-svc; AR_CREDIT_NOTE_REVENUE is seeded per tenant.
const (
	MappingReceivable        = "AR_RECEIVABLE_STANDARD"
	MappingRevenue           = "AR_REVENUE_STANDARD"
	MappingTaxOutput         = "AR_TAX_OUTPUT"
	MappingCreditNoteRevenue = "AR_CREDIT_NOTE_REVENUE"
)

// Thresholds (compared with the note's own amount in its own currency; no FX conversion is performed anywhere in AR).
var (
	// MaterialCorrectionThreshold: a note whose gross is strictly above it is material: the principal who issues it must not
	// be the one who approved it. The approver is, at every value, neither creator nor submitter.
	MaterialCorrectionThreshold = kit.MustAmount("10000")
	// EvidenceRequiredThreshold: a credit note above it must carry an evidence_id (EVIDENCE_REQUIRED otherwise). Checked on
	// the net at draft creation and on the gross at submission.
	EvidenceRequiredThreshold = kit.MustAmount("1000")
)

// Codes local to AR-06 beyond the shared spec section 14 classes.
const (
	CodeCorrectionAlreadyIssued = "CORRECTION_ALREADY_ISSUED"
	CodeCorrectionDuplicate     = "CORRECTION_DUPLICATE"
	CodeCorrectionSourceInvalid = "CORRECTION_SOURCE_MISMATCH"

	AggregateType = "Correction"
	maxLines      = 500
	maxText       = 4000

	// SchemaIssuedNote versions the frozen issued representation.
	SchemaIssuedNote = "zoiko.ar06.issued-note/1"

	SeriesCredit = "CN"
	SeriesDebit  = "DN"
)

// Event types (one dotted lowercase scheme: ar.<aggregate>.<verb>).
const (
	EventDraftCreated        = "ar.correction.draft_created"
	EventSubmitted           = "ar.correction.submitted"
	EventApproved            = "ar.correction.approved"
	EventApprovalInvalidated = "ar.correction.approval_invalidated"
	EventCancelled           = "ar.correction.cancelled"
	EventCreditNoteIssued    = "ar.credit_note.issued"
	EventDebitNoteIssued     = "ar.debit_note.issued"
	EventInvoiceCorrected    = "ar.customer_invoice.corrected"
	EventApplicationRequest  = "ar.correction.application_requested"
	EventReplacementLinked   = "ar.correction.replacement_linked"
)

// Kind is the note type.
type Kind string

const (
	KindCredit Kind = "CREDIT_NOTE"
	KindDebit  Kind = "DEBIT_NOTE"
)

func (k Kind) valid() bool { return k == KindCredit || k == KindDebit }

// Series is the numbering series of the kind.
func (k Kind) Series() string {
	if k == KindDebit {
		return SeriesDebit
	}
	return SeriesCredit
}

// State is the correction lifecycle: Draft -> PendingApproval -> Approved -> Issued -> Applied -> Settled; Cancelled is a
// branch available only before issue. APPLIED/SETTLED are legal forward moves of an issued note (the DB trigger allows them);
// the application result itself is the orthogonal application_state dimension, set in the issue transaction.
type State string

const (
	StateDraft           State = "DRAFT"
	StatePendingApproval State = "PENDING_APPROVAL"
	StateApproved        State = "APPROVED"
	StateIssued          State = "ISSUED"
	StateApplied         State = "APPLIED"
	StateSettled         State = "SETTLED"
	StateCancelled       State = "CANCELLED"
)

// IsIssued reports whether the note is (or was) an authoritative issued document.
func (s State) IsIssued() bool { return s == StateIssued || s == StateApplied || s == StateSettled }

// Transition names a lifecycle move.
type Transition string

const (
	TrSubmit     Transition = "SUBMIT"
	TrApprove    Transition = "APPROVE"
	TrIssue      Transition = "ISSUE"
	TrInvalidate Transition = "INVALIDATE_APPROVAL"
	TrCancel     Transition = "CANCEL_UNISSUED"
)

var transitions = map[Transition]map[State]State{
	TrSubmit:     {StateDraft: StatePendingApproval},
	TrApprove:    {StatePendingApproval: StateApproved},
	TrIssue:      {StateApproved: StateIssued},
	TrInvalidate: {StateApproved: StateDraft, StatePendingApproval: StateDraft},
	TrCancel:     {StateDraft: StateCancelled, StatePendingApproval: StateCancelled, StateApproved: StateCancelled},
}

// Next applies a transition or returns 409 INVALID_STATE_TRANSITION. An issued note refuses every content transition with
// the immutability class instead (invariant 1).
func Next(cur State, t Transition) (State, error) {
	if to, ok := transitions[t][cur]; ok {
		return to, nil
	}
	if cur.IsIssued() {
		if t == TrIssue {
			return "", kit.Conflict(CodeCorrectionAlreadyIssued, "the note is already issued: it keeps its number and is never issued twice")
		}
		return "", kit.Conflict(kit.CodeImmutable, "an issued credit/debit note is never edited in place")
	}
	return "", kit.Conflict(kit.CodeInvalidTransition, fmt.Sprintf("%s is not allowed from state %s", t, cur))
}

// Application and e-invoice dimensions.
const (
	AppNotApplied = "NOT_APPLIED"
	AppApplied    = "APPLIED"

	EInvNotRequired = "NOT_REQUIRED"
	EInvRequired    = "REQUIRED"
)

var einvoiceStates = map[string]bool{"NOT_REQUIRED": true, "REQUIRED": true, "PREPARED": true, "SUBMITTED": true, "PENDING_UNKNOWN": true, "ACCEPTED": true, "REJECTED": true}

// ── content ──────────────────────────────────────────────────────────────────

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	refRe      = regexp.MustCompile(`^[A-Za-z0-9_.:/#\-]{1,255}$`)
	codeRe     = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
	clientRe   = regexp.MustCompile(`^[A-Za-z0-9_.:/#\-]{1,128}$`)
	reasonRe   = regexp.MustCompile(`^[A-Za-z0-9_.:/#\- ]{1,128}$`)

	supplyKinds = map[string]bool{"GOODS": true, "SERVICES": true, "DIGITAL_SERVICES": true}
	maxLineNet  = kit.MustAmount("1000000000000")
)

// LineInput is a requested note line. The net is computed by the server; there is deliberately NO tax field: the editor
// can neither supply nor override tax (invariant 3).
type LineInput struct {
	SourceLineNo          int        `json:"source_line_no,omitempty"`
	Description           string     `json:"description"`
	Quantity              kit.Amount `json:"quantity"`
	UnitPrice             kit.Amount `json:"unit_price"`
	TaxCategory           string     `json:"tax_category,omitempty"`
	ProductClassification string     `json:"product_classification,omitempty"`
	SupplyKind            string     `json:"supply_kind,omitempty"`
}

// Line is a stored note line; Tax is the share of its tax group's determination (derived).
type Line struct {
	LineNo                int        `json:"line_no"`
	SourceLineNo          int        `json:"source_line_no,omitempty"`
	Description           string     `json:"description"`
	Quantity              kit.Amount `json:"quantity"`
	UnitPrice             kit.Amount `json:"unit_price"`
	Net                   kit.Amount `json:"net_amount"`
	Tax                   kit.Amount `json:"tax_amount"`
	TaxCategory           string     `json:"tax_category,omitempty"`
	ProductClassification string     `json:"product_classification,omitempty"`
	SupplyKind            string     `json:"supply_kind,omitempty"`
}

// BuildLines validates requested lines against the ISSUED source invoice's lines and computes their nets. A credit note's
// line must name the source line it corrects and cannot exceed that line's net; a debit note's source_line_no is optional.
func BuildLines(kind Kind, currency string, in []LineInput, src []kit.IssuedInvoiceLine) ([]Line, error) {
	if len(in) == 0 {
		return nil, kit.Invalid("a correction needs at least one line")
	}
	if len(in) > maxLines {
		return nil, kit.Invalid(fmt.Sprintf("a correction has at most %d lines", maxLines))
	}
	byNo := map[int]kit.IssuedInvoiceLine{}
	for _, l := range src {
		byNo[l.LineNumber] = l
	}
	out := make([]Line, 0, len(in))
	for i, l := range in {
		n := i + 1
		desc := strings.TrimSpace(l.Description)
		switch {
		case desc == "" || utf8.RuneCountInString(desc) > 500:
			return nil, kit.Invalid(fmt.Sprintf("line %d: description is required (max 500 characters)", n))
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
		case l.SourceLineNo < 0:
			return nil, kit.Invalid(fmt.Sprintf("line %d: source_line_no must be a source invoice line number", n))
		case kind == KindCredit && l.SourceLineNo == 0:
			return nil, kit.Invalid(fmt.Sprintf("line %d: a credit note line must name the source invoice line it corrects (source_line_no)", n))
		}
		net := ar04.LineNet(l.Quantity, l.UnitPrice, currency)
		if net > maxLineNet {
			return nil, kit.Invalid(fmt.Sprintf("line %d: net amount out of range", n))
		}
		ln := Line{LineNo: n, SourceLineNo: l.SourceLineNo, Description: desc, Quantity: l.Quantity, UnitPrice: l.UnitPrice, Net: net,
			TaxCategory: l.TaxCategory, ProductClassification: l.ProductClassification, SupplyKind: l.SupplyKind}
		if l.SourceLineNo > 0 {
			sl, ok := byNo[l.SourceLineNo]
			if !ok {
				return nil, kit.Err(422, CodeCorrectionSourceInvalid, fmt.Sprintf("line %d: the source invoice has no line %d", n, l.SourceLineNo))
			}
			if kind == KindCredit && net > sl.Net {
				return nil, kit.Unprocessable(kit.CodeCorrectionExceedsEligible, fmt.Sprintf("line %d: the credit (%s) exceeds the net of source line %d (%s)", n, net, l.SourceLineNo, sl.Net))
			}
			if ln.TaxCategory == "" {
				ln.TaxCategory = sl.TaxCode
			}
		}
		out = append(out, ln)
	}
	return out, nil
}

// Content is the editable content of a draft (everything the approval is bound to, apart from the server-derived tax result).
type Content struct {
	ClientReference string          `json:"client_reference,omitempty"`
	ReasonCode      string          `json:"reason_code"`
	ReasonText      string          `json:"reason,omitempty"`
	EvidenceID      string          `json:"evidence_id,omitempty"`
	SupplyDate      string          `json:"supply_date"`
	Tax             ar04.TaxContext `json:"tax_context"`
}

// Validate checks everything that does not need a database.
func (c Content) Validate() error {
	if strings.TrimSpace(c.ReasonCode) == "" {
		return kit.Err(400, kit.CodeReasonRequired, "reason_code is required")
	}
	switch {
	case !reasonRe.MatchString(c.ReasonCode):
		return kit.Invalid("reason_code must be 1-128 characters of letters, digits, space and _ . : / # -")
	case c.ClientReference != "" && !clientRe.MatchString(c.ClientReference):
		return kit.Invalid("client_reference must be 1-128 characters of letters, digits and _ . : / # -")
	case utf8.RuneCountInString(c.ReasonText) > maxText:
		return kit.Invalid(fmt.Sprintf("reason is limited to %d characters", maxText))
	case c.EvidenceID != "" && !refRe.MatchString(c.EvidenceID):
		return kit.Invalid("evidence_id has invalid characters")
	}
	if _, err := time.Parse("2006-01-02", c.SupplyDate); err != nil {
		return kit.Invalid("supply_date is required (YYYY-MM-DD)")
	}
	return c.Tax.Validate()
}

// EvidenceNeeded reports whether a credit note of this amount needs an evidence_id.
func EvidenceNeeded(kind Kind, amount kit.Amount) bool {
	return kind == KindCredit && amount > EvidenceRequiredThreshold
}

// IsMaterial reports whether the gross is above MaterialCorrectionThreshold.
func IsMaterial(gross kit.Amount) bool { return gross > MaterialCorrectionThreshold }

func mustUUID(field, v string) error {
	if _, err := uuid.Parse(v); err != nil {
		return kit.Invalid(field + " must be a UUID")
	}
	return nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// ── eligibility ──────────────────────────────────────────────────────────────

// Eligibility is the correctable position of one source invoice, derived exactly from the source's issued amounts and the
// ISSUED credit notes recorded against it.
type Eligibility struct {
	Currency       string     `json:"currency"`
	SourceNet      kit.Amount `json:"source_net"`
	SourceTax      kit.Amount `json:"source_tax"`
	SourceGross    kit.Amount `json:"source_gross"`
	CreditedNet    kit.Amount `json:"credited_net"`
	CreditedGross  kit.Amount `json:"credited_gross"`
	RemainingNet   kit.Amount `json:"remaining_correctable_net"`
	RemainingGross kit.Amount `json:"remaining_correctable_gross"`
}

// EligibilityOf computes the position.
func EligibilityOf(src *kit.IssuedInvoice, creditedNet, creditedGross kit.Amount) Eligibility {
	return Eligibility{Currency: src.Currency, SourceNet: src.Net, SourceTax: src.Tax, SourceGross: src.Gross, CreditedNet: creditedNet, CreditedGross: creditedGross,
		RemainingNet: src.Net.Sub(creditedNet), RemainingGross: src.Gross.Sub(creditedGross)}
}

// Check refuses a credit above the remaining correctable amount (net always; gross once the tax result is known).
func (e Eligibility) Check(net, gross kit.Amount, haveGross bool) error {
	if net > e.RemainingNet || (haveGross && gross > e.RemainingGross) {
		return kit.Unprocessable(kit.CodeCorrectionExceedsEligible, fmt.Sprintf(
			"the credit (net %s, gross %s) exceeds the remaining correctable amount of the source invoice (net %s, gross %s)",
			net, gross, e.RemainingNet, e.RemainingGross))
	}
	return nil
}

// ── tax (shared pure logic of AR-04, the only place tax figures are interpreted) ─

func toAR04(lines []Line) []ar04.Line {
	out := make([]ar04.Line, len(lines))
	for i, l := range lines {
		out[i] = ar04.Line{LineNo: l.LineNo, Description: l.Description, Quantity: l.Quantity, UnitPrice: l.UnitPrice, Net: l.Net, Tax: l.Tax,
			TaxCategory: l.TaxCategory, ProductClassification: l.ProductClassification, SupplyKind: l.SupplyKind}
	}
	return out
}

// ValidateForTax checks the inputs tax-determination-svc requires before it is called.
func ValidateForTax(c Content, lines []Line) error {
	return ar04.ValidateForTax(ar04.Content{Tax: c.Tax}, toAR04(lines))
}

// BuildGroups groups lines by (tax_category, product_classification, supply_kind).
func BuildGroups(lines []Line) []ar04.GroupSpec { return ar04.BuildGroups(toAR04(lines)) }

// ApplyTax allocates each group's tax across its lines (exactly) and returns the lines with Tax set.
func ApplyTax(lines []Line, groups []ar04.TaxGroup, currency string) ([]Line, error) {
	tmp := toAR04(lines)
	if err := ar04.ApplyTax(tmp, groups, currency); err != nil {
		return nil, err
	}
	out := make([]Line, len(lines))
	copy(out, lines)
	for i := range out {
		out[i].Tax = tmp[i].Tax
	}
	return out, nil
}

// TotalsOf proves lines and groups agree and returns the totals.
func TotalsOf(lines []Line, groups []ar04.TaxGroup, currency string) (ar04.Totals, error) {
	return ar04.TotalsOf(toAR04(lines), groups, currency)
}

// ── header / read model ──────────────────────────────────────────────────────

// Header is the stable correction.
type Header struct {
	CorrectionID        string     `json:"correction_id"`
	TenantID            string     `json:"tenant_id"`
	LegalEntityID       string     `json:"legal_entity_id"`
	CustomerPartyID     string     `json:"customer_party_id"`
	Kind                Kind       `json:"kind"`
	SourceInvoiceID     string     `json:"source_invoice_id"`
	SourceInvoiceNumber string     `json:"source_invoice_number"`
	Currency            string     `json:"currency"`
	State               State      `json:"lifecycle_state"`
	ApplicationState    string     `json:"application_state"`
	EInvoiceState       string     `json:"einvoice_state"`
	AccountingState     string     `json:"accounting_state"`
	AccountingJournalID string     `json:"accounting_journal_id,omitempty"`
	AccountingError     string     `json:"accounting_error,omitempty"`
	Content             Content    `json:"content"`
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
	DocumentNumber      string     `json:"document_number,omitempty"`
	NumberingSeries     string     `json:"numbering_series,omitempty"`
	SequenceNo          int64      `json:"sequence_no,omitempty"`
	IssueDate           string     `json:"issue_date,omitempty"`
	DueDate             string     `json:"due_date,omitempty"`
	IssuedBy            string     `json:"issued_by,omitempty"`
	IssuedAt            *time.Time `json:"issued_at,omitempty"`
	IssuedContentHash   string     `json:"issued_content_hash,omitempty"`
	OpenItemID          string     `json:"open_item_id,omitempty"`
	Version             int        `json:"version"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`

	issuedCanonical string
}

// Correction is the read model: header, lines and tax result.
type Correction struct {
	Header
	Lines     []Line          `json:"lines"`
	TaxGroups []ar04.TaxGroup `json:"tax_groups"`
}

// ContentHash is the approval-binding hash of everything an approver relies on: source linkage, reason, lines and the tax
// result. It deliberately excludes the legal number and issue date, allocated only at issue.
func ContentHash(h *Header, lines []Line, groups []ar04.TaxGroup) string {
	type hl struct {
		N  int    `json:"n"`
		S  int    `json:"s"`
		D  string `json:"d"`
		Q  string `json:"q"`
		U  string `json:"u"`
		L  string `json:"l"`
		X  string `json:"x"`
		C  string `json:"c"`
		K  string `json:"k"`
		SK string `json:"sk"`
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
		Kind     Kind   `json:"kind"`
		Source   string `json:"source"`
		Currency string `json:"currency"`
		Reason   string `json:"reason"`
		Text     string `json:"text"`
		Evidence string `json:"evidence"`
		Supply   string `json:"supply"`
		Ref      string `json:"ref"`
		Tax      ar04.TaxContext
		Lines    []hl `json:"lines"`
		Groups   []hg `json:"groups"`
	}
	c := h.Content
	d := doc{Entity: h.LegalEntityID, Customer: h.CustomerPartyID, Kind: h.Kind, Source: h.SourceInvoiceID, Currency: h.Currency, Reason: c.ReasonCode,
		Text: c.ReasonText, Evidence: c.EvidenceID, Supply: c.SupplyDate, Ref: c.ClientReference, Tax: c.Tax}
	for _, l := range lines {
		d.Lines = append(d.Lines, hl{l.LineNo, l.SourceLineNo, l.Description, l.Quantity.String(), l.UnitPrice.String(), l.Net.String(), l.Tax.String(),
			l.TaxCategory, l.ProductClassification, l.SupplyKind})
	}
	for _, g := range groups {
		d.Groups = append(d.Groups, hg{g.GroupNo, g.TaxCategory, g.ProductClassification, g.SupplyKind, g.DeterminationID, g.Provenance, g.Net.String(), g.Tax.String()})
	}
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── sealed issued representation ─────────────────────────────────────────────

type IssuedLine struct {
	LineNo                int        `json:"line_no"`
	SourceLineNo          int        `json:"source_line_no"`
	Description           string     `json:"description"`
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
	Schema              string           `json:"schema"`
	CorrectionID        string           `json:"correction_id"`
	TenantID            string           `json:"tenant_id"`
	LegalEntityID       string           `json:"legal_entity_id"`
	CustomerPartyID     string           `json:"customer_party_id"`
	Kind                Kind             `json:"kind"`
	DocumentNumber      string           `json:"document_number"`
	NumberingSeries     string           `json:"numbering_series"`
	SequenceNo          int64            `json:"sequence_no"`
	Currency            string           `json:"currency"`
	IssueDate           string           `json:"issue_date"`
	SupplyDate          string           `json:"supply_date"`
	DueDate             string           `json:"due_date"`
	SourceInvoiceID     string           `json:"source_invoice_id"`
	SourceInvoiceNumber string           `json:"source_invoice_number"`
	ReasonCode          string           `json:"reason_code"`
	Reason              string           `json:"reason"`
	EvidenceID          string           `json:"evidence_id"`
	TaxContext          ar04.TaxContext  `json:"tax_context"`
	Lines               []IssuedLine     `json:"lines"`
	TaxGroups           []IssuedTaxGroup `json:"tax_groups"`
	Net                 kit.Amount       `json:"net_total"`
	Tax                 kit.Amount       `json:"tax_total"`
	Gross               kit.Amount       `json:"gross_total"`
	ApprovedHash        string           `json:"approved_content_hash"`
	ApprovedBy          string           `json:"approved_by"`
	ApprovedAt          string           `json:"approved_at"`
	IssuedBy            string           `json:"issued_by"`
	IssuedAt            string           `json:"issued_at"`
	EInvoiceRequired    bool             `json:"einvoice_required"`
}

// IssueInput is everything the sealing needs, all server-derived.
type IssueInput struct {
	H                *Header
	Lines            []Line
	Groups           []ar04.TaxGroup
	Number           string
	Series           string
	Sequence         int64
	IssueDate        string
	DueDate          string
	IssuedBy         string
	IssuedAt         time.Time
	EInvoiceRequired bool
}

// BuildIssued produces the canonical issued-representation JSON and its sha256. The exact bytes are stored (TEXT) beside the hash.
func BuildIssued(in IssueInput) (canonical string, hash string, err error) {
	h := in.H
	d := IssuedDocument{Schema: SchemaIssuedNote, CorrectionID: h.CorrectionID, TenantID: h.TenantID, LegalEntityID: h.LegalEntityID, CustomerPartyID: h.CustomerPartyID,
		Kind: h.Kind, DocumentNumber: in.Number, NumberingSeries: in.Series, SequenceNo: in.Sequence, Currency: h.Currency, IssueDate: in.IssueDate,
		SupplyDate: h.Content.SupplyDate, DueDate: in.DueDate, SourceInvoiceID: h.SourceInvoiceID, SourceInvoiceNumber: h.SourceInvoiceNumber,
		ReasonCode: h.Content.ReasonCode, Reason: h.Content.ReasonText, EvidenceID: h.Content.EvidenceID, TaxContext: h.Content.Tax,
		Lines: []IssuedLine{}, TaxGroups: []IssuedTaxGroup{}, Net: h.NetTotal, Tax: h.TaxTotal, Gross: h.GrossTotal, ApprovedHash: h.ApprovedHash,
		ApprovedBy: h.ApprovedBy, IssuedBy: in.IssuedBy, IssuedAt: in.IssuedAt.UTC().Format(time.RFC3339Nano), EInvoiceRequired: in.EInvoiceRequired}
	if h.ApprovedAt != nil {
		d.ApprovedAt = h.ApprovedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, l := range in.Lines {
		d.Lines = append(d.Lines, IssuedLine{l.LineNo, l.SourceLineNo, l.Description, l.Quantity, l.UnitPrice, l.Net, l.Tax, l.TaxCategory, l.ProductClassification, l.SupplyKind})
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

// NumberingProfile is the per-entity format of the legal number (jurisdiction-controlled, effective-dated: invariant 18). When an
// entity has no profile row for the series, the default applies; that default is a documented pre-production placeholder.
type NumberingProfile struct {
	Series string
	Prefix string
	Width  int
}

// DefaultNumbering applies when a legal entity has no ar06_numbering_profiles row: CN-000001 / DN-000001.
func DefaultNumbering(k Kind) NumberingProfile {
	return NumberingProfile{Series: k.Series(), Prefix: k.Series() + "-", Width: 6}
}

// Format renders sequence n as prefix + zero-padded number. A number wider than the width is never truncated.
func (p NumberingProfile) Format(n int64) string {
	w := p.Width
	if w < 1 {
		w = 1
	}
	return fmt.Sprintf("%s%0*d", p.Prefix, w, n)
}

// Validate checks a profile row.
func (p NumberingProfile) Validate() error {
	if !regexp.MustCompile(`^[A-Z0-9_-]{1,16}$`).MatchString(p.Series) {
		return kit.Invalid("numbering series must be 1-16 characters of A-Z, 0-9, _ or -")
	}
	if p.Width < 1 || p.Width > 12 {
		return kit.Invalid("numbering width must be 1-12")
	}
	return nil
}

// ── invalidation ─────────────────────────────────────────────────────────────

// invalidation is returned out of the issue/approve flow when the approved basis no longer holds (tax result changed, stored
// content no longer matches its approval): the correction is then returned to DRAFT in its own transaction.
type invalidation struct {
	Reason string
	Detail map[string]any
}

func (i *invalidation) Error() string { return "approval invalidated: " + i.Reason }

func errApprovalInvalidated(msg string) *kit.Problem {
	return kit.Conflict(kit.CodeInvoiceApprovalInvalidated, msg)
}
