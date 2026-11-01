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

	// The same three outcomes when the counterpart is a banking-connector-svc
	// canonical transaction rather than a journal (A2b). The outcome says
	// what the search found; the rule says which service and kind of record
	// it found it in.
	HeuristicCandidateTransactionWithinDateTolerance HeuristicRule = "CANDIDATE_TRANSACTION_WITHIN_DATE_TOLERANCE"
	HeuristicCandidateTransactionDateSkew            HeuristicRule = "CANDIDATE_TRANSACTION_DATE_SKEW"
	HeuristicCandidateTransactionAmbiguous           HeuristicRule = "CANDIDATE_TRANSACTION_AMBIGUOUS"

	// The same three outcomes again when the counterpart is two
	// general-ledger-svc journals together, neither of which alone equals
	// the line (A2b part 2: "one payment booked as two journals"). Tried
	// only when the single-journal search above finds nothing at all — a
	// book-side counterpart, single or paired, is always preferred over a
	// bank-side canonical transaction search.
	HeuristicCandidateMultiJournalWithinDateTolerance HeuristicRule = "CANDIDATE_MULTI_JOURNAL_WITHIN_DATE_TOLERANCE"
	HeuristicCandidateMultiJournalDateSkew            HeuristicRule = "CANDIDATE_MULTI_JOURNAL_DATE_SKEW"
	HeuristicCandidateMultiJournalAmbiguous           HeuristicRule = "CANDIDATE_MULTI_JOURNAL_AMBIGUOUS"
)

// Fixed labels, like the other rules' (types.go): a named rule fired, not a
// statistical estimate. A single same-amount journal on the line's own cash
// account inside the governed window is the strongest evidence this service
// produces; the same journal outside it is weaker.
const (
	candidateWithinToleranceConfidence = 90.0
	candidateDateSkewConfidence        = 65.0
	candidateAmbiguousConfidence       = 50.0

	// A two-journal combination is a compound inference — two separate
	// records both have to be exactly right, not one — so it is scored
	// below its single-journal counterpart even at the same date outcome.
	// A judgment call (B-style interpretation, not spec-mandated), documented
	// here so a reviewer can adjust it directly.
	candidateMultiJournalWithinToleranceConfidence = 80.0
	candidateMultiJournalDateSkewConfidence        = 55.0
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
// DATE_SKEW candidate may lie, the FINALIZED entries read for each cash
// account the run's lines use, and the NORMALIZED canonical transactions read
// for each bank account whose lines no journal matched (A2b).
//
// A key present in TransactionsByBankAccount means that account was read,
// even when its slice is empty. A line that needs the canonical search on an
// account that was not read is refused, never treated as "no candidate".
type CandidateSearch struct {
	Tolerance                 DateTolerance
	HorizonDays               int
	EntriesByAccount          map[string][]LedgerEntry
	TransactionsByBankAccount map[string][]CanonicalTransaction
}

// candidateJournal is one journal's net movement on a line's cash account.
type candidateJournal struct {
	journalID string
	date      string
	days      int
	net       *big.Rat
}

// lineSearch is the per-line result before the cross-line exclusivity pass.
// At most one of the journal and transaction halves is set: the canonical
// search runs only on a line the journal search left NO_CANDIDATE.
type lineSearch struct {
	outcome    CandidateSearchOutcome
	proposed   *candidateJournal
	candidates []candidateJournal // every qualifying journal, for an AMBIGUOUS rationale

	// proposedPair/pairCandidates are A2b part 2's many-to-one form: tried
	// only when no single journal qualifies at all (proposed == nil and
	// candidates is empty). Mutually exclusive with proposed/candidates —
	// a line is matched by exactly one kind of journal candidate.
	proposedPair   *[2]candidateJournal
	pairCandidates [][2]candidateJournal // every qualifying two-journal combination, for an AMBIGUOUS rationale

	proposedTxn   *candidateTransaction
	txnCandidates []candidateTransaction // every qualifying transaction, for an AMBIGUOUS rationale
	// txnSearched: the canonical search ran on this line, so a final
	// NO_CANDIDATE means neither service had a counterpart.
	txnSearched bool
}

// searchCandidates finds, for each line, the journals bank-reconciliation-svc
// would accept as its match (verifyJournalMatches): of this tenant and the
// line's legal entity, posting to the line's gl_cash_account_code, with a net
// movement on it — debits minus credits — exactly equal to the line's signed
// amount. FINALIZED is guaranteed by the read (journal_status=FINALIZED).
// Currency is required to match too, which bank-reconciliation-svc does not
// check for journals: a same-figure journal in another currency is not this
// line's book side.
// journalAccount is one journal's accumulated net movement on a line's cash
// account, before the date/tolerance decision — shared by the single-journal
// and two-journal (A2b part 2) searches below.
type journalAccount struct {
	date       string
	net        *big.Rat
	wrongScope bool
	mixedDates bool
	hasEntries bool
}

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

		byJournal := map[string]*journalAccount{}
		for _, e := range search.EntriesByAccount[line.GLCashAccountCode] {
			if e.AccountCode != line.GLCashAccountCode {
				continue
			}
			a := byJournal[e.JournalID]
			if a == nil {
				a = &journalAccount{date: e.TransactionDate, net: new(big.Rat)}
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
			// No single journal qualifies at all — try A2b part 2's
			// many-to-one search before giving up to NO_CANDIDATE (which
			// would otherwise send this line to the canonical-transaction
			// search).
			var err error
			ls, err = searchJournalPairs(byJournal, amounts[i], stmt, search)
			if err != nil {
				return nil, err
			}
		}
		switch {
		case ls.proposed != nil:
			proposedBy[ls.proposed.journalID] = append(proposedBy[ls.proposed.journalID], i)
		case ls.proposedPair != nil:
			proposedBy[ls.proposedPair[0].journalID] = append(proposedBy[ls.proposedPair[0].journalID], i)
			proposedBy[ls.proposedPair[1].journalID] = append(proposedBy[ls.proposedPair[1].journalID], i)
		}
		out[i] = ls
	}

	// One journal is one book entry: bank-reconciliation-svc can match it to
	// one line. Proposing it for two would invite a human to confirm both, so
	// every line that claimed a shared journal is AMBIGUOUS instead — whether
	// the claim was a single journal or (A2b part 2) one half of a pair.
	for _, idx := range proposedBy {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			ls := out[i]
			switch {
			case ls.proposed != nil:
				out[i] = lineSearch{outcome: CandidateAmbiguous, candidates: []candidateJournal{*ls.proposed}}
			case ls.proposedPair != nil:
				out[i] = lineSearch{outcome: CandidateAmbiguous, pairCandidates: [][2]candidateJournal{*ls.proposedPair}}
			}
		}
	}
	return out, nil
}

