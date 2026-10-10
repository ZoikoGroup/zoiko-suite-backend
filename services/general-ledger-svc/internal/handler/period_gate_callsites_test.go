package handler_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
)

// refRecorder records every PeriodRef a handler passes to the period check.
type refRecorder struct {
	mu   sync.Mutex
	refs []close.PeriodRef
}

func (c *refRecorder) CheckPeriodOpen(context.Context, string, string, string) error { return nil }
func (c *refRecorder) CheckPeriodOpenAt(_ context.Context, _ string, ref close.PeriodRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs = append(c.refs, ref)
	return nil
}

// TestPeriodCheckCallSites_CarryAPostingDate proves each of the 8 period-check
// call sites hands the shadow gate a posting date, or deliberately none (site 7).
func TestPeriodCheckCallSites_CarryAPostingDate(t *testing.T) {
	d := func(y int, m time.Month, day int) string { return domain.NewDate(y, m, day).String() }
	today := time.Now().UTC().Truncate(24 * time.Hour)

	type tc struct {
		name     string
		setup    func(s *stubStore)
		method   string
		path     string
		body     any
		wantCode int
		// wantDates is the posting date of each PeriodRef, in call order;
		// "" means the site deliberately has no date (shadow skips no_date).
		wantDates []string
	}
	validated := func(id string) func(*stubStore) {
		return func(s *stubStore) {
			s.journals[id] = &domain.JournalHeader{
				JournalID: id, TenantID: testTenantID, LegalEntityID: "e1", FiscalPeriod: "2026-07",
				Status: domain.JournalStatusValidated, ApprovalStatus: domain.ApprovalStatusPostingRequested,
				PostingDate: domain.NewDate(2026, time.July, 15), TransactionDate: domain.NewDate(2026, time.July, 14),
			}
		}
	}
	finalized := func(id string) func(*stubStore) {
		return func(s *stubStore) {
			s.journals[id] = &domain.JournalHeader{
				JournalID: id, TenantID: testTenantID, LegalEntityID: "e1", FiscalPeriod: "2026-07",
				Status:      domain.JournalStatusFinalized,
				PostingDate: domain.NewDate(2026, time.July, 15), TransactionDate: domain.NewDate(2026, time.July, 14),
			}
			s.lines[id] = []domain.JournalLine{{AccountCode: "1000", DebitAmount: 100}, {AccountCode: "4000", CreditAmount: 100}}
		}
	}
	cases := []tc{
		{"site1 CreateJournal: request posting_date", nil, http.MethodPost, "/v1/journals/", validCreateReq(), http.StatusCreated,
			[]string{d(2026, 7, 31)}},
		{"site2 PostJournal: header posting_date", validated("j2"), http.MethodPost, "/v1/journals/j2/post", nil, http.StatusOK,
			[]string{d(2026, 7, 15)}},
		{"site3 ReverseJournal: reversal's own posting date (today)", finalized("j3"), http.MethodPost, "/v1/journals/j3/reverse",
			domain.ReverseJournalRequest{Reason: "r", CorrelationID: "c-rev"}, http.StatusCreated,
			[]string{today.Format(domain.DateLayout)}},
		{"site4+5 PostAccountingEvent: request date, then header date at commit", nil, http.MethodPost, "/v1/postings/events",
			validPostEventReq(), http.StatusCreated, []string{d(2026, 7, 1), d(2026, 7, 1)}},
		{"site6 PostApprovedJournal: header posting_date", validated("j6"), http.MethodPost, "/v1/postings/journals",
			domain.PostApprovedJournalRequest{JournalID: "j6"}, http.StatusOK, []string{d(2026, 7, 15)}},
		{"site7 CreateReversalPosting: NO date (skipped, not guessed)", finalized("j7"), http.MethodPost, "/v1/postings/reversals",
			domain.CreateReversalPostingRequest{OriginalJournalID: "j7", Reason: "r"}, http.StatusCreated, []string{""}},
		{"site8 ReprocessFailedPosting (VALIDATED): header posting_date", func(s *stubStore) {
			validated("j8")(s)
			jid := "j8"
			s.postingExecutions["x-8"] = &domain.PostingExecution{ExecutionID: "x-8", TenantID: testTenantID, LegalEntityID: "e1",
				Status: domain.PostingExecutionStatusFailed, JournalID: &jid}
		}, http.MethodPost, "/v1/postings/x-8/reprocess", nil, http.StatusOK, []string{d(2026, 7, 15)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStubStore()
			if c.setup != nil {
				c.setup(s)
			}
			rr := &refRecorder{}
			r := newRouterWithClose(s, &stubPublisher{}, &stubAuthZ{}, rr)
			rec := doRequest(r, c.method, c.path, c.body, "principal-1")
			if rec.Code != c.wantCode {
				t.Fatalf("status %d (want %d): %s", rec.Code, c.wantCode, rec.Body.String())
			}
			if len(rr.refs) != len(c.wantDates) {
				t.Fatalf("expected %d period checks, got %d: %+v", len(c.wantDates), len(rr.refs), rr.refs)
			}
			for i, ref := range rr.refs {
				if ref.PostingDate.String() != c.wantDates[i] {
					t.Errorf("check %d: posting date %q, want %q", i, ref.PostingDate.String(), c.wantDates[i])
				}
				if ref.LegalEntityID != "e1" || ref.PeriodName != "2026-07" {
					t.Errorf("check %d: legacy inputs changed: %+v", i, ref)
				}
			}
		})
	}
}
