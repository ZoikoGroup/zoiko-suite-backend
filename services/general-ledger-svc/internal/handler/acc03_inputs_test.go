package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// Coverage for the ACC-03 required business/source inputs added in
// migration 000006: journal type, transaction/posting dates, currency, book,
// dimensions and evidence refs.

func postJournal(t *testing.T, req domain.CreateJournalRequest) (*stubStore, int, domain.JournalWithLines) {
	t.Helper()
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")

	var body domain.JournalWithLines
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
	}
	return store, rec.Code, body
}

func TestACC03_InputsArePersistedOnTheJournal(t *testing.T) {
	req := validCreateReq()
	book := "BOOK-STAT-GB"
	basis := "IFRS"
	req.BookID = &book
	req.ReportingBasis = &basis
	req.EvidenceRefs = []string{"doc-invoice-991"}
	req.Lines[0].Dimensions = domain.Dimensions{"cost_centre": "CC-100", "project": "PRJ-7"}

	_, code, got := postJournal(t, req)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}

	if got.JournalType != domain.JournalTypeStandard {
		t.Errorf("journal_type = %q, want STANDARD", got.JournalType)
	}
	if got.TransactionDate.String() != "2026-07-28" || got.PostingDate.String() != "2026-07-31" {
		t.Errorf("dates = %s / %s, want 2026-07-28 / 2026-07-31", got.TransactionDate, got.PostingDate)
	}
	if got.CurrencyCode != "GBP" {
		t.Errorf("currency_code = %q, want GBP", got.CurrencyCode)
	}
	if got.BookID == nil || *got.BookID != book {
		t.Errorf("book_id = %v, want %q", got.BookID, book)
	}
	if len(got.EvidenceRefs) != 1 || got.EvidenceRefs[0] != "doc-invoice-991" {
		t.Errorf("evidence_refs = %v, want [doc-invoice-991]", got.EvidenceRefs)
	}
	if got.Lines[0].Dimensions["cost_centre"] != "CC-100" {
		t.Errorf("dimensions = %v, want cost_centre CC-100", got.Lines[0].Dimensions)
	}
}

// Each required input is refused by name, so a caller adopting the contract can
// fix one field at a time instead of guessing at a generic 400.
func TestACC03_EachRequiredInputIsRefusedByName(t *testing.T) {
	cases := []struct {
		field  string
		mutate func(*domain.CreateJournalRequest)
	}{
		{"journal_type", func(r *domain.CreateJournalRequest) { r.JournalType = "" }},
		{"transaction_date", func(r *domain.CreateJournalRequest) { r.TransactionDate = domain.Date{} }},
		{"posting_date", func(r *domain.CreateJournalRequest) { r.PostingDate = domain.Date{} }},
		{"currency_code", func(r *domain.CreateJournalRequest) { r.CurrencyCode = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			req := validCreateReq()
			tc.mutate(&req)

			store, code, _ := postJournal(t, req)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", code)
			}
			if len(store.journals) != 0 {
				t.Error("a journal was written despite a missing required input")
			}
		})
	}
}

func TestACC03_UnknownJournalTypeIsRefused(t *testing.T) {
	req := validCreateReq()
	req.JournalType = "ACRUAL" // plausible typo for ACCRUAL

	_, code, _ := postJournal(t, req)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unrecognised journal type", code)
	}
}

// UNSPECIFIED is the marker migration 000006 backfilled onto pre-contract rows.
// Accepting it on a new journal would let a caller opt out of the input.
func TestACC03_UnspecifiedJournalTypeIsNotAcceptable(t *testing.T) {
	req := validCreateReq()
	req.JournalType = domain.JournalTypeUnspecified

	_, code, _ := postJournal(t, req)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — UNSPECIFIED is a backfill marker, not a choice", code)
	}
}

func TestACC03_CurrencyMustBeThreeUppercaseLetters(t *testing.T) {
	for _, bad := range []string{"gbp", "GB", "GBPX", "G8P", "   "} {
		req := validCreateReq()
		req.CurrencyCode = bad

		_, code, _ := postJournal(t, req)
		if code != http.StatusBadRequest {
			t.Errorf("currency %q -> %d, want 400", bad, code)
		}
	}
}

// A document dated after the day it posts is a data-entry error every time.
// The reverse — posted later than dated — is ordinary and must be accepted.
func TestACC03_PostingDateCannotPrecedeTransactionDate(t *testing.T) {
	req := validCreateReq()
	req.TransactionDate = domain.NewDate(2026, time.July, 31)
	req.PostingDate = domain.NewDate(2026, time.July, 28)

	_, code, _ := postJournal(t, req)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}

	req.PostingDate = req.TransactionDate
	if _, code, _ := postJournal(t, req); code != http.StatusCreated {
		t.Fatalf("same-day posting -> %d, want 201", code)
	}
}

