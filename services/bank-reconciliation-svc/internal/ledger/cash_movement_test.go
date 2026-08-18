package ledger_test

// Unit tests for the signed, per-account cash movement that replaced
// NetAmount. NetAmount summed one side of a balanced journal, so it was
// always positive and carried no direction at all — which is why comparing
// its magnitude to a statement line's magnitude let a payment out reconcile
// against a receipt in.

import (
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/ledger"
)

func journal(lines ...ledger.JournalLine) ledger.Journal {
	return ledger.Journal{JournalID: "j1", Status: "FINALIZED", Lines: lines}
}

func TestCashMovementCents_DebitToCashIsMoneyIn(t *testing.T) {
	j := journal(
		ledger.JournalLine{AccountCode: "1000", DebitAmount: 500},
		ledger.JournalLine{AccountCode: "4000", CreditAmount: 500},
	)
	got, touched := j.CashMovementCents("1000")
	if !touched {
		t.Fatal("journal posts to 1000 but was reported as not touching it")
	}
	if got != 50000 {
		t.Fatalf("got %d cents, want +50000 — a debit to the bank account is money IN", got)
	}
}

func TestCashMovementCents_CreditToCashIsMoneyOut(t *testing.T) {
	j := journal(
		ledger.JournalLine{AccountCode: "1000", CreditAmount: 500},
		ledger.JournalLine{AccountCode: "5000", DebitAmount: 500},
	)
	got, _ := j.CashMovementCents("1000")
	if got != -50000 {
		t.Fatalf("got %d cents, want -50000 — a credit to the bank account is money OUT", got)
	}
}

// The two directions must not produce the same number. This is the whole
// defect in one assertion: under the old magnitude comparison they did.
func TestCashMovementCents_DirectionsAreDistinguishable(t *testing.T) {
	in, _ := journal(ledger.JournalLine{AccountCode: "1000", DebitAmount: 500}).CashMovementCents("1000")
	out, _ := journal(ledger.JournalLine{AccountCode: "1000", CreditAmount: 500}).CashMovementCents("1000")
	if in == out {
		t.Fatalf("money in and money out both reported as %d — direction is not being distinguished", in)
	}
}

// Not touching an account is not the same as netting to zero on it: one means
// the journal has nothing to do with this bank account, the other that its
// movements cancel. A caller must be able to tell them apart.
func TestCashMovementCents_UntouchedAccountIsNotZeroMovement(t *testing.T) {
	j := journal(
		ledger.JournalLine{AccountCode: "5000", DebitAmount: 500},
		ledger.JournalLine{AccountCode: "4000", CreditAmount: 500},
	)
	if got, touched := j.CashMovementCents("1000"); touched {
		t.Fatalf("journal does not post to 1000 but reported touched with %d", got)
	}

	netsToZero := journal(
		ledger.JournalLine{AccountCode: "1000", DebitAmount: 500},
		ledger.JournalLine{AccountCode: "1000", CreditAmount: 500},
	)
	got, touched := netsToZero.CashMovementCents("1000")
	if !touched || got != 0 {
		t.Fatalf("got (%d, %v), want (0, true) for a journal whose movements on 1000 cancel", got, touched)
	}
}

// Amounts are NUMERIC(18,2) at both ends and float64 only in transit.
// 60.10 + 40.20 == 100.30 is false in binary floating point, and this
// comparison decides whether money is declared reconciled.
func TestCashMovementCents_SumsExactlyInCents(t *testing.T) {
	j := journal(
		ledger.JournalLine{AccountCode: "1000", DebitAmount: 60.10},
		ledger.JournalLine{AccountCode: "1000", DebitAmount: 40.20},
	)
	got, _ := j.CashMovementCents("1000")
	if got != ledger.ToCents(100.30) {
		t.Fatalf("got %d, want %d — cents must sum exactly", got, ledger.ToCents(100.30))
	}
}

func TestToCents_RoundsRatherThanTruncates(t *testing.T) {
	cases := map[float64]int64{
		12.34:   1234,
		0.1:     10,
		0.29:    29,
		-500.05: -50005,
		100.30:  10030,
	}
	for in, want := range cases {
		if got := ledger.ToCents(in); got != want {
			t.Errorf("ToCents(%v) = %d, want %d — truncation loses a cent", in, got, want)
		}
	}
}
