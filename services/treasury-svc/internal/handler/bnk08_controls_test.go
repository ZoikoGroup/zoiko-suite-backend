package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"zoiko.io/treasury-svc/internal/domain"
)

func seedCashAccount(s *mockStore, id string, balance *domain.Decimal) {
	s.bankAccounts[id] = &domain.BankAccount{BankAccountID: id, TenantID: "tenant-abc", LegalEntityID: "ent-1", CurrencyCode: "USD", AccountStatus: domain.BankAccountActive}
	if balance != nil {
		s.cashBalances[id] = &domain.CashBalance{BankAccountID: id, AvailableBalance: *balance, LedgerBalance: *balance, AsOfTimestamp: time.Now().UTC()}
	}
}

// A failed balance read fails the calculation closed. Before, the account
// was skipped and the snapshot understated cash with nothing to say so.
func TestHandler_CalculateCashPosition_BalanceReadFailureFailsClosed(t *testing.T) {
	s := newMockStore()
	hundred := domain.Decimal("100")
	seedCashAccount(s, "a1", &hundred)
	s.balErr = errors.New("connection reset")
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/cash-position/calculate",
		map[string]string{"legal_entity_id": "ent-1", "reporting_currency": "USD"}, "analyst-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("calculate with balances unreadable: %d %s, want 503", rr.Code, rr.Body.String())
	}
	if len(s.cashPositions) != 0 {
		t.Fatal("a snapshot was written without its bank balances")
	}
}

// An ACTIVE account with no balance makes the position partial, so the
// snapshot is stale and cannot be published as current.
func TestHandler_CalculateCashPosition_MissingBalanceIsPartial(t *testing.T) {
	s := newMockStore()
	hundred := domain.Decimal("100")
	seedCashAccount(s, "a1", &hundred)
	seedCashAccount(s, "a2", nil)
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/cash-position/calculate",
		map[string]string{"legal_entity_id": "ent-1", "reporting_currency": "USD"}, "analyst-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("calculate: %d %s", rr.Code, rr.Body.String())
	}
	var snap domain.CashPositionSnapshot
	_ = json.Unmarshal(rr.Body.Bytes(), &snap)
	if !snap.HasStaleComponent || snap.BankBalance != domain.Decimal("100") {
		t.Fatalf("position with one account's balance missing: stale=%v bank=%v, want stale and 100", snap.HasStaleComponent, snap.BankBalance)
	}
}

// The freshness and drill-down reads need view permission in the
// snapshot's legal entity, like every other cash-position read. Before,
// any principal in the tenant could read any entity's account balances.
func TestHandler_CashPositionSnapshotReads_RequirePermission(t *testing.T) {
	s := newMockStore()
	hundred := domain.Decimal("100")
	seedCashAccount(s, "a1", &hundred)
	allowed := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})
	rr := doTransferJSONRequest(t, allowed, http.MethodPost, "/v1/treasury/cash-position/calculate",
		map[string]string{"legal_entity_id": "ent-1", "reporting_currency": "USD"}, "analyst-1")
	var snap domain.CashPositionSnapshot
	_ = json.Unmarshal(rr.Body.Bytes(), &snap)

	authz := &mockAuthz{allowed: false}
	denied := newTransferRouter(s, authz, &mockTransferClients{})
	for _, path := range []string{"/v1/treasury/cash-position/" + snap.SnapshotID + "/freshness", "/v1/treasury/cash-position/" + snap.SnapshotID + "/drilldown"} {
		if rr := doTransferJSONRequest(t, denied, http.MethodGet, path, nil, "outsider"); rr.Code != http.StatusForbidden {
			t.Errorf("%s without permission: %d, want 403", path, rr.Code)
		}
		if len(authz.entities) == 0 || authz.entities[len(authz.entities)-1] != "ent-1" {
			t.Errorf("%s checked legal entities %v, want ent-1", path, authz.entities)
		}
		if rr := doTransferJSONRequest(t, allowed, http.MethodGet, path, nil, "analyst-1"); rr.Code != http.StatusOK {
			t.Errorf("%s with permission: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

// Refresh refuses a prior snapshot from another legal entity, and writes
// nothing when it does.
func TestHandler_RefreshCashPosition_PriorMustBeSameEntity(t *testing.T) {
	s := newMockStore()
	hundred := domain.Decimal("100")
	seedCashAccount(s, "a1", &hundred)
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})
	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/cash-position/calculate",
		map[string]string{"legal_entity_id": "ent-1", "reporting_currency": "USD"}, "analyst-1")
	var other domain.CashPositionSnapshot
	_ = json.Unmarshal(rr.Body.Bytes(), &other)

	rr = doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/cash-position/refresh",
		map[string]string{"legal_entity_id": "ent-2", "reporting_currency": "USD", "prior_snapshot_id": other.SnapshotID}, "analyst-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("refresh for ent-2 retiring ent-1's snapshot: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if len(s.cashPositions) != 1 || s.cashPositions[other.SnapshotID].Status != domain.CashPositionCalculated {
		t.Fatalf("the refused refresh changed state: %d snapshots, ent-1's now %s", len(s.cashPositions), s.cashPositions[other.SnapshotID].Status)
	}
}
