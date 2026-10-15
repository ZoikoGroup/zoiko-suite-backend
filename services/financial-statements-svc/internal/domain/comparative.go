package domain

import (
	"fmt"
	"sort"
)

// statementOrder is the fixed presentation order of the statements this
// version produces; anything else sorts after them by name.
var statementOrder = map[StatementType]int{StatementBalanceSheet: 0, StatementIncomeStatement: 1}

// lineIdentity is what makes two facts "the same reported line" under a
// mapping version: the account code under V1 (a line is one account), the
// exact account_name under V2 (a line is every same-category account sharing
// that name; an unnamed account is its own line, keyed by code).
func lineIdentity(f StatementFact, mappingVersion string) (lineID, error) {
	switch mappingVersion {
	case MappingVersionV1:
		return lineID{code: f.AccountCode}, nil
	case MappingVersionV2:
		if f.AccountName == "" {
			return lineID{code: f.AccountCode}, nil
		}
		return lineID{name: f.AccountName}, nil
	}
	return lineID{}, fmt.Errorf("unknown presentation mapping version %q", mappingVersion)
}

// lineID is exactly one of a name (a named V2 line) or a code (a V1 line,
// or an unnamed V2 line, which never merges with anything).
type lineID struct{ name, code string }

// BuildComparative pairs current and prior facts line by line. Both sides
// must have been built under the same mappingVersion (GetComparatives
// refuses otherwise, SPEC_DEVIATIONS B8), so they share one granularity.
//
// Facts are grouped by (statement type, lineIdentity). Within a group, facts
// of the same category pair directly. If what is left is exactly one fact on
// each side, they pair as a reclassification (PriorCategory set). Anything
// else stays one-sided. Every line present in either period therefore
// appears exactly once; a line moved to a different statement shows on each,
// with one side nil. It fails closed on a duplicate line within one period
// or an unparseable amount rather than guessing which value is right.
func BuildComparative(current, prior []StatementFact, mappingVersion string) ([]ComparativeStatement, error) {
	type group struct {
		stmt StatementType
		id   lineID
	}
	index := func(facts []StatementFact, period string) (map[group]map[AccountCategory]StatementFact, error) {
		out := map[group]map[AccountCategory]StatementFact{}
		for _, f := range facts {
			id, err := lineIdentity(f, mappingVersion)
			if err != nil {
				return nil, err
			}
			if _, err := ParseAmount(f.Amount); err != nil {
				return nil, fmt.Errorf("%s period line %q: %w", period, f.AccountCode, err)
			}
			g := group{f.StatementType, id}
			if out[g] == nil {
				out[g] = map[AccountCategory]StatementFact{}
			}
			if _, dup := out[g][f.Category]; dup {
				return nil, fmt.Errorf("%s period has duplicate line %q (%s) on %s", period, f.AccountName, f.Category, f.StatementType)
			}
			out[g][f.Category] = f
		}
		return out, nil
	}
	cur, err := index(current, "current")
	if err != nil {
		return nil, err
	}
	pri, err := index(prior, "prior")
	if err != nil {
		return nil, err
	}

	groups := map[group]bool{}
	for g := range cur {
		groups[g] = true
	}
	for g := range pri {
		groups[g] = true
	}

	byStmt := map[StatementType][]ComparativeFact{}
	for g := range groups {
		c, p := cur[g], pri[g]
		var curLeft, priLeft []StatementFact
		for cat, cf := range c {
			if pf, ok := p[cat]; ok {
				byStmt[g.stmt] = append(byStmt[g.stmt], pairLine(&cf, &pf))
			} else {
				curLeft = append(curLeft, cf)
			}
		}
		for cat, pf := range p {
			if _, ok := c[cat]; !ok {
				priLeft = append(priLeft, pf)
			}
		}
		if len(curLeft) == 1 && len(priLeft) == 1 {
			byStmt[g.stmt] = append(byStmt[g.stmt], pairLine(&curLeft[0], &priLeft[0]))
			continue
		}
		for i := range curLeft {
			byStmt[g.stmt] = append(byStmt[g.stmt], pairLine(&curLeft[i], nil))
		}
		for i := range priLeft {
			byStmt[g.stmt] = append(byStmt[g.stmt], pairLine(nil, &priLeft[i]))
		}
	}

	out := make([]ComparativeStatement, 0, len(byStmt))
	for stmt, ls := range byStmt {
		sort.Slice(ls, func(i, j int) bool {
			if ls[i].AccountCode != ls[j].AccountCode {
				return ls[i].AccountCode < ls[j].AccountCode
			}
			if ls[i].AccountName != ls[j].AccountName {
				return ls[i].AccountName < ls[j].AccountName
			}
			return ls[i].Category < ls[j].Category
		})
		out = append(out, ComparativeStatement{StatementType: stmt, Lines: ls})
	}
	sort.Slice(out, func(i, j int) bool {
		oi, iok := statementOrder[out[i].StatementType]
		oj, jok := statementOrder[out[j].StatementType]
		if iok != jok {
			return iok
		}
		if oi != oj {
			return oi < oj
		}
		return out[i].StatementType < out[j].StatementType
	})
	return out, nil
}

// pairLine builds one comparative line from a current fact, a prior fact,
// or both. Amounts were already validated by BuildComparative's index.
func pairLine(cur, pri *StatementFact) ComparativeFact {
	var l ComparativeFact
	if cur != nil {
		id, amt := cur.FactID, cur.Amount
		l.AccountCode, l.AccountName, l.Category = cur.AccountCode, cur.AccountName, cur.Category
		l.CurrentFactID, l.CurrentAmount = &id, &amt
		l.CurrentSourceAccountCodes = recordedSourceCodes(*cur)
	}
	if pri != nil {
		id, amt := pri.FactID, pri.Amount
		if cur == nil {
			l.AccountCode, l.AccountName, l.Category = pri.AccountCode, pri.AccountName, pri.Category
		} else if pri.Category != cur.Category {
			cat := pri.Category
			l.PriorCategory = &cat
		}
		l.PriorFactID, l.PriorAmount = &id, &amt
		l.PriorSourceAccountCodes = recordedSourceCodes(*pri)
	}
	if cur != nil && pri != nil {
		c, _ := ParseAmount(cur.Amount)
		p, _ := ParseAmount(pri.Amount)
		change := CanonicalAmount(c.Sub(c, p))
		l.Change = &change
	}
	return l
}

// recordedSourceCodes lists the account codes summed into an aggregated
// fact. Nil for a fact with no recorded sources (V1, or pre-aggregation
// rows), where the line's own account_code already says it all.
func recordedSourceCodes(f StatementFact) []string {
	if len(f.SourceAccounts) == 0 {
		return nil
	}
	codes := make([]string, len(f.SourceAccounts))
	for i, a := range f.SourceAccounts {
		codes[i] = a.AccountCode
	}
	return codes
}
