package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"math/big"
	"strings"
	"testing"
)

func einvoiceSpec() Spec {
	return Spec{
		Kind: "EINVOICE", RequiredScope: "einvoice.send", Rails: []string{"PEPPOL-BIS@3"}, Currencies: []string{"EUR", "JPY"}, MaxAmount: "1000000.00",
		Transform: TransformUBL,
		Submit:    Request{Path: "/v1/documents", ContentType: "application/xml"},
		Response: Response{RefPath: "document_id", StatusPath: "status", EchoPath: "document_uuid",
			StatusMap: map[string]string{"RECEIVED": "ACKNOWLEDGED", "CLEARED": "ACCEPTED", "DELIVERED": "DELIVERED", "REJECTED": "REJECTED"}},
		Auth: Auth{Type: "bearer"}, IdempotencyHeader: "Idempotency-Key", RetrySafe: true, TimeoutSeconds: 5,
		Schema: json.RawMessage(`{"type":"object","required":["invoice_no","lines","totals"]}`),
		EInvoice: &EInvoiceRules{
			RecipientSchemes: map[string]string{"0088": `^[0-9]{13}$`, "0192": `^[0-9]{9}$`},
			DocumentTypes:    []string{"380", "381"}, CustomizationID: "urn:cen.eu:en16931:2017", ProfileID: "urn:fdc:peppol.eu:2017:poacc:billing:01:1.0",
		},
	}
}

func goodInvoice() Invoice {
	return Invoice{InvoiceNo: "INV-2026-001", Type: "380", IssueDate: "2026-03-01", DueDate: "2026-03-31", Currency: "EUR", Note: "Net 30 & thanks <3",
		Supplier: Party{Name: "Acme & Sons GmbH", VATID: "DE123456789", Endpoint: Endpoint{Scheme: "0088", ID: "4012345000009"}},
		Customer: Party{Name: "Buyer Ltd", Endpoint: Endpoint{Scheme: "0192", ID: "987654321"}},
		Lines: []Line{
			{ID: "1", Description: "Widget", Quantity: "3", UnitPrice: "10.00", TaxRate: "20.00", LineTotal: "30.00"},
			{ID: "2", Description: "Gadget", Quantity: "1.5", UnitPrice: "9.99", TaxRate: "7.00", LineTotal: "14.99"}, // 14.985 rounds half up
		},
		Totals: Totals{Net: "44.99", Tax: "7.05", Gross: "52.04"}} // tax: 6.00 + 1.05 (14.99 x 7% = 1.0493)
}

func invJSON(t *testing.T, inv Invoice) []byte {
	t.Helper()
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func build(t *testing.T, s Spec, inv Invoice, rail string) ([]byte, error) {
	t.Helper()
	return s.Normalize().BuildRequestDoc(Canonical{"rail": rail, "reference": "sub-1"}, invJSON(t, inv))
}

func TestEInvoiceProfileValidation(t *testing.T) {
	if err := einvoiceSpec().Normalize().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Spec){
		"no rules":             func(s *Spec) { s.EInvoice = nil },
		"no schemes":           func(s *Spec) { s.EInvoice.RecipientSchemes = nil },
		"bad pattern":          func(s *Spec) { s.EInvoice.RecipientSchemes = map[string]string{"0088": "("} },
		"bad scheme id":        func(s *Spec) { s.EInvoice.RecipientSchemes = map[string]string{"bad id": ".*"} },
		"attachments w/o size": func(s *Spec) { s.EInvoice.AllowAttachments = true },
		"bad doc type":         func(s *Spec) { s.EInvoice.DocumentTypes = []string{"999"} },
		"body with transform":  func(s *Spec) { s.Submit.Body = []Field{{Name: "x", From: "const", Const: 1}} },
		"wrong content type":   func(s *Spec) { s.Submit.ContentType = "application/json" },
		"unknown transform":    func(s *Spec) { s.Transform = "other" },
		"no schema":            func(s *Spec) { s.Schema = nil },
	}
	for name, mut := range cases {
		s := einvoiceSpec()
		mut(&s)
		if err := s.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
			t.Errorf("%s: want ErrSpecInvalid, got %v", name, err)
		}
	}
	// only an EINVOICE profile may name the transformer or carry the rules
	tx := taxSpec()
	tx.Transform = TransformUBL
	if err := tx.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
		t.Errorf("tax + transform: %v", err)
	}
	tx = taxSpec()
	tx.EInvoice = einvoiceSpec().EInvoice
	if err := tx.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
		t.Errorf("tax + einvoice rules: %v", err)
	}
	// DELIVERED is an e-invoice network status; a tax or bank profile cannot map to it
	tx = taxSpec()
	tx.Response.StatusMap["X"] = "DELIVERED"
	if err := tx.Normalize().Validate(); !errors.Is(err, ErrSpecInvalid) {
		t.Errorf("tax + DELIVERED: %v", err)
	}
	// content type defaults to xml for the transformer
	n := einvoiceSpec()
	n.Submit.ContentType = ""
	if n.Normalize().Submit.ContentType != "application/xml" {
		t.Error("the transformer sends xml by default")
	}
}

