package clients_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/asset-management-svc/internal/clients"
)

func captureLedger(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/postings/events" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"journal_id":"j-1","status":"POSTED"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestPostAssetEventAccountingEvent_SendsCurrencyAndDocumentDate(t *testing.T) {
	srv, got := captureLedger(t)
	c := clients.New(srv.URL, "", zap.NewNop())

	lines := []clients.LedgerLine{{AccountCode: "A", DebitAmount: 10}, {AccountCode: "B", CreditAmount: 10}}
	id, err := c.PostAssetEventAccountingEvent(context.Background(), "t", "p", "le-1", "2026-09", "d", "evt-1", "corr", "EUR", "2026-09-15", lines)
	if err != nil || id != "j-1" {
		t.Fatalf("unexpected result %q, %v", id, err)
	}
	if (*got)["transaction_currency"] != "EUR" || (*got)["document_date"] != "2026-09-15" {
		t.Fatalf("body missing real currency/date: %v", *got)
	}
	if (*got)["source_event_id"] != "evt-1" {
		t.Fatalf("source_event_id lost: %v", *got)
	}
}

// Depreciation has no real currency/date source in this service; the client
// must not invent one.
func TestPostDepreciationAccountingEvent_DoesNotInventCurrencyOrDate(t *testing.T) {
	srv, got := captureLedger(t)
	c := clients.New(srv.URL, "", zap.NewNop())

	lines := []clients.LedgerLine{{AccountCode: "A", DebitAmount: 10}, {AccountCode: "B", CreditAmount: 10}}
	if _, err := c.PostDepreciationAccountingEvent(context.Background(), "t", "p", "le-1", "2026-09", "d", "run-1", "corr", lines); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*got)["transaction_currency"]; ok {
		t.Fatalf("must not send an invented currency: %v", *got)
	}
	if _, ok := (*got)["document_date"]; ok {
		t.Fatalf("must not send an invented date: %v", *got)
	}
}
