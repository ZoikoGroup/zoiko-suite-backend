package ledger_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/reconciliation-intelligence-svc/internal/ledger"
)

var scope = ledger.Scope{TenantID: "t-1", PrincipalID: "p-1", LegalEntityID: "le-1"}

func serve(t *testing.T, status int, body string, seen *http.Header) *ledger.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return ledger.NewClient(srv.URL, zap.NewNop())
}

func entry(tenant, entity, account, date, debit string) string {
	return `{"journal_id":"j-1","tenant_id":"` + tenant + `","legal_entity_id":"` + entity + `","account_code":"` + account +
		`","debit_amount":` + debit + `,"credit_amount":0,"currency_code":"USD","transaction_date":"` + date + `"}`
}

// page wraps raw entry JSON in general-ledger-svc's QueryLedger response
// envelope (A15): {"entries": [...], "next_cursor": ...}. cursor is the
// literal JSON for next_cursor — "null" for a complete (non-truncated) page.
func page(cursor string, entries ...string) string {
	body := "["
	for i, e := range entries {
		if i > 0 {
			body += ","
		}
		body += e
	}
	body += "]"
	return `{"entries":` + body + `,"next_cursor":` + cursor + `}`
}

func TestListCashAccountEntries_ExactAmountsAndEnvelope(t *testing.T) {
	var seen http.Header
	c := serve(t, http.StatusOK, page("null", entry("t-1", "le-1", "1000", "2026-07-02", "125.5")), &seen)
	got, err := c.ListCashAccountEntries(context.Background(), scope, "1000", "2026-06-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Debit.Cmp(big.NewRat(251, 2)) != 0 || got[0].Credit.Sign() != 0 {
		t.Fatalf("unexpected entries: %+v", got)
	}
	for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Legal-Entity-Id", "X-Request-Id", "X-Correlation-ID", "X-Source-Channel"} {
		if seen.Get(h) == "" {
			t.Errorf("envelope header %s not sent", h)
		}
	}
}

// A15: general-ledger-svc's LedgerEntry.debit_amount/credit_amount are now
// exact decimal strings on the wire, not bare JSON numbers. json.Number
// accepts a quoted numeric string exactly as it accepts a bare number, so
// this client needs no change for the new wire shape — this pins that down
// against a future encoding/json behavior change or client refactor.
func TestListCashAccountEntries_AcceptsQuotedDecimalStringAmount(t *testing.T) {
	c := serve(t, http.StatusOK, page("null", entry("t-1", "le-1", "1000", "2026-07-02", `"125.50"`)), nil)
	got, err := c.ListCashAccountEntries(context.Background(), scope, "1000", "2026-06-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Debit.Cmp(big.NewRat(251, 2)) != 0 {
		t.Fatalf("expected the quoted decimal string parsed exactly as 125.50, got %+v", got)
	}
}

// Every row is checked against what was asked for; anything else fails the
// whole read closed rather than becoming a proposal.
func TestListCashAccountEntries_OutOfScopeRowFailsClosed(t *testing.T) {
	for name, row := range map[string]string{
		"other tenant":      entry("t-2", "le-1", "1000", "2026-07-02", "1"),
		"other entity":      entry("t-1", "le-2", "1000", "2026-07-02", "1"),
		"other account":     entry("t-1", "le-1", "2000", "2026-07-02", "1"),
		"outside window":    entry("t-1", "le-1", "1000", "2026-09-02", "1"),
		"negative amount":   entry("t-1", "le-1", "1000", "2026-07-02", "-1"),
		"unparseable money": entry("t-1", "le-1", "1000", "2026-07-02", `"abc"`),
	} {
		t.Run(name, func(t *testing.T) {
			c := serve(t, http.StatusOK, page("null", row), nil)
			if _, err := c.ListCashAccountEntries(context.Background(), scope, "1000", "2026-06-01", "2026-08-01"); !errors.Is(err, ledger.ErrInconsistent) {
				t.Fatalf("expected ErrInconsistent, got %v", err)
			}
		})
	}
}

func TestListCashAccountEntries_StatusAndScopeErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := serve(t, http.StatusBadRequest, `{}`, nil).ListCashAccountEntries(ctx, scope, "1000", "2026-06-01", "2026-08-01"); !errors.Is(err, ledger.ErrRequestRefused) {
		t.Fatalf("400: expected ErrRequestRefused, got %v", err)
	}
	if _, err := serve(t, http.StatusServiceUnavailable, `{}`, nil).ListCashAccountEntries(ctx, scope, "1000", "2026-06-01", "2026-08-01"); !errors.Is(err, ledger.ErrServiceUnavailable) {
		t.Fatalf("503: expected ErrServiceUnavailable, got %v", err)
	}
	if _, err := serve(t, http.StatusOK, `[]`, nil).ListCashAccountEntries(ctx, ledger.Scope{TenantID: "t-1"}, "1000", "2026-06-01", "2026-08-01"); !errors.Is(err, ledger.ErrScopeMissing) {
		t.Fatalf("no principal: expected ErrScopeMissing, got %v", err)
	}
}

// TestListCashAccountEntries_FollowsCursorAcrossPages proves a truncated
// first page (general-ledger-svc A15's next_cursor) is followed to
// completion rather than refused, and that after_entry_seq is forwarded on
// the next request exactly as the server named it.
func TestListCashAccountEntries_FollowsCursorAcrossPages(t *testing.T) {
	var gotCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCursors = append(gotCursors, r.URL.Query().Get("after_entry_seq"))
		w.WriteHeader(http.StatusOK)
		switch len(gotCursors) {
		case 1:
			_, _ = w.Write([]byte(page("42", entry("t-1", "le-1", "1000", "2026-07-02", "1"))))
		default:
			_, _ = w.Write([]byte(page("null", entry("t-1", "le-1", "1000", "2026-07-03", "2"))))
		}
	}))
	t.Cleanup(srv.Close)
	c := ledger.NewClient(srv.URL, zap.NewNop())

	got, err := c.ListCashAccountEntries(context.Background(), scope, "1000", "2026-06-01", "2026-08-01")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both pages' entries, got %d: %+v", len(got), got)
	}
	if len(gotCursors) != 2 || gotCursors[0] != "" || gotCursors[1] != "42" {
		t.Fatalf("expected an unset cursor then after_entry_seq=42, got %v", gotCursors)
	}
}

// TestListCashAccountEntries_RefusesBeyondMaxPages proves an upstream that
// never stops naming a next_cursor is still a bounded, fail-closed refusal,
// not an infinite loop.
func TestListCashAccountEntries_RefusesBeyondMaxPages(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(page("1", entry("t-1", "le-1", "1000", "2026-07-02", "1"))))
	}))
	t.Cleanup(srv.Close)
	c := ledger.NewClient(srv.URL, zap.NewNop())

	if _, err := c.ListCashAccountEntries(context.Background(), scope, "1000", "2026-06-01", "2026-08-01"); !errors.Is(err, ledger.ErrWindowTooLarge) {
		t.Fatalf("expected ErrWindowTooLarge, got %v", err)
	}
	if calls == 0 {
		t.Fatal("expected at least one request before giving up")
	}
}
