package clients

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBankAccountHTTPClient_VerifyBankAccount_DecodesTreasuryDetail locks in
// treasury-svc's GET /v1/treasury/accounts/{id} response shape: the account
// fields are embedded at the top level (not under a "bank_account" object),
// and currency_code is the field BNK-02 now records on the connection.
func TestBankAccountHTTPClient_VerifyBankAccount_DecodesTreasuryDetail(t *testing.T) {
	var gotPath, gotTenant, gotPrincipal, gotCorrelation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotPrincipal = r.Header.Get("X-Principal-Id")
		gotCorrelation = r.Header.Get("X-Correlation-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"bank_account_id":"acct-1",
			"tenant_id":"tenant-1",
			"legal_entity_id":"le-1",
			"account_status":"ACTIVE",
			"currency_code":"EUR",
			"is_ownership_verified":true
		}`))
	}))
	defer srv.Close()

	ref, err := NewBankAccountHTTPClient(srv.URL).VerifyBankAccount(
		context.Background(), "tenant-1", "acct-1", "principal-1", "corr-1")
	if err != nil {
		t.Fatalf("VerifyBankAccount: %v", err)
	}
	if ref.BankAccountID != "acct-1" || ref.LegalEntityID != "le-1" || ref.CurrencyCode != "EUR" || ref.OperationalStatus != "ACTIVE" {
		t.Fatalf("unexpected decoded account: %+v", ref)
	}
	if gotPath != "/v1/treasury/accounts/acct-1" || gotTenant != "tenant-1" || gotPrincipal != "principal-1" || gotCorrelation != "corr-1" {
		t.Fatalf("unexpected request: path=%q tenant=%q principal=%q correlation=%q", gotPath, gotTenant, gotPrincipal, gotCorrelation)
	}
}

func TestBankAccountHTTPClient_VerifyBankAccount_RejectsTenantMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bank_account_id":"acct-1","tenant_id":"tenant-2","currency_code":"EUR"}`))
	}))
	defer srv.Close()

	_, err := NewBankAccountHTTPClient(srv.URL).VerifyBankAccount(
		context.Background(), "tenant-1", "acct-1", "principal-1", "corr-1")
	if !errors.Is(err, ErrBankAccountInvalid) {
		t.Fatalf("expected ErrBankAccountInvalid, got %v", err)
	}
}

func TestBankAccountHTTPClient_VerifyBankAccount_FailsClosedOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewBankAccountHTTPClient(srv.URL).VerifyBankAccount(
		context.Background(), "tenant-1", "acct-1", "principal-1", "corr-1")
	if !errors.Is(err, ErrBankAccountUnavailable) {
		t.Fatalf("expected ErrBankAccountUnavailable, got %v", err)
	}
}
