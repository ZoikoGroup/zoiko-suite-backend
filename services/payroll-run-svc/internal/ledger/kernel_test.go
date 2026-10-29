package ledger_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/ledger"
)

type kernelCall struct {
	path   string
	header http.Header
	body   map[string]any
}

// kernelLedger answers the two steps of a kernel posting: the accounting event, then the posting of that event into a book.
func kernelLedger(t *testing.T, eventStatus, postStatus int, postBody string) (*ledger.Client, *[]kernelCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []kernelCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, kernelCall{path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
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
	return ledger.New(srv.URL, "svc-payroll", nil, zap.NewNop()).WithKernelBook("book-1"), &calls
}

func kernelBridge() domain.AccountingBridge {
	b := bridge(domain.PurposeResult)
	b.RunRefID, b.ResultID, b.BridgeID = "run-1", "res-7", "br-1"
	b.CreatedAt = time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	return b
}

// With a kernel book the result is an accounting event, then a posting of that event into the book: never the legacy posting command.
func TestPost_KernelBookRegistersAnEventAndPostsItIntoTheBook(t *testing.T) {
	cl, calls := kernelLedger(t, 201, 201, `{"journal_id":"j-1","posting_execution_id":"ex-1"}`)
	out := cl.Post(context.Background(), kernelBridge(), "")

	assert.True(t, out.Posted)
	assert.Equal(t, "j-1", out.JournalID)
	assert.Equal(t, "ex-1", out.ExecutionID)
	require.Len(t, *calls, 2)
	ev, post := (*calls)[0], (*calls)[1]
	assert.Equal(t, "/v1/accounting-events", ev.path)
	assert.Equal(t, "/v1/accounting-postings", post.path)
	assert.Equal(t, "PAYROLL.RESULT_ACCEPTED", ev.body["event_type"])
	assert.Equal(t, "wfp-payroll:r:PRIMARY:PAYROLL_RESULT", ev.body["source_event_id"])
	assert.Equal(t, ev.body["source_event_id"], ev.body["idempotency_key"], "a retry is the same event")
	assert.Equal(t, "payroll-run-svc", ev.body["source_system"])
	facts := ev.body["facts"].(map[string]any)
	assert.Equal(t, "100.5000", facts["gross"])
	assert.Equal(t, "100.5000", facts["net"])
	assert.Equal(t, "run-1", facts["result_ref"])
	assert.Equal(t, "res-7", facts["result_version"])
	assert.Equal(t, "ev-1", post.body["accounting_event_id"])
	assert.Equal(t, "book-1", post.body["book_id"])
	assert.Equal(t, "2026-09-30T00:00:00Z", post.body["posting_date"])
	for _, c := range *calls {
		assert.Equal(t, "svc-payroll", c.header.Get("X-Principal-Id"))
		assert.Equal(t, "le-1", c.header.Get("X-Legal-Entity-Id"))
		assert.NotEqual(t, "/v1/postings/events", c.path)
	}
}

// A reversal is the same as before whatever the book: the ledger reversal command, on the journal the result produced.
func TestPost_KernelBookDoesNotChangeReversal(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"execution_id":"ex-2","status":"COMMITTED","journal_id":"j-rev"}`))
	}))
	t.Cleanup(srv.Close)
	out := ledger.New(srv.URL, "svc", nil, zap.NewNop()).WithKernelBook("book-1").Post(context.Background(), bridge(domain.PurposeReversal), "j-original")
	assert.True(t, out.Posted)
	assert.Equal(t, "/v1/postings/reversals", path)
}

func TestPost_KernelBookOutcomeMapping(t *testing.T) {
	cases := map[string]struct {
		eventStatus, postStatus int
		posted, final           bool
		errContains             string
	}{
		"the event is refused for good":     {422, 0, false, true, "register event"},
		"the posting is refused for good":   {201, 422, false, true, "post"},
		"a locked period":                   {201, 409, false, true, "409"},
		"the ledger is down at the event":   {503, 0, false, false, "503"},
		"the ledger is down at the posting": {201, 503, false, false, "503"},
		"rate limited":                      {429, 0, false, false, "429"},
	}
	for name, c := range cases {
		cl, _ := kernelLedger(t, c.eventStatus, c.postStatus, `{}`)
		out := cl.Post(context.Background(), kernelBridge(), "")
		assert.Equal(t, c.posted, out.Posted, name)
		assert.Equal(t, c.final, out.Final, "%s: a refusal is final, an outage is retried", name)
		assert.Contains(t, out.Err, c.errContains, name)
	}
	cl, _ := kernelLedger(t, 201, 201, `{"posting_execution_id":"ex"}`)
	out := cl.Post(context.Background(), kernelBridge(), "")
	assert.False(t, out.Posted)
	assert.False(t, out.Final, "committed without a journal id is retried, not guessed")
}

// A proposal the event cannot express is final and never reaches the ledger.
func TestPost_KernelBookRefusesAProposalItCannotExpress(t *testing.T) {
	cl, calls := kernelLedger(t, 201, 201, `{"journal_id":"j"}`)
	b := kernelBridge()
	b.Lines = append(b.Lines, domain.PostingLine{MappingKey: "MYSTERY", Debit: "1.0000", Credit: "0.0000"})
	out := cl.Post(context.Background(), b, "")
	assert.False(t, out.Posted)
	assert.True(t, out.Final)
	assert.Empty(t, *calls)
}
