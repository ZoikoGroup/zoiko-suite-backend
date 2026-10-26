package domain

import (
	"math/big"
	"testing"
)

func bridgeFor(t *testing.T, in ResultPostingInput) *AccountingBridge {
	t.Helper()
	lines, err := BuildPostingLines(in)
	if err != nil {
		t.Fatal(err)
	}
	return &AccountingBridge{RunRefID: "run-1", ResultID: "res-7", Lines: lines}
}

func kernelRat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

// The facts of the kernel event say the same as the proposal: gross, net and every component kind, with the employer side carried once
// because the ledger rule posts the cost and the liability from one amount.
func TestKernelFacts_CarryTheProposalComponentByComponent(t *testing.T) {
	b := bridgeFor(t, ResultPostingInput{Gross: kernelRat("10000"), Net: kernelRat("7500"), ByKind: map[string]*big.Rat{
		KindEmployeeTax: kernelRat("1800"), KindEmployeePension: kernelRat("700"), KindEmployerTax: kernelRat("600"), KindEmployerPension: kernelRat("300")}})
	f, err := b.KernelFacts()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gross": "10000.0000", "net": "7500.0000", "ee_tax": "1800.0000", "ee_pension": "700.0000", "er_tax": "600.0000", "er_pension": "300.0000",
		"ee_deduction": "0", "ee_social": "0", "ee_benefit": "0", "er_social": "0", "er_benefit": "0", "er_other": "0", "result_ref": "run-1", "result_version": "res-7"}
	for k, v := range want {
		if f[k] != v {
			t.Fatalf("fact %s: %q, want %q (%v)", k, f[k], v, f)
		}
	}
	if len(f) != len(want) {
		t.Fatalf("%v", f)
	}
}

func TestKernelFacts_RefuseWhatTheEventCannotSay(t *testing.T) {
	good := func() *AccountingBridge {
		return bridgeFor(t, ResultPostingInput{Gross: kernelRat("1000"), Net: kernelRat("800"), ByKind: map[string]*big.Rat{KindEmployeeTax: kernelRat("200")}})
	}
	b := good()
	b.Lines = append(b.Lines, PostingLine{MappingKey: "SOMETHING_ELSE", Debit: "1.0000", Credit: "0.0000"})
	if _, err := b.KernelFacts(); err == nil {
		t.Fatal("a key with no place in a payroll result is an error, not dropped")
	}
	b = good()
	b.Lines = append(b.Lines, b.Lines[0])
	if _, err := b.KernelFacts(); err == nil {
		t.Fatal("a repeated key is an error")
	}
	b = bridgeFor(t, ResultPostingInput{Gross: kernelRat("1000"), Net: kernelRat("1000"), ByKind: map[string]*big.Rat{KindEmployerTax: kernelRat("50")}})
	for i := range b.Lines {
		if b.Lines[i].MappingKey == EmployerPayableKey(KindEmployerTax) {
			b.Lines[i].Credit = "49.0000"
		}
	}
	if _, err := b.KernelFacts(); err == nil {
		t.Fatal("an employer liability that differs from the employer cost cannot be one fact")
	}
}
