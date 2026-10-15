package domain

import (
	"fmt"
	"sort"
)

// statementOrder is the fixed presentation order of the statements this
// version produces; anything else sorts after them by name.
var statementOrder = map[StatementType]int{StatementBalanceSheet: 0, StatementIncomeStatement: 1}

// BuildComparative pairs current and prior facts by (statement type, account
// code). Every account present in either period yields exactly one line. An
// account reported on different statements in the two periods (a category
// change across statements) yields one line on each, each with one side nil.
// It fails closed on a duplicate account code within one period or an
// unparseable amount rather than guessing which value is right.
func BuildComparative(current, prior []StatementFact) ([]ComparativeStatement, error) {
	type key struct {
		stmt StatementType
		code string
	}
	lines := map[key]*ComparativeFact{}

	index := func(facts []StatementFact, period string) (map[key]StatementFact, error) {
		out := make(map[key]StatementFact, len(facts))
		for _, f := range facts {
			k := key{f.StatementType, f.AccountCode}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%s period has duplicate account %q on %s", period, f.AccountCode, f.StatementType)
			}
			if _, err := ParseAmount(f.Amount); err != nil {
				return nil, fmt.Errorf("%s period account %q: %w", period, f.AccountCode, err)
			}
			out[k] = f
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

	for k, f := range cur {
		id, amt := f.FactID, f.Amount
		lines[k] = &ComparativeFact{
			AccountCode: f.AccountCode, AccountName: f.AccountName, Category: f.Category,
			CurrentFactID: &id, CurrentAmount: &amt,
		}
	}
	for k, f := range pri {
		id, amt := f.FactID, f.Amount
		line, ok := lines[k]
		if !ok {
			lines[k] = &ComparativeFact{
				AccountCode: f.AccountCode, AccountName: f.AccountName, Category: f.Category,
				PriorFactID: &id, PriorAmount: &amt,
			}
			continue
		}
		line.PriorFactID, line.PriorAmount = &id, &amt
		if f.Category != line.Category {
			c := f.Category
			line.PriorCategory = &c
		}
		// Both amounts were parsed successfully in index above.
		c, _ := ParseAmount(*line.CurrentAmount)
		p, _ := ParseAmount(amt)
		change := CanonicalAmount(c.Sub(c, p))
		line.Change = &change
	}

	byStmt := map[StatementType][]ComparativeFact{}
	for k, l := range lines {
		byStmt[k.stmt] = append(byStmt[k.stmt], *l)
	}
	out := make([]ComparativeStatement, 0, len(byStmt))
	for stmt, ls := range byStmt {
		sort.Slice(ls, func(i, j int) bool { return ls[i].AccountCode < ls[j].AccountCode })
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