// §4/§5 class S: a server-resolved value on the envelope outranks the body's.
func TestACC03_EnvelopeBookIDOutranksBody(t *testing.T) {
	req := validCreateReq()
	bodyBook := "BOOK-FROM-BODY"
	req.BookID = &bodyBook

	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestWithHeaders(r, http.MethodPost, "/v1/journals/", req, "principal-1", map[string]string{
		"X-Book-Id":       "BOOK-FROM-ENVELOPE",
		"X-Evidence-Refs": "doc-from-envelope",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var got domain.JournalWithLines
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.BookID == nil || *got.BookID != "BOOK-FROM-ENVELOPE" {
		t.Fatalf("book_id = %v, want the envelope value to win", got.BookID)
	}
}

// The body and the envelope carry different evidence in practice — the envelope
// the request-wide document, the body the specific schedules — so both survive.
func TestACC03_EvidenceRefsAreUnionedNotReplaced(t *testing.T) {
	req := validCreateReq()
	req.EvidenceRefs = []string{"doc-schedule-1", "doc-shared"}

	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestWithHeaders(r, http.MethodPost, "/v1/journals/", req, "principal-1", map[string]string{
		"X-Evidence-Refs": "doc-invoice, doc-shared",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}

	var got domain.JournalWithLines
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	want := []string{"doc-schedule-1", "doc-shared", "doc-invoice"}
	if len(got.EvidenceRefs) != len(want) {
		t.Fatalf("evidence_refs = %v, want %v (union, deduplicated)", got.EvidenceRefs, want)
	}
	for i, w := range want {
		if got.EvidenceRefs[i] != w {
			t.Fatalf("evidence_refs = %v, want %v", got.EvidenceRefs, want)
		}
	}
}

// A reversal must land in the same book and currency as what it reverses, or it
// does not net the original out.
func TestACC03_ReversalInheritsBookCurrencyAndDocumentDate(t *testing.T) {
	store := newStubStore()
	book := "BOOK-STAT-GB"
	original := &domain.JournalHeader{
		JournalID:       "j-1",
		TenantID:        testTenantID,
		LegalEntityID:   "e1",
		FiscalPeriod:    "2026-07",
		Status:          domain.JournalStatusFinalized,
		JournalType:     domain.JournalTypeStandard,
		TransactionDate: domain.NewDate(2026, time.July, 28),
		PostingDate:     domain.NewDate(2026, time.July, 31),
		CurrencyCode:    "GBP",
		BookID:          &book,
		EvidenceRefs:    []string{"doc-invoice-991"},
	}
	store.journals["j-1"] = original
	store.lines["j-1"] = []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100, Dimensions: domain.Dimensions{"cost_centre": "CC-100"}},
		{AccountCode: "4000", CreditAmount: 100},
	}

	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j-1/reverse",
		domain.ReverseJournalRequest{Reason: "posted in error", CorrelationID: "rev-1"}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var got domain.JournalWithLines
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	if got.JournalType != domain.JournalTypeReversal {
		t.Errorf("journal_type = %q, want REVERSAL", got.JournalType)
	}
	if got.CurrencyCode != "GBP" {
		t.Errorf("currency_code = %q, want the original's GBP", got.CurrencyCode)
	}
	if got.BookID == nil || *got.BookID != book {
		t.Errorf("book_id = %v, want the original's %q", got.BookID, book)
	}
	// The document has not changed, so its date has not either.
	if got.TransactionDate.String() != "2026-07-28" {
		t.Errorf("transaction_date = %s, want the original's 2026-07-28", got.TransactionDate)
	}
	// The reversal reaches the ledger now, not on the day the original did.
	if got.PostingDate.Before(original.PostingDate.Time) {
		t.Errorf("posting_date = %s, want on or after the original's %s", got.PostingDate, original.PostingDate)
	}
	if got.Lines[0].Dimensions["cost_centre"] != "CC-100" {
		t.Errorf("dimensions = %v, want the original's axes preserved", got.Lines[0].Dimensions)
	}
}

func TestACC03_DateRejectsNonISOFormats(t *testing.T) {
	for _, bad := range []string{`"28/07/2026"`, `"2026-07-28T00:00:00Z"`, `"July 28 2026"`, `"2026-13-01"`} {
		var d domain.Date
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("%s was accepted as a date; want a refusal", bad)
		}
	}

	var d domain.Date
	if err := json.Unmarshal([]byte(`"2026-07-28"`), &d); err != nil {
		t.Fatalf("ISO date refused: %v", err)
	}
	if d.String() != "2026-07-28" {
		t.Fatalf("round-trip = %s, want 2026-07-28", d)
	}
}

func TestACC03_DateOmittedMarshalsAsNullNotYearZero(t *testing.T) {
	b, err := json.Marshal(domain.Date{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "null" {
		t.Fatalf("zero Date marshals as %s, want null", b)
	}
}
