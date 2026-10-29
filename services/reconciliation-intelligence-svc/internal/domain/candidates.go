package domain

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// CandidateSearchOutcome is what the search for a book-side counterpart of
// an UNMATCHED statement line found (A2a). It is stored on every searched
// line, so "searched, nothing found" is a recorded result, never the absence
// of one.
type CandidateSearchOutcome string

const (
	// CandidateWithinDateTolerance: exactly one journal, dated within the
	// governed date tolerance. Proposed as AUTO_MATCH.
	CandidateWithinDateTolerance CandidateSearchOutcome = "WITHIN_DATE_TOLERANCE"
	// CandidateDateSkew: no journal within the tolerance, exactly one beyond
	// it inside the search horizon. Proposed for manual review as DATE_SKEW.
	CandidateDateSkew CandidateSearchOutcome = "DATE_SKEW"
	// CandidateAmbiguous: more than one journal qualifies, or one journal
	// qualifies for more than one line. Nothing is proposed; a human picks.
	CandidateAmbiguous CandidateSearchOutcome = "AMBIGUOUS"
	// CandidateNone: the search ran and no journal qualifies.
	CandidateNone CandidateSearchOutcome = "NO_CANDIDATE"
	// CandidateNoCashAccount: the line carries no gl_cash_account_code, so
	// there is no account to search — and bank-reconciliation-svc could not
	// verify a journal match for it either.
	CandidateNoCashAccount CandidateSearchOutcome = "NO_CASH_ACCOUNT"
)

const (
	HeuristicCandidateWithinDateTolerance HeuristicRule = "CANDIDATE_JOURNAL_WITHIN_DATE_TOLERANCE"
	HeuristicCandidateDateSkew            HeuristicRule = "CANDIDATE_JOURNAL_DATE_SKEW"
	HeuristicCandidateAmbiguous           HeuristicRule = "CANDIDATE_JOURNAL_AMBIGUOUS"
)

// Fixed labels, like the other rules' (types.go): a named rule fired, not a
// statistical estimate. A single same-amount journal on the line's own cash
// account inside the governed window is the strongest evidence this service
// produces; the same journal outside it is weaker.
const (
	candidateWithinToleranceConfidence = 90.0
	candidateDateSkewConfidence        = 65.0
	candidateAmbiguousConfidence       = 50.0
)

// MetricCandidateDateTolerance is the financial-control-svc TolerancePolicy
// metric whose date_tolerance_days decides AUTO_MATCH against DATE_SKEW. It
// is governed, versioned and per (tenant, legal entity), like the write-off
// tolerance; nothing here falls back to a default.
const MetricCandidateDateTolerance = "RECONCILIATION_CANDIDATE_DATE_MATCH"

// DateTolerance is the governed date tolerance used for a candidate search:
// a journal dated at most Days calendar days from the statement date is
// within it. Version identifies the approved policy row.
type DateTolerance struct {
	Days    int
	Version int
}

// LedgerEntry is one FINALIZED general-ledger-svc ledger entry on a cash
// account, as read for a candidate search. Amounts are exact.
type LedgerEntry struct {
	JournalID       string
	TenantID        string
	LegalEntityID   string
	AccountCode     string
	CurrencyCode    string
	TransactionDate string // YYYY-MM-DD
	Debit           *big.Rat
	Credit          *big.Rat
}

// CandidateSearch is the input to the counterpart search for one run: the
// governed tolerance, the outer bound on how far from the statement date a
// DATE_SKEW candidate may lie, and the FINALIZED entries read for each cash
// account the run's lines use.
type CandidateSearch struct {
	Tolerance        DateTolerance
	HorizonDays      int
	EntriesByAccount map[string][]LedgerEntry
}

// candidateJournal is one journal's net movement on a line's cash account.
type candidateJournal struct {
	journalID string
	date      string
	days      int
	net       *big.Rat
}

// lineSearch is the per-line result before the cross-line exclusivity pass.
type lineSearch struct {
	outcome    CandidateSearchOutcome
	proposed   *candidateJournal
	candidates []candidateJournal // every qualifying journal, for an AMBIGUOUS rationale
}

