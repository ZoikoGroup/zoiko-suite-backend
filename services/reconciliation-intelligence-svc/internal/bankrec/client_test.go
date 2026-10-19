package bankrec

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

const (
	runID   = "6f1c2a8e-3b7d-4c55-9a10-2d4e8f7b9c01"
	entity  = "2a4b6c8d-0000-4000-8000-000000000001"
	account = "ba-1"
)

var scope = Scope{TenantID: "tenant-1", PrincipalID: "principal-1", LegalEntityID: entity}

func testRun() *Run {
	return &Run{RunID: runID, LegalEntityID: entity, BankAccountID: account, StatementDate: "2026-07-01", Status: "RUNNING"}
}

// lineJSON renders one statement line the way bank-reconciliation-svc
// does: amount as a bare JSON number (its float64 field), statement_date as
// an RFC 3339 timestamp.
func lineJSON(id, amount string, mods ...func(map[string]string)) string {
	f := map[string]string{
		"legal_entity_id": entity, "bank_account_id": account,
		"statement_date": "2026-07-01T00:00:00Z", "status": "UNMATCHED",
	}
	for _, m := range mods {
		m(f)
	}
	return fmt.Sprintf(`{"statement_line_id":%q,"tenant_id":"tenant-1","legal_entity_id":%q,"bank_account_id":%q,`+
		`"statement_date":%q,"amount":%s,"currency_code":"USD","bank_reference":"REF-%s","status":%q}`,
		id, f["legal_entity_id"], f["bank_account_id"], f["statement_date"], amount, id, f["status"])
}

// serve answers every request with body and fails the test on anything but
// a GET — this client must never call a bank-reconciliation-svc write route.
func serve(t *testing.T, status int, body string, seen func(*http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("bankrec client sent %s %s; it is read-only", r.Method, r.URL.Path)
		}
		if seen != nil {
			seen(r)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, zap.NewNop())
}

// The canonical envelope is forwarded with the CALLER's values, and
// bank-reconciliation-svc's mandatory read headers are all present. This is
// the defect class fixed twice in financial-close-svc (7abd0f8a, d6c2cc38).
func TestEnvelopeForwarded_CallerValues(t *testing.T) {
	var got http.Header
	c := serve(t, http.StatusOK, `{"run_id":"`+runID+`","legal_entity_id":"`+entity+`","bank_account_id":"ba-1","statement_date":"2026-07-01","status":"RUNNING"}`,
		func(r *http.Request) { got = r.Header.Clone() })

	ctx := svcenvelope.WithEnvelope(context.Background(), svcenvelope.Envelope{
		RequestID: "req-caller", CorrelationID: "corr-caller", CausationID: "cause-caller",
		SourceChannel: svcenvelope.ChannelIntegration, SourceSystem: "erp-x",
	})
	if _, err := c.GetRun(ctx, scope, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	want := map[string]string{
		"X-Tenant-Id":       "tenant-1",
		"X-Principal-Id":    "principal-1",
		"X-Legal-Entity-Id": entity,
		"X-Request-Id":      "req-caller",
		"X-Correlation-ID":  "corr-caller",
		"X-Causation-Id":    "cause-caller",
		"X-Source-Channel":  "integration",
		// An external channel obliges source_system; without it
		// bank-reconciliation-svc would refuse the forwarded channel.
		"X-Source-System": "erp-x",
	}
	for h, v := range want {
		if got.Get(h) != v {
			t.Errorf("%s = %q, want %q", h, got.Get(h), v)
		}
	}
}

// With no envelope in context (no inbound request to inherit from), ids are
// minted and the channel is "system" — the mandatory set is never sent short.
func TestEnvelopeForwarded_MintsWhenAbsent(t *testing.T) {
	var got http.Header
	c := serve(t, http.StatusOK, `[]`, func(r *http.Request) { got = r.Header.Clone() })
	if _, err := c.ListUnmatchedLines(context.Background(), scope, testRun()); err != nil {
		t.Fatalf("ListUnmatchedLines: %v", err)
	}
	for _, h := range []string{"X-Tenant-Id", "X-Principal-Id", "X-Request-Id", "X-Correlation-ID"} {
		if got.Get(h) == "" {
			t.Errorf("%s not sent", h)
		}
	}
	if got.Get("X-Source-Channel") != "system" {
		t.Errorf("X-Source-Channel = %q, want system", got.Get("X-Source-Channel"))
	}
}

// No verified tenant or principal: refuse without calling out. Never a
// made-up identity (b75655a2).
func TestScopeMissing_NoCall(t *testing.T) {
	called := false
	c := serve(t, http.StatusOK, `[]`, func(*http.Request) { called = true })
	for _, s := range []Scope{{PrincipalID: "p"}, {TenantID: "t"}} {
		if _, err := c.GetRun(context.Background(), s, runID); !errors.Is(err, ErrScopeMissing) {
			t.Fatalf("expected ErrScopeMissing, got %v", err)
		}
	}
	if called {
		t.Fatal("a request was sent with no verified tenant/principal")
	}
}

func TestGetRun_StatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrRunNotFound},
		{http.StatusInternalServerError, ErrServiceUnavailable},
		{http.StatusServiceUnavailable, ErrServiceUnavailable},
		// A 400/401 is this request's defect (e.g. envelope_incomplete), not
		// an outage — reported as such.
		{http.StatusBadRequest, ErrRequestRefused},
		{http.StatusUnauthorized, ErrRequestRefused},
	}
	for _, tc := range cases {
		c := serve(t, tc.status, `{"error":"x"}`, nil)
		if _, err := c.GetRun(context.Background(), scope, runID); !errors.Is(err, tc.want) {
			t.Errorf("HTTP %d: got %v, want %v", tc.status, err, tc.want)
		}
	}

	c := NewClient("http://127.0.0.1:1", zap.NewNop()) // nothing listens here
	if _, err := c.GetRun(context.Background(), scope, runID); !errors.Is(err, ErrServiceUnavailable) {
		t.Errorf("unreachable: got %v, want ErrServiceUnavailable", err)
	}
}

