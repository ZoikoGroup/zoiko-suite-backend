package policy

import (
	"errors"
	"testing"

	"zoiko.io/eventing/catalogue"
)

func TestCatalogueACLRefusesAnotherServicesType(t *testing.T) {
	acl, err := CatalogueACL()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := catalogue.All()
	var owned catalogue.Entry
	for _, e := range all {
		if e.ServiceName() == "treasury-svc" {
			owned = e
			break
		}
	}
	if owned.Type == "" {
		t.Fatal("no treasury-svc entry found")
	}
	if err := acl.Check("treasury-svc", owned.Type); err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	if err := acl.Check("payment-status-svc", owned.Type); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a non-owner must be refused, got %v", err)
	}
}

func TestRolesAndAmbiguousProducersAreNotOwned(t *testing.T) {
	for _, p := range []string{"AR", "Legal", "Period Service", "Party Service", "Tax/E-Invoice"} {
		if s := (catalogue.Entry{Producer: p}).ServiceName(); s != "" {
			t.Errorf("%q must stay unowned, got %q", p, s)
		}
	}
	for p, want := range map[string]string{
		"Connectivity Api Bridge Service": "connectivity-api-bridge-svc",
		"Accounting Kernel":               "general-ledger-svc",
		"Payment Initiation Adapter":      "payment-initiation-adapter-svc",
		"Bank Reconciliation Service":     "bank-reconciliation-svc",
	} {
		if got := (catalogue.Entry{Producer: p}).ServiceName(); got != want {
			t.Errorf("%q -> %q, want %q", p, got, want)
		}
	}
}
