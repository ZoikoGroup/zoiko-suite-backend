package clients_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

// FetchJournal reads general-ledger-svc's GET /v1/journals/{id} header and
// lines, which is what lets a run check a leg's entity, status and period.
func TestFetchJournal_HeaderAndLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/journals/j-1" || r.Header.Get("X-Tenant-Id") != "tenant-1" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"journal_id":"j-1","tenant_id":"tenant-1","legal_entity_id":"le-us","fiscal_period":"2026-07",
			"status":"FINALIZED","journal_type":"STANDARD","lines":[
			{"journal_line_id":"l-1","line_number":1,"account_code":"1500-IC-REC","debit_amount":500,"credit_amount":0},
			{"journal_line_id":"l-2","line_number":2,"account_code":"4000-IC-REV","debit_amount":0,"credit_amount":500}]}`))
	}))
	t.Cleanup(srv.Close)
	c := clients.New(srv.URL, "", zap.NewNop())

	j, err := c.FetchJournal(context.Background(), "tenant-1", "j-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.LegalEntityID != "le-us" || j.FiscalPeriod != "2026-07" || j.Status != "FINALIZED" || len(j.Lines) != 2 ||
		j.Lines[1].LineNumber != 2 || j.Lines[1].AccountCode != "4000-IC-REV" || j.Lines[1].CreditAmount != 500 {
		t.Fatalf("journal: %+v", j)
	}
	if _, err := c.FetchJournal(context.Background(), "tenant-1", "j-missing"); !errors.Is(err, domain.ErrGLServiceUnavailable) {
		t.Fatalf("missing journal: %v", err)
	}
}

// The pair's period and counterparty side decode as intercompany-accounting-svc
// serves them; a pre-000005 pair has no fiscal_period.
func TestFetchEliminableIntercompanyEntries_PeriodAndCounterparty(t *testing.T) {
	srv := intercompanyServer(t, `[
		{"intercompany_entry_id":"ic-1","match_status":"MATCHED","amount":500,"currency_code":"USD",
		 "counterparty_amount":400.25,"counterparty_currency_code":"GBP","fiscal_period":"2026-07",
		 "source_legal_entity_id":"le-us","target_legal_entity_id":"le-uk","source_journal_id":"s1","target_journal_id":"t1"},
		{"intercompany_entry_id":"ic-2","match_status":"CLOSED","amount":10,"counterparty_amount":10,"source_journal_id":"s2","target_journal_id":"t2"}
	]`)
	got, err := clients.New("", srv.URL, zap.NewNop()).FetchEliminableIntercompanyEntries(context.Background(), "tenant-1", "principal-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].FiscalPeriod == nil || *got[0].FiscalPeriod != "2026-07" || got[0].CounterpartyAmount != "400.25" ||
		got[0].CounterpartyCurrencyCode != "GBP" || got[0].CurrencyCode != "USD" {
		t.Fatalf("ic-1: %+v", got[0])
	}
	if got[1].FiscalPeriod != nil {
		t.Fatalf("ic-2 should carry no period: %+v", got[1])
	}
}
