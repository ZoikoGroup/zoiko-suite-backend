package ledger_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/ledger"
)

func bridge(purpose string) domain.AccountingBridge {
	return domain.AccountingBridge{
		TenantID: "t-1", LegalEntityID: "le-1", FiscalPeriod: "2026-09", Description: "Payroll", SourceEventID: "wfp-payroll:r:PRIMARY:" + purpose,
		CorrelationID: "corr-1", TransactionCurrency: "GBP", DocumentDate: "2026-09-30", PostingDate: "2026-09-30", PostingPurpose: purpose,
		Lines: []domain.PostingLine{
			{MappingKey: domain.KeyGrossExpense, Debit: "100.5000", Credit: "0.0000"},
			{MappingKey: domain.KeyNetPayable, Debit: "0.0000", Credit: "100.5000"},
		},
	}
}

type captured struct {
	method, path string
	header       http.Header
	body         []byte
}

func server(t *testing.T, status int, body string, c *captured) *ledger.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c != nil {
			c.method, c.path, c.header = r.Method, r.URL.Path, r.Header.Clone()
			c.body, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return ledger.New(srv.URL, "svc-payroll", nil, zap.NewNop())
}

func TestPost_ResultGoesToThePostingEngineWithAnIdempotencyKey(t *testing.T) {
	var c captured
	cl := server(t, 200, `{"execution_id":"ex-1","status":"COMMITTED","journal_id":"j-1"}`, &c)
	out := cl.Post(context.Background(), bridge(domain.PurposeResult), "")

	assert.True(t, out.Posted)
	assert.Equal(t, "ex-1", out.ExecutionID)
	assert.Equal(t, "j-1", out.JournalID)
	assert.Equal(t, "POST", c.method)
	assert.Equal(t, "/v1/postings/events", c.path, "the posting engine is the only door")
	assert.Equal(t, "t-1", c.header.Get("X-Tenant-Id"))
	assert.Equal(t, "svc-payroll", c.header.Get("X-Principal-Id"), "the service identity, not a caller's")
	assert.Equal(t, "le-1", c.header.Get("X-Legal-Entity-Id"))
	assert.Equal(t, "corr-1", c.header.Get("X-Correlation-ID"))
	assert.Equal(t, "system", c.header.Get("X-Source-Channel"))
	assert.NotEmpty(t, c.header.Get("X-Request-Id"))
	assert.Equal(t, "wfp-payroll:r:PRIMARY:PAYROLL_RESULT", c.header.Get("Idempotency-Key"), "a retry is the same posting")

	var body map[string]any
	require.NoError(t, json.Unmarshal(c.body, &body))
	assert.Equal(t, "wfp-payroll:r:PRIMARY:PAYROLL_RESULT", body["source_event_id"])
	assert.Len(t, body["lines"], 2)
	assert.NotContains(t, string(c.body), "account_code", "no account code is ever sent")
}

func TestPost_ReversalNamesTheOriginalJournal(t *testing.T) {
	var c captured
	cl := server(t, 201, `{"execution_id":"ex-2","status":"COMMITTED","journal_id":"j-rev"}`, &c)
	b := bridge(domain.PurposeReversal)
	b.Description = "Reversal of Payroll: retro correction"
	out := cl.Post(context.Background(), b, "j-original")

	assert.True(t, out.Posted)
	assert.Equal(t, "j-rev", out.JournalID)
	assert.Equal(t, "/v1/postings/reversals", c.path)
	var body map[string]string
	require.NoError(t, json.Unmarshal(c.body, &body))
	assert.Equal(t, "j-original", body["original_journal_id"])
	assert.Contains(t, body["reason"], "retro correction")
}

