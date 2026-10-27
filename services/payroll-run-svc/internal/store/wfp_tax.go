package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// insertTaxLinesTx stores the tax engine's breakdown of one payslip, in the transaction that stores the
// slip. A slip with no engine lines (the legacy placeholder, or no tax at all) stores none.
func insertTaxLinesTx(ctx context.Context, tx pgx.Tx, tenantID string, slip domain.PaySlip) error {
	for _, l := range slip.TaxLines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pay_slip_tax_lines (
				line_id, tenant_id, slip_id, sequence, tax_name, tax_type, rate_pct, amount,
				calculation_id, engine_type, rule_version, jurisdiction_code, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			l.LineID, tenantID, slip.SlipID, l.Sequence, l.TaxName, l.TaxType, l.RatePct, l.Amount,
			l.CalculationID, l.EngineType, l.RuleVersion, l.JurisdictionCode, l.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

// attachTaxLinesTx loads the tax lines of every slip of a run in one query and attaches them.
func attachTaxLinesTx(ctx context.Context, tx pgx.Tx, tenantID, runID string, slips []domain.PaySlip) error {
	rows, err := tx.Query(ctx, `
		SELECT l.line_id::text, l.tenant_id, l.slip_id::text, l.sequence, l.tax_name, l.tax_type, l.rate_pct::float8, l.amount::float8,
		       l.calculation_id, l.engine_type, l.rule_version, l.jurisdiction_code, l.created_at
		FROM pay_slip_tax_lines l
		JOIN pay_slips s ON s.slip_id = l.slip_id
		WHERE s.run_id = $1 AND l.tenant_id = $2
		ORDER BY l.slip_id, l.sequence`, runID, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	bySlip := map[string][]domain.PaySlipTaxLine{}
	for rows.Next() {
		var l domain.PaySlipTaxLine
		if err := rows.Scan(&l.LineID, &l.TenantID, &l.SlipID, &l.Sequence, &l.TaxName, &l.TaxType, &l.RatePct, &l.Amount,
			&l.CalculationID, &l.EngineType, &l.RuleVersion, &l.JurisdictionCode, &l.CreatedAt); err != nil {
			return err
		}
		bySlip[l.SlipID] = append(bySlip[l.SlipID], l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range slips {
		slips[i].TaxLines = bySlip[slips[i].SlipID]
	}
	return nil
}
