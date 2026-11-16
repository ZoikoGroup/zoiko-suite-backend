package policy

import (
	"errors"
	"testing"

	"zoiko.io/eventing/envelope"
)

const posted = "com.zoikosuite.accounting.journal.posted"

func acl(t *testing.T, strict bool) *ProducerACL {
	t.Helper()
	a, err := NewProducerACL(map[string][]string{
		"general-ledger-svc":  {"com.zoikosuite.accounting.journal.*", "com.zoikosuite.accounting.account-mapping.*"},
		"financial-close-svc": {"com.zoikosuite.accounting.period.*"},
		"ar-svc":              {"com.zoikosuite.ar.customer-invoice.issued"},
	}, strict)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// EV-T23: two producers attempt to emit journal.posted; only the owner may.
func TestOnlyTheOwnerMayProduce_EV_T23(t *testing.T) {
	a := acl(t, false)
	if err := a.Check("general-ledger-svc", posted); err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	if err := a.Check("rogue-svc", posted); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a second producer must be refused, got %v", err)
	}
	if err := a.Check("ar-svc", "com.zoikosuite.ar.customer-invoice.issued"); err != nil {
		t.Fatal(err)
	}
	if err := a.Check("general-ledger-svc", "com.zoikosuite.ar.customer-invoice.issued"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("exact grant must be exclusive: %v", err)
	}
}

func TestLenientAllowsUnownedStrictDoesNot(t *testing.T) {
	if err := acl(t, false).Check("anyone", "com.zoikosuite.tax.tax-return.accepted"); err != nil {
		t.Fatalf("lenient mode must allow an unowned type: %v", err)
	}
	if err := acl(t, true).Check("anyone", "com.zoikosuite.tax.tax-return.accepted"); !errors.Is(err, ErrUnowned) {
		t.Fatalf("strict mode must refuse an unowned type: %v", err)
	}
	if err := acl(t, true).Check("general-ledger-svc", "com.zoikosuite.accounting.journal.invented"); !errors.Is(err, ErrNotCatalogued) {
		t.Fatalf("strict mode must refuse an uncatalogued type: %v", err)
	}
	if err := acl(t, true).Check("general-ledger-svc", posted); err != nil {
		t.Fatalf("owner of a catalogued type: %v", err)
	}
}

func TestInvalidTypeRefused(t *testing.T) {
	for _, bad := range []string{"", "journal.posted", "com.zoikosuite.accounting.journal.posted.v1"} {
		if err := acl(t, false).Check("general-ledger-svc", bad); !errors.Is(err, ErrInvalidType) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestOverlappingGrantsRefused(t *testing.T) {
	cases := map[string]map[string][]string{
		"same exact type":              {"a": {posted}, "b": {posted}},
		"exact under another's family": {"a": {"com.zoikosuite.accounting.journal.*"}, "b": {posted}},
		"nested families":              {"a": {"com.zoikosuite.accounting.journal.*"}, "b": {"com.zoikosuite.accounting.journal.*"}},
	}
	for name, g := range cases {
		if _, err := NewProducerACL(g, false); err == nil {
			t.Errorf("%s: overlapping ownership was accepted", name)
		}
	}
	if _, err := NewProducerACL(map[string][]string{"a": {"not-a-type"}}, false); err == nil {
		t.Error("malformed grant accepted")
	}
	if _, err := NewProducerACL(map[string][]string{"a": {"com.zoikosuite.*"}}, false); err == nil {
		t.Error("a family broader than domain.aggregate accepted")
	}
	// one service may hold an exact type inside its own family
	if _, err := NewProducerACL(map[string][]string{"a": {"com.zoikosuite.accounting.journal.*", posted}}, false); err != nil {
		t.Errorf("same-owner exact within family: %v", err)
	}
}

// ---- residency -------------------------------------------------------------

func res(t *testing.T, rules ...TransferRule) *Residency {
	t.Helper()
	r, err := NewResidency(rules)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// EV-T10: a restricted UK tenant event attempts a US subscription.
func TestRestrictedUKEventToUSIsDenied_EV_T10(t *testing.T) {
	d := res(t).Decide("us-consumer", posted, "t1", "uk", "us", envelope.Restricted)
	if d.Allowed || d.Reason == "" || d.EventRegion != "uk" || d.ConsumerRegion != "us" || d.Consumer != "us-consumer" || d.At.IsZero() {
		t.Fatalf("must deny with evidence: %+v", d)
	}
}

func TestSameRegionAllowed(t *testing.T) {
	if !res(t).Decide("c", posted, "t", "uk", "uk", envelope.Restricted).Allowed {
		t.Fatal("same-region delivery must be allowed")
	}
}

func TestTransferRuleIsDirectionalAndCapsClassification(t *testing.T) {
	r := res(t, TransferRule{From: "eu-west", To: "uk", MaxClassification: envelope.Confidential, Purpose: "group consolidation"})
	if d := r.Decide("c", posted, "t", "eu-west", "uk", envelope.Confidential); !d.Allowed || d.Purpose != "group consolidation" {
		t.Fatalf("permitted transfer refused: %+v", d)
	}
	if r.Decide("c", posted, "t", "eu-west", "uk", envelope.Internal).Allowed != true {
		t.Fatal("a lower classification is covered by the rule")
	}
	if r.Decide("c", posted, "t", "eu-west", "uk", envelope.Restricted).Allowed {
		t.Fatal("restricted exceeds the rule's cap")
	}
	if r.Decide("c", posted, "t", "uk", "eu-west", envelope.Public).Allowed {
		t.Fatal("rules are one-way")
	}
}

func TestResidencyFailsClosed(t *testing.T) {
	r := res(t)
	if r.Decide("c", posted, "t", "", "uk", envelope.Public).Allowed {
		t.Error("event with no region must be refused")
	}
	if r.Decide("c", posted, "t", "uk", "", envelope.Public).Allowed {
		t.Error("consumer with no region must be refused")
	}
	// An unknown classification is treated as restricted, so no rule below restricted covers it.
	rr := res(t, TransferRule{From: "uk", To: "us", MaxClassification: envelope.Confidential, Purpose: "p"})
	d := rr.Decide("c", posted, "t", "uk", "us", envelope.Classification("secret"))
	if d.Allowed || d.Classification != envelope.Restricted {
		t.Errorf("unknown classification must be treated as restricted: %+v", d)
	}
}

func TestInvalidRulesRefused(t *testing.T) {
	for name, rule := range map[string]TransferRule{
		"same region":    {From: "uk", To: "uk", MaxClassification: envelope.Public, Purpose: "p"},
		"no purpose":     {From: "uk", To: "us", MaxClassification: envelope.Public},
		"bad class":      {From: "uk", To: "us", MaxClassification: "x", Purpose: "p"},
		"missing region": {From: "", To: "us", MaxClassification: envelope.Public, Purpose: "p"},
	} {
		if _, err := NewResidency([]TransferRule{rule}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCheckSpec(t *testing.T) {
	a := acl(t, false)
	if err := a.CheckSpec(envelope.Spec{Service: "general-ledger-svc", Type: posted}); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckSpec(envelope.Spec{Service: "rogue-svc", Type: posted}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v", err)
	}
}
