package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ErrMergeConflict: the merge cannot be made as asked. The message says why; it is a 409.
var ErrMergeConflict = errors.New("identity merge conflict")

// Link outcomes of a merge.
const (
	LinkMoved   = "MOVED"
	LinkRetired = "RETIRED"
)

// LinkOutcome is what a merge did with one of the duplicate's provider links.
type LinkOutcome struct {
	LinkID string `json:"link_id"`
	Issuer string `json:"issuer"`
	Action string `json:"action"`
}

// IdentityMerge is the record of one duplicate being merged into a survivor. The duplicate's id is the alias.
type IdentityMerge struct {
	MergeID       string        `json:"merge_id"`
	TenantID      string        `json:"tenant_id"`
	SurvivorID    string        `json:"survivor_id"`
	DuplicateID   string        `json:"duplicate_id"`
	Reason        string        `json:"reason"`
	MergedBy      string        `json:"merged_by"`
	CorrelationID string        `json:"correlation_id,omitempty"`
	MergedAt      time.Time     `json:"merged_at"`
	LinkOutcomes  []LinkOutcome `json:"link_outcomes"`
}

const mergeCols = `merge_id, tenant_id, survivor_id, duplicate_id, reason, merged_by, correlation_id, merged_at, link_outcomes`

func scanMerge(row pgx.Row) (*IdentityMerge, error) {
	var m IdentityMerge
	var raw []byte
	err := row.Scan(&m.MergeID, &m.TenantID, &m.SurvivorID, &m.DuplicateID, &m.Reason, &m.MergedBy, &m.CorrelationID, &m.MergedAt, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m.LinkOutcomes); err != nil {
		return nil, err
	}
	if m.LinkOutcomes == nil {
		m.LinkOutcomes = []LinkOutcome{}
	}
	return &m, nil
}

