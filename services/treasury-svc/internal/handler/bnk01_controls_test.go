package handler_test

import (
	"net/http"
	"testing"

	"zoiko.io/treasury-svc/internal/domain"
)

// The account list is per legal entity: without one there is nothing to
// authorize against. Before, it skipped the permission check and returned
// every account in the tenant.
func TestHandler_ListBankAccounts_RequiresLegalEntity(t *testing.T) {
	s := newMockStore()
	s.bankAccounts["a1"] = &domain.BankAccount{BankAccountID: "a1", TenantID: "tenant-abc", LegalEntityID: "ent-1", MaskedAccountNumber: "****1234", AccountStatus: domain.BankAccountActive}
	allowed := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})
	denied := newTransferRouter(s, &mockAuthz{allowed: false}, &mockTransferClients{})

	if rr := doTransferJSONRequest(t, denied, http.MethodGet, "/v1/treasury/accounts", nil, "usr-1"); rr.Code != http.StatusBadRequest {
		t.Fatalf("tenant-wide account list: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if rr := doTransferJSONRequest(t, denied, http.MethodGet, "/v1/treasury/accounts?legal_entity_id=ent-1", nil, "usr-1"); rr.Code != http.StatusForbidden {
		t.Fatalf("list without permission: %d, want 403", rr.Code)
	}
	if rr := doTransferJSONRequest(t, allowed, http.MethodGet, "/v1/treasury/accounts?legal_entity_id=ent-1", nil, "usr-1"); rr.Code != http.StatusOK {
		t.Fatalf("list with permission: %d %s", rr.Code, rr.Body.String())
	}
}

// bank_identifier changes only through rotate-token; amend refuses to
// change it and leaves fields it was not given alone.
func TestHandler_AmendBankAccountMetadata_IdentifierNeedsRotation(t *testing.T) {
	s := newMockStore()
	s.bankAccounts["a1"] = &domain.BankAccount{
		BankAccountID: "a1", TenantID: "tenant-abc", LegalEntityID: "ent-1", AccountName: "Old", BranchRef: "BR-1",
		BankIdentifier: "SWIFT-OLD", Country: "GB", AccountStatus: domain.BankAccountActive,
	}
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/accounts/a1/amend-metadata", map[string]string{"bank_identifier": "SWIFT-NEW"}, "ops-1")
	if rr.Code != http.StatusBadRequest || s.bankAccounts["a1"].BankIdentifier != "SWIFT-OLD" {
		t.Fatalf("amend changing bank_identifier: %d %s, identifier now %q", rr.Code, rr.Body.String(), s.bankAccounts["a1"].BankIdentifier)
	}

	rr = doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/accounts/a1/amend-metadata", map[string]string{"account_name": "New", "bank_identifier": "SWIFT-OLD"}, "ops-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("amend resending the same identifier: %d %s", rr.Code, rr.Body.String())
	}
	if a := s.bankAccounts["a1"]; a.AccountName != "New" || a.BranchRef != "BR-1" || a.Country != "GB" {
		t.Fatalf("amend changed fields it was not given: %+v", a)
	}
}
