package handler_test

// SPEC_DEVIATIONS A4: both sides of a pair, its period and dimensions, a
// currency-aware match and exact decimal amounts.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

func createBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"source_legal_entity_id": "le-us",
		"target_legal_entity_id": "le-uk",
		"source_journal_id":      "j-001",
		"amount":                 5000,
		"currency_code":          "USD",
	}
	for k, v := range overrides {
		body[k] = v
	}
	return body
}

func TestCreateEntry_SameCurrency_CounterpartyDefaultsToSourceSide(t *testing.T) {
	s := newStubStoreReal()
	rr := doReq(newRouter(s, &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/",
		createBody(map[string]any{"fiscal_period": "2026-09", "dimensions": map[string]string{"cost_centre": "CC-1"}}), "p-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.IntercompanyEntry
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.CounterpartyAmount != "5000" || got.CounterpartyCurrencyCode != "USD" {
		t.Fatalf("expected counterparty 5000 USD, got %s %s", got.CounterpartyAmount, got.CounterpartyCurrencyCode)
	}
	if got.FiscalPeriod == nil || *got.FiscalPeriod != "2026-09" || got.Dimensions["cost_centre"] != "CC-1" {
		t.Fatalf("period/dimensions not kept: %+v", got)
	}
}

func TestCreateEntry_CrossCurrency(t *testing.T) {
	s := newStubStoreReal()
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})

	// No counterparty amount: there is no FX rate to derive one from.
	rr := doReq(r, http.MethodPost, "/v1/intercompany/entries/",
		createBody(map[string]any{"counterparty_currency_code": "GBP"}), "p-1")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "counterparty_amount_required") {
		t.Fatalf("expected 400 counterparty_amount_required, got %d: %s", rr.Code, rr.Body.String())
	}

	// Stated as a decimal string, the form ZS-DATA-001 D06 prefers.
	rr = doReq(r, http.MethodPost, "/v1/intercompany/entries/",
		createBody(map[string]any{"counterparty_currency_code": "GBP", "counterparty_amount": "3950.1234"}), "p-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var got domain.IntercompanyEntry
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Amount != "5000" || got.CurrencyCode != "USD" || got.CounterpartyAmount != "3950.1234" || got.CounterpartyCurrencyCode != "GBP" {
		t.Fatalf("unexpected sides: %+v", got)
	}
}

func TestCreateEntry_RefusesAmountsAndCodesItCannotStoreExactly(t *testing.T) {
	cases := map[string]map[string]any{
		"invalid_amount":              {"amount": "5000.00001"},
		"invalid_currency":            {"currency_code": "usd"},
		"counterparty_amount_differs": {"counterparty_amount": 4999},
		"invalid_fiscal_period":       {"fiscal_period": "2026\n09"},
		"invalid_dimensions":          {"dimensions": map[string]string{"": "x"}},
	}
	for code, override := range cases {
		rr := doReq(newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/",
			createBody(override), "p-1")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), code) {
			t.Errorf("%s: expected 400 %s, got %d: %s", code, code, rr.Code, rr.Body.String())
		}
	}
	// A fraction is not an amount, even though big.Rat would parse it.
	rr := doReq(newRouter(newStubStoreReal(), &stubAuthZ{}, &stubLedger{}), http.MethodPost, "/v1/intercompany/entries/",
		createBody(map[string]any{"amount": "1/3"}), "p-1")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a fractional amount, got %d", rr.Code)
	}
}

func seedPair(s *stubStoreReal, amount domain.Decimal, cur string, cpAmount domain.Decimal, cpCur string) {
	s.entries["e1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "e1", TenantID: "tenant-abc",
		SourceLegalEntityID: "le-us", TargetLegalEntityID: "le-uk", SourceJournalID: "j-src",
		Amount: amount, CurrencyCode: cur, CounterpartyAmount: cpAmount, CounterpartyCurrencyCode: cpCur,
		Dimensions: domain.Dimensions{}, MatchStatus: domain.MatchStatusUnmatched,
	}
}

func journal(cur string, amount domain.Decimal) *stubLedger {
	return &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-tgt", LegalEntityID: "le-uk", Status: "FINALIZED", CurrencyCode: cur,
		Lines: []ledger.JournalLine{{AccountCode: "2300", CreditAmount: amount}},
	}}
}

func match(t *testing.T, s *stubStoreReal, leg *stubLedger) domain.MatchEntryResponse {
	t.Helper()
	rr := doReq(newRouter(s, &stubAuthZ{}, leg), http.MethodPost, "/v1/intercompany/entries/e1/match",
		map[string]any{"target_journal_id": "j-tgt"}, "p-1")
	var resp domain.MatchEntryResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	return resp
}

// The deviation's own example: a 5,000 GBP counterparty journal used to
// match a 5,000 USD pair, because the currency was never looked at.
func TestMatchEntry_CurrencyDiffers_RecordedAsMismatch(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	resp := match(t, s, journal("GBP", "5000"))
	if resp.MatchStatus != domain.MatchStatusMismatch || resp.MismatchReason == nil || !strings.HasPrefix(*resp.MismatchReason, "currency_mismatch:") {
		t.Fatalf("expected a currency_mismatch MISMATCH, got %+v", resp)
	}
}

// general-ledger-svc gave journals that predate its currency column "XXX".
// Their currency is unknown, so they cannot be matched.
func TestMatchEntry_JournalWithNoKnownCurrency_RecordedAsMismatch(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	resp := match(t, s, journal("XXX", "5000"))
	if resp.MatchStatus != domain.MatchStatusMismatch || resp.MismatchReason == nil || !strings.HasPrefix(*resp.MismatchReason, "currency_unverifiable:") {
		t.Fatalf("expected a currency_unverifiable MISMATCH, got %+v", resp)
	}
}

// A cross-currency pair is matched against its counterparty side.
func TestMatchEntry_CrossCurrencyPair_MatchesCounterpartySide(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "3950.1234", "GBP")
	if resp := match(t, s, journal("GBP", "3950.1234")); resp.MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED, got %+v", resp)
	}

	s = newStubStoreReal()
	seedPair(s, "5000", "USD", "3950.1234", "GBP")
	if resp := match(t, s, journal("GBP", "5000")); resp.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("a journal for the source amount must not match the counterparty side, got %+v", resp)
	}
}

// At the top of NUMERIC(18,4) float64 cannot tell these two amounts apart:
// its spacing there is 2^-6, and both round to 1e14, so the old float
// comparison saw a difference of 0 and matched. Compared exactly they
// differ by 0.0049, more than the 0.001 tolerance.
func TestMatchEntry_ComparesAmountsExactly(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "99999999999999.9999", "USD", "99999999999999.9999", "USD")
	if resp := match(t, s, journal("USD", "99999999999999.995")); resp.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH, got %+v", resp)
	}

	// Exactly at the tolerance still matches, as it did before.
	s = newStubStoreReal()
	seedPair(s, "100.001", "USD", "100.001", "USD")
	if resp := match(t, s, journal("USD", "100")); resp.MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED at the tolerance boundary, got %+v", resp)
	}
}
