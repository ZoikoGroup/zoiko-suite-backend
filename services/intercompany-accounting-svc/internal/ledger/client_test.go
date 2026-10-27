package ledger_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// general-ledger-svc writes line amounts as JSON numbers. They must arrive
// with every digit, along with the journal's currency and period, which the
// match now depends on.
func TestGetJournal_DecodesCurrencyPeriodAndExactAmounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/journals/j-1" || r.Header.Get("X-Tenant-Id") != "tenant-1" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"journal_id":"j-1","legal_entity_id":"le-uk","status":"FINALIZED",
			"currency_code":"GBP","fiscal_period":"2026-09",
			"lines":[{"account_code":"1300","debit_amount":99999999999999.9999,"credit_amount":0},
			         {"account_code":"2300","debit_amount":0,"credit_amount":99999999999999.9999}]}`))
	}))
	defer srv.Close()

	j, err := ledger.NewClient(srv.URL, zap.NewNop()).GetJournal(context.Background(), "tenant-1", "j-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.CurrencyCode != "GBP" || j.FiscalPeriod != "2026-09" {
		t.Fatalf("currency/period not decoded: %+v", j)
	}
	if j.Lines[0].DebitAmount != domain.Decimal("99999999999999.9999") || j.Lines[1].CreditAmount != domain.Decimal("99999999999999.9999") {
		t.Fatalf("line amounts lost precision: %+v", j.Lines)
	}
}
