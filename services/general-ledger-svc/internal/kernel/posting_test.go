package kernel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func prop(debit, credit string, lines int) *Proposal {
	p := &Proposal{TotalDebitFunc: debit, TotalCreditFunc: credit}
	for i := 0; i < lines; i++ {
		p.Lines = append(p.Lines, ProposalLine{LineNo: i + 1, AmountFunctional: "1.00"})
	}
	return p
}

func TestCheckBalanced(t *testing.T) {
	if err := checkBalanced(prop("120.00", "120.00", 3)); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*Proposal{
		"one cent out":  prop("120.01", "120.00", 3),
		"not a number":  prop("x", "120.00", 3),
		"nothing":       prop("0", "0", 3),
		"a single line": prop("5.00", "5.00", 1),
	} {
		e, ok := AsException(checkBalanced(p))
		if !ok || e.Family != FamBal {
			t.Fatalf("%s: %v", name, e)
		}
	}
}

func TestCheckLedgerPrecision(t *testing.T) {
	ok := &Proposal{Lines: []ProposalLine{{LineNo: 1, AmountFunctional: "120.00"}, {LineNo: 2, AmountFunctional: "7"}, {LineNo: 3, AmountFunctional: "0.500"}}}
	if err := checkLedgerPrecision(ok); err != nil {
		t.Fatalf("trailing zeros are not precision: %v", err)
	}
	bad := &Proposal{Lines: []ProposalLine{{LineNo: 2, AmountFunctional: "1.005"}}}
	if e, isExc := AsException(checkLedgerPrecision(bad)); !isExc || e.Family != FamData || e.Code != "LEDGER_PRECISION" {
		t.Fatalf("%v", e)
	}
}

func TestFiscalPeriodOf(t *testing.T) {
	if got := FiscalPeriodOf(time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)); got != "2026-03" {
		t.Fatal(got)
	}
	// the period is the UTC day the posting date names, whatever zone the caller's clock is in
	if got := FiscalPeriodOf(time.Date(2026, 4, 1, 0, 30, 0, 0, time.FixedZone("x", -3600*5))); got != "2026-04" {
		t.Fatal(got)
	}
}

// existingRepo answers FindPosting with a stored consequence and fails the test on anything that would read or write more.
type existingRepo struct {
	Repo
	t *testing.T
}

func (r existingRepo) FindPosting(_ context.Context, _, ev, book, basis string) (*PostResult, error) {
	return &PostResult{AccountingEventID: ev, BookID: book, PostingBasis: basis, JournalID: "j-1", RuleVersion: "1.0.0"}, nil
}
func (r existingRepo) GetEvent(context.Context, string, string) (*AccountingEvent, error) {
	r.t.Fatal("an existing consequence must be returned without reading the event")
	return nil, nil
}

func TestPostEventReturnsTheExistingConsequenceFirst(t *testing.T) {
	s := &Service{Repo: existingRepo{t: t}}
	res, err := s.PostEvent(context.Background(), "t", Actor{}, PostRequest{AccountingEventID: "8b6c1a9e-6b0e-4a25-9d0e-4f6f1d1c2a11", BookID: "0f9b1c52-1e0b-4d0e-8f63-3a2b7d9f4c11"}, nil, nil)
	if err != nil || !res.Existing || res.JournalID != "j-1" || res.PostingBasis != DefaultPostingBasis {
		t.Fatalf("%v %v", res, err)
	}
}

func TestPostEventRejectsAMalformedRequest(t *testing.T) {
	s := &Service{Repo: existingRepo{t: t}}
	valid := "8b6c1a9e-6b0e-4a25-9d0e-4f6f1d1c2a11"
	for name, req := range map[string]PostRequest{
		"no event":         {BookID: valid},
		"no book":          {AccountingEventID: valid},
		"bad basis":        {AccountingEventID: valid, BookID: valid, PostingBasis: "not ok!"},
		"override w/o why": {AccountingEventID: valid, BookID: valid, OverrideSoftClose: true},
	} {
		_, err := s.PostEvent(context.Background(), "t", Actor{}, req, nil, nil)
		var e *Exception
		if !errors.As(err, &e) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
