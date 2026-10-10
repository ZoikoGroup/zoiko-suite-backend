package domain

import (
	"strings"
	"testing"
	"time"
)

func baseProposal() *PaymentProposal {
	return &PaymentProposal{
		ProposalID: "prop-1", LegalEntityID: "le-1", PayingBankAccountRef: "acct-1", Currency: "USD",
		PaymentDate: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), PaymentMethod: "ACH",
		GrossAmount: 110, WithholdingAmount: 10, NetAmount: 100,
	}
}

func baseItems() []ProposalItem {
	snap := time.Date(2026, 10, 1, 8, 0, 0, 123456000, time.UTC)
	return []ProposalItem{
		{ItemID: "i-1", PayableSource: SourceAPInvoice, PayableID: "inv-1", PayeeRef: "sup-1", GrossAmount: 110,
			WithholdingAmount: 10, NetAmount: 100, Currency: "USD", DueDate: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
			PayeeSnapshotAt: &snap, TaxDeterminationID: "tax-1", IsActive: true},
	}
}

// TestComputeFingerprint_BindsEveryAuthorizedField is negative-path #26/#27:
// each field the spec says an authorization binds must change the digest.
func TestComputeFingerprint_BindsEveryAuthorizedField(t *testing.T) {
	base := ComputeFingerprint(baseProposal(), baseItems(), StatusFrozen)
	if !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("expected sha256: prefix, got %q", base)
	}

	proposalChanges := map[string]func(p *PaymentProposal){
		"proposal id":    func(p *PaymentProposal) { p.ProposalID = "prop-2" },
		"legal entity":   func(p *PaymentProposal) { p.LegalEntityID = "le-2" },
		"payer account":  func(p *PaymentProposal) { p.PayingBankAccountRef = "acct-2" },
		"currency":       func(p *PaymentProposal) { p.Currency = "EUR" },
		"value date":     func(p *PaymentProposal) { p.PaymentDate = p.PaymentDate.AddDate(0, 0, 1) },
		"payment method": func(p *PaymentProposal) { p.PaymentMethod = "WIRE" },
		"gross amount":   func(p *PaymentProposal) { p.GrossAmount = 110.01 },
		"withholding":    func(p *PaymentProposal) { p.WithholdingAmount = 10.01 },
		"net amount":     func(p *PaymentProposal) { p.NetAmount = 100.01 },
	}
	for name, mutate := range proposalChanges {
		p := baseProposal()
		mutate(p)
		if ComputeFingerprint(p, baseItems(), StatusFrozen) == base {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}

	itemChanges := map[string]func(it *ProposalItem){
		"payee":                 func(it *ProposalItem) { it.PayeeRef = "sup-2" },
		"payable":               func(it *ProposalItem) { it.PayableID = "inv-2" },
		"payable source":        func(it *ProposalItem) { it.PayableSource = SourceExpenseClaim },
		"item gross":            func(it *ProposalItem) { it.GrossAmount = 111 },
		"item withholding":      func(it *ProposalItem) { it.WithholdingAmount = 11 },
		"item net":              func(it *ProposalItem) { it.NetAmount = 99 },
		"item currency":         func(it *ProposalItem) { it.Currency = "EUR" },
		"due date":              func(it *ProposalItem) { it.DueDate = it.DueDate.AddDate(0, 0, 1) },
		"payee version":         func(it *ProposalItem) { s := it.PayeeSnapshotAt.Add(time.Microsecond); it.PayeeSnapshotAt = &s },
		"payee version removed": func(it *ProposalItem) { it.PayeeSnapshotAt = nil },
		"tax determination":     func(it *ProposalItem) { it.TaxDeterminationID = "tax-2" },
		"exception ref":         func(it *ProposalItem) { it.ExceptionRef = "exc-1" },
	}
	for name, mutate := range itemChanges {
		items := baseItems()
		mutate(&items[0])
		if ComputeFingerprint(baseProposal(), items, StatusFrozen) == base {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}

	if ComputeFingerprint(baseProposal(), baseItems(), StatusCancelled) == base {
		t.Error("a different status did not change the fingerprint")
	}
	extra := append(baseItems(), ProposalItem{ItemID: "i-2", PayableID: "inv-9", NetAmount: 1, IsActive: true})
	if ComputeFingerprint(baseProposal(), extra, StatusFrozen) == base {
		t.Error("an additional item did not change the fingerprint")
	}
}

func TestComputeFingerprint_StableAndIgnoresInactiveAndOrder(t *testing.T) {
	two := append(baseItems(), ProposalItem{ItemID: "i-2", PayableSource: SourceAPInvoice, PayableID: "inv-2", PayeeRef: "sup-2", NetAmount: 5, GrossAmount: 5, Currency: "USD", IsActive: true})
	a := ComputeFingerprint(baseProposal(), two, StatusFrozen)

	reversed := []ProposalItem{two[1], two[0]}
	if ComputeFingerprint(baseProposal(), reversed, StatusFrozen) != a {
		t.Error("item order must not change the fingerprint")
	}
	withInactive := append(append([]ProposalItem{}, two...), ProposalItem{ItemID: "i-3", PayableID: "gone", NetAmount: 9, IsActive: false})
	if ComputeFingerprint(baseProposal(), withInactive, StatusFrozen) != a {
		t.Error("an inactive (removed) item must not change the fingerprint")
	}
	if ComputeFingerprint(baseProposal(), two, StatusFrozen) != a {
		t.Error("fingerprint is not deterministic")
	}
	// 0.1+0.2 style float noise must not matter: money is hashed in cents.
	p := baseProposal()
	p.NetAmount = 0.1 + 0.2
	q := baseProposal()
	q.NetAmount = 0.3
	if ComputeFingerprint(p, nil, StatusFrozen) != ComputeFingerprint(q, nil, StatusFrozen) {
		t.Error("float noise changed the fingerprint")
	}
}

// TestComputeFingerprint_FieldsCannotBleed: quoting keeps adjacent string
// fields from colliding ("ab","c" vs "a","bc").
func TestComputeFingerprint_FieldsCannotBleed(t *testing.T) {
	a := baseItems()
	a[0].PayableID, a[0].PayeeRef = "ab", "c"
	b := baseItems()
	b[0].PayableID, b[0].PayeeRef = "a", "bc"
	if ComputeFingerprint(baseProposal(), a, StatusFrozen) == ComputeFingerprint(baseProposal(), b, StatusFrozen) {
		t.Error("adjacent fields collided")
	}
}
