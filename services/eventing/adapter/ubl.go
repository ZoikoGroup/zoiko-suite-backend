package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// The e-invoice transformer (ZS-SVC-M-001 INT-09). The adapter receives a sealed, approved canonical invoice and
// renders it as a UBL 2.1 Invoice / CreditNote for a network profile. It never changes economics: if the canonical
// invoice is not internally consistent, or the rendered document does not carry exactly the canonical totals, the
// document is refused and nothing is sent. Invoice economics, tax determination and the document number belong to
// AR and TAX.

// Errors. Messages name the rule and the field, never a value: an invoice carries commercial data.
var (
	ErrInvoiceInvalid       = errors.New("the canonical invoice is not internally consistent")
	ErrRecipientInvalid     = errors.New("the recipient identifier is not valid for this network profile")
	ErrAttachmentProhibited = errors.New("attachments are not permitted by this network profile")
	ErrTotalMismatch        = errors.New("the transformed document does not carry the canonical totals")
)

// TransformUBL is the only transformer id a profile may name today.
const TransformUBL = "ubl-2.1"

// EInvoiceRules are the network profile's rules on the document and its parties.
type EInvoiceRules struct {
	// RecipientSchemes maps an endpoint scheme id (an ICD such as "0088") to the regular expression its
	// identifiers must match. A scheme that is not listed is not accepted: an ambiguous recipient blocks
	// before send.
	RecipientSchemes map[string]string `json:"recipient_schemes"`
	// AllowAttachments permits embedded attachments up to MaxAttachmentBytes in total; false prohibits them.
	AllowAttachments   bool     `json:"allow_attachments,omitempty"`
	MaxAttachmentBytes int      `json:"max_attachment_bytes,omitempty"`
	AllowedMimeTypes   []string `json:"allowed_mime_types,omitempty"`
	// DocumentTypes are the UNTDID 1001 codes the profile carries: 380 invoice, 381 credit note.
	DocumentTypes []string `json:"document_types,omitempty"`
	// CustomizationID / ProfileID are stamped on the document as the network profile identifiers.
	CustomizationID string `json:"customization_id,omitempty"`
	ProfileID       string `json:"profile_id,omitempty"`
}

func (r *EInvoiceRules) validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(f, a...)) }
	if r == nil {
		return bad("an EINVOICE profile needs its einvoice rules")
	}
	if len(r.RecipientSchemes) == 0 {
		return bad("einvoice.recipient_schemes is required: an unlisted recipient scheme is refused")
	}
	for k, v := range r.RecipientSchemes {
		if !regexp.MustCompile(`^[0-9A-Za-z]{1,10}$`).MatchString(k) {
			return bad("a recipient scheme id is malformed")
		}
		if _, err := regexp.Compile(v); err != nil || v == "" {
			return bad("a recipient identifier pattern does not compile")
		}
	}
	if r.AllowAttachments && (r.MaxAttachmentBytes <= 0 || r.MaxAttachmentBytes > 10<<20) {
		return bad("allow_attachments needs max_attachment_bytes between 1 and 10 MiB")
	}
	for _, t := range r.DocumentTypes {
		if t != "380" && t != "381" {
			return bad("document_types may list 380 and 381")
		}
	}
	return nil
}

// Invoice is the canonical document the adapter receives (amounts are exact decimal strings).
type Invoice struct {
	InvoiceNo string `json:"invoice_no"`
	Type      string `json:"type"` // 380 invoice, 381 credit note
	IssueDate string `json:"issue_date"`
	DueDate   string `json:"due_date,omitempty"`
	Currency  string `json:"currency"`
	Note      string `json:"note,omitempty"`
	Supplier  Party  `json:"supplier"`
	Customer  Party  `json:"customer"`
	Lines     []Line `json:"lines"`
	Totals    Totals `json:"totals"`
	// Attachments are embedded documents (e.g. a PDF rendering).
	Attachments []Attachment `json:"attachments,omitempty"`
}

type Party struct {
	Name     string   `json:"name"`
	VATID    string   `json:"vat_id,omitempty"`
	Endpoint Endpoint `json:"endpoint"`
}

type Endpoint struct {
	Scheme string `json:"scheme"`
	ID     string `json:"id"`
}

type Line struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Quantity    string `json:"quantity"`
	UnitPrice   string `json:"unit_price"`
	TaxRate     string `json:"tax_rate"` // percent, e.g. "20.00"
	LineTotal   string `json:"line_total"`
}

