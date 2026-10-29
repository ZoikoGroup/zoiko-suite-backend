package kernel

import (
	"context"
	"testing"
	"time"
)

// orchRepo serves the few reads the orchestration makes; everything else is the embedded nil Repo and would panic if used.
type orchRepo struct {
	Repo
	events []AccountingEvent
	books  []Book
	rules  map[string][]RuleSet // by book id
	cons   map[string]map[string]ConsequenceRecord
}

func (r *orchRepo) GetEvent(_ context.Context, _, id string) (*AccountingEvent, error) {
	for i := range r.events {
		if r.events[i].AccountingEventID == id {
			return &r.events[i], nil
		}
	}
	return nil, ErrNotFound
}
func (r *orchRepo) ListEvents(context.Context, string, EventFilter) ([]AccountingEvent, error) {
	return r.events, nil
}
func (r *orchRepo) ListBooks(context.Context, string, BookFilter) ([]Book, error) {
	return append([]Book(nil), r.books...), nil
}
func (r *orchRepo) ActiveRuleSets(_ context.Context, _, _, bookType, _, _ string, _ time.Time) ([]RuleSet, error) {
	for _, b := range r.books {
		if b.BookType == bookType {
			return r.rules[b.BookID], nil
		}
	}
	return nil, nil
}
func (r *orchRepo) EventConsequences(_ context.Context, _, ev string) (map[string]ConsequenceRecord, error) {
	if c := r.cons[ev]; c != nil {
		return c, nil
	}
	return map[string]ConsequenceRecord{}, nil
}

func orchBook(id, code, typ string) Book {
	return Book{BookID: id, BookCode: code, BookType: typ, AccountingFrameworkID: "IFRS", Status: BookActive, EffectiveFrom: d0}
}

func posted(j string) ConsequenceRecord {
	return ConsequenceRecord{Journals: []ConsequenceJournal{{JournalID: j, Basis: "PRIMARY"}}}
}