// searchCandidates finds, for each line, the journals bank-reconciliation-svc
// would accept as its match (verifyJournalMatches): of this tenant and the
// line's legal entity, posting to the line's gl_cash_account_code, with a net
// movement on it — debits minus credits — exactly equal to the line's signed
// amount. FINALIZED is guaranteed by the read (journal_status=FINALIZED).
// Currency is required to match too, which bank-reconciliation-svc does not
// check for journals: a same-figure journal in another currency is not this
// line's book side.
func searchCandidates(lines []BankStatementLine, amounts []*big.Rat, searched map[int]bool, tenantID string, search *CandidateSearch) (map[int]lineSearch, error) {
	out := make(map[int]lineSearch, len(searched))
	proposedBy := map[string][]int{}

	for i, line := range lines {
		if !searched[i] {
			continue
		}
		if line.GLCashAccountCode == "" {
			out[i] = lineSearch{outcome: CandidateNoCashAccount}
			continue
		}
		stmt, err := time.Parse(dateLayout, line.StatementDate)
		if err != nil {
			return nil, fmt.Errorf("statement line %s: statement date %q: %w", line.StatementLineID, line.StatementDate, err)
		}

		type acc struct {
			date       string
			net        *big.Rat
			wrongScope bool
			mixedDates bool
			hasEntries bool
		}
		byJournal := map[string]*acc{}
		for _, e := range search.EntriesByAccount[line.GLCashAccountCode] {
			if e.AccountCode != line.GLCashAccountCode {
				continue
			}
			a := byJournal[e.JournalID]
			if a == nil {
				a = &acc{date: e.TransactionDate, net: new(big.Rat)}
				byJournal[e.JournalID] = a
			}
			a.hasEntries = true
			// Any entry of the journal outside the line's scope disqualifies
			// the whole journal: it is not this line's book side.
			if e.TenantID != tenantID || e.LegalEntityID != line.LegalEntityID || e.CurrencyCode != line.CurrencyCode {
				a.wrongScope = true
			}
			if e.TransactionDate != a.date {
				a.mixedDates = true
			}
			a.net.Add(a.net, e.Debit)
			a.net.Sub(a.net, e.Credit)
		}

		var within, skew []candidateJournal
		for journalID, a := range byJournal {
			if !a.hasEntries || a.wrongScope || a.mixedDates || a.net.Cmp(amounts[i]) != 0 {
				continue
			}
			d, err := time.Parse(dateLayout, a.date)
			if err != nil {
				return nil, fmt.Errorf("journal %s: transaction date %q: %w", journalID, a.date, err)
			}
			days := int(d.Sub(stmt).Hours() / 24)
			if days < 0 {
				days = -days
			}
			c := candidateJournal{journalID: journalID, date: a.date, days: days, net: a.net}
			switch {
			case days <= search.Tolerance.Days:
				within = append(within, c)
			case days <= search.HorizonDays:
				skew = append(skew, c)
			}
		}
		sortCandidates(within)
		sortCandidates(skew)

		var ls lineSearch
		switch {
		case len(within) == 1:
			ls = lineSearch{outcome: CandidateWithinDateTolerance, proposed: &within[0]}
		case len(within) > 1:
			ls = lineSearch{outcome: CandidateAmbiguous, candidates: within}
		case len(skew) == 1:
			ls = lineSearch{outcome: CandidateDateSkew, proposed: &skew[0]}
		case len(skew) > 1:
			ls = lineSearch{outcome: CandidateAmbiguous, candidates: skew}
		default:
			ls = lineSearch{outcome: CandidateNone}
		}
		if ls.proposed != nil {
			proposedBy[ls.proposed.journalID] = append(proposedBy[ls.proposed.journalID], i)
		}
		out[i] = ls
	}

	// One journal is one book entry: bank-reconciliation-svc can match it to
	// one line. Proposing it for two would invite a human to confirm both, so
	// every line that claimed a shared journal is AMBIGUOUS instead.
	for _, idx := range proposedBy {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			ls := out[i]
			out[i] = lineSearch{outcome: CandidateAmbiguous, candidates: []candidateJournal{*ls.proposed}}
		}
	}
	return out, nil
}

func sortCandidates(cs []candidateJournal) {
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].days != cs[b].days {
			return cs[a].days < cs[b].days
		}
		return cs[a].journalID < cs[b].journalID
	})
}

