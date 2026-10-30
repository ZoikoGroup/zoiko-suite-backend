package kernel

import (
	"math/big"
	"testing"
)

func TestCompareGroupsFindsADifferenceThatATotalWouldHide(t *testing.T) {
	a := map[string]*big.Rat{"VEHICLES": big.NewRat(2360000, 100), "EQUIPMENT": big.NewRat(590000, 100)}
	ledger := []GroupTotal{{Key: "EQUIPMENT", Debit: "11000.00", Credit: "300.00"}, {Key: "VEHICLES", Debit: "19000.00", Credit: "200.00"}}
	rows, worst := compareGroups(a, ledger, true)
	if len(rows) != 2 || rows[0]["key"] != "EQUIPMENT" || rows[0]["difference"] != "-4800.000000" || rows[1]["difference"] != "4800.000000" {
		t.Fatalf("%v", rows)
	}
	if worst.Cmp(big.NewRat(4800, 1)) != 0 {
		t.Fatalf("the worst group decides the case: %v", worst)
	}
}

func TestCompareGroupsCountsAGroupOnOneSideOnlyAgainstZero(t *testing.T) {
	a := map[string]*big.Rat{"WH-1": big.NewRat(100, 1)}
	ledger := []GroupTotal{{Key: "WH-1", Debit: "100.00", Credit: "0.00"}, {Key: "", Debit: "50.00", Credit: "0.00"}}
	rows, worst := compareGroups(a, ledger, true)
	if len(rows) != 2 || rows[0]["key"] != "(none)" || rows[0]["difference"] != "-50.000000" || worst.Cmp(big.NewRat(50, 1)) != 0 {
		t.Fatalf("entries with no value of the dimension are a group of their own: %v %v", rows, worst)
	}
}

func TestCreditNormalGroupsAreCompareOnTheCreditSide(t *testing.T) {
	a := map[string]*big.Rat{"X": big.NewRat(100, 1)}
	rows, worst := compareGroups(a, []GroupTotal{{Key: "X", Debit: "20.00", Credit: "120.00"}}, false)
	if rows[0]["actual"] != "100.000000" || worst.Sign() != 0 {
		t.Fatalf("%v", rows)
	}
}

func TestEveryReconciliationTypeHasItsControl(t *testing.T) {
	for _, typ := range ReconTypes {
		if ownSided(typ) {
			continue
		}
		if _, ok := controlOf[typ]; !ok {
			t.Fatalf("%s has no control type", typ)
		}
	}
	if controlOf[ReconPayroll].DebitNormal || controlOf[ReconTax].DebitNormal || !controlOf[ReconAsset].DebitNormal || !controlOf[ReconInventory].DebitNormal {
		t.Fatal("liabilities are credit-normal, the asset register and inventory debit-normal")
	}
}
