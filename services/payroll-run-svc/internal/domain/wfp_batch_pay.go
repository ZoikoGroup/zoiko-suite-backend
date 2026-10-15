package domain

// A payroll calculation that names a FROZEN WFP input batch takes its inputs from that batch instead of
// re-reading contracts, compensation and benefits (WFP-CTRL-006: the calculation uses the versioned frozen
// population). This file turns one worker's batch lines into the figures a payslip needs. It is pure and exact:
// amounts are summed as rationals and rounded once, and anything it cannot treat is an error rather than a
// guess.

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// BatchItem is one payslip line derived from batch lines: lines with the same type and payroll code are
// summed into one (a payslip carries one line per component code).
type BatchItem struct {
	Code      string
	Type      string // EARNING | DEDUCTION
	IsTaxable bool
	Amount    float64
}

// BatchWorkerPay is one worker's pay derived from their ACCEPTED batch lines.
type BatchWorkerPay struct {
	WorkerID   string
	Currency   string
	Gross      float64
	Deductions float64 // employee-side deductions and employee benefit contributions
	Taxable    float64
	Items      []BatchItem // every line except base salary (base is gross less the component earnings)
}

// BuildBatchPay derives each worker's pay from a batch's ACCEPTED lines. Rules:
//   - EARNING lines make gross; TAXABLE ones make taxable pay.
//   - DEDUCTION lines and BENEFIT_EMPLOYEE contributions are employee deductions; PRE_TAX ones come off taxable pay.
//   - BENEFIT_EMPLOYER lines are employer cost: not a deduction, not pay.
//   - REIMBURSEMENT and ADJUSTMENT have no defined treatment in the calculation yet and are refused.
//   - One worker has one currency.
func BuildBatchPay(lines []InputLine) (map[string]*BatchWorkerPay, error) {
	type acc struct {
		gross, ded, taxableGross, preTax *big.Rat
		currency                         string
		items                            map[string]*struct {
			item BatchItem
			sum  *big.Rat
		}
	}
	byWorker := map[string]*acc{}
	for _, l := range lines {
		if l.State != InputLineAccepted {
			continue
		}
		a := byWorker[l.WorkerID]
		if a == nil {
			a = &acc{gross: new(big.Rat), ded: new(big.Rat), taxableGross: new(big.Rat), preTax: new(big.Rat), currency: l.Currency,
				items: map[string]*struct {
					item BatchItem
					sum  *big.Rat
				}{}}
			byWorker[l.WorkerID] = a
		}
		if l.Currency != a.currency {
			return nil, fmt.Errorf("%w: worker %s has %s and %s", ErrBatchMixedCurrency, l.WorkerID, a.currency, l.Currency)
		}
		amt, err := ParseDecimal(l.Amount)
		if err != nil {
			return nil, fmt.Errorf("line %s: %w", l.LineID, err)
		}
		treatment := ""
		if l.TaxTreatment != nil {
			treatment = *l.TaxTreatment
		}
		code := l.InputCode
		if l.PayrollCode != nil && strings.TrimSpace(*l.PayrollCode) != "" {
			code = *l.PayrollCode
		}
		add := func(typ string, taxable bool) {
			if l.SourceKind == SourceBaseSalary {
				return
			}
			key := typ + "|" + code
			e := a.items[key]
			if e == nil {
				e = &struct {
					item BatchItem
					sum  *big.Rat
				}{item: BatchItem{Code: code, Type: typ, IsTaxable: taxable}, sum: new(big.Rat)}
				a.items[key] = e
			}
			e.sum.Add(e.sum, amt)
		}
		switch {
		case l.Category == CategoryEarning:
			a.gross.Add(a.gross, amt)
			if treatment == TaxTaxable {
				a.taxableGross.Add(a.taxableGross, amt)
			}
			add("EARNING", treatment == TaxTaxable)
		case l.Category == CategoryDeduction || (l.Category == CategoryBenefit && l.SourceKind == SourceBenefitEmployee):
			a.ded.Add(a.ded, amt)
			if treatment == TaxPreTax {
				a.preTax.Add(a.preTax, amt)
			}
			add("DEDUCTION", treatment == TaxPreTax)
		case l.Category == CategoryBenefit && l.SourceKind == SourceBenefitEmployer:
			// employer cost: not part of the worker's pay
		default:
			return nil, fmt.Errorf("%w: %s on worker %s", ErrBatchCategoryUnsupported, l.Category, l.WorkerID)
		}
	}
	out := make(map[string]*BatchWorkerPay, len(byWorker))
	for id, a := range byWorker {
		taxable := new(big.Rat).Sub(a.taxableGross, a.preTax)
		if taxable.Sign() < 0 {
			taxable = new(big.Rat)
		}
		p := &BatchWorkerPay{WorkerID: id, Currency: a.currency, Gross: ratToMoney(a.gross), Deductions: ratToMoney(a.ded), Taxable: ratToMoney(taxable)}
		keys := make([]string, 0, len(a.items))
		for k := range a.items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e := a.items[k]
			it := e.item
			it.Amount = ratToMoney(e.sum)
			p.Items = append(p.Items, it)
		}
		out[id] = p
	}
	return out, nil
}

// ratToMoney rounds an exact amount to two decimals, half away from zero, once.
func ratToMoney(r *big.Rat) float64 {
	cents := new(big.Rat).Mul(r, big.NewRat(100, 1))
	n, d := cents.Num(), cents.Denom()
	q, rem := new(big.Int).QuoRem(n, d, new(big.Int))
	twice := new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2))
	if twice.Cmp(d) >= 0 {
		if n.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	f, _ := new(big.Rat).SetFrac(q, big.NewInt(100)).Float64()
	return f
}

var (
	ErrBatchMixedCurrency       = errorString("a worker's batch lines are in more than one currency")
	ErrBatchCategoryUnsupported = errorString("the batch carries an input category the calculation has no treatment for")
	ErrBatchNotFrozen           = errorString("the input batch is not FROZEN")
	ErrBatchEntityMismatch      = errorString("the input batch belongs to a different legal entity than the run")
	ErrBatchPopulationMismatch  = errorString("the frozen batch and the active workers are not the same population")
	ErrBatchEmpty               = errorString("the input batch has no accepted lines")
	ErrRunLinkedToOtherBatch    = errorString("this run already consumed a different input batch")
)