// searchJournalPairs is A2b part 2's many-to-one search, tried only once the
// single-journal search above finds nothing at all. It considers every
// distinct pair of journals on this cash account (each individually real,
// correctly scoped, single-dated, and within the search horizon) whose
// combined net movement equals the line's amount exactly — bounded to
// exactly two journals, the specific case the gap names ("one payment
// booked as two journals"), not an open-ended combinatorial search.
func searchJournalPairs(byJournal map[string]*journalAccount, amount *big.Rat, stmt time.Time, search *CandidateSearch) (lineSearch, error) {
	type eligible struct {
		journalID string
		date      string
		days      int
		net       *big.Rat
	}
	var pool []eligible
	for journalID, a := range byJournal {
		if !a.hasEntries || a.wrongScope || a.mixedDates {
			continue
		}
		d, err := time.Parse(dateLayout, a.date)
		if err != nil {
			return lineSearch{}, fmt.Errorf("journal %s: transaction date %q: %w", journalID, a.date, err)
		}
		days := int(d.Sub(stmt).Hours() / 24)
		if days < 0 {
			days = -days
		}
		if days > search.HorizonDays {
			continue // outside the horizon even alone; never a candidate combined either
		}
		pool = append(pool, eligible{journalID: journalID, date: a.date, days: days, net: a.net})
	}
	// Deterministic enumeration order: byJournal is a map, and a decision
	// that depends on iteration order would be nondeterministic.
	sort.Slice(pool, func(a, b int) bool { return pool[a].journalID < pool[b].journalID })

	var within, skew [][2]candidateJournal
	for x := 0; x < len(pool); x++ {
		for y := x + 1; y < len(pool); y++ {
			sum := new(big.Rat).Add(pool[x].net, pool[y].net)
			if sum.Cmp(amount) != 0 {
				continue
			}
			maxDays := pool[x].days
			if pool[y].days > maxDays {
				maxDays = pool[y].days
			}
			pair := [2]candidateJournal{
				{journalID: pool[x].journalID, date: pool[x].date, days: pool[x].days, net: pool[x].net},
				{journalID: pool[y].journalID, date: pool[y].date, days: pool[y].days, net: pool[y].net},
			}
			if maxDays <= search.Tolerance.Days {
				within = append(within, pair)
			} else {
				skew = append(skew, pair)
			}
		}
	}
	switch {
	case len(within) == 1:
		return lineSearch{outcome: CandidateWithinDateTolerance, proposedPair: &within[0]}, nil
	case len(within) > 1:
		return lineSearch{outcome: CandidateAmbiguous, pairCandidates: within}, nil
	case len(skew) == 1:
		return lineSearch{outcome: CandidateDateSkew, proposedPair: &skew[0]}, nil
	case len(skew) > 1:
		return lineSearch{outcome: CandidateAmbiguous, pairCandidates: skew}, nil
	default:
		return lineSearch{outcome: CandidateNone}, nil
	}
}

