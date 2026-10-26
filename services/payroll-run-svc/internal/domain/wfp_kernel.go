package domain

import (
	"fmt"
	"math/big"
	"strings"
)

// KernelFacts expresses a frozen payroll proposal as the facts of general-ledger-svc's PAYROLL.RESULT_ACCEPTED accounting event, for a
// tenant that posts payroll through the accounting kernel instead of the legacy posting command. The ledger's rule turns the facts
// back into the same lines (gross, net, and each employee and employer component kind on its own account) and refuses a result that
// does not balance. Nothing is recomputed here: an amount is carried as the proposal states it, and a line this function cannot
// express (an unknown mapping key, a repeated key, an employer liability that differs from the employer cost) is an error, never
// dropped.
func (b *AccountingBridge) KernelFacts() (map[string]string, error) {
	facts := map[string]string{"gross": "0", "net": "0", "result_ref": b.RunRefID, "result_version": b.ResultID}
	for _, k := range employeeKindOrder {
		facts["ee_"+strings.ToLower(kindSuffix(k))] = "0"
	}
	for _, k := range employerKindOrder {
		facts["er_"+strings.ToLower(kindSuffix(k))] = "0"
	}
	seen := map[string]bool{}
	employerPayable := map[string]*big.Rat{}
	set := func(l PostingLine, fact, side string) error {
		amt := l.Debit
		if side == "credit" {
			amt = l.Credit
		}
		if seen[l.MappingKey] {
			return fmt.Errorf("the mapping key %s appears twice in the proposal", l.MappingKey)
		}
		seen[l.MappingKey] = true
		facts[fact] = amt
		return nil
	}
	expense := map[string]string{}
	for _, k := range employerKindOrder {
		expense[EmployerExpenseKey(k)] = "er_" + strings.ToLower(kindSuffix(k))
	}
	payable := map[string]string{}
	for _, k := range employerKindOrder {
		payable[EmployerPayableKey(k)] = "er_" + strings.ToLower(kindSuffix(k))
	}
	employee := map[string]string{}
	for _, k := range employeeKindOrder {
		employee[EmployeePayableKey(k)] = "ee_" + strings.ToLower(kindSuffix(k))
	}
	for _, l := range b.Lines {
		var err error
		switch fact, ok := "", false; {
		case l.MappingKey == KeyGrossExpense:
			err = set(l, "gross", "debit")
		case l.MappingKey == KeyNetPayable:
			err = set(l, "net", "credit")
		default:
			if fact, ok = employee[l.MappingKey]; ok {
				err = set(l, fact, "credit")
			} else if fact, ok = expense[l.MappingKey]; ok {
				err = set(l, fact, "debit")
			} else if fact, ok = payable[l.MappingKey]; ok {
				if seen[l.MappingKey] {
					err = fmt.Errorf("the mapping key %s appears twice in the proposal", l.MappingKey)
					break
				}
				seen[l.MappingKey] = true
				v, perr := ParseDecimal(l.Credit)
				if perr != nil {
					err = perr
					break
				}
				employerPayable[fact] = v
			} else {
				err = fmt.Errorf("the mapping key %s has no place in a payroll result", l.MappingKey)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	for fact, v := range employerPayable {
		cost, err := ParseDecimal(facts[fact])
		if err != nil {
			return nil, err
		}
		if cost.Cmp(v) != 0 {
			return nil, fmt.Errorf("the employer liability %s (%s) differs from the employer cost (%s)", fact, FormatDecimal(v), FormatDecimal(cost))
		}
	}
	for key, fact := range expense {
		if !seen[key] {
			if p, ok := employerPayable[fact]; ok && p.Sign() != 0 {
				return nil, fmt.Errorf("the employer liability %s has no matching employer cost", fact)
			}
		}
	}
	return facts, nil
}
