package catalogue

import (
	"strings"
	"testing"

	"zoiko.io/eventing/envelope"
)

// The §19 inventory has 16 family tables and 196 event types.
func TestCatalogueIsComplete(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 196 {
		t.Fatalf("expected the 196 types of ZS-EVENT-001 §19, got %d", len(all))
	}
	fam := map[string]bool{}
	for _, e := range all {
		fam[e.Family] = true
	}
	if len(fam) != 16 {
		t.Fatalf("expected 16 families, got %d", len(fam))
	}
}

// §21 semantic lint: every catalogued type passes it.
func TestEveryEntryPassesSemanticLint(t *testing.T) {
	all, _ := All()
	for _, e := range all {
		if v := Lint(e); len(v) > 0 {
			t.Errorf("%s: %s", e.Type, strings.Join(v, "; "))
		}
	}
}

// The catalogue must agree with the envelope's own type check, so a type that
// is catalogued can always be put in an envelope.
func TestEveryTypeIsAcceptedByEnvelope(t *testing.T) {
	all, _ := All()
	for _, e := range all {
		if !envelope.ValidType(e.Type) {
			t.Errorf("%s is rejected by envelope.New", e.Type)
		}
	}
}

func TestLintRejectsBadEntries(t *testing.T) {
	ok := Entry{Type: "com.zoikosuite.ar.customer-invoice.issued", Family: "f", Producer: "AR", Subject: "s", Consumers: "c"}
	cases := map[string]func(Entry) Entry{
		"generic verb":   func(e Entry) Entry { e.Type = "com.zoikosuite.ar.customer-invoice.updated"; return e },
		"present tense":  func(e Entry) Entry { e.Type = "com.zoikosuite.ar.customer-invoice.issue"; return e },
		"version suffix": func(e Entry) Entry { e.Type = "com.zoikosuite.ar.customer-invoice.issued.v1"; return e },
		"unknown domain": func(e Entry) Entry { e.Type = "com.zoikosuite.crm.lead.created"; return e },
		"no prefix":      func(e Entry) Entry { e.Type = "invoice.issued"; return e },
		"no producer":    func(e Entry) Entry { e.Producer = ""; return e },
		"no consumers":   func(e Entry) Entry { e.Consumers = " "; return e },
	}
	if v := Lint(ok); len(v) != 0 {
		t.Fatalf("baseline should pass: %v", v)
	}
	for name, mut := range cases {
		if len(Lint(mut(ok))) == 0 {
			t.Errorf("%s: lint accepted it", name)
		}
	}
}

func TestLookup(t *testing.T) {
	e, ok := Lookup("com.zoikosuite.accounting.journal.posted")
	if !ok || e.Producer != "Accounting Kernel" || e.Domain() != "accounting" || e.Aggregate() != "journal" || e.Fact() != "posted" {
		t.Fatalf("bad lookup: %+v ok=%v", e, ok)
	}
	if _, ok := Lookup("com.zoikosuite.accounting.journal.edited"); ok {
		t.Fatal("uncatalogued type must not resolve")
	}
}