const dateLayout = "2006-01-02"

// candidateItem is the suggestion proposing journal c as the book side of
// line: AUTO_MATCH within the governed date tolerance, DATE_SKEW (for manual
// review) beyond it. Advisory: the match itself is proposed and confirmed in
// bank-reconciliation-svc, by humans.
func candidateItem(line BankStatementLine, amount *big.Rat, outcome CandidateSearchOutcome, c candidateJournal, runID, tenantID string, toleranceVersion int, search *CandidateSearch, now time.Time) ReconciliationSuggestion {
	item := ReconciliationSuggestion{
		TenantID:                tenantID,
		LegalEntityID:           line.LegalEntityID,
		BankReconciliationRunID: runID,
		StatementLineID:         line.StatementLineID,
		CounterpartJournalID:    c.journalID,
		SourceRecordA:           line.BankReference,
		SourceRecordB:           c.journalID,
		AmountA:                 CanonicalAmount(amount),
		AmountB:                 CanonicalAmount(c.net),
		DiscrepancyAmount:       "0",
		ToleranceVersion:        toleranceVersion,
		CandidateSearchOutcome:  outcome,
		DateToleranceVersion:    search.Tolerance.Version,
		SuggestionStatus:        StatusRecommended,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	scope := fmt.Sprintf("FINALIZED general-ledger-svc journal %s moves cash account %s by %s %s, exactly this line's amount, dated %s",
		c.journalID, line.GLCashAccountCode, CanonicalAmount(c.net), line.CurrencyCode, c.date)
	if outcome == CandidateWithinDateTolerance {
		item.DiscrepancyType = DiscrepancyMissingReference
		item.Recommendation = RecommendationAutoMatch
		item.HeuristicRule = HeuristicCandidateWithinDateTolerance
		item.ConfidenceScore = candidateWithinToleranceConfidence
		item.ResolutionNotes = fmt.Sprintf(
			"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — %d day(s) from the statement date, within the governed date tolerance of %d day(s) (policy version %d), and the only such journal. Proposed only: propose and confirm the match in bank-reconciliation-svc",
			line.StatementLineID, line.StatementDate, runID, scope, c.days, search.Tolerance.Days, search.Tolerance.Version)
		return item
	}
	item.DiscrepancyType = DiscrepancyDateSkew
	item.Recommendation = RecommendationManualReview
	item.HeuristicRule = HeuristicCandidateDateSkew
	item.ConfidenceScore = candidateDateSkewConfidence
	item.ResolutionNotes = fmt.Sprintf(
		"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — %d day(s) from the statement date, beyond the governed date tolerance of %d day(s) (policy version %d) but within the %d-day search horizon, and no journal lies within the tolerance. Review whether the timing difference is acceptable before proposing the match in bank-reconciliation-svc",
		line.StatementLineID, line.StatementDate, runID, scope, c.days, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays)
	return item
}

// candidateNote is appended to a missing-reference suggestion's rationale so
// the search result reads alongside the rule that fired.
func candidateNote(ls lineSearch, line BankStatementLine, search *CandidateSearch) string {
	switch ls.outcome {
	case CandidateAmbiguous:
		ids := make([]string, len(ls.candidates))
		for k, c := range ls.candidates {
			ids[k] = fmt.Sprintf("%s (%s)", c.journalID, c.date)
		}
		return fmt.Sprintf("; candidate search on cash account %s (date tolerance %d day(s), policy version %d, horizon %d day(s)) found more than one qualifying journal, or a journal another line also qualifies for: %s — none is proposed; a human picks the match",
			line.GLCashAccountCode, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays, strings.Join(ids, ", "))
	case CandidateNone:
		return fmt.Sprintf("; candidate search on cash account %s found no FINALIZED journal of this legal entity and currency moving it by exactly this amount within %d day(s) of the statement date (date tolerance policy version %d)",
			line.GLCashAccountCode, search.HorizonDays, search.Tolerance.Version)
	case CandidateNoCashAccount:
		return "; no candidate search: the statement line carries no gl_cash_account_code, so there is no cash account to search (and bank-reconciliation-svc could not verify a journal match for it)"
	}
	return ""
}
