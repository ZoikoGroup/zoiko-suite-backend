package domain

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IntercompanyPair is the part of intercompany-accounting-svc's pair a run
// reads. Amount/CurrencyCode are the source entity's side and
// CounterpartyAmount/CounterpartyCurrencyCode the target entity's, as that
// service's MatchIntercompany compares them. FiscalPeriod is nil for a pair
// created before its migration 000005.
type IntercompanyPair struct {
	IntercompanyEntryID      string  `json:"intercompany_entry_id"`
	SourceLegalEntityID      string  `json:"source_legal_entity_id"`
	TargetLegalEntityID      string  `json:"target_legal_entity_id"`
	SourceJournalID          string  `json:"source_journal_id"`
	TargetJournalID          *string `json:"target_journal_id,omitempty"`
	Amount                   Decimal `json:"amount"`
	CurrencyCode             string  `json:"currency_code,omitempty"`
	CounterpartyAmount       Decimal `json:"counterparty_amount,omitempty"`
	CounterpartyCurrencyCode string  `json:"counterparty_currency_code,omitempty"`
	FiscalPeriod             *string `json:"fiscal_period,omitempty"`
	MatchStatus              string  `json:"match_status"`
}

// PostedJournal is one general-ledger-svc journal as a run sees it: its
// header (entity, period, status) and its posted lines. Line amounts are
// still float64, as general-ledger-svc serves them (B6).
type PostedJournal struct {
	JournalID     string       `json:"journal_id"`
	LegalEntityID string       `json:"legal_entity_id"`
	FiscalPeriod  string       `json:"fiscal_period"`
	Status        string       `json:"status"`
	Lines         []PostedLine `json:"lines"`
}

