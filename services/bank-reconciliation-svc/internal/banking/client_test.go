package banking_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/banking"
)

func TestGetCanonicalTransaction_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/banking/canonical-transactions/txn123" {
			t.Errorf("expected path /v1/banking/canonical-transactions/txn123, got %s", r.URL.Path)
		}
		if r.Header.Get("X-Tenant-Id") != "tenant456" {
			t.Errorf("expected tenant header, got %s", r.Header.Get("X-Tenant-Id"))
		}
		txn := banking.CanonicalTransaction{
			TransactionID: "txn123",
			TenantID:      "tenant456",
			Status:        "NORMALIZED",
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(txn)
	}))
	defer srv.Close()

	client := banking.NewHTTPClient(srv.URL)
	txn, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "txn123")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if txn.TransactionID != "txn123" {
		t.Errorf("expected txn123, got %s", txn.TransactionID)
	}
}

func TestGetCanonicalTransaction_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := banking.NewHTTPClient(srv.URL)
	_, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "txn123")
	if err != banking.ErrTransactionNotFound {
		t.Fatalf("expected ErrTransactionNotFound, got %v", err)
	}
}

func TestGetCanonicalTransaction_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := banking.NewHTTPClient(srv.URL)
	_, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "txn123")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
