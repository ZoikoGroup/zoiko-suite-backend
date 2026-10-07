package banking

import (
	"testing"
	"time"

	"zoiko.io/contract/types"
)

func TestMaskSensitiveIdentifier(t *testing.T) {
	rawAccount := "123456789012"
	masked := MaskSensitiveIdentifier(rawAccount)
	expected := "********9012"
	if masked != expected {
		t.Fatalf("expected %s, got %s", expected, masked)
	}

	short := "123"
	if MaskSensitiveIdentifier(short) != "****" {
		t.Fatalf("expected **** for short input")
	}
}

func TestBankStatement_ArithmeticValidation(t *testing.T) {
	statement := BankStatement{
		BankStatementID:  types.MustNewV7(),
		TenantID:         types.MustNewV7(),
		BankConnectionID: types.MustNewV7(),
		OpeningBalance:   types.MustParseMoney("1000.00"),
		ClosingBalance:   types.MustParseMoney("1350.25"),
		CurrencyCode:     "USD",
		Transactions: []BankTransaction{
			{
				BankTransactionID: types.MustNewV7(),
				Amount:            types.MustParseMoney("500.50"), // Deposit (+500.50)
			},
			{
				BankTransactionID: types.MustNewV7(),
				Amount:            types.MustParseMoney("-150.25"), // Withdrawal (-150.25)
			},
		},
	}

	// 1000.00 + 500.50 - 150.25 = 1350.25 -> Should balance
	if err := statement.ValidateArithmetic(); err != nil {
		t.Fatalf("expected valid statement arithmetic, got: %v", err)
	}

	// Corrupt closing balance -> Should fail
	corrupted := statement
	corrupted.ClosingBalance = types.MustParseMoney("1350.00")
	if err := corrupted.ValidateArithmetic(); err == nil {
		t.Fatalf("expected mismatch error for corrupted closing balance")
	}
}

func TestBankReconciliationMatch_Validation(t *testing.T) {
	match := BankReconciliationMatch{
		MatchID:           types.MustNewV7(),
		TenantID:          types.MustNewV7(),
		BankTransactionID: types.MustNewV7(),
		MatchedObjectType: "journal_entry",
		MatchedObjectID:   types.MustNewV7(),
		MatchedAmount:     types.MustParseMoney("500.50"),
		DifferenceAmount:  types.ZeroMoney(),
		MatchedAt:         time.Now().UTC(),
		MatchedBy:         "AUTO_MATCHER",
	}

	if err := match.Validate(); err != nil {
		t.Fatalf("expected valid match, got error: %v", err)
	}
}