type PostedLine struct {
	LineNumber   int     `json:"line_number"`
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// Elimination outcomes. A BLOCKED pair was in the run's scope but nothing of
// it was eliminated; BlockedReason says why.
const (
	EliminationOutcomeEliminated = "ELIMINATED"
	EliminationOutcomeBlocked    = "BLOCKED"
)

// How a run decided a pair belongs to its period: the pair's own
// fiscal_period, or (for a pair that records none) its source journal's,
// which intercompany-accounting-svc's migration 000005 names as the pair's
// true period. UNDETERMINED: the pair records no period and its source
// journal could not be read.
const (
	PeriodBasisPair          = "PAIR"
	PeriodBasisSourceJournal = "SOURCE_JOURNAL"
	PeriodBasisUndetermined  = "UNDETERMINED"
)

const (
	EliminationSideSource = "SOURCE"
	EliminationSideTarget = "TARGET"
)

// EliminatedLine is one journal line a run subtracted from group balances.
// Amounts are the shortest decimal form of the float64 general-ledger-svc
// served, so the record states exactly what was subtracted.
type EliminatedLine struct {
	Side         string  `json:"side"`
	JournalID    string  `json:"journal_id"`
	LineNumber   int     `json:"line_number"`
	AccountCode  string  `json:"account_code"`
	DebitAmount  Decimal `json:"debit_amount"`
	CreditAmount Decimal `json:"credit_amount"`

	debit, credit float64
}

// Net is the line's debit minus credit, the amount FetchTrialBalance added
// to the account and elimination subtracts.
func (l EliminatedLine) Net() float64 { return l.debit - l.credit }

// Elimination is one run's record of one in-scope intercompany pair
// (consolidation_eliminations, migration 000007): what the pair was, which
// period basis put it in scope, the intercompany accounts the run declared,
// and either the lines eliminated or why nothing was.
type Elimination struct {
	EliminationID       string `json:"elimination_id"`
	TenantID            string `json:"tenant_id"`
	ConsolidationRunID  string `json:"consolidation_run_id"`
	IntercompanyEntryID string `json:"intercompany_entry_id"`
	SourceLegalEntityID string `json:"source_legal_entity_id"`
	TargetLegalEntityID string `json:"target_legal_entity_id"`
	FiscalPeriod        string `json:"fiscal_period"`
	PeriodBasis         string `json:"period_basis"`
	MatchStatus         string `json:"match_status"`

	SourceJournalID          string  `json:"source_journal_id"`
	TargetJournalID          *string `json:"target_journal_id,omitempty"`
	Amount                   Decimal `json:"amount"`
	CurrencyCode             string  `json:"currency_code"`
	CounterpartyAmount       Decimal `json:"counterparty_amount,omitempty"`
	CounterpartyCurrencyCode string  `json:"counterparty_currency_code"`

	IntercompanyAccountCodes []string         `json:"intercompany_account_codes"`
	Outcome                  string           `json:"outcome"`
	BlockedReason            *string          `json:"blocked_reason,omitempty"`
	Lines                    []EliminatedLine `json:"lines"`
	CreatedAt                time.Time        `json:"created_at"`
}

// PairScope is where a pair stands against a run's scope.
type PairScope int

const (
	PairOutOfScope PairScope = iota
	PairInScope
	// PairNeedsSourcePeriod: both entities are in the run, but the pair
	// records no period; its source journal's period decides.
	PairNeedsSourcePeriod
)

// ScopePair applies a run's scope to a pair: both entities must be among the
// run's child entities (the only entities whose balances the run summed),
// and the pair's period must be the run's.
func ScopePair(p IntercompanyPair, entities map[string]bool, runPeriod string) PairScope {
	if !entities[p.SourceLegalEntityID] || !entities[p.TargetLegalEntityID] {
		return PairOutOfScope
	}
	if p.FiscalPeriod == nil || *p.FiscalPeriod == "" {
		return PairNeedsSourcePeriod
	}
	if *p.FiscalPeriod != runPeriod {
		return PairOutOfScope
	}
	return PairInScope
}

// EliminationAmountTolerance is intercompany-accounting-svc's
// MatchIntercompany tolerance (its matchTolerance, 0.001). A pair matched
// within it is eliminated when its intercompany lines agree within it too.
var EliminationAmountTolerance = big.NewRat(1, 1000)

// NormalizeAccountCodes trims, de-duplicates and sorts a run's declared
// intercompany accounts. An empty entry is an error.
func NormalizeAccountCodes(codes []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("intercompany_account_codes must not contain an empty account code")
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PlanElimination decides, for one in-scope pair, which lines a run
// subtracts. It returns the lines, or a reason the pair is blocked; a
// blocked pair contributes no line at all, so a pair is never half
// eliminated. Each leg must be a FINALIZED journal of its own entity in the
// run's period (only those were summed into the run's balances), and its
// lines on the declared intercompany accounts must total the pair's amount
// for that side (source: amount; target: counterparty amount), within
// EliminationAmountTolerance. Only those lines are eliminated; any other
// line of the same journal stays in the group balance.
func PlanElimination(p IntercompanyPair, source, target *PostedJournal, accounts []string, runPeriod string) ([]EliminatedLine, string) {
	if len(accounts) == 0 {
		return nil, "the run declared no intercompany_account_codes, so no line of the pair's journals can be identified as intercompany"
	}
	if p.TargetJournalID == nil || *p.TargetJournalID == "" {
		return nil, fmt.Sprintf("%s pair has no target_journal_id", strings.ToLower(p.MatchStatus))
	}
	declared := map[string]bool{}
	for _, a := range accounts {
		declared[a] = true
	}

	sourceLines, reason := planLeg(EliminationSideSource, source, p.SourceLegalEntityID, p.Amount, declared, runPeriod)
	if reason != "" {
		return nil, reason
	}
	counterparty := p.CounterpartyAmount
	if counterparty == "" {
		return nil, "the pair carries no counterparty_amount to check the target journal's intercompany lines against"
	}
	targetLines, reason := planLeg(EliminationSideTarget, target, p.TargetLegalEntityID, counterparty, declared, runPeriod)
	if reason != "" {
		return nil, reason
	}
	return append(sourceLines, targetLines...), ""
}

func planLeg(side string, j *PostedJournal, entityID string, expected Decimal, declared map[string]bool, runPeriod string) ([]EliminatedLine, string) {
	label := strings.ToLower(side)
	switch {
	case j == nil:
		return nil, fmt.Sprintf("%s journal was not read", label)
	case j.LegalEntityID != entityID:
		return nil, fmt.Sprintf("%s journal %s belongs to entity %s, not the pair's %s entity %s", label, j.JournalID, j.LegalEntityID, label, entityID)
	case j.Status != "FINALIZED":
		return nil, fmt.Sprintf("%s journal %s is %s; only FINALIZED journals are in the run's balances", label, j.JournalID, j.Status)
	case j.FiscalPeriod != runPeriod:
		return nil, fmt.Sprintf("%s journal %s is in period %s, not the run's %s, so its lines are not in the run's balances", label, j.JournalID, j.FiscalPeriod, runPeriod)
	}

	var lines []EliminatedLine
	debits, credits := new(big.Rat), new(big.Rat)
	for _, l := range j.Lines {
		if !declared[l.AccountCode] {
			continue
		}
		d, c := floatRat(l.DebitAmount), floatRat(l.CreditAmount)
		debits.Add(debits, d)
		credits.Add(credits, c)
		lines = append(lines, EliminatedLine{
			Side: side, JournalID: j.JournalID, LineNumber: l.LineNumber, AccountCode: l.AccountCode,
			DebitAmount: Decimal(CanonicalAmount(d)), CreditAmount: Decimal(CanonicalAmount(c)),
			debit: l.DebitAmount, credit: l.CreditAmount,
		})
	}
	if len(lines) == 0 {
		return nil, fmt.Sprintf("%s journal %s has no line on a declared intercompany account", label, j.JournalID)
	}

	want, err := expected.Rat()
	if err != nil {
		return nil, fmt.Sprintf("the pair's %s amount %q is not a decimal", label, string(expected))
	}
	total := debits
	if credits.Cmp(debits) > 0 {
		total = credits
	}
	diff := new(big.Rat).Sub(total, want)
	if diff.Abs(diff).Cmp(EliminationAmountTolerance) > 0 {
		return nil, fmt.Sprintf("%s journal %s's intercompany lines total %s, not the pair's %s amount %s",
			label, j.JournalID, CanonicalAmount(total), label, CanonicalAmount(want))
	}
	return lines, ""
}

// floatRat is the exact decimal a float64 line amount was most likely sent
// as: its shortest round-tripping decimal form.
func floatRat(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	return r
}

// RunEliminationExplanation is ExplainElimination's answer for one run: the
// elimination rows that run recorded for the entity pair (either
// direction), read back as written, never re-derived.
type RunEliminationExplanation struct {
	ConsolidationRunID string `json:"consolidation_run_id"`
	GroupLegalEntityID string `json:"group_legal_entity_id"`
	EntityA            string `json:"entity_a"`
	EntityB            string `json:"entity_b"`
	FiscalPeriod       string `json:"fiscal_period"`
	Rule               string `json:"rule"`

	Eliminated   bool          `json:"eliminated"`
	Reason       string        `json:"reason"`
	Eliminations []Elimination `json:"eliminations"`
}

// BuildRunEliminationExplanation answers from the run's recorded rows.
func BuildRunEliminationExplanation(run *ConsolidationRun, entityA, entityB string, rows []Elimination) *RunEliminationExplanation {
	out := &RunEliminationExplanation{
		ConsolidationRunID: run.ConsolidationRunID,
		GroupLegalEntityID: run.GroupLegalEntityID,
		EntityA:            entityA,
		EntityB:            entityB,
		FiscalPeriod:       run.FiscalPeriod,
		Rule:               EliminationRuleMatchedOrClosed,
		Eliminations:       []Elimination{},
	}
	var eliminated, blocked int
	for _, e := range rows {
		if (e.SourceLegalEntityID == entityA && e.TargetLegalEntityID == entityB) ||
			(e.SourceLegalEntityID == entityB && e.TargetLegalEntityID == entityA) {
			out.Eliminations = append(out.Eliminations, e)
			if e.Outcome == EliminationOutcomeEliminated {
				eliminated++
			} else {
				blocked++
			}
		}
	}
	out.Eliminated = eliminated > 0 && blocked == 0
	switch {
	case len(out.Eliminations) == 0:
		out.Reason = "the run recorded no elimination for this pair: no MATCHED or CLOSED intercompany pair between these entities was in the run's scope"
	case blocked == 0:
		out.Reason = fmt.Sprintf("the run eliminated %d intercompany pair(s) between these entities", eliminated)
	default:
		out.Reason = fmt.Sprintf("the run eliminated %d and blocked %d intercompany pair(s) between these entities; each blocked row says why", eliminated, blocked)
	}
	return out
}
