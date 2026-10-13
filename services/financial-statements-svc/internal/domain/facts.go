package domain

import "fmt"

// BuildFacts turns validated snapshot content into one StatementFact per
// source account (see the package doc comment on aggregation scope). The
// caller fills in TenantID, StatementSetID, FactID and CreatedAt.
func BuildFacts(content SnapshotContent) ([]StatementFact, error) {
	if err := content.Validate(); err != nil {
		return nil, err
	}
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
