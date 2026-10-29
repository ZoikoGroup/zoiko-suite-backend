package kernel

import (
	"context"
	"sort"
	"time"
)

// Multi-book posting orchestration (standard §4, §37): one accounting event can have a consequence in several books of an entity.
// Each posting unit is atomic by itself; there is no distributed transaction across books. So the completion of the whole is
// explicit: an event is COMPLETE when every book that is required to carry it has posted, and INCOMPLETE — with the books that have not
// and why — until then. A book that has posted never hides one that has not.

// Consequence states of one book for one event.
const (
	ConsPosted        = "POSTED"
	ConsException     = "EXCEPTION"
	ConsPending       = "PENDING"
	ConsNotApplicable = "NOT_APPLICABLE"
)

// Completion states of an event across its books.
const (
	EventComplete      = "COMPLETE"
	EventIncomplete    = "INCOMPLETE"
	EventNotApplicable = "NOT_APPLICABLE"
)

// ConsequenceJournal is one posting of an event in a book.
type ConsequenceJournal struct {
	JournalID string `json:"journal_id"`
	Basis     string `json:"posting_basis"`
}

// ConsequenceFailure is the latest failed attempt to post an event in a book.
type ConsequenceFailure struct {
	ExecutionID string    `json:"posting_execution_id"`
	Status      string    `json:"status"`
	Reason      string    `json:"failure_reason,omitempty"`
	At          time.Time `json:"at"`
}

// ConsequenceRecord is what the ledger holds about one event in one book.
type ConsequenceRecord struct {
	Journals    []ConsequenceJournal
	LastFailure *ConsequenceFailure
}

// ConsequenceRepo reads what has been posted for an event, by book.
type ConsequenceRepo interface {
	EventConsequences(ctx context.Context, tenant, eventID string) (map[string]ConsequenceRecord, error)
}

// BookOutcome is the state of one book for one event.
type BookOutcome struct {
	BookID      string               `json:"accounting_book_id"`
	BookCode    string               `json:"book_code"`
	BookType    string               `json:"book_type"`
	Framework   string               `json:"accounting_framework_id"`
	Status      string               `json:"status"`
	RuleSet     string               `json:"posting_rule,omitempty"`
	Journals    []ConsequenceJournal `json:"journals,omitempty"`
	LastFailure *ConsequenceFailure  `json:"last_failure,omitempty"`
	Detail      string               `json:"detail,omitempty"`
}

// PostingStatus is the completion of one accounting event across the books of its entity.
type PostingStatus struct {
	AccountingEventID string        `json:"accounting_event_id"`
	EventType         string        `json:"event_type"`
	LegalEntityID     string        `json:"legal_entity_id"`
	RegisteredAt      time.Time     `json:"registered_at"`
	Status            string        `json:"status"`
	RequiredBooks     int           `json:"required_books"`
	PostedBooks       int           `json:"posted_books"`
	Books             []BookOutcome `json:"books"`
	// Outstanding names the books still to post, so an operator knows where to look first.
	Outstanding []string `json:"outstanding_book_codes,omitempty"`
}

// OrchestrationService answers how far an event's consequences have come.
type OrchestrationService struct {
	Repo  Repo
	Cons  ConsequenceRepo
	Clock func() time.Time
}

func (s *OrchestrationService) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func eventPostingDate(ev *AccountingEvent) time.Time {
	switch {
	case ev.PostingDateRequested != nil:
		return today(*ev.PostingDateRequested)
	case ev.DocumentDate != nil:
		return today(*ev.DocumentDate)
	}
	return today(ev.OccurredAt)
}

// Status reports the completion of one event. The books that must carry it are the entity's books in force on the posting date for which
// a rule set applies to the event; a book with no rule for this kind of event is not expected to carry it and does not hold it back.
func (s *OrchestrationService) Status(ctx context.Context, tenant, eventID string) (*PostingStatus, error) {
	ev, err := s.Repo.GetEvent(ctx, tenant, eventID)
	if err != nil {
		return nil, err
	}
	books, err := s.Repo.ListBooks(ctx, tenant, BookFilter{LegalEntityID: ev.LegalEntityID, Status: BookActive, Limit: 200})
	if err != nil {
		return nil, err
	}
	return s.statusOf(ctx, tenant, ev, books)
}

