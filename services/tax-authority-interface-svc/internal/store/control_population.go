package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
)

// ControlPopulationReader is implemented by stores that can serve the
// ZS-CONTROL-001 §9 control populations (see
// docs/architecture/control-population-contract.md). It is deliberately not part
// of Store, so the in-memory double is not forced to fake SQL semantics.
type ControlPopulationReader interface {
	QueryUnacknowledgedFilings(ctx context.Context, tenantID string, q domain.UnacknowledgedFilingsQuery) (*domain.ControlPopulationPage, error)
}

// unacknowledgedFilingsScope: $1 tenant_id, $2 legal_entity_id, $3 submitted_before
// (timestamptz). The entity comes from the owning tax_interfaces row (the
// submission table has no legal_entity_id); both tables carry an explicit
// tenant_id predicate. The violation set is every submission before the cut-off
// whose ack_reference is NULL or blank. No taxpayer identifiers, credentials or
// authority payloads are selected. tax_filing_submissions has no currency column,
// so currency is the ISO 4217 "no currency" code XXX. age_days derives from $3,
// never from now().
const unacknowledgedFilingsScope = `
	SELECT s.submission_id                                     AS record_id,
	       s.submission_id                                     AS reference,
	       s.tax_amount                                        AS amount,
	       'XXX'::text                                         AS currency,
	       (s.submitted_at AT TIME ZONE 'UTC')::date           AS txn_date,
	       jsonb_build_object(
	           'interface_id', s.interface_id,
	           'tax_period',   s.tax_period,
	           'filing_type',  s.filing_type,
	           'status',       s.status,
	           'age_days',     floor(extract(epoch FROM ($3::timestamptz - s.submitted_at))::numeric / 86400)::bigint::text) AS attrs
	FROM tax_filing_submissions s
	JOIN tax_interfaces i
	  ON i.interface_id = s.interface_id AND i.tenant_id = s.tenant_id
	WHERE s.tenant_id = $1::text
	  AND i.tenant_id = $1::text
	  AND i.legal_entity_id = $2::text
	  AND s.submitted_at < $3::timestamptz
	  AND (s.ack_reference IS NULL OR s.ack_reference ~ '^\s*$')`

// controlDigestExpr covers every wire field including all attributes, so any
// change to a record changes the watermark.
const controlDigestExpr = `record_id || '|' || reference || '|' || amount::text || '|' || currency || '|' || txn_date::text || '|' || attrs::text`

// QueryUnacknowledgedFilings returns one page of the unacknowledged-filings
// population, its whole-set watermark and declared totals, all read in one
// REPEATABLE READ read-only transaction.
func (p *PgStore) QueryUnacknowledgedFilings(ctx context.Context, tenantID string, q domain.UnacknowledgedFilingsQuery) (*domain.ControlPopulationPage, error) {
	args := []any{tenantID, q.LegalEntityID, q.SubmittedBefore.UTC()}
	var after any
	if q.AfterRecordID != "" {
		after = q.AfterRecordID
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}

	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_id: %w", err)
	}

	page := &domain.ControlPopulationPage{
		Records:        []domain.ControlPopulationRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	var digest string
	err = tx.QueryRow(ctx, `
		WITH scope AS (`+unacknowledgedFilingsScope+`)
		SELECT COUNT(*),
		       md5(COALESCE(string_agg(`+controlDigestExpr+`, ',' ORDER BY record_id COLLATE "C"), ''))
		FROM scope`, args...,
	).Scan(&page.DeclaredTotals.RowCount, &digest)
	if err != nil {
		return nil, err
	}
	if page.DeclaredTotals.RowCount > domain.MaxControlPopulationRecords {
		return nil, domain.ErrControlPopulationTooLarge
	}
	page.Watermark = fmt.Sprintf("tx1:n=%d;md5=%s", page.DeclaredTotals.RowCount, digest)

	totals, err := tx.Query(ctx, `
		WITH scope AS (`+unacknowledgedFilingsScope+`)
		SELECT currency, SUM(amount)::text FROM scope GROUP BY currency ORDER BY currency COLLATE "C"`, args...)
	if err != nil {
		return nil, err
	}
	for totals.Next() {
		var cur, sum string
		if err := totals.Scan(&cur, &sum); err != nil {
			totals.Close()
			return nil, err
		}
		page.DeclaredTotals.Totals[cur] = sum
	}
	totals.Close()
	if err := totals.Err(); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		WITH scope AS (`+unacknowledgedFilingsScope+`)
		SELECT record_id, reference, amount::text, currency, to_char(txn_date, 'YYYY-MM-DD'), attrs
		FROM scope
		WHERE ($4::text IS NULL OR record_id COLLATE "C" > $4::text COLLATE "C")
		ORDER BY record_id COLLATE "C" ASC
		LIMIT $5`, append(args, after, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlPopulationRecord
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date, &r.Attributes); err != nil {
			return nil, err
		}
		page.Records = append(page.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		page.NextRecordID = page.Records[limit-1].RecordID
	}
	return page, nil
}
