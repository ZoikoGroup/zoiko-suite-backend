package banking_test

import (
	"context"
	"encoding/json"
	"errors"
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
		// C2: banking-connector-svc refuses this read without a principal.
		if r.Header.Get("X-Principal-Id") != "principal789" {
			t.Errorf("expected principal header principal789, got %q", r.Header.Get("X-Principal-Id"))
		}
		if r.Header.Get("X-Source-Channel") != "system" {
			t.Errorf("expected source channel system, got %q", r.Header.Get("X-Source-Channel"))
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
	txn, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "principal789", "txn123")
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
	_, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "principal789", "txn123")
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
	_, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "principal789", "txn123")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestGetCanonicalTransaction_NoPrincipal_FailsClosedWithoutCalling: a
// missing principal is a caller bug, never an anonymous read — the client
// refuses before anything reaches the wire.
func TestGetCanonicalTransaction_NoPrincipal_FailsClosedWithoutCalling(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	client := banking.NewHTTPClient(srv.URL)
	_, err := client.GetCanonicalTransaction(context.Background(), "tenant456", "", "txn123")
	if !errors.Is(err, banking.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if called {
		t.Fatal("expected no request to be sent without a principal")
	}
}
