package store

// GET /v1/decisions/summary: the register's KPIs, panels and filter facets,
// computed in one transaction so every figure describes the same moment.

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// Summary — see Store. Window figures count decisions whose decided_at falls
// in [From, To); the chain, package and outbox figures are the tenant's
// whole ledger, which no window narrows.
func (s *PgStore) Summary(ctx context.Context, p SummaryParams) (*domain.DecisionSummary, error) {
	span := p.To.Sub(p.From)
	out := &domain.DecisionSummary{From: p.From, To: p.To, PreviousFrom: p.From.Add(-span)}
	out.Facets.Priorities = append([]string{}, domain.Priorities...)
	out.Facets.Categories = []string{domain.CategoryApproved, domain.CategoryPending, domain.CategoryEscalated, domain.CategoryDenied, domain.CategoryOther}

	// $1 tenant, $2 from, $3 to, $4 entity ('' = every entity).
	const scope = `g.tenant_id = $1 AND g.decided_at >= $2 AND g.decided_at < $3 AND ($4 = '' OR g.legal_entity_id = $4)`
	args := []any{p.TenantID, p.From, p.To, p.LegalEntityID}
	prevArgs := []any{p.TenantID, out.PreviousFrom, p.From, p.LegalEntityID}

	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		countBy := func(target *domain.CategoryCounts, a []any, extra string) error {
			rows, err := tx.Query(ctx, `SELECT `+categoryExpr+` AS cat, count(*) FROM governance_decisions g
				WHERE `+scope+extra+` GROUP BY cat`, a...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var cat string
				var n int64
				if err := rows.Scan(&cat, &n); err != nil {
					return err
				}
				target.Add(cat, n)
			}
			return rows.Err()
		}
		if err := countBy(&out.Totals, args, ""); err != nil {
			return err
		}
		if err := countBy(&out.PreviousTotals, prevArgs, ""); err != nil {
			return err
		}
		var open domain.CategoryCounts
		if err := countBy(&open, args, ` AND NOT EXISTS (SELECT 1 FROM governance_decisions f
			WHERE f.tenant_id = g.tenant_id AND f.causation_id = g.decision_id)`); err != nil {
			return err
		}
		out.OpenPending, out.OpenEscalated = open.Pending, open.Escalated

		named := func(q string, a ...any) ([]domain.NamedCount, error) {
			rows, err := tx.Query(ctx, q, a...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			res := []domain.NamedCount{}
			for rows.Next() {
				var c domain.NamedCount
				if err := rows.Scan(&c.Key, &c.Label, &c.Count); err != nil {
					return nil, err
				}
				res = append(res, c)
			}
			return res, rows.Err()
		}
		var err error
		if out.ByDomain, err = named(`SELECT d.domain_code, d.label, count(g.decision_id)
			FROM decision_domains d
			LEFT JOIN governance_decisions g ON g.domain = d.domain_code AND `+scope+`
			GROUP BY d.domain_code, d.label, d.sort_order
			ORDER BY count(g.decision_id) DESC, d.sort_order`, args...); err != nil {
			return err
		}
		if out.ByJurisdiction, err = named(`SELECT COALESCE(g.jurisdiction_context, ''), COALESCE(g.jurisdiction_context, 'None recorded'), count(*)
			FROM governance_decisions g WHERE `+scope+`
			GROUP BY g.jurisdiction_context ORDER BY count(*) DESC, 1`, args...); err != nil {
			return err
		}
		if out.ByPriority, err = named(`SELECT COALESCE(g.priority, 'UNSET'), COALESCE(g.priority, 'Not set'), count(*)
			FROM governance_decisions g WHERE `+scope+`
			GROUP BY g.priority ORDER BY count(*) DESC, 1`, args...); err != nil {
			return err
		}
		if out.TopSources, err = named(`SELECT g.actor_id, g.actor_id, count(*)
			FROM governance_decisions g WHERE `+scope+`
			GROUP BY g.actor_id ORDER BY count(*) DESC, 1 LIMIT 5`, args...); err != nil {
			return err
		}

		// Dense daily series (UTC days): a day with no decision is a zero.
		byDay := map[string]*domain.CategoryCounts{}
		rows, err := tx.Query(ctx, `SELECT to_char(g.decided_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), `+categoryExpr+`, count(*)
			FROM governance_decisions g WHERE `+scope+` GROUP BY 1, 2`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var day, cat string
			var n int64
			if err := rows.Scan(&day, &cat, &n); err != nil {
				rows.Close()
				return err
			}
			if byDay[day] == nil {
				byDay[day] = &domain.CategoryCounts{}
			}
			byDay[day].Add(cat, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for d := truncDay(p.From); d.Before(p.To); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			pt := domain.TrendPoint{Date: key}
			if c := byDay[key]; c != nil {
				pt.CategoryCounts = *c
			}
			out.Trend = append(out.Trend, pt)
		}

		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE NOT r.outcomes_match)
			FROM replay_manifests r
			JOIN governance_decisions g ON g.tenant_id = r.tenant_id AND g.decision_id = r.decision_id
			WHERE r.tenant_id = $1 AND r.replayed_at >= $2 AND r.replayed_at < $3 AND ($4 = '' OR g.legal_entity_id = $4)`,
			args...).Scan(&out.Replays.Total, &out.Replays.Mismatches); err != nil {
			return err
		}

		var head []byte
		switch err := tx.QueryRow(ctx, `SELECT chain_seq, curr_hash FROM governance_decisions
			WHERE tenant_id = $1 ORDER BY chain_seq DESC LIMIT 1`, p.TenantID).Scan(&out.Chain.Length, &head); err {
		case nil:
			out.Chain.HeadHash = hex.EncodeToString(head)
		case pgx.ErrNoRows:
		default:
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(max(to_seq), 0), max(sealed_at)
			FROM evidence_packages WHERE tenant_id = $1`, p.TenantID).Scan(
			&out.Chain.Packages, &out.Chain.SealedThrough, &out.Chain.LastSealedAt); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE published_at IS NULL),
				count(*) FILTER (WHERE published_at IS NOT NULL),
				COALESCE(EXTRACT(EPOCH FROM now() - min(created_at) FILTER (WHERE published_at IS NULL)), 0)
			FROM outbox WHERE tenant_id = $1`, p.TenantID).Scan(
			&out.Outbox.Pending, &out.Outbox.Published, &out.Outbox.OldestPendingSeconds); err != nil {
			return err
		}

		drows, err := tx.Query(ctx, `SELECT domain_code, label FROM decision_domains ORDER BY sort_order`)
		if err != nil {
			return err
		}
		for drows.Next() {
			var d domain.DomainRef
			if err := drows.Scan(&d.Code, &d.Label); err != nil {
				drows.Close()
				return err
			}
			out.Facets.Domains = append(out.Facets.Domains, d)
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return err
		}
		jrows, err := tx.Query(ctx, `SELECT DISTINCT jurisdiction_context FROM governance_decisions
			WHERE tenant_id = $1 AND jurisdiction_context IS NOT NULL ORDER BY 1`, p.TenantID)
		if err != nil {
			return err
		}
		defer jrows.Close()
		out.Facets.Jurisdictions = []string{}
		for jrows.Next() {
			var j string
			if err := jrows.Scan(&j); err != nil {
				return err
			}
			out.Facets.Jurisdictions = append(out.Facets.Jurisdictions, j)
		}
		return jrows.Err()
	})
	if err != nil {
		return nil, s.unavailable("Summary", err)
	}
	return out, nil
}

func truncDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
