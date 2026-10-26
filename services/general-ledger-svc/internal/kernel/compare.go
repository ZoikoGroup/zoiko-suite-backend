package kernel

import (
	"math/big"
	"sort"
)

// Multi-book comparison (standard §14, acceptance A10): one source event can produce a different, rule-approved journal set in each
// book without duplicating the business record. The comparison lays the books' consequences of one event side by side.

// BookConsequence is what one book holds for the event: the net of its posting journals by account.
type BookConsequence struct {
	BookID      string            `json:"book_id"`
	BookCode    string            `json:"book_code"`
	BookType    string            `json:"book_type"`
	Framework   string            `json:"accounting_framework_id"`
	Currency    string            `json:"functional_currency"`
	RuleName    string            `json:"posting_rule,omitempty"`
	RuleVersion string            `json:"posting_rule_version,omitempty"`
	Journals    []string          `json:"journal_numbers"`
	Net         map[string]string `json:"net_by_account"` // debit minus credit
	TotalDebit  string            `json:"total_debit"`
	Reversed    bool              `json:"reversed,omitempty"`
}

// BookDifference is an account whose net differs between books.
type BookDifference struct {
	AccountCode string            `json:"account_code"`
	ByBook      map[string]string `json:"net_by_book"`
}

// BookComparison is the side-by-side view of an event's consequences.
type BookComparison struct {
	AccountingEventID string            `json:"accounting_event_id"`
	Books             []BookConsequence `json:"books"`
	Differences       []BookDifference  `json:"differences"`
	// SingleSource is true when every book posting traces to the same accounting event: the source record exists once.
	SingleSource bool `json:"single_source_record"`
}

// CompareBooks builds the comparison from a posting trace. Reversed postings are listed, not netted into the figures of a book.
func CompareBooks(tr *Trace) *BookComparison {
	out := &BookComparison{Books: []BookConsequence{}, Differences: []BookDifference{}, SingleSource: tr.Event != nil}
	if tr.Event != nil {
		out.AccountingEventID = tr.Event.AccountingEventID
	}
	accounts := map[string]bool{}
	for _, p := range tr.Postings {
		if p.Book == nil {
			continue
		}
		bc := BookConsequence{BookID: p.Book.BookID, BookCode: p.Book.BookCode, BookType: p.Book.BookType, Framework: p.Book.Framework, Currency: p.Book.FunctionalCurrency,
			Journals: []string{}, Net: map[string]string{}}
		if p.Rule != nil {
			bc.RuleName, bc.RuleVersion = p.Rule.Name, p.Rule.Version
		}
		net := map[string]*big.Rat{}
		td := new(big.Rat)
		for _, j := range p.Journals {
			if j.Role != "POSTING" {
				continue
			}
			bc.Journals = append(bc.Journals, j.JournalNumber)
			if j.Status == "REVERSED" {
				bc.Reversed = true
				continue
			}
			for _, l := range j.Lines {
				d, c := dec(l.Debit), dec(l.Credit)
				addTo(net, l.AccountCode, new(big.Rat).Sub(d, c))
				td.Add(td, d)
			}
		}
		for k, v := range net {
			if v.Sign() != 0 {
				bc.Net[k] = v.FloatString(2)
				accounts[k] = true
			}
		}
		bc.TotalDebit = td.FloatString(2)
		out.Books = append(out.Books, bc)
	}
	sort.Slice(out.Books, func(i, j int) bool { return out.Books[i].BookCode < out.Books[j].BookCode })
	names := make([]string, 0, len(accounts))
	for a := range accounts {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		d := BookDifference{AccountCode: a, ByBook: map[string]string{}}
		same := true
		first := ""
		for i, b := range out.Books {
			v := b.Net[a]
			if v == "" {
				v = "0.00"
			}
			d.ByBook[b.BookCode] = v
			if i == 0 {
				first = v
			} else if v != first {
				same = false
			}
		}
		if !same {
			out.Differences = append(out.Differences, d)
		}
	}
	return out
}