func TestEventIsIncompleteUntilEveryRequiredBookHasPosted(t *testing.T) {
	ev := *event(okFacts())
	ev.RegisteredAt = d0
	rule := []RuleSet{{Name: "r", Version: "1.0.0"}}
	r := &orchRepo{events: []AccountingEvent{ev},
		books: []Book{orchBook("b-stat", "STAT", BookStatutory), orchBook("b-mgmt", "MGMT", "MANAGEMENT"), orchBook("b-tax", "TAX", "TAX")},
		rules: map[string][]RuleSet{"b-stat": rule, "b-mgmt": rule}, // no rule for the tax book: it is not expected to carry this event
		cons:  map[string]map[string]ConsequenceRecord{}}
	svc := &OrchestrationService{Repo: r, Cons: r}
	status := func() *PostingStatus {
		t.Helper()
		st, err := svc.Status(context.Background(), tenant, ev.AccountingEventID)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	byBook := func(st *PostingStatus) map[string]string {
		m := map[string]string{}
		for _, b := range st.Books {
			m[b.BookCode] = b.Status
		}
		return m
	}

	// nothing posted yet: both required books are pending
	st := status()
	if st.Status != EventIncomplete || st.RequiredBooks != 2 || st.PostedBooks != 0 || len(st.Outstanding) != 2 ||
		byBook(st)["STAT"] != ConsPending || byBook(st)["MGMT"] != ConsPending || byBook(st)["TAX"] != ConsNotApplicable {
		t.Fatalf("%+v", st)
	}

	// one book has posted: the event is still incomplete and says which book has not
	r.cons[ev.AccountingEventID] = map[string]ConsequenceRecord{"b-stat": posted("j1")}
	st = status()
	if st.Status != EventIncomplete || st.PostedBooks != 1 || len(st.Outstanding) != 1 || st.Outstanding[0] != "MGMT" || byBook(st)["STAT"] != ConsPosted {
		t.Fatalf("a posted book must not hide the one that has not: %+v", st)
	}

	// the other book's attempt failed: an exception, still outstanding
	r.cons[ev.AccountingEventID]["b-mgmt"] = ConsequenceRecord{LastFailure: &ConsequenceFailure{ExecutionID: "x1", Status: "FAILED", Reason: "no rule"}}
	st = status()
	if st.Status != EventIncomplete || byBook(st)["MGMT"] != ConsException {
		t.Fatalf("%+v", st)
	}
	// a failure that was resolved without a posting leaves it pending, not complete
	r.cons[ev.AccountingEventID]["b-mgmt"] = ConsequenceRecord{LastFailure: &ConsequenceFailure{ExecutionID: "x1", Status: "RESOLVED"}}
	if st = status(); st.Status != EventIncomplete || byBook(st)["MGMT"] != ConsPending {
		t.Fatalf("%+v", st)
	}

	// both required books posted: complete
	r.cons[ev.AccountingEventID]["b-mgmt"] = posted("j2")
	if st = status(); st.Status != EventComplete || st.PostedBooks != 2 || len(st.Outstanding) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestOrchestrationEdges(t *testing.T) {
	ev := *event(okFacts())
	ev.RegisteredAt = d0
	rule := []RuleSet{{Name: "r", Version: "1.0.0"}}
	stat := orchBook("b-stat", "STAT", BookStatutory)
	r := &orchRepo{events: []AccountingEvent{ev}, books: []Book{stat}, rules: map[string][]RuleSet{"b-stat": rule}, cons: map[string]map[string]ConsequenceRecord{}}
	svc := &OrchestrationService{Repo: r, Cons: r}
	get := func() *PostingStatus {
		st, err := svc.Status(context.Background(), tenant, ev.AccountingEventID)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// two active rule sets claim the event: the book cannot post, and that is an exception, not a wait
	r.rules["b-stat"] = []RuleSet{{Name: "a", Version: "1"}, {Name: "b", Version: "1"}}
	if st := get(); st.Status != EventIncomplete || st.Books[0].Status != ConsException {
		t.Fatalf("%+v", st)
	}
	r.rules["b-stat"] = rule

	// a book that was not yet in force on the posting date does not hold the event back
	later := orchBook("b-new", "NEW", "MANAGEMENT")
	later.EffectiveFrom = d0.AddDate(5, 0, 0)
	r.books, r.rules["b-new"] = []Book{stat, later}, rule
	r.cons[ev.AccountingEventID] = map[string]ConsequenceRecord{"b-stat": posted("j1")}
	if st := get(); st.Status != EventComplete || st.RequiredBooks != 1 {
		t.Fatalf("%+v", st)
	}

	// no book needs the event at all
	r.books = []Book{orchBook("b-tax", "TAX", "TAX")}
	r.cons = map[string]map[string]ConsequenceRecord{}
	if st := get(); st.Status != EventNotApplicable || st.RequiredBooks != 0 {
		t.Fatalf("%+v", st)
	}

	// an unknown event
	if _, err := svc.Status(context.Background(), tenant, "nope"); err == nil {
		t.Fatal("an unknown event has no status")
	}
}

func TestIncompleteEventsListSkipsCompleteAndTooRecentOnes(t *testing.T) {
	rule := []RuleSet{{Name: "r", Version: "1.0.0"}}
	now := d0.AddDate(0, 0, 10)
	mk := func(id string, age time.Duration) AccountingEvent {
		e := *event(okFacts())
		e.AccountingEventID, e.RegisteredAt = id, now.Add(-age)
		return e
	}
	r := &orchRepo{
		events: []AccountingEvent{mk("fresh", time.Minute), mk("stuck", 2*time.Hour), mk("done", 3*time.Hour), mk("stuck2", 4*time.Hour)},
		books:  []Book{orchBook("b-stat", "STAT", BookStatutory), orchBook("b-mgmt", "MGMT", "MANAGEMENT")},
		rules:  map[string][]RuleSet{"b-stat": rule, "b-mgmt": rule},
		cons: map[string]map[string]ConsequenceRecord{
			"fresh":  {},
			"stuck":  {"b-stat": posted("j1")},
			"done":   {"b-stat": posted("j2"), "b-mgmt": posted("j3")},
			"stuck2": {},
		},
	}
	svc := &OrchestrationService{Repo: r, Cons: r, Clock: func() time.Time { return now }}
	res, err := svc.Incomplete(context.Background(), tenant, entity, 0, 0, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 4 || len(res.Incomplete) != 2 || res.Incomplete[0].AccountingEventID != "stuck" || res.Incomplete[1].AccountingEventID != "stuck2" {
		t.Fatalf("%+v", res)
	}
	// with no minimum age the one still on its way is listed too, and the limit caps the list
	if res, _ = svc.Incomplete(context.Background(), tenant, entity, 0, 0, 0); len(res.Incomplete) != 3 {
		t.Fatalf("%+v", res)
	}
	if res, _ = svc.Incomplete(context.Background(), tenant, entity, 0, 1, 0); len(res.Incomplete) != 1 || res.Incomplete[0].AccountingEventID != "fresh" {
		t.Fatalf("%+v", res)
	}
}
