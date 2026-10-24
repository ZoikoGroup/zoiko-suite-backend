package clients_test

// SPEC_DEVIATIONS A13: every journal this service posted was refused by
// general-ledger-svc. Its envelope middleware (write-strict) refuses a write
// without request id, correlation id, source channel, legal entity and
// Idempotency-Key, and its CreateJournal refuses a body without ACC-03's
// journal_type, transaction/posting dates and currency. These tests pin both
// halves of the request, on every write the posting paths make: create,
// validate, post and reverse.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
)

type recordedWrite struct {
	path   string
	header http.Header
	body   map[string]any
}

// journalLedger accepts a create (201, PENDING), validate and post (200) and
// reverse (201), recording each write.
type journalLedger struct {
	mu     sync.Mutex
	writes []recordedWrite
}

func (l *journalLedger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	l.mu.Lock()
	l.writes = append(l.writes, recordedWrite{path: r.URL.Path, header: r.Header.Clone(), body: body})
	l.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/journals":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"journal_id":"j-1","status":"PENDING"}`))
	case strings.HasSuffix(r.URL.Path, "/reverse"):
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"journal_id":"j-rev"}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func (l *journalLedger) recorded() []recordedWrite {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedWrite(nil), l.writes...)
}

func newJournalClients(t *testing.T) (*clients.Clients, *journalLedger) {
	t.Helper()
	l := &journalLedger{}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return clients.New("", srv.URL, "", "", "", "", "", "", zap.NewNop()), l
}

var periodEnd = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

func meta(journalType string) domain.GLJournalMeta {
	return domain.GLJournalMeta{JournalType: journalType, TransactionDate: periodEnd, PostingDate: periodEnd, CurrencyCode: "GBP"}
}

// assertLedgerEnvelope checks the §4 headers general-ledger-svc's write-strict
// envelope refuses a write without.
func assertLedgerEnvelope(t *testing.T, w recordedWrite, wantIdempotencyKey string) {
	t.Helper()
	for header, want := range map[string]string{
		"X-Tenant-Id":       "t1",
		"X-Principal-Id":    "p1",
		"X-Legal-Entity-Id": "le-1",
		"X-Source-Channel":  "system",
		"Idempotency-Key":   wantIdempotencyKey,
	} {
		if got := w.header.Get(header); got != want {
			t.Errorf("%s: %s = %q, want %q", w.path, header, got, want)
		}
	}
	for _, header := range []string{"X-Request-Id", "X-Correlation-ID"} {
		if w.header.Get(header) == "" {
			t.Errorf("%s: %s missing", w.path, header)
		}
	}
}

func assertACC03Body(t *testing.T, body map[string]any, wantType string) {
	t.Helper()
	for field, want := range map[string]string{
		"journal_type":     wantType,
		"transaction_date": "2026-01-31",
		"posting_date":     "2026-01-31",
		"currency_code":    "GBP",
	} {
		if got, _ := body[field].(string); got != want {
			t.Errorf("create body %s = %q, want %q", field, got, want)
		}
	}
}

func TestPostAccrualRecognitionJournal_SendsACC03FieldsAndEnvelopeOnEveryWrite(t *testing.T) {
	c, l := newJournalClients(t)
	id, err := c.PostAccrualRecognitionJournal(context.Background(), "t1", "le-1", "2026-01", "sch-1:2026-01", "p1",
		"audit fee — recognition 2026-01", "6100", "2100", 300, meta(domain.GLJournalTypeAccrual))
	if err != nil || id != "j-1" {
		t.Fatalf("PostAccrualRecognitionJournal = %q, %v", id, err)
	}
	w := l.recorded()
	if len(w) != 3 || w[0].path != "/v1/journals" || w[1].path != "/v1/journals/j-1/validate" || w[2].path != "/v1/journals/j-1/post" {
		t.Fatalf("expected create, validate, post; got %+v", w)
	}
	assertACC03Body(t, w[0].body, "ACCRUAL")
	assertLedgerEnvelope(t, w[0], "gl-journal-create:sch-1:2026-01")
	assertLedgerEnvelope(t, w[1], "gl-journal-validate:j-1")
	assertLedgerEnvelope(t, w[2], "gl-journal-post:j-1")
}

// A retried posting must present the same Idempotency-Key GL saw the first
// time, or the envelope's replay protection protects nothing.
func TestPostAccrualRecognitionJournal_RetryPresentsTheSameIdempotencyKey(t *testing.T) {
	c, l := newJournalClients(t)
	for i := 0; i < 2; i++ {
		if _, err := c.PostAccrualRecognitionJournal(context.Background(), "t1", "le-1", "2026-01", "sch-1:2026-01", "p1",
			"d", "6100", "2100", 300, meta(domain.GLJournalTypeAccrual)); err != nil {
			t.Fatal(err)
		}
	}
	w := l.recorded()
	if w[0].header.Get("Idempotency-Key") != w[3].header.Get("Idempotency-Key") {
		t.Fatalf("create keys differ across retries: %q vs %q", w[0].header.Get("Idempotency-Key"), w[3].header.Get("Idempotency-Key"))
	}
}

func TestPostAllocationAndMultiLineJournals_SendACC03Fields(t *testing.T) {
	ctx := context.Background()
	t.Run("allocation", func(t *testing.T) {
		c, l := newJournalClients(t)
		if _, err := c.PostAllocationJournal(ctx, "t1", "le-1", "2026-01", "run-1", "p1", "IT allocation", "5000", 100,
			[]domain.AllocationJournalLine{{AccountCode: "6100", Amount: 100}}, meta(domain.GLJournalTypeReclass)); err != nil {
			t.Fatal(err)
		}
		w := l.recorded()
		assertACC03Body(t, w[0].body, "RECLASS")
		assertLedgerEnvelope(t, w[0], "gl-journal-create:run-1")
	})
	t.Run("multi-line", func(t *testing.T) {
		c, l := newJournalClients(t)
		if _, err := c.PostMultiLineJournal(ctx, "t1", "le-1", "2026-01", "batch-1", "p1", "opening balances",
			[]domain.JournalLineInput{{AccountCode: "1000", DebitAmount: 5}, {AccountCode: "3000", CreditAmount: 5}}, meta(domain.GLJournalTypeOpening)); err != nil {
			t.Fatal(err)
		}
		w := l.recorded()
		assertACC03Body(t, w[0].body, "OPENING")
		assertLedgerEnvelope(t, w[0], "gl-journal-create:batch-1")
	})
}

func TestReverseGLJournal_SendsEnvelope(t *testing.T) {
	c, l := newJournalClients(t)
	id, err := c.ReverseGLJournal(context.Background(), "t1", "p1", "le-1", "j-1", "estimate revised", "rev-1")
	if err != nil || id != "j-rev" {
		t.Fatalf("ReverseGLJournal = %q, %v", id, err)
	}
	assertLedgerEnvelope(t, l.recorded()[0], "gl-journal-reverse:rev-1")
}
