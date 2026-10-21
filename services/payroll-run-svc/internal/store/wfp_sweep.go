package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SweepDueAllTenants is the background form of the liability DUE sweep: RECOGNIZED
// liabilities whose due date has arrived become DUE, for every tenant. Like the posting
// dispatcher it finds the tenants under the relay context, then works inside each tenant's
// own context. Moving to DUE is idempotent (a second run finds nothing), so replicas that
// overlap do no harm. It returns how many liabilities moved.
func (s *PgStore) SweepDueAllTenants(ctx context.Context, asOf string, mk LiabilityEventBuilder) (int, error) {
	moved := 0
	err := s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM wfp_payroll_liabilities WHERE state = 'RECOGNIZED' AND due_date <= $1::date`, asOf)
		if err != nil {
			return fmt.Errorf("find tenants with due liabilities: %w", err)
		}
		var tenants []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			tenants = append(tenants, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range tenants {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", t); err != nil {
				return fmt.Errorf("set tenant context: %w", err)
			}
			list, err := queryLiabilities(ctx, tx, `
				UPDATE wfp_payroll_liabilities SET state = 'DUE', version = version + 1, updated_at = now()
				WHERE tenant_id = $1 AND state = 'RECOGNIZED' AND due_date <= $2::date
				RETURNING `+liabCols, t, asOf)
			if err != nil {
				return err
			}
			for i := range list {
				if err := enqueueLiability(ctx, tx, mk, LiabilityChange{Kind: "DUE", Liability: &list[i]}); err != nil {
					return err
				}
			}
			moved += len(list)
		}
		return nil
	})
	return moved, err
}
