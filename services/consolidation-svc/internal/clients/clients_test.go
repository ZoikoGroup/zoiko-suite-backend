package clients_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

// intercompanyServer answers GET /v1/intercompany/entries with body, the way
// intercompany-accounting-svc does: amount is a JSON number literal of the
// exact stored digits.
func intercompanyServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/intercompany/entries" || r.Header.Get("X-Tenant-Id") != "tenant-1" ||
			r.Header.Get("X-Principal-Id") != "principal-1" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// One pair in every status intercompany-accounting-svc has. Only MATCHED and
// CLOSED (matched, then closed after its period was consolidated) are
// eliminable. Before this, CLOSED was dropped, so a re-run for an
// already-consolidated period treated the pair as never matched and left it
// uneliminated.
func TestFetchEliminableIntercompanyEntries_MatchedAndClosedOnly(t *testing.T) {
	srv := intercompanyServer(t, `[
		{"intercompany_entry_id":"ic-unmatched","match_status":"UNMATCHED","amount":1,"source_journal_id":"s1"},
		{"intercompany_entry_id":"ic-awaiting","match_status":"AWAITING_COUNTERPARTY","amount":1,"source_journal_id":"s2"},
		{"intercompany_entry_id":"ic-matched","match_status":"MATCHED","amount":5000,"source_journal_id":"s3","target_journal_id":"t3"},
		{"intercompany_entry_id":"ic-mismatch","match_status":"MISMATCH","amount":1,"source_journal_id":"s4","target_journal_id":"t4"},
		{"intercompany_entry_id":"ic-disputed","match_status":"DISPUTED","amount":1,"source_journal_id":"s5","target_journal_id":"t5"},
		{"intercompany_entry_id":"ic-resolved","match_status":"RESOLVED","amount":1,"source_journal_id":"s6","target_journal_id":"t6"},
		{"intercompany_entry_id":"ic-closed","match_status":"CLOSED","amount":99999999999999.9999,"source_journal_id":"s7","target_journal_id":"t7"}
	]`)

	got, err := clients.New("", srv.URL, zap.NewNop()).FetchEliminableIntercompanyEntries(context.Background(), "tenant-1", "principal-1")
	if err != nil {
		t.Fatalf("FetchEliminableIntercompanyEntries: %v", err)
	}
	byID := map[string]clients.IntercompanyEntry{}
	for _, e := range got {
		byID[e.IntercompanyEntryID] = e
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly the MATCHED and CLOSED entries, got %+v", got)
	}
	if e, ok := byID["ic-matched"]; !ok || e.MatchStatus != "MATCHED" {
		t.Errorf("MATCHED entry must still be eliminable, got %+v", got)
	}
	closed, ok := byID["ic-closed"]
	if !ok {
		t.Fatalf("CLOSED entry must be eliminable, got %+v", got)
	}
	if closed.TargetJournalID == nil || *closed.TargetJournalID != "t7" {
		t.Errorf("CLOSED entry lost its target journal: %+v", closed)
	}
	// float64 would have decoded this as 1e+14.
	if closed.Amount != domain.Decimal("99999999999999.9999") {
		t.Errorf("amount lost precision: %q", closed.Amount)
	}
}

// The amount may also arrive as a decimal string, the form ZS-DATA-001 D06
// prefers, and intercompany-accounting-svc already accepts on input.
func TestFetchEliminableIntercompanyEntries_StringAmount(t *testing.T) {
	srv := intercompanyServer(t, `[{"intercompany_entry_id":"ic-1","match_status":"MATCHED","amount":"1234.5678"}]`)
	got, err := clients.New("", srv.URL, zap.NewNop()).FetchEliminableIntercompanyEntries(context.Background(), "tenant-1", "principal-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Amount != "1234.5678" {
		t.Fatalf("got %+v", got)
	}
}

// A malformed amount fails the read rather than becoming zero, which would
// quietly lower the elimination bound.
func TestFetchEliminableIntercompanyEntries_MalformedAmountFails(t *testing.T) {
	for _, amount := range []string{`"1/3"`, `"abc"`, `true`} {
		srv := intercompanyServer(t, `[{"intercompany_entry_id":"ic-1","match_status":"MATCHED","amount":`+amount+`}]`)
		if _, err := clients.New("", srv.URL, zap.NewNop()).FetchEliminableIntercompanyEntries(context.Background(), "tenant-1", "principal-1"); err == nil {
			t.Errorf("amount %s: expected the fetch to fail", amount)
		}
	}
}
