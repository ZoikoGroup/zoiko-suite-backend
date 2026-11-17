package policytype

import (
	"errors"
	"testing"
)

// PD-01/PD-03: only APPROVAL_THRESHOLD and SIGNATORY_MATRIX are decided here;
// spend, SoD and legal basis belong to their owning services.
func TestCatalogue_OwnersAndModes(t *testing.T) {
	want := map[string][2]string{
		ApprovalThreshold: {OwnerPolicy, ModeEvaluatedHere},
		SignatoryMatrix:   {OwnerPolicy, ModeResolvedHere},
		SpendControl:      {OwnerSpendControls, ModeReferenceOnly},
		SoDRule:           {OwnerAuthorization, ModeReferenceOnly},
		LegalBasis:        {OwnerPrivacy, ModeReferenceOnly},
		AccountingPolicy:  {OwnerPolicy, ModeResolvedHere},
	}
	if len(All()) != len(want) {
		t.Fatalf("catalogue has %d families", len(All()))
	}
	for typ, w := range want {
		i, ok := Lookup(typ)
		if !ok || i.OwningService != w[0] || i.Mode != w[1] || i.DecisionRoute == "" {
			t.Fatalf("%s: %+v", typ, i)
		}
		if i.EvaluableHere != (typ == ApprovalThreshold || typ == SignatoryMatrix) {
			t.Fatalf("%s evaluable_here=%v", typ, i.EvaluableHere)
		}
		if ReferenceOnly(typ) != (w[1] == ModeReferenceOnly) {
			t.Fatalf("%s reference-only mismatch", typ)
		}
	}
	if ReferenceOnly("UNKNOWN") {
		t.Fatal("an unknown type is not a reference family")
	}
}

func TestValidateReference(t *testing.T) {
	ok := map[string]string{
		SpendControl: `{"rule_set_ref":"spend-controls:policy/MKT","rule_set_version":"3","jurisdiction":"GB"}`,
		SoDRule:      `{"rule_set_ref":"authorization:sod/default","rule_set_version":"12","owning_service":"authorization-svc"}`,
		LegalBasis:   `{"jurisdiction":"US-CA","rule_reference":"CAN-SPAM 15 U.S.C. 7704"}`,
	}
	for typ, p := range ok {
		if err := ValidateReference(typ, []byte(p)); err != nil {
			t.Fatalf("%s refused: %v", typ, err)
		}
	}
	bad := []struct{ typ, payload string }{
		{SpendControl, ``},
		{SpendControl, `[]`},
		{SpendControl, `{"rule_set_ref":"x"}`},
		{SpendControl, `{"rule_set_ref":"x","rule_set_version":"1","effect":"DENY"}`},
		{SpendControl, `{"rule_set_ref":"x","rule_set_version":"1","owning_service":"policy-svc"}`},
		{SoDRule, `{"rule_set_ref":"x","rule_set_version":"1","jurisdiction":"Great Britain"}`},
		{SoDRule, `{"rule_set_ref":"<script>","rule_set_version":"1"}`},
		{LegalBasis, `{"rule_reference":"GDPR 6(1)(a)"}`},
		{LegalBasis, `{"jurisdiction":"GLOBAL","rule_reference":"x"}`},
		{LegalBasis, `{"jurisdiction":"GB"}`},
		{LegalBasis, `{"jurisdiction":"GB","rule_reference":"x","outcome":"PERMIT"}`},
	}
	for _, c := range bad {
		if err := ValidateReference(c.typ, []byte(c.payload)); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("%s %s accepted (%v)", c.typ, c.payload, err)
		}
	}
	// Families decided here are not reference families.
	if err := ValidateReference(ApprovalThreshold, nil); err != nil {
		t.Fatal(err)
	}
}
