package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// LedgerPosition implements kernel.ReconRepo: the sums of the entries of one control type in one book up to a date and, when
// upToSeq is given, up to a watermark, so the same position can be reproduced later. IncludeUnbooked adds the entries of the
// entity that name no book; GroupBy also sums the position by a dimension.
func (r *KernelRepo) LedgerPosition(ctx context.Context, tenant, entity, bookCode, control string, asOf time.Time, upToSeq int64, opts kernel.PositionOpts) (kernel.LedgerPosition, error) {
	var p kernel.LedgerPosition
	const where = `tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND (book_id = $3 OR ($7::boolean AND book_id = '')) AND control_type = $4 AND posting_date <= $5::date AND ($6::bigint = 0 OR entry_seq <= $6)`
	err := r.tx(ctx, tenant, func(t pgx.Tx) error {
		if e := t.QueryRow(ctx, `
			SELECT COALESCE(max(entry_seq), 0), count(*), COALESCE(sum(debit_amount), 0)::text, COALESCE(sum(credit_amount), 0)::text,
			       COALESCE(array_agg(DISTINCT account_code), '{}')
			FROM ledger_entries WHERE `+where, tenant, entity, bookCode, control, asOf, upToSeq, opts.IncludeUnbooked).Scan(&p.Watermark, &p.Count, &p.DebitTotal, &p.CreditTotal, &p.AccountCodes); e != nil {
			return e
		}
		if opts.GroupBy == "" {
			return nil
		}
		// the groups are read to the watermark of the total: nothing newer is counted in either
		rows, e := t.Query(ctx, `SELECT COALESCE(dimensions->>$8, ''), sum(debit_amount)::text, sum(credit_amount)::text FROM ledger_entries
			WHERE `+where+` AND entry_seq <= $9 GROUP BY 1 ORDER BY 1`, tenant, entity, bookCode, control, asOf, upToSeq, opts.IncludeUnbooked, opts.GroupBy, p.Watermark)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var g kernel.GroupTotal
			if e := rows.Scan(&g.Key, &g.Debit, &g.Credit); e != nil {
				return e
			}
			p.Groups = append(p.Groups, g)
		}
		return rows.Err()
	})
	return p, kernelErr(err)
}
