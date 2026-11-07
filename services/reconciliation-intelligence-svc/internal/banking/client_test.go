package banking_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-intelligence-svc/internal/banking"
)

var scope = banking.Scope{TenantID: "t-1", PrincipalID: "p-1", LegalEntityID: "le-1"}

const txnID = "7a1b2c3d-0000-4000-8000-0000000a2b2b"

type seenRequest struct {
	header http.Header
	method string
	path   string
	query  url.Values
}

func serve(t *testing.T, status int, body string, seen *seenRequest) *banking.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = seenRequest{header: r.Header.Clone(), method: r.Method, path: r.URL.Path, query: r.URL.Query()}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return banking.NewClient(srv.URL, zap.NewNop())
}

// txn is one ListTransactions row, as banking-connector-svc renders it:
// amount a decimal string with four places, transaction_date a timestamp.
func txn(tenant, entity, account, currency, date, amount, status string) string {
	acct := `null`
	if account != "" {
		acct = `"` + account + `"`
	}
	return `{"transaction_id":"` + txnID + `","tenant_id":"` + tenant + `","legal_entity_id":"` + entity +
		`","connection_id":"conn-1","bank_account_id":` + acct + `,"statement_line_id":"bl-1","mapping_version":1,` +
		`"transaction_date":"` + date + `","amount":` + amount + `,"currency":"` + currency +
		`","category":"TRANSFER","counterparty":"","status":"` + status + `"}`
}

func good(amount string) string {
	return txn("t-1", "le-1", "ba-1", "USD", "2026-07-02T00:00:00Z", amount, "NORMALIZED")
}

func page(rows ...string) string {
	return `{"transactions":[` + strings.Join(rows, ",") + `]}`
}

func list(c *banking.Client) error {
	_, err := c.ListBankAccountTransactions(context.Background(), scope, "ba-1", "USD", "2026-06-01", "2026-08-01")
	return err
}

func TestListBankAccountTransactions_QueryEnvelopeAndExactSignedAmount(t *testing.T) {
	var seen seenRequest
	c := serve(t, http.StatusOK, page(good(`"-125.5000"`)), &seen)
	got, err := c.ListBankAccountTransactions(context.Background(), scope, "ba-1", "USD", "2026-06-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TransactionID != txnID || got[0].Amount.Cmp(big.NewRat(-251, 2)) != 0 ||
		got[0].TransactionDate != "2026-07-02" || got[0].BankAccountID != "ba-1" || got[0].Status != "NORMALIZED" {
		t.Fatalf("unexpected transactions: %+v", got)
	}
	if seen.method != http.MethodGet || seen.path != "/v1/banking/canonical-transactions" {
		t.Fatalf("expected GET /v1/banking/canonical-transactions, got %s %s", seen.method, seen.path)
	}
	// Exactly the parameters the route accepts; it refuses any other.
	want := map[string]string{"legal_entity_id": "le-1", "bank_account_id": "ba-1", "currency": "USD",
		"transaction_date_from": "2026-06-01", "transaction_date_to": "2026-08-01", "limit": "5000"}
	if len(seen.query) != len(want) {
		t.Fatalf("unexpected query parameters: %v", seen.query)
	}
	for k, v := range want {
		if seen.query.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, seen.query.Get(k), v)
		}
	}
	for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Request-Id", "X-Correlation-ID", "X-Source-Channel"} {
		if seen.header.Get(h) == "" {
			t.Errorf("envelope header %s not sent", h)
		}
	}
}