type Totals struct {
	Net   string `json:"net"`
	Tax   string `json:"tax"`
	Gross string `json:"gross"`
}

type Attachment struct {
	Filename      string `json:"filename"`
	Mime          string `json:"mime"`
	ContentBase64 string `json:"content_base64"`
}

func rat(s string) (*big.Rat, bool) {
	if !decimalRe.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// roundHalfUp rounds r to `places` decimals, half away from zero, as an exact rational.
func roundHalfUp(r *big.Rat, places int) *big.Rat {
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil))
	x := new(big.Rat).Mul(r, scale)
	half := big.NewRat(1, 2)
	if x.Sign() < 0 {
		x.Sub(x, half)
	} else {
		x.Add(x, half)
	}
	n := new(big.Int).Quo(x.Num(), x.Denom()) // truncates toward zero
	return new(big.Rat).Quo(new(big.Rat).SetInt(n), scale)
}

func money(r *big.Rat, places int) string { return roundHalfUp(r, places).FloatString(places) }

// ParseInvoice decodes the canonical document strictly (unknown fields are refused).
func ParseInvoice(doc []byte) (*Invoice, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var inv Invoice
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("%w: document shape", ErrInvoiceInvalid)
	}
	return &inv, nil
}

var (
	dateRe      = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])$`)
	currencyRe2 = regexp.MustCompile(`^[A-Z]{3}$`)
)

// CheckEconomics proves the canonical invoice is internally consistent: quantity x unit price = line total, the
// lines sum to the net total, the per-line tax at each rate sums to the tax total, and net + tax = gross. It
// recomputes and compares; it never repairs. A failure means the AR/TAX source is wrong, and that is theirs to fix.
func (inv *Invoice) CheckEconomics() error {
	bad := func(m string) error { return fmt.Errorf("%w: %s", ErrInvoiceInvalid, m) }
	if inv.InvoiceNo == "" || len(inv.InvoiceNo) > 64 {
		return bad("invoice_no is required")
	}
	if inv.Type != "380" && inv.Type != "381" {
		return bad("type must be 380 or 381")
	}
	if !dateRe.MatchString(inv.IssueDate) || (inv.DueDate != "" && !dateRe.MatchString(inv.DueDate)) {
		return bad("dates must be YYYY-MM-DD")
	}
	if !currencyRe2.MatchString(inv.Currency) {
		return bad("currency must be an ISO 4217 code")
	}
	if len(inv.Lines) == 0 || len(inv.Lines) > 999 {
		return bad("between 1 and 999 lines are required")
	}
	places := minorExponent(inv.Currency)
	net, tax := new(big.Rat), new(big.Rat)
	seen := map[string]bool{}
	for i, l := range inv.Lines {
		if l.ID == "" || seen[l.ID] {
			return bad(fmt.Sprintf("lines[%d].id is missing or repeated", i))
		}
		seen[l.ID] = true
		q, ok1 := rat(l.Quantity)
		p, ok2 := rat(l.UnitPrice)
		rate, ok3 := rat(l.TaxRate)
		lt, ok4 := rat(l.LineTotal)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return bad(fmt.Sprintf("lines[%d] has a malformed number", i))
		}
		if rate.Sign() < 0 || rate.Cmp(big.NewRat(100, 1)) > 0 {
			return bad(fmt.Sprintf("lines[%d].tax_rate must be 0-100", i))
		}
		if roundHalfUp(new(big.Rat).Mul(q, p), places).Cmp(roundHalfUp(lt, places)) != 0 {
			return bad(fmt.Sprintf("lines[%d]: quantity x unit_price does not equal line_total", i))
		}
		net.Add(net, roundHalfUp(lt, places))
		lineTax := new(big.Rat).Mul(roundHalfUp(lt, places), new(big.Rat).Quo(rate, big.NewRat(100, 1)))
		tax.Add(tax, roundHalfUp(lineTax, places))
	}
	tn, ok1 := rat(inv.Totals.Net)
	tt, ok2 := rat(inv.Totals.Tax)
	tg, ok3 := rat(inv.Totals.Gross)
	if !ok1 || !ok2 || !ok3 {
		return bad("totals has a malformed number")
	}
	if net.Cmp(roundHalfUp(tn, places)) != 0 {
		return bad("the lines do not sum to totals.net")
	}
	if tax.Cmp(roundHalfUp(tt, places)) != 0 {
		return bad("the line taxes do not sum to totals.tax")
	}
	if new(big.Rat).Add(net, tax).Cmp(roundHalfUp(tg, places)) != 0 {
		return bad("totals.net + totals.tax does not equal totals.gross")
	}
	return nil
}

// CheckRecipient validates both parties' endpoints against the network profile. An unlisted scheme, or an
// identifier that does not match its scheme, blocks before send.
func (inv *Invoice) CheckRecipient(r *EInvoiceRules) error {
	for who, p := range map[string]Party{"supplier": inv.Supplier, "customer": inv.Customer} {
		if p.Name == "" {
			return fmt.Errorf("%w: %s.name is required", ErrInvoiceInvalid, who)
		}
		pat, ok := r.RecipientSchemes[p.Endpoint.Scheme]
		if !ok {
			return fmt.Errorf("%w: %s.endpoint.scheme is not accepted by this profile", ErrRecipientInvalid, who)
		}
		if !regexp.MustCompile(pat).MatchString(p.Endpoint.ID) {
			return fmt.Errorf("%w: %s.endpoint.id does not match its scheme", ErrRecipientInvalid, who)
		}
	}
	if len(r.DocumentTypes) > 0 {
		ok := false
		for _, t := range r.DocumentTypes {
			ok = ok || t == inv.Type
		}
		if !ok {
			return fmt.Errorf("%w: document type %s is not carried by this profile", ErrRecipientInvalid, inv.Type)
		}
	}
	return nil
}

// CheckAttachments enforces the profile's attachment policy.
func (inv *Invoice) CheckAttachments(r *EInvoiceRules) error {
	if len(inv.Attachments) == 0 {
		return nil
	}
	if !r.AllowAttachments {
		return ErrAttachmentProhibited
	}
	total := 0
	for i, a := range inv.Attachments {
		raw, err := base64.StdEncoding.DecodeString(a.ContentBase64)
		if err != nil || len(raw) == 0 || a.Filename == "" || strings.ContainsAny(a.Filename, "/\\") {
			return fmt.Errorf("%w: attachments[%d] is malformed", ErrInvoiceInvalid, i)
		}
		total += len(raw)
		if len(r.AllowedMimeTypes) > 0 {
			ok := false
			for _, m := range r.AllowedMimeTypes {
				ok = ok || m == a.Mime
			}
			if !ok {
				return fmt.Errorf("%w: attachments[%d] has a type this profile does not carry", ErrAttachmentProhibited, i)
			}
		}
	}
	if total > r.MaxAttachmentBytes {
		return fmt.Errorf("%w: attachments exceed the profile's size limit", ErrAttachmentProhibited)
	}
	return nil
}

// ── UBL rendering ───────────────────────────────────────────────────────────

const (
	nsInvoice    = "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
	nsCreditNote = "urn:oasis:names:specification:ubl:schema:xsd:CreditNote-2"
	nsCAC        = "urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"
	nsCBC        = "urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
)

type xmlAmount struct {
	CurrencyID string `xml:"currencyID,attr"`
	Value      string `xml:",chardata"`
}
type xmlEndpoint struct {
	SchemeID string `xml:"schemeID,attr"`
	Value    string `xml:",chardata"`
}
type xmlParty struct {
	Endpoint xmlEndpoint `xml:"cbc:EndpointID"`
	Name     string      `xml:"cac:PartyName>cbc:Name"`
	TaxID    string      `xml:"cac:PartyTaxScheme>cbc:CompanyID,omitempty"`
}
type xmlTaxCategory struct {
	Percent string `xml:"cbc:Percent"`
	Scheme  string `xml:"cac:TaxScheme>cbc:ID"`
}
type xmlLine struct {
	ID       string `xml:"cbc:ID"`
	Quantity struct {
		UnitCode string `xml:"unitCode,attr"`
		Value    string `xml:",chardata"`
	} `xml:"cbc:InvoicedQuantity"`
	LineExtension xmlAmount `xml:"cbc:LineExtensionAmount"`
	Item          struct {
		Description string         `xml:"cbc:Description"`
		Category    xmlTaxCategory `xml:"cac:ClassifiedTaxCategory"`
	} `xml:"cac:Item"`
	Price xmlAmount `xml:"cac:Price>cbc:PriceAmount"`
}
type xmlAttachment struct {
	Filename string `xml:"cbc:ID"`
	Embedded struct {
		MimeCode string `xml:"mimeCode,attr"`
		Filename string `xml:"filename,attr"`
		Value    string `xml:",chardata"`
	} `xml:"cac:Attachment>cbc:EmbeddedDocumentBinaryObject"`
}
type xmlInvoice struct {
	XMLName       xml.Name
	NSCAC         string          `xml:"xmlns:cac,attr"`
	NSCBC         string          `xml:"xmlns:cbc,attr"`
	Customization string          `xml:"cbc:CustomizationID,omitempty"`
	Profile       string          `xml:"cbc:ProfileID,omitempty"`
	ID            string          `xml:"cbc:ID"`
	UUID          string          `xml:"cbc:UUID"`
	IssueDate     string          `xml:"cbc:IssueDate"`
	DueDate       string          `xml:"cbc:DueDate,omitempty"`
	TypeCode      string          `xml:"cbc:InvoiceTypeCode"`
	Note          string          `xml:"cbc:Note,omitempty"`
	Currency      string          `xml:"cbc:DocumentCurrencyCode"`
	Attachments   []xmlAttachment `xml:"cac:AdditionalDocumentReference,omitempty"`
	Supplier      xmlParty        `xml:"cac:AccountingSupplierParty>cac:Party"`
	Customer      xmlParty        `xml:"cac:AccountingCustomerParty>cac:Party"`
	TaxTotal      xmlAmount       `xml:"cac:TaxTotal>cbc:TaxAmount"`
	Monetary      struct {
		LineExtension xmlAmount `xml:"cbc:LineExtensionAmount"`
		TaxExclusive  xmlAmount `xml:"cbc:TaxExclusiveAmount"`
		TaxInclusive  xmlAmount `xml:"cbc:TaxInclusiveAmount"`
		Payable       xmlAmount `xml:"cbc:PayableAmount"`
	} `xml:"cac:LegalMonetaryTotal"`
	Lines []xmlLine `xml:"cac:InvoiceLine"`
}

// RenderUBL renders the canonical invoice as UBL 2.1. reference is the submission identity the network echoes
// back (carried as the document UUID). The result is then PARSED BACK and its totals compared with the canonical
// totals: a transformer or profile bug that changes the money is caught before anything is sent.
func RenderUBL(inv *Invoice, rules *EInvoiceRules, reference string) ([]byte, error) {
	places := minorExponent(inv.Currency)
	x := xmlInvoice{NSCAC: nsCAC, NSCBC: nsCBC, ID: inv.InvoiceNo, UUID: reference, IssueDate: inv.IssueDate, DueDate: inv.DueDate,
		TypeCode: inv.Type, Note: inv.Note, Currency: inv.Currency, Customization: rules.CustomizationID, Profile: rules.ProfileID}
	ns, name := nsInvoice, "Invoice"
	if inv.Type == "381" {
		ns, name = nsCreditNote, "CreditNote"
	}
	x.XMLName = xml.Name{Local: name}
	x.XMLName.Space = ""
	party := func(p Party) xmlParty {
		return xmlParty{Endpoint: xmlEndpoint{SchemeID: p.Endpoint.Scheme, Value: p.Endpoint.ID}, Name: p.Name, TaxID: p.VATID}
	}
	x.Supplier, x.Customer = party(inv.Supplier), party(inv.Customer)
	cur := inv.Currency
	amt := func(s string) xmlAmount {
		r, _ := rat(s)
		return xmlAmount{CurrencyID: cur, Value: money(r, places)}
	}
	x.TaxTotal = amt(inv.Totals.Tax)
	x.Monetary.LineExtension, x.Monetary.TaxExclusive = amt(inv.Totals.Net), amt(inv.Totals.Net)
	x.Monetary.TaxInclusive, x.Monetary.Payable = amt(inv.Totals.Gross), amt(inv.Totals.Gross)
	for _, l := range inv.Lines {
		var xl xmlLine
		xl.ID = l.ID
		xl.Quantity.UnitCode, xl.Quantity.Value = "C62", l.Quantity
		xl.LineExtension = amt(l.LineTotal)
		xl.Item.Description = l.Description
		xl.Item.Category = xmlTaxCategory{Percent: l.TaxRate, Scheme: "VAT"}
		xl.Price = amt(l.UnitPrice)
		x.Lines = append(x.Lines, xl)
	}
	for _, a := range inv.Attachments {
		var xa xmlAttachment
		xa.Filename = a.Filename
		xa.Embedded.MimeCode, xa.Embedded.Filename, xa.Embedded.Value = a.Mime, a.Filename, a.ContentBase64
		x.Attachments = append(x.Attachments, xa)
	}
	body, err := xml.MarshalIndent(x, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: render", ErrInvoiceInvalid)
	}
	// the root carries its UBL namespace as the default namespace
	out := bytes.Replace(body, []byte("<"+name+" "), []byte("<"+name+` xmlns="`+ns+`" `), 1)
	if inv.Type == "381" { // a credit note names its line, quantity and type code differently
		for _, p := range [][2]string{{"cac:InvoiceLine", "cac:CreditNoteLine"}, {"cbc:InvoicedQuantity", "cbc:CreditedQuantity"}, {"cbc:InvoiceTypeCode", "cbc:CreditNoteTypeCode"}} {
			out = bytes.ReplaceAll(out, []byte(p[0]), []byte(p[1]))
		}
	}
	out = append([]byte(xml.Header), out...)
	if err := VerifyTransformedTotals(out, inv); err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyTransformedTotals reads the rendered UBL back and compares what it says with the canonical invoice: the
// payable, tax-inclusive, tax-exclusive and tax totals, the sum of the line amounts, the line count, the document
// currency on every amount, and the document identity. Exact decimal comparison.
func VerifyTransformedTotals(rendered []byte, inv *Invoice) error {
	mismatch := func(what string) error { return fmt.Errorf("%w: %s", ErrTotalMismatch, what) }
	dec := xml.NewDecoder(bytes.NewReader(rendered))
	places := minorExponent(inv.Currency)
	var path []string
	got := map[string]string{}
	lineSum, lines := new(big.Rat), 0
	currencies := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name.Local)
			for _, a := range t.Attr {
				if a.Name.Local == "currencyID" {
					currencies[a.Value] = true
				}
			}
			if len(path) == 2 && (t.Name.Local == "InvoiceLine" || t.Name.Local == "CreditNoteLine") {
				lines++
			}
		case xml.EndElement:
			path = path[:len(path)-1]
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text == "" || len(path) == 0 {
				continue
			}
			joined := strings.Join(path, "/")
			switch {
			case strings.HasSuffix(joined, "LegalMonetaryTotal/PayableAmount"):
				got["payable"] = text
			case strings.HasSuffix(joined, "LegalMonetaryTotal/TaxInclusiveAmount"):
				got["gross"] = text
			case strings.HasSuffix(joined, "LegalMonetaryTotal/TaxExclusiveAmount"):
				got["net"] = text
			case strings.HasSuffix(joined, "LegalMonetaryTotal/LineExtensionAmount"):
				got["lineext"] = text
			case strings.HasSuffix(joined, "TaxTotal/TaxAmount"):
				got["tax"] = text
			case strings.HasSuffix(joined, "Line/LineExtensionAmount") && len(path) == 3:
				if r, ok := rat(text); ok {
					lineSum.Add(lineSum, r)
				}
			case len(path) == 2 && path[1] == "ID":
				got["id"] = text
			}
		}
	}
	eq := func(key, want string) bool {
		g, ok1 := rat(got[key])
		w, ok2 := rat(want)
		return ok1 && ok2 && roundHalfUp(g, places).Cmp(roundHalfUp(w, places)) == 0
	}
	switch {
	case !eq("payable", inv.Totals.Gross):
		return mismatch("the payable amount differs from the canonical gross total")
	case !eq("gross", inv.Totals.Gross):
		return mismatch("the tax-inclusive amount differs from the canonical gross total")
	case !eq("net", inv.Totals.Net), !eq("lineext", inv.Totals.Net):
		return mismatch("the net amount differs from the canonical net total")
	case !eq("tax", inv.Totals.Tax):
		return mismatch("the tax amount differs from the canonical tax total")
	case roundHalfUp(lineSum, places).Cmp(func() *big.Rat { n, _ := rat(inv.Totals.Net); return roundHalfUp(n, places) }()) != 0:
		return mismatch("the rendered lines do not sum to the canonical net total")
	case lines != len(inv.Lines):
		return mismatch("the rendered line count differs from the canonical invoice")
	case got["id"] != inv.InvoiceNo:
		return mismatch("the document number differs from the canonical invoice")
	case len(currencies) != 1 || !currencies[inv.Currency]:
		return mismatch("an amount is not in the document currency")
	}
	return nil
}
