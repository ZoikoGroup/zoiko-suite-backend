package kernel

import (
	"fmt"
	"sort"
	"time"
)

// Standard rule packs (standard §14–§17): the generic AR, AP and bank posting patterns, each with golden test cases. A pack is
// a starting point, not a shortcut around governance: instantiating one creates DRAFT rule sets, which still go through test,
// approval by someone other than the author and activation by a third person before anything posts.
//
// Account mappings are by key (AR_CONTROL, REVENUE, ...). Which account a key means is the tenant's own, versioned,
// effective-dated mapping, so the packs carry no account numbers.

// PackRule is one rule of a pack.
type PackRule struct {
	Name      string
	Domain    string
	EventType string
	Content   RuleContent
	TestPack  []TestCase
}

// RulePack is a named set of rules for one area.
type RulePack struct {
	Name        string
	Description string
	Rules       []PackRule
}

func line(ref, side, key, amount string, when ...Condition) LineTemplate {
	return LineTemplate{LineRef: ref, Side: side, Account: AccountRef{MappingKey: key}, Amount: amount, When: when}
}

func gt0(fact string) Condition { return Condition{Fact: fact, Op: "gt", Value: "0"} }

func content(tax []string, conds []Condition, lines ...LineTemplate) RuleContent {
	c := RuleContent{Conditions: conds, LineTemplates: lines, RoundingPolicyID: "HALF_EVEN"}
	if len(tax) > 0 {
		c.TaxDependency = &TaxDependency{RequiredFacts: tax}
	}
	return c
}

// packAccounts are the account codes the golden cases resolve each mapping key to.
var packAccounts = map[string]string{
	"AR_CONTROL": "1100", "REVENUE": "4000", "OUTPUT_TAX": "2200", "BANK": "1000", "UNAPPLIED_CASH": "2150", "BAD_DEBT": "6900", "SALES_RETURNS": "4100",
	"PROCESSOR_CLEARING": "1050", "PROCESSOR_FEES": "6950", "AP_CONTROL": "2000", "EXPENSE": "6000", "INPUT_TAX": "1300", "PAYMENT_CLEARING": "2050", "BANK_FEES": "6960", "FX_REALIZED_GAIN": "7100", "FX_REALIZED_LOSS": "7200",
	"PAYROLL_EXPENSE": "6100", "EMPLOYER_CONTRIB_EXPENSE": "6110", "PAYROLL_TAX_LIABILITY": "2310", "EMPLOYEE_DEDUCTIONS_LIABILITY": "2320", "EMPLOYER_CONTRIB_LIABILITY": "2330", "NET_PAY_PAYABLE": "2340",
	"FIXED_ASSET_COST": "1500", "ACCUMULATED_DEPRECIATION": "1590", "DEPRECIATION_EXPENSE": "6200", "IMPAIRMENT_LOSS": "6210", "REVALUATION_SURPLUS": "3100", "ASSET_CLEARING": "2060",
	"DISPOSAL_GAIN": "7500", "DISPOSAL_LOSS": "7510", "INVENTORY": "1400", "GRNI": "2070", "COGS": "5000", "INVENTORY_VARIANCE": "5100",
	"TAX_PAYABLE": "2210", "INCOME_TAX_EXPENSE": "8000", "DEFERRED_TAX": "2220", "TAX_ADJUSTMENT": "8100", "WITHHOLDING_RECEIVABLE": "1310",
	"PREPAID_ASSET": "1200", "ACCRUED_LIABILITY": "2400", "DEFERRED_REVENUE": "2500",
	"PAYROLL_GROSS_EXPENSE": "6150", "PAYROLL_EMPLOYER_TAX_EXPENSE": "6151", "PAYROLL_EMPLOYER_SOCIAL_EXPENSE": "6152", "PAYROLL_EMPLOYER_PENSION_EXPENSE": "6154", "PAYROLL_EMPLOYER_BENEFIT_EXPENSE": "6153", "PAYROLL_EMPLOYER_OTHER_EXPENSE": "6155",
	"PAYROLL_NET_PAY_PAYABLE": "2350", "PAYROLL_EMPLOYEE_DEDUCTION_PAYABLE": "2351", "PAYROLL_EMPLOYEE_TAX_PAYABLE": "2352", "PAYROLL_EMPLOYEE_SOCIAL_PAYABLE": "2353", "PAYROLL_EMPLOYEE_PENSION_PAYABLE": "2354", "PAYROLL_EMPLOYEE_BENEFIT_PAYABLE": "2355",
	"PAYROLL_EMPLOYER_TAX_PAYABLE": "2361", "PAYROLL_EMPLOYER_SOCIAL_PAYABLE": "2362", "PAYROLL_EMPLOYER_BENEFIT_PAYABLE": "2363", "PAYROLL_EMPLOYER_PENSION_PAYABLE": "2364", "PAYROLL_EMPLOYER_OTHER_PAYABLE": "2365",
}

