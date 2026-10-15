package domain

import "testing"

func TestNext_LegalTransitions(t *testing.T) {
	cases := []struct {
		cmd  Command
		from State
		want State
	}{
		{CmdRefreshDraft, StateDraft, StateDraft},
		{CmdValidate, StateDraft, StateValidated},
		{CmdSubmitForReview, StateValidated, StateInReview},
		{CmdRequestChanges, StateInReview, StateDraft},
		{CmdApprove, StateInReview, StateApproved},
		{CmdCertify, StateApproved, StateCertified},
		{CmdPublish, StateCertified, StatePublished},
	}
	for _, c := range cases {
		got, err := Next(c.cmd, c.from)
		if err != nil {
			t.Fatalf("Next(%s, %s): unexpected error: %v", c.cmd, c.from, err)
		}
		if got != c.want {
			t.Fatalf("Next(%s, %s) = %s, want %s", c.cmd, c.from, got, c.want)
		}
	}
}

func TestNext_IllegalTransitions_AreInvalid(t *testing.T) {
	cases := []struct {
		cmd  Command
		from State
	}{
		{CmdValidate, StateInReview},
		{CmdValidate, StateCertified},
		{CmdSubmitForReview, StateDraft},
		{CmdApprove, StateDraft},
		{CmdApprove, StateApproved},
		{CmdCertify, StateInReview},
		{CmdCertify, StateCertified}, // no double-certify
		{CmdPublish, StateApproved},
		{CmdPublish, StatePublished}, // no double-publish
		{CmdRequestChanges, StateDraft},
		{CmdRefreshDraft, StateCertified},
	}
	for _, c := range cases {
		if _, err := Next(c.cmd, c.from); err == nil {
			t.Fatalf("Next(%s, %s): expected INVALID_TRANSITION, got nil error", c.cmd, c.from)
		} else if de, ok := AsError(err); !ok || de.Code != CodeInvalidTransition {
			t.Fatalf("Next(%s, %s): expected CodeInvalidTransition, got %v", c.cmd, c.from, err)
		}
	}
}

func TestNext_UnknownState(t *testing.T) {
	if _, err := Next(CmdValidate, State("BOGUS")); err == nil {
		t.Fatal("expected an error for an unknown state")
	}
}

func TestCanSupersede(t *testing.T) {
	for _, s := range []State{StateCertified, StatePublished} {
		if !CanSupersede(s) {
			t.Fatalf("CanSupersede(%s) = false, want true", s)
		}
	}
	for _, s := range []State{StateDraft, StateValidated, StateInReview, StateApproved, StateSuperseded} {
		if CanSupersede(s) {
			t.Fatalf("CanSupersede(%s) = true, want false", s)
		}
	}
}

func TestSnapshotContent_Validate(t *testing.T) {
	good := SnapshotContent{
		Currency: "GBP",
		Accounts: []SnapshotContentAccount{
			{AccountCode: "1000", AccountName: "Cash", Category: CategoryAsset, Balance: "100.00"},
		},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected valid content, got %v", err)
	}

	cases := []SnapshotContent{
		{Currency: "", Accounts: good.Accounts}, // missing currency
		{Currency: "GBP", Accounts: nil},        // no accounts
		{Currency: "GBP", Accounts: []SnapshotContentAccount{{AccountName: "x", Category: CategoryAsset, Balance: "1"}}}, // missing code
		{Currency: "GBP", Accounts: []SnapshotContentAccount{
			{AccountCode: "1000", Category: CategoryAsset, Balance: "1"},
			{AccountCode: "1000", Category: CategoryAsset, Balance: "2"},
		}}, // duplicate code
		{Currency: "GBP", Accounts: []SnapshotContentAccount{{AccountCode: "1000", Category: "BOGUS", Balance: "1"}}},                  // bad category
		{Currency: "GBP", Accounts: []SnapshotContentAccount{{AccountCode: "1000", Category: CategoryAsset, Balance: "not-a-number"}}}, // bad balance
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d: expected a validation error, got nil", i)
		}
	}
}

