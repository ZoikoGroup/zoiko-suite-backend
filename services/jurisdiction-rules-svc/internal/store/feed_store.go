package store

// Automatic legal drift from a regulatory feed (Doc 03 §8.2 "external
// regulatory feed changes" consumed; Governance audit J-03/J-04).

import (
	"context"
	"fmt"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ClaimConsumedEvent records eventID as consumed in q's transaction. It returns
// false when the event was consumed before (a redelivery): the caller applies
// nothing. Shared with the entity consumer's de-duplication table.
func (s *PgStore) ClaimConsumedEvent(ctx context.Context, q Querier, eventID, eventType string) (bool, error) {
	tag, err := q.Exec(ctx, `INSERT INTO consumed_events (event_id, event_type) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING`, eventID, eventType)
	if err != nil {
		return false, fmt.Errorf("%w: claim consumed event: %v", domain.ErrStoreUnavailable, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ActiveRuleIDsByFeedReference lists the ACTIVE rules that cite an external
// feed reference, oldest first.
func (s *PgStore) ActiveRuleIDsByFeedReference(ctx context.Context, q Querier, ref string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT jurisdiction_rule_id::text FROM jurisdiction_rules
		WHERE external_feed_reference = $1 AND rule_status = 'ACTIVE' ORDER BY created_at, jurisdiction_rule_id`, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: rules by feed reference: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: rules by feed reference: %v", domain.ErrStoreUnavailable, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
