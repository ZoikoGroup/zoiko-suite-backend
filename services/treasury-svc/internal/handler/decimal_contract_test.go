package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/treasury-svc/internal/domain"
)

func seedTransferAccounts(s *mockStore) {
	s.bankAccounts["src-dec"] = &domain.BankAccount{
		BankAccountID: "src-dec", TenantID: "tenant-abc", LegalEntityID: "ent-123",
		CurrencyCode: "USD", AccountStatus: domain.BankAccountActive,
	}
	s.bankAccounts["tgt-dec"] = &domain.BankAccount{
		BankAccountID: "tgt-dec", TenantID: "tenant-abc", LegalEntityID: "ent-123",
		CurrencyCode: "USD", AccountStatus: domain.BankAccountActive,
	}
}

func transferBody(amount string) json.RawMessage {
	return json.RawMessage(`{"source_bank_account_id":"src-dec","target_bank_account_id":"tgt-dec","amount":` +
		amount + `,"currency_code":"USD","correlation_id":"corr-decimal"}`)
}

// A34: an amount with more than four decimal places would be rounded by
// NUMERIC(20,4), so it is refused at the edge instead of silently changed
// before the hash, threshold check and BNK-06 see it.
func TestHandler_CreateTreasuryTransfer_RefusesAmountTheColumnWouldRound(t *testing.T) {
	s := newMockStore()
	seedTransferAccounts(s)
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBody("10.00001"), "maker-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("5-decimal amount: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if len(s.transfers) != 0 {
		t.Fatal("a transfer was written with an amount the column cannot store exactly")
	}
}

// 0.3 - 0.1 is exactly 0.2 in decimal. A float64 implementation computes
// 0.19999999999999998 and blocks this transfer against a 0.2 threshold;
// the exact path must not.
func TestHandler_CreateTreasuryTransfer_ThresholdCheckIsExact(t *testing.T) {
	s := newMockStore()
	seedTransferAccounts(s)
	s.cashBalances["src-dec"] = &domain.CashBalance{
		BankAccountID: "src-dec", AvailableBalance: "0.3", LedgerBalance: "0.3", AsOfTimestamp: time.Now().UTC(),
	}
	s.thresholds["ent-123:USD"] = &domain.LiquidityThreshold{
		LegalEntityID: "ent-123", CurrencyCode: "USD", MinimumRequiredBalance: "0.2",
	}
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBody("0.1"), "maker-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("0.3 - 0.1 against a 0.2 threshold: %d %s, want 201", rr.Code, rr.Body.String())
	}
}

// The response keeps the old wire shape: amount is a bare JSON number, not
// a quoted decimal string, so existing callers decoding it as a number do
// not break.
func TestHandler_CreateTreasuryTransfer_ResponseAmountStaysAJSONNumber(t *testing.T) {
	s := newMockStore()
	seedTransferAccounts(s)
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBody("10.005"), "maker-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	dec := json.NewDecoder(rr.Body)
	dec.UseNumber()
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	num, ok := resp["amount"].(json.Number)
	if !ok || num.String() != "10.005" {
		t.Fatalf("amount = %#v (%T), want JSON number 10.005", resp["amount"], resp["amount"])
	}
}

// A rate has its own NUMERIC(20,8) scale. Eight places are accepted; a
// ninth is refused rather than rounded.
func TestHandler_RecordFXRate_RefusesRateTheColumnWouldRound(t *testing.T) {
	s := newMockStore()
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	ok := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/fx/rates",
		map[string]any{"currency_pair": "EUR/USD", "rate": "1.12345678"}, "treasurer-1")
	if ok.Code != http.StatusCreated {
		t.Fatalf("8-decimal rate: %d %s, want 201", ok.Code, ok.Body.String())
	}
	bad := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/fx/rates",
		map[string]any{"currency_pair": "EUR/USD", "rate": "1.123456789"}, "treasurer-1")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("9-decimal rate: %d %s, want 400", bad.Code, bad.Body.String())
	}
}