func TestBuildFacts_MapsCategoriesToStatements(t *testing.T) {
	content := SnapshotContent{
		Currency: "GBP",
		Accounts: []SnapshotContentAccount{
			{AccountCode: "1000", AccountName: "Cash", Category: CategoryAsset, Balance: "500.00"},
			{AccountCode: "2000", AccountName: "Payables", Category: CategoryLiability, Balance: "100.00"},
			{AccountCode: "3000", AccountName: "Share Capital", Category: CategoryEquity, Balance: "400.00"},
			{AccountCode: "4000", AccountName: "Sales", Category: CategoryRevenue, Balance: "1000.00"},
			{AccountCode: "5000", AccountName: "COGS", Category: CategoryExpense, Balance: "600.00"},
		},
	}
	// Every name is distinct, so both mapping versions produce one fact per
	// account here; only the statement placement is under test.
	for _, version := range []string{MappingVersionV1, MappingVersionV2} {
		facts, err := BuildFacts(content, version)
		if err != nil {
			t.Fatalf("%s: BuildFacts: %v", version, err)
		}
		if len(facts) != 5 {
			t.Fatalf("%s: expected 5 facts, got %d", version, len(facts))
		}
		byCode := make(map[string]StatementFact, len(facts))
		for _, f := range facts {
			byCode[f.AccountCode] = f
		}
		if byCode["1000"].StatementType != StatementBalanceSheet {
			t.Errorf("%s: asset account should map to balance sheet", version)
		}
		if byCode["4000"].StatementType != StatementIncomeStatement {
			t.Errorf("%s: revenue account should map to income statement", version)
		}
	}
}

func TestBuildFacts_RejectsInvalidContent(t *testing.T) {
	if _, err := BuildFacts(SnapshotContent{}, MappingVersionCurrent); err == nil {
		t.Fatal("expected an error for empty content")
	}
}

func TestValidateTieOut_Balances(t *testing.T) {
	facts := []StatementFact{
		{Category: CategoryAsset, Amount: "500"},
		{Category: CategoryLiability, Amount: "100"},
		{Category: CategoryEquity, Amount: "400"},
		{Category: CategoryRevenue, Amount: "1000"}, // ignored by the BS tie-out
	}
	errs, err := ValidateTieOut(facts)
	if err != nil {
		t.Fatalf("ValidateTieOut: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected a balanced sheet, got errors: %v", errs)
	}
}

func TestValidateTieOut_DetectsImbalance(t *testing.T) {
	facts := []StatementFact{
		{Category: CategoryAsset, Amount: "500"},
		{Category: CategoryLiability, Amount: "100"},
		{Category: CategoryEquity, Amount: "300"}, // short by 100
	}
	errs, err := ValidateTieOut(facts)
	if err != nil {
		t.Fatalf("ValidateTieOut: %v", err)
	}
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 validation error, got %d: %v", len(errs), errs)
	}
}

func TestValidateTieOut_EmptyCategoryIsLegitimateZero(t *testing.T) {
	// No liabilities or equity at all: assets must then be zero to tie out.
	facts := []StatementFact{
		{Category: CategoryAsset, Amount: "0"},
	}
	errs, err := ValidateTieOut(facts)
	if err != nil {
		t.Fatalf("ValidateTieOut: %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("expected tie-out to hold with all-zero facts, got: %v", errs)
	}
}

func TestParseAndCanonicalAmount(t *testing.T) {
	r, err := ParseAmount("7.000000000000")
	if err != nil {
		t.Fatalf("ParseAmount: %v", err)
	}
	if got := CanonicalAmount(r); got != "7" {
		t.Fatalf("CanonicalAmount(7.000...) = %q, want \"7\"", got)
	}
	if _, err := ParseAmount("not-a-number"); err == nil {
		t.Fatal("expected an error for a non-numeric amount")
	}
	if _, err := ParseAmount(""); err == nil {
		t.Fatal("expected an error for an empty amount")
	}
}
