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

	"zoiko.io/asset-management-svc/internal/clients"
	"zoiko.io/asset-management-svc/internal/domain"
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
	return clients.New(srv.URL, "", zap.NewNop()).WithKernelBook("book-1", "GBP"), &calls
}

func TestKernelPostingIsOffUnlessBookAndCurrencyAreBothSet(t *testing.T) {
	c := clients.New("http://x", "", zap.NewNop())
	if c.KernelPostingEnabled() || c.WithKernelBook("book-1", "").KernelPostingEnabled() || c.WithKernelBook("", "GBP").KernelPostingEnabled() {
		t.Fatal("the kernel needs the book and its currency")
	}
	if !c.WithKernelBook("book-1", "GBP").KernelPostingEnabled() {
		t.Fatal("both set")
	}
}

// One category of a run becomes an ASSET.DEPRECIATION_POSTED event carrying the class as a dimension, then a posting of that event into
// the book at the end of the period; the account codes of the run are nowhere in it.
func TestPostDepreciationKernelEvent_RegistersTheEventAndPostsItIntoTheBook(t *testing.T) {
	c, calls := kernelServer(t, 201, 201, `{"journal_id":"j-1"}`)
	id, err := c.PostDepreciationKernelEvent(context.Background(), "t-1", "approver-1", "le-1", "2026-02", "run-9", "IT", "1000.00", "corr-1")
	if err != nil || id != "j-1" {
		t.Fatalf("%q %v", id, err)
	}
	if len(*calls) != 2 {
		t.Fatalf("%d calls", len(*calls))
	}
	ev, post := (*calls)[0], (*calls)[1]
	if ev.path != "/v1/accounting-events" || post.path != "/v1/accounting-postings" {
		t.Fatalf("%s %s", ev.path, post.path)
	}
	if ev.body["event_type"] != "ASSET.DEPRECIATION_POSTED" || ev.body["transaction_currency"] != "GBP" || ev.body["document_date"] != "2026-02-28T00:00:00Z" ||
		ev.body["source_event_id"] != "ast-dep:run-9:IT" || ev.body["idempotency_key"] != "ast-dep:run-9:IT" {
		t.Fatalf("%v", ev.body)
	}
	if ev.body["dimensions"].(map[string]any)["asset_class"] != "IT" {
		t.Fatalf("%v", ev.body["dimensions"])
	}
	facts := ev.body["facts"].(map[string]any)
	if facts["amount"] != "1000.00" || facts["schedule_ref"] != "run-9" || facts["period_ref"] != "2026-02" || facts["policy_version"] == "" {
		t.Fatalf("%v", facts)
	}
	if post.body["accounting_event_id"] != "ev-1" || post.body["book_id"] != "book-1" || post.body["posting_date"] != "2026-02-28T00:00:00Z" {
		t.Fatalf("%v", post.body)
	}
	for _, k := range *calls {
		if k.header.Get("X-Principal-Id") != "approver-1" || k.header.Get("X-Legal-Entity-Id") != "le-1" || k.header.Get("Idempotency-Key") != "ast-dep:run-9:IT" {
			t.Fatalf("%v", k.header)
		}
	}
}

func TestPostDepreciationKernelEvent_RefusalsAreErrorsAndNothingElseIsSent(t *testing.T) {
	c, calls := kernelServer(t, 422, 0, "")
	if _, err := c.PostDepreciationKernelEvent(context.Background(), "t", "p", "le", "2026-02", "run", "IT", "1.00", "c"); !errors.Is(err, domain.ErrLedgerUnavailable) || len(*calls) != 1 {
		t.Fatalf("%v %d", err, len(*calls))
	}
	c, calls = kernelServer(t, 201, 422, `{"code":"ACCOUNT_UNAVAILABLE"}`)
	if _, err := c.PostDepreciationKernelEvent(context.Background(), "t", "p", "le", "2026-02", "run", "IT", "1.00", "c"); !errors.Is(err, domain.ErrLedgerUnavailable) || len(*calls) != 2 {
		t.Fatalf("%v %d", err, len(*calls))
	}
	c, _ = kernelServer(t, 201, 201, `{}`)
	if _, err := c.PostDepreciationKernelEvent(context.Background(), "t", "p", "le", "2026-02", "run", "IT", "1.00", "c"); err == nil {
		t.Fatal("a posting answered without a journal id is not a posting")
	}
	if _, err := c.PostDepreciationKernelEvent(context.Background(), "t", "p", "le", "February", "run", "IT", "1.00", "c"); err == nil {
		t.Fatal("a period that is not YYYY-MM has no period end")
	}
}
