package handler

import (
	"math"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payroll-run-svc/internal/compensation"
	"zoiko.io/payroll-run-svc/internal/domain"
)

// slipCalculation is one employee's resolved pay for a run.
type slipCalculation struct {
	gross       float64
	tax         float64
	deductions  float64
	net         float64
	taxable     float64
	currency    string
	structureID *string
}

// calculateSlip turns a base salary and an optional compensation breakdown into
// the four figures a payslip reports.
//
// Before compensation-svc was wired in, this was three hardcoded lines:
//
//	tax      := gross * 0.20
//	benefits := gross * 0.05
//	net      := gross - tax - benefits
//
// Gross and deductions now come from the employee's configured structure. Tax
// is still a rate rather than a jurisdiction rule — payroll-tax-svc owns that
// and is not wired yet — but it is applied to the taxable amount rather than to
// gross, which is a different and more defensible number: non-taxable earnings
// are excluded from it and taxable deductions come off it.
//
// A nil breakdown means the employee has no structure configured, which is a
// real state and not an error: they are paid a flat base salary with no
// components. That deliberately yields zero component deductions rather than
// the previous fabricated 5%.
func (h *Handler) calculateSlip(baseAmount float64, bd *compensation.Breakdown) slipCalculation {
	if bd == nil {
		taxable := round2(baseAmount)
		tax := round2(taxable * h.defaultTaxRate)
		return slipCalculation{
			gross:      round2(baseAmount),
			tax:        tax,
			deductions: 0,
			net:        round2(round2(baseAmount) - tax),
			taxable:    taxable,
		}
	}

	structureID := bd.StructureID
	tax := round2(bd.TaxableAmount * h.defaultTaxRate)

	return slipCalculation{
		gross:      bd.GrossEarnings,
		tax:        tax,
		deductions: bd.TotalDeductions,
		// bd.NetAmount is gross minus component deductions; tax is withheld on
		// top of it. Net can legitimately go negative when deductions exceed
		// pay — that is a reportable state, not something to clamp away.
		net:         round2(bd.NetAmount - tax),
		taxable:     bd.TaxableAmount,
		currency:    bd.Currency,
		structureID: &structureID,
	}
}

// buildSlipItems copies a breakdown's lines onto the slip that used them.
//
// The lines are copied rather than referenced on purpose: if the structure is
// edited next quarter, this payslip must still show the percentages it was
// actually paid on.
func buildSlipItems(tenantID, slipID string, now time.Time, bd *compensation.Breakdown) []domain.PaySlipItem {
	if bd == nil || len(bd.Lines) == 0 {
		return nil
	}

	items := make([]domain.PaySlipItem, 0, len(bd.Lines))
	for _, line := range bd.Lines {
		componentID := line.ComponentID
		var componentIDPtr *string
		if componentID != "" {
			componentIDPtr = &componentID
		}

		items = append(items, domain.PaySlipItem{
			ItemID:            uuid.NewString(),
			TenantID:          tenantID,
			SlipID:            slipID,
			ComponentID:       componentIDPtr,
			ComponentCode:     line.ComponentCode,
			ComponentName:     line.ComponentName,
			ComponentType:     line.ComponentType,
			IsTaxable:         line.IsTaxable,
			CalculationMethod: line.CalculationMethod,
			CalculationValue:  line.CalculationValue,
			Amount:            line.Amount,
			Sequence:          line.Sequence,
			CreatedAt:         now,
		})
	}
	return items
}

// round2 rounds to two decimal places, so stored totals match the lines a
// reader can see on the payslip rather than drifting from them.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
