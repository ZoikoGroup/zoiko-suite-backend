package store

// Rule effectivity crossings (ZS-JUR-001 s26 regulatory-rule.effective and
// .expired; migration 000035).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// RuleCrossing is a rule whose effective interval started or ended.
type RuleCrossing struct {
	Kind               string // "effective" or "expired"
	JurisdictionRuleID string
	JurisdictionID     string
	RuleDomain         string
	RuleCode           string
	RuleVersion        int
	RuleStatus         string
	At                 time.Time // effective_from or effective_to
	RowVersion         int64
}

// ClaimRuleCrossings returns the crossings in (scanned_through, now] and moves
// the watermark to now. It must run inside a unit of work, so the watermark and
// the events the caller writes for the crossings commit together. The
// watermark row is locked (FOR UPDATE), so two replicas never announce the same
// crossing. The first call only starts the watermark and returns nothing.
//
// A rule counts when it is in force: ACTIVE for a start; ACTIVE, SUPERSEDED or
// RETIRED for an end (a superseded rule's interval is closed when it ends).
func (s *PgStore) ClaimRuleCrossings(ctx context.Context, now time.Time) ([]RuleCrossing, error) {
	q := s.db(ctx)
	var from time.Time
	err := q.QueryRow(ctx, `SELECT scanned_through FROM rule_effectivity_watch WHERE watch_id = 1 FOR UPDATE`).Scan(&from)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := q.Exec(ctx, `INSERT INTO rule_effectivity_watch (watch_id, scanned_through) VALUES (1, $1) ON CONFLICT (watch_id) DO NOTHING`, now); err != nil {
			return nil, fmt.Errorf("%w: start effectivity watermark: %v", domain.ErrStoreUnavailable, err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read effectivity watermark: %v", domain.ErrStoreUnavailable, err)
	}
	if !now.After(from) {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT 'effective', jurisdiction_rule_id::text, jurisdiction_id::text, rule_domain, rule_code, rule_version, rule_status, effective_from, row_version
		  FROM jurisdiction_rules
		 WHERE rule_status = 'ACTIVE' AND effective_from > $1 AND effective_from <= $2
		UNION ALL
		SELECT 'expired', jurisdiction_rule_id::text, jurisdiction_id::text, rule_domain, rule_code, rule_version, rule_status, effective_to, row_version
		  FROM jurisdiction_rules
		 WHERE rule_status IN ('ACTIVE', 'SUPERSEDED', 'RETIRED') AND effective_to > $1 AND effective_to <= $2
		 ORDER BY 8, 2`, from, now)
	if err != nil {
		return nil, fmt.Errorf("%w: scan rule crossings: %v", domain.ErrStoreUnavailable, err)
	}
	var out []RuleCrossing
	for rows.Next() {
		var c RuleCrossing
		if err := rows.Scan(&c.Kind, &c.JurisdictionRuleID, &c.JurisdictionID, &c.RuleDomain, &c.RuleCode, &c.RuleVersion, &c.RuleStatus, &c.At, &c.RowVersion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: scan rule crossing: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rule crossings: %v", domain.ErrStoreUnavailable, err)
	}
	if _, err := q.Exec(ctx, `UPDATE rule_effectivity_watch SET scanned_through = $1, updated_at = NOW() WHERE watch_id = 1`, now); err != nil {
		return nil, fmt.Errorf("%w: advance effectivity watermark: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
