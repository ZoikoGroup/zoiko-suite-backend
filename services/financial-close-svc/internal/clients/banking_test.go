package clients_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

func TestListBankAccounts_RequestAndEntityFilter(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		// A treasury that (wrongly) also returned another entity's account.
		_, _ = w.Write([]byte(`[
			{"bank_account_id":"a-1","legal_entity_id":"le-1","account_name":"Operating","masked_account_number":"****1","account_status":"ACTIVE","created_at":"2024-01-01T00:00:00Z"},
			{"bank_account_id":"a-2","legal_entity_id":"le-OTHER","account_name":"Not ours","account_status":"ACTIVE","created_at":"2024-01-01T00:00:00Z"}]`))
	}))
	defer srv.Close()

	accounts, err := newClients(t, "http://ledger.invalid").WithBankingURLs(srv.URL, "http://bankrec.invalid").
		ListBankAccounts(t.Context(), "tenant-1", "controller-1", "le-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v1/treasury/accounts" || got.URL.Query().Get("legal_entity_id") != "le-1" {
		t.Fatalf("request %s", got.URL)
	}
	if got.Header.Get("X-Tenant-Id") != "tenant-1" || got.Header.Get("X-Principal-Id") != "controller-1" {
		t.Fatalf("headers %v", got.Header)
	}
	if len(accounts) != 1 || accounts[0].BankAccountID != "a-1" || accounts[0].AccountStatus != "ACTIVE" {
		t.Fatalf("accounts %+v: another entity's account must never be counted for this one", accounts)
	}
}

func TestBankingClients_FailuresAreErrorsNeverEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newClients(t, "http://ledger.invalid").WithBankingURLs(srv.URL, srv.URL)

	if _, err := c.ListBankAccounts(t.Context(), "t", "p", "le-1"); !errors.Is(err, domain.ErrTreasuryUnavailable) {
		t.Fatalf("treasury 403: %v", err)
	}
	if _, err := c.GetBankReconciliationStatus(t.Context(), "t", "p", "le-1", time.Now(), time.Now()); !errors.Is(err, domain.ErrBankReconciliationUnavailable) {
		t.Fatalf("bank-rec 403: %v", err)
	}
	unset := newClients(t, "http://ledger.invalid")
	if _, err := unset.ListBankAccounts(t.Context(), "t", "p", "le-1"); !errors.Is(err, domain.ErrTreasuryUnavailable) {
		t.Fatalf("unconfigured treasury must be an error: %v", err)
	}
}

func TestGetBankReconciliationStatus_RequestAndDecode(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`{"legal_entity_id":"le-1","period_start":"2026-10-01","period_end":"2026-10-31","accounts":[
			{"bank_account_id":"a-1",
			 "latest_certified":{"run_id":"r-23","statement_date":"2026-10-23","status":"CERTIFIED","certified_at":"2026-10-24T12:00:00Z"},
			 "latest_run":{"run_id":"r-30","statement_date":"2026-10-30","status":"EXCEPTIONS_OPEN"}}]}`))
	}))
	defer srv.Close()

	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	status, err := newClients(t, "http://ledger.invalid").WithBankingURLs("http://treasury.invalid", srv.URL).
		GetBankReconciliationStatus(t.Context(), "tenant-1", "controller-1", "le-1", start, end)
	if err != nil {
		t.Fatal(err)
	}
	q := got.URL.Query()
	if got.URL.Path != "/v1/reconciliation-runs/period-status" || q.Get("legal_entity_id") != "le-1" ||
		q.Get("period_start") != "2026-10-01" || q.Get("period_end") != "2026-10-31" {
		t.Fatalf("request %s", got.URL)
	}
	if got.Header.Get("X-Principal-Id") != "controller-1" {
		t.Fatal("principal not forwarded")
	}
	if len(status) != 1 || status[0].LatestCertified.RunID != "r-23" || status[0].LatestRun.Status != "EXCEPTIONS_OPEN" {
		t.Fatalf("decoded %+v", status)
	}
}
