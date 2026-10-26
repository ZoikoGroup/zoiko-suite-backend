package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
)

// MarkDepreciationRunEmittedToKernel records a run whose depreciation went through the ledger accounting kernel: one journal per asset
// category. journal_id keeps the first so every existing reader still sees a journal; ledger_journal_ids lists them all, so superseding
// the run can reverse each of them.
func (s *PgStore) MarkDepreciationRunEmittedToKernel(ctx context.Context, runID string, journalIDs []string, at time.Time) error {
	if len(journalIDs) == 0 {
		return domain.ErrInvalidRunTransition
	}
	ids, _ := json.Marshal(journalIDs)
	return s.transitionRun(ctx, runID, domain.DepreciationRunStatusApproved, domain.DepreciationRunStatusAccountingEventEmitted,
		", emitted_at = $2, journal_id = $3, ledger_journal_ids = $4::jsonb", at, journalIDs[0], string(ids))
}

// AssetCategories returns the category (the asset class the kernel reconciles the register by) of each asset.
func (s *PgStore) AssetCategories(ctx context.Context, assetIDs []string) (map[string]string, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	out := map[string]string{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT asset_id::text, asset_category FROM fixed_assets WHERE tenant_id = $1 AND asset_id::text = ANY($2)`, tenantID, assetIDs)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id, cat string
			if e := rows.Scan(&id, &cat); e != nil {
				return e
			}
			out[id] = cat
		}
		return rows.Err()
	})
	return out, err
}
