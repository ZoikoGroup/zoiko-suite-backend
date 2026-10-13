package domain

import "fmt"

// ValidateTieOut is REP-01's one mandatory engineering control this version
// implements in full: the balance sheet must tie out — Assets must equal
// Liabilities plus Equity, computed from the statement facts actually
// produced, not re-read from the source snapshot. An empty category (no
// accounts at all in that category) is a legitimate zero, not an error.
//
// Returns the validation errors found (empty slice means it ties out).
func ValidateTieOut(facts []StatementFact) ([]string, error) {
	var assets, liabilities, equity []string
	for _, f := range facts {
		switch f.Category {
		case CategoryAsset:
			assets = append(assets, f.Amount)
		case CategoryLiability:
			liabilities = append(liabilities, f.Amount)
		case CategoryEquity:
			equity = append(equity, f.Amount)
		}
	}
	assetsTotal, err := SumAmounts(assets)
	if err != nil {
		return nil, fmt.Errorf("sum assets: %w", err)
	}
	liabilitiesTotal, err := SumAmounts(liabilities)
	if err != nil {
		return nil, fmt.Errorf("sum liabilities: %w", err)
	}
	equityTotal, err := SumAmounts(equity)
	if err != nil {
		return nil, fmt.Errorf("sum equity: %w", err)
	}

	rightSide := liabilitiesTotal.Add(liabilitiesTotal, equityTotal)
	if assetsTotal.Cmp(rightSide) != 0 {
		return []string{fmt.Sprintf(
			"balance_sheet_tie_out_failed: assets %s != liabilities + equity %s",
			CanonicalAmount(assetsTotal), CanonicalAmount(rightSide),
		)}, nil
	}
	return nil, nil
}
