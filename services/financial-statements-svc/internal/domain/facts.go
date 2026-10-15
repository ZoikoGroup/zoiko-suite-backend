package domain

import (
	"fmt"
	"sort"
)

// BuildFacts turns validated snapshot content into StatementFacts under the
// given presentation mapping version. The caller fills in TenantID,
// StatementSetID, FactID and CreatedAt.
//
//   - MappingVersionV1: one fact per source account.
//   - MappingVersionV2: accounts sharing (statement, category, exact
//     non-empty account_name) become one fact whose amount is their exact
//     big.Rat sum. Category is part of the key on purpose: merging an ASSET
//     and a LIABILITY that happen to share a name would move a balance
//     between tie-out buckets. Unnamed accounts never merge — a missing name
//     is not evidence that two accounts are the same line.
//
// An unknown version is refused rather than defaulted: a set must only ever
// be built with the mapping it records.
func BuildFacts(content SnapshotContent, mappingVersion string) ([]StatementFact, error) {
	if err := content.Validate(); err != nil {
		return nil, err
	}
	switch mappingVersion {
	case MappingVersionV1:
		return buildFactsPerAccount(content)
	case MappingVersionV2:
		return buildFactsAggregated(content)
	}
	return nil, Errf(CodeValidationFailed, "unknown presentation mapping version %q", mappingVersion)
}

func buildFactsPerAccount(content SnapshotContent) ([]StatementFact, error) {
	facts := make([]StatementFact, 0, len(content.Accounts))
	for _, a := range content.Accounts {
		stmt, ok := StatementFor(a.Category)
		if !ok {
			return nil, fmt.Errorf("no statement mapping for category %q", a.Category)
		}
		amt, err := ParseAmount(a.Balance)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", a.AccountCode, err)
		}
		facts = append(facts, StatementFact{
			StatementType: stmt,
			Category:      a.Category,
			AccountCode:   a.AccountCode,
			AccountName:   a.AccountName,
			Amount:        CanonicalAmount(amt),
		})
	}
	return facts, nil
}

func buildFactsAggregated(content SnapshotContent) ([]StatementFact, error) {
	type key struct {
		stmt StatementType
		cat  AccountCategory
		// Exactly one of name / code is set: an unnamed account is keyed by
		// its (unique) code so it always stays its own line.
		name, code string
	}
	var order []key
	groups := map[key][]SnapshotContentAccount{}
	for _, a := range content.Accounts {
		stmt, ok := StatementFor(a.Category)
		if !ok {
			return nil, fmt.Errorf("no statement mapping for category %q", a.Category)
		}
		k := key{stmt: stmt, cat: a.Category, name: a.AccountName}
		if a.AccountName == "" {
			k.code = a.AccountCode
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], a)
	}

	facts := make([]StatementFact, 0, len(order))
	for _, k := range order {
		members := groups[k]
		sort.Slice(members, func(i, j int) bool { return members[i].AccountCode < members[j].AccountCode })
		balances := make([]string, len(members))
		for i, m := range members {
			balances[i] = m.Balance
		}
		total, err := SumAmounts(balances)
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", members[0].AccountName, err)
		}
		facts = append(facts, StatementFact{
			StatementType: k.stmt,
			Category:      k.cat,
			// The lowest member code represents the line: unique per set
			// (codes are unique), stable, and a real account code.
			AccountCode:    members[0].AccountCode,
			AccountName:    members[0].AccountName,
			Amount:         CanonicalAmount(total),
			SourceAccounts: members,
		})
	}
	return facts, nil
}
