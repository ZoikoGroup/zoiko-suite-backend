package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/clients"
	"zoiko.io/inventory-management-svc/internal/domain"
)

type kcall struct {
	path   string
	header http.Header
	body   map[string]any
}

func kernelServer(t *testing.T, eventStatus, postStatus int, postBody string) (*clients.Clients, *[]kcall) {
	t.Helper()
	var calls []kcall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		calls = append(calls, kcall{r.URL.Path, r.Header.Clone(), body})
		switch r.URL.Path {
		case "/v1/accounting-events":
			w.WriteHeader(eventStatus)
			_, _ = w.Write([]byte(`{"accounting_event_id":"ev-1"}`))
		case "/v1/accounting-postings":
			w.WriteHeader(postStatus)
			_, _ = w.Write([]byte(postBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return clients.New("", srv.URL, zap.NewNop()).WithKernelBook("book-1", "GBP"), &calls
}

func TestKernelPostingIsOffUnlessBookAndCurrencyAreBothSet(t *testing.T) {
	c := clients.New("", "http://x", zap.NewNop())
	if c.KernelPostingEnabled() || c.WithKernelBook("book-1", "").KernelPostingEnabled() || c.WithKernelBook("", "GBP").KernelPostingEnabled() {
		t.Fatal("the kernel needs the book and its currency")
	}
	if !c.WithKernelBook("book-1", "GBP").KernelPostingEnabled() {
		t.Fatal("both set")
	}
}

// An event is registered with its facts and dimensions, then posted into the book at the end of its period; a retry is the same event.
func TestPostInventoryKernelEvent_RegistersTheEventAndPostsItIntoTheBook(t *testing.T) {
	c, calls := kernelServer(t, 201, 201, `{"journal_id":"j-1"}`)
	e := clients.KernelEvent{EventType: "INVENTORY.NRV_WRITE_DOWN", SourceKey: "inv-wd:w-1", FiscalPeriod: "2026-09",
		Facts: map[string]string{"amount": "100.00", "write_down_ref": "w-1", "valuation_evidence_ref": "NRV-1"}, Dimensions: map[string]string{"location": "WH-1", "item_class": "FINISHED"}}
	id, err := c.PostInventoryKernelEvent(context.Background(), "t-1", "approver-1", "le-1", "corr-1", e)
	if err != nil || id != "j-1" || len(*calls) != 2 {
		t.Fatalf("%q %v %d", id, err, len(*calls))
	}
	ev, post := (*calls)[0], (*calls)[1]
	if ev.path != "/v1/accounting-events" || post.path != "/v1/accounting-postings" {
		t.Fatalf("%s %s", ev.path, post.path)
	}
	if ev.body["event_type"] != "INVENTORY.NRV_WRITE_DOWN" || ev.body["transaction_currency"] != "GBP" || ev.body["document_date"] != "2026-09-30T00:00:00Z" ||
		ev.body["source_event_id"] != "inv-wd:w-1" || ev.body["idempotency_key"] != "inv-wd:w-1" || ev.body["source_system"] != "inventory-management-svc" {
		t.Fatalf("%v", ev.body)
	}
	if ev.body["dimensions"].(map[string]any)["location"] != "WH-1" || ev.body["facts"].(map[string]any)["valuation_evidence_ref"] != "NRV-1" {
		t.Fatalf("%v", ev.body)
	}
	if post.body["accounting_event_id"] != "ev-1" || post.body["book_id"] != "book-1" || post.body["posting_date"] != "2026-09-30T00:00:00Z" {
		t.Fatalf("%v", post.body)
	}
	for _, k := range *calls {
		if k.header.Get("X-Principal-Id") != "approver-1" || k.header.Get("X-Legal-Entity-Id") != "le-1" || k.header.Get("Idempotency-Key") != "inv-wd:w-1" {
			t.Fatalf("%v", k.header)
		}
	}
}

func TestPostInventoryKernelEvent_RefusalsAreErrors(t *testing.T) {
	e := clients.KernelEvent{EventType: "INVENTORY.COGS_RECOGNIZED", SourceKey: "k", FiscalPeriod: "2026-09", Facts: map[string]string{}, Dimensions: map[string]string{}}
	c, calls := kernelServer(t, 422, 0, "")
	if _, err := c.PostInventoryKernelEvent(context.Background(), "t", "p", "le", "c", e); !errors.Is(err, domain.ErrLedgerUnavailable) || len(*calls) != 1 {
		t.Fatalf("%v %d", err, len(*calls))
	}
	c, calls = kernelServer(t, 201, 422, `{"code":"ACCOUNT_UNAVAILABLE"}`)
	if _, err := c.PostInventoryKernelEvent(context.Background(), "t", "p", "le", "c", e); !errors.Is(err, domain.ErrLedgerUnavailable) || len(*calls) != 2 {
		t.Fatalf("%v %d", err, len(*calls))
	}
	c, _ = kernelServer(t, 201, 201, `{}`)
	if _, err := c.PostInventoryKernelEvent(context.Background(), "t", "p", "le", "c", e); err == nil {
		t.Fatal("a posting answered without a journal id is not a posting")
	}
	e.FiscalPeriod = "September"
	if _, err := c.PostInventoryKernelEvent(context.Background(), "t", "p", "le", "c", e); err == nil {
		t.Fatal("a period that is not YYYY-MM has no period end")
	}
}