func TestGetRun_IncompleteOrWrongRunRefused(t *testing.T) {
	for name, body := range map[string]string{
		"different run id": `{"run_id":"00000000-0000-0000-0000-000000000000","legal_entity_id":"e","bank_account_id":"b","statement_date":"2026-07-01"}`,
		"no bank account":  `{"run_id":"` + runID + `","legal_entity_id":"e","statement_date":"2026-07-01"}`,
		"not json":         `<html>`,
	} {
		c := serve(t, http.StatusOK, body, nil)
		if _, err := c.GetRun(context.Background(), scope, runID); !errors.Is(err, ErrInconsistent) {
			t.Errorf("%s: got %v, want ErrInconsistent", name, err)
		}
	}
}

// The lines are read with the run's own population scope plus
// status=UNMATCHED, and amounts arrive exactly as written — from a JSON
// number or a JSON string — never through float64 on this side.
func TestListUnmatchedLines_ScopeAndExactAmounts(t *testing.T) {
	var gotPath string
	var gotQuery map[string][]string
	body := "[" + lineJSON("sl-1", "100.25") + "," + lineJSON("sl-2", "90071992547409.93") + "," + lineJSON("sl-3", `"-1200.50"`) + "]"
	c := serve(t, http.StatusOK, body, func(r *http.Request) { gotPath = r.URL.Path; gotQuery = r.URL.Query() })

	lines, err := c.ListUnmatchedLines(context.Background(), scope, testRun())
	if err != nil {
		t.Fatalf("ListUnmatchedLines: %v", err)
	}
	if gotPath != "/v1/statement-lines" {
		t.Fatalf("path = %s", gotPath)
	}
	for k, v := range map[string]string{"bank_account_id": account, "statement_date": "2026-07-01", "status": "UNMATCHED", "limit": "1000"} {
		if q := gotQuery[k]; len(q) != 1 || q[0] != v {
			t.Errorf("query %s = %v, want %s", k, q, v)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for i, want := range []string{"100.25", "90071992547409.93", "-1200.5"} {
		if lines[i].Amount != want {
			t.Errorf("line %d amount = %s, want %s", i, lines[i].Amount, want)
		}
	}
	if lines[0].StatementLineID != "sl-1" || lines[0].BankReference != "REF-sl-1" || lines[0].StatementDate != "2026-07-01" {
		t.Errorf("line 0 = %+v", lines[0])
	}
}

// GET /v1/statement-lines has no cursor. A full page cannot be told apart
// from a truncated population, so it is refused rather than analyzed.
func TestListUnmatchedLines_FullPageRefused(t *testing.T) {
	parts := make([]string, maxLines)
	for i := range parts {
		parts[i] = lineJSON(fmt.Sprintf("sl-%d", i), "1")
	}
	c := serve(t, http.StatusOK, "["+strings.Join(parts, ",")+"]", nil)
	if _, err := c.ListUnmatchedLines(context.Background(), scope, testRun()); !errors.Is(err, ErrPopulationTooLarge) {
		t.Fatalf("got %v, want ErrPopulationTooLarge", err)
	}

	// One short of the cap is a complete population.
	c = serve(t, http.StatusOK, "["+strings.Join(parts[:maxLines-1], ",")+"]", nil)
	if lines, err := c.ListUnmatchedLines(context.Background(), scope, testRun()); err != nil || len(lines) != maxLines-1 {
		t.Fatalf("999 lines: got %d lines, err %v", len(lines), err)
	}
}

// Every line must be one of the run's UNMATCHED lines. A filter ignored
// upstream must not become suggestions about other accounts, dates or
// entities.
func TestListUnmatchedLines_OutOfScopeRefused(t *testing.T) {
	cases := map[string]string{
		"other account":   lineJSON("x", "1", func(f map[string]string) { f["bank_account_id"] = "ba-other" }),
		"other date":      lineJSON("x", "1", func(f map[string]string) { f["statement_date"] = "2026-07-02T00:00:00Z" }),
		"matched":         lineJSON("x", "1", func(f map[string]string) { f["status"] = "MATCHED" }),
		"other entity":    lineJSON("x", "1", func(f map[string]string) { f["legal_entity_id"] = "someone-else" }),
		"duplicate":       lineJSON("x", "1") + "," + lineJSON("x", "2"),
		"float artifact":  lineJSON("x", "0.30000000000000004"),
		"too many digits": lineJSON("x", "100000000000000"),
		"not a number":    lineJSON("x", `"abc"`),
	}
	for name, lines := range cases {
		c := serve(t, http.StatusOK, "["+lines+"]", nil)
		if _, err := c.ListUnmatchedLines(context.Background(), scope, testRun()); !errors.Is(err, ErrInconsistent) {
			t.Errorf("%s: got %v, want ErrInconsistent", name, err)
		}
	}
}

func TestDateOnly(t *testing.T) {
	for in, want := range map[string]string{
		"2026-07-01":                "2026-07-01",
		"2026-07-01T00:00:00Z":      "2026-07-01",
		"2026-07-01T00:00:00+05:30": "2026-07-01", // the written date, not shifted to UTC
	} {
		if got, err := dateOnly(in); err != nil || got != want {
			t.Errorf("dateOnly(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := dateOnly("July 1"); err == nil {
		t.Error("expected an error for a non-date")
	}
}
