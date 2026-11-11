package catalogue

import (
	"strings"
	"testing"

	"zoiko.io/eventing/envelope"
)

// The §19 inventory has 16 family tables and 196 event types.
func TestCatalogueIsComplete(t *testing.T) {
	all, err := Spec()
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

// Extensions are real services' facts; they must lint like any other entry and
// be distinguishable from the section 19 inventory.
func TestExtensionsAreMarkedAndOwned(t *testing.T) {
	all, _ := All()
	spec, _ := Spec()
	ext := len(all) - len(spec)
	if ext != 248 {
		t.Fatalf("expected 248 extension entries, got %d", ext)
	}
	for _, e := range all {
		if e.Origin != "" && e.Origin != "extension" {
			t.Errorf("%s: unknown origin %q", e.Type, e.Origin)
		}
	}
}

// The naming-debt list may only shrink: every entry must still be catalogued as an
// extension and still fail the past-tense rule. Fix the name, then delete the line.
func TestNamingDebtIsReal(t *testing.T) {
	for typ := range extensionNamingDebt {
		e, ok := Lookup(typ)
		if !ok || e.Origin != "extension" {
			t.Errorf("%s is in extensionNamingDebt but is not a catalogued extension: remove it", typ)
			continue
		}
		if pastTense.MatchString(e.Fact()) {
			t.Errorf("%s now conforms to the past-tense rule: remove it from extensionNamingDebt", typ)
		}
	}
}