var packControls = map[string]string{"1100": "AR", "2000": "AP", "1000": "BANK", "2150": "CLEARING", "1050": "CLEARING", "2050": "CLEARING",
	"2310": "PAYROLL", "2320": "PAYROLL", "2330": "PAYROLL", "2340": "PAYROLL", "1500": "ASSET", "1590": "ASSET", "2060": "CLEARING", "1400": "INVENTORY", "2070": "CLEARING", "2210": "TAX", "2220": "TAX"}

func golden(name, event, cp string, facts map[string]string, want ...ExpectedLine) TestCase {
	return TestCase{Name: name, Event: TestEvent{EventType: event, TransactionCurrency: "GBP", Facts: facts, CounterpartyID: cp},
		Accounts: packAccounts, Controls: packControls, Expect: TestExpectation{Lines: want}}
}

func refused(name, event, cp string, facts map[string]string, family string) TestCase {
	return TestCase{Name: name, Event: TestEvent{EventType: event, TransactionCurrency: "GBP", Facts: facts, CounterpartyID: cp},
		Accounts: packAccounts, Controls: packControls, Expect: TestExpectation{Exception: family}}
}

func dr(code, amount string) ExpectedLine {
	return ExpectedLine{Side: Debit, AccountCode: code, Amount: amount}
}
func cr(code, amount string) ExpectedLine {
	return ExpectedLine{Side: Credit, AccountCode: code, Amount: amount}
}

// drf and crf also state the functional-currency amount, for the cases that are about the rate.
func drf(code, amount, functional string) ExpectedLine {
	return ExpectedLine{Side: Debit, AccountCode: code, Amount: amount, Functional: functional}
}
func crf(code, amount, functional string) ExpectedLine {
	return ExpectedLine{Side: Credit, AccountCode: code, Amount: amount, Functional: functional}
}

// settledAt is a USD event in a GBP book at a settlement rate, for the realized-exchange cases.
func settledAt(name, event, cp string, facts map[string]string, rate string, want ...ExpectedLine) TestCase {
	return TestCase{Name: name, Event: TestEvent{EventType: event, TransactionCurrency: "USD", Facts: facts, CounterpartyID: cp}, Accounts: packAccounts, Controls: packControls,
		Functional: "GBP", FX: &FXBasis{Rate: rate, RateType: "SPOT", Source: "golden", AsOf: "2026-03-10"}, Expect: TestExpectation{Lines: want}}
}

var realizedFX = &RealizedFX{Gain: AccountRef{MappingKey: "FX_REALIZED_GAIN"}, Loss: AccountRef{MappingKey: "FX_REALIZED_LOSS"}}

func withRealizedFX(c RuleContent) RuleContent {
	c.RealizedFX = realizedFX
	return c
}

type f = map[string]string

// Packs returns the standard rule packs: the core AR, AP and bank packs and the subledger packs of the later waves.
func Packs() []RulePack {
	return append(append(corePacks(), subledgerPacks()...), payrollResultPacks()...)
}

