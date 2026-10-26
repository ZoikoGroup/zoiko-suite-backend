package kernel

import (
	"context"
	"testing"
	"time"
)

func TestEveryStandardPackPassesItsOwnGoldenCases(t *testing.T) {
	for _, p := range Packs() {
		for _, rs := range p.Drafts("1.0.0", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BookStatutory, "IFRS") {
			rs := rs
			rs.TenantID = tenant
			rs.Status = RuleDraft
			if err := rs.Validate(); err != nil {
				t.Fatalf("%s/%s does not validate: %v", p.Name, rs.Name, err)
			}
			if len(rs.TestPack) == 0 {
				t.Fatalf("%s/%s has no golden cases", p.Name, rs.Name)
			}
			if failed := RunTestPack(context.Background(), &rs, *book()); len(failed) != 0 {
				t.Fatalf("%s/%s: %v", p.Name, rs.Name, failed)
			}
		}
	}
}

func TestPacksCoverTheMatricesOfSection14(t *testing.T) {
	have := map[string]bool{}
	for _, p := range Packs() {
		for _, et := range p.PackEventTypes() {
			if have[et] {
				t.Fatalf("event type %s is in two packs", et)
			}
			have[et] = true
		}
	}
	for _, et := range []string{"AR.INVOICE_ISSUED", "AR.CREDIT_NOTE_ISSUED", "AR.CASH_RECEIVED", "AP.INVOICE_APPROVED", "AP.PAYMENT_ACCEPTED", "AP.PAYMENT_SETTLED", "AP.PAYMENT_REJECTED", "AR.WRITE_OFF", "AR.CHARGEBACK", "BANK.FEE", "BANK.PROCESSOR_SETTLED"} {
		if !have[et] {
			t.Fatalf("no standard rule for %s", et)
		}
	}
	if _, ok := PackByName("ar-standard"); !ok {
		t.Fatal("ar-standard")
	}
	if _, ok := PackByName("nope"); ok {
		t.Fatal("an unknown pack")
	}
}

func TestACounterpartyOnTheEventReachesTheControlAccountLine(t *testing.T) {
	p, _ := PackByName("ar-standard")
	rs := p.Drafts("1.0.0", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BookStatutory, "IFRS")[0]
	rs.Status = RuleActive
	ev := &AccountingEvent{AccountingEventID: "e", TenantID: tenant, LegalEntityID: entity, EventType: rs.EventType, TransactionCurrency: "GBP", CounterpartyID: "CUST-9",
		Facts: map[string]string{"gross": "120.00", "net": "100.00", "tax": "20.00"}}
	prop, err := Evaluate(context.Background(), EvalInput{Event: ev, Book: book(), Rule: &rs, PostingDate: rs.EffectiveFrom, Directory: testDirectory{accounts: packAccounts, controls: packControls}})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range prop.Lines {
		switch l.AccountCode {
		case "1100":
			if l.CounterpartyID != "CUST-9" || l.ControlType != "AR" {
				t.Fatalf("the receivable line carries its customer: %+v", l)
			}
		default:
			if l.CounterpartyID != "" {
				t.Fatalf("an ordinary line carries no counterparty: %+v", l)
			}
		}
	}
}
