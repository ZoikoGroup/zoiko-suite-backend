package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
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

func transferBodyCurrency(amount, currencyCode string) json.RawMessage {
	return json.RawMessage(`{"source_bank_account_id":"src-dec","target_bank_account_id":"tgt-dec","amount":` +
		amount + `,"currency_code":"` + currencyCode + `","correlation_id":"corr-decimal-ccy"}`)
}

// newTransferRouterWithCurrency is newTransferRouter plus a caller-supplied
// CurrencyClient, for the real-currency-precision tests below that need
// more than one currency's answer in the same test.
func newTransferRouterWithCurrency(s *mockStore, authz *mockAuthz, tc handler.TransferClients, cur handler.CurrencyClient) chi.Router {
	h := handler.New(s, authz, &mockClients{}, tc, nil, zap.NewNop(), cur)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

func seedTransferAccountsCurrency(s *mockStore, currencyCode string) {
	s.bankAccounts["src-dec"] = &domain.BankAccount{
		BankAccountID: "src-dec", TenantID: "tenant-abc", LegalEntityID: "ent-123",
		CurrencyCode: currencyCode, AccountStatus: domain.BankAccountActive,
	}
	s.bankAccounts["tgt-dec"] = &domain.BankAccount{
		BankAccountID: "tgt-dec", TenantID: "tenant-abc", LegalEntityID: "ent-123",
		CurrencyCode: currencyCode, AccountStatus: domain.BankAccountActive,
	}
}

// A real currency's decimal places come from its own ISO 4217 minor-unit
// count (JPY=0, USD/GBP=2, BHD=3), resolved from currency-registry-svc —
// never a single hardcoded number applied to every currency. These three
// cases are the proof: JPY refuses any fractional amount, BHD accepts a
// 3rd decimal place a flat "2 decimals" rule would have refused, and
// USD/GBP (the common case) is unaffected.
func TestHandler_CreateTreasuryTransfer_RefusesAmountExceedingCurrencyPrecision_JPY(t *testing.T) {
	s := newMockStore()
	seedTransferAccountsCurrency(s, "JPY")
	r := newTransferRouterWithCurrency(s, &mockAuthz{allowed: true}, &mockTransferClients{}, realISOMinorUnits())

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBodyCurrency("100.5", "JPY"), "maker-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("JPY amount with a fractional part: %d %s, want 400", rr.Code, rr.Body.String())
	}
	if len(s.transfers) != 0 {
		t.Fatal("a JPY transfer was written with a fractional amount JPY does not allow")
	}
}

func TestHandler_CreateTreasuryTransfer_AcceptsAmountAtCurrencyPrecision_BHD(t *testing.T) {
	s := newMockStore()
	seedTransferAccountsCurrency(s, "BHD")
	r := newTransferRouterWithCurrency(s, &mockAuthz{allowed: true}, &mockTransferClients{}, realISOMinorUnits())

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBodyCurrency("100.123", "BHD"), "maker-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("BHD amount with 3 decimal places (BHD's real minor-unit count): %d %s, want 201", rr.Code, rr.Body.String())
	}
}

func TestHandler_CreateTreasuryTransfer_RefusesAmountExceedingCurrencyPrecision_USD(t *testing.T) {
	s := newMockStore()
	seedTransferAccountsCurrency(s, "USD")
	r := newTransferRouterWithCurrency(s, &mockAuthz{allowed: true}, &mockTransferClients{}, realISOMinorUnits())

	// 3 decimal places fits NUMERIC(20,4) but exceeds USD's real 2-decimal
	// minor-unit count.
	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBodyCurrency("10.005", "USD"), "maker-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("USD amount with 3 decimal places: %d %s, want 400", rr.Code, rr.Body.String())
	}
}

func TestHandler_CreateTreasuryTransfer_AcceptsOrdinaryUSDAmount_NoRegression(t *testing.T) {
	s := newMockStore()
	seedTransferAccountsCurrency(s, "USD")
	r := newTransferRouterWithCurrency(s, &mockAuthz{allowed: true}, &mockTransferClients{}, realISOMinorUnits())

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBodyCurrency("10.01", "USD"), "maker-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("ordinary 2-decimal USD amount: %d %s, want 201 (no regression)", rr.Code, rr.Body.String())
	}
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
//
// 10.01, not 10.005: this test's fixture used to carry a 3rd decimal place
// on a USD amount, which this currency-aware validation now correctly
// refuses (USD's real ISO 4217 minor-unit count is 2) — see
// TestHandler_CreateTreasuryTransfer_RefusesAmountExceedingCurrencyPrecision
// below for that behavior. This test is only about the JSON wire shape, so
// it uses a USD-valid 2-decimal amount instead.
func TestHandler_CreateTreasuryTransfer_ResponseAmountStaysAJSONNumber(t *testing.T) {
	s := newMockStore()
	seedTransferAccounts(s)
	r := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers", transferBody("10.01"), "maker-1")
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
	if !ok || num.String() != "10.01" {
		t.Fatalf("amount = %#v (%T), want JSON number 10.01", resp["amount"], resp["amount"])
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