func corePacks() []RulePack {
	taxFacts := []string{"gross", "net", "tax"}
	return []RulePack{
		{
			Name:        "ar-standard",
			Description: "Accounts receivable: invoice, credit note, cash receipt (applied and unapplied), allocation, write-off, processor receipt and settlement, chargeback",
			Rules: []PackRule{
				{Name: "ar-invoice-issued", Domain: "AR", EventType: "AR.INVOICE_ISSUED",
					Content: content(taxFacts, nil, line("L1", Debit, "AR_CONTROL", "gross"), line("L2", Credit, "REVENUE", "net"), line("L3", Credit, "OUTPUT_TAX", "tax", gt0("tax"))),
					TestPack: []TestCase{
						golden("invoice with tax", "AR.INVOICE_ISSUED", "CUST-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00"}, dr("1100", "120.00"), cr("4000", "100.00"), cr("2200", "20.00")),
						golden("invoice without tax", "AR.INVOICE_ISSUED", "CUST-1", f{"gross": "100.00", "net": "100.00", "tax": "0.00"}, dr("1100", "100.00"), cr("4000", "100.00")),
						refused("a receivable needs its customer", "AR.INVOICE_ISSUED", "", f{"gross": "120.00", "net": "100.00", "tax": "20.00"}, FamCOA),
						refused("the tax result is authoritative", "AR.INVOICE_ISSUED", "CUST-1", f{"gross": "120.00", "net": "100.00"}, FamTax),
						refused("a total that does not add up is not posted", "AR.INVOICE_ISSUED", "CUST-1", f{"gross": "120.01", "net": "100.00", "tax": "20.00"}, FamBal),
					}},
				{Name: "ar-credit-note-issued", Domain: "AR", EventType: "AR.CREDIT_NOTE_ISSUED",
					Content: content(taxFacts, []Condition{{Fact: "original_invoice_ref", Op: "exists"}}, line("L1", Debit, "SALES_RETURNS", "net"), line("L2", Debit, "OUTPUT_TAX", "tax", gt0("tax")), line("L3", Credit, "AR_CONTROL", "gross")),
					TestPack: []TestCase{
						golden("credit note", "AR.CREDIT_NOTE_ISSUED", "CUST-1", f{"gross": "60.00", "net": "50.00", "tax": "10.00", "original_invoice_ref": "INV-1"}, dr("4100", "50.00"), dr("2200", "10.00"), cr("1100", "60.00")),
						refused("a credit note names the invoice it corrects", "AR.CREDIT_NOTE_ISSUED", "CUST-1", f{"gross": "60.00", "net": "50.00", "tax": "10.00"}, FamRule),
					}},
				{Name: "ar-cash-received", Domain: "AR", EventType: "AR.CASH_RECEIVED",
					Content: content(nil, []Condition{{Fact: "allocation_ref", Op: "exists"}}, line("L1", Debit, "BANK", "amount"), line("L2", Credit, "AR_CONTROL", "amount")),
					TestPack: []TestCase{
						golden("allocated receipt", "AR.CASH_RECEIVED", "CUST-1", f{"amount": "120.00", "allocation_ref": "ALLOC-1"}, dr("1000", "120.00"), cr("1100", "120.00")),
						refused("an unallocated receipt is not applied to a receivable", "AR.CASH_RECEIVED", "CUST-1", f{"amount": "120.00"}, FamRule),
					}},
				{Name: "ar-cash-received-unapplied", Domain: "AR", EventType: "AR.CASH_RECEIVED_UNAPPLIED",
					Content: content(nil, nil, line("L1", Debit, "BANK", "amount"), line("L2", Credit, "UNAPPLIED_CASH", "amount")),
					TestPack: []TestCase{
						golden("unapplied cash waits in clearing", "AR.CASH_RECEIVED_UNAPPLIED", "CUST-1", f{"amount": "75.50"}, dr("1000", "75.50"), cr("2150", "75.50")),
					}},
				{Name: "ar-cash-allocated", Domain: "AR", EventType: "AR.CASH_ALLOCATED",
					Content: content(nil, []Condition{{Fact: "allocation_ref", Op: "exists"}}, line("L1", Debit, "UNAPPLIED_CASH", "amount"), line("L2", Credit, "AR_CONTROL", "amount")),
					TestPack: []TestCase{
						golden("allocation clears the unapplied cash", "AR.CASH_ALLOCATED", "CUST-1", f{"amount": "75.50", "allocation_ref": "ALLOC-2"}, dr("2150", "75.50"), cr("1100", "75.50")),
					}},
				{Name: "ar-cash-received-fx", Domain: "AR", EventType: "AR.CASH_RECEIVED_FX",
					Content: withRealizedFX(RuleContent{Conditions: []Condition{{Fact: "allocation_ref", Op: "exists"}, {Fact: "carrying_functional", Op: "exists"}}, RoundingPolicyID: "HALF_EVEN",
						LineTemplates: []LineTemplate{line("L1", Debit, "BANK", "amount"), {LineRef: "L2", Side: Credit, Account: AccountRef{MappingKey: "AR_CONTROL"}, Amount: "amount", Functional: "carrying_functional"}}}),
					TestPack: []TestCase{
						settledAt("settled at a better rate than recognised: a realized gain", "AR.CASH_RECEIVED_FX", "CUST-1", f{"amount": "100.00", "allocation_ref": "A-1", "carrying_functional": "80.00"}, "0.85",
							drf("1000", "100.00", "85.00"), crf("1100", "100.00", "80.00"), crf("7100", "0.00", "5.00")),
						settledAt("settled at a worse rate: a realized loss", "AR.CASH_RECEIVED_FX", "CUST-1", f{"amount": "100.00", "allocation_ref": "A-2", "carrying_functional": "90.00"}, "0.85",
							drf("1000", "100.00", "85.00"), crf("1100", "100.00", "90.00"), drf("7200", "0.00", "5.00")),
						settledAt("settled at the carrying amount: no exchange difference", "AR.CASH_RECEIVED_FX", "CUST-1", f{"amount": "100.00", "allocation_ref": "A-3", "carrying_functional": "85.00"}, "0.85",
							drf("1000", "100.00", "85.00"), crf("1100", "100.00", "85.00")),
						{Name: "the carrying amount comes from the subledger and is required", Event: TestEvent{EventType: "AR.CASH_RECEIVED_FX", TransactionCurrency: "USD", Facts: f{"amount": "100.00", "allocation_ref": "A-4"}, CounterpartyID: "CUST-1"},
							Accounts: packAccounts, Controls: packControls, Functional: "GBP", FX: &FXBasis{Rate: "0.85", RateType: "SPOT", Source: "golden", AsOf: "2026-03-10"}, Expect: TestExpectation{Exception: FamRule}},
					}},
				{Name: "ar-write-off", Domain: "AR", EventType: "AR.WRITE_OFF",
					Content: content(taxFacts, []Condition{{Fact: "reason_code", Op: "exists"}, {Fact: "authority_ref", Op: "exists"}}, line("L1", Debit, "BAD_DEBT", "net"), line("L2", Debit, "OUTPUT_TAX", "tax", gt0("tax")), line("L3", Credit, "AR_CONTROL", "gross")),
					TestPack: []TestCase{
						golden("write-off with reason and authority", "AR.WRITE_OFF", "CUST-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "reason_code": "INSOLVENCY", "authority_ref": "APPR-9"}, dr("6900", "100.00"), dr("2200", "20.00"), cr("1100", "120.00")),
						refused("a write-off needs a reason code and an authority", "AR.WRITE_OFF", "CUST-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00"}, FamRule),
					}},
				{Name: "ar-receipt-via-processor", Domain: "AR", EventType: "AR.RECEIPT_VIA_PROCESSOR",
					Content: content(nil, nil, line("L1", Debit, "PROCESSOR_CLEARING", "amount"), line("L2", Credit, "AR_CONTROL", "amount")),
					TestPack: []TestCase{
						golden("a card receipt posts to processor clearing first", "AR.RECEIPT_VIA_PROCESSOR", "CUST-1", f{"amount": "200.00"}, dr("1050", "200.00"), cr("1100", "200.00")),
					}},
				{Name: "bank-processor-settled", Domain: "BANK", EventType: "BANK.PROCESSOR_SETTLED",
					Content: content(nil, nil, line("L1", Debit, "BANK", "net_settlement"), line("L2", Debit, "PROCESSOR_FEES", "fee", gt0("fee")), line("L3", Credit, "PROCESSOR_CLEARING", "gross")),
					TestPack: []TestCase{
						golden("settlement net of the processor fee", "BANK.PROCESSOR_SETTLED", "", f{"gross": "200.00", "fee": "5.80", "net_settlement": "194.20"}, dr("1000", "194.20"), dr("6950", "5.80"), cr("1050", "200.00")),
						refused("a settlement that does not add up is not posted", "BANK.PROCESSOR_SETTLED", "", f{"gross": "200.00", "fee": "5.80", "net_settlement": "194.00"}, FamBal),
					}},
				{Name: "ar-chargeback", Domain: "AR", EventType: "AR.CHARGEBACK",
					Content: content(nil, []Condition{{Fact: "original_receipt_ref", Op: "exists"}}, line("L1", Debit, "AR_CONTROL", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{
						golden("a chargeback is a new event that names the receipt", "AR.CHARGEBACK", "CUST-1", f{"amount": "120.00", "original_receipt_ref": "RCPT-1"}, dr("1100", "120.00"), cr("1000", "120.00")),
						refused("a chargeback names the original receipt", "AR.CHARGEBACK", "CUST-1", f{"amount": "120.00"}, FamRule),
					}},
			},
		},
		{
			Name:        "ap-standard",
			Description: "Accounts payable: supplier invoice, supplier credit note, payment accepted, settled and rejected",
			Rules: []PackRule{
				{Name: "ap-invoice-approved", Domain: "AP", EventType: "AP.INVOICE_APPROVED",
					Content: content([]string{"gross", "net", "tax", "tax_recoverable"},
						[]Condition{{Fact: "duplicate_check", Op: "eq", Value: "PASSED"}, {Fact: "match_status", Op: "in", Value: []any{"MATCHED", "NOT_REQUIRED"}}},
						line("L1", Debit, "EXPENSE", "net + tax - tax_recoverable"), line("L2", Debit, "INPUT_TAX", "tax_recoverable", gt0("tax_recoverable")), line("L3", Credit, "AP_CONTROL", "gross")),
					TestPack: []TestCase{
						golden("fully recoverable input tax", "AP.INVOICE_APPROVED", "SUP-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PASSED", "match_status": "MATCHED"}, dr("6000", "100.00"), dr("1300", "20.00"), cr("2000", "120.00")),
						golden("irrecoverable tax goes to the expense", "AP.INVOICE_APPROVED", "SUP-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "0.00", "duplicate_check": "PASSED", "match_status": "NOT_REQUIRED"}, dr("6000", "120.00"), cr("2000", "120.00")),
						refused("duplicate detection must have completed", "AP.INVOICE_APPROVED", "SUP-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PENDING", "match_status": "MATCHED"}, FamRule),
						refused("a failed three-way match is not posted", "AP.INVOICE_APPROVED", "SUP-1", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PASSED", "match_status": "OUT_OF_TOLERANCE"}, FamRule),
						refused("a payable needs its supplier", "AP.INVOICE_APPROVED", "", f{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PASSED", "match_status": "MATCHED"}, FamCOA),
					}},
				{Name: "ap-credit-note-received", Domain: "AP", EventType: "AP.CREDIT_NOTE_RECEIVED",
					Content: content([]string{"gross", "net", "tax", "tax_recoverable"}, []Condition{{Fact: "original_invoice_ref", Op: "exists"}},
						line("L1", Debit, "AP_CONTROL", "gross"), line("L2", Credit, "INPUT_TAX", "tax_recoverable", gt0("tax_recoverable")), line("L3", Credit, "EXPENSE", "net + tax - tax_recoverable")),
					TestPack: []TestCase{
						golden("supplier credit note", "AP.CREDIT_NOTE_RECEIVED", "SUP-1", f{"gross": "60.00", "net": "50.00", "tax": "10.00", "tax_recoverable": "10.00", "original_invoice_ref": "SINV-1"}, dr("2000", "60.00"), cr("1300", "10.00"), cr("6000", "50.00")),
					}},
				{Name: "ap-payment-accepted", Domain: "AP", EventType: "AP.PAYMENT_ACCEPTED",
					Content: content(nil, nil, line("L1", Debit, "AP_CONTROL", "amount"), line("L2", Credit, "PAYMENT_CLEARING", "amount")),
					TestPack: []TestCase{
						golden("acceptance moves the payable to payment clearing", "AP.PAYMENT_ACCEPTED", "SUP-1", f{"amount": "120.00"}, dr("2000", "120.00"), cr("2050", "120.00")),
					}},
				{Name: "ap-payment-settled", Domain: "AP", EventType: "AP.PAYMENT_SETTLED",
					Content: content(nil, nil, line("L1", Debit, "PAYMENT_CLEARING", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{
						golden("settlement credits the bank only now", "AP.PAYMENT_SETTLED", "SUP-1", f{"amount": "120.00"}, dr("2050", "120.00"), cr("1000", "120.00")),
					}},
				{Name: "ap-payment-settled-fx", Domain: "AP", EventType: "AP.PAYMENT_SETTLED_FX",
					Content: withRealizedFX(RuleContent{Conditions: []Condition{{Fact: "carrying_functional", Op: "exists"}}, RoundingPolicyID: "HALF_EVEN",
						LineTemplates: []LineTemplate{{LineRef: "L1", Side: Debit, Account: AccountRef{MappingKey: "PAYMENT_CLEARING"}, Amount: "amount", Functional: "carrying_functional"}, line("L2", Credit, "BANK", "amount")}}),
					TestPack: []TestCase{
						settledAt("a supplier payment settled at a worse rate: a realized loss", "AP.PAYMENT_SETTLED_FX", "SUP-1", f{"amount": "100.00", "carrying_functional": "80.00"}, "0.85",
							drf("2050", "100.00", "80.00"), crf("1000", "100.00", "85.00"), drf("7200", "0.00", "5.00")),
						settledAt("settled at a better rate: a realized gain", "AP.PAYMENT_SETTLED_FX", "SUP-1", f{"amount": "100.00", "carrying_functional": "90.00"}, "0.85",
							drf("2050", "100.00", "90.00"), crf("1000", "100.00", "85.00"), crf("7100", "0.00", "5.00")),
					}},
				{Name: "ap-payment-rejected", Domain: "AP", EventType: "AP.PAYMENT_REJECTED",
					Content: content(nil, nil, line("L1", Debit, "PAYMENT_CLEARING", "amount"), line("L2", Credit, "AP_CONTROL", "amount")),
					TestPack: []TestCase{
						golden("rejection restores the payable", "AP.PAYMENT_REJECTED", "SUP-1", f{"amount": "120.00"}, dr("2050", "120.00"), cr("2000", "120.00")),
					}},
			},
		},
		{
			Name:        "bank-standard",
			Description: "Bank and cash: bank fees. Imported bank transactions are not journals; they are matched, and an adjustment is a controlled journal naming its reconciliation case",
			Rules: []PackRule{
				{Name: "bank-fee", Domain: "BANK", EventType: "BANK.FEE",
					Content: content(taxFacts, nil, line("L1", Debit, "BANK_FEES", "net"), line("L2", Debit, "INPUT_TAX", "tax", gt0("tax")), line("L3", Credit, "BANK", "gross")),
					TestPack: []TestCase{
						golden("bank fee with recoverable tax", "BANK.FEE", "", f{"gross": "12.00", "net": "10.00", "tax": "2.00"}, dr("6960", "10.00"), dr("1300", "2.00"), cr("1000", "12.00")),
						golden("bank fee without tax", "BANK.FEE", "", f{"gross": "10.00", "net": "10.00", "tax": "0.00"}, dr("6960", "10.00"), cr("1000", "10.00")),
					}},
			},
		},
	}
}

// PackByName returns a pack, or false.
func PackByName(name string) (RulePack, bool) {
	for _, p := range Packs() {
		if p.Name == name {
			return p, true
		}
	}
	return RulePack{}, false
}

// PackEventTypes lists the event types a pack posts, in order.
func (p RulePack) PackEventTypes() []string {
	var out []string
	for _, r := range p.Rules {
		out = append(out, r.EventType)
	}
	sort.Strings(out)
	return out
}

// Drafts turns the pack into rule sets ready to be created as drafts for a book type and framework from a date.
func (p RulePack) Drafts(version string, effectiveFrom time.Time, bookType, framework string) []RuleSet {
	out := make([]RuleSet, 0, len(p.Rules))
	for _, r := range p.Rules {
		out = append(out, RuleSet{
			Name: r.Name, Domain: r.Domain, EventType: r.EventType, BookType: bookType, FrameworkID: framework, Version: version,
			EffectiveFrom: effectiveFrom, Content: r.Content, TestPack: r.TestPack, TestPackID: fmt.Sprintf("pack-%s-%s", p.Name, version),
		})
	}
	return out
}
