package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// Scope of the entry-legs population. $1 = tenant_id, $2 = legal_entity_id.
// The tenant predicate is explicit; RLS (app.tenant_id) is defence in depth.
// Amounts stay NUMERIC end to end and are only ever read as text.
//
// The target leg only exists once a target journal has been matched, so a
// target scope without target_journal_id is excluded. uuid::text is canonical
// lower-case. Every match_status is returned.
const entryLegsSelect = `
	SELECT intercompany_entry_id::text AS id,
	       %s AS reference,
	       amount AS amount,
	       currency_code AS currency,
	       (created_at AT TIME ZONE 'UTC')::date AS bdate,
	       match_status AS status,
	       %s AS counterparty,
	       source_journal_id::text AS source_journal_id,
	       COALESCE(target_journal_id::text, '') AS target_journal_id
	FROM intercompany_entries
	WHERE tenant_id = $1 AND %s`

var (
	entryLegsSourceScope = fmt.Sprintf(entryLegsSelect,
		`source_journal_id::text`, `target_legal_entity_id`, `source_legal_entity_id = $2`)
	entryLegsTargetScope = fmt.Sprintf(entryLegsSelect,
		`target_journal_id::text`, `source_legal_entity_id`, `target_legal_entity_id = $2 AND target_journal_id IS NOT NULL`)
)

// ControlPopulation returns one keyset page of entry-legs plus the whole-set
// watermark and declared totals, all read in ONE REPEATABLE READ, READ ONLY
// transaction so they are mutually consistent. The population is lifetime:
// entries carry no period.
//
// Watermark = '<count>:<md5(string_agg(id|status|amount|reference, ',' ORDER BY id))>'.
func (s *PgStore) ControlPopulation(ctx context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	var scope string
	switch q.Leg {
	case domain.LegSource:
		scope = entryLegsSourceScope
	case domain.LegTarget:
		scope = entryLegsTargetScope
	default:
		return nil, fmt.Errorf("unknown leg %q", q.Leg)
	}
	if q.TenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if ctxTenant := svcmiddleware.TenantFromContext(ctx); ctxTenant != q.TenantID {
		return nil, fmt.Errorf("tenant mismatch between context and query")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only; nothing to commit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", q.TenantID); err != nil {
		return nil, fmt.Errorf("set tenant context: %w", err)
	}

	args := []any{q.TenantID, q.LegalEntityID}
	out := &domain.ControlPopulationPage{
		Records:        []domain.ControlRecord{},
		DeclaredTotals: domain.ControlDeclaredTotals{Totals: map[string]string{}},
	}

	var count int
	var digest string
	if err := tx.QueryRow(ctx, `WITH scope AS (`+scope+`)
		SELECT count(*),
		       COALESCE(md5(string_agg(id || '|' || status || '|' || amount::text || '|' || reference, ',' ORDER BY id COLLATE "C")), '')
		FROM scope`, args...).Scan(&count, &digest); err != nil {
		return nil, err
	}
	if count > domain.ControlPopulationMaxRecords {
		return nil, domain.ErrPopulationTooLarge
	}
	out.DeclaredTotals.RowCount = count
	out.Watermark = fmt.Sprintf("%d:%s", count, digest)

	trows, err := tx.Query(ctx, `WITH scope AS (`+scope+`)
		SELECT currency, sum(amount)::text FROM scope GROUP BY currency ORDER BY currency`, args...)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var cur, total string
		if err := trows.Scan(&cur, &total); err != nil {
			trows.Close()
			return nil, err
		}
		out.DeclaredTotals.Totals[cur] = total
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}

	// Fetch limit+1 to learn whether another page exists, without OFFSET.
	pageArgs := append(append([]any{}, args...), q.AfterRecordID, q.Limit+1)
	rows, err := tx.Query(ctx, `WITH scope AS (`+scope+`)
		SELECT id, reference, amount::text, currency, to_char(bdate, 'YYYY-MM-DD'),
		       status, counterparty, source_journal_id, target_journal_id
		FROM scope
		WHERE ($3::text = '' OR id COLLATE "C" > $3::text COLLATE "C")
		ORDER BY id COLLATE "C"
		LIMIT $4`, pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.ControlRecord
		var status, counterparty, srcJ, tgtJ string
		if err := rows.Scan(&r.RecordID, &r.Reference, &r.Amount, &r.Currency, &r.Date,
			&status, &counterparty, &srcJ, &tgtJ); err != nil {
			return nil, err
		}
		r.Attributes = map[string]string{
			"intercompany_entry_id":  r.RecordID,
			"match_status":           status,
			"leg":                    q.Leg,
			"counterparty_entity_id": counterparty,
			"source_journal_id":      srcJ,
			"target_journal_id":      tgtJ,
		}
		out.Records = append(out.Records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Records) > q.Limit {
		out.Records = out.Records[:q.Limit]
		out.NextCursor = out.Records[q.Limit-1].RecordID // raw id; the handler makes it opaque
	}
	return out, nil
}