func (s *OrchestrationService) statusOf(ctx context.Context, tenant string, ev *AccountingEvent, books []Book) (*PostingStatus, error) {
	cons, err := s.Cons.EventConsequences(ctx, tenant, ev.AccountingEventID)
	if err != nil {
		return nil, err
	}
	pd := eventPostingDate(ev)
	st := &PostingStatus{AccountingEventID: ev.AccountingEventID, EventType: ev.EventType, LegalEntityID: ev.LegalEntityID, RegisteredAt: ev.RegisteredAt, Books: []BookOutcome{}}
	sort.Slice(books, func(i, j int) bool { return books[i].BookCode < books[j].BookCode })
	for i := range books {
		bk := &books[i]
		bc := BookOutcome{BookID: bk.BookID, BookCode: bk.BookCode, BookType: bk.BookType, Framework: bk.AccountingFrameworkID}
		rec := cons[bk.BookID]
		if !bk.InForce(pd) {
			// a book that was not in force on the posting date is not expected to carry the event, but anything it did post is shown
			bc.Status, bc.Detail, bc.Journals = ConsNotApplicable, "the book was not in force on "+pd.Format("2006-01-02"), rec.Journals
			st.Books = append(st.Books, bc)
			continue
		}
		sets, err := s.Repo.ActiveRuleSets(ctx, tenant, ev.EventType, bk.BookType, bk.AccountingFrameworkID, bk.Jurisdiction, pd)
		if err != nil {
			return nil, err
		}
		sets = preferJurisdiction(sets, bk.Jurisdiction)
		if len(sets) == 0 && len(rec.Journals) == 0 {
			bc.Status, bc.Detail = ConsNotApplicable, "no active rule set for "+ev.EventType+" in this book"
			st.Books = append(st.Books, bc)
			continue
		}
		if len(sets) == 1 {
			bc.RuleSet = sets[0].Name + " " + sets[0].Version
		}
		st.RequiredBooks++
		bc.Journals, bc.LastFailure = rec.Journals, rec.LastFailure
		switch {
		case len(rec.Journals) > 0:
			bc.Status = ConsPosted
			st.PostedBooks++
		case len(sets) > 1:
			bc.Status, bc.Detail = ConsException, "more than one active rule set claims this event"
		case rec.LastFailure != nil && (rec.LastFailure.Status == "FAILED" || rec.LastFailure.Status == "QUARANTINED"):
			bc.Status, bc.Detail = ConsException, "the last attempt to post failed"
		default:
			bc.Status = ConsPending
			if rec.LastFailure != nil {
				bc.Detail = "an earlier failure was resolved; the posting has not been made"
			}
		}
		if bc.Status != ConsPosted {
			st.Outstanding = append(st.Outstanding, bk.BookCode)
		}
		st.Books = append(st.Books, bc)
	}
	switch {
	case st.RequiredBooks == 0:
		st.Status = EventNotApplicable
	case st.PostedBooks == st.RequiredBooks:
		st.Status = EventComplete
	default:
		st.Status = EventIncomplete
	}
	return st, nil
}

// IncompleteResult is the incomplete events among the most recent ones of an entity.
type IncompleteResult struct {
	Scanned    int             `json:"events_scanned"`
	Incomplete []PostingStatus `json:"incomplete_events"`
}

// MaxIncompleteScan bounds how many recent events one call looks at.
const MaxIncompleteScan = 500

// Incomplete looks through the entity's most recent events (newest first, at most scan of them, and only those registered at least
// minAge ago so an event still on its way is not reported) and returns up to limit that are not complete.
func (s *OrchestrationService) Incomplete(ctx context.Context, tenant, entity string, scan, limit int, minAge time.Duration) (*IncompleteResult, error) {
	if scan < 1 || scan > MaxIncompleteScan {
		scan = 100
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	evs, err := s.Repo.ListEvents(ctx, tenant, EventFilter{LegalEntityID: entity, Limit: scan})
	if err != nil {
		return nil, err
	}
	books := map[string][]Book{}
	res := &IncompleteResult{Incomplete: []PostingStatus{}}
	cutoff := s.now().Add(-minAge)
	for i := range evs {
		ev := &evs[i]
		res.Scanned++
		if minAge > 0 && ev.RegisteredAt.After(cutoff) {
			continue
		}
		bs, ok := books[ev.LegalEntityID]
		if !ok {
			if bs, err = s.Repo.ListBooks(ctx, tenant, BookFilter{LegalEntityID: ev.LegalEntityID, Status: BookActive, Limit: 200}); err != nil {
				return nil, err
			}
			books[ev.LegalEntityID] = bs
		}
		st, err := s.statusOf(ctx, tenant, ev, append([]Book(nil), bs...))
		if err != nil {
			return nil, err
		}
		if st.Status == EventIncomplete {
			res.Incomplete = append(res.Incomplete, *st)
			if len(res.Incomplete) >= limit {
				break
			}
		}
	}
	return res, nil
}