// An amount that a float64 cannot tell from its neighbour comes back exactly.
func TestListBankAccountTransactions_AmountNeverGoesThroughFloat64(t *testing.T) {
	c := serve(t, http.StatusOK, page(good(`"12345678901234.5677"`)), nil)
	got, err := c.ListBankAccountTransactions(context.Background(), scope, "ba-1", "USD", "2026-06-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Rat).SetString("12345678901234.5677")
	neighbour, _ := new(big.Rat).SetString("12345678901234.5678")
	if got[0].Amount.Cmp(want) != 0 || got[0].Amount.Cmp(neighbour) == 0 {
		t.Fatalf("amount not held exactly: %s", got[0].Amount.FloatString(4))
	}
}

// A page with a next_cursor is not the whole result: refused, never read as
// complete. Cursor-following is deliberately not implemented.
func TestListBankAccountTransactions_PageWithNextCursorIsRefused(t *testing.T) {
	c := serve(t, http.StatusOK, `{"transactions":[`+good(`"1.0000"`)+`],"next_cursor":"YWJj"}`, nil)
	if err := list(c); !errors.Is(err, banking.ErrWindowTooLarge) {
		t.Fatalf("expected ErrWindowTooLarge, got %v", err)
	}
}

// Every row is checked against what was asked for; anything else fails the
// whole read closed rather than becoming a proposal.
func TestListBankAccountTransactions_OutOfScopeRowFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"other tenant":            page(txn("t-2", "le-1", "ba-1", "USD", "2026-07-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"other entity":            page(txn("t-1", "le-2", "ba-1", "USD", "2026-07-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"other bank account":      page(txn("t-1", "le-1", "ba-2", "USD", "2026-07-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"no bank account":         page(txn("t-1", "le-1", "", "USD", "2026-07-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"other currency":          page(txn("t-1", "le-1", "ba-1", "EUR", "2026-07-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"not NORMALIZED":          page(txn("t-1", "le-1", "ba-1", "USD", "2026-07-02T00:00:00Z", `"1.0000"`, "SUPERSEDED")),
		"outside window":          page(txn("t-1", "le-1", "ba-1", "USD", "2026-09-02T00:00:00Z", `"1.0000"`, "NORMALIZED")),
		"outside window, in UTC":  page(txn("t-1", "le-1", "ba-1", "USD", "2026-08-01T23:30:00-05:00", `"1.0000"`, "NORMALIZED")),
		"amount as a JSON number": page(good(`125.5`)),
		"unparseable amount":      page(good(`"abc"`)),
		"unstorable amount":       page(good(`"1.00001"`)),
		"duplicate transaction":   page(good(`"1.0000"`), good(`"2.0000"`)),
		"transaction id not uuid": strings.Replace(page(good(`"1.0000"`)), txnID, "not-a-uuid", 1),
		"no transactions array":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := list(serve(t, http.StatusOK, body, nil)); !errors.Is(err, banking.ErrInconsistent) {
				t.Fatalf("expected ErrInconsistent, got %v", err)
			}
		})
	}
}

func TestListBankAccountTransactions_EmptyPageIsAnAnswer(t *testing.T) {
	got, err := serve(t, http.StatusOK, page(), nil).ListBankAccountTransactions(context.Background(), scope, "ba-1", "USD", "2026-06-01", "2026-08-01")
	if err != nil || len(got) != 0 {
		t.Fatalf("expected an empty, successful read, got %+v %v", got, err)
	}
}

func TestListBankAccountTransactions_StatusAndScopeErrors(t *testing.T) {
	ctx := context.Background()
	// 403 is what banking-connector-svc answers while this service's
	// principal holds no bundle granting bank.transaction.read.
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		if err := list(serve(t, status, `{}`, nil)); !errors.Is(err, banking.ErrRequestRefused) {
			t.Fatalf("%d: expected ErrRequestRefused, got %v", status, err)
		}
	}
	if err := list(serve(t, http.StatusServiceUnavailable, `{}`, nil)); !errors.Is(err, banking.ErrServiceUnavailable) {
		t.Fatalf("503: expected ErrServiceUnavailable, got %v", err)
	}
	if err := list(banking.NewClient("http://127.0.0.1:1", zap.NewNop())); !errors.Is(err, banking.ErrServiceUnavailable) {
		t.Fatalf("unreachable: expected ErrServiceUnavailable, got %v", err)
	}
	for _, s := range []banking.Scope{{TenantID: "t-1", LegalEntityID: "le-1"}, {TenantID: "t-1", PrincipalID: "p-1"}} {
		if _, err := serve(t, http.StatusOK, page(), nil).ListBankAccountTransactions(ctx, s, "ba-1", "USD", "2026-06-01", "2026-08-01"); !errors.Is(err, banking.ErrScopeMissing) {
			t.Fatalf("scope %+v: expected ErrScopeMissing, got %v", s, err)
		}
	}
}