// MergeIdentities merges duplicateID into survivorID in one transaction: the duplicate is disabled, its provider
// links are moved to the survivor (or retired where the survivor already holds a link at that issuer), the
// decision is recorded on both, and recs (built by build from the outcome) are enqueued. Nothing is deleted and
// no id is reused: the duplicate keeps its row.
//
// Idempotent by (tenant, duplicate): repeating the same merge returns the recorded one with created=false;
// merging an already-merged duplicate into a different survivor is ErrMergeConflict.
func (s *PgStore) MergeIdentities(ctx context.Context, tenantID, survivorID, duplicateID, reason, actor, correlationID string,
	build func(m IdentityMerge) ([]outbox.Record, error)) (*IdentityMerge, bool, error) {
	var out *IdentityMerge
	created := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// One merge at a time per tenant: the chain checks below are only sound if nobody merges beside them.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('identity_merge'), hashtext($1))`, tenantID); err != nil {
			return err
		}
		if prior, err := scanMerge(tx.QueryRow(ctx, `SELECT `+mergeCols+` FROM principal_merges
			WHERE tenant_id = $1 AND duplicate_id = $2`, tenantID, duplicateID)); err != nil {
			return err
		} else if prior != nil {
			if prior.SurvivorID != survivorID {
				return fmt.Errorf("%w: %s was already merged into %s", ErrMergeConflict, duplicateID, prior.SurvivorID)
			}
			out = prior
			return nil
		}

		statuses := map[string]string{}
		rows, err := tx.Query(ctx, `SELECT principal_id, status FROM principals
			WHERE tenant_id = $1 AND principal_id IN ($2, $3) ORDER BY principal_id FOR UPDATE`, tenantID, survivorID, duplicateID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				rows.Close()
				return err
			}
			statuses[id] = st
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, ok := statuses[survivorID]; !ok {
			return fmt.Errorf("%w: survivor %s", domain.ErrPrincipalNotFound, survivorID)
		}
		if _, ok := statuses[duplicateID]; !ok {
			return fmt.Errorf("%w: duplicate %s", domain.ErrPrincipalNotFound, duplicateID)
		}
		if statuses[survivorID] == string(domain.PrincipalStatusDisabled) {
			return fmt.Errorf("%w: the survivor is disabled", ErrMergeConflict)
		}
		var chained bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principal_merges WHERE tenant_id = $1
			AND (duplicate_id = $2 OR survivor_id = $3))`, tenantID, survivorID, duplicateID).Scan(&chained); err != nil {
			return err
		}
		if chained {
			return fmt.Errorf("%w: a merge does not chain; the survivor was merged away or the duplicate already absorbed another identity", ErrMergeConflict)
		}

		now := time.Now().UTC()
		outcomes := []LinkOutcome{}
		lrows, err := tx.Query(ctx, `SELECT l.link_id, l.issuer,
			       EXISTS (SELECT 1 FROM federation_links s WHERE s.tenant_id = l.tenant_id AND s.principal_id = $3
			                AND s.issuer = l.issuer)
			  FROM federation_links l
			 WHERE l.tenant_id = $1 AND l.principal_id = $2 AND l.status <> 'DEPROVISIONED'
			 ORDER BY l.created_at, l.link_id`, tenantID, duplicateID, survivorID)
		if err != nil {
			return err
		}
		for lrows.Next() {
			var o LinkOutcome
			var held bool
			if err := lrows.Scan(&o.LinkID, &o.Issuer, &held); err != nil {
				lrows.Close()
				return err
			}
			o.Action = LinkMoved
			if held {
				o.Action = LinkRetired
			}
			outcomes = append(outcomes, o)
		}
		lrows.Close()
		if err := lrows.Err(); err != nil {
			return err
		}
		for _, o := range outcomes {
			if o.Action == LinkMoved {
				if _, err := tx.Exec(ctx, `UPDATE federation_links SET principal_id = $3 WHERE tenant_id = $1 AND link_id = $2`,
					tenantID, o.LinkID, survivorID); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE federation_links SET status = 'DEPROVISIONED', deprovisioned_at = NOW()
				WHERE tenant_id = $1 AND link_id = $2`, tenantID, o.LinkID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `UPDATE principals SET status = 'DISABLED', updated_at = $3, disabled_at = $3,
			disabled_reason = 'MERGED' WHERE tenant_id = $1 AND principal_id = $2`, tenantID, duplicateID, now); err != nil {
			return err
		}

		raw, err := json.Marshal(outcomes)
		if err != nil {
			return err
		}
		m := IdentityMerge{MergeID: ulid.Make().String(), TenantID: tenantID, SurvivorID: survivorID, DuplicateID: duplicateID,
			Reason: reason, MergedBy: actor, CorrelationID: correlationID, MergedAt: now, LinkOutcomes: outcomes}
		if _, err := tx.Exec(ctx, `INSERT INTO principal_merges (merge_id, tenant_id, survivor_id, duplicate_id, reason, merged_by,
			correlation_id, merged_at, link_outcomes) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			m.MergeID, tenantID, survivorID, duplicateID, reason, actor, correlationID, now, raw); err != nil {
			return err
		}
		if err := s.appendDecision(ctx, tx, duplicateID, tenantID, "IDENTITY_MERGED_AWAY", "APPLIED",
			"into:"+survivorID+" by:"+actor, correlationID, now); err != nil {
			return err
		}
		if err := s.appendDecision(ctx, tx, survivorID, tenantID, "IDENTITY_MERGED_IN", "APPLIED",
			"from:"+duplicateID+" by:"+actor, correlationID, now); err != nil {
			return err
		}
		recs, err := build(m)
		if err != nil {
			return err
		}
		if err := s.enqueueAll(ctx, tx, recs); err != nil {
			return err
		}
		out, created = &m, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// MergedInto reports the survivor a duplicate id was merged into, or "" when it was not merged away.
func (s *PgStore) MergedInto(ctx context.Context, tenantID, principalID string) (string, error) {
	var id string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT survivor_id FROM principal_merges WHERE tenant_id = $1 AND duplicate_id = $2`,
			tenantID, principalID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return id, err
}