// journalPhase is an analysis up to and including the journal search: the
// exact amounts, the duplicate copies, and each searched line's result.
type journalPhase struct {
	amounts []*big.Rat
	dupOf   map[int]duplicateOf
	dupsOf  map[int][]int
	found   map[int]lineSearch
}

// analyzeSearch runs the deterministic first half of an analysis — amounts,
// duplicates, the journal search — shared by AnalyzeUnmatchedStatementLines
// and CanonicalSearchLines so both see exactly the same NO_CANDIDATE lines.
func analyzeSearch(lines []BankStatementLine, tenantID string, search *CandidateSearch) (*journalPhase, error) {
	if search == nil || search.Tolerance.Version <= 0 || search.Tolerance.Days < 0 || search.HorizonDays < search.Tolerance.Days {
		return nil, fmt.Errorf("a governed candidate date tolerance and a search horizon at least as wide are required")
	}
	amounts := make([]*big.Rat, len(lines))
	for i, line := range lines {
		amount, err := ParseStorableAmount(line.Amount)
		if err != nil {
			return nil, fmt.Errorf("statement line %s: %w", line.StatementLineID, err)
		}
		amounts[i] = amount
	}

	// Duplicate detection runs first and is exclusive for the duplicate: a
	// line identified as a copy of another gets ONE suggestion,
	// DUPLICATE_ENTRY, and not also a write-off or review recommendation.
	// Writing off or investigating a copy is the wrong action, and two
	// suggestions about the same line could contradict each other. The
	// anchor — the entry that stays — still goes through the rules below:
	// once the copy is removed it is one real entry that is still UNMATCHED.
	// So every line is still the subject of exactly one suggestion.
	dupOf, dupsOf := findDuplicates(lines, amounts)

	// A copy is not searched: the entry it duplicates is, once.
	searched := make(map[int]bool, len(lines))
	for i := range lines {
		if _, isDup := dupOf[i]; !isDup {
			searched[i] = true
		}
	}
	found, err := searchCandidates(lines, amounts, searched, tenantID, search)
	if err != nil {
		return nil, err
	}
	return &journalPhase{amounts: amounts, dupOf: dupOf, dupsOf: dupsOf, found: found}, nil
}

