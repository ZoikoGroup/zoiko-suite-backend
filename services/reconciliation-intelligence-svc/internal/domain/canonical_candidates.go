package domain

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// CanonicalTransaction is one NORMALIZED banking-connector-svc canonical bank
// transaction, as read for a candidate search (A2b). Amount is exact and
// signed the way banking-connector-svc stores it: the raw statement line's
// posted amount, positive for money into the account.
type CanonicalTransaction struct {
	TransactionID   string
	TenantID        string
	LegalEntityID   string
	BankAccountID   string
	Currency        string
	Status          string
	TransactionDate string // YYYY-MM-DD, UTC
	Amount          *big.Rat
}

const canonicalStatusNormalized = "NORMALIZED"

// candidateTransaction is one canonical transaction qualifying for a line.
type candidateTransaction struct {
	transactionID string
	date          string
	days          int
	amount        *big.Rat
}

// searchCanonical runs the second half of the counterpart search on every
// line the journal search (searchCandidates, including its exclusivity pass)
// left NO_CANDIDATE — and on no other line: a line with a journal candidate,
// an ambiguous one, or no cash account keeps that result.
//
// It finds the canonical transactions bank-reconciliation-svc would accept as
// the line's match (verifyCanonicalMatch): NORMALIZED, of this tenant, in the
// line's currency, with an amount equal to the line's. That check compares
// signed amounts directly (in cents); both services sign an amount the same
// way — positive is money in — so the comparison here is a direct signed
// equality too, exact as big.Rat rather than in cents. It is stricter in two
// ways bank-reconciliation-svc is not, as for journals: the transaction must
// be of the line's legal entity, and on the line's own bank account.
func searchCanonical(lines []BankStatementLine, amounts []*big.Rat, found map[int]lineSearch, tenantID string, search *CandidateSearch) error {
	proposedBy := map[string][]int{}

	for i, line := range lines {
		ls, searched := found[i]
		if !searched || ls.outcome != CandidateNone {
			continue
		}
		if line.BankAccountID == "" {
			return fmt.Errorf("statement line %s has no bank account to search for a canonical transaction", line.StatementLineID)
		}
		txns, read := search.TransactionsByBankAccount[line.BankAccountID]
		if !read {
			// "Could not look" must never become "no candidate".
			return fmt.Errorf("statement line %s needs a canonical transaction search, but bank account %s was not read", line.StatementLineID, line.BankAccountID)
		}
		stmt, err := time.Parse(dateLayout, line.StatementDate)
		if err != nil {
			return fmt.Errorf("statement line %s: statement date %q: %w", line.StatementLineID, line.StatementDate, err)
		}

		var within, skew []candidateTransaction
		for _, t := range txns {
			if t.TenantID != tenantID || t.LegalEntityID != line.LegalEntityID || t.BankAccountID != line.BankAccountID ||
				t.Status != canonicalStatusNormalized || t.Currency != line.CurrencyCode ||
				t.Amount == nil || t.Amount.Cmp(amounts[i]) != 0 {
				continue
			}
			d, err := time.Parse(dateLayout, t.TransactionDate)
			if err != nil {
				return fmt.Errorf("canonical transaction %s: transaction date %q: %w", t.TransactionID, t.TransactionDate, err)
			}
			days := int(d.Sub(stmt).Hours() / 24)
			if days < 0 {
				days = -days
			}
			c := candidateTransaction{transactionID: t.TransactionID, date: t.TransactionDate, days: days, amount: t.Amount}
			switch {
			case days <= search.Tolerance.Days:
				within = append(within, c)
			case days <= search.HorizonDays:
				skew = append(skew, c)
			}
		}
		sortTransactions(within)
		sortTransactions(skew)

		next := lineSearch{outcome: CandidateNone, txnSearched: true}
		switch {
		case len(within) == 1:
			next = lineSearch{outcome: CandidateWithinDateTolerance, proposedTxn: &within[0], txnSearched: true}
		case len(within) > 1:
			next = lineSearch{outcome: CandidateAmbiguous, txnCandidates: within, txnSearched: true}
		case len(skew) == 1:
			next = lineSearch{outcome: CandidateDateSkew, proposedTxn: &skew[0], txnSearched: true}
		case len(skew) > 1:
			next = lineSearch{outcome: CandidateAmbiguous, txnCandidates: skew, txnSearched: true}
		}
		if next.proposedTxn != nil {
			proposedBy[next.proposedTxn.transactionID] = append(proposedBy[next.proposedTxn.transactionID], i)
		}
		found[i] = next
	}

	// One canonical transaction is one bank fact: bank-reconciliation-svc can
	// match it to one line. Every line that claimed a shared transaction is
	// AMBIGUOUS instead. This pass is over transactions only — a journal and
	// a transaction are different records even if their ids were equal, and
	// a line holding a journal proposal never reaches this search.
	for _, idx := range proposedBy {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			ls := found[i]
			found[i] = lineSearch{outcome: CandidateAmbiguous, txnCandidates: []candidateTransaction{*ls.proposedTxn}, txnSearched: true}
		}
	}
	return nil
}