func TestPost_OutcomeMapping(t *testing.T) {
	cases := map[string]struct {
		status        int
		body          string
		posted, final bool
		errContains   string
	}{
		"committed":                    {200, `{"execution_id":"e","status":"COMMITTED","journal_id":"j"}`, true, false, ""},
		"created":                      {201, `{"execution_id":"e","status":"COMMITTED","journal_id":"j"}`, true, false, ""},
		"replay of a failed execution": {200, `{"execution_id":"e","status":"FAILED","failure_reason":"period closed"}`, false, true, "period closed"},
		"replay of a quarantined one":  {200, `{"execution_id":"e","status":"QUARANTINED"}`, false, true, "QUARANTINED"},
		"committed without a journal":  {200, `{"execution_id":"e","status":"COMMITTED"}`, false, false, "without a journal id"},
		"unmapped account key":         {422, `{"error_code":"posting_rule_ambiguous"}`, false, true, "422"},
		"bad request":                  {400, `{"error":"invalid_line"}`, false, true, "400"},
		"forbidden":                    {403, `{}`, false, true, "403"},
		"closed period conflict":       {409, `{}`, false, true, "409"},
		"rate limited":                 {429, ``, false, false, "429"},
		"timeout":                      {408, ``, false, false, "408"},
		"server error":                 {500, ``, false, false, "500"},
		"unavailable":                  {503, ``, false, false, "503"},
	}
	for name, c := range cases {
		out := server(t, c.status, c.body, nil).Post(context.Background(), bridge(domain.PurposeResult), "")
		assert.Equal(t, c.posted, out.Posted, name)
		assert.Equal(t, c.final, out.Final, "%s: a refusal is final, an outage is retried", name)
		if c.errContains != "" {
			assert.Contains(t, out.Err, c.errContains, name)
		}
	}
}

func TestPost_ATransportFailureIsTransient(t *testing.T) {
	out := ledger.New("http://127.0.0.1:1", "svc", nil, zap.NewNop()).Post(context.Background(), bridge(domain.PurposeResult), "")
	assert.False(t, out.Posted)
	assert.False(t, out.Final, "an unreachable ledger is retried, never rejected")
	assert.Contains(t, out.Err, "unreachable")
}

func TestPost_AnUnexpressibleProposalIsFinal(t *testing.T) {
	b := bridge(domain.PurposeResult)
	b.Lines[0].Debit = "abc"
	out := server(t, 200, `{}`, nil).Post(context.Background(), b, "")
	assert.True(t, out.Final)
	assert.Contains(t, out.Err, "cannot build the ledger request")
}

func TestGetJournal(t *testing.T) {
	var c captured
	cl := server(t, 200, `{"journal_id":"j-1","status":"FINALIZED","legal_entity_id":"le-1","fiscal_period":"2026-09","currency_code":"GBP",
		"lines":[{"debit_amount":100.5,"credit_amount":0},{"debit_amount":0,"credit_amount":100.5}]}`, &c)
	j, ok, err := cl.GetJournal(context.Background(), "t-1", "le-1", "corr", "j/1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "FINALIZED", j.Status)
	assert.Len(t, j.Lines, 2)
	assert.Equal(t, "GET", c.method)
	assert.Equal(t, "/v1/journals/j/1", c.path, "the id is path-escaped on the wire (decoded by the server)")
	assert.Equal(t, "t-1", c.header.Get("X-Tenant-Id"))

	_, ok, err = server(t, 404, `{}`, nil).GetJournal(context.Background(), "t", "le", "c", "j")
	require.NoError(t, err)
	assert.False(t, ok, "no such journal is an answer")

	for _, status := range []int{401, 403, 500, 503} {
		_, ok, err := server(t, status, `{}`, nil).GetJournal(context.Background(), "t", "le", "c", "j")
		assert.ErrorIs(t, err, domain.ErrLedgerUnavailable, "status %d is not an answer", status)
		assert.False(t, ok)
	}
	_, _, err = server(t, 200, `garbage`, nil).GetJournal(context.Background(), "t", "le", "c", "j")
	assert.ErrorIs(t, err, domain.ErrLedgerUnavailable)
	_, _, err = ledger.New("http://127.0.0.1:1", "svc", nil, zap.NewNop()).GetJournal(context.Background(), "t", "le", "c", "j")
	assert.ErrorIs(t, err, domain.ErrLedgerUnavailable)
}