// CanonicalSearchLines returns the lines whose analysis needs
// banking-connector-svc's canonical transactions: exactly those the journal
// search (EntriesByAccount) leaves NO_CANDIDATE. The caller reads each such
// line's bank account into TransactionsByBankAccount before
// AnalyzeUnmatchedStatementLines; when no line needs it, nothing is read.
func CanonicalSearchLines(lines []BankStatementLine, tenantID string, search *CandidateSearch) ([]BankStatementLine, error) {
	a, err := analyzeSearch(lines, tenantID, search)
	if err != nil {
		return nil, err
	}
	var out []BankStatementLine
	for i, line := range lines {
		if ls, searched := a.found[i]; searched && ls.outcome == CandidateNone {
			out = append(out, line)
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

// candidateItemMulti is candidateItem's A2b part 2 form: the book side is two
// journals together, neither of which alone equals the line.
func candidateItemMulti(line BankStatementLine, amount *big.Rat, outcome CandidateSearchOutcome, pair [2]candidateJournal, runID, tenantID string, toleranceVersion int, search *CandidateSearch, now time.Time) ReconciliationSuggestion {
	ids := []string{pair[0].journalID, pair[1].journalID}
	combinedNet := new(big.Rat).Add(pair[0].net, pair[1].net)
	maxDays := pair[0].days
	if pair[1].days > maxDays {
		maxDays = pair[1].days
	}
	item := ReconciliationSuggestion{
		TenantID:                tenantID,
		LegalEntityID:           line.LegalEntityID,
		BankReconciliationRunID: runID,
		StatementLineID:         line.StatementLineID,
		CounterpartJournalIDs:   ids,
		SourceRecordA:           line.BankReference,
		SourceRecordB:           strings.Join(ids, "+"),
		AmountA:                 CanonicalAmount(amount),
		AmountB:                 CanonicalAmount(combinedNet),
		DiscrepancyAmount:       "0",
		ToleranceVersion:        toleranceVersion,
		CandidateSearchOutcome:  outcome,
		DateToleranceVersion:    search.Tolerance.Version,
		SuggestionStatus:        StatusRecommended,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	scope := fmt.Sprintf("FINALIZED general-ledger-svc journals %s and %s together move cash account %s by %s %s, exactly this line's amount (dated %s and %s)",
		pair[0].journalID, pair[1].journalID, line.GLCashAccountCode, CanonicalAmount(combinedNet), line.CurrencyCode, pair[0].date, pair[1].date)
	if outcome == CandidateWithinDateTolerance {
		item.DiscrepancyType = DiscrepancyMissingReference
		item.Recommendation = RecommendationAutoMatch
		item.HeuristicRule = HeuristicCandidateMultiJournalWithinDateTolerance
		item.ConfidenceScore = candidateMultiJournalWithinToleranceConfidence
		item.ResolutionNotes = fmt.Sprintf(
			"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — the later of the two is %d day(s) from the statement date, within the governed date tolerance of %d day(s) (policy version %d), and the only such pair. No single journal matched alone. Proposed only: propose and confirm the match in bank-reconciliation-svc",
			line.StatementLineID, line.StatementDate, runID, scope, maxDays, search.Tolerance.Days, search.Tolerance.Version)
		return item
	}
	item.DiscrepancyType = DiscrepancyDateSkew
	item.Recommendation = RecommendationManualReview
	item.HeuristicRule = HeuristicCandidateMultiJournalDateSkew
	item.ConfidenceScore = candidateMultiJournalDateSkewConfidence
	item.ResolutionNotes = fmt.Sprintf(
		"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — the later of the two is %d day(s) from the statement date, beyond the governed date tolerance of %d day(s) (policy version %d) but within the %d-day search horizon, and no single journal or other two-journal combination lies within the tolerance. Review whether the timing difference is acceptable before proposing the match in bank-reconciliation-svc",
		line.StatementLineID, line.StatementDate, runID, scope, maxDays, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays)
	return item
}

// candidateNote is appended to a missing-reference suggestion's rationale so
// the search result reads alongside the rule that fired.
func candidateNote(ls lineSearch, line BankStatementLine, search *CandidateSearch) string {
	switch {
	case ls.outcome == CandidateAmbiguous && len(ls.pairCandidates) > 0:
		ids := make([]string, len(ls.pairCandidates))
		for k, p := range ls.pairCandidates {
			ids[k] = fmt.Sprintf("%s+%s", p[0].journalID, p[1].journalID)
		}
		return fmt.Sprintf("; candidate search on cash account %s (date tolerance %d day(s), policy version %d, horizon %d day(s)) found more than one qualifying two-journal combination, or one shared with another line: %s — none is proposed; a human picks the match",
			line.GLCashAccountCode, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays, strings.Join(ids, ", "))
	case ls.outcome == CandidateAmbiguous && len(ls.txnCandidates) > 0:
		ids := make([]string, len(ls.txnCandidates))
		for k, c := range ls.txnCandidates {
			ids[k] = fmt.Sprintf("%s (%s)", c.transactionID, c.date)
		}
		return fmt.Sprintf("; candidate search found no FINALIZED general-ledger-svc journal on cash account %s, then on bank account %s found more than one qualifying NORMALIZED banking-connector-svc canonical transaction, or a transaction another line also qualifies for (date tolerance %d day(s), policy version %d, horizon %d day(s)): %s — none is proposed; a human picks the match",
			line.GLCashAccountCode, line.BankAccountID, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays, strings.Join(ids, ", "))
	case ls.outcome == CandidateNone && ls.txnSearched:
		return fmt.Sprintf("; candidate search found no FINALIZED general-ledger-svc journal of this legal entity and currency moving cash account %s by exactly this amount, and no NORMALIZED banking-connector-svc canonical transaction on bank account %s of exactly this signed amount and currency, within %d day(s) of the statement date (date tolerance policy version %d)",
			line.GLCashAccountCode, line.BankAccountID, search.HorizonDays, search.Tolerance.Version)
	}
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