func TestTransformRendersUBLWithExactTotals(t *testing.T) {
	out, err := build(t, einvoiceSpec(), goodInvoice(), "PEPPOL-BIS@3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`, `<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"`,
		`<cbc:CustomizationID>urn:cen.eu:en16931:2017</cbc:CustomizationID>`, `<cbc:ID>INV-2026-001</cbc:ID>`, `<cbc:UUID>sub-1</cbc:UUID>`,
		`<cbc:InvoiceTypeCode>380</cbc:InvoiceTypeCode>`, `<cbc:DocumentCurrencyCode>EUR</cbc:DocumentCurrencyCode>`,
		`<cbc:EndpointID schemeID="0088">4012345000009</cbc:EndpointID>`, `<cbc:EndpointID schemeID="0192">987654321</cbc:EndpointID>`,
		`<cbc:TaxAmount currencyID="EUR">7.05</cbc:TaxAmount>`, `<cbc:PayableAmount currencyID="EUR">52.04</cbc:PayableAmount>`,
		`<cbc:LineExtensionAmount currencyID="EUR">14.99</cbc:LineExtensionAmount>`, `<cbc:InvoicedQuantity unitCode="C62">1.5</cbc:InvoicedQuantity>`,
		`Acme &amp; Sons GmbH`, `Net 30 &amp; thanks &lt;3`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if err := xml.Unmarshal(out, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("the document must be well-formed: %v", err)
	}
	again, _ := build(t, einvoiceSpec(), goodInvoice(), "PEPPOL-BIS@3")
	if !bytes.Equal(out, again) {
		t.Fatal("the same approved invoice must render to the same bytes (the hash is evidence)")
	}
	inv := goodInvoice()
	if err := VerifyTransformedTotals(out, &inv); err != nil {
		t.Fatal(err)
	}
}

func TestCreditNoteUsesTheCreditNoteVocabulary(t *testing.T) {
	inv := goodInvoice()
	inv.Type = "381"
	out, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`<CreditNote xmlns="urn:oasis:names:specification:ubl:schema:xsd:CreditNote-2"`, `<cac:CreditNoteLine>`, `<cbc:CreditedQuantity unitCode="C62">`, `<cbc:CreditNoteTypeCode>381</cbc:CreditNoteTypeCode>`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(s, "InvoiceLine") || strings.Contains(s, "InvoicedQuantity") {
		t.Error("a credit note must not use invoice element names")
	}
	// a profile that does not carry credit notes refuses one
	sp := einvoiceSpec()
	sp.EInvoice.DocumentTypes = []string{"380"}
	if _, err := build(t, sp, inv, "PEPPOL-BIS@3"); !errors.Is(err, ErrRecipientInvalid) {
		t.Fatalf("%v", err)
	}
}

// acceptance: wrong profile
func TestWrongProfileIsRefusedExplicitly(t *testing.T) {
	if _, err := build(t, einvoiceSpec(), goodInvoice(), "OTHER-NET@1"); !errors.Is(err, ErrRailUnsupported) {
		t.Fatalf("%v", err)
	}
	inv := goodInvoice()
	inv.Currency = "USD"
	if _, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3"); !errors.Is(err, ErrInvoiceInvalid) && !errors.Is(err, ErrCurrency) {
		t.Fatalf("%v", err)
	}
	sp := einvoiceSpec()
	sp.MaxAmount = "50.00"
	if _, err := build(t, sp, goodInvoice(), "PEPPOL-BIS@3"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("the profile limit applies to the gross total: %v", err)
	}
	if _, err := einvoiceSpec().Normalize().BuildRequestDoc(Canonical{"rail": "PEPPOL-BIS@3"}, nil); !errors.Is(err, ErrDocumentMissing) {
		t.Fatalf("%v", err)
	}
}

// the canonical invoice must already be consistent: the adapter recomputes and compares, never repairs
func TestInconsistentEconomicsAreRefusedNotRepaired(t *testing.T) {
	cases := map[string]func(*Invoice){
		"lines do not sum to net": func(i *Invoice) { i.Totals.Net = "45.00" },
		"tax does not add up":     func(i *Invoice) { i.Totals.Tax = "7.00" },
		"gross is not net + tax":  func(i *Invoice) { i.Totals.Gross = "52.05" },
		"qty x price != line":     func(i *Invoice) { i.Lines[0].LineTotal = "31.00"; i.Totals.Net = "45.99" },
		"negative rate":           func(i *Invoice) { i.Lines[0].TaxRate = "-1" },
		"rate over 100":           func(i *Invoice) { i.Lines[0].TaxRate = "101" },
		"malformed number":        func(i *Invoice) { i.Lines[0].Quantity = "3,5" },
		"duplicate line id":       func(i *Invoice) { i.Lines[1].ID = "1" },
		"no lines":                func(i *Invoice) { i.Lines = nil },
		"bad type":                func(i *Invoice) { i.Type = "999" },
		"bad date":                func(i *Invoice) { i.IssueDate = "01/03/2026" },
		"no invoice number":       func(i *Invoice) { i.InvoiceNo = "" },
		"bad currency":            func(i *Invoice) { i.Currency = "euro" },
	}
	for name, mut := range cases {
		inv := goodInvoice()
		mut(&inv)
		if _, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3"); !errors.Is(err, ErrInvoiceInvalid) {
			t.Errorf("%s: want ErrInvoiceInvalid, got %v", name, err)
		}
	}
	// the failure never echoes a value
	inv := goodInvoice()
	inv.Totals.Net = "12345678.99"
	_, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3")
	if err == nil || strings.Contains(err.Error(), "12345678.99") {
		t.Fatalf("%v", err)
	}
	// unknown fields in the canonical document are refused: nothing rides along unchecked
	raw, _ := json.Marshal(map[string]any{"invoice_no": "X", "lines": []any{}, "totals": map[string]any{}, "secret_margin": 1})
	if _, err := einvoiceSpec().Normalize().BuildRequestDoc(Canonical{"rail": "PEPPOL-BIS@3", "reference": "r"}, raw); !errors.Is(err, ErrInvoiceInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestRoundingIsHalfUpAndCurrencyAware(t *testing.T) {
	// JPY has no minor unit: amounts are whole and the transformer must not invent decimals
	inv := Invoice{InvoiceNo: "J-1", Type: "380", IssueDate: "2026-03-01", Currency: "JPY",
		Supplier: Party{Name: "S", Endpoint: Endpoint{Scheme: "0088", ID: "4012345000009"}}, Customer: Party{Name: "C", Endpoint: Endpoint{Scheme: "0192", ID: "987654321"}},
		Lines:  []Line{{ID: "1", Description: "x", Quantity: "3", UnitPrice: "1000", TaxRate: "10.00", LineTotal: "3000"}},
		Totals: Totals{Net: "3000", Tax: "300", Gross: "3300"}}
	out, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `<cbc:PayableAmount currencyID="JPY">3300</cbc:PayableAmount>`) {
		t.Fatalf("%s", out)
	}
	if got := money(mustRat("2.675"), 2); got != "2.68" {
		t.Fatalf("half up: %s", got)
	}
	if got := money(mustRat("-2.675"), 2); got != "-2.68" {
		t.Fatalf("half away from zero: %s", got)
	}
}

func mustRat(s string) *big.Rat { r, _ := rat(s); return r }

// acceptance: recipient ID invalid
func TestInvalidRecipientBlocksBeforeSend(t *testing.T) {
	cases := map[string]func(*Invoice){
		"unlisted scheme":          func(i *Invoice) { i.Customer.Endpoint.Scheme = "9999" },
		"id does not match scheme": func(i *Invoice) { i.Customer.Endpoint.ID = "12" },
		"no scheme":                func(i *Invoice) { i.Customer.Endpoint = Endpoint{ID: "987654321"} },
		"no id":                    func(i *Invoice) { i.Customer.Endpoint.ID = "" },
		"supplier endpoint bad":    func(i *Invoice) { i.Supplier.Endpoint.ID = "abc" },
		"no customer name":         func(i *Invoice) { i.Customer.Name = "" },
	}
	for name, mut := range cases {
		inv := goodInvoice()
		mut(&inv)
		_, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3")
		if !errors.Is(err, ErrRecipientInvalid) && !errors.Is(err, ErrInvoiceInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	inv := goodInvoice()
	inv.Customer.Endpoint.ID = "SECRET-NOT-A-NUMBER"
	if _, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3"); err == nil || strings.Contains(err.Error(), "SECRET-NOT-A-NUMBER") {
		t.Fatalf("the failure must not echo the identifier: %v", err)
	}
}

// acceptance: attachment prohibited
func TestAttachmentPolicy(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	inv := goodInvoice()
	inv.Attachments = []Attachment{{Filename: "invoice.pdf", Mime: "application/pdf", ContentBase64: pdf}}
	if _, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3"); !errors.Is(err, ErrAttachmentProhibited) {
		t.Fatalf("prohibited by default: %v", err)
	}
	allow := einvoiceSpec()
	allow.EInvoice.AllowAttachments, allow.EInvoice.MaxAttachmentBytes, allow.EInvoice.AllowedMimeTypes = true, 1024, []string{"application/pdf"}
	out, err := build(t, allow, inv, "PEPPOL-BIS@3")
	if err != nil || !strings.Contains(string(out), `mimeCode="application/pdf" filename="invoice.pdf"`) {
		t.Fatalf("%v", err)
	}
	for name, a := range map[string]Attachment{
		"wrong mime":   {Filename: "x.exe", Mime: "application/x-msdownload", ContentBase64: pdf},
		"path in name": {Filename: "../x.pdf", Mime: "application/pdf", ContentBase64: pdf},
		"not base64":   {Filename: "x.pdf", Mime: "application/pdf", ContentBase64: "!!"},
		"empty":        {Filename: "x.pdf", Mime: "application/pdf", ContentBase64: ""},
	} {
		bad := goodInvoice()
		bad.Attachments = []Attachment{a}
		if _, err := build(t, allow, bad, "PEPPOL-BIS@3"); !errors.Is(err, ErrAttachmentProhibited) && !errors.Is(err, ErrInvoiceInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	big := goodInvoice()
	big.Attachments = []Attachment{{Filename: "big.pdf", Mime: "application/pdf", ContentBase64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 2048))}}
	if _, err := build(t, allow, big, "PEPPOL-BIS@3"); !errors.Is(err, ErrAttachmentProhibited) {
		t.Fatalf("over the size limit: %v", err)
	}
}

// acceptance: transformed total differs from canonical invoice
func TestATransformedDocumentThatChangesTheMoneyIsCaught(t *testing.T) {
	inv := goodInvoice()
	out, err := build(t, einvoiceSpec(), inv, "PEPPOL-BIS@3")
	if err != nil {
		t.Fatal(err)
	}
	tamper := func(old, new string) []byte {
		if !strings.Contains(string(out), old) {
			t.Fatalf("no %q to tamper with", old)
		}
		return []byte(strings.Replace(string(out), old, new, 1))
	}
	cases := map[string][]byte{
		"payable":     tamper(`<cbc:PayableAmount currencyID="EUR">52.04`, `<cbc:PayableAmount currencyID="EUR">52.05`),
		"gross":       tamper(`<cbc:TaxInclusiveAmount currencyID="EUR">52.04`, `<cbc:TaxInclusiveAmount currencyID="EUR">52.14`),
		"net":         tamper(`<cbc:TaxExclusiveAmount currencyID="EUR">44.99`, `<cbc:TaxExclusiveAmount currencyID="EUR">44.98`),
		"tax":         tamper(`<cbc:TaxAmount currencyID="EUR">7.05`, `<cbc:TaxAmount currencyID="EUR">7.06`),
		"a line":      tamper(`<cbc:LineExtensionAmount currencyID="EUR">30.00`, `<cbc:LineExtensionAmount currencyID="EUR">30.01`),
		"currency":    tamper(`<cbc:PayableAmount currencyID="EUR">`, `<cbc:PayableAmount currencyID="USD">`),
		"document no": tamper(`<cbc:ID>INV-2026-001</cbc:ID>`, `<cbc:ID>INV-2026-002</cbc:ID>`),
		"a lost line": []byte(strings.Replace(string(out), "<cac:InvoiceLine>", "<cac:Other>", 1)),
	}
	for name, doc := range cases {
		if err := VerifyTransformedTotals(doc, &inv); !errors.Is(err, ErrTotalMismatch) {
			t.Errorf("%s: want ErrTotalMismatch, got %v", name, err)
		}
	}
	// the check reads what the document SAYS: an amount that is only wrong after the document is rendered cannot hide
	if err := VerifyTransformedTotals([]byte("not xml"), &inv); !errors.Is(err, ErrTotalMismatch) {
		t.Errorf("garbage: %v", err)
	}
}
