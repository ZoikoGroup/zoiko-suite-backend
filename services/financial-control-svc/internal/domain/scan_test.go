package domain

import (
	"encoding/json"
	"testing"
)

func scanLogic(t *testing.T, s string) RuleLogic {
	t.Helper()
	l, err := ParseRuleLogic(json.RawMessage(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return l
}

func TestScanEveryRecordIsFinding(t *testing.T) {
	l := scanLogic(t, `{"kind":"EXCEPTION_SCAN","finding_category":"CLASSIFICATION","finding_reason":"DIRECT_CONTROL_POSTING","finding_assertion":"CLASSIFICATION"}`)
	out, err := ScanExceptions([]PopulationRecord{
		{RecordID: "b", Amount: "-12.50", Currency: "USD"},
		{RecordID: "a", Amount: "100", Currency: "USD"},
	}, l)
	if err != nil || len(out.Exceptions) != 2 {
		t.Fatalf("got %v %v", out.Exceptions, err)
	}
	if out.Exceptions[0].RecordIDs[0] != "a" || out.Exceptions[1].Exposure != "12.5" && out.Exceptions[1].Exposure != "12.50" {
		t.Fatalf("order/exposure wrong: %+v", out.Exceptions)
	}
}

func TestScanEmptyPopulationPasses(t *testing.T) {
	l := scanLogic(t, `{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA","finding_reason":"UNPOSTED_EVENT","finding_assertion":"COMPLETENESS"}`)
	out, _ := ScanExceptions(nil, l)
	if len(out.Exceptions) != 0 {
		t.Fatal("empty population must pass")
	}
}

func TestScanConditions(t *testing.T) {
	l := scanLogic(t, `{"kind":"EXCEPTION_SCAN","finding_category":"AUTHORIZATION","finding_reason":"REVIEW_GAP","finding_assertion":"AUTHORIZATION",
		"conditions":[{"attr":"review_gap","op":"nonempty","detail":"no independent approval"}]}`)
	out, _ := ScanExceptions([]PopulationRecord{
		{RecordID: "1", Amount: "5", Currency: "USD", Attributes: map[string]string{"review_gap": ""}},
		{RecordID: "2", Amount: "5", Currency: "USD", Attributes: map[string]string{"review_gap": "SELF_APPROVED"}},
		{RecordID: "3", Amount: "5", Currency: "USD"},
	}, l)
	if len(out.Exceptions) != 1 || out.Exceptions[0].RecordIDs[0] != "2" || out.Exceptions[0].Detail != "no independent approval" {
		t.Fatalf("got %+v", out.Exceptions)
	}
}

func TestScanRejectsBadLogic(t *testing.T) {
	for _, s := range []string{
		`{"kind":"EXCEPTION_SCAN"}`,
		`{"kind":"EXCEPTION_SCAN","finding_category":"BOGUS","finding_reason":"X_Y_Z","finding_assertion":"A"}`,
		`{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA","finding_reason":"lower","finding_assertion":"A"}`,
		`{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA","finding_reason":"X_Y_Z","finding_assertion":"A","conditions":[{"attr":"a","op":"eq"}]}`,
		`{"kind":"EXCEPTION_SCAN","finding_category":"LATE_DATA","finding_reason":"X_Y_Z","finding_assertion":"A","conditions":[{"attr":"a","op":"zz"}]}`,
	} {
		if _, err := ParseRuleLogic(json.RawMessage(s)); err == nil {
			t.Errorf("expected error for %s", s)
		}
	}
}