func sortTransactions(cs []candidateTransaction) {
	sort.Slice(cs, func(a, b int) bool {
		if cs[a].days != cs[b].days {
			return cs[a].days < cs[b].days
		}
		return cs[a].transactionID < cs[b].transactionID
	})
}

// canonicalItem is the suggestion proposing canonical transaction c as the
// counterpart of line: AUTO_MATCH within the governed date tolerance,
// DATE_SKEW (for manual review) beyond it. Advisory: the match itself is
// proposed and confirmed in bank-reconciliation-svc, by humans.
func canonicalItem(line BankStatementLine, amount *big.Rat, outcome CandidateSearchOutcome, c candidateTransaction, runID, tenantID string, toleranceVersion int, search *CandidateSearch, now time.Time) ReconciliationSuggestion {
	item := ReconciliationSuggestion{
		TenantID:                 tenantID,
		LegalEntityID:            line.LegalEntityID,
		BankReconciliationRunID:  runID,
		StatementLineID:          line.StatementLineID,
		CounterpartTransactionID: c.transactionID,
		SourceRecordA:            line.BankReference,
		SourceRecordB:            c.transactionID,
		AmountA:                  CanonicalAmount(amount),
		AmountB:                  CanonicalAmount(c.amount),
		DiscrepancyAmount:        "0",
		ToleranceVersion:         toleranceVersion,
		CandidateSearchOutcome:   outcome,
		DateToleranceVersion:     search.Tolerance.Version,
		SuggestionStatus:         StatusRecommended,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	scope := fmt.Sprintf("no FINALIZED general-ledger-svc journal on cash account %s matches it; NORMALIZED banking-connector-svc canonical transaction %s on bank account %s is %s %s, exactly this line's signed amount, dated %s",
		line.GLCashAccountCode, c.transactionID, line.BankAccountID, CanonicalAmount(c.amount), line.CurrencyCode, c.date)
	if outcome == CandidateWithinDateTolerance {
		item.DiscrepancyType = DiscrepancyMissingReference
		item.Recommendation = RecommendationAutoMatch
		item.HeuristicRule = HeuristicCandidateTransactionWithinDateTolerance
		item.ConfidenceScore = candidateWithinToleranceConfidence
		item.ResolutionNotes = fmt.Sprintf(
			"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — %d day(s) from the statement date, within the governed date tolerance of %d day(s) (policy version %d), and the only such transaction. Proposed only: propose and confirm the match in bank-reconciliation-svc",
			line.StatementLineID, line.StatementDate, runID, scope, c.days, search.Tolerance.Days, search.Tolerance.Version)
		return item
	}
	item.DiscrepancyType = DiscrepancyDateSkew
	item.Recommendation = RecommendationManualReview
	item.HeuristicRule = HeuristicCandidateTransactionDateSkew
	item.ConfidenceScore = candidateDateSkewConfidence
	item.ResolutionNotes = fmt.Sprintf(
		"heuristic rule: statement line %s (dated %s) is UNMATCHED in bank-reconciliation run %s; %s — %d day(s) from the statement date, beyond the governed date tolerance of %d day(s) (policy version %d) but within the %d-day search horizon, and no transaction lies within the tolerance. Review whether the timing difference is acceptable before proposing the match in bank-reconciliation-svc",
		line.StatementLineID, line.StatementDate, runID, scope, c.days, search.Tolerance.Days, search.Tolerance.Version, search.HorizonDays)
	return item
}
